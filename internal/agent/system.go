package agent

import (
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// Clock sets the guest's wall-clock time. Server uses SystemClock; tests
// use a fake, because setting the real clock needs root.
type Clock interface {
	SetTime(t time.Time) error
}

// Entropy adds random bytes from the host to the guest kernel's random
// number generator. Server uses KernelEntropy; tests use a fake.
type Entropy interface {
	AddEntropy(b []byte) error
}

// SystemClock is the real Clock: it sets CLOCK_REALTIME, the clock behind
// `date` and time.Now, with the clock_settime system call. That needs the
// CAP_SYS_TIME capability, which the agent has as root in the guest.
type SystemClock struct{}

// SetTime sets the system clock to t.
func (SystemClock) SetTime(t time.Time) error {
	ts := unix.NsecToTimespec(t.UnixNano())
	if err := unix.ClockSettime(unix.CLOCK_REALTIME, &ts); err != nil {
		return fmt.Errorf("clock_settime(CLOCK_REALTIME): %w", err)
	}
	return nil
}

// KernelEntropy is the real Entropy. It works in two steps:
//
//  1. Write the bytes to /dev/urandom. The kernel mixes anything written
//     there into its entropy pool. (It does not count them as "trusted"
//     entropy; that would need the RNDADDENTROPY ioctl. We only need the
//     clones to differ, not more entropy.)
//  2. The RNDRESEEDCRNG ioctl makes the kernel rebuild its random number
//     generator from the pool right now. Without it, random numbers could
//     come from the old state, shared by every clone of a snapshot, until
//     the kernel's next periodic reseed.
//
// Step 2 needs CAP_SYS_ADMIN, which the agent has as root in the guest.
type KernelEntropy struct{}

// AddEntropy mixes b into the kernel's entropy pool and reseeds the RNG.
func (KernelEntropy) AddEntropy(b []byte) error {
	f, err := os.OpenFile("/dev/urandom", os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("open /dev/urandom: %w", err)
	}
	defer f.Close() //nolint:errcheck // nothing useful to do with a close error here

	if _, err := f.Write(b); err != nil {
		return fmt.Errorf("write /dev/urandom: %w", err)
	}

	// An ioctl needs the raw file descriptor. SyscallConn().Control lends it
	// to us safely: the file cannot be closed while the function runs.
	raw, err := f.SyscallConn()
	if err != nil {
		return fmt.Errorf("/dev/urandom: %w", err)
	}
	var ioctlErr error
	if err := raw.Control(func(fd uintptr) {
		ioctlErr = unix.IoctlSetInt(int(fd), unix.RNDRESEEDCRNG, 0)
	}); err != nil {
		return fmt.Errorf("/dev/urandom: %w", err)
	}
	if ioctlErr != nil {
		return fmt.Errorf("ioctl RNDRESEEDCRNG: %w", ioctlErr)
	}
	return nil
}
