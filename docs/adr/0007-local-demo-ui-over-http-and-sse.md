# ADR-0007: Local demo UI over HTTP and SSE

- **Status:** Accepted
- **Date:** 2026-11-27 (week 4b, part A)
- **Amends:** [ADR-0002](0002-grpc-for-apis.md) (gRPC for both APIs): a third interface, browser ↔ `sandboxctl ui`, is HTTP

## Context

The project has to be shown to people who will not read a terminal: an instructor, a class. The user asked for a page with buttons (the presenter clicks, the audience watches) that shows what happens to each clone, what the host sees, the audit chain and the timings, live on the dev machine, and as a recording anywhere else, offline.

ADR-0002 made gRPC the only API style. A browser cannot speak gRPC over a Unix socket, so the page needs something in between. That something is a new control surface for sandboxd, reachable from any program that can open a TCP connection to the loopback interface, including every web page the presenter has open in the same browser.

## Decision

1. **`sandboxctl ui` is an HTTP server and an ordinary sandboxd client.** It reaches sandboxd only through the existing socket, with the same socket rules as every other `sandboxctl` command (ADR-0006): it starts runs with `Run` and reads `Info`, `List` and `HostStats`, each unary call with a 2 s deadline. It never runs anything itself, sends no env or stdin, and every daemon limit applies to it. sandboxd records the UI's own UID as the caller of the runs it starts.
2. **Browser ↔ server is JSON over HTTP for the buttons, and Server-Sent Events for everything that happens.** SSE is one long HTTP response that the browser's `EventSource` reads and reconnects by itself; it needs no library and no WebSocket. Each event is one `data:` line holding one JSON object; `encoding/json` escapes every newline inside it, so guest output can never end an event early or forge one.
3. **Two new daemon calls, additive only** (`buf breaking` passes): `Info` (mode, versions, architecture, limits, whether the host is itself a VM, each class with the cold boot measured by `make snapshots`, never a typed-in number) and `HostStats` (each live clone's PID, RSS and PSS, and counts of processes, run folders, jails and cgroups, read by the daemon, so in jailer mode root reads what the jailed UIDs own). `StateChange.run_id` and `RunInfo.jailed_uid` are added too.
4. **The UI is local and guarded** (threat model T13):
   - it listens on a loopback address only; there is no flag for anything else;
   - a random 128-bit token in the printed address becomes an HttpOnly, SameSite=Strict cookie named after the port, then a redirect to `/` drops the token from the address bar (it is on a projector); every request needs the cookie, compared in constant time;
   - the `Host` header must be this server's address or `localhost` with its port, which defeats DNS rebinding; requests that change something must carry this server's `Origin`, no cross-site `Sec-Fetch-Site`, a JSON body of at most 64 KiB; there are no CORS headers;
   - every response has `Content-Security-Policy: default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'`, `Referrer-Policy: no-referrer`, `X-Content-Type-Options: nosniff` and `Cache-Control: no-store`; the server has a 5 s header timeout, a 60 s idle timeout and 16 KiB of headers at most;
   - at most 8 pages watch at once, each with at most 256 queued events; a page that falls behind is dropped (and reconnects), never waited for; one scenario runs at a time.
5. **Guest output is untrusted everywhere in the page.** It is shown only through `textContent`, after removing control, escape and bidirectional-override characters; a Go test fails if the page's script uses `innerHTML`, `outerHTML`, `insertAdjacentHTML`, `document.write`, `eval(` or `Function(`. Each lane keeps only its newest 64 KiB, so a flood or a fork bomb cannot hang the projector.
6. **Replay is honest.** `--record FILE` writes every event the live page got, with its time, into a new file (0600, never overwritten) of at most 32 MiB. `--replay FILE` and `sandboxctl ui export` play it back with one player:
   - the LIVE badge exists only in the live page's HTML; the replay page's RECORDING banner is part of its HTML, so nothing in a session file can remove it, and the page title and the export's file name say "recording";
   - playback keeps the original timing, and a speed-up is labelled on screen; the custom command box is disabled; the host and timing panels say "recorded";
   - the recording carries the daemon's mode, architecture, versions and whether its host is a VM, so nested-VM numbers are never shown as a physical machine's, and each run's audit record hash, to check against `sandboxctl audit verify` on the machine that made it;
   - the session file is an allowlist: a file with any field the format does not define is refused. It never holds the token, env values, stdin or host paths (the daemon's texts have them replaced by `<state>`, `<repo>`, `~`).
7. **The export is one self-contained file** that opens from `file://` anywhere: the stylesheet and scripts inline, under a `<meta>` CSP of `default-src 'none'` with only their SHA-256 hashes (computed at export time), `connect-src 'none'`; the session is a JSON data block read with `textContent` and `JSON.parse`, encoded with `encoding/json`'s HTML escaping, so `</script>` or `<!--` in guest output cannot end it. Styles change only through `element.style`, never `style=` attributes, which the hashes would not allow.
8. **No new Go modules:** `net/http`, `embed` and the standard library. The page's vanilla JavaScript is the project's only non-Go code ([ADR-0001](0001-go-for-all-components.md) is Go for every component), because browsers run JavaScript and nothing else; it has no build step, no framework and loads nothing from anywhere else.

## Consequences

**Good:**
- The demo is clicks, not typing, and every number on the page is the daemon's own (phases, timings, memory, the audit chain); a recording plays anywhere without KVM, and the export is one file to send or open offline.
- The daemon learns nothing about browsers: the UI is one more client of its socket, and its two new calls change nothing.
- Recordings are evidence of what happened, not a mock-up: each run's audit hash ties it to the machine's log.

**Bad:**
- A new local control surface. Anyone who can read the printed address (the terminal, the shell history if it was copied there) can start runs as the user while the UI runs. Other local users can reach the port but not the token; a web page can reach the port but not the cookie, and the Host and Origin checks refuse it.
- Plain HTTP on the loopback interface: no TLS, so the cookie cannot be `Secure` (browsers drop such a cookie on plain HTTP).
- A recording holds guest output as captured; whoever gets the file sees it.
- One more language in the repository (about 1,200 lines of JavaScript), checked by Go tests for its sinks but not linted or unit-tested on its own.

**Alternatives rejected:**
- *gRPC-Web or a gRPC proxy:* a new dependency, and the browser would reach the daemon's API almost directly.
- *WebSockets:* two-way, which the page does not need, and not in the standard library.
- *A native desktop app or a terminal UI:* not usable from a projector laptop that has only a browser, and not exportable as one file.
- *Serving on all interfaces (`--allow-remote`):* dropped from the plan; on the "Later" list. A demo on another machine uses the export.
- *Scanning `/proc` by process name in the UI:* the UI would guess; the daemon knows its clones (`HostStats`), and in jailer mode only it can read the jailed processes.
