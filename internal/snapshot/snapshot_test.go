package snapshot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/redwan-cpp/MicroVM/internal/state"
)

func TestClasses(t *testing.T) {
	if got := ClassNames(); !slices.Equal(got, []string{"small", "medium"}) {
		t.Errorf("ClassNames = %v", got)
	}
	small, err := LookupClass("small")
	if err != nil || small.VCPUs != 1 || small.MemoryMiB != 256 || small.ScratchMiB != 64 {
		t.Errorf("small = %+v, %v; want 1 vCPU, 256 MiB, 64 MiB scratch", small, err)
	}
	medium, err := LookupClass("medium")
	if err != nil || medium.VCPUs != 2 || medium.MemoryMiB != 512 || medium.ScratchMiB != 128 {
		t.Errorf("medium = %+v, %v; want 2 vCPU, 512 MiB, 128 MiB scratch", medium, err)
	}
	if _, err := LookupClass("huge"); err == nil || !strings.Contains(err.Error(), "small, medium") {
		t.Errorf("LookupClass(huge) = %v, want an error listing the classes", err)
	}
	for _, c := range Classes {
		// The cgroup must leave Firecracker room beyond guest memory, or the
		// VM is killed as soon as the guest uses all of its memory.
		if c.CgroupMemoryMiB <= c.MemoryMiB || c.CgroupPids <= c.VCPUs+2 {
			t.Errorf("%s: cgroup limits %d MiB / %d pids are too tight for %d MiB / %d vCPU",
				c.Name, c.CgroupMemoryMiB, c.CgroupPids, c.MemoryMiB, c.VCPUs)
		}
	}
}

func TestCreateScratch(t *testing.T) {
	path := filepath.Join(t.TempDir(), ScratchName)
	if err := CreateScratch(path, 64); err != nil {
		t.Fatalf("CreateScratch: %v", err)
	}
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		t.Fatal(err)
	}
	if st.Size != 64<<20 {
		t.Errorf("size %d, want 64 MiB", st.Size)
	}
	if st.Blocks != 0 {
		t.Errorf("%d blocks allocated, want a sparse file with none", st.Blocks)
	}
	if st.Mode&0o777 != 0o600 {
		t.Errorf("mode %04o, want 0600", st.Mode&0o777)
	}
	if err := CreateScratch(path, 64); err == nil {
		t.Error("second CreateScratch on the same path succeeded, want an error (never overwrite)")
	}
	if err := CreateScratch(filepath.Join(t.TempDir(), "zero"), 0); err == nil {
		t.Error("CreateScratch with 0 MiB succeeded, want an error")
	}
}

func TestVMGenIDBound(t *testing.T) {
	for ls, want := range map[string]bool{
		"/sys/bus/platform/drivers/vmgenid:\nVMGENCTR:00\nbind\nuevent\nunbind\n": true,
		"/sys/bus/platform/drivers/vmgenid:\nbind\nuevent\nunbind\n":              false,
		"": false,
		"/sys/bus/acpi/drivers/vmgenid:\nQEMUVGID:00\nbind\nunbind\nuevent": true,
	} {
		if got := vmgenidBound(ls); got != want {
			t.Errorf("vmgenidBound(%q) = %v, want %v", ls, got, want)
		}
	}
}

func TestNewVersionIDSortsByTime(t *testing.T) {
	a := newVersionID(time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC))
	b := newVersionID(time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC))
	if !versionName.MatchString(a) || !versionName.MatchString(b) {
		t.Fatalf("IDs %q, %q do not match %v", a, b, versionName)
	}
	if !strings.HasPrefix(a, "20261118T090000Z-") || a >= b {
		t.Errorf("IDs %q, %q; want time-sortable names", a, b)
	}
}

func TestManifestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := &Manifest{Format: ManifestFormat, Class: "small", VCPUs: 1, FirecrackerVersion: "v1.17.0",
		CreatedAt: time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC), VMGenID: true}
	if err := writeManifest(dir, want); err != nil {
		t.Fatalf("writeManifest: %v", err)
	}
	if info, err := os.Stat(filepath.Join(dir, ManifestName)); err != nil || info.Mode().Perm() != 0o444 {
		t.Errorf("manifest: %v, %v; want mode 0444", info, err)
	}
	got, err := readManifest(dir)
	if err != nil {
		t.Fatalf("readManifest: %v", err)
	}
	if *got != *want {
		t.Errorf("read back %+v, want %+v", got, want)
	}
}

func TestReadManifestIsStrict(t *testing.T) {
	for name, body := range map[string]string{
		"unknown field": `{"format": 1, "class": "small", "surprise": true}`,
		"older format":  `{"format": 1, "class": "small"}`,
		"newer format":  `{"format": 3, "class": "small"}`,
		"not json":      `hello`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, ManifestName), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := readManifest(dir); err == nil {
				t.Errorf("readManifest(%s) succeeded, want an error", body)
			}
		})
	}
}

// fakeVersion puts a version of class into the store by hand (no VM):
// small files, real hashes, mode 0444, and "current" pointing at it.
func fakeVersion(t *testing.T, root *state.Root, class Class, id string) *Version {
	t.Helper()
	classDir := filepath.Join(root.SnapshotsDir(), class.Name)
	dir := filepath.Join(classDir, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	m := &Manifest{Format: ManifestFormat, Class: class.Name, VCPUs: class.VCPUs, MemoryMiB: class.MemoryMiB,
		ScratchMiB: class.ScratchMiB, FirecrackerVersion: "v1.17.0"}
	for name, sum := range map[string]*string{StateName: &m.StateSHA256, MemName: &m.MemSHA256, RootfsName: &m.RootfsSHA256,
		ScratchName: &m.ScratchSHA256} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(id+name), 0o444); err != nil { //nolint:gosec // G306: store files are 0444 by design
			t.Fatal(err)
		}
		var err error
		if *sum, err = hashFile(t.Context(), path); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeManifest(dir, m); err != nil {
		t.Fatal(err)
	}
	if err := setCurrent(classDir, id); err != nil {
		t.Fatal(err)
	}
	return &Version{ID: id, Dir: dir, Manifest: m}
}

// openState opens a state folder for one test. Not in t.TempDir(): its
// name includes the test's name, and the socket paths would get too long.
func openState(t *testing.T) *state.Root {
	t.Helper()
	dir, err := os.MkdirTemp("", "snapshot-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	root, err := state.Open(dir, state.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return root
}

const testID = "20261118T090000Z-aaaaaa"

func TestOpenAndCurrent(t *testing.T) {
	root := openState(t)
	want := fakeVersion(t, root, Classes[0], testID)

	s, err := Open(t.Context(), root, "v1.17.0")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	got, err := s.Current("small")
	if err != nil || got.ID != want.ID || got.Dir != want.Dir || got.Path(MemName) != filepath.Join(want.Dir, MemName) {
		t.Errorf("Current(small) = %+v, %v; want %+v", got, err, want)
	}
	// medium was never built: Open is fine, Current says what to do.
	if _, err := s.Current("medium"); err == nil || !strings.Contains(err.Error(), errRebuild) {
		t.Errorf("Current(medium) = %v, want %q", err, errRebuild)
	}
	if _, err := s.Current("huge"); err == nil || !strings.Contains(err.Error(), "unknown size class") {
		t.Errorf("Current(huge) = %v, want unknown size class", err)
	}
}

func TestOpenRefusesABadSnapshot(t *testing.T) {
	tests := map[string]struct {
		change  func(t *testing.T, v *Version)
		version string
		want    string
	}{
		"other Firecracker": {
			change: func(*testing.T, *Version) {}, version: "v1.18.0",
			want: "made by Firecracker v1.17.0, but this is Firecracker v1.18.0: run make snapshots",
		},
		"changed memory file": {
			change: func(t *testing.T, v *Version) {
				replaceFile(t, v.Path(MemName), "tampered", 0o444)
			},
			want: "mem.snap has SHA-256",
		},
		"writable file": {
			change: func(t *testing.T, v *Version) {
				if err := os.Chmod(v.Path(StateName), 0o644); err != nil { //nolint:gosec // G302: the test makes a store file writable on purpose
					t.Fatal(err)
				}
			},
			want: "vm.snap has mode 0644",
		},
		"changed scratch template": {
			change: func(t *testing.T, v *Version) {
				replaceFile(t, v.Path(ScratchName), "tampered", 0o444)
			},
			want: "scratch.ext4 has SHA-256",
		},
		"missing root disk": {
			change: func(t *testing.T, v *Version) {
				if err := os.Remove(v.Path(RootfsName)); err != nil {
					t.Fatal(err)
				}
			},
			want: "rootfs.ext4",
		},
		"class changed shape": {
			change: func(t *testing.T, v *Version) {
				v.Manifest.MemoryMiB = 128
				if err := os.Remove(v.Path(ManifestName)); err != nil {
					t.Fatal(err)
				}
				if err := writeManifest(v.Dir, v.Manifest); err != nil {
					t.Fatal(err)
				}
			},
			want: "but the class is now",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			root := openState(t)
			v := fakeVersion(t, root, Classes[0], testID)
			tt.change(t, v)
			version := tt.version
			if version == "" {
				version = "v1.17.0"
			}
			_, err := Open(t.Context(), root, version)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Open = %v, want an error containing %q", err, tt.want)
			}
		})
	}
}

func TestOpenStopsWhenTheContextEnds(t *testing.T) {
	root := openState(t)
	fakeVersion(t, root, Classes[0], testID)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := Open(ctx, root, "v1.17.0"); !errors.Is(err, context.Canceled) {
		t.Errorf("Open = %v, want context.Canceled", err)
	}
}

func TestLinkInto(t *testing.T) {
	root := openState(t)
	v := fakeVersion(t, root, Classes[0], testID)
	_, dir, err := root.NewRunDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := v.LinkInto(dir); err != nil {
		t.Fatalf("LinkInto: %v", err)
	}
	for _, name := range []string{StateName, MemName, RootfsName} {
		var store, clone syscall.Stat_t
		if syscall.Stat(v.Path(name), &store) != nil || syscall.Stat(filepath.Join(dir, name), &clone) != nil {
			t.Fatalf("%s is missing", name)
		}
		if store.Ino != clone.Ino || store.Nlink != 2 {
			t.Errorf("%s: inode %d vs %d, %d links; want the same file, 2 links", name, store.Ino, clone.Ino, store.Nlink)
		}
	}
	if err := v.LinkInto(dir); err == nil {
		t.Error("second LinkInto into the same folder succeeded, want an error")
	}
}

func TestSetCurrentSwapsTheLink(t *testing.T) {
	root := openState(t)
	fakeVersion(t, root, Classes[0], "20261118T090000Z-aaaaaa")
	fakeVersion(t, root, Classes[0], "20261118T100000Z-bbbbbb")
	classDir := filepath.Join(root.SnapshotsDir(), "small")
	if got, _ := os.Readlink(filepath.Join(classDir, currentName)); got != "20261118T100000Z-bbbbbb" {
		t.Errorf("current -> %q, want the newer version", got)
	}
	entries, _ := os.ReadDir(classDir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".current-") {
			t.Errorf("left behind %s", e.Name())
		}
	}
}

func TestPruneKeepsTheNewestAndCurrent(t *testing.T) {
	root := openState(t)
	ids := []string{
		"20261118T090000Z-aaaaaa", "20261118T100000Z-bbbbbb",
		"20261118T110000Z-cccccc", "20261118T120000Z-dddddd",
	}
	for _, id := range ids {
		fakeVersion(t, root, Classes[0], id)
	}
	classDir := filepath.Join(root.SnapshotsDir(), "small")
	// Point current at an old version: it must survive too.
	if err := setCurrent(classDir, ids[0]); err != nil {
		t.Fatal(err)
	}

	deleted, err := Prune(root, "small", 2)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if !slices.Equal(deleted, []string{ids[1]}) {
		t.Errorf("deleted %v, want only %v", deleted, ids[1])
	}
	for _, id := range []string{ids[0], ids[2], ids[3]} {
		if _, err := os.Stat(filepath.Join(classDir, id)); err != nil {
			t.Errorf("%s: %v, want it kept", id, err)
		}
	}
}

// replaceFile replaces path with a new file (a new inode, as `make guest`
// would) holding content.
func replaceFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	tmp := path + ".new"
	if err := os.WriteFile(tmp, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

func TestManifestJSONNames(t *testing.T) {
	// The field names are a file format; pin them.
	data, err := json.Marshal(Manifest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"format", "class", "vcpus", "memory_mib", "scratch_mib", "firecracker_version",
		"kernel_sha256", "rootfs_sha256", "agent_version", "vm_snap_sha256", "mem_snap_sha256", "scratch_sha256",
		"vmgenid", "created_at"} {
		if !strings.Contains(string(data), `"`+key+`"`) {
			t.Errorf("manifest JSON has no %q: %s", key, data)
		}
	}
}

func TestOpenRefusesAWeek3Store(t *testing.T) {
	root := openState(t)
	v := fakeVersion(t, root, Classes[0], testID)
	// A week-3 manifest: format 1, no scratch template.
	if err := os.Remove(v.Path(ManifestName)); err != nil {
		t.Fatal(err)
	}
	v.Manifest.Format = 1
	if err := writeManifest(v.Dir, v.Manifest); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(t.Context(), root, "v1.17.0"); err == nil || !strings.Contains(err.Error(), "run make snapshots") {
		t.Errorf("Open of a format-1 store = %v, want run make snapshots", err)
	}
}

// allocated returns how many bytes of disk the file at path takes.
func allocated(t *testing.T, path string) int64 {
	t.Helper()
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		t.Fatal(err)
	}
	return st.Blocks * 512 // st_blocks counts 512-byte units
}

func TestCopySparseKeepsHoles(t *testing.T) {
	dir := t.TempDir()
	from := filepath.Join(dir, "sparse")
	const size = 16 << 20
	f, err := os.Create(from) //nolint:gosec // G304: a test file
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	// Data at the start, in the middle and at the very end; holes between.
	for i, at := range []int64{0, 5<<20 + 123, size - 4096} {
		if _, err := f.WriteAt(bytes.Repeat([]byte{'a' + byte(i)}, 4096), at); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	to := filepath.Join(dir, "copy")
	if err := copySparse(from, to, 0o600); err != nil {
		t.Fatalf("copySparse: %v", err)
	}
	want, _ := os.ReadFile(from) //nolint:gosec // G304: a test file
	got, _ := os.ReadFile(to)    //nolint:gosec // G304: a test file
	if !bytes.Equal(got, want) {
		t.Fatal("the copy's contents differ")
	}
	if info, err := os.Stat(to); err != nil || info.Mode().Perm() != 0o600 || info.Size() != size {
		t.Errorf("copy: %v, %v; want mode 0600 and %d bytes", info, err, size)
	}
	// The copy takes no more disk space than the original: the holes
	// stayed holes. (The original's own blocks are the bound, since a
	// filesystem may allocate a little more than the data.)
	if a, b := allocated(t, from), allocated(t, to); b > a || b >= size/2 {
		t.Errorf("copy allocates %d bytes, the original %d (of %d): the holes were filled", b, a, size)
	}
	if err := copySparse(from, to, 0o600); err == nil {
		t.Error("copySparse over an existing file succeeded, want an error (never overwrite)")
	}

	// A file with no data at all is copied as one hole.
	empty := filepath.Join(dir, "empty")
	if err := CreateScratch(empty, 1); err != nil {
		t.Fatal(err)
	}
	if err := copySparse(empty, filepath.Join(dir, "empty-copy"), 0o600); err != nil {
		t.Fatalf("copySparse of a hole: %v", err)
	}
	if info, err := os.Stat(filepath.Join(dir, "empty-copy")); err != nil || info.Size() != 1<<20 ||
		allocated(t, filepath.Join(dir, "empty-copy")) != 0 {
		t.Errorf("copy of an all-hole file: %v, %v; want 1 MiB, nothing allocated", info, err)
	}
}

func TestScratchTemplateAndCopy(t *testing.T) {
	mkfs, err := FindMkfs()
	if err != nil {
		t.Skip(err)
	}
	root := openState(t)
	v := fakeVersion(t, root, Classes[0], testID)
	// Replace the fake template with a real one, made the way Build does.
	if err := os.Remove(v.Path(ScratchName)); err != nil {
		t.Fatal(err)
	}
	if err := makeScratchTemplate(t.Context(), mkfs, v.Path(ScratchName), 8); err != nil {
		t.Fatalf("makeScratchTemplate: %v", err)
	}
	info, err := os.Stat(v.Path(ScratchName))
	if err != nil || info.Size() != 8<<20 {
		t.Fatalf("template: %v, %v; want exactly 8 MiB, the placeholder's size", info, err)
	}
	// An ext4 superblock starts at byte 1024; its magic number 0xEF53 is at
	// offset 0x38 in it.
	head := make([]byte, 2048)
	tf, err := os.Open(v.Path(ScratchName))
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadFull(tf, head)
	_ = tf.Close()
	if err != nil || head[1024+0x38] != 0x53 || head[1024+0x39] != 0xef {
		t.Errorf("no ext4 superblock in the template (%v)", err)
	}
	if a := allocated(t, v.Path(ScratchName)); a > 2<<20 {
		t.Errorf("template allocates %d bytes of 8 MiB, want a sparse file", a)
	}

	_, dir, err := root.NewRunDir()
	if err != nil {
		t.Fatal(err)
	}
	clone := filepath.Join(dir, ScratchName)
	if err := v.CopyScratch(clone); err != nil {
		t.Fatalf("CopyScratch: %v", err)
	}
	var store, mine syscall.Stat_t
	if syscall.Stat(v.Path(ScratchName), &store) != nil || syscall.Stat(clone, &mine) != nil {
		t.Fatal("a scratch disk is missing")
	}
	// A copy, never a link: each clone's writes stay its own.
	if store.Ino == mine.Ino || store.Nlink != 1 || mine.Nlink != 1 {
		t.Errorf("the clone's scratch disk shares the template's inode (%d, %d links)", mine.Ino, mine.Nlink)
	}
	if mine.Mode&0o777 != 0o600 || mine.Size != store.Size || mine.Blocks > store.Blocks {
		t.Errorf("copy: mode %04o, %d bytes, %d blocks; want 0600, %d bytes, at most %d blocks",
			mine.Mode&0o777, mine.Size, mine.Blocks, store.Size, store.Blocks)
	}
}
