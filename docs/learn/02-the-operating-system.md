# 02 · The operating system

> ⏱ 15 minutes
> **After this, you can explain:** what the operating system does, and why a program with your keycard is dangerous.

---

## 1. A short story

You install a new app on your phone. It pops up:
*"Allow this app to access your photos?"*

Who is asking? Not the app. It's your phone's **operating system**, the manager, checking before it lets the app in.

## 2. The idea: the manager runs the building

In our office building, **workers can't just grab things**. They can't open a cabinet, use the phone line, or take a desk on their own.

They have to **ask the manager**:
- 🗄️ "Can I open this file?"
- 🌐 "Can I use the internet?"
- 🗂️ "Can I have more desk space?"

The manager checks their keycard, then says **yes** or **no**.

That manager is the **operating system** (OS for short). Windows, macOS, Android and Kali Linux are all operating systems.

The most important part of the manager, the part that really controls everything, is called the **kernel**. Think of it as the manager's private office.
Real life: Android phones use the same kind of kernel as your Kali computer, the **Linux** kernel.

## 3. Keycards: users and permissions

Every person on the computer has a **user** account. Yours is `red`.

Your user is your **keycard**. It opens:
- ✅ your own files (photos, homework, downloads)
- ❌ not other people's files
- ❌ not the computer's important system files

What a keycard is allowed to open is called its **permissions**.

## 4. The master key: root and sudo

There is one special user called **root**. Root has the **master key**: it can open *everything* and change *anything*.

Normally you don't use the master key. When you need it for one command, you type `sudo` in front of it.
**`sudo` = "borrow the master key for this one command."** The computer asks for your password first.

That's why the install command from earlier starts with `sudo`: installing software changes system files.

💡 **Run `sudo` commands in your own terminal** (`Ctrl+Alt+T`), not inside a terminal that's built into another app. Some apps run their terminal inside a little sandbox of their own that **blocks the master key on purpose**. If you try there, you'll see a *"no new privileges"* error. It's the same kind of protection our project will put around every mini-office.

## 5. Why bad programs are dangerous

Here's the problem: **the manager only checks the keycard. It doesn't check if the program is honest.**

- A program you run carries **your** keycard, so it can delete or send away **your** files.
- A program run with `sudo` carries the **master key**, so it can wreck **the whole computer**.

⚠️ **Rule for life:** never run a program you don't trust, and *especially* never with `sudo`.

## 6. Try it

```bash
uname -r
```
👀 It shows your kernel's version, something like `7.1.5+kali-amd64`. That's your manager's "version number".

```bash
cat /etc/shadow
```
👀 It says `Permission denied`. This file holds the computer's password data, and **your keycard isn't allowed to open it.** That's the manager saying no. ✅

```bash
sudo whoami
```
👀 Type your password (nothing shows while you type, that's normal). It prints `root`. For that one command, you held the master key.

## 7. In our project

- Each mini-office (VM) gets **its own manager** (its own kernel). A program inside can trick *that* manager, but it never gets near your real one.
- Programs in the sandbox **never get your keycard**. They can't see your files at all.
- Our control-room program, `sandboxd`, does need the master key, because it builds the mini-offices. So it has to be written very carefully.

## 8. Check yourself

<details><summary>1. A program wants to read a file. Who does it have to ask?</summary>

The operating system (its kernel, the manager). The manager checks the keycard (the user's permissions) and says yes or no.
</details>

<details><summary>2. What does <code>sudo</code> do?</summary>

It lets you borrow the master key (root) for one command, after you type your password.
</details>

<details><summary>3. Why is running a bad program with <code>sudo</code> worse than without it?</summary>

Without `sudo` it carries your keycard, so it can harm your files. With `sudo` it carries the master key, so it can harm the whole computer.
</details>

## 9. New words

| Word | Meaning |
|------|---------|
| Kernel | The core of the operating system: the manager's private office |
| User | An account on the computer, your keycard (yours is `red`) |
| Permissions | What a keycard is allowed to open |
| root | The special user with the master key |
| `sudo` | Borrow the master key for one command |

**Next:** [03 · The terminal](03-the-terminal.md)
