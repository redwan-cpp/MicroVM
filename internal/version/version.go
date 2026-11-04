// Package version reports which build of MicroVM Sandbox is running.
//
// When you build with `make build`, the Makefile fills in Version and Commit
// using the linker flag `-X` (see LDFLAGS in the Makefile). A plain
// `go build` skips that step, so you get the defaults "dev" and "none".
// Knowing exactly which build is running matters later: every audit record
// will include it.
package version

import (
	"fmt"
	"runtime"
)

// Version and Commit are variables, not constants, because the linker can
// only overwrite variables at build time.
var (
	// Version is the release tag, for example "v0.1.0", or "dev".
	Version = "dev"
	// Commit is the short git commit hash the binary was built from.
	Commit = "none"
)

// String returns a one-line description of the build, for example:
//
//	sandboxctl v0.1.0 (commit 1a2b3c4, go1.27.3 linux/amd64)
func String(program string) string {
	return fmt.Sprintf("%s %s (commit %s, %s %s/%s)",
		program, Version, Commit, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}
