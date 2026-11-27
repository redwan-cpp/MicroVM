# 11 · The daemon and the life of one run

> ⏱ 20 minutes
> **After this, you can explain:** what happens between typing `sandboxctl run` and getting your prompt back, why every run ends the same way whatever goes wrong, how the audit log shows that nobody changed it afterwards, and what the demo page shows and why it is locked.

---

## 1. A short story

The control room now has an **operator** at a desk, and a **request form** you hand in at the window.
You write *"run this, small office, 30 seconds"* and pass it through. The operator checks the form, waits for a free office, has one set up from the saved copy, gives the assistant the job, reads the results back to you as they come, and has the office torn down.
Then, before saying "done", the operator writes one line in a **logbook** whose every page carries a fingerprint of the page before.

The operator is `sandboxd`. The form is `sandboxctl`. The logbook is the audit log.

## 2. The window: a Unix socket

`sandboxctl` and `sandboxd` talk gRPC ([ADR-0002](../adr/0002-grpc-for-apis.md)) over a **Unix socket**, a file that two programs on one computer use to talk (doc 06). Who may use it is decided like any file: by its owner and mode ([ADR-0006](../adr/0006-daemon-socket-and-state-ownership.md)).

| | Rootless mode | Jailer mode |
|---|---|---|
| The daemon runs as | you | root |
| Its socket | `$XDG_RUNTIME_DIR/sandboxd/sandboxd.sock` | `/run/sandboxd/sandboxd.sock` |
| Who may connect | only you (0600 in a 0700 folder) | root and the group `sandbox` (0660) |

The kernel also tells the daemon **who** connected (`SO_PEERCRED`): the caller's UID, which no request can fake. It goes into `sandboxctl ps` and into the audit record.

The daemon also takes a **lock** on the state folder (`<state>/lock`) for as long as it runs. A second daemon on the same folder refuses to start: two daemons appending to one audit log would break its chain.

## 3. The life of one run

Every run is a little **state machine**: a fixed list of states and the only steps allowed between them.

```
PENDING ──→ RESTORING ──→ MOUNTING ──→ RUNNING ──→ STOPPING ──→ DONE
   │            │            │            │                  └─→ FAILED
   └────────────┴────────────┴────────────┴──→ STOPPING (something went wrong)
```

| State | What happens | About how long |
|-------|--------------|----------------|
| PENDING | accepted; waits for one of the `--max-concurrent` slots (4) | 0 ms when a slot is free |
| RESTORING | a clone from the base snapshot (doc 09) | 20 ms |
| MOUNTING | the clone's own scratch disk at `/work` | 4 ms |
| RUNNING | your command; output streams back as it comes | your command |
| STOPPING | the clone is destroyed, then the audit record written | 60–100 ms |
| DONE / FAILED | reported to you, last | |

Each step is stamped with the time (`sandboxctl run -v` prints them). Any other step, say RUNNING back to RESTORING, is a bug, and the code refuses it.

The important rule: **every run ends with STOPPING**. A failed restore, a broken agent, a timeout, you pressing Ctrl-C, the daemon shutting down: whatever happens, the clone is destroyed exactly once and the record is written, and only then do you hear "DONE" or "FAILED".

How it ended (the **outcome**) is judged by the host, never by what the agent says (doc 08): `EXITED` with the command's exit code, `TIMED_OUT`, `OUTPUT_TRUNCATED`, or `FAILED` with a reason. `sandboxctl run` exits with the command's own code, or 124 for a timeout, 123 for cut-off output and 125 when the run failed. A command can exit with 123, 124 or 125 itself, so those numbers alone prove nothing: `-v` and `sandboxctl ps RUN_ID` show the real outcome.

## 4. `/work`: a disk of its own

Each clone gets a **scratch disk**, mounted at `/work`, where commands run unless they ask for another folder. The snapshot store keeps one formatted, empty ext4 disk per size class (`scratch.ext4`, made by `make snapshots`), and each clone gets its own **copy**: never a hard link, because writes must stay private. The copy keeps the template's "holes" (doc 09), so it takes about 0.3 ms.

One catch comes from the snapshot. The VM it was made from had a blank placeholder in that place, and its kernel may remember a few blank blocks of it. So before mounting, the agent tells the guest kernel to forget what it cached of the disk (`BLKFLSBUF`). Three clones writing to `/work` at once, each seeing only its own file, is the test that this works.

## 5. The logbook: a hash chain

The audit log, `<state>/audit/audit.jsonl`, gets one line per run: who ran what, in which mode, the outcome, every state change and the timings. It holds the command (`argv`), but **never the values of the environment variables or the stdin**: those may hold secrets.

Each line carries two fingerprints (SHA-256 hashes, doc 09):

```
{"record":{...run 1...},"prev_hash":"0000…0000","hash":"7a3f…"}
{"record":{...run 2...},"prev_hash":"7a3f…",    "hash":"c41e…"}
{"record":{...run 3...},"prev_hash":"c41e…",    "hash":"09bd…"}
```

`hash` is the SHA-256 of `prev_hash` followed by the record's exact bytes. Change one letter in run 2 and its hash no longer matches; fix that hash too, and run 3's `prev_hash` no longer matches. Delete or swap records, same thing. `sandboxctl audit verify` walks the chain and names the first line that breaks it.

What it **cannot** catch: someone deleting the newest records (a shorter chain is still a chain) or rewriting the whole file with fresh hashes. Whoever can write the file can do that: you in rootless mode, root in jailer mode. Keeping a copy of the newest hash somewhere else is on the "Later" list.

The log **fails closed**. If the chain is broken when the daemon starts, it refuses to start (`sandboxd rotate-audit` moves the file aside as evidence and starts a new chain). If writing a record ever fails, the daemon accepts no more runs: no run without its record.

## 6. When things go away

- **You press Ctrl-C** (or your program dies): the call ends, the daemon stops the run, destroys the clone and writes a FAILED record: "the caller went away".
- **The daemon is stopped** (SIGTERM, Ctrl-C): it accepts no new runs, stops the running ones (each still destroyed and recorded), closes the log and removes its socket, within 10 seconds. In jailer mode it also removes the two folders the jailer made as root around every clone, `jail/firecracker/` and the cgroup `microvm/`, once they are empty, so a clean run leaves nothing of root's behind. That bound is firm: a run still in STOPPING when it runs out (a clone that will not die, say) can lose its audit record, because the log is closed under it. The daemon logs that loudly ("runs still stopping", "AUDIT RECORD NOT WRITTEN"), and its exit code says the shutdown failed.
- **The daemon crashes**: in rootless mode the kernel kills every Firecracker it started (the parent-death signal, doc 10). In jailer mode that signal is gone, and a chroot or cgroup may stay behind; the **reconciler** in week 7 cleans those up when the daemon starts again. What the daemon keeps in memory, the last 100 runs and their output for `ps` and `logs`, is gone after a restart; the audit log is what lasts.

## 7. The window with a view: the demo page

Typing commands in front of an audience is slow and hard to follow. `sandboxctl ui` is a **web page** for showing all of the above: the presenter clicks a button, and everyone watches the clones go through their states.

```
browser ──HTTP + SSE──→ sandboxctl ui ──gRPC, the same socket──→ sandboxd ──→ clones
 (shows)                 (asks and relays)                         (does the work)
```

- The page talks to `sandboxctl ui` over **HTTP**, because a browser cannot open the daemon's Unix socket or speak gRPC there ([ADR-0007](../adr/0007-local-demo-ui-over-http-and-sse.md)). A button sends a small JSON request; everything that happens comes back over **Server-Sent Events**: one long answer that sends a line of JSON for each event (a state change, output, a result), as it happens.
- `sandboxctl ui` is just another client of the daemon, like `sandboxctl run`: it asks for runs with `Run`, and for the panels it asks `Info` (versions, the mode, the cold boot of each snapshot), `List` (the history) and `HostStats` (how many Firecracker processes, run folders, jails and cgroups exist, and each clone's memory). It never starts a VM itself, and the audit log names the user who started it as the caller.
- **RSS and PSS**, in the host panel: RSS counts every page a process has in memory; PSS splits a page shared by n processes into n parts. Clones share the snapshot's memory file (doc 09), so their PSS adds up to much less than their RSS. That is copy-on-write, measured.
- **It is a door, so it has a lock.** The page is served only on `127.0.0.1`, and only to a browser that opened the address printed with a random **token** (kept as a cookie afterwards). It refuses requests addressed to any other name (a trick called DNS rebinding) and changes from any other page (cross-site requests). Guest output is shown only as plain text, never as markup, so a command that prints `<script>` gets its text shown, nothing more (threat model T13).
- **Record, replay, export.** `--record FILE` keeps every event the page got, with its time. `--replay FILE` plays it back with the original timing and needs no daemon; `sandboxctl ui export` makes one HTML file of it that opens offline anywhere. A replay always says **RECORDING**, and when and on what kind of machine it was made: a recording is evidence, not a live demo, and each run's audit hash ties it to the log of the machine that made it.

## 8. Try it

```bash
make demo1
```
👀 The whole of Demo 1: the daemon starts, three clones run at once (each sees only its own secret in `/work`), then `ps`, `logs`, `audit verify`, and nothing left behind.

```bash
bin/sandboxd serve &  until bin/sandboxctl ps > /dev/null 2>&1; do sleep 0.1; done
bin/sandboxctl run -v -- sh -c 'pwd; ls'; kill %1
```
👀 The daemon starts (the loop waits until it answers: it checks every snapshot hash first). Then every state change with its time, `/work` and `lost+found` (the only thing on a fresh ext4 disk), the outcome and the phase timings.

```bash
bin/sandboxctl audit verify && tail -n 1 guest/out/state/audit/audit.jsonl
```
👀 The chain checked, and the newest record: the run, its states, its `prev_hash` and `hash`.

```bash
make demo-ui
```
👀 Open the address it prints, click "Three clones, private /work": three lanes fill at once, the host panel counts three Firecrackers (PSS well below RSS), then drops back to "0 VMs, 0 folders left"; the audit panel shows three new records, each pointing at the one before.

## 9. In our project

| Piece | Where |
|-------|-------|
| The API | `proto/sandbox/daemon/v1/sandbox.proto` |
| The daemon: socket, slots, the run, shutdown | `internal/daemon`, `cmd/sandboxd` |
| The state machine and the host's verdict | `internal/run` |
| The audit log | `internal/audit` |
| The scratch template and its copy | `internal/snapshot` (`scratch.go`) |
| Mounting `/work` in the guest | `internal/agent` (`scratch.go`) |
| The CLI | `cmd/sandboxctl` |
| Demo 1, as a test | `test/e2e/daemon_test.go`, `test/e2e/jailer_test.go` |
| The demo page | `internal/ui` (server, recorder, export), `internal/ui/web` (the page), `cmd/sandboxctl/ui.go`, `test/e2e/ui_test.go` |

## 10. Check yourself

<details><summary>1. You press Ctrl-C while your command runs. What happens to the clone?</summary>

The call ends, so the daemon stops the run. It still goes through STOPPING: the clone is destroyed and a FAILED record ("the caller went away") is written.
</details>

<details><summary>2. Someone edits run 5's exit code in the audit log and fixes its hash. Does verify notice?</summary>

Yes: run 6's `prev_hash` still holds run 5's old hash, so the chain breaks at run 6. Only deleting the newest records, or rewriting every hash from the edit onwards, goes unnoticed.
</details>

<details><summary>3. Why is each clone's scratch disk a copy and not a hard link, when the memory file is a link?</summary>

The memory file is mapped copy-on-write: clones share it but their writes stay private in memory. A disk file is written in place, so a shared one would let one clone read another's files.
</details>

<details><summary>4. A web page you visit tries to send a request to the demo page on 127.0.0.1:8090. What stops it?</summary>

It has no token (the cookie is HttpOnly and SameSite=Strict, so another site's request does not carry it), its `Origin` is not the demo page's, and if it renamed itself to resolve to 127.0.0.1 its `Host` header would still carry its own name. Any one of these makes the server refuse it.
</details>

## 11. New words

| Word | Meaning |
|------|---------|
| Daemon | A program that runs in the background and serves requests |
| State machine | A fixed set of states and the only steps allowed between them |
| Slot | One of the places for a running clone; a run waits in PENDING for one |
| Outcome | How a run ended, as the host judges it |
| Hash chain | Records that each carry a fingerprint of the one before |
| Fail closed | When something breaks, stop instead of carrying on unchecked |
| SO_PEERCRED | The kernel's answer to "who is at the other end of this socket?" |
| Server-Sent Events (SSE) | One long HTTP answer in which the server sends a line for each event, as it happens |
| RSS / PSS | A process's memory: every page it holds (RSS), or its fair share of pages it shares with others (PSS) |
| Token | A long random secret; whoever has it may use the page |

**Next:** 12 · policies (week 5): what a run may ask for, and how the daemon says no.
