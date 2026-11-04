# 01 · Inside a computer

> ⏱ 15 minutes
> **After this, you can explain:** what the CPU, memory and disk do, and why too many open apps make a computer slow.

---

## 1. A short story

You open 40 browser tabs. Everything gets slow and laggy.
You close some tabs, and it's fast again.

Why? By the end of this page you'll know.

## 2. The three main parts

### 👷 CPU = the workers

The **CPU** does all the actual work.
Your computer has **16 workers**.

But you have hundreds of apps running, and only 16 workers. How?
**They take turns, super fast.** Like a chef cooking 5 dishes: stir one, flip another, back to the first. It looks like everything happens at once.

### 🗂️ Memory = the desks

**Memory** (also called **RAM**) is the workers' desk space.
Whatever they're working on **right now** has to be on a desk.

- ⚡ Very fast
- 📏 Limited: you have about **15 GB**
- 🧹 Cleared when you turn the computer off. That's why unsaved work gets lost.

### 🗄️ Disk = the filing cabinets

The **disk** stores everything long-term: your files, photos, apps.

- 📦 Big: you have about **464 GB**
- 💾 Keeps everything, even when the computer is off
- 🐢 Slow compared with the desks: about **1,000 times slower**

## 3. Program vs process

- A **program** is like a **recipe in a cookbook**. It just sits there.
- A **process** is **someone actually cooking that recipe**.

When you open an app, the computer:
1. takes the recipe from the cabinet (disk),
2. puts it on a desk (memory),
3. gives it to a worker (CPU) to follow.

## 4. Back to the story: why 40 tabs are slow

Every tab needs desk space. When the desks are full, the computer moves some papers back to the **slow cabinets**. Now workers keep walking to the cabinets and back. 🐢

**Lesson: desk space (memory) usually runs out first.** Remember this. It matters a lot in our project.

## 5. Try it

Open a terminal (`Ctrl+Alt+T`) and type each line:

```bash
nproc
```
👀 It shows `16`: your number of workers.

```bash
free -h
```
👀 Look at the `Mem:` line, in the `total` column: about `15Gi`. That's your desk space.

```bash
lscpu | grep Virtualization
```
👀 It shows `AMD-V`. This means your CPU can run mini-offices (virtual machines) fast. **Our project needs this, and you have it.** ✅

## 6. In our project

Each sandbox gets a **small share** of your computer, for example **1 worker and a small desk**.
Since desk space runs out first, it decides how many sandboxes can run at the same time.

## 7. Check yourself

<details><summary>1. Workers, desks, cabinets: which is the CPU, which is memory, which is the disk?</summary>

Workers = CPU. Desks = memory. Cabinets = disk.
</details>

<details><summary>2. What's the difference between a program and a process?</summary>

A program is a recipe sitting in the cookbook. A process is someone cooking it right now.
</details>

<details><summary>3. Why do too many tabs make the computer slow?</summary>

Memory (the desks) gets full, so the computer keeps moving things to and from the slow disk.
</details>

## 8. New words

| Word | Meaning |
|------|---------|
| CPU | The workers: does the work |
| Memory (RAM) | The desks: fast, limited, cleared at power-off |
| Disk | The cabinets: big, permanent, slower |
| Program | A recipe: instructions sitting on the disk |
| Process | A program that's running right now |

**Next:** [02 · The operating system](02-the-operating-system.md): the manager and your keycard.
