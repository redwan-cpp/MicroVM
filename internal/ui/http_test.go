package ui

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// reply is one answer of the handler.
type reply struct {
	StatusCode int
	Header     http.Header
	Body       []byte
	cookies    []*http.Cookie
}

// Cookies are the cookies the answer sets.
func (r *reply) Cookies() []*http.Cookie { return r.cookies }

// do sends one request straight to the handler, with the Host and the
// headers given, and the right cookie if cookie is true.
func do(t *testing.T, s *Server, method, target, host string, cookie bool, hdr map[string]string, body string) *reply {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Host = host
	if cookie {
		req.AddCookie(&http.Cookie{Name: s.cookieName, Value: s.token}) //nolint:gosec // G124: a cookie the test sends, not one it sets
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	resp := rec.Result()
	defer resp.Body.Close() //nolint:errcheck // in memory
	data, _ := io.ReadAll(resp.Body)
	return &reply{StatusCode: resp.StatusCode, Header: resp.Header, Body: data, cookies: resp.Cookies()}
}

// checkHeaders checks the security headers every response carries.
func checkHeaders(t *testing.T, what string, resp *reply) {
	t.Helper()
	h := resp.Header
	if h.Get("Content-Security-Policy") != liveCSP || h.Get("Referrer-Policy") != "no-referrer" ||
		h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Cache-Control") != "no-store" {
		t.Errorf("%s: security headers %v", what, h)
	}
	for k := range h {
		if strings.HasPrefix(strings.ToLower(k), "access-control-") {
			t.Errorf("%s: a CORS header %s", what, k)
		}
	}
}

func TestTokenCookieAndRedirect(t *testing.T) {
	e := newEnv(t, "")
	s := e.srv
	host := s.hostPort
	if !strings.HasPrefix(s.cookieName, "microvm_ui_") || !strings.HasSuffix(s.cookieName, strconv.Itoa(s.port)) || len(s.token) != 32 {
		t.Errorf("cookie %q, token %q: want the port in the name and 128 bits of token", s.cookieName, s.token)
	}

	// No cookie: nothing at all, not even the page or its scripts.
	for _, path := range []string{"/", "/app.js", "/live.js", "/app.css", "/api/events"} {
		resp := do(t, s, http.MethodGet, path, host, false, nil, "")
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("GET %s without the cookie: %d, want 401", path, resp.StatusCode)
		}
		checkHeaders(t, "GET "+path+" without the cookie", resp)
	}
	if resp := do(t, s, http.MethodPost, "/api/run", host, false, map[string]string{"Origin": "http://" + host,
		"Content-Type": "application/json"}, `{"scenario":"demo1"}`); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("POST /api/run without the cookie: %d", resp.StatusCode)
	}
	if len(e.fake.requests()) != 0 {
		t.Fatal("a run started without the cookie")
	}

	// The wrong token, in the URL or in the cookie.
	if resp := do(t, s, http.MethodGet, "/?token=00000000000000000000000000000000", host, false, nil, ""); resp.StatusCode != http.StatusUnauthorized ||
		resp.Header.Get("Set-Cookie") != "" {
		t.Errorf("a wrong token: %d, cookie %q", resp.StatusCode, resp.Header.Get("Set-Cookie"))
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = host
	req.AddCookie(&http.Cookie{Name: s.cookieName, Value: strings.Repeat("a", 32)}) //nolint:gosec // G124: a cookie the test sends
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("a wrong cookie: %d", rec.Code)
	}

	// The start URL: a strict, HTTP-only cookie, then away from the URL.
	resp := do(t, s, http.MethodGet, "/?token="+s.token, host, false, nil, "")
	c := resp.Cookies()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" || len(c) != 1 || c[0].Name != s.cookieName ||
		c[0].Value != s.token || !c[0].HttpOnly || c[0].SameSite != http.SameSiteStrictMode || c[0].Path != "/" {
		t.Errorf("start URL: %d to %q, cookies %+v", resp.StatusCode, resp.Header.Get("Location"), c)
	}
	checkHeaders(t, "the start URL", resp)
	page := do(t, s, http.MethodGet, "/", host, true, nil, "")
	body := page.Body
	if page.StatusCode != http.StatusOK || !bytes.Contains(body, []byte(`id="badge-live"`)) || page.Header.Get("Content-Type") != "text/html; charset=utf-8" {
		t.Errorf("the page: %d, %q", page.StatusCode, page.Header.Get("Content-Type"))
	}
	checkHeaders(t, "the page", page)
	if r := do(t, s, http.MethodGet, "/nothing", host, true, nil, ""); r.StatusCode != http.StatusNotFound {
		t.Errorf("an unknown path: %d", r.StatusCode)
	} else {
		checkHeaders(t, "a 404", r)
	}
	if r := do(t, s, http.MethodGet, "/player.js", host, true, nil, ""); r.StatusCode != http.StatusNotFound {
		t.Errorf("the live server serves the replay player: %d", r.StatusCode)
	}
}

func TestHostHeader(t *testing.T) {
	e := newEnv(t, "")
	s := e.srv
	port := strconv.Itoa(s.port)
	for _, host := range []string{"evil.example:" + port, "evil.example", "127.0.0.1:1", "127.0.0.1", "", "localhost.:" + port, "[::1]:" + port} {
		resp := do(t, s, http.MethodGet, "/?token="+s.token, host, true, nil, "")
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("Host %q: %d, want 403 (DNS rebinding)", host, resp.StatusCode)
		}
		checkHeaders(t, "Host "+host, resp)
	}
	for _, host := range []string{"127.0.0.1:" + port, "localhost:" + port, "LOCALHOST:" + port} {
		if resp := do(t, s, http.MethodGet, "/", host, true, nil, ""); resp.StatusCode != http.StatusOK {
			t.Errorf("Host %q: %d, want 200", host, resp.StatusCode)
		}
	}
}

func TestOriginContentTypeAndBody(t *testing.T) {
	e := newEnv(t, "")
	s := e.srv
	host := s.hostPort
	jsonCT := "application/json"
	for name, hdr := range map[string]map[string]string{
		"no Origin":          {"Content-Type": jsonCT},
		"another Origin":     {"Content-Type": jsonCT, "Origin": "http://evil.example"},
		"another port":       {"Content-Type": jsonCT, "Origin": "http://127.0.0.1:1"},
		"null Origin":        {"Content-Type": jsonCT, "Origin": "null"},
		"cross-site fetch":   {"Content-Type": jsonCT, "Origin": "http://" + host, "Sec-Fetch-Site": "cross-site"},
		"a form, not JSON":   {"Content-Type": "application/x-www-form-urlencoded", "Origin": "http://" + host},
		"text/plain no-cors": {"Content-Type": "text/plain", "Origin": "http://" + host},
	} {
		resp := do(t, s, http.MethodPost, "/api/run", host, true, hdr, `{"scenario":"demo1"}`)
		if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusUnsupportedMediaType {
			t.Errorf("%s: %d, want refused", name, resp.StatusCode)
		}
	}
	if n := len(e.fake.requests()); n != 0 {
		t.Fatalf("%d runs started by refused requests", n)
	}

	ok := map[string]string{"Content-Type": jsonCT, "Origin": "http://" + host, "Sec-Fetch-Site": "same-origin"}
	for body, want := range map[string]int{
		`{"scenario":"demo1","clones":2}`:                                    http.StatusBadRequest, // a preset takes no clone count
		`{"scenario":"demo1","surprise":1}`:                                  http.StatusBadRequest, // unknown field
		`{"scenario":"demo1"} {"scenario":"demo1"}`:                          http.StatusBadRequest, // two values
		`{"scenario":"custom","line":"x","clones":5}`:                        http.StatusBadRequest, // over max_concurrent (4)
		`{"scenario":"nope"}`:                                                http.StatusBadRequest,
		`{"scenario":"custom","line":"` + strings.Repeat("x", 70<<10) + `"}`: http.StatusRequestEntityTooLarge,
	} {
		resp := do(t, s, http.MethodPost, "/api/run", host, true, ok, body)
		if resp.StatusCode != want {
			t.Errorf("body %.60q: %d, want %d", body, resp.StatusCode, want)
		}
	}
	if n := len(e.fake.requests()); n != 0 {
		t.Fatalf("%d runs started by bad requests", n)
	}
	if resp := do(t, s, http.MethodGet, "/api/run", host, true, nil, ""); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET /api/run: %d", resp.StatusCode)
	}
}

func TestExpand(t *testing.T) {
	info := infoEvent(must(new(fakeDaemon).Info(t.Context(), nil)), "test")
	p, err := expand(runRequest{Scenario: "demo1"}, info)
	if err != nil || len(p.reqs) != 3 || p.event.Clones != 3 || p.event.Class != "small" {
		t.Fatalf("demo1: %+v, %v", p, err)
	}
	for i, r := range p.reqs {
		cmd := strings.Join(r.GetArgv(), " ")
		n := strconv.Itoa(i + 1)
		if r.GetArgv()[0] != "sh" || !strings.Contains(cmd, `"secret of clone `+n+`" > /work/secret-`+n) ||
			len(r.GetEnv()) != 0 || len(r.GetStdin()) != 0 || r.GetSizeClass() != "small" || r.GetMaxOutputBytes() != 1<<20 {
			t.Errorf("demo1 clone %d: %v", i+1, r)
		}
	}
	p, err = expand(runRequest{Scenario: "custom", Line: "uname -a; ls /", Clones: 4, Class: "medium", TimeoutMS: 5000}, info)
	if err != nil || len(p.reqs) != 4 || p.reqs[3].GetArgv()[2] != "uname -a; ls /" || p.reqs[0].GetSizeClass() != "medium" ||
		p.reqs[0].GetTimeoutMs() != 5000 {
		t.Errorf("custom: %+v, %v", p, err)
	}
	for _, id := range []string{"hostile", "forkbomb", "flood"} {
		if p, err := expand(runRequest{Scenario: id}, info); err != nil || len(p.reqs) != 1 || p.event.Caption == "" {
			t.Errorf("%s: %+v, %v", id, p, err)
		}
	}
	for name, req := range map[string]runRequest{
		"empty line":     {Scenario: "custom", Line: "  "},
		"a NUL":          {Scenario: "custom", Line: "a\x00b"},
		"not UTF-8":      {Scenario: "custom", Line: "a\xffb"},
		"too long":       {Scenario: "custom", Line: strings.Repeat("x", maxLine+1)},
		"over 10 min":    {Scenario: "custom", Line: "x", TimeoutMS: 600001},
		"too many":       {Scenario: "custom", Line: "x", Clones: 9},
		"negative":       {Scenario: "custom", Line: "x", Clones: -1},
		"unknown class":  {Scenario: "demo1", Class: "huge"},
		"preset + line":  {Scenario: "hostile", Line: "rm -rf /"},
		"preset + time":  {Scenario: "flood", TimeoutMS: 1},
		"no such button": {Scenario: "rootkit"},
	} {
		if _, err := expand(req, info); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func TestScenarioOverHTTPAndSSE(t *testing.T) {
	e := newEnv(t, "")
	st := e.events()
	// A new page first gets the daemon's Info.
	first := st.until(t, func(ev *Event) bool { return ev.Type == TypeInfo })
	if info := first[len(first)-1].Info; info.Mode != "rootless" || info.MaxClones != 4 || len(info.Classes) != 2 ||
		info.Classes[0].ColdBootUS != 81000 || info.UIVersion != "sandboxctl test" {
		t.Errorf("info %+v", info)
	}

	resp, body := e.post("/api/run", `{"scenario":"demo1"}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST /api/run: %d %s", resp.StatusCode, body)
	}
	evs := st.until(t, endOf(1))
	results, outputs := map[int]*ResultEvent{}, map[int]string{}
	var states []string
	for _, ev := range evs {
		switch ev.Type {
		case TypeResult:
			results[ev.Result.Lane] = ev.Result
		case TypeOutput:
			outputs[ev.Output.Lane] += ev.Output.Data
		case TypeState:
			if ev.State.Lane == 0 {
				states = append(states, ev.State.State)
				if ev.State.RunID == "" || ev.State.MS < 0 {
					t.Errorf("state %+v", ev.State)
				}
			}
		}
	}
	for i := range 3 {
		n := strconv.Itoa(i + 1)
		if r := results[i]; r == nil || r.Outcome != "EXITED" || r.ExitCode != 0 || r.TimingsUS.Restore != 19900 || r.RunID == "" {
			t.Errorf("lane %d: result %+v", i, results[i])
		}
		if outputs[i] != "lost+found\nsecret-"+n+"\nsecret of clone "+n+"\n" {
			t.Errorf("lane %d: output %q", i, outputs[i])
		}
	}
	if strings.Join(states, ",") != "PENDING,RESTORING,MOUNTING,RUNNING,STOPPING,DONE" {
		t.Errorf("lane 0 states %v", states)
	}
	end := evs[len(evs)-1].End
	if end.Error != "" || len(end.Audit) != 3 {
		t.Fatalf("end %+v", end)
	}
	for _, a := range end.Audit {
		if a.Hash == "" || a.Missing != "" || a.RunID != results[a.Lane].RunID {
			t.Errorf("audit ref %+v, want the run's record", a)
		}
	}
	// The UI never sends env or stdin.
	for _, r := range e.fake.requests() {
		if len(r.GetEnv()) != 0 || len(r.GetStdin()) != 0 {
			t.Errorf("a request with env or stdin: %v", r)
		}
	}

	// Verify: the chain holds the three records.
	resp, body = e.post("/api/verify", `{}`)
	var v VerifyEvent
	if err := json.Unmarshal([]byte(body), &v); err != nil || resp.StatusCode != http.StatusOK || !v.OK || v.Records != 3 || len(v.Head) != 64 {
		t.Errorf("verify: %d %s", resp.StatusCode, body)
	}
	st.until(t, func(ev *Event) bool { return ev.Type == TypeVerify && ev.Verify.OK })

	// A page that connects now catches up with everything: the scenario,
	// its lanes, their results and the end.
	late := e.events().until(t, endOf(1))
	var n int
	for _, ev := range late {
		if ev.Type == TypeResult {
			n++
		}
	}
	if n != 3 {
		t.Errorf("a late page got %d results, want 3", n)
	}
}

func TestOneScenarioAtATimeAndStop(t *testing.T) {
	e := newEnv(t, "")
	st := e.events()
	if resp, body := e.post("/api/run", `{"scenario":"custom","line":"block","clones":2}`); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("run: %d %s", resp.StatusCode, body)
	}
	st.until(t, func(ev *Event) bool { return ev.Type == TypeState && ev.State.State == "RUNNING" })
	if resp, body := e.post("/api/run", `{"scenario":"demo1"}`); resp.StatusCode != http.StatusConflict {
		t.Errorf("a second scenario while one runs: %d %s, want 409", resp.StatusCode, body)
	}
	if resp, body := e.post("/api/stop", `{}`); resp.StatusCode != http.StatusOK || !strings.Contains(body, "true") {
		t.Errorf("stop: %d %s", resp.StatusCode, body)
	}
	evs := st.until(t, endOf(1))
	var failed int
	for _, ev := range evs {
		if ev.Type == TypeResult && ev.Result.Outcome == "FAILED" && strings.Contains(ev.Result.Error, "stopped from the page") {
			failed++
		}
	}
	if failed != 2 || !strings.Contains(evs[len(evs)-1].End.Error, "stopped from the page") {
		t.Errorf("%d lanes stopped from the page, end %+v", failed, evs[len(evs)-1].End)
	}
	// Free again.
	if resp, body := e.post("/api/run", `{"scenario":"demo1"}`); resp.StatusCode != http.StatusAccepted {
		t.Errorf("after the stop: %d %s", resp.StatusCode, body)
	}
	st.until(t, endOf(2))
	if resp, body := e.post("/api/stop", `{}`); resp.StatusCode != http.StatusOK || !strings.Contains(body, "false") {
		t.Errorf("stop with nothing running: %d %s", resp.StatusCode, body)
	}
}

func TestHostileOutputIsRelayedSafely(t *testing.T) {
	e := newEnv(t, "")
	st := e.events()
	if resp, body := e.post("/api/run", `{"scenario":"custom","line":"evil"}`); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("run: %d %s", resp.StatusCode, body)
	}
	// until fails on any stream line that is not an event: the guest's
	// "\n\ndata: " never forges one.
	evs := st.until(t, endOf(1))
	var stdout, details string
	var result *ResultEvent
	for _, ev := range evs {
		switch ev.Type {
		case TypeOutput:
			if ev.Output.Stream == "stdout" {
				stdout += ev.Output.Data
			}
		case TypeState:
			details += ev.State.Detail
		case TypeResult:
			if result != nil {
				t.Errorf("a second (forged?) result: %+v", ev.Result)
			}
			result = ev.Result
		}
	}
	want := "a\n\ndata: {\"type\":\"result\"}\n\nevent: x\r\n</script><script>alert(1)</script><!-- \x1b[31mred\x1b[0m \x1b]0;title\x07" +
		"bad \uFFFD bytes, split é joined\n" // a run of bad bytes becomes one U+FFFD
	if stdout != want {
		t.Errorf("stdout relayed as %q, want %q (as captured, made valid UTF-8, the split é kept whole)", stdout, want)
	}
	// The daemon's own texts (a state's detail, a result's error) lose
	// the host's paths; guest output is relayed as captured.
	dir := e.fake.stateDir
	if result == nil || strings.Contains(result.Error, dir) || !strings.Contains(result.Error, "<state>/runs/") ||
		strings.Contains(details, dir) || !strings.Contains(details, "<state>/runs/") {
		t.Errorf("details %q, result %+v: want <state> instead of %s", details, result, dir)
	}
}

func TestTooManyViewers(t *testing.T) {
	e := newEnv(t, "")
	for range maxViewers {
		e.events()
	}
	req, _ := http.NewRequest(http.MethodGet, e.base+"/api/events", nil)
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("viewer %d: %d, want 503", maxViewers+1, resp.StatusCode)
	}
}

func TestHubDropsASlowViewer(t *testing.T) {
	var logs syncBuf
	h := newHub(log.New(&logs, "", 0), nil)
	slow, _, err := h.subscribe()
	if err != nil {
		t.Fatal(err)
	}
	fast, _, _ := h.subscribe()
	got := 0
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-fast.ch:
				got++
			case <-fast.gone:
				return
			}
		}
	}()
	for i := range viewerQueue + 10 {
		h.publish(&Event{Type: TypeNotice, Notice: &NoticeEvent{Level: "info", Text: strconv.Itoa(i)}})
		time.Sleep(50 * time.Microsecond)
	}
	select {
	case <-slow.gone:
	default:
		t.Fatal("the slow viewer was not dropped")
	}
	if !strings.Contains(logs.String(), "fell more than 256 events behind") || h.viewerCount() != 1 {
		t.Errorf("log %q, %d viewers", logs.String(), h.viewerCount())
	}
	h.close()
	<-done
	if got < viewerQueue {
		t.Errorf("the fast viewer got %d events", got)
	}
	if _, _, err := h.subscribe(); err == nil {
		t.Error("subscribe after close succeeded")
	}
}

func TestListenAddressIsChecked(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close() //nolint:errcheck // test
	if _, err := New(t.Context(), Options{Listener: lis}); err == nil || !strings.Contains(err.Error(), "neither a daemon") {
		t.Errorf("New without a daemon or a session: %v", err)
	}
	if _, err := New(t.Context(), Options{}); err == nil {
		t.Error("New without a listener succeeded")
	}
}
