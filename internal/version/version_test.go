package version

import (
	"runtime"
	"strings"
	"testing"
)

// TestString checks that the build description names the program, the
// version, the commit and the Go version. `make test` runs every function
// whose name starts with "Test".
func TestString(t *testing.T) {
	got := String("sandboxctl")

	for _, want := range []string{"sandboxctl", Version, Commit, runtime.Version()} {
		if !strings.Contains(got, want) {
			t.Errorf("String() = %q, want it to contain %q", got, want)
		}
	}
}
