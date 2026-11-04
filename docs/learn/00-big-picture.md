# 00 · The big picture

> ⏱ 15 minutes
> **After this, you can explain:** what our project does, and why.

---

## 1. A short story

You ask an AI helper: *"Delete my old downloads."*
It makes a typo, and deletes **all** your files instead.

How could it do that? Because:

> **A program you run can do anything you can do.**

Our project stops this from happening.

## 2. The idea: a sandbox

Think of a sandbox in a playground. Kids can make a mess, but **the sand stays in the box**.

We do the same for programs:
- each program gets **its own box** to run in;
- it **can't touch anything outside** the box;
- when it's done, we **throw the box away**.

## 3. Your computer is an office building

| In the building | In the computer |
|-----------------|-----------------|
| 👷 Workers | **CPU**: does the work |
| 🗂️ Desks | **Memory**: holds what's being worked on right now |
| 🗄️ Filing cabinets | **Disk**: stores everything long-term |
| 🏢 The manager | **Operating system**: in charge of everything |
| 🪪 Your keycard | **Your permissions** |

When you run a program, **you lend it your keycard**. The manager sees your keycard and lets the program open all your stuff. That's the danger from the story.

## 4. Three ways to let a stranger work in your building

**1. Give them your desk and your keycard.**
This is what normally happens. ❌ Dangerous.

**2. Put them in a locked room (a "container").**
Better, but they still use **the same manager** as everyone else. If they trick the manager, they can get everywhere. ⚠️ OK, not great.

**3. Build them a separate mini-office with its own manager (a "virtual machine").**
If they trick *that* manager, they only get the mini-office. Your real building is safe. ✅ Safest.

We use option 3, with a **tiny, fast** version of it called **Firecracker**.
Real life: Amazon built Firecracker and uses it to safely run other people's code (a service called AWS Lambda).

## 5. The speed trick: a save point

Setting up a mini-office from scratch takes a few seconds. That's too slow if every job needs a fresh one.

So we do what video games do: **save points**.
1. We set up one mini-office **once**.
2. We **save** it. This is called a **snapshot**.
3. For every job, we **load the save**. That takes a few milliseconds, faster than a blink.

## 6. Try it

Open a terminal: press `Ctrl+Alt+T`. Type this and press Enter:

```bash
whoami
```
👀 It prints your name, `red`. **That's the keycard every program you run gets.**

## 7. In our project

Every job will:
1. get a fresh mini-office, loaded from the save point;
2. run inside it;
3. get thrown away afterwards.

You'll build up to this, one step at a time.

## 8. Check yourself

<details><summary>1. Why can a program you run delete your files?</summary>

Because it runs with your keycard, so it can do anything you can.
</details>

<details><summary>2. Why is a mini-office (VM) safer than a locked room (container)?</summary>

The mini-office has its own manager. Tricking it doesn't get you into the real building.
</details>

<details><summary>3. What is a snapshot, in game terms?</summary>

A save point. We load it instead of starting from scratch, so it's very fast.
</details>

## 9. New words

| Word | Meaning |
|------|---------|
| Sandbox | A closed box where a program runs without harming anything outside |
| Container | A locked room that still shares the building's manager |
| Virtual machine (VM) | A separate mini-computer inside your computer, with its own manager |
| Firecracker | The tool we use to make tiny, fast VMs |
| Snapshot | A save point of a VM |

**Next:** [01 · Inside a computer](01-inside-a-computer.md)
