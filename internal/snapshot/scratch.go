package snapshot

import (
	"fmt"
	"os"
)

// CreateScratch creates a blank scratch disk of sizeMiB at path: a sparse
// file, all zeros, that takes no disk space until the guest writes to it.
// It never overwrites an existing file. The file is mode 0600; in jailer
// mode the caller then gives it to the jailed user.
//
// The VM a snapshot is made from and every clone of it get their scratch
// disk from this one function, so the disks match exactly: same size, same
// (zero) contents. That matters because the guest kernel may have read the
// start of /dev/vdb at boot (looking for partitions) and keeps what it read
// in the memory that the snapshot freezes. The scratch disk is not mounted
// in the snapshot (ADR-0003, decision 4).
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
