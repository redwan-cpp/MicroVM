# Learning guide

You don't need to know anything to start. Each doc is a **15-minute read** with only a few new words.
If something isn't clear, that's the doc's fault, not yours. Read it once more, slowly, and ask about anything that still doesn't click.

## The one picture we use: an office building

Think of your computer as an office building:

| In the building | In the computer |
|-----------------|-----------------|
| 👷 Workers | **CPU**: does the work |
| 🗂️ Desks | **Memory**: holds what's being worked on right now |
| 🗄️ Filing cabinets | **Disk**: stores everything long-term |
| 📖 An instruction manual | **Program** |
| 🏢 The manager | **Operating system**: in charge of everything |
| 🪪 Your keycard | **Your permissions**: what you're allowed to open |

New rows get added as you reach new topics.

<details><summary>The full table, for later (you don't need it yet)</summary>

| In the building | In the computer | Doc |
|-----------------|-----------------|-----|
| A locked room that still uses the same manager | Container | 04 |
| A separate mini-office with its own manager | Virtual machine (VM) | 04 |
| The warehouse guards around the mini-office | Hypervisor (KVM) | 05 |
| A tiny mini-office: just a door, a desk, an intercom, a phone socket | microVM (Firecracker) | 06 |
| A video-game save point for a mini-office | Snapshot | 09 |
| A shared textbook: photocopy only the page you scribble on | Copy-on-write | 09 |
| A guard who takes your keycard and locks you in before work | Jailer | 10 |
| An intercom between the mini-office and the control room | vsock | 08 |
| The phone line to the outside world | Network | 13 |
| A call-blocking list | Firewall | 14 |
| The control-room operator | `sandboxd` | 11 |
| The request form | `sandboxctl` | 11 |
| The assistant inside the mini-office | `sandbox-agent` | 08 |
| The rental contract | Policy | 12 |
| The logbook | Audit log | 11 |
</details>

## Every doc has the same parts

1. **A short story:** why it matters.
2. **The idea:** in the office-building picture.
3. **Try it:** up to 3 safe commands. They only *show* things and never change anything.
4. **In our project:** where you'll meet it again.
5. **Check yourself:** 3 questions, answers hidden.
6. **New words:** a short list.

## Reading order

**Start here (no project code needed):**
1. [00 · The big picture](00-big-picture.md): what we're building and why
2. [01 · Inside a computer](01-inside-a-computer.md): workers, desks, cabinets
3. [02 · The operating system](02-the-operating-system.md): the manager and your keycard
4. [03 · The terminal](03-the-terminal.md): typing commands
5. [04 · Containers vs virtual machines](04-containers-vs-vms.md): the core idea of the project

**Then the project** (docs 05–18), written week by week as the code gets built. The [build plan](../build-plan.md) lists them.

## How to study

- Read it once. It's fine if not everything clicks yet.
- Type the "Try it" commands yourself.
- Answer the questions out loud before you peek.
