// Package clone restores microVMs from the base snapshots in
// internal/snapshot: one clone per run, destroyed afterwards.
//
// A restore takes six steps, each timed (Timings):
//
//  1. Prepare: a fresh run folder with hard links to the snapshot's
//     vm.snap, mem.snap and rootfs.ext4.
//  2. Scratch: the clone's own copy of the snapshot's formatted scratch
//     disk template (never a link: its writes are its own).
//  3. Process start: an empty Firecracker in that folder.
//  4. LoadSnapshot: Firecracker maps mem.snap copy-on-write and resumes
//     the VM exactly where the snapshot paused it, agent included.
//  5. Agent ready: the host connects to the agent (the guest's listening
//     socket survived the snapshot; the old connections did not).
//  6. De-twin: every clone woke up with the same clock and the same
//     random-number state as the snapshot, so the host sets the guest
//     clock (SyncTime) and mixes fresh entropy into its RNG (Reseed). Both
//     must succeed, or the clone is destroyed (ADR-0003, decision 5).
//
// The scratch disk is not mounted yet: the caller does that with
// MountScratch (sandboxd in its MOUNTING state).
//
// A clone runs in one of two modes, and reports which: rootless (the
// default; Firecracker runs as our own user) or jailer (Firecracker runs
// in a chroot as an unprivileged user, in its own cgroup; needs root). See
// ADR-0005. Every timing carries the mode, so rootless numbers are never
// shown as jailed ones.
package clone

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/redwan-cpp/MicroVM/internal/agentclient"
	agentv1 "github.com/redwan-cpp/MicroVM/internal/gen/sandbox/agent/v1"
	"github.com/redwan-cpp/MicroVM/internal/snapshot"
	"github.com/redwan-cpp/MicroVM/internal/state"
	"github.com/redwan-cpp/MicroVM/internal/vmm"
)

const (
	// agentReadyTimeout is how long the agent may take to answer after
	// LoadSnapshot. It is usually about 15 ms.
	agentReadyTimeout = 5 * time.Second
	// entropySize is how many random bytes Reseed sends.
	entropySize = 32
	// consoleTail is how much of the guest console an error message shows.
	consoleTail = 2048
)

// Mode says how a clone's Firecracker is confined.
type Mode string

const (
	// Rootless means Firecracker runs as our own user, in its own run folder,
	// with Firecracker's seccomp filters and nothing else around it.
	Rootless Mode = "rootless"
	// Jailer means Firecracker runs in a jailer chroot, as an unprivileged
	// user, in its own cgroup (internal/jailer).
	Jailer Mode = "jailer"
)

// Options says how to restore a clone.
type Options struct {
	// Firecracker is the firecracker program.
	Firecracker string
	// ID names the clone (its run folder, and in jailer mode its jail and
	// cgroup). It must look like state.NewID's IDs; empty means a fresh
	// one. sandboxd passes its run ID, so one name is used everywhere.
	ID string
	// Jailer, if not nil, restores the clone in jailer mode. That needs
	// root (the jailer creates chroots, device files and cgroups), and the
	// store's state folder opened with state.Options{Jailer: true,
	// Firecracker: ...}.
	Jailer *JailerOptions
}

// JailerOptions configures jailer mode.
type JailerOptions struct {
	// Binary is the jailer program (third_party/firecracker/jailer).
	Binary string
	// UID and GID are the unprivileged identity Firecracker runs as, for
	// example jailer.DefaultUID and jailer.DefaultGID. Restore refuses an
	// ID that a user, a group or a subordinate ID range already uses.
	// sandboxd gives every running clone an ID of its own.
	UID, GID int
	// CgroupRoot is where cgroup v2 is mounted. Empty means: read it from
	// /proc/self/mounts, as the jailer does.
	CgroupRoot string
}

// Timings is how long each step of a restore took (see the package
// comment), measured on the host.
type Timings struct {
	// Mode is the mode these numbers were measured in.
	Mode Mode
	// Prepare is making the run folder and its hard links.
	Prepare time.Duration
	// Scratch is copying the scratch disk template into it.
	Scratch time.Duration
	// ProcessStart is starting Firecracker until its API socket answers.
	ProcessStart time.Duration
	// LoadSnapshot is the PUT /snapshot/load call, resume included.
	LoadSnapshot time.Duration
	// AgentReady is from the end of LoadSnapshot to the agent's first
	// answer.
	AgentReady time.Duration
	// DeTwin is SyncTime and Reseed.
	DeTwin time.Duration
	// Total is all of the above.
	Total time.Duration
}

// String formats t on one line, mode first.
func (t Timings) String() string {
	ms := func(d time.Duration) string { return fmt.Sprintf("%.1fms", float64(d)/float64(time.Millisecond)) }
	return fmt.Sprintf("%s: prepare %s, scratch %s, process start %s, LoadSnapshot %s, agent ready %s, de-twin %s, total %s",
		t.Mode, ms(t.Prepare), ms(t.Scratch), ms(t.ProcessStart), ms(t.LoadSnapshot), ms(t.AgentReady), ms(t.DeTwin), ms(t.Total))
}

// ExecResult is how a command in a clone ended (agentclient.Result), with
// the clone's mode, because it carries a duration too.
type ExecResult struct {
	agentclient.Result
	Mode Mode
}

// Clone is one restored microVM with a connected agent. Create it with
// Restore and always call Destroy.
type Clone struct {
	id      string
	mode    Mode
	class   snapshot.Class
	version *snapshot.Version
	dir     string // the run folder, or in jailer mode the chroot
	scratch string // the clone's scratch disk, in dir
	owner   *vmm.Owner
	jail    *jail // jailer mode only
	proc    *vmm.Process
	agent   *agentclient.Client
	timings Timings

	destroyOnce sync.Once
	destroyErr  error
}

// Restore makes a new clone of the current snapshot of class from store
// (see the package comment for the steps). ctx bounds the restore only;
// the clone runs until Destroy. On error Restore destroys what it made;
// if that cleanup fails as well, the returned error includes Destroy's.
func Restore(ctx context.Context, store *snapshot.Store, class string, opts Options) (_ *Clone, err error) {
	began := time.Now()
	c := &Clone{mode: Rootless, id: opts.ID}
	if opts.Jailer != nil {
		c.mode = Jailer
	}
	if c.id == "" {
		c.id = state.NewID()
	}
	if !state.ValidID(c.id) {
		return nil, fmt.Errorf("clone: %q is not a run ID", c.id)
	}
	if c.class, err = snapshot.LookupClass(class); err != nil {
		return nil, err
	}
	if c.version, err = store.Current(class); err != nil {
		return nil, err
	}
	// From here on, Destroy cleans up whatever exists so far. If that
	// fails too, the caller must hear about it: something (a run folder, a
	// jail, a cgroup) is left on the host.
	defer func() {
		if err != nil {
			err = errors.Join(err, c.Destroy())
		}
	}()

	// 1. Prepare the run folder (or the chroot).
	var cfg vmm.Config
	if opts.Jailer == nil {
		cfg, err = c.prepareRootless(store.Root(), opts)
	} else {
		cfg, err = c.prepareJailed(store.Root(), opts)
	}
	if err != nil {
		return nil, fmt.Errorf("clone: %w", err)
	}
	prepared := time.Now()

	// 2. The clone's own scratch disk: a copy of the template, the jailed
	// user's in jailer mode (Firecracker writes it).
	if err := c.version.CopyScratch(c.scratch); err != nil {
		return nil, fmt.Errorf("clone: %w", err)
	}
	if o := c.owner; o != nil {
		if err := os.Lchown(c.scratch, o.UID, o.GID); err != nil {
			return nil, fmt.Errorf("clone: give %s to the jailed user: %w", snapshot.ScratchName, err)
		}
	}
	scratched := time.Now()

	// 3. Start Firecracker (in jailer mode: the jailer, which locks itself
	// in and then becomes Firecracker).
	if c.proc, err = vmm.Start(ctx, cfg); err != nil {
		return nil, fmt.Errorf("clone: %w", err)
	}
	started := time.Now()

	// 4. Load the snapshot, by the relative names in the run folder.
	if err := c.proc.Client().LoadSnapshot(ctx, vmm.SnapshotLoad{
		SnapshotPath: snapshot.StateName,
		MemBackend:   vmm.MemoryBackend{BackendType: "File", BackendPath: snapshot.MemName},
		ResumeVM:     true,
	}); err != nil {
		return nil, c.withConsole(fmt.Errorf("clone: load snapshot: %w", err))
	}
	loaded := time.Now()

	// 5. Connect to the agent.
	readyCtx, cancel := context.WithTimeout(ctx, agentReadyTimeout)
	defer cancel()
	if c.agent, _, err = agentclient.WaitReady(readyCtx, c.proc.VsockPath(), c.proc.Done()); err != nil {
		return nil, c.withConsole(fmt.Errorf("clone: %w", err))
	}
	ready := time.Now()

	// 6. De-twin.
	if err := deTwin(ctx, c.agent); err != nil {
		return nil, c.withConsole(fmt.Errorf("clone: de-twin: %w", err))
	}
	done := time.Now()

	c.timings = Timings{
		Mode:         c.mode,
		Prepare:      prepared.Sub(began),
		Scratch:      scratched.Sub(prepared),
		ProcessStart: started.Sub(scratched),
		LoadSnapshot: loaded.Sub(started),
		AgentReady:   ready.Sub(loaded),
		DeTwin:       done.Sub(ready),
		Total:        done.Sub(began),
	}
	return c, nil
}

// prepareRootless makes a fresh run folder <state>/runs/<id> with links
// to the snapshot's files.
func (c *Clone) prepareRootless(root *state.Root, opts Options) (vmm.Config, error) {
	dir, err := root.CreateRunDir(c.id)
	if err != nil {
		return vmm.Config{}, err
	}
	c.dir = dir
	c.scratch = filepath.Join(dir, snapshot.ScratchName)
	if err := c.version.LinkInto(dir); err != nil {
		return vmm.Config{}, err
	}
	return vmm.Config{Binary: opts.Firecracker, RunDir: dir}, nil
}

// deTwin sets the guest clock from the host's and reseeds the guest RNG
// with fresh host entropy. A clone that resumed from a snapshot has the
// snapshot's clock (seconds or days behind) and its RNG state, which is
// the same in every clone.
func deTwin(ctx context.Context, agent *agentclient.Client) error {
	if err := agent.SyncTime(ctx, time.Now()); err != nil {
		return err
	}
	entropy := make([]byte, entropySize)
	if _, err := rand.Read(entropy); err != nil {
		return fmt.Errorf("host entropy: %w", err)
	}
	return agent.Reseed(ctx, entropy)
}

// withConsole adds the end of the guest console and of Firecracker's own
// log to err.
func (c *Clone) withConsole(err error) error {
	return fmt.Errorf("%w\nguest console (end):\n%s\nfirecracker log (end):\n%s",
		err, c.proc.ConsoleTail(consoleTail), c.proc.LogTail(consoleTail))
}

// ID is the clone's run ID (state.NewID), also the name of its folder
// (and in jailer mode of its jail and cgroup).
func (c *Clone) ID() string { return c.id }

// Mode is how the clone's Firecracker is confined.
func (c *Clone) Mode() Mode { return c.mode }

// Timings is how long each step of the restore took.
func (c *Clone) Timings() Timings { return c.timings }

// Class is the clone's size class.
func (c *Clone) Class() snapshot.Class { return c.class }

// Version is the snapshot version the clone was restored from.
func (c *Clone) Version() *snapshot.Version { return c.version }

// Dir is the clone's run folder on the host: Firecracker's working
// directory (in jailer mode its chroot). Destroy deletes it.
func (c *Clone) Dir() string { return c.dir }

// JailDir is the jailer's folder for this clone, <state>/jail/firecracker/<id>,
// or "" in rootless mode. Destroy deletes it.
func (c *Clone) JailDir() string {
	if c.jail == nil {
		return ""
	}
	return c.jail.dir
}

// CgroupDir is the clone's cgroup, for example
// /sys/fs/cgroup/microvm/<id>, or "" in rootless mode. Destroy removes it.
func (c *Clone) CgroupDir() string {
	if c.jail == nil {
		return ""
	}
	return c.jail.cgroup
}

// Pid is the clone's Firecracker process ID.
func (c *Clone) Pid() int { return c.proc.Pid() }

// Done is closed when the clone's Firecracker has exited.
func (c *Clone) Done() <-chan struct{} { return c.proc.Done() }

// ConsoleTail returns the end of the guest console, safe to print.
func (c *Clone) ConsoleTail(n int) string { return c.proc.ConsoleTail(n) }

// MountScratch asks the agent to mount the clone's scratch disk at /work
// (agentclient.Client.MountScratch), with the class's scratch size. From
// then on, commands run in /work unless they ask for another folder.
func (c *Clone) MountScratch(ctx context.Context) error {
	if err := c.agent.MountScratch(ctx, uint64(c.class.ScratchMiB)<<20); err != nil { //nolint:gosec // G115: a class's size is a small positive number
		return fmt.Errorf("clone: %w", err)
	}
	return nil
}

// LogTail returns the end of Firecracker's own log, safe to print. It is
// kept in a 64 KiB ring buffer, never in a file that could grow.
func (c *Clone) LogTail(n int) string { return c.proc.LogTail(n) }

// Exec runs a command in the clone (see agentclient.Client.Exec, whose
// rules all apply: ctx needs a deadline, and the host enforces the
// timeout and the output cap itself).
func (c *Clone) Exec(ctx context.Context, req *agentv1.ExecRequest, stdout, stderr io.Writer) (ExecResult, error) {
	res, err := c.agent.Exec(ctx, req, stdout, stderr)
	return ExecResult{Result: res, Mode: c.mode}, err
}

// Destroy ends the clone: it closes the agent connection, stops
// Firecracker (the VM and everything in it are gone) and deletes the run
// folder with its links and scratch disk. In jailer mode it first kills
// every process in the clone's cgroup, then deletes the jail folder and
// the cgroup too. The snapshot itself is never touched. Destroy may be
// called any number of times.
//
// A nil error means all of it is gone: Firecracker has exited, and the
// run folder (in jailer mode the jail folder and the cgroup) no longer
// exists. Otherwise the error is a *CleanupError: something of the clone
// is still on the host.
func (c *Clone) Destroy() error {
	c.destroyOnce.Do(func() { c.destroyErr = c.destroy() })
	return c.destroyErr
}

func (c *Clone) destroy() error {
	var errs []error
	if c.agent != nil {
		_ = c.agent.Close()
	}
	if c.jail != nil {
		errs = append(errs, c.jail.kill())
	}
	switch {
	case c.proc != nil:
		errs = append(errs, c.proc.Stop()) // also deletes c.dir
	case c.dir != "":
		errs = append(errs, os.RemoveAll(c.dir)) // prepared, never started
	}
	if c.jail != nil {
		errs = append(errs, c.jail.remove())
	}
	if err := errors.Join(errs...); err != nil {
		return &CleanupError{ID: c.id, Err: err}
	}
	return nil
}

// CleanupError means Destroy could not remove all of a clone: some of it
// (its process, folder, jail or cgroup) may still be on the host. Restore
// returns it too, joined with its own error, when cleaning up after a
// failed restore fails; find it with errors.As. sandboxd retires the
// clone's slot (and in jailer mode its UID) when it sees one, so nothing
// new reuses what might be left.
type CleanupError struct {
	// ID is the clone's ID.
	ID string
	// Err is what went wrong.
	Err error
}

func (e *CleanupError) Error() string { return fmt.Sprintf("clone: destroy %s: %v", e.ID, e.Err) }

// Unwrap returns Err.
func (e *CleanupError) Unwrap() error { return e.Err }
