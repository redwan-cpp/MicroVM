package vmm

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The Process tests need a "firecracker" to start. Instead of the real one
// (which needs KVM), they start this test program itself with an
// environment variable that makes TestMain act as a small fake: it opens
// the API socket like Firecracker and answers every request with 204.
const fakeEnv = "VMM_TEST_FAKE_FIRECRACKER"

func TestMain(m *testing.M) {
	switch mode := os.Getenv(fakeEnv); mode {
	case "":
		os.Exit(m.Run()) // the normal test run
	case "exit-at-once":
		fmt.Println("fake firecracker: giving up")
		os.Exit(3)
	case "version":
		fmt.Print("Firecracker v1.17.0\n\n2026-11-18T00:00:00 [anonymous-instance:main] Firecracker exiting successfully. exit_code=0\n")
	default:
		fakeFirecracker(mode == "ignore-sigterm")
	}
}

// fakeFirecracker parses the same flags as Firecracker, prints a line on
// stdout and stderr (the "console"), and serves the API socket until it is
// killed.
func fakeFirecracker(ignoreSIGTERM bool) {
	var apiSock, logPath string
	for i := 1; i+1 < len(os.Args); i++ {
		switch os.Args[i] {
		case "--api-sock":
			apiSock = os.Args[i+1]
		case "--log-path":
			logPath = os.Args[i+1]
		}
	}
	if _, err := os.Stat(logPath); err != nil { //nolint:gosec // G703: the path comes from the test itself; Firecracker needs the log file to exist
		fmt.Fprintln(os.Stderr, "fake firecracker: no log file:", err)
		os.Exit(1)
	}
	if ignoreSIGTERM {
		signal.Ignore(syscall.SIGTERM)
	}
	lis, err := net.Listen("unix", apiSock)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake firecracker:", err)
		os.Exit(1)
	}
	cwd, _ := os.Getwd()
	fmt.Println("fake console: hello on stdout, cwd=" + cwd)
	fmt.Fprintln(os.Stderr, "fake console: hello on stderr")
	srv := &http.Server{
		Handler:           http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }),
		ReadHeaderTimeout: time.Second,
	}
	_ = srv.Serve(lis)
}

// startFake starts the fake in the given mode, with a fresh run folder in
// a test folder, so what is left can be checked afterwards.
func startFake(t *testing.T, mode string) (*Process, string, error) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv(fakeEnv, mode)
	p, err := Start(t.Context(), Config{Binary: os.Args[0], RunDir: newRunDir(t, tmp)})
	if p != nil {
		t.Cleanup(func() { _ = p.Stop() })
	}
	return p, tmp, err
}

// newRunDir creates a private run folder in parent, like
// state.Root.NewRunDir.
func newRunDir(t *testing.T, parent string) string {
	t.Helper()
	dir := filepath.Join(parent, "r-test")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestProcessStartAndStop(t *testing.T) {
	p, tmp, err := startFake(t, "serve")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	if p.Dir() != filepath.Join(tmp, "r-test") {
		t.Errorf("run folder %s, want the one from the Config, %s", p.Dir(), filepath.Join(tmp, "r-test"))
	}
	if info, err := os.Stat(p.Dir()); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("run folder: %v, %v; want mode 0700", info, err)
	}
	if info, err := os.Stat(p.APISocketPath()); err != nil || info.Mode().Type() != os.ModeSocket {
		t.Errorf("API socket: %v, %v; want a socket", info, err)
	}
	if filepath.Dir(p.VsockPath()) != p.Dir() {
		t.Errorf("VsockPath %s is not in the run folder", p.VsockPath())
	}
	if info, err := os.Stat(p.LogPath()); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("log file: %v, %v; want mode 0600", info, err)
	}
	if err := p.Client().Start(t.Context()); err != nil {
		t.Errorf("API call to the fake: %v", err)
	}
	waitFor(t, "both console lines", func() bool {
		c := p.Console()
		return bytes.Contains(c, []byte("hello on stdout")) && bytes.Contains(c, []byte("hello on stderr"))
	})
	// Firecracker runs in its run folder, so the relative names it got
	// (--api-sock api.sock) land there.
	if !bytes.Contains(p.Console(), []byte("cwd="+p.Dir()+"\n")) {
		t.Errorf("the fake did not run in the run folder %s; console:\n%s", p.Dir(), p.Console())
	}

	pid := p.Pid()
	if err := p.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := p.Stop(); err != nil {
		t.Errorf("second Stop: %v, want nil", err)
	}
	assertGone(t, p, pid)
}

func TestProcessStopEscalatesToSIGKILL(t *testing.T) {
	p, _, err := startFake(t, "ignore-sigterm")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	p.stopGrace = 200 * time.Millisecond

	pid := p.Pid()
	start := time.Now()
	if err := p.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if took := time.Since(start); took < p.stopGrace || took > 3*time.Second {
		t.Errorf("Stop took %v, want a little over the %v grace", took, p.stopGrace)
	}
	assertGone(t, p, pid)
}

func TestProcessStartFailsWhenFirecrackerExits(t *testing.T) {
	_, tmp, err := startFake(t, "exit-at-once")
	if err == nil || !strings.Contains(err.Error(), "giving up") {
		t.Errorf("err = %v, want it to mention the console output", err)
	}
	assertEmpty(t, tmp)
}

func TestProcessStartFailsWithoutBinary(t *testing.T) {
	tmp := t.TempDir()
	_, err := Start(t.Context(), Config{Binary: filepath.Join(tmp, "no-firecracker-here"), RunDir: newRunDir(t, tmp)})
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("err = %v, want a not-found error", err)
	}
	assertEmpty(t, tmp)
}

func TestBinaryVersion(t *testing.T) {
	t.Setenv(fakeEnv, "version")
	v, err := BinaryVersion(t.Context(), os.Args[0])
	if err != nil || v != "v1.17.0" {
		t.Errorf("BinaryVersion = %q, %v; want v1.17.0", v, err)
	}
}

func TestParseVersion(t *testing.T) {
	for out, want := range map[string]string{
		"Firecracker v1.17.0\n\nexiting":   "v1.17.0",
		"Firecracker v1.18.0-dev\n":        "v1.18.0-dev",
		"Firecracker v1.17.0":              "v1.17.0",
		"firecracker v1.17.0\n":            "",
		"Jailer v1.17.0\n":                 "",
		"Firecracker 1.17.0\n":             "",
		"":                                 "",
		"Firecracker v1.17.0 extra junk\n": "",
	} {
		got, err := parseVersion([]byte(out))
		if got != want || (want == "") != (err != nil) {
			t.Errorf("parseVersion(%q) = %q, %v; want %q", out, got, err, want)
		}
	}
}

func TestProcessStartNeedsARunDir(t *testing.T) {
	if _, err := Start(t.Context(), Config{Binary: os.Args[0]}); err == nil {
		t.Error("Start without a RunDir succeeded, want an error")
	}
}

// assertGone checks that the process has exited and its folder is deleted.
func assertGone(t *testing.T, p *Process, pid int) {
	t.Helper()
	select {
	case <-p.Done():
	default:
		t.Error("Done() is not closed after Stop")
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Errorf("process %d still exists after Stop (kill -0: %v)", pid, err)
	}
	if _, err := os.Stat(p.Dir()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("run folder %s still exists after Stop (%v)", p.Dir(), err)
	}
}

func assertEmpty(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Errorf("%s holds %v (%v); want no run folder left behind", dir, entries, err)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
