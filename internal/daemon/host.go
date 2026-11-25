package daemon

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"

	daemonv1 "github.com/redwan-cpp/MicroVM/internal/gen/sandbox/daemon/v1"
	"github.com/redwan-cpp/MicroVM/internal/jailer"
	"github.com/redwan-cpp/MicroVM/internal/vmm"
)

// What the daemon tells its clients about the host it runs on: whether
// that host is itself a VM (Info), and what its clones use right now
// (HostStats). sandboxctl ui shows both; neither changes anything.

// HostKind is what the daemon found out about its host when it started.
type HostKind struct {
	// Kind is physical, virtual or unknown.
	Kind daemonv1.HostKind
	// Detail says what Kind is based on.
	Detail string
}

// The places DetectHost reads. /sys/class/dmi/id holds what the
// machine's firmware says about it; a VM's firmware names its hypervisor.
const (
	cpuinfoPath = "/proc/cpuinfo"
	dmiDir      = "/sys/class/dmi/id"
	maxHostFile = 1 << 20 // /proc/cpuinfo of a big machine
)

// dmiFields are the firmware fields DetectHost looks at.
var dmiFields = []string{"sys_vendor", "product_name", "board_vendor", "bios_vendor", "chassis_vendor"}

// DetectHost finds out, once, whether this host is itself a VM: from the
// "hypervisor" flag in /proc/cpuinfo (x86 CPUs set it inside a VM) and
// from the firmware's names in /sys/class/dmi/id. It only reads files;
// it never runs a program (in jailer mode the daemon is root).
func DetectHost() HostKind {
	cpuinfo, _ := readSmall(cpuinfoPath, maxHostFile)
	dmi := make(map[string]string)
	for _, f := range dmiFields {
		if b, err := readSmall(filepath.Join(dmiDir, f), 4<<10); err == nil {
			dmi[f] = vmm.CleanConsole(bytes.TrimSpace(b), 128)
		}
	}
	return classifyHost(cpuinfo, dmi, runtime.GOARCH)
}

// knownHypervisors are words a VM's firmware uses for itself, lower case,
// and how to say so.
var knownHypervisors = []struct{ word, name string }{
	{"qemu", "QEMU"}, {"kvm", "KVM"}, {"vmware", "VMware"}, {"virtualbox", "VirtualBox"}, {"innotek", "VirtualBox"},
	{"xen", "Xen"}, {"bochs", "Bochs"}, {"bhyve", "bhyve"}, {"parallels", "Parallels"}, {"apple virtualization", "Apple Virtualization"},
	{"amazon ec2", "Amazon EC2"}, {"google compute engine", "Google Compute Engine"}, {"openstack", "OpenStack"},
	{"hyper-v", "Hyper-V"}, {"virtual machine", "a virtual machine"}, {"cloud hypervisor", "Cloud Hypervisor"},
	{"firecracker", "Firecracker"},
}

// classifyHost decides what DetectHost read: cpuinfo is /proc/cpuinfo,
// dmi the firmware fields it could read, goarch runtime.GOARCH.
//
//   - Any sign of a hypervisor (the CPU flag, or the firmware naming one)
//     means a VM.
//   - On x86 the flag is reliable enough to say "physical" when it is
//     missing and the firmware names no hypervisor either.
//   - Elsewhere (arm64 has no such flag) a firmware that names no
//     hypervisor proves nothing: unknown.
func classifyHost(cpuinfo []byte, dmi map[string]string, goarch string) HostKind {
	hasFlags, hypervisor := cpuFlags(cpuinfo)
	var evidence []string
	if hypervisor {
		evidence = append(evidence, "the CPU's hypervisor flag is set")
	}
	all := strings.ToLower(strings.Join(mapValues(dmi), " "))
	for _, h := range knownHypervisors {
		if strings.Contains(all, h.word) {
			evidence = append(evidence, "the firmware names "+h.name)
			break
		}
	}
	switch {
	case len(evidence) > 0:
		return HostKind{daemonv1.HostKind_HOST_KIND_VIRTUAL, strings.Join(evidence, "; ")}
	case hasFlags && (goarch == "amd64" || goarch == "386"):
		detail := "no hypervisor CPU flag, and the firmware names no hypervisor"
		if len(dmi) == 0 {
			detail = "no hypervisor CPU flag (the firmware's names could not be read)"
		}
		return HostKind{daemonv1.HostKind_HOST_KIND_PHYSICAL, detail}
	case len(dmi) > 0:
		return HostKind{daemonv1.HostKind_HOST_KIND_UNKNOWN, "the firmware names no hypervisor, but this CPU has no flag to confirm it"}
	}
	return HostKind{daemonv1.HostKind_HOST_KIND_UNKNOWN, "neither the CPU flags nor the firmware's names could be read"}
}

// cpuFlags reads the first "flags" line of /proc/cpuinfo (x86 only):
// whether there is one, and whether it lists "hypervisor".
func cpuFlags(cpuinfo []byte) (hasFlags, hypervisor bool) {
	sc := bufio.NewScanner(bytes.NewReader(cpuinfo))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		name, value, ok := strings.Cut(sc.Text(), ":")
		if !ok || strings.TrimSpace(name) != "flags" {
			continue
		}
		for _, f := range strings.Fields(value) {
			if f == "hypervisor" {
				return true, true
			}
		}
		return true, false
	}
	return false, false
}

func mapValues(m map[string]string) []string {
	var out []string
	for _, k := range dmiFields {
		if v, ok := m[k]; ok {
			out = append(out, v)
		}
	}
	return out
}

// readSmall reads at most limit bytes of the file at path.
func readSmall(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // G304: fixed paths under /proc and /sys
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read-only
	return io.ReadAll(io.LimitReader(f, limit))
}

// reason is err without the path it names: HostStats's errors are shown
// in sandboxctl ui and recorded with a session, which never holds host
// paths.
func reason(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Op + ": " + pe.Err.Error()
	}
	return err.Error()
}

// hostStats gathers HostStats. One call at a time: a client that polls
// fast never makes the daemon scan /proc many times at once.
type hostStats struct {
	mu         sync.Mutex
	procDir    string // /proc (tests replace it)
	cgroupOnce sync.Once
	cgroupDir  string // <cgroup v2 root>/microvm, or "" with cgroupErr
	cgroupErr  error
}

// cgroupParent finds the cgroup folder the jailer puts every clone's
// cgroup in (internal/jailer: <root>/microvm/<id>), once.
func (h *hostStats) cgroupParent() (string, error) {
	h.cgroupOnce.Do(func() {
		f, err := os.Open("/proc/self/mounts")
		if err != nil {
			h.cgroupErr = err
			return
		}
		defer f.Close() //nolint:errcheck // read-only
		root, err := jailer.CgroupRoot(f)
		if err != nil {
			h.cgroupErr = err
			return
		}
		h.cgroupDir = filepath.Join(root, jailer.ParentCgroup)
	})
	return h.cgroupDir, h.cgroupErr
}

// countDir counts the entries of dir; a folder that does not exist has
// none.
func countDir(dir string) *daemonv1.Count {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return &daemonv1.Count{}
	}
	if err != nil {
		return &daemonv1.Count{Error: reason(err)}
	}
	return &daemonv1.Count{N: uint32(min(len(entries), 1<<31))} //nolint:gosec // G115: capped
}

// countSubdirs counts the folders in dir (a cgroup folder also holds
// files: its own control files).
func countSubdirs(dir string) *daemonv1.Count {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return &daemonv1.Count{}
	}
	if err != nil {
		return &daemonv1.Count{Error: reason(err)}
	}
	n := uint32(0)
	for _, e := range entries {
		if e.IsDir() {
			n++
		}
	}
	return &daemonv1.Count{N: n}
}

// processesUnder counts the processes whose working folder is under one
// of dirs. Processes it may not look at (another user's, in rootless
// mode) are skipped: a rootless daemon's clones are all its own user's.
func (h *hostStats) processesUnder(ctx context.Context, dirs ...string) *daemonv1.Count {
	entries, err := os.ReadDir(h.procDir)
	if err != nil {
		return &daemonv1.Count{Error: reason(err)}
	}
	n := uint32(0)
	for _, e := range entries {
		if ctx.Err() != nil {
			return &daemonv1.Count{Error: "stopped: " + ctx.Err().Error()}
		}
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue // not a process
		}
		cwd, err := os.Readlink(filepath.Join(h.procDir, e.Name(), "cwd"))
		if err != nil {
			continue // gone, or not ours to look at
		}
		for _, d := range dirs {
			if strings.HasPrefix(cwd, d+string(filepath.Separator)) {
				n++
				break
			}
		}
	}
	return &daemonv1.Count{N: n}
}

// memory reads a process's RSS and PSS from /proc/<pid>/smaps_rollup.
func (h *hostStats) memory(pid int) (rss, pss uint64, err error) {
	if pid <= 0 {
		return 0, 0, errors.New("no process")
	}
	data, err := readSmall(filepath.Join(h.procDir, strconv.Itoa(pid), "smaps_rollup"), 16<<10)
	if err != nil {
		return 0, 0, fmt.Errorf("smaps_rollup: %s", reason(err))
	}
	return parseSmapsRollup(data)
}

// parseSmapsRollup reads the "Rss:" and "Pss:" lines (in kB) of
// smaps_rollup.
func parseSmapsRollup(data []byte) (rss, pss uint64, err error) {
	var gotRss, gotPss bool
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 || fields[2] != "kB" {
			continue
		}
		n, perr := strconv.ParseUint(fields[1], 10, 64)
		if perr != nil || n > 1<<44 {
			continue
		}
		switch fields[0] {
		case "Rss:":
			rss, gotRss = n<<10, true
		case "Pss:":
			pss, gotPss = n<<10, true
		}
	}
	if !gotRss || !gotPss {
		return 0, 0, errors.New("smaps_rollup: no Rss or Pss line")
	}
	return rss, pss, nil
}
