package snapshot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

// mkfsTimeout bounds making one scratch template.
const mkfsTimeout = 30 * time.Second

// CreateScratch creates a blank scratch disk of sizeMiB at path: a sparse
// file, all zeros, that takes no disk space until the guest writes to it.
// It never overwrites an existing file. The file is mode 0600.
//
// It is the placeholder disk of the VM a snapshot is made from: the guest
// sees a /dev/vdb of the right size, never mounts it, and the snapshot
// records it under the relative name scratch.ext4. Every clone gets a
// copy of the store's formatted template there instead (CopyScratch), of
// exactly the same size. The guest kernel may have read the start of the
// placeholder at boot (looking for partitions) and keep those zeros in
// the memory that the snapshot freezes; the agent drops them before it
// mounts the clone's disk (agent.KernelScratch). The scratch disk is not
// mounted in the snapshot (ADR-0003, decision 4).
func CreateScratch(path string, sizeMiB int) error {
	if sizeMiB <= 0 {
		return fmt.Errorf("snapshot: scratch disk size %d MiB", sizeMiB)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // G304: a path in our own run folder
	if err != nil {
		return fmt.Errorf("snapshot: create scratch disk: %w", err)
	}
	// Truncate to the size: the file gets a length, but no data blocks.
	if err := f.Truncate(int64(sizeMiB) << 20); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return fmt.Errorf("snapshot: size scratch disk: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("snapshot: create scratch disk: %w", err)
	}
	return nil
}

// makeScratchTemplate makes the formatted scratch disk that every clone of
// a snapshot gets a copy of: an empty ext4 filesystem of sizeMiB at path,
// labelled "work", whose root folder belongs to root (uid 0) in the guest.
//
// It needs no root on the host: mkfs.ext4 writes into an ordinary file,
// and -d fills the new filesystem from a folder (an empty one here)
// without mounting anything, as guest/build-rootfs.sh does for the root
// disk. The file starts as a sparse placeholder of exactly the size of the
// snapshot's (CreateScratch), so the two always match, and
// assume_storage_prezeroed=1 tells mkfs.ext4 that it reads as zeros: then
// it writes only the filesystem's metadata (about 400 KiB of 64 MiB) and
// marks the inode tables as already zeroed, so a clone's kernel has no
// background zeroing to do either. That needs e2fsprogs 1.47 or newer.
func makeScratchTemplate(ctx context.Context, mkfs, path string, sizeMiB int) error {
	if err := CreateScratch(path, sizeMiB); err != nil {
		return err
	}
	empty, err := os.MkdirTemp(filepath.Dir(path), ".scratch-root-")
	if err != nil {
		return fmt.Errorf("snapshot: %w", err)
	}
	defer os.Remove(empty) //nolint:errcheck // an empty temporary folder

	ctx, cancel := context.WithTimeout(ctx, mkfsTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, mkfs, "-q", "-F", "-L", "work", //nolint:gosec // G204: mkfs comes from our own configuration
		"-E", "root_owner=0:0,assume_storage_prezeroed=1", "-d", empty, path)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("snapshot: make the scratch template with %s (e2fsprogs 1.47 or newer is needed): %w\n%s",
			mkfs, err, out)
	}
	return nil
}

// FindMkfs returns the mkfs.ext4 program: from PATH, or else from
// /usr/sbin or /sbin, which an ordinary user's PATH often lacks.
func FindMkfs() (string, error) {
	if path, err := exec.LookPath("mkfs.ext4"); err == nil {
		return path, nil
	}
	for _, path := range []string{"/usr/sbin/mkfs.ext4", "/sbin/mkfs.ext4"} {
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return path, nil
		}
	}
	return "", errors.New("snapshot: no mkfs.ext4 in PATH, /usr/sbin or /sbin: install e2fsprogs")
}

// CopyScratch gives a clone its own scratch disk at to: a copy of the
// version's formatted template, mode 0600, never overwriting a file. It is
// a real copy, never a hard link: what one clone writes to its disk must
// stay its own (threats T2 and T3). The copy keeps the template's holes,
// so it is as sparse as the template and takes about a millisecond.
func (v *Version) CopyScratch(to string) error {
	if err := copySparse(v.Path(ScratchName), to, 0o600); err != nil {
		return fmt.Errorf("snapshot: copy the scratch template: %w", err)
	}
	return nil
}

// copySparse copies the regular file from into a new file to (mode perm,
// never overwriting one), keeping its holes. A sparse file has "holes":
// ranges that read as zeros but take no disk space. A plain copy would
// read those zeros and write them, so the copy would take the file's full
// size on disk. Instead copySparse asks the kernel where the data is
// (lseek with SEEK_DATA and SEEK_HOLE, Linux 3.1 and newer; a filesystem
// that does not track holes reports all of the file as data) and copies
// only those ranges; the rest of the copy stays a hole.
func copySparse(from, to string, perm os.FileMode) (err error) {
	src, err := os.Open(from) //nolint:gosec // G304: a path in our own store
	if err != nil {
		return err
	}
	defer src.Close() //nolint:errcheck // read-only file
	info, err := src.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", from)
	}

	dst, err := os.OpenFile(to, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm) //nolint:gosec // G304: a path in our own run folder or chroot
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := dst.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			_ = os.Remove(to)
		}
	}()
	// The full length first: whatever is not written below is a hole.
	size := info.Size()
	if err := dst.Truncate(size); err != nil {
		return err
	}

	buf := make([]byte, 256<<10)
	for off := int64(0); off < size; {
		data, err := src.Seek(off, unix.SEEK_DATA)
		if errors.Is(err, unix.ENXIO) {
			break // no more data after off: the rest is one hole
		}
		if err != nil {
			return fmt.Errorf("find data in %s: %w", from, err)
		}
		hole, err := src.Seek(data, unix.SEEK_HOLE) // every file ends with a hole, at the latest at its end
		if err != nil {
			return fmt.Errorf("find a hole in %s: %w", from, err)
		}
		if _, err := io.CopyBuffer(io.NewOffsetWriter(dst, data), io.NewSectionReader(src, data, hole-data), buf); err != nil {
			return err
		}
		off = hole
	}
	return nil
}
