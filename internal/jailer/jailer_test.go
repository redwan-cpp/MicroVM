package jailer

import (
	"reflect"
	"strings"
	"testing"
)

func validSpec() Spec {
	return Spec{
		ID:              "r-0123456789abcdef",
		UID:             DefaultUID,
		GID:             DefaultGID,
		ChrootBase:      "/srv/state/jail",
		Firecracker:     "/opt/fc/firecracker",
		Limits:          Limits{CPUs: 2, MemoryBytes: 576 << 20, Pids: 16},
		FirecrackerArgs: []string{"--api-sock", "api.sock"},
		Shared:          []string{"/srv/state/snapshots/small/v1/vm.snap", "/srv/state/snapshots/small/v1/mem.snap"},
		Owned:           []string{"scratch.ext4", "firecracker.log"},
	}
}

func TestBuild(t *testing.T) {
	plan, err := Build(validSpec())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	wantArgs := []string{
		"--id", "r-0123456789abcdef",
		"--exec-file", "/opt/fc/firecracker",
		"--uid", "70000", "--gid", "70000",
		"--chroot-base-dir", "/srv/state/jail",
		"--cgroup-version", "2",
		"--parent-cgroup", "microvm",
		"--cgroup", "cpu.max=200000 100000",
		"--cgroup", "memory.max=603979776",
		"--cgroup", "pids.max=16",
		"--",
		"--api-sock", "api.sock",
	}
	if !reflect.DeepEqual(plan.Args, wantArgs) {
		t.Errorf("Args:\n got  %q\n want %q", plan.Args, wantArgs)
	}
	for _, a := range plan.Args {
		if a == "--new-pid-ns" || a == "--daemonize" {
			t.Errorf("Args contain %s: the PID we start must stay Firecracker's, with our console", a)
		}
	}
	if plan.JailDir != "/srv/state/jail/firecracker/r-0123456789abcdef" ||
		plan.ChrootDir != "/srv/state/jail/firecracker/r-0123456789abcdef/root" {
		t.Errorf("JailDir %s, ChrootDir %s", plan.JailDir, plan.ChrootDir)
	}
	if plan.Cgroup != "microvm/r-0123456789abcdef" {
		t.Errorf("Cgroup %s", plan.Cgroup)
	}
	wantLinks := []Link{
		{"/srv/state/snapshots/small/v1/vm.snap", plan.ChrootDir + "/vm.snap"},
		{"/srv/state/snapshots/small/v1/mem.snap", plan.ChrootDir + "/mem.snap"},
	}
	if !reflect.DeepEqual(plan.Links, wantLinks) {
		t.Errorf("Links %v, want %v", plan.Links, wantLinks)
	}
	if !reflect.DeepEqual(plan.Owned, []string{plan.ChrootDir + "/scratch.ext4", plan.ChrootDir + "/firecracker.log"}) {
		t.Errorf("Owned %v", plan.Owned)
	}
}

func TestBuildRefusesBadSpecs(t *testing.T) {
	tests := map[string]func(*Spec){
		"empty ID":         func(s *Spec) { s.ID = "" },
		"long ID":          func(s *Spec) { s.ID = strings.Repeat("a", 65) },
		"ID with a slash":  func(s *Spec) { s.ID = "../etc" },
		"non-ASCII ID":     func(s *Spec) { s.ID = "r-é" },
		"root UID":         func(s *Spec) { s.UID = 0 },
		"root GID":         func(s *Spec) { s.GID = 0 },
		"relative base":    func(s *Spec) { s.ChrootBase = "state/jail" },
		"unclean base":     func(s *Spec) { s.ChrootBase = "/srv/state/../jail" },
		"relative binary":  func(s *Spec) { s.Firecracker = "firecracker" },
		"no CPUs":          func(s *Spec) { s.Limits.CPUs = 0 },
		"no memory":        func(s *Spec) { s.Limits.MemoryBytes = 0 },
		"no pids":          func(s *Spec) { s.Limits.Pids = 0 },
		"relative shared":  func(s *Spec) { s.Shared = []string{"vm.snap"} },
		"duplicate shared": func(s *Spec) { s.Shared = []string{"/a/vm.snap", "/b/vm.snap"} },
		"owned path":       func(s *Spec) { s.Owned = []string{"dir/scratch.ext4"} },
		"owned dot-dot":    func(s *Spec) { s.Owned = []string{".."} },
		"owned and shared": func(s *Spec) { s.Owned = []string{"vm.snap"} },
		// Names the jailer itself creates in the chroot root.
		"shared binary name": func(s *Spec) { s.Shared = []string{"/srv/other/firecracker"} },
		"owned pid file":     func(s *Spec) { s.Owned = []string{"firecracker.pid"} },
		"owned dev":          func(s *Spec) { s.Owned = []string{"dev"} },
		"shared run":         func(s *Spec) { s.Shared = []string{"/srv/state/run"} },
		"owned old_root":     func(s *Spec) { s.Owned = []string{"old_root"} },
		"renamed binary": func(s *Spec) {
			s.Firecracker = "/opt/fc/fc-v1.17"
			s.Owned = []string{"fc-v1.17"}
		},
		"negative CPU quota": func(s *Spec) {
			s.Limits.CPUs = -1
		},
	}
	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			spec := validSpec()
			change(&spec)
			if _, err := Build(spec); err == nil {
				t.Errorf("Build accepted %+v", spec)
			}
		})
	}
}

func TestJailPaths(t *testing.T) {
	// The folder is named after the binary, whatever it is called.
	if got := ChrootDir("/s/jail", "/opt/fc/fc-v1.17", "r-1"); got != "/s/jail/fc-v1.17/r-1/root" {
		t.Errorf("ChrootDir = %s", got)
	}
	if got := JailDir("/s/jail", "/opt/fc/firecracker", "r-1"); got != "/s/jail/firecracker/r-1" {
		t.Errorf("JailDir = %s", got)
	}
	if got := ReservedNames("/opt/fc/firecracker"); !reflect.DeepEqual(got,
		[]string{"firecracker", "firecracker.pid", "dev", "run", "old_root"}) {
		t.Errorf("ReservedNames = %v", got)
	}
}

func TestCgroupRoot(t *testing.T) {
	mounts := `sysfs /sys sysfs rw,nosuid,nodev,noexec,relatime 0 0
tmpfs /run tmpfs rw,nosuid,nodev 0 0
cgroup2 /sys/fs/cgroup cgroup2 rw,nosuid,nodev,noexec,relatime,nsdelegate 0 0
cgroup2 /other cgroup2 rw 0 0
`
	got, err := CgroupRoot(strings.NewReader(mounts))
	if err != nil || got != "/sys/fs/cgroup" {
		t.Errorf("CgroupRoot = %q, %v; want the first cgroup2 mount", got, err)
	}
	got, err = CgroupRoot(strings.NewReader(`none /my\040cgroups cgroup2 rw 0 0`))
	if err != nil || got != "/my cgroups" {
		t.Errorf("CgroupRoot with an escaped space = %q, %v", got, err)
	}
	if _, err := CgroupRoot(strings.NewReader("sysfs /sys sysfs rw 0 0\n")); err == nil {
		t.Error("CgroupRoot without a cgroup2 mount succeeded")
	}
}

func TestCheckIdentity(t *testing.T) {
	users := "root:x:0:0:root:/root:/bin/bash\nred:x:1000:1000::/home/red:/bin/zsh\n"
	group := "root:x:0:\nred:x:1000:\n"
	subuid := "red:100000:65536\n"
	subgid := "red:100000:65536\n"
	check := func(uid, gid int) error {
		return CheckIdentity(uid, gid, strings.NewReader(users), strings.NewReader(group),
			strings.NewReader(subuid), strings.NewReader(subgid))
	}

	if err := check(DefaultUID, DefaultGID); err != nil {
		t.Errorf("the default identity: %v", err)
	}
	for _, c := range []struct {
		uid, gid int
		file     string
	}{
		{1000, DefaultGID, "/etc/passwd"},
		{DefaultUID, 1000, "/etc/group"},
		{100000, DefaultGID, "/etc/subuid"},
		{165535, DefaultGID, "/etc/subuid"},
		{DefaultUID, 120000, "/etc/subgid"},
	} {
		err := check(c.uid, c.gid)
		if err == nil || !strings.Contains(err.Error(), c.file) {
			t.Errorf("uid %d gid %d: %v, want a clash in %s", c.uid, c.gid, err, c.file)
		}
	}
	if err := check(165536, DefaultGID); err != nil {
		t.Errorf("uid 165536 is just past red's range: %v", err)
	}
	if err := CheckIdentity(1000, 1000, nil, nil, nil, nil); err != nil {
		t.Errorf("with no files to read: %v", err)
	}
}
