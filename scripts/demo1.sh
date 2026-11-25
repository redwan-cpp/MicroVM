#!/usr/bin/env bash
# demo1.sh: Demo 1, live. Three clones run at once from one snapshot, each
# with its own writes, and everything is cleaned up afterwards.
#
# Run it with `make demo1`. Before each step it prints what the step shows and
# waits for Enter; every command is printed before it runs.
#
#   scripts/demo1.sh               rootless mode (the fallback), pause before every step
#   scripts/demo1.sh --no-pause    run straight through (rehearsal; exits non-zero if a check fails)
#   scripts/demo1.sh --jailer      jailer mode: chroot, own UID and cgroup per clone. Only the
#                                  daemon (bin/sandboxd serve --jailer) runs as root, under sudo;
#                                  everything is built as you. sudo is also used to stop it
#                                  (kill -TERM) and to look into the root-only jail/firecracker/
#
# Needs: make firecracker, make guest (they download; run them before the demo).
set -euo pipefail

PAUSE=1
JAILER=0
for arg in "$@"; do
  case "${arg}" in
    --no-pause) PAUSE=0 ;;
    --jailer) JAILER=1 ;;
    *)
      echo "usage: $0 [--no-pause] [--jailer]" >&2
      exit 2
      ;;
  esac
done

if [[ -t 1 ]]; then
  B=$'\e[1m' D=$'\e[2m' G=$'\e[32m' R=$'\e[31m' Y=$'\e[33m' M=$'\e[35m' N=$'\e[0m'
else
  B="" D="" G="" R="" Y="" M="" N=""
fi

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${REPO_ROOT}"
STATE="${MICROVM_STATE_DIR:-guest/out/state}"

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
show() { # show 'command line': print it, then run it; stop the demo if it fails
  echo "${Y}\$ $1${N}"
  bash -c "$1"
}
die() {
  echo "${R}demo1: $*${N}" >&2
  exit 1
}

# --- Preflight: fail now, not halfway through the talk ----------------------
missing=()
command -v go > /dev/null || missing+=("Go (add ~/.local/go/bin to PATH)")
[[ -x third_party/firecracker/firecracker ]] || missing+=("Firecracker (make firecracker)")
[[ -f guest/out/vmlinux && -f guest/out/rootfs.ext4 ]] || missing+=("guest image (make guest)")
if ((${#missing[@]})); then
  printf 'not ready for Demo 1, missing:\n' >&2
  printf '  - %s\n' "${missing[@]}" >&2
  exit 1
fi
if ((EUID == 0)); then
  die "run this as yourself, not as root: it builds everything as you, and with --jailer it runs only the daemon with sudo"
fi

# Where the daemon listens (ADR-0006), and how it is started.
if ((JAILER)); then
  SOCKET="/run/sandboxd/sandboxd.sock"
  MODE="jailer"
  DAEMON_CMD="sudo bin/sandboxd serve --jailer --state-dir ${STATE} --socket-group $(id -g)"
else
  [[ -n "${XDG_RUNTIME_DIR:-}" ]] || die "XDG_RUNTIME_DIR is not set; log in normally, or set it to a private folder of yours"
  SOCKET="${XDG_RUNTIME_DIR}/sandboxd/sandboxd.sock"
  MODE="rootless"
  DAEMON_CMD="bin/sandboxd serve --state-dir ${STATE}"
fi
export SANDBOXD_SOCKET="${SOCKET}"
if [[ -S "${SOCKET}" ]] && bin/sandboxctl ps > /dev/null 2>&1; then
  die "a sandboxd already listens on ${SOCKET}; stop it first"
fi

# In jailer mode, ask for the sudo password now, before the talk starts. The
# daemon is the only program that runs as root; nothing is built with sudo.
if ((JAILER)); then
  echo "${B}Jailer mode${N}: the daemon needs root (the jailer creates chroots, device files and cgroups)."
  echo "This is the only command that runs as root, a binary built by you:"
  echo
  echo "    ${Y}${DAEMON_CMD}${N}"
  echo
  if ! sudo -n true 2> /dev/null; then
    if [[ -t 0 ]]; then
      sudo -v || die "sudo failed"
    else
      echo "sudo needs your password and there is no terminal to ask on. Run the demo from a terminal:" >&2
      echo "    scripts/demo1.sh --jailer" >&2
      exit 1
    fi
  fi
fi

LOG="$(mktemp "${TMPDIR:-/tmp}/sandboxd-demo1.XXXXXX.log")"
# DAEMON_PID is the background job that runs the daemon: sandboxd itself in
# rootless mode, sudo in jailer mode. sudo runs as root, so we may not
# signal it (kill -0 says EPERM) but can see it in /proc; it stays until
# sandboxd exits, and passes the signals it gets on to sandboxd.
DAEMON_PID=""
daemon_alive() {
  [[ -n "${DAEMON_PID}" ]] || return 1
  if ((JAILER)); then
    [[ -e "/proc/${DAEMON_PID}" ]]
  else
    kill -0 "${DAEMON_PID}" 2> /dev/null
  fi
}
# daemon_answers: the daemon accepts connections and answers. A socket
# file alone proves nothing: a crashed daemon leaves a stale one.
daemon_answers() {
  bin/sandboxctl --socket "${SOCKET}" ps > /dev/null 2>&1
}
stop_daemon() {
  [[ -n "${DAEMON_PID}" ]] || return 0
  if daemon_alive; then
    # The daemon stops on SIGTERM: it destroys every clone first.
    if ((JAILER)); then
      # sandboxd is root's: the signal goes through sudo (refresh the
      # login first, so a long talk does not make it ask mid-way).
      if [[ -t 0 ]]; then sudo -v || true; fi
      if ! sudo -n kill -TERM "${DAEMON_PID}" 2> /dev/null; then
        echo "${R}demo1: could not stop the daemon; stop it yourself with:${N}" >&2
        echo "    sudo kill -TERM ${DAEMON_PID}" >&2
        DAEMON_PID=""
        return 1
      fi
    else
      kill -TERM "${DAEMON_PID}" 2> /dev/null || true
    fi
    # It takes at most 10 s (daemon.ShutdownTimeout); allow a little more.
    for _ in $(seq 150); do
      daemon_alive || break
      sleep 0.1
    done
  fi
  if daemon_alive; then
    echo "${R}demo1: the daemon (pid ${DAEMON_PID}) did not stop within 15 s; stop it with:${N}" >&2
    if ((JAILER)); then echo "    sudo kill -KILL ${DAEMON_PID}" >&2; else echo "    kill -KILL ${DAEMON_PID}" >&2; fi
    DAEMON_PID=""
    return 1
  fi
  wait "${DAEMON_PID}" 2> /dev/null || true # it has exited: this only collects it
  DAEMON_PID=""
}
trap 'stop_daemon || true; rm -f "${LOG}"' EXIT

clear 2> /dev/null || true
cat << EOF
${B}MicroVM Sandbox: Demo 1${N}
Three clones at once from one snapshot, each with its own writes,
and nothing left behind afterwards.

EOF
if ((JAILER)); then
  echo "${G}${B}Mode: jailer${N}: every clone's Firecracker in its own chroot, as its own UID, in its own cgroup."
else
  echo "${Y}${B}Mode: rootless mode (fallback)${N}: Firecracker runs as you, without the jailer's chroot,"
  echo "UID drop and cgroups. Fine for a demo, not for hostile code (ADR-0005). For the real thing: --jailer."
fi
echo "${D}Research and teaching project, not security audited (see SECURITY.md).${N}"
pause

step "Build everything as yourself" \
  "The three programs, then one base snapshot per size class: a booted VM with its agent, saved to disk."
show "make build"
show "make snapshots"

step "Start the daemon (${MODE} mode)" \
  "sandboxd opens the snapshot store once (every hash checked), verifies the audit log, locks the state folder, and listens on a Unix socket."
echo "${Y}\$ ${DAEMON_CMD} &${N}"
if ((JAILER)); then
  sudo -n bin/sandboxd serve --jailer --state-dir "${STATE}" --socket-group "$(id -g)" > "${LOG}" 2>&1 &
else
  bin/sandboxd serve --state-dir "${STATE}" > "${LOG}" 2>&1 &
fi
DAEMON_PID=$!
# Wait until it answers: opening the store hashes every snapshot file
# first, which takes a few seconds on a slow disk.
for _ in $(seq 600); do
  daemon_answers && break
  daemon_alive || break
  sleep 0.1
done
daemon_answers || { cat "${LOG}" >&2; die "the daemon did not start (its log is above)"; }
sed 's/^/   /' "${LOG}"
show "ls -l ${SOCKET}"

step "Three clones at once" \
  "One command, three times at once. Each clone writes its own secret to /work, its own scratch disk, then lists /work."
cat << 'EOF'
  each clone runs:  sh -c 'echo "secret of clone $N" > /work/secret-$N; ls /work; sleep 1; cat /work/secret-*'
EOF
pids=()
for n in 1 2 3; do
  echo "${Y}\$ bin/sandboxctl run --env N=${n} -- sh -c '...' &${N}"
  # A subshell, so that its exit status is sandboxctl's (pipefail), not sed's.
  (
    bin/sandboxctl run --env "N=${n}" -- sh -c \
      'echo "secret of clone $N" > /work/secret-$N; ls /work; sleep 1; cat /work/secret-*' 2>&1 |
      sed "s/^/   [clone ${n}] /"
  ) &
  pids+=($!)
done
failed=0
for pid in "${pids[@]}"; do
  wait "${pid}" || failed=1
done
((failed == 0)) || die "a clone failed"
echo "${G}   ✓ each clone sees only its own secret: three private scratch disks, one shared snapshot${N}"

step "A new run starts clean" \
  "A fourth clone, after the others are gone: its /work holds nothing of theirs (only ext4's lost+found)."
show "bin/sandboxctl run -- ls -A /work"

step "The daemon's view: sandboxctl ps" \
  "Every run with its state, class, mode, the caller's UID (from the socket, not from the request) and how it ended."
show "bin/sandboxctl ps --all"
FIRST="$(bin/sandboxctl ps --all | awk 'NR == 2 {print $1}')"
[[ -n "${FIRST}" ]] || die "no runs listed"
show "bin/sandboxctl ps ${FIRST}"

step "The output it kept: sandboxctl logs" \
  "The daemon keeps the first 1 MiB of each run's output in memory, for the last 100 runs."
show "bin/sandboxctl logs ${FIRST}"

step "The audit log: one hash-chained record per run" \
  "Each record holds the SHA-256 of the one before. Change, remove or reorder one, and verify points at it."
show "bin/sandboxctl audit verify --state-dir ${STATE}"
show "tail -n 1 ${STATE}/audit/audit.jsonl | cut -c 1-300; echo ..."

step "Stop the daemon" \
  "SIGTERM: no new runs, running ones are stopped, every clone destroyed, the audit log closed, the socket removed."
if ((JAILER)); then
  echo "${Y}\$ sudo kill -TERM ${DAEMON_PID}   # sudo passes it on to sandboxd${N}"
else
  echo "${Y}\$ kill -TERM ${DAEMON_PID}${N}"
fi
stop_daemon || die "the daemon did not stop"
sed 's/^/   /' "${LOG}" | tail -n 3

step "Nothing left behind" \
  "No Firecracker process, no run folder, no jail, no cgroup, no socket."
left=0
show "pgrep -a firecracker || echo 'no Firecracker processes'"
pgrep -x firecracker > /dev/null && left=1
show "ls -A ${STATE}/runs; echo '(runs/ is empty)'"
[[ -z "$(ls -A "${STATE}/runs")" ]] || left=1
if ((JAILER)); then
  # The jailer keeps the shared parent folder jail/firecracker/ (root's,
  # mode 0700, so only root can look inside); each clone's own jail in it
  # must be gone.
  show "sudo -n find ${STATE}/jail -mindepth 2 -maxdepth 2; echo '(no jails left)'"
  jails="$(sudo -n find "${STATE}/jail" -mindepth 2 -maxdepth 2 2> /dev/null)" || jails="(could not look: sudo)"
  [[ -z "${jails}" ]] || left=1
  show "ls /sys/fs/cgroup/microvm 2> /dev/null || echo 'no microvm cgroups'"
  [[ -d /sys/fs/cgroup/microvm && -n "$(find /sys/fs/cgroup/microvm -mindepth 1 -maxdepth 1 -type d)" ]] && left=1
fi
show "ls -l ${SOCKET} 2> /dev/null || echo 'no socket'"
[[ -e "${SOCKET}" ]] && left=1
((left == 0)) || die "something was left behind (see above)"

echo
echo "${G}${B}Demo 1 done (${MODE} mode): three clones at once, private writes, nothing left behind.${N}"
