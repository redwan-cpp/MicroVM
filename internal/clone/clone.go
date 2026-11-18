// Package clone restores microVMs from the base snapshots in
// internal/snapshot: one clone per run, destroyed afterwards.
//
// A restore takes five steps, each timed (Timings):
//
//  1. Prepare: a fresh run folder with hard links to the snapshot's
//     vm.snap, mem.snap and rootfs.ext4, and a new blank scratch disk.
//  2. Process start: an empty Firecracker in that folder.
//  3. LoadSnapshot: Firecracker maps mem.snap copy-on-write and resumes
//     the VM exactly where the snapshot paused it, agent included.
//  4. Agent ready: the host connects to the agent (the guest's listening
//     socket survived the snapshot; the old connections did not).
//  5. De-twin: every clone woke up with the same clock and the same
//     random-number state as the snapshot, so the host sets the guest
//     clock (SyncTime) and mixes fresh entropy into its RNG (Reseed). Both
//     must succeed, or the clone is destroyed (ADR-0003, decision 5).
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
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/redwan-cpp/MicroVM/internal/agentclient"
	agentv1 "github.com/redwan-cpp/MicroVM/internal/gen/sandbox/agent/v1"
	"github.com/redwan-cpp/MicroVM/internal/snapshot"
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
}

// Timings is how long each step of a restore took (see the package
// comment), measured on the host.
type Timings struct {
	// Mode is the mode these numbers were measured in.
	Mode Mode
	// Prepare is making the run folder: hard links and the scratch disk.
	Prepare time.Duration
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
	return fmt.Sprintf("%s: prepare %s, process start %s, LoadSnapshot %s, agent ready %s, de-twin %s, total %s",
		t.Mode, ms(t.Prepare), ms(t.ProcessStart), ms(t.LoadSnapshot), ms(t.AgentReady), ms(t.DeTwin), ms(t.Total))
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
	proc    *vmm.Process
	agent   *agentclient.Client
	timings Timings

	destroyOnce sync.Once
	destroyErr  error
}

// Restore makes a new clone of the current snapshot of class from store
// (see the package comment for the steps). ctx bounds the restore only;
// the clone runs until Destroy. On error nothing is left behind.
func Restore(ctx context.Context, store *snapshot.Store, class string, opts Options) (_ *Clone, err error) {
	began := time.Now()
	c := &Clone{mode: Rootless}
	if c.class, err = snapshot.LookupClass(class); err != nil {
		return nil, err
	}
	if c.version, err = store.Current(class); err != nil {
		return nil, err
	}

	// 1. Prepare the run folder.
	id, dir, err := store.Root().NewRunDir()
	if err != nil {
		return nil, err
	}
	c.id = id
	if err := prepare(c.version, c.class, dir); err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("clone: %w", err)
	}
	prepared := time.Now()

	// 2. Start Firecracker. From here on, Destroy cleans up everything.
	c.proc, err = vmm.Start(ctx, vmm.Config{Binary: opts.Firecracker, RunDir: dir})
	if err != nil {
		return nil, fmt.Errorf("clone: %w", err)
	}
	defer func() {
		if err != nil {
			_ = c.Destroy()
		}
	}()
	started := time.Now()

	// 3. Load the snapshot, by the relative names in the run folder.
	if err := c.proc.Client().LoadSnapshot(ctx, vmm.SnapshotLoad{
		SnapshotPath: snapshot.StateName,
		MemBackend:   vmm.MemoryBackend{BackendType: "File", BackendPath: snapshot.MemName},
		ResumeVM:     true,
	}); err != nil {
		return nil, c.withConsole(fmt.Errorf("clone: load snapshot: %w", err))
	}
	loaded := time.Now()

	// 4. Connect to the agent.
	readyCtx, cancel := context.WithTimeout(ctx, agentReadyTimeout)
	defer cancel()
	if c.agent, _, err = agentclient.WaitReady(readyCtx, c.proc.VsockPath(), c.proc.Done()); err != nil {
		return nil, c.withConsole(fmt.Errorf("clone: %w", err))
	}
	ready := time.Now()

	// 5. De-twin.
	if err := deTwin(ctx, c.agent); err != nil {
		return nil, c.withConsole(fmt.Errorf("clone: de-twin: %w", err))
	}
	done := time.Now()

	c.timings = Timings{
		Mode:         c.mode,
		Prepare:      prepared.Sub(began),
		ProcessStart: started.Sub(prepared),
		LoadSnapshot: loaded.Sub(started),
		AgentReady:   ready.Sub(loaded),
		DeTwin:       done.Sub(ready),
		Total:        done.Sub(began),
	}
	return c, nil
}

// prepare fills a clone's folder: links to the snapshot's files and a new
// scratch disk, made by the same function as the build VM's.
func prepare(v *snapshot.Version, class snapshot.Class, dir string) error {
	if err := v.LinkInto(dir); err != nil {
		return err
	}
	return snapshot.CreateScratch(filepath.Join(dir, snapshot.ScratchName), class.ScratchMiB)
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

// withConsole adds the end of the guest console to err.
func (c *Clone) withConsole(err error) error {
	return fmt.Errorf("%w\nguest console (end):\n%s", err, c.proc.ConsoleTail(consoleTail))
}

// ID is the clone's run ID (state.NewID), also the name of its folder.
func (c *Clone) ID() string { return c.id }

// Mode is how the clone's Firecracker is confined.
func (c *Clone) Mode() Mode { return c.mode }

// Timings is how long each step of the restore took.
func (c *Clone) Timings() Timings { return c.timings }

// Class is the clone's size class.
func (c *Clone) Class() snapshot.Class { return c.class }

// Version is the snapshot version the clone was restored from.
func (c *Clone) Version() *snapshot.Version { return c.version }

// Dir is the clone's run folder on the host. Destroy deletes it.
func (c *Clone) Dir() string { return c.proc.Dir() }

// Pid is the clone's Firecracker process ID.
func (c *Clone) Pid() int { return c.proc.Pid() }

// Done is closed when the clone's Firecracker has exited.
func (c *Clone) Done() <-chan struct{} { return c.proc.Done() }

// ConsoleTail returns the end of the guest console, safe to print.
func (c *Clone) ConsoleTail(n int) string { return c.proc.ConsoleTail(n) }

// Exec runs a command in the clone (see agentclient.Client.Exec, whose
// rules all apply: ctx needs a deadline, and the host enforces the
// timeout and the output cap itself).
func (c *Clone) Exec(ctx context.Context, req *agentv1.ExecRequest, stdout, stderr io.Writer) (ExecResult, error) {
	res, err := c.agent.Exec(ctx, req, stdout, stderr)
	return ExecResult{Result: res, Mode: c.mode}, err
}

// Destroy ends the clone: it closes the agent connection, stops
// Firecracker (the VM and everything in it are gone) and deletes the run
// folder with its links and scratch disk. The snapshot itself is never
// touched. Destroy may be called any number of times.
func (c *Clone) Destroy() error {
	c.destroyOnce.Do(func() {
		if c.agent != nil {
			_ = c.agent.Close()
		}
		if err := c.proc.Stop(); err != nil {
			c.destroyErr = fmt.Errorf("clone: %w", err)
		}
	})
	return c.destroyErr
}
