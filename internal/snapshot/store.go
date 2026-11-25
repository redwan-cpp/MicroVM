package snapshot

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"

	"github.com/redwan-cpp/MicroVM/internal/state"
)

// currentName is the symlink to the newest version of a class.
const currentName = "current"

// versionName is the shape of a version folder's name (newVersionID).
var versionName = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z-[0-9a-f]{6}$`)

// Version is one snapshot version in the store.
type Version struct {
	// ID is the version folder's name, for example 20261118T224644Z-1a2b3c.
	ID string
	// Dir is the version folder.
	Dir string
	// Manifest describes it.
	Manifest *Manifest
}

// Path returns the path of one of the version's files (StateName, MemName
// or RootfsName).
func (v *Version) Path(name string) string { return filepath.Join(v.Dir, name) }

// LinkInto hard-links the files a restore needs (vm.snap, mem.snap and
// rootfs.ext4) into dir, a clone's run folder or chroot, under the names
// the snapshot uses. They are links, never copies: every clone maps the
// same mem.snap, so they share its page-cache pages (ADR-0003, decision
// 3), and nothing is copied on the restore path. dir must be on the
// store's filesystem (state.Open checks that for the state folder).
func (v *Version) LinkInto(dir string) error {
	for _, name := range []string{StateName, MemName, RootfsName} {
		if err := linkFile(v.Path(name), filepath.Join(dir, name)); err != nil {
			return err
		}
	}
	return nil
}

// Store is an opened, checked snapshot store. It is safe for concurrent
// use: it never changes after Open.
type Store struct {
	root     *state.Root
	versions map[string]*Version // class name → its current version
}

// Open opens the snapshot store in root and checks, once, the current
// version of every class that has one. A class without snapshots is fine
// here (Current reports it); a broken one fails Open:
//
//   - the manifest must match the class as defined today (vCPUs, memory,
//     scratch size), name firecrackerVersion (vmm.BinaryVersion of the
//     Firecracker that will load it), since a snapshot only loads in the
//     Firecracker version that made it, and name this machine's
//     architecture (HostArch);
//   - vm.snap, mem.snap, rootfs.ext4 and the scratch template
//     scratch.ext4 must have the SHA-256 the manifest recorded, and mode
//     0444.
//
// Hashing reads every file once (about a second for both classes); it
// happens here and never when a clone is restored, so restore times never
// include it. Open pins each class to the version that was current at that
// moment: a later build does not change what this Store restores.
func Open(ctx context.Context, root *state.Root, firecrackerVersion string) (*Store, error) {
	arch, err := HostArch()
	if err != nil {
		return nil, err
	}
	s := &Store{root: root, versions: make(map[string]*Version)}
	for _, class := range Classes {
		v, err := openCurrent(ctx, root, class, firecrackerVersion, arch)
		if errors.Is(err, errNotBuilt) {
			continue
		}
		if err != nil {
			return nil, err
		}
		s.versions[class.Name] = v
	}
	return s, nil
}

// errNotBuilt means a class has no snapshot yet (no "current" link).
var errNotBuilt = errors.New("snapshot: not built yet")

// openCurrent loads and checks the current version of class, or returns
// errNotBuilt.
func openCurrent(ctx context.Context, root *state.Root, class Class, firecrackerVersion, arch string) (*Version, error) {
	classDir := filepath.Join(root.SnapshotsDir(), class.Name)
	id, err := os.Readlink(filepath.Join(classDir, currentName))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, errNotBuilt
	}
	if err != nil {
		return nil, fmt.Errorf("snapshot: %w", err)
	}
	if !versionName.MatchString(id) {
		return nil, fmt.Errorf("snapshot: %s/%s points at %q, not a version folder", classDir, currentName, id)
	}
	v := &Version{ID: id, Dir: filepath.Join(classDir, id)}
	if v.Manifest, err = readManifest(v.Dir); err != nil {
		return nil, err
	}
	if err := check(ctx, v, class, firecrackerVersion, arch); err != nil {
		return nil, fmt.Errorf("snapshot: %s version %s: %w", class.Name, id, err)
	}
	return v, nil
}

// errRebuild ends every "this snapshot cannot be used" message.
const errRebuild = "run make snapshots"

// check verifies one version against its manifest, the class and this
// machine.
func check(ctx context.Context, v *Version, class Class, firecrackerVersion, arch string) error {
	m := v.Manifest
	if m.Arch != arch {
		return fmt.Errorf("made on %s, but this machine is %s: a snapshot only restores on the architecture that made it: %s",
			orUnknown(m.Arch), arch, errRebuild)
	}
	if m.Class != class.Name || m.VCPUs != class.VCPUs || m.MemoryMiB != class.MemoryMiB || m.ScratchMiB != class.ScratchMiB {
		return fmt.Errorf("made for %s with %d vCPU, %d MiB, %d MiB scratch, but the class is now %d vCPU, %d MiB, %d MiB scratch: %s",
			m.Class, m.VCPUs, m.MemoryMiB, m.ScratchMiB, class.VCPUs, class.MemoryMiB, class.ScratchMiB, errRebuild)
	}
	if m.FirecrackerVersion != firecrackerVersion {
		return fmt.Errorf("made by Firecracker %s, but this is Firecracker %s: %s",
			m.FirecrackerVersion, firecrackerVersion, errRebuild)
	}
	for _, f := range []struct{ name, want string }{
		{StateName, m.StateSHA256},
		{MemName, m.MemSHA256},
		{RootfsName, m.RootfsSHA256},
		{ScratchName, m.ScratchSHA256},
	} {
		info, err := os.Stat(v.Path(f.name))
		if err != nil {
			return fmt.Errorf("%w: %s", err, errRebuild)
		}
		if info.Mode().Perm() != storeFileMode {
			return fmt.Errorf("%s has mode %04o, want %04o (read-only for everyone): %s",
				f.name, info.Mode().Perm(), storeFileMode, errRebuild)
		}
		got, err := hashFile(ctx, v.Path(f.name))
		if err != nil {
			return err
		}
		if got != f.want {
			return fmt.Errorf("%s has SHA-256 %s, the manifest says %s: the file changed: %s", f.name, got, f.want, errRebuild)
		}
	}
	return nil
}

// orUnknown names an architecture for a message.
func orUnknown(arch string) string {
	if arch == "" {
		return "an unknown architecture"
	}
	return arch
}

// Classes returns the current version of every class that has one,
// smallest class first.
func (s *Store) Classes() []*Version {
	var out []*Version
	for _, c := range Classes {
		if v, ok := s.versions[c.Name]; ok {
			out = append(out, v)
		}
	}
	return out
}

// Root is the state folder the store lives in.
func (s *Store) Root() *state.Root { return s.root }

// Current returns the version of class that Open checked.
func (s *Store) Current(class string) (*Version, error) {
	v, ok := s.versions[class]
	if !ok {
		if _, err := LookupClass(class); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("snapshot: no %s snapshot in %s: %s", class, s.root.SnapshotsDir(), errRebuild)
	}
	return v, nil
}

// Prune deletes all but the newest keep versions of class, never the
// current one. A clone that is still running keeps working: its hard links
// keep the files' contents alive until it is destroyed. But a running
// sandboxd keeps restoring from the version it opened, so `sandboxd
// build-snapshots` only prunes while it holds the state folder's lock,
// which a running daemon never lets go of (state.Root.Lock). It returns
// the IDs it deleted.
func Prune(root *state.Root, class string, keep int) ([]string, error) {
	classDir := filepath.Join(root.SnapshotsDir(), class)
	entries, err := os.ReadDir(classDir)
	if err != nil {
		return nil, fmt.Errorf("snapshot: %w", err)
	}
	current, _ := os.Readlink(filepath.Join(classDir, currentName))

	var ids []string
	for _, e := range entries {
		if e.IsDir() && versionName.MatchString(e.Name()) {
			ids = append(ids, e.Name())
		}
	}
	slices.Sort(ids) // oldest first: the names start with the build time
	var deleted []string
	for i, id := range ids {
		if i >= len(ids)-keep || id == current {
			continue
		}
		if err := os.RemoveAll(filepath.Join(classDir, id)); err != nil {
			return deleted, fmt.Errorf("snapshot: delete %s: %w", id, err)
		}
		deleted = append(deleted, id)
	}
	return deleted, nil
}
