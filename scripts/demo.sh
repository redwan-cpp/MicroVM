#!/usr/bin/env bash
# demo.sh: the live demo of MicroVM Sandbox, one step at a time.
#
# Run it with `make demo`. Before each step it prints what the step shows and
# waits for Enter, so you can explain. Every command is printed before it runs,
# so the audience sees exactly what is typed.
#
#   scripts/demo.sh              # presenter mode: pause before every step
#   scripts/demo.sh --no-pause   # run straight through (rehearsal)
#
# Needs: make firecracker, make guest (run them before the demo; they download).
set -euo pipefail

PAUSE=1
[[ "${1:-}" == "--no-pause" ]] && PAUSE=0

if [[ -t 1 ]]; then
  B=$'\e[1m' D=$'\e[2m' G=$'\e[32m' Y=$'\e[33m' M=$'\e[35m' N=$'\e[0m'
else
  B="" D="" G="" Y="" M="" N=""
fi

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${REPO_ROOT}"

STEP=0
step() { # step "title" "one line on why it matters"
  STEP=$((STEP + 1))
  echo
  echo "${M}${B}━━ ${STEP}. $1 ━━${N}"
  echo "${D}$2${N}"
  pause
}
pause() {
  if ((PAUSE)); then
    read -r -p "${D}   [Enter]${N} " _ < /dev/tty || true
  fi
}
run() { # run 'command line': show it, then run it; a failing command is part of the demo
  echo "${Y}\$ $1${N}"
  bash -c "$1" || echo "${D}   (exit code $?)${N}"
}

# --- Preflight: fail now, not halfway through the talk ----------------------
missing=()
command -v go > /dev/null || missing+=("Go (add ~/.local/go/bin to PATH)")
[[ -x third_party/firecracker/firecracker ]] || missing+=("Firecracker (make firecracker)")
[[ -f guest/out/vmlinux && -f guest/out/rootfs.ext4 ]] || missing+=("guest image (make guest)")
if ((${#missing[@]})); then
  printf 'not ready for the demo, missing:\n' >&2
  printf '  - %s\n' "${missing[@]}" >&2
  exit 1
fi

clear 2> /dev/null || true
cat <<EOF
${B}MicroVM Sandbox${N}
Run untrusted code in disposable Firecracker microVMs.

${B}The problem.${N} AI agents and scripts run code with our permissions: one bad
command can delete files or leak secrets. Containers share the host kernel,
so one kernel bug lets code break out. Full VMs are safe but take seconds.

${B}The idea.${N} Give every run its own hardware-isolated microVM. Start it in
milliseconds, apply a policy (CPU, memory, files, network), run the command,
then destroy everything.

${D}Research and teaching project, not security audited (see SECURITY.md).${N}
EOF
pause

step "The host can run hardware-isolated VMs" \
  "KVM turns the CPU's virtualization support into /dev/kvm. cgroup v2 will enforce limits later."
run "grep -m1 -owE 'vmx|svm' /proc/cpuinfo   # vmx = Intel VT-x, svm = AMD-V"
run "ls -l /dev/kvm"
run "stat -fc %T /sys/fs/cgroup"

step "The foundation: three Go programs, checked on every push" \
  "sandboxd (daemon), sandboxctl (CLI), sandbox-agent (runs inside the VM). Same checks run in GitHub CI."
run "make lint test build"
run "./bin/sandboxd --version; ./bin/sandboxctl version; ./bin/sandbox-agent --version"

step "Everything we download is pinned and fingerprint-checked" \
  "Firecracker, the guest kernel and the guest root disk: exact versions, SHA-256 checked."
run "make firecracker"
run "third_party/firecracker/firecracker --version | head -1"
run "make guest"

step "Boot a real microVM by hand" \
  "Four REST calls to Firecracker build the VM. This is exactly what sandboxd will automate."
run "scripts/boot-vm.sh"

step "Every run starts clean" \
  "Run 1 writes a secret. Run 2 is a brand-new VM and cannot find it."
run "scripts/boot-vm.sh -q -- 'echo \"my secret\" > /tmp/note; cat /tmp/note'"
run "scripts/boot-vm.sh -q -- 'cat /tmp/note'"

step "Hostile code stays inside" \
  "No host files are shared in, there is no network card, and the root disk is read-only."
run "scripts/boot-vm.sh -q -- 'ls -A /home /root; ping -c1 -W1 8.8.8.8; rm /bin/sh'"

step "A fork bomb only takes down its own VM" \
  "It fills the guest's process table. The host doesn't notice, and the timeout destroys the VM."
run "scripts/boot-vm.sh -q --timeout 5 -- 'echo \"starting a fork bomb...\"; bomb() { bomb | bomb & }; { bomb; } 2>/dev/null; sleep 60'"
run "uptime; pgrep -a firecracker || echo 'no Firecracker processes left on the host'"

step "The policy decides the VM's size" \
  "Today a flag. From week 5, a YAML policy validated by sandboxd (deny by default)."
run "scripts/boot-vm.sh -q --vcpus 2 --mem 512 -- 'nproc; free -m | head -2'"

step "Demo 1: the daemon runs three clones at once" \
  "Every VM above cold-booted by hand. sandboxd restores clones from a snapshot in about 20 ms; Demo 1 has its own script."
run "./bin/sandboxctl run -- echo hi 2> /dev/null || echo 'no sandboxd running: Demo 1 starts one'"
cat << EOF

  ${Y}make demo1${N}                  three clones at once, private /work disks, ps, logs, the audit chain
  ${Y}scripts/demo1.sh --jailer${N}   the same in jailer mode: only the daemon runs with sudo

EOF

step "What comes next" \
  "Demo 1 works: the daemon restores, runs and destroys clones, and writes an audit record for every run."
cat <<EOF

  ${G}✓ week 0${N}   foundations: plan, ADRs, threat model, Go skeleton, CI, pinned tools
  ${G}✓ week 1${N}   pinned guest kernel, rootless root disk, boot by hand through the API
  ${G}✓ week 2${N}   sandbox-agent inside the VM, gRPC over vsock; the host enforces every limit (make e2e)
  ${G}✓ week 3${N}   snapshots: restore in ~15-20 ms instead of booting; jailer mode (chroot, UID drop,
             cgroups): manual root runs passed on the dev machine, 29-61 ms per restore (not in CI)
  ${G}✓ week 4${N}   ${B}Demo 1${N}: sandboxd + sandboxctl, three clones at once from one snapshot, each with
             its own /work disk; run state machine; hash-chained audit log (make demo1);
             in jailer mode as three jailed UIDs in the root runs of make e2e-jailer
    week 8   ${B}Demo 2${N}: networking off by default, policies change the limits
    week 9   escape-test suite: every attempt blocked or logged
    week 10  benchmarks: p50/p99 restore latency, clones per GB
    week 13  final report and live demo

  Where it ends up:
    ${Y}sandboxctl run --policy policies/default.yaml -- python3 -c 'print("hello from a microVM")'${N}

EOF
echo "${B}Thank you. Questions?${N}"
