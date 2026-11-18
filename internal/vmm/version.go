package vmm

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"time"
)

// versionTimeout bounds `firecracker --version`.
const versionTimeout = 5 * time.Second

// versionLine is the first line of `firecracker --version`, for example
// "Firecracker v1.17.0".
var versionLine = regexp.MustCompile(`^Firecracker (v[0-9]+\.[0-9]+\.[0-9]+\S*)$`)

// BinaryVersion runs `binary --version` and returns Firecracker's version,
// for example "v1.17.0". A snapshot can only be loaded by the Firecracker
// version that made it, so the snapshot store records this and checks it.
func BinaryVersion(ctx context.Context, binary string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, versionTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, binary, "--version").Output() //nolint:gosec // G204: the binary path comes from our own configuration
	if err != nil {
		return "", fmt.Errorf("vmm: %s --version: %w", binary, err)
	}
	return parseVersion(out)
}

// parseVersion reads the version from the output of `firecracker --version`.
func parseVersion(out []byte) (string, error) {
	first, _, _ := bytes.Cut(out, []byte("\n"))
	m := versionLine.FindSubmatch(bytes.TrimSpace(first))
	if m == nil {
		return "", fmt.Errorf("vmm: unexpected --version output %q", first)
	}
	return string(m[1]), nil
}
