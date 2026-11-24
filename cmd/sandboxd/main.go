// Command sandboxd is the control-room operator: a daemon (a program that runs
// in the background all the time) that creates, runs and destroys microVMs.
// It is the only part of the system that talks to Firecracker.
//
//	sandboxd serve            the daemon itself (internal/daemon)
//	sandboxd build-snapshots  build the base snapshots (make snapshots)
//	sandboxd rotate-audit     move a broken audit log aside, start a new chain
//	sandboxd --version
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/redwan-cpp/MicroVM/internal/audit"
	"github.com/redwan-cpp/MicroVM/internal/daemon"
	"github.com/redwan-cpp/MicroVM/internal/snapshot"
	"github.com/redwan-cpp/MicroVM/internal/state"
	"github.com/redwan-cpp/MicroVM/internal/version"
	"github.com/redwan-cpp/MicroVM/internal/vmm"
)

// buildTimeout bounds a whole `sandboxd build-snapshots`.
const buildTimeout = 10 * time.Minute

// The default paths are relative to the repository root, where make runs.
const (
	defaultFirecracker = "third_party/firecracker/firecracker"
	defaultJailer      = "third_party/firecracker/jailer"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "serve":
			os.Exit(serve(os.Args[2:]))
		case "build-snapshots":
			os.Exit(buildSnapshots(os.Args[2:]))
		case "rotate-audit":
			os.Exit(rotateAudit(os.Args[2:]))
		}
	}

	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: sandboxd serve | build-snapshots | rotate-audit | --version (each has -h)")
	}
	flag.Parse()
	if *showVersion {
		fmt.Println(version.String("sandboxd"))
		return
	}
	flag.Usage()
	os.Exit(2) // exit code 2 is the usual "you used the command wrong"
}

// stateDirFlag adds --state-dir (and its older name --state) to flags.
func stateDirFlag(flags *flag.FlagSet) *string {
	dir := flags.String("state-dir", state.Dir(state.DefaultDir),
		"state folder (default: $"+state.EnvVar+", else "+state.DefaultDir+")")
	flags.StringVar(dir, "state", *dir, "the same as --state-dir")
	return dir
}

// serve runs the daemon until SIGINT or SIGTERM, then shuts it down
// within daemon.ShutdownTimeout. It returns the exit code.
func serve(args []string) int {
	flags := flag.NewFlagSet("sandboxd serve", flag.ContinueOnError)
	stateDir := stateDirFlag(flags)
	socket := flags.String("socket", "", "the socket to listen on (default: $XDG_RUNTIME_DIR/sandboxd/sandboxd.sock, "+
		"with --jailer "+daemon.JailerSocket+")")
	jailerMode := flags.Bool("jailer", false, "jailer mode: every clone in a chroot, as its own UID, in its own cgroup (needs root)")
	jailerBin := flags.String("jailer-binary", defaultJailer, "the jailer program (jailer mode)")
	firecracker := flags.String("firecracker", defaultFirecracker, "the firecracker program")
	group := flags.String("socket-group", "", "jailer mode: the group that may use the socket "+
		"(default: "+daemon.SocketGroup+" if it exists, else only root)")
	maxConc := flags.Int("max-concurrent", daemon.DefaultMaxConcurrent, "how many runs may have a clone at once (1 to 32)")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() > 0 {
		flags.Usage()
		return 2
	}

	logger := log.New(os.Stderr, "sandboxd: ", log.LstdFlags|log.Lmicroseconds)
	d, err := daemon.Start(daemon.Options{
		StateDir:      *stateDir,
		Socket:        *socket,
		Firecracker:   *firecracker,
		Jailer:        *jailerMode,
		JailerBinary:  *jailerBin,
		SocketGroup:   *group,
		MaxConcurrent: *maxConc,
		Logger:        logger,
		Version:       version.String("sandboxd"),
	})
	if err != nil {
		logger.Print(err)
		return 1
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	select {
	case s := <-sig:
		logger.Printf("%v: shutting down (at most %v)", s, daemon.ShutdownTimeout)
	case err := <-d.Done():
		logger.Printf("serving stopped: %v", err)
	}
	signal.Stop(sig)
	ctx, cancel := context.WithTimeout(context.Background(), daemon.ShutdownTimeout)
	defer cancel()
	if err := d.Shutdown(ctx); err != nil {
		logger.Printf("shutdown: %v", err)
		return 1
	}
	logger.Print("stopped; every clone destroyed")
	return 0
}

// buildSnapshots builds a new snapshot for each size class asked for,
// keeps the newest few versions, then opens the store once to check every
// hash. It returns the exit code.
func buildSnapshots(args []string) int {
	flags := flag.NewFlagSet("sandboxd build-snapshots", flag.ContinueOnError)
	stateDir := stateDirFlag(flags)
	firecracker := flags.String("firecracker", defaultFirecracker, "the firecracker program")
	kernel := flags.String("kernel", "guest/out/vmlinux", "the guest kernel")
	rootfs := flags.String("rootfs", "guest/out/rootfs.ext4", "the guest root disk")
	classList := flags.String("class", strings.Join(snapshot.ClassNames(), ","), "size classes to build, comma-separated")
	keep := flags.Int("keep", 2, "how many versions of each class to keep")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() > 0 || *keep < 1 {
		flags.Usage()
		return 2
	}
	var classes []snapshot.Class
	for _, name := range strings.Split(*classList, ",") {
		c, err := snapshot.LookupClass(strings.TrimSpace(name))
		if err != nil {
			fmt.Fprintln(os.Stderr, "sandboxd:", err)
			return 2
		}
		classes = append(classes, c)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, buildTimeout)
	defer cancel()
	if err := build(ctx, *stateDir, classes, *keep, snapshot.BuildConfig{
		Firecracker: *firecracker, Kernel: *kernel, Rootfs: *rootfs,
	}); err != nil {
		fmt.Fprintln(os.Stderr, "sandboxd:", err)
		return 1
	}
	return 0
}

func build(ctx context.Context, stateDir string, classes []snapshot.Class, keep int, cfg snapshot.BuildConfig) error {
	root, err := state.Open(stateDir, state.Options{})
	if err != nil {
		return err
	}
	for _, class := range classes {
		res, err := snapshot.Build(ctx, root, class, cfg)
		if err != nil {
			return err
		}
		m := res.Version.Manifest
		fmt.Printf("%-6s built %s in %v: %d vCPU, %d MiB; cold boot to agent %v; VMGenID %v\n",
			class.Name, res.Version.ID, res.Total.Round(10*time.Millisecond), m.VCPUs, m.MemoryMiB,
			res.Boot.Round(time.Millisecond), yesNo(m.VMGenID))
	}
	if err := prune(root, classes, keep); err != nil {
		return err
	}

	// Open the store the way every user of it will: this checks every
	// manifest against this Firecracker and every file against its hash.
	fcVersion, err := vmm.BinaryVersion(ctx, cfg.Firecracker)
	if err != nil {
		return err
	}
	if _, err := snapshot.Open(ctx, root, fcVersion); err != nil {
		return err
	}
	fmt.Printf("snapshot store %s checked (Firecracker %s)\n", root.SnapshotsDir(), fcVersion)
	return nil
}

// prune deletes old snapshot versions, but only while it holds the state
// folder's lock: a running sandboxd holds it and keeps restoring from the
// version it opened, so then nothing is pruned. The build itself never
// waits or fails for that reason.
func prune(root *state.Root, classes []snapshot.Class, keep int) error {
	lock, err := root.Lock()
	if errors.Is(err, state.ErrLocked) {
		fmt.Println("old versions not pruned: a sandboxd holds the state folder's lock (it may be using them); " +
			"run make snapshots again after it stops")
		return nil
	}
	if err != nil {
		return err
	}
	defer lock.Release() //nolint:errcheck // releasing a lock we hold
	for _, class := range classes {
		deleted, err := snapshot.Prune(root, class.Name, keep)
		if err != nil {
			return err
		}
		for _, id := range deleted {
			fmt.Printf("%-6s deleted old version %s (keeping %d)\n", class.Name, id, keep)
		}
	}
	return nil
}

// rotateAudit moves the audit log aside and starts a new chain, the
// documented way out of a broken one. It needs the state folder's lock, so
// no daemon may be running.
func rotateAudit(args []string) int {
	flags := flag.NewFlagSet("sandboxd rotate-audit", flag.ContinueOnError)
	stateDir := stateDirFlag(flags)
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() > 0 {
		flags.Usage()
		return 2
	}
	root, err := state.Open(*stateDir, state.Options{})
	if err != nil {
		fmt.Fprintln(os.Stderr, "sandboxd:", err)
		return 1
	}
	lock, err := root.Lock()
	if err != nil {
		fmt.Fprintln(os.Stderr, "sandboxd: stop the running sandboxd first:", err)
		return 1
	}
	defer lock.Release() //nolint:errcheck // releasing a lock we hold
	if _, err := audit.Verify(audit.Path(root.AuditDir())); err == nil {
		fmt.Fprintln(os.Stderr, "sandboxd: note: the audit chain verifies; rotating it anyway")
	}
	var owner *audit.Owner
	if os.Geteuid() == 0 {
		uid, gid, err := root.Owner()
		if err != nil {
			fmt.Fprintln(os.Stderr, "sandboxd:", err)
			return 1
		}
		owner = &audit.Owner{UID: uid, GID: gid}
	}
	l, aside, err := audit.Rotate(root.AuditDir(), owner, time.Now())
	if err != nil {
		fmt.Fprintln(os.Stderr, "sandboxd:", err)
		return 1
	}
	if err := l.Close(); err != nil {
		fmt.Fprintln(os.Stderr, "sandboxd:", err)
		return 1
	}
	fmt.Printf("moved the old audit log to %s (keep it as evidence)\nnew chain started in %s, its first record notes the rotation\n",
		aside, filepath.Join(root.AuditDir(), audit.FileName))
	return 0
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
