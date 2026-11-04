# 03 · The terminal

> ⏱ 15 minutes
> **After this, you can:** open a terminal, move between folders, and see what's inside them.

---

## 1. A short story

In movies, hackers type fast into black windows full of text. It looks like magic.

It isn't. That black window is just **a chat with your computer**. You type a request, and it answers. By the end of this page, you'll be doing it too.

## 2. The idea: texting the manager

Normally you use the computer by **clicking** on things.
The **terminal** lets you use it by **typing** instead.

It works like a chat:
1. You type a **command**, for example `whoami`.
2. You press **Enter**.
3. The computer types back an answer.

Why bother, if clicking works? Because typing is **faster, exact, and repeatable**. Our whole project is controlled this way.

## 3. How a command is built

A command is like a short sentence:

```
ls      -l       Downloads
│       │        └── what: which folder
│       └── how: "-l" means "show details"
└── the action: "ls" means "list what's here"
```

**Action, then how, then what.** Most commands follow this pattern.

## 4. Folders and paths

Your files live in **folders** (also called directories), inside other folders, like boxes inside boxes.

A **path** is the address of a folder or file:

```
/home/red/Downloads
```

Read it from the left: start at the top (`/`), go into `home`, then into `red`, then into `Downloads`.

**Shortcut:** `~` means **your home folder**. For you, `~` is the same as `/home/red`.

## 5. The three commands you'll use most

| Command | Means | Like asking |
|---------|-------|-------------|
| `pwd` | print where I am | "Which room am I in?" |
| `ls` | list what's here | "What's in this room?" |
| `cd` | change folder | "Take me to that room." |

## 6. Handy tricks

- ⌨️ **Tab key:** type the first few letters of a name and press `Tab`. It finishes the name for you.
- ⬆️ **Up arrow:** brings back your last command, so you don't retype it.
- 🛑 **`Ctrl+C`:** stops a command that's running.
- 📋 **Copy and paste:** in the terminal it's `Ctrl+Shift+C` and `Ctrl+Shift+V` (with *Shift*!).

## 7. Try it

```bash
pwd
```
👀 It shows `/home/red`. You start in your home folder.

```bash
cd ~/Downloads/project/OS
```
👀 It shows nothing. That's normal: no news is good news. You've moved into our project's folder.

```bash
ls
```
👀 You see `README.md`, `docs`, `SECURITY.md` and more. **Those are our project's files.** 🎉

## 8. In our project

When the project is done, you'll run a sandbox by typing a command like this:

```
sandboxctl run --policy small.yaml -- python3 homework.py
```

It follows the same pattern: the action (`sandboxctl run`), how (`--policy small.yaml`), and what to run (`python3 homework.py`).

## 9. Check yourself

<details><summary>1. What does the terminal let you do?</summary>

Use the computer by typing commands instead of clicking.
</details>

<details><summary>2. What do <code>pwd</code>, <code>ls</code> and <code>cd</code> do?</summary>

`pwd` shows where you are. `ls` lists what's in the folder. `cd` moves you to another folder.
</details>

<details><summary>3. What does <code>~</code> mean?</summary>

Your home folder. For you, that's `/home/red`.
</details>

## 10. New words

| Word | Meaning |
|------|---------|
| Terminal | A window where you type commands to the computer |
| Command | One typed request, like `ls` |
| Folder (directory) | A box that holds files and other folders |
| Path | The address of a file or folder, like `/home/red/Downloads` |
| `~` | Shortcut for your home folder |

**Next:** [04 · Containers vs virtual machines](04-containers-vs-vms.md)
