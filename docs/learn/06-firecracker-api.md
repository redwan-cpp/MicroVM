# 06 · Firecracker and its API

> ⏱ 15 minutes
> **After this, you can explain:** how four small requests turn an empty Firecracker program into a running microVM, and how that VM ends.

---

## 1. A short story

You order a flat-pack mini-office by phone.
The builder arrives with an empty plot and a phone, and waits. You call and say:
*"One worker, this much desk space. Here is the manager's handbook. Here is the filing cabinet, read-only. Go."*
A minute later the office is open for work.

Firecracker works exactly like that builder. The phone line is its **API**.

## 2. The idea: start empty, describe, then switch on

When you start the `firecracker` program, **there is no VM yet**. It only opens a "phone line" and waits for instructions.

| In the building | In Firecracker |
|-----------------|----------------|
| ☎️ The builder's phone line | The **API socket**: a file like `firecracker.sock` |
| 📝 One sentence on the phone | One **request**, for example "the VM gets 1 CPU and 256 MiB" |
| 🔑 "Go!" | The `InstanceStart` request: the VM powers on |

An **API** (application programming interface) is a set of requests a program understands.
Firecracker's requests are **HTTP**, the same language your web browser speaks to websites. But they don't travel over the internet: they go through a **Unix socket**, a special file that two programs on the same computer use to talk. Only programs that may open that file can send requests.

## 3. The four requests of `make boot`

`scripts/boot-vm.sh` sends these with `curl`. Each one is a **PUT** ("set this to that") with a small **JSON** body: a text format of names and values in `{ }`.

**1. The size of the machine**
```
PUT /machine-config   {"vcpu_count": 1, "mem_size_mib": 256}
```
One virtual CPU (a worker) and 256 MiB of memory (desk space).

**2. The kernel and its command line**
```
PUT /boot-source   {"kernel_image_path": ".../guest/out/vmlinux", "boot_args": "console=ttyS0 reboot=k panic=1 ..."}
```
The guest's own kernel (its manager), and the **kernel command line**: words that tell the kernel how to start. The ones we use:

| Word | Why |
|------|-----|
| `console=ttyS0` | Print messages on the first serial port. Firecracker passes it to its own output, so we can read it. |
| `reboot=k` | To reboot, use the (virtual) keyboard controller. Firecracker sees that and **exits**: "reboot" means "end the VM". |
| `panic=1` | If the kernel crashes, reboot after 1 second, so a broken VM ends instead of hanging. |
| `pci=off` | Firecracker's devices are not on a PCI bus, so don't search for one. |
| `nomodule` | Don't load kernel modules; this kernel has everything built in. |
| `init=/sbin/demo-init` | The first program to start (PID 1): our small script. |
| `i8042.noaux ...` | Skip probing for a mouse and other keyboard-controller extras. The probe alone cost about 0.5 s. |
| `quiet loglevel=1` | Print fewer kernel messages. |

Words the kernel doesn't know, like `sandbox_cmd_b64=...`, are handed to the init program as environment variables. That's how `make boot -- 'some command'` gets the command inside.

**3. The root disk**
```
PUT /drives/rootfs   {"drive_id": "rootfs", "path_on_host": ".../guest/out/rootfs.ext4", "is_root_device": true, "is_read_only": true}
```
A file on the host becomes the guest's disk, `/dev/vda`. Firecracker adds `root=/dev/vda ro` to the command line by itself.
It is **read-only**: no run can change the disk for the next one.

**4. Power on**
```
PUT /actions   {"action_type": "InstanceStart"}
```
The kernel starts. About 70 ms later our init script runs.

Every answer is a number: **204** means "done", and anything 4xx or 5xx comes with a reason, like `{"fault_message": "Invalid kernel path"}`. The script stops at the first error.

## 4. How the VM ends

There is no "shut down" request in our script. The VM ends itself:
the init script runs its job, then calls `reboot -f`. Because of `reboot=k`, Firecracker exits, and the VM and everything in its memory are gone.
If the job takes too long, the script kills the Firecracker process after the timeout. Then the run folder (socket, log) is deleted too.

## 5. Try it

```bash
make boot
```
👀 Every request is printed with a `→`, and each answer with `← 204 OK`. Then the guest prints facts about itself.

```bash
scripts/boot-vm.sh -q -- 'cat /proc/cmdline'
```
👀 The guest prints its kernel command line: our words, plus `root=/dev/vda ro` and a `virtio_mmio.device=...` that Firecracker added.

```bash
grep -c '^  /' third_party/firecracker/firecracker_spec.yaml
```
👀 `26`: the request paths Firecracker v1.17.0 understands (after `make firecracker`). We use only five of them.

## 6. In our project

- `scripts/boot-vm.sh` sends these four requests with `curl`, so you can see them.
- `internal/vmm` sends the same requests from Go: `PutMachineConfig`, `PutBootSource`, `PutDrive`, `Start`. It also starts the Firecracker process, keeps the last 64 KiB of the guest console, and cleans up the run folder.
- From week 2 there is a fifth request, `PUT /vsock`. It gives the VM an "intercom" to the host. That's doc 08.

## 7. Check yourself

<details><summary>1. What does Firecracker do right after it starts?</summary>

Nothing much: it opens its API socket and waits. There is no VM until the requests describe one and `InstanceStart` switches it on.
</details>

<details><summary>2. Why does `reboot` inside the guest end the whole VM?</summary>

Because of `reboot=k`: the guest kernel reboots through the virtual keyboard controller, and Firecracker treats that as "exit".
</details>

<details><summary>3. Why is the root disk read-only?</summary>

So that nothing a run does to the disk can be seen by the next run. Each run starts from the same clean disk.
</details>

## 8. New words

| Word | Meaning |
|------|---------|
| API | The set of requests a program understands |
| HTTP | The request language of the web; Firecracker speaks it too |
| Unix socket | A special file two programs on one computer use to talk |
| JSON | A text format for data: names and values inside `{ }` |
| Kernel command line | Start-up words for the kernel, like `console=ttyS0` |
| `InstanceStart` | The request that powers the VM on |

**Next:** 08 · the guest agent and vsock: how the host sends commands into a running VM.
