// Command sandboxd is the control-room operator: a daemon (a program that runs
// in the background all the time) that creates, runs and destroys microVMs.
// It is the only part of the system that talks to Firecracker.
//
// Week 3: `sandboxd --version`, and `sandboxd build-snapshots`, which builds
// the base snapshots every clone is restored from (`make snapshots` runs
// it). The real daemon arrives in week 4.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/redwan-cpp/MicroVM/internal/snapshot"
	"github.com/redwan-cpp/MicroVM/internal/state"
	"github.com/redwan-cpp/MicroVM/internal/version"
	"github.com/redwan-cpp/MicroVM/internal/vmm"
)

// buildTimeout bounds a whole `sandboxd build-snapshots`.
const buildTimeout = 10 * time.Minute

func main() {
	if len(os.Args) > 1 && os.Args[1] == "build-snapshots" {
		os.Exit(buildSnapshots(os.Args[2:]))
	}

	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version.String("sandboxd"))
		return
	}

	fmt.Fprintln(os.Stderr, "sandboxd: the daemon is not built yet (arrives in week 4); try --version or build-snapshots")
	os.Exit(1)
}

// buildSnapshots builds a new snapshot for each size class asked for,
// keeps the newest few versions, then opens the store once to check every
// hash. It returns the exit code.
func buildSnapshots(args []string) int {
	flags := flag.NewFlagSet("sandboxd build-snapshots", flag.ContinueOnError)
	stateDir := flags.String("state", state.Dir(state.DefaultDir),
		"state folder (default: $"+state.EnvVar+", else "+state.DefaultDir+")")
	firecracker := flags.String("firecracker", "third_party/firecracker/firecracker", "the firecracker program")
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
		deleted, err := snapshot.Prune(root, class.Name, keep)
		if err != nil {
			return err
		}
		for _, id := range deleted {
			fmt.Printf("%-6s deleted old version %s (keeping %d)\n", class.Name, id, keep)
		}
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

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
