package ui

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The page's JavaScript never turns text into markup: guest output is
// untrusted everywhere, and the page shows it with textContent only.
func TestNoForbiddenSinksInThePage(t *testing.T) {
	entries, err := web.ReadDir("web")
	if err != nil {
		t.Fatal(err)
	}
	sinks := []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval(", "Function(", "setAttribute('style'",
		`setAttribute("style"`, ".cssText", "javascript:"}
	seen := 0
	for _, e := range entries {
		name := e.Name()
		data := string(webFile(name))
		if strings.HasSuffix(name, ".js") {
			seen++
			for _, sink := range sinks {
				if strings.Contains(data, sink) {
					t.Errorf("%s uses %s", name, sink)
				}
			}
			if !strings.Contains(data, "'use strict'") {
				t.Errorf("%s is not strict", name)
			}
		}
		if strings.HasSuffix(name, ".html") {
			// No inline script or style, no event handler attributes: the
			// CSP allows none, and the export hashes only its own.
			for _, re := range []*regexp.Regexp{regexp.MustCompile(`<script(\s[^>]*)?>[^<]`), regexp.MustCompile(`(?i)\sstyle=`),
				regexp.MustCompile(`(?i)\son[a-z]+=`), regexp.MustCompile(`(?i)<style`), regexp.MustCompile(`(?i)https?://`)} {
				if re.MatchString(data) {
					t.Errorf("%s matches %v", name, re)
				}
			}
		}
	}
	if seen != 3 {
		t.Errorf("%d scripts checked, want app.js, live.js and player.js", seen)
	}
}

// Only the live page has the LIVE badge; nothing the replay page loads
// can make one.
func TestOnlyTheLivePageSaysLive(t *testing.T) {
	if !bytes.Contains(webFile("live.html"), []byte(`id="badge-live"`)) {
		t.Error("live.html has no LIVE badge")
	}
	for _, name := range []string{"replay.html", "app.js", "player.js"} {
		data := string(webFile(name))
		if strings.Contains(data, "badge-live") || strings.Contains(data, "LIVE") {
			t.Errorf("%s can show a LIVE badge", name)
		}
	}
	if !bytes.Contains(webFile("replay.html"), []byte("RECORDING, not live.")) {
		t.Error("replay.html has no recording banner")
	}
}

// inline returns the bodies of the page's inline <tag> elements (not the
// JSON data block).
func inline(page, tag string) []string {
	re := regexp.MustCompile(`(?s)<` + tag + `>(.*?)</` + tag + `>`)
	var out []string
	for _, m := range re.FindAllStringSubmatch(page, -1) {
		out = append(out, m[1])
	}
	return out
}

// sessionBlock returns the JSON data block of a replay page.
func sessionBlock(t *testing.T, page string) string {
	t.Helper()
	const open = `<script type="application/json" id="session">`
	i := strings.Index(page, open)
	if i < 0 {
		t.Fatal("no session block")
	}
	rest := page[i+len(open):]
	j := strings.Index(rest, "</script>")
	if j < 0 {
		t.Fatal("the session block does not end")
	}
	return rest[:j]
}

func TestExportIsSelfContainedAndEscaped(t *testing.T) {
	s := fixtureSession(t)
	dir := t.TempDir()
	path := filepath.Join(dir, DefaultExportName(s))
	if !strings.Contains(path, "microvm-recording-2026-11-25-0930.html") {
		t.Errorf("the default name %s does not say recording and when", path)
	}
	if err := Export(s, path); err != nil {
		t.Fatalf("Export: %v", err)
	}
	data, err := os.ReadFile(path) //nolint:gosec // G304: the test's own file
	if err != nil {
		t.Fatal(err)
	}
	page := string(data)

	// Nothing is fetched from anywhere: no URL (this fixture has none in
	// its output), no external script, stylesheet or image. The only
	// <link> is the empty data: icon, which keeps browsers from asking
	// for /favicon.ico.
	if strings.Contains(page, "http://") || strings.Contains(page, "https://") {
		t.Error("the export holds a URL")
	}
	markup := strings.Replace(page, sessionBlock(t, page), "", 1)
	markup = strings.Replace(markup, `<link rel="icon" href="data:,">`, "", 1)
	for _, bad := range []string{"<link", " src=", "@import", "url(", " href="} {
		if strings.Contains(markup, bad) {
			t.Errorf("the export's markup holds %q", bad)
		}
	}
	// The CSP names exactly the hashes of the inline elements.
	m := regexp.MustCompile(`<meta http-equiv="Content-Security-Policy" content="([^"]*)">`).FindStringSubmatch(page)
	if m == nil {
		t.Fatal("no CSP meta tag")
	}
	csp := m[1]
	for _, part := range []string{"default-src 'none'", "connect-src 'none'", "base-uri 'none'", "form-action 'none'", "img-src data:"} {
		if !strings.Contains(csp, part) {
			t.Errorf("CSP %q lacks %q", csp, part)
		}
	}
	scripts, styles := inline(page, "script"), inline(page, "style")
	if len(scripts) != 2 || len(styles) != 1 {
		t.Fatalf("%d inline scripts and %d styles, want 2 and 1", len(scripts), len(styles))
	}
	for _, body := range scripts {
		if !strings.Contains(csp, "script-src "+cspHash(scripts[0])+" "+cspHash(scripts[1])+";") || !strings.Contains(csp, cspHash(body)) {
			t.Errorf("the CSP does not name the script's hash %s", cspHash(body))
		}
	}
	if !strings.Contains(csp, "style-src "+cspHash(styles[0])+";") || strings.Contains(csp, "unsafe") {
		t.Errorf("CSP %q", csp)
	}

	// The data block: no literal </script or <!-- inside, and the guest's
	// output comes back exactly as captured.
	block := sessionBlock(t, page)
	if strings.Contains(strings.ToLower(block), "</script") || strings.Contains(block, "<!--") || strings.Contains(block, "<") {
		t.Error("the session block holds markup")
	}
	var back Session
	if err := json.Unmarshal([]byte(block), &back); err != nil {
		t.Fatal(err)
	}
	if got := back.Events[3].Event.Output.Data; got != hostileText {
		t.Errorf("the output came back as %q, want %q", got, hostileText)
	}
	// Always a recording: the banner and the title.
	if !strings.Contains(page, "RECORDING, not live.") || !strings.Contains(page, "<title>MicroVM Sandbox · recording from 2026-11-25 09:30 UTC</title>") ||
		strings.Contains(page, `id="badge-live"`) {
		t.Error("the export does not say it is a recording")
	}

	// Never over an existing file; only .html.
	if err := Export(s, path); err == nil || !strings.Contains(err.Error(), "exists already") {
		t.Errorf("a second export to the same file: %v", err)
	}
	if err := Export(s, filepath.Join(dir, "x.js")); err == nil {
		t.Error("an export named .js")
	}
}

// A session whose texts claim to be live still plays as a recording: the
// banner is the template's, never the session's.
func TestReplayOfASessionThatClaimsToBeLive(t *testing.T) {
	s := fixtureSession(t)
	s.Meta.UIVersion = `LIVE <span id="badge-live">LIVE</span>`
	s.Meta.Info.Mode = "LIVE"
	s.Events[1].Event.Scenario.Title = "LIVE"
	page, err := replayPage(s, false)
	if err != nil {
		t.Fatal(err)
	}
	p := string(page)
	block := sessionBlock(t, p)
	outside := strings.Replace(p, block, "", 1)
	if strings.Contains(outside, "LIVE") || strings.Contains(outside, "badge-live") || !strings.Contains(outside, "RECORDING, not live.") ||
		!strings.Contains(outside, "recording") {
		t.Error("the replay page says LIVE, or lacks the recording banner")
	}
}

func TestReplayServer(t *testing.T) {
	s := fixtureSession(t)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(t.Context(), Options{Listener: lis, Replay: s, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	host := srv.hostPort
	// The same rules as live: no cookie, nothing.
	if r := do(t, srv, http.MethodGet, "/", host, false, nil, ""); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("replay page without the cookie: %d", r.StatusCode)
	}
	r := do(t, srv, http.MethodGet, "/", host, true, nil, "")
	page := string(r.Body)
	if r.StatusCode != http.StatusOK || !strings.Contains(page, "RECORDING, not live.") || strings.Contains(page, "badge-live") ||
		!strings.Contains(page, `<script src="player.js"></script>`) || strings.Contains(page, "Content-Security-Policy") {
		t.Errorf("the replay page: %d", r.StatusCode)
	}
	checkHeaders(t, "the replay page", r)
	escaped, _ := json.Marshal("</script>") // what encoding/json makes of it: the < and > as escapes
	if got := sessionBlock(t, page); !strings.Contains(got, strings.Trim(string(escaped), `"`)) || strings.Contains(got, "<") {
		t.Errorf("the served session block is not escaped: %.200s", got)
	}
	for path, want := range map[string]int{"/player.js": 200, "/app.js": 200, "/app.css": 200, "/live.js": 404, "/api/events": 404} {
		if r := do(t, srv, http.MethodGet, path, host, true, nil, ""); r.StatusCode != want {
			t.Errorf("replay GET %s: %d, want %d", path, r.StatusCode, want)
		}
	}
	if r := do(t, srv, http.MethodPost, "/api/run", host, true, map[string]string{"Origin": "http://" + host, "Content-Type": "application/json"},
		`{"scenario":"demo1"}`); r.StatusCode != http.StatusNotFound && r.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("replay POST /api/run: %d, want nothing there", r.StatusCode)
	}
}
