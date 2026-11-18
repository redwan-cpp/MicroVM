# 08 · The guest agent and vsock

> ⏱ 15 minutes
> **After this, you can explain:** how the host gets a command into a running microVM and its output back out, and why the host never believes what comes back.

---

## 1. A short story

The mini-office has an **intercom** to the control room. An **assistant** sits inside, next to the stranger who does the work.
The control room says *"run this"*, and the assistant reads the results back, line by line, then says *"done, it ended with code 0"*.

But the stranger is in the same room. They can push the assistant aside, pick up the intercom and say *"done, all fine"*, whatever really happened.
So the control room keeps its own stopwatch and counts the pages itself.

That's our whole design in one picture.

## 2. From power-on to the assistant

In week 1 the job travelled on the kernel command line. Now the VM starts an assistant that takes jobs over the intercom:

```
InstanceStart
  → the guest kernel starts                   (doc 06)
  → it runs /sbin/demo-init as PID 1          (init=/sbin/demo-init)
  → demo-init mounts /proc, /sys, /dev, /tmp
  → sandbox_mode=agent?  start sandbox-agent  (/usr/local/bin/sandbox-agent)
  → the agent listens on vsock port 1024
```

About 85 ms after `InstanceStart`, the agent answers its first call.

The init script stays **PID 1**, the first process. That matters: when a process's parent dies, Linux hands the orphan to PID 1, and when it ends, PID 1 must collect ("reap") it, or it stays behind as a **zombie**: a dead process still taking a slot in the process table. Our init shell reaps every orphan while it waits for the agent. If the agent ever stops, init powers the VM off.

## 3. vsock: the intercom

**vsock** ("virtual socket") is a way for a VM and its host to talk **without a network card**. An address is two numbers:
- the **CID** says which machine: the host is 2, our guest is 3;
- the **port** says which program: our agent listens on **1024**.

Firecracker doesn't use the host kernel's vsock. On the host side, it turns the intercom into a **Unix socket** (doc 06): a file in the VM's private run folder, `v.sock`. To call the agent, the host:

1. connects to `v.sock`,
2. writes `CONNECT 1024` and a newline,
3. reads back `OK` and a number.

From then on the same connection carries bytes to and from the agent. If nobody listens on 1024 yet (the guest is still booting), Firecracker just hangs up; `internal/vsockconn` reports that as `ErrNotReady`, and the caller tries again 10 ms later.

## 4. gRPC streaming: output while it runs

On top of that connection we speak **gRPC** ([ADR-0002](../adr/0002-grpc-for-apis.md)). The messages are defined in `proto/sandbox/agent/v1/agent.proto`, and `make generate` turns them into Go code.

`Exec` is a **stream**: one request in, many answers out.

```
host  → agent   ExecRequest  argv ["sh", "-c", "echo hi; echo oops >&2"], timeout, output cap
host  ← agent   stdout "hi\n"
host  ← agent   stderr "oops\n"
host  ← agent   exit   exit_code 0      (exactly one, always last)
```

So you see output while the command runs, not only at the end.

## 5. Why there is no encryption (TLS) on vsock

TLS protects data crossing a network where others can read or change it. Here there is no network: the bytes go from the guest, through Firecracker, into a socket file in a folder only our user can open (mode 0700). Anyone who could get in between already controls the host, and then TLS can't help.
TLS would not even tell us that we're talking to *our* agent: its secret key would have to live in the guest, where untrusted code runs as root and can read it.

## 6. Why the host can't trust the agent

The command runs **as root inside the guest**. It can kill the agent and listen on port 1024 itself. So whatever answers there might flood us with output, never answer, send broken messages, or lie: *"exit code 0, nothing cut off"*.

So the host (`internal/agentclient`) checks everything itself:

| Danger | What the host does |
|--------|--------------------|
| Never finishes | Every call has a deadline. For `Exec`: the command's timeout + 1 s, then the host gives up and reports "timed out" |
| Floods output | The host counts the bytes. At the cap it stops reading and cancels the call |
| Huge messages | Messages over 64 KiB are refused |
| Broken order | No exit event, two exit events, or data after it: an error |
| Lies | "Timed out" and "output truncated" come from the host's own clock and byte count. The agent's claims are only logged |

This is trust boundary 3 in the [threat model](../threat-model.md) (threat T7).

## 7. Cleaning up processes

Each command runs in its own **process group**, a family of processes that can be signalled together. The agent sends the whole family `SIGKILL` (an un-ignorable "stop now") when the timeout runs out, when the host cancels, and as soon as the main process ends, so nothing it left in the background keeps running.

That is **best effort**: a process can leave its group (`setsid`). The agent then stops waiting for it after 500 ms, but it may still run. The real guarantee is simpler: after the run, **the host destroys the whole VM**, and with it every process inside.

## 8. Try it

```bash
grep -n "rpc " proto/sandbox/agent/v1/agent.proto
```
👀 The four calls the agent offers: Health, Exec, SyncTime and Reseed.

```bash
scripts/boot-vm.sh -q -- 'ls -l /dev/vsock /usr/local/bin/sandbox-agent'
```
👀 Inside a guest: the vsock device and the agent program on the read-only disk.

```bash
make e2e
```
👀 A real VM boots, and the test logs `InstanceStart → agent ready: ...ms`, runs `echo hi`, a command that times out, one that floods output, and checks for zombies. Then the VM is destroyed. (Needs KVM; it builds the guest image first.)

## 9. In our project

| Piece | Where |
|-------|-------|
| The API | `proto/sandbox/agent/v1/agent.proto` |
| The agent (guest) | `cmd/sandbox-agent`, `internal/agent` |
| `CONNECT` handshake (host) | `internal/vsockconn` |
| Client that checks everything (host) | `internal/agentclient` |
| Start and stop Firecracker (host) | `internal/vmm` |
| The whole path, on a real VM | `test/e2e` |

Later, `sandboxd` (week 4) uses exactly these pieces for every run.

## 10. Check yourself

<details><summary>1. Why does the agent run "next to" the command, and why is that a problem?</summary>

It has to be inside the VM to start the command. But the command runs as root in the same VM, so it can stop or replace the agent. That's why the host checks everything itself.
</details>

<details><summary>2. What does Firecracker's vsock look like on the host, and how do you reach port 1024?</summary>

A Unix socket file. You connect to it, write `CONNECT 1024`, and read back `OK` and a number. Then the connection goes to the agent.
</details>

<details><summary>3. A process escaped the process group with setsid. What stops it?</summary>

Destroying the VM after the run. The group kill can't reach it; it only stops Exec from waiting for it.
</details>

## 11. New words

| Word | Meaning |
|------|---------|
| Agent | The small program inside the VM that runs commands for the host |
| vsock | A VM-to-host connection without a network card |
| CID | The "which machine" number of a vsock address (host 2, guest 3) |
| Stream | One request, many answers, in order |
| PID 1 | The first process; it adopts and reaps orphans |
| Zombie | A finished process nobody has collected yet |
| Process group | A family of processes that can be signalled together |
| TLS | Encryption for connections over a network |
