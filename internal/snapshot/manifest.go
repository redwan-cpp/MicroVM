package snapshot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// ManifestFormat is the version of the manifest.json layout. Format 2
// (week 4) added the scratch disk template; format 3 (week 4b) added the
// architecture, the guest kernel's version and the cold boot time. A
// store of an older format lacks them, so opening it says "run make
// snapshots".
const ManifestFormat = 3

// maxManifestSize is the largest manifest.json we read.
const maxManifestSize = 64 << 10

// Manifest describes one snapshot version: what it was built from, with
// which tools, and the SHA-256 of every file it is made of. It is written
// once, at build time, and checked when the store is opened.
type Manifest struct {
	// Format is ManifestFormat.
	Format int `json:"format"`
	// Arch is the CPU architecture the snapshot was made on, "x86_64" or
	// "aarch64" (HostArch). It holds that machine's vCPU registers, so it
	// only restores on the same architecture.
	Arch string `json:"arch"`
	// Class is the size class's name, and VCPUs, MemoryMiB and ScratchMiB
	// its shape when the snapshot was made.
	Class      string `json:"class"`
	VCPUs      int    `json:"vcpus"`
	MemoryMiB  int    `json:"memory_mib"`
	ScratchMiB int    `json:"scratch_mib"`

	// FirecrackerVersion is the Firecracker that made it, for example
	// "v1.17.0". Only that version may load it.
	FirecrackerVersion string `json:"firecracker_version"`
	// KernelSHA256 is the guest kernel that booted the VM. It is not
	// needed to restore (the kernel is in guest memory), only recorded.
	KernelSHA256 string `json:"kernel_sha256"`
	// KernelVersion is what `uname -r` said in the build VM, for example
	// "6.1.155": the kernel every clone of this snapshot runs.
	KernelVersion string `json:"kernel_version"`
	// RootfsSHA256 is the root disk, the store's own rootfs.ext4.
	RootfsSHA256 string `json:"rootfs_sha256"`
	// AgentVersion is what sandbox-agent in the VM reported.
	AgentVersion string `json:"agent_version"`
	// StateSHA256 and MemSHA256 are vm.snap and mem.snap.
	StateSHA256 string `json:"vm_snap_sha256"`
	MemSHA256   string `json:"mem_snap_sha256"`
	// ScratchSHA256 is scratch.ext4, the formatted template every clone
	// gets its own copy of.
	ScratchSHA256 string `json:"scratch_sha256"`

	// VMGenID is true when the guest's vmgenid driver was bound to
	// Firecracker's VM generation ID device. Then the guest kernel
	// reseeds its random number generator by itself on every restore,
	// on top of the agent's Reseed (docs/learn/09-snapshots-and-cow.md).
	VMGenID bool `json:"vmgenid"`

	// ColdBootUS is the build VM's cold boot in microseconds, from
	// InstanceStart to the agent's first answer. It is the reference a
	// restore is compared with (sandboxd's Info call, sandboxctl ui),
	// measured on this machine when the snapshot was made, never a number
	// typed in by hand.
	ColdBootUS uint64 `json:"cold_boot_us"`

	// CreatedAt is when the snapshot was made (UTC).
	CreatedAt time.Time `json:"created_at"`
}

// readManifest reads dir/manifest.json.
func readManifest(dir string) (*Manifest, error) {
	f, err := os.Open(filepath.Join(dir, ManifestName)) //nolint:gosec // G304: a path in our own store
	if err != nil {
		return nil, fmt.Errorf("snapshot: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only file

	var m Manifest
	dec := json.NewDecoder(io.LimitReader(f, maxManifestSize))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("snapshot: %s: %w", filepath.Join(dir, ManifestName), err)
	}
	if m.Format != ManifestFormat {
		return nil, fmt.Errorf("snapshot: %s has format %d, this build reads %d: run make snapshots",
			filepath.Join(dir, ManifestName), m.Format, ManifestFormat)
	}
	return &m, nil
}

// writeManifest writes dir/manifest.json, read-only (0444), and syncs it
// to disk.
func writeManifest(dir string, m *Manifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("snapshot: encode manifest: %w", err)
	}
	path := filepath.Join(dir, ManifestName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o444) //nolint:gosec // G302,G304: read-only for everyone on purpose, like every store file
	if err != nil {
		return fmt.Errorf("snapshot: write manifest: %w", err)
	}
	_, err = f.Write(append(data, '\n'))
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("snapshot: write manifest: %w", err)
	}
	return nil
}

// hashFile returns the SHA-256 of the file at path, in hex. A memory file
// is hundreds of MiB, so it stops early if ctx ends.
func hashFile(ctx context.Context, path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // G304: a path in our own store or guest/out
	if err != nil {
		return "", fmt.Errorf("snapshot: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only file
	h := sha256.New()
	if _, err := io.Copy(h, ctxReader{ctx, f}); err != nil {
		return "", fmt.Errorf("snapshot: hash %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ctxReader is an io.Reader that fails once ctx has ended.
type ctxReader struct {
	ctx context.Context // only lives for one io.Copy
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
