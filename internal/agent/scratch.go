package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"

	"golang.org/x/sys/unix"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/redwan-cpp/MicroVM/internal/gen/sandbox/agent/v1"
)

const (
	// ScratchDevice is the clone's scratch disk in the guest: the second
	// drive, after the root disk (/dev/vda). The host attaches its own
	// copy of a formatted ext4 template to every clone (internal/snapshot).
	ScratchDevice = "/dev/vdb"
	// ScratchDir is where MountScratch mounts it. The folder is in the
	// read-only root disk (guest/build-rootfs.sh makes it).
	ScratchDir = "/work"
)

// Scratch mounts the scratch disk. Server uses KernelScratch; tests use a
// fake, because mounting needs root.
type Scratch interface {
	// Mount mounts the scratch disk, which must be sizeBytes long.
	Mount(sizeBytes uint64) error
	// Dir is where Mount mounts it: the default working folder of every
	// command afterwards.
	Dir() string
}

// KernelScratch is the real Scratch: ScratchDevice at ScratchDir.
type KernelScratch struct{}

// Mount checks the size of ScratchDevice, drops whatever the guest kernel
// has cached of it, and mounts it at ScratchDir as ext4, nodev and nosuid.
//
// Dropping the cache matters because of the snapshot. The VM a snapshot is
// made from has a blank placeholder disk at /dev/vdb, and its kernel may
// have read the start of it at boot (looking for partitions). Those
// all-zero blocks are in the kernel's page cache, which is guest memory,
// which the snapshot freezes. Every clone then gets a formatted disk in
// that place, so before mounting it the kernel must forget the zeros:
// BLKFLSBUF does that (it needs CAP_SYS_ADMIN, which the agent has as root
// in the guest). The week-4 three-clone test is the proof that it works.
func (KernelScratch) Mount(sizeBytes uint64) error {
	f, err := os.Open(ScratchDevice)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("no %s: the host attached no scratch disk", ScratchDevice)
	}
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck // opened read-only

	// lseek to the end of a block device returns its size in bytes.
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return fmt.Errorf("size of %s: %w", ScratchDevice, err)
	}
	if size < 0 || uint64(size) != sizeBytes {
		return fmt.Errorf("%s is %d bytes, the host attached %d", ScratchDevice, size, sizeBytes)
	}

	raw, err := f.SyscallConn()
	if err != nil {
		return fmt.Errorf("%s: %w", ScratchDevice, err)
	}
	var ioctlErr error
	if err := raw.Control(func(fd uintptr) {
		ioctlErr = unix.IoctlSetInt(int(fd), unix.BLKFLSBUF, 0)
	}); err != nil {
		return fmt.Errorf("%s: %w", ScratchDevice, err)
	}
	if ioctlErr != nil {
		return fmt.Errorf("ioctl BLKFLSBUF on %s: %w", ScratchDevice, ioctlErr)
	}

	// nodev: device files on the disk do not work; nosuid: set-user-ID
	// programs on it do not gain privileges.
	if err := unix.Mount(ScratchDevice, ScratchDir, "ext4", unix.MS_NODEV|unix.MS_NOSUID, ""); err != nil {
		return fmt.Errorf("mount %s at %s: %w", ScratchDevice, ScratchDir, err)
	}
	return nil
}

// Dir is ScratchDir.
func (KernelScratch) Dir() string { return ScratchDir }

// MountScratch mounts the scratch disk once. A second call for a disk of
// the same size changes nothing and succeeds; one for another size fails.
func (s *Server) MountScratch(_ context.Context, req *agentv1.MountScratchRequest) (*agentv1.MountScratchResponse, error) {
	size := req.GetSizeBytes()
	if size == 0 {
		return nil, status.Error(codes.InvalidArgument, "size_bytes must be the size of the scratch disk")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.scratchSize == size:
		return &agentv1.MountScratchResponse{}, nil // already mounted
	case s.scratchSize != 0:
		return nil, status.Errorf(codes.FailedPrecondition,
			"the scratch disk is already mounted with %d bytes, not %d", s.scratchSize, size)
	}
	if err := s.scratch.Mount(size); err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "mount the scratch disk: %v", err)
	}
	s.scratchSize = size
	return &agentv1.MountScratchResponse{}, nil
}

// workingDir is the working folder of a command that asked for none: the
// scratch disk once it is mounted, "/" before.
func (s *Server) workingDir() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.scratchSize != 0 {
		return s.scratch.Dir()
	}
	return "/"
}
