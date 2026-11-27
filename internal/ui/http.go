package ui

import (
	"bytes"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// web holds the page: HTML, CSS and vanilla JavaScript, no build step,
// nothing fetched from anywhere else. app.js draws every panel; live.js
// feeds it from the server's event stream, player.js from a recording.
//
//go:embed web
var web embed.FS

// webFile returns one embedded file; they are all there by construction.
func webFile(name string) []byte {
	data, err := web.ReadFile("web/" + name)
	if err != nil {
		panic("ui: the embedded page has no " + name) // a build without its own files
	}
	return data
}

// liveCSP is the Content-Security-Policy of every response: scripts,
// styles and connections from this server only, nothing inline, no
// frames, no <base>, forms only back here.
const liveCSP = "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; " +
	"frame-ancestors 'none'; base-uri 'none'; form-action 'self'"

// The deadlines of one HTTP exchange (beyond http.Server's own).
const (
	bodyDeadline  = 10 * time.Second // reading a request body
	writeDeadline = 10 * time.Second // writing a response, or one SSE event
	ssePing       = 15 * time.Second // an SSE comment, so dead connections are noticed
)

// routes builds the handler: every request goes through guard first.
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.page)
	mux.HandleFunc("GET /app.css", s.asset("app.css", "text/css; charset=utf-8"))
	mux.HandleFunc("GET /app.js", s.asset("app.js", "text/javascript; charset=utf-8"))
	if s.live {
		mux.HandleFunc("GET /live.js", s.asset("live.js", "text/javascript; charset=utf-8"))
		mux.HandleFunc("GET /api/events", s.events)
		mux.HandleFunc("POST /api/run", s.run)
		mux.HandleFunc("POST /api/stop", s.stop)
		mux.HandleFunc("POST /api/verify", s.verifyHandler)
	} else {
		mux.HandleFunc("GET /player.js", s.asset("player.js", "text/javascript; charset=utf-8"))
	}
	return s.guard(mux)
}

// guard is every request's checks, in order:
//
//  1. security headers on every response, whatever comes next;
//  2. the Host header must name this server (defeats DNS rebinding: a
//     web page whose name resolves to 127.0.0.1 still sends its own
//     name);
//  3. GET /?token=T with the right token sets the cookie and redirects
//     to /, so the token leaves the address bar (it is on a projector);
//  4. everything else needs the cookie (compared in constant time);
//  5. a request that changes something must come from this page: its
//     Origin must be this server (defeats cross-site requests), and
//     its body must be JSON.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", liveCSP)
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Cache-Control", "no-store")

		if !s.hosts[strings.ToLower(r.Host)] {
			plain(w, http.StatusForbidden, "this server answers only to "+s.hostPort+" (Host was "+clean(r.Host, 100)+")")
			return
		}
		if r.URL.Path == "/" && r.URL.Query().Has("token") {
			if r.Method != http.MethodGet || !s.tokenOK(r.URL.Query().Get("token")) {
				plain(w, http.StatusUnauthorized, "wrong token: open the address sandboxctl ui printed")
				return
			}
			// Not Secure: the page is plain HTTP on the loopback interface,
			// and browsers drop a Secure cookie there.
			http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: see above; HttpOnly and SameSite=Strict are set
				Name: s.cookieName, Value: s.token, Path: "/",
				HttpOnly: true, SameSite: http.SameSiteStrictMode,
			})
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		c, err := r.Cookie(s.cookieName)
		if err != nil || !s.tokenOK(c.Value) {
			plain(w, http.StatusUnauthorized, "open the address sandboxctl ui printed: it carries this page's token")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if origin := r.Header.Get("Origin"); origin != "http://"+r.Host {
				plain(w, http.StatusForbidden, "this request must come from the page itself (Origin "+clean(origin, 100)+")")
				return
			}
			if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" {
				plain(w, http.StatusForbidden, "this request must come from the page itself (Sec-Fetch-Site "+clean(site, 40)+")")
				return
			}
			if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				plain(w, http.StatusUnsupportedMediaType, "send JSON")
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, maxBody)
			_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(bodyDeadline))
		}
		next.ServeHTTP(w, r)
	})
}

// tokenOK compares a token with ours in constant time.
func (s *Server) tokenOK(t string) bool {
	return subtle.ConstantTimeCompare([]byte(t), []byte(s.token)) == 1
}

// plain answers with a short text.
func plain(w http.ResponseWriter, code int, text string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(code)
	_, _ = io.WriteString(w, "sandboxctl ui: "+text+"\n") //nolint:gosec // G705: text/plain with nosniff, never HTML
}

// writeJSON answers with v as JSON.
func writeJSON(w http.ResponseWriter, code int, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		plain(w, http.StatusInternalServerError, "encode the answer: "+err.Error())
		return
	}
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(writeDeadline))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(append(data, '\n'))
}

// page serves the page: the live one, or the replay one with the session
// in it.
func (s *Server) page(w http.ResponseWriter, _ *http.Request) {
	body := s.replayPage
	if s.live {
		body = webFile("live.html")
	}
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(writeDeadline))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(body)
}

// asset serves one embedded file.
func (s *Server) asset(name, contentType string) http.HandlerFunc {
	body := webFile(name)
	return func(w http.ResponseWriter, _ *http.Request) {
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(writeDeadline))
		w.Header().Set("Content-Type", contentType)
		_, _ = w.Write(body)
	}
}

// events is the page's event stream (Server-Sent Events): first what
// brings it up to date, then every event as it happens. Each event is
// one "data:" line holding one JSON object; encoding/json escapes every
// newline inside it, so nothing a guest writes can end an event early or
// forge another one.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)
	v, initial, err := s.hub.subscribe()
	if errors.Is(err, errTooManyViewers) {
		plain(w, http.StatusServiceUnavailable, fmt.Sprintf("%d pages are watching already; close one", maxViewers))
		return
	}
	if err != nil {
		plain(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	defer s.hub.unsubscribe(v)

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	var buf bytes.Buffer
	send := func(data []byte) error {
		buf.Reset()
		if data == nil {
			buf.WriteString(": ping\n\n")
		} else {
			buf.WriteString("data: ")
			buf.Write(data)
			buf.WriteString("\n\n")
		}
		_ = rc.SetWriteDeadline(time.Now().Add(writeDeadline))
		if _, err := w.Write(buf.Bytes()); err != nil {
			return err
		}
		return rc.Flush()
	}
	if send([]byte(`{"type":"hello"}`)) != nil {
		return
	}
	for _, data := range initial {
		if send(data) != nil {
			return
		}
	}
	ping := time.NewTicker(ssePing)
	defer ping.Stop()
	for {
		select {
		case data := <-v.ch:
			if send(data) != nil {
				return
			}
		case <-ping.C:
			if send(nil) != nil {
				return
			}
		case <-v.gone:
			return
		case <-r.Context().Done():
			return
		}
	}
}

// decodeBody reads a JSON body into v: one object, no unknown fields.
func decodeBody(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return fmt.Errorf("%w: the body is larger than %d KiB", errTooLarge, maxBody>>10)
		}
		return badRequest("%v", err)
	}
	if dec.More() {
		return badRequest("more than one JSON value")
	}
	return nil
}

var errTooLarge = errors.New("too large")

// errorCode is the HTTP status for an error of the API.
func errorCode(err error) int {
	switch {
	case errors.Is(err, errTooLarge):
		return http.StatusRequestEntityTooLarge
	case errors.Is(err, errBadRequest):
		return http.StatusBadRequest
	case errors.Is(err, errBusy):
		return http.StatusConflict
	}
	return http.StatusInternalServerError
}

// run is POST /api/run: start a scenario.
func (s *Server) run(w http.ResponseWriter, r *http.Request) {
	var req runRequest
	if err := decodeBody(r, &req); err != nil {
		writeJSON(w, errorCode(err), map[string]string{"error": err.Error()})
		return
	}
	ev, err := s.startScenario(req)
	if err != nil {
		writeJSON(w, errorCode(err), map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, ev)
}

// stop is POST /api/stop: end the running scenario (sandboxd stops its
// runs and destroys their clones).
func (s *Server) stop(w http.ResponseWriter, r *http.Request) {
	var req struct{}
	if err := decodeBody(r, &req); err != nil {
		writeJSON(w, errorCode(err), map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"stopped": s.stopScenario()})
}

// verifyHandler is POST /api/verify: verify the audit chain now.
func (s *Server) verifyHandler(w http.ResponseWriter, r *http.Request) {
	var req struct{}
	if err := decodeBody(r, &req); err != nil {
		writeJSON(w, errorCode(err), map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, s.verify())
}
