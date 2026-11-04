# 04 · Containers vs virtual machines

> ⏱ 15 minutes
> **After this, you can explain:** the core idea of our whole project: why we use virtual machines instead of containers.

---

## 1. A short story

An apartment building has one smart-lock system for every door.
A burglar finds a flaw in that system. Now **every apartment** opens.

A row of separate houses is different. Each house has **its own lock**. Break one lock and you get one house, nothing more.

That's the difference between a container and a virtual machine.

## 2. Containers: locked rooms, one shared manager

A **container** is a **locked room** inside your building:
- ✅ the program inside can't see the other rooms;
- ✅ it starts very fast and uses very little space;
- ⚠️ but it still asks **the same manager** (the same kernel) for everything.

Real life: **Docker** is the most famous container tool. Companies use it everywhere to keep their own apps tidy.

## 3. The weak spot: one manager for everyone

The kernel (the manager) is a giant rulebook: millions of lines of instructions. Big rulebooks have mistakes. A mistake in a program is called a **bug**.

If a program in a container finds a bug in the manager, it can trick the manager into opening **every room in the building**, just like the burglar and the smart locks.

So containers are fine for **your own** programs, but too risky for **strangers'** programs.

## 4. Virtual machines: a separate mini-office

A **virtual machine** (VM) is a **separate mini-office** built inside your building:
- ✅ it has **its own manager** (its own kernel);
- ✅ its walls are built by the **CPU itself**, the `AMD-V` feature you found in doc 01;
- ✅ if a program tricks the mini-office's manager, it only gets the mini-office.

Your real building's manager isn't even reachable from inside. 🛡️

The downside: a normal VM is **big and slow**. It has lots of features and takes seconds to start.
Real life: **VirtualBox** makes normal VMs. People use it to run Windows inside Linux, for example.

## 5. The best of both: a microVM

| | Strong walls? | Fast to start? |
|---|---|---|
| 📦 Container (Docker) | ⚠️ Weaker: shares the manager | ✅ Fast |
| 🏢 Normal VM (VirtualBox) | ✅ Strong | ❌ Slow (seconds) |
| 🔥 **microVM (Firecracker)** | ✅ **Strong** | ✅ **Fast** |

A **microVM** is a VM with almost everything removed: just a door, a desk, an intercom and a phone socket.
- **Fewer parts** mean fewer places for bugs.
- **Fewer parts** also mean it starts fast.

That's what Firecracker makes, and it's what we'll use.

## 6. Try it

```bash
ls /dev/kvm
```
👀 It prints `/dev/kvm`. This is the **door** that lets programs use your CPU's VM feature. In Linux, this feature is called **KVM**. Firecracker uses this door.

```bash
lsmod | grep kvm
```
👀 Among the lines, you see `kvm_amd` and `kvm`. That means the kernel's VM helper is **switched on**. ✅

## 7. In our project

Every job gets **its own microVM**, a mini-office with its own manager and walls built by the CPU.
Even if the program inside is evil and finds a bug, the damage stays in a mini-office that we throw away right after.

Coming up next: how the CPU builds those walls (doc 05), and how Firecracker uses them (doc 06).

## 8. Check yourself

<details><summary>1. What do all containers on a computer share, and why is that risky?</summary>

They share one manager (the kernel). If a program finds a bug in it, it can get into everything.
</details>

<details><summary>2. Why is a VM safer?</summary>

It has its own manager, and the CPU builds its walls. Tricking its manager only gets you the mini-office.
</details>

<details><summary>3. Why is a microVM both safe and fast?</summary>

It's a real VM (safe walls), but with almost everything removed. That means fewer bugs and a quicker start.
</details>

## 9. New words

| Word | Meaning |
|------|---------|
| Container | A locked room that shares the building's manager |
| Docker | A popular tool for making containers |
| Bug | A mistake in a program |
| microVM | A tiny, stripped-down VM that starts fast |
| KVM | Linux's name for the CPU's VM feature. Its door is `/dev/kvm` |

🎉 **That's the end of the foundations.** You now know enough to follow the project.
**Next:** 05 · KVM: how your CPU builds the mini-office walls (comes with week 1).
