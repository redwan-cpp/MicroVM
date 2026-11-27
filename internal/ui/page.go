package ui

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"os"
	"regexp"
	"strings"

	"golang.org/x/sys/unix"
)

// The replay page is replay.html with the session embedded. `--replay`
// serves it with app.css, app.js and player.js as files of their own;
// `ui export` puts all three inside it, so the file opens from file://
// on any machine, offline. Both play the session with the same player:
// one honest code path. The page always says it is a recording, because
// the banner is in replay.html itself and nothing in a session file can
// remove it.
//
// The session is a JSON data block, <script type="application/json"
// id="session">, read by the player with textContent and JSON.parse,
// never run. encoding/json writes <, > and & in it as \u escapes, so
// guest output that holds "</script>" or "<!--" can never end the block
// or start markup.
//
// An exported file has no server to send a header, so its
// Content-Security-Policy is a <meta> tag: no source at all by default,
// and only the page's own inline <style> and <script> elements, named by
// their SHA-256, which is computed here when the file is made. Styles
// are set from the script through element.style, never with style=
// attributes, which those hashes would not allow.

// The markers replay.html holds for the parts filled in here.
const (
	markCSP     = "<!--CSP-->"
	markSession = "<!--SESSION-->"
	markTitle   = "<!--TITLE-->"
)

// replayPage builds the replay page for s. With inline, it is the
// self-contained export: the stylesheet and scripts inside, and a CSP
// meta tag with their hashes.
func replayPage(s *Session, inline bool) ([]byte, error) {
	data, err := json.Marshal(s) // HTML-escaping on: no "<" survives
	if err != nil {
		return nil, fmt.Errorf("ui: encode the session: %w", err)
	}
	if bytes.Contains(bytes.ToLower(data), []byte("</script")) || bytes.Contains(data, []byte("<!--")) {
		return nil, errors.New("ui: BUG: the session's JSON could end its <script> block") // encoding/json never allows this
	}
	page := string(webFile("replay.html"))
	title := "MicroVM Sandbox · recording from " + s.Meta.CreatedAt.UTC().Format("2006-01-02 15:04 UTC")
	page = strings.Replace(page, markTitle, html.EscapeString(title), 1)
	page = strings.Replace(page, markSession, `<script type="application/json" id="session">`+string(data)+`</script>`, 1)
	if !inline {
		return []byte(strings.Replace(page, markCSP, "", 1)), nil
	}

	var scriptHashes, styleHashes []string
	for _, f := range []struct {
		tag, name, element string
		hashes             *[]string
	}{
		{`<link rel="stylesheet" href="app.css">`, "app.css", "style", &styleHashes},
		{`<script src="app.js"></script>`, "app.js", "script", &scriptHashes},
		{`<script src="player.js"></script>`, "player.js", "script", &scriptHashes},
	} {
		body := string(webFile(f.name))
		if endsElement(body, f.element) {
			return nil, fmt.Errorf("ui: BUG: %s holds </%s or <!--, which would end its inline element", f.name, f.element)
		}
		if !strings.Contains(page, f.tag) {
			return nil, fmt.Errorf("ui: BUG: replay.html has no %s", f.tag)
		}
		page = strings.Replace(page, f.tag, "<"+f.element+">"+body+"</"+f.element+">", 1)
		*f.hashes = append(*f.hashes, cspHash(body))
	}
	csp := "default-src 'none'; script-src " + strings.Join(scriptHashes, " ") + "; style-src " + strings.Join(styleHashes, " ") +
		"; img-src data:; connect-src 'none'; base-uri 'none'; form-action 'none'"
	page = strings.Replace(page, markCSP, `<meta http-equiv="Content-Security-Policy" content="`+csp+`">`, 1)
	return []byte(page), nil
}

// cspHash is a CSP source for an inline element's exact text.
func cspHash(body string) string {
	sum := sha256.Sum256([]byte(body))
	return "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
}

// endsElement reports whether text, put inside <element>…</element>,
// could end it early (or, in a script, change how it is parsed).
func endsElement(text, element string) bool {
	lower := strings.ToLower(text)
	return strings.Contains(lower, "</"+element) || strings.Contains(text, "<!--")
}

// DefaultExportName is the export's file name when none is given: it
// says it is a recording, and when it was made.
func DefaultExportName(s *Session) string {
	return "microvm-recording-" + s.Meta.CreatedAt.UTC().Format("2006-01-02-1504") + ".html"
}

// exportName is the shape of a name Export accepts.
var exportName = regexp.MustCompile(`(?i)\.html?$`)

// Export writes the self-contained replay page of s to path, a new file:
// an existing one is never overwritten.
func Export(s *Session, path string) error {
	if !exportName.MatchString(path) {
		return fmt.Errorf("ui: %s: an export is an HTML file; give it a name ending in .html", path)
	}
	page, err := replayPage(s, true)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, 0o644) //nolint:gosec // G302,G304: a page to share, at the path the user gave
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("ui: %s exists already: an export never overwrites a file; choose another --out", path)
		}
		return fmt.Errorf("ui: %w", err)
	}
	_, err = f.Write(page)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("ui: write %s: %w", path, err)
	}
	return nil
}
