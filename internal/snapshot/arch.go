package snapshot

import (
	"fmt"
	"runtime"
)

// The CPU architectures MicroVM Sandbox knows, named as Firecracker and the
// Linux kernel name them (Go calls them amd64 and arm64).
const (
	// ArchX86_64 is 64-bit Intel and AMD.
	ArchX86_64 = "x86_64"
	// ArchAarch64 is 64-bit ARM, for example Apple Silicon.
	ArchAarch64 = "aarch64"
)

// HostArch returns the architecture this program runs on, "x86_64" or
// "aarch64". A snapshot holds the vCPU registers of the machine it was
// made on, so it only restores on the same architecture: the manifest
// records it (Manifest.Arch), and Open refuses a store made on another
// one. Any other architecture is an error, never a guess.
func HostArch() (string, error) { return archOf(runtime.GOARCH) }

// archOf maps a Go architecture name (runtime.GOARCH) to ours.
func archOf(goarch string) (string, error) {
	switch goarch {
	case "amd64":
		return ArchX86_64, nil
	case "arm64":
		return ArchAarch64, nil
	}
	return "", fmt.Errorf("snapshot: unsupported architecture %q: MicroVM Sandbox runs on x86_64 (amd64) and aarch64 (arm64) only", goarch)
}
