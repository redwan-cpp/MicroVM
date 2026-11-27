//go:build e2e

package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/redwan-cpp/MicroVM/internal/audit"
	"github.com/redwan-cpp/MicroVM/internal/ui"
)

// The demo UI, end to end: bin/sandboxd serve and bin/sandboxctl ui as
// processes, the Demo 1 and hostile-code scenarios started over HTTP as
// the page starts them, their events read from the page's event stream
// (SSE), the host panel down to zero, the audit chain verified; then the
// recording played back without a daemon, and exported.

// proc is a background program and everything it printed.
type proc struct {
	cmd *exec.Cmd
	mu  sync.Mutex
	out strings.Builder
	url chan string // the first http:// address it prints
}

// startProc starts bin with args. Cleanup stops it if the test has not.
func startProc(t *testing.T, bin string, env []string, args ...string) *proc {
	t.Helper()
	p := &proc{cmd: exec.Command(bin, args...), url: make(chan string, 1)} //nolint:gosec // G204: our own binaries
	p.cmd.Env = append(os.Environ(), env...)
	r, w := io.Pipe()
	p.cmd.Stdout, p.cmd.Stderr = w, w
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	urlRE := regexp.MustCompile(`http://127\.0\.0\.1:[0-9]+/\?token=[0-9a-f]{32}`)
	go func() {
		sc := bufio.NewScanner(r)
		sent := false
		for sc.Scan() {
			p.mu.Lock()
			p.out.WriteString(sc.Text() + "\n")
			p.mu.Unlock()
			if u := urlRE.FindString(sc.Text()); u != "" && !sent {
				p.url <- u
				sent = true
			}
		}
	}()
	t.Cleanup(func() {
		p.stop(t, syscall.SIGTERM)
		_ = w.Close()
		t.Logf("%s printed:\n%s", filepath.Base(bin), p.output())
	})
	return p
}

func (p *proc) output() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.out.String()
}

// stop sends sig and waits up to 20 s; it returns the exit code.
func (p *proc) stop(t *testing.T, sig syscall.Signal) int {
	t.Helper()
	if p.cmd.ProcessState != nil {
		return p.cmd.ProcessState.ExitCode()
	}
	_ = p.cmd.Process.Signal(sig)
	done := make(chan struct{})
	go func() {
		_ = p.cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		_ = p.cmd.Process.Kill()
		<-done
		t.Errorf("%s did not stop within 20 s", p.cmd.Path)
	}
	return p.cmd.ProcessState.ExitCode()
}

// waitURL returns the address the UI printed.
func (p *proc) waitURL(t *testing.T) string {
	t.Helper()
	select {
	case u := <-p.url:
		return u
	case <-time.After(15 * time.Second):
		t.Fatalf("sandboxctl ui printed no address:\n%s", p.output())
	}
	return ""
}

// page is a browser's view of the UI: a cookie jar that opened the start
// URL.
type page struct {
	t      *testing.T
	base   string
	client *http.Client
}

func openPage(t *testing.T, startURL string) *page {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	pg := &page{t: t, base: strings.SplitN(startURL, "/?", 2)[0], client: &http.Client{Jar: jar, Timeout: 30 * time.Second}}
	resp, err := pg.client.Get(startURL)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Request.URL.RawQuery != "" {
		t.Fatalf("the start URL ended at %v with %d", resp.Request.URL, resp.StatusCode)
	}
	return pg
}

// get fetches path; it returns the status and the body.
func (pg *page) get(path string) (int, string) {
	pg.t.Helper()
	resp, err := pg.client.Get(pg.base + path)
	if err != nil {
		pg.t.Fatal(err)
	}
	defer resp.Body.Close() //nolint:errcheck // test
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// post sends JSON as the page does (same Origin).
func (pg *page) post(path, body string) (int, string) {
	pg.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, pg.base+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", pg.base)
	resp, err := pg.client.Do(req)
	if err != nil {
		pg.t.Fatal(err)
	}
	defer resp.Body.Close() //nolint:errcheck // test
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(data)
}

// sse is the page's open event stream.
type sse struct {
	events chan *ui.Event
}

func (pg *page) stream(ctx context.Context) *sse {
	pg.t.Helper()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, pg.base+"/api/events", nil)
	resp, err := (&http.Client{Jar: pg.client.Jar}).Do(req) //nolint:bodyclose // closed when ctx ends, below
	if err != nil {
		pg.t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
		pg.t.Fatalf("event stream: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	s := &sse{events: make(chan *ui.Event, 4096)}
	go func() {
		defer resp.Body.Close() //nolint:errcheck // test
		defer close(s.events)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
		for sc.Scan() {
			line := sc.Text()
			data, ok := strings.CutPrefix(line, "data: ")
			if !ok {
				continue // blank lines and ": ping"
			}
			var ev ui.Event
			if json.Unmarshal([]byte(data), &ev) == nil && ev.Type != "" {
				s.events <- &ev
			}
		}
	}()
	return s
}

// until collects events until done says stop.
func (s *sse) until(t *testing.T, what string, timeout time.Duration, done func(*ui.Event) bool) []*ui.Event {
	t.Helper()
	var got []*ui.Event
	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-s.events:
			if !ok {
				t.Fatalf("the event stream ended while waiting for %s", what)
			}
			got = append(got, ev)
			if done(ev) {
				return got
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s (%d events)", what, len(got))
		}
	}
}

func endOfSeg(seg int) func(*ui.Event) bool {
	return func(ev *ui.Event) bool { return ev.Type == ui.TypeEnd && ev.End.Seg == seg }
}

// laneView is what one lane of a scenario showed.
type laneView struct {
	out    string
	result *ui.ResultEvent
	states []string
	hash   string
}

func lanes(evs []*ui.Event) map[int]*laneView {
	m := map[int]*laneView{}
	get := func(i int) *laneView {
		if m[i] == nil {
			m[i] = &laneView{}
		}
		return m[i]
	}
	for _, ev := range evs {
		switch ev.Type {
		case ui.TypeOutput:
			get(ev.Output.Lane).out += ev.Output.Data
		case ui.TypeState:
			l := get(ev.State.Lane)
			l.states = append(l.states, ev.State.State)
		case ui.TypeResult:
			get(ev.Result.Lane).result = ev.Result
		case ui.TypeEnd:
			for _, a := range ev.End.Audit {
				get(a.Lane).hash = a.Hash
			}
		}
	}
	return m
}

func TestDemoUI(t *testing.T) {
	a := findArtifacts(t)
	sandboxd, sandboxctl := filepath.Join(a.repo, "bin", "sandboxd"), filepath.Join(a.repo, "bin", "sandboxctl")
	for _, bin := range []string{sandboxd, sandboxctl} {
		if _, err := os.Stat(bin); err != nil {
			t.Skipf("no %s (%v): run make build", bin, err)
		}
	}
	root := newStateRoot(t, a)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	version := buildSmall(ctx, t, a, root)

	socket := filepath.Join(root.Dir(), "d.sock")
	daemon := startProc(t, sandboxd, nil, "serve", "--state-dir", root.Dir(), "--socket", socket, "--firecracker", a.firecracker,
		"--max-concurrent", "4")
	waitForSocket(t, socket)

	// sandboxctl ui, recording, on a free loopback port.
	work := t.TempDir()
	record := filepath.Join(work, "session.jsonl")
	uiProc := startProc(t, sandboxctl, []string{"SANDBOXD_SOCKET=" + socket}, "ui", "--listen", "127.0.0.1:0",
		"--state-dir", root.Dir(), "--record", record)
	startURL := uiProc.waitURL(t)
	base := strings.SplitN(startURL, "/?", 2)[0]

	// The guards, on the real server: no cookie, a foreign Host, a POST
	// from another origin.
	if resp, err := http.Get(base + "/"); err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("the page without the token: %v, %v", resp, err)
	} else {
		_ = resp.Body.Close()
	}
	pg := openPage(t, startURL)
	req, _ := http.NewRequest(http.MethodGet, base+"/", nil)
	req.Host = "attacker.example:80"
	if resp, err := pg.client.Do(req); err != nil || resp.StatusCode != http.StatusForbidden {
		t.Errorf("a foreign Host header: %v, %v", resp, err)
	} else {
		_ = resp.Body.Close()
	}
	req, _ = http.NewRequest(http.MethodPost, base+"/api/run", strings.NewReader(`{"scenario":"demo1"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://attacker.example")
	if resp, err := pg.client.Do(req); err != nil || resp.StatusCode != http.StatusForbidden {
		t.Errorf("a cross-origin POST: %v, %v", resp, err)
	} else {
		_ = resp.Body.Close()
	}
	if code, body := pg.get("/"); code != http.StatusOK || !strings.Contains(body, `id="badge-live"`) {
		t.Errorf("the live page: %d", code)
	}

	sctx, stopStream := context.WithCancel(ctx)
	defer stopStream()
	st := pg.stream(sctx)
	first := st.until(t, "the daemon's Info", 10*time.Second, func(ev *ui.Event) bool { return ev.Type == ui.TypeInfo })
	info := first[len(first)-1].Info
	if info.Mode != "rootless" || len(info.Classes) == 0 || info.Classes[0].ColdBootUS == 0 || info.Arch == "" || info.HostKind == "" {
		t.Errorf("info %+v", info)
	}

	// Demo 1, as the page's button starts it.
	if code, body := pg.post("/api/run", `{"scenario":"demo1"}`); code != http.StatusAccepted {
		t.Fatalf("start Demo 1: %d %s", code, body)
	}
	evs := st.until(t, "Demo 1's end", time.Minute, endOfSeg(1))
	ls := lanes(evs)
	ids := map[string]bool{}
	for i := range 3 {
		l := ls[i]
		want := fmt.Sprintf("lost+found\nsecret-%d\nsecret of clone %d\n", i+1, i+1)
		if l == nil || l.result == nil || l.result.Outcome != "EXITED" || l.result.ExitCode != 0 || l.out != want ||
			l.hash == "" || l.result.TimingsUS.Restore == 0 {
			t.Fatalf("clone %d: %+v (output %q), want EXITED 0 with only its own secret, %q", i+1, l, l.out, want)
		}
		if strings.Join(l.states, ",") != "PENDING,RESTORING,MOUNTING,RUNNING,STOPPING,DONE" {
			t.Errorf("clone %d states %v", i+1, l.states)
		}
		ids[l.result.RunID] = true
		t.Logf("Demo 1 via the UI, clone %d (%s): restore %.1f ms, audit record %.12s", i+1, l.result.RunID,
			float64(l.result.TimingsUS.Restore)/1000, l.hash)
	}

	// The host panel comes back to zero, as sandboxd reports it.
	st.until(t, "the host panel at zero", 15*time.Second, func(ev *ui.Event) bool {
		h := ev.Host
		return ev.Type == ui.TypeHost && h.Error == "" && len(h.Clones) == 0 && h.Processes.N == 0 && h.Processes.Error == "" &&
			h.RunDirs.N == 0 && h.RunDirs.Error == "" && h.JailDirs.N == 0
	})

	// Hostile code: no network, a read-only root disk.
	if code, body := pg.post("/api/run", `{"scenario":"hostile"}`); code != http.StatusAccepted {
		t.Fatalf("start the hostile scenario: %d %s", code, body)
	}
	h := lanes(st.until(t, "the hostile scenario's end", time.Minute, endOfSeg(2)))[0]
	if h == nil || h.result == nil || h.result.Outcome != "EXITED" || h.result.ExitCode == 0 ||
		!strings.Contains(h.out, "Read-only file system") || !strings.Contains(h.out, "unreachable") {
		t.Errorf("hostile: %+v, output %q", h, h.out)
	}

	// Verify the chain: four runs, four records.
	code, body := pg.post("/api/verify", `{}`)
	var v ui.VerifyEvent
	if err := json.Unmarshal([]byte(body), &v); err != nil || code != http.StatusOK || !v.OK || v.Records != 4 {
		t.Errorf("verify: %d %s", code, body)
	}

	// Stop the UI: the recording gets its end line.
	stopStream()
	if code := uiProc.stop(t, syscall.SIGINT); code != 0 {
		t.Errorf("sandboxctl ui exited %d", code)
	}
	if info, err := os.Lstat(record); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("the recording: %v, %v; want mode 0600", info, err)
	}
	session, err := ui.LoadSession(record)
	if err != nil {
		t.Fatalf("load the recording: %v", err)
	}
	if session.End == nil || !session.End.Verify.OK || session.End.Verify.Records != 4 {
		t.Errorf("the recording's end %+v", session.End)
	}
	raw, _ := os.ReadFile(record) //nolint:gosec // G304: the test's own file
	token := regexp.MustCompile(`token=([0-9a-f]{32})`).FindStringSubmatch(startURL)[1]
	if strings.Contains(string(raw), token) || strings.Contains(string(raw), root.Dir()) {
		t.Error("the recording holds the token or the state folder's path")
	}
	// Its audit hashes are the log's.
	entries, err := audit.Tail(audit.Path(root.AuditDir()), 10)
	if err != nil {
		t.Fatal(err)
	}
	inLog := map[string]string{}
	for _, e := range entries {
		inLog[e.RunID] = e.Hash
	}
	for _, te := range session.Events {
		if te.Event.Type == ui.TypeEnd {
			for _, ref := range te.Event.End.Audit {
				if ref.Hash == "" || inLog[ref.RunID] != ref.Hash {
					t.Errorf("recorded audit ref %+v, the log says %q", ref, inLog[ref.RunID])
				}
			}
		}
	}

	// No daemon from here on: the recording plays without one.
	if code := daemon.stop(t, syscall.SIGTERM); code != 0 {
		t.Errorf("sandboxd exited %d", code)
	}
	checkNothingLeft(t, root, version)

	replay := startProc(t, sandboxctl, nil, "ui", "--replay", record, "--listen", "127.0.0.1:0")
	rp := openPage(t, replay.waitURL(t))
	code, body = rp.get("/")
	if code != http.StatusOK || !strings.Contains(body, "RECORDING, not live.") || strings.Contains(body, `id="badge-live"`) {
		t.Errorf("the replay page: %d", code)
	}
	for id := range ids {
		if !strings.Contains(body, id) {
			t.Errorf("the replay page lacks run %s", id)
		}
	}
	if code, _ := rp.get("/player.js"); code != http.StatusOK {
		t.Errorf("the player: %d", code)
	}
	if code := replay.stop(t, syscall.SIGINT); code != 0 {
		t.Errorf("sandboxctl ui --replay exited %d", code)
	}

	// Export: one file.
	out := filepath.Join(work, "demo.html")
	c := exec.CommandContext(ctx, sandboxctl, "ui", "export", "--from", record, "--out", out) //nolint:gosec // G204: our own binary
	if msg, err := c.CombinedOutput(); err != nil {
		t.Fatalf("export: %v\n%s", err, msg)
	}
	html, err := os.ReadFile(out) //nolint:gosec // G304: the test's own file
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(html), `<meta http-equiv="Content-Security-Policy" content="default-src 'none'; script-src 'sha256-`) ||
		!strings.Contains(string(html), "RECORDING, not live.") || strings.Contains(string(html), `<script src=`) {
		t.Error("the export is not one self-contained recording")
	}
	t.Logf("recorded %d events (%d bytes), exported %d bytes", len(session.Events), len(raw), len(html))
}

// waitForSocket waits until a daemon listens on path.
func waitForSocket(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("sandboxd did not open its socket")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
