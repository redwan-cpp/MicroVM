#!/usr/bin/env bash
# demo-ui.sh: the demo page (sandboxctl ui), for showing the project in a
# browser. It builds everything as you, starts sandboxd, then serves the
# page on this machine only and prints the address to open. Ctrl-C stops
# the page, then the daemon (every clone destroyed).
#
# Run it with `make demo-ui` (arguments: make demo-ui ARGS='--record s.jsonl').
#
#   scripts/demo-ui.sh                   rootless mode (the fallback)
#   scripts/demo-ui.sh --jailer          jailer mode: chroot, own UID and cgroup per clone. Only the
#                                        prebuilt daemon (bin/sandboxd serve --jailer) runs as root,
#                                        under sudo; the page and everything else run as you
#   scripts/demo-ui.sh --record FILE     also record the session into FILE (a new file); afterwards
#                                        sandboxctl ui export --from FILE writes one HTML file of it
#   scripts/demo-ui.sh --listen ADDR     another loopback address (default 127.0.0.1:8090; port 0
#                                        picks a free one, and the page's address says which)
#
# The state folder is $MICROVM_STATE_DIR (default guest/out/state). In rootless
# mode $SANDBOXD_SOCKET picks another socket than the default one.
# Needs: make firecracker, make guest (they download; run them before the talk).
set -euo pipefail

JAILER=0
RECORD=""
LISTEN="127.0.0.1:8090"
while (($#)); do
  case "$1" in
    --jailer) JAILER=1 ;;
    --record)
      RECORD="${2:?--record needs a file name}"
      shift
      ;;
    --listen)
      LISTEN="${2:?--listen needs an address}"
      shift
      ;;
    *)
      echo "usage: $0 [--jailer] [--record FILE] [--listen 127.0.0.1:8090]" >&2
      exit 2
      ;;
  esac
  shift
done

if [[ -t 1 ]]; then
  B=$'\e[1m' D=$'\e[2m' G=$'\e[32m' R=$'\e[31m' Y=$'\e[33m' N=$'\e[0m'
else
  B="" D="" G="" R="" Y="" N=""
fi
die() {
  echo "${R}demo-ui: $*${N}" >&2
  exit 1
}

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${REPO_ROOT}"
STATE="${MICROVM_STATE_DIR:-guest/out/state}"

# --- Preflight: fail now, not in front of the audience ----------------------
missing=()
command -v go > /dev/null || missing+=("Go (add ~/.local/go/bin to PATH)")
[[ -x third_party/firecracker/firecracker ]] || missing+=("Firecracker (make firecracker)")
[[ -f guest/out/vmlinux ]] || missing+=("guest kernel (make guest)")
if ((${#missing[@]})); then
  printf 'not ready for the demo page, missing:\n' >&2
  printf '  - %s\n' "${missing[@]}" >&2
  exit 1
fi
((EUID != 0)) || die "run this as yourself, not as root: it builds everything as you, and with --jailer it runs only the daemon with sudo"
[[ -z "${RECORD}" || ! -e "${RECORD}" ]] || die "${RECORD} exists already: a recording never overwrites a file; choose another name"

# Where the daemon listens (ADR-0006), and how it is started. The daemon
# may run 8 clones at once, the most one scenario of the page asks for.
if ((JAILER)); then
  SOCKET="/run/sandboxd/sandboxd.sock"
  MODE="jailer"
  DAEMON_CMD="sudo bin/sandboxd serve --jailer --state-dir ${STATE} --socket-group $(id -g) --max-concurrent 8"
else
  [[ -n "${XDG_RUNTIME_DIR:-}" || -n "${SANDBOXD_SOCKET:-}" ]] ||
    die "XDG_RUNTIME_DIR is not set; log in normally, or set SANDBOXD_SOCKET to a socket path of yours"
  SOCKET="${SANDBOXD_SOCKET:-${XDG_RUNTIME_DIR}/sandboxd/sandboxd.sock}"
  MODE="rootless"
  # --socket only for a socket of your own choice: the default one's folder
  # is the daemon's to create and check (ADR-0006).
  SOCKET_ARGS=()
  [[ -z "${SANDBOXD_SOCKET:-}" ]] || SOCKET_ARGS=(--socket "${SOCKET}")
  DAEMON_CMD="bin/sandboxd serve --state-dir ${STATE}${SANDBOXD_SOCKET:+ --socket ${SOCKET}} --max-concurrent 8"
fi
export SANDBOXD_SOCKET="${SOCKET}"

echo "${B}MicroVM Sandbox: the demo page${N} (${MODE} mode)"
echo "${D}Research and teaching project, not security audited (see SECURITY.md).${N}"
echo

# --- Build as yourself ------------------------------------------------------
echo "${Y}\$ make build snapshots${N}   ${D}(as you: the programs, the guest image, one snapshot per size class)${N}"
make --no-print-directory build snapshots

if [[ -S "${SOCKET}" ]] && bin/sandboxctl ps > /dev/null 2>&1; then
  die "a sandboxd already listens on ${SOCKET}; stop it first"
fi

# In jailer mode, say exactly what runs as root, then make sure sudo will
# not stop to ask for a password halfway through.
if ((JAILER)); then
  echo
  echo "${B}Jailer mode${N}: the daemon needs root (the jailer creates chroots, device files and cgroups)."
  echo "This is the only command that runs as root, a binary built by you:"
  echo
  echo "    ${Y}${DAEMON_CMD}${N}"
  echo
  echo "The page itself (sandboxctl ui) runs as you and talks to the daemon over its socket."
  if ! sudo -n true 2> /dev/null; then
    if [[ -t 0 ]]; then
      sudo -v || die "sudo failed"
    else
      echo "sudo needs your password and there is no terminal to ask on. Run it from a terminal:" >&2
      echo "    scripts/demo-ui.sh --jailer" >&2
      exit 1
    fi
  fi
fi

# --- The daemon, in the background -------------------------------------------
LOG="$(mktemp "${TMPDIR:-/tmp}/sandboxd-demo-ui.XXXXXX.log")"
# As in demo1.sh: DAEMON_PID is sandboxd itself in rootless mode, sudo in
# jailer mode (root's: we may not signal it, but can see it in /proc; it
# passes the signals it gets on to sandboxd).
DAEMON_PID=""
daemon_alive() {
  [[ -n "${DAEMON_PID}" ]] || return 1
  if ((JAILER)); then
    [[ -e "/proc/${DAEMON_PID}" ]]
  else
    kill -0 "${DAEMON_PID}" 2> /dev/null
  fi
}
daemon_answers() {
  bin/sandboxctl --socket "${SOCKET}" ps > /dev/null 2>&1
}
stop_daemon() {
  [[ -n "${DAEMON_PID}" ]] || return 0
  if daemon_alive; then
    if ((JAILER)); then
      if [[ -t 0 ]]; then sudo -v || true; fi
      if ! sudo -n kill -TERM "${DAEMON_PID}" 2> /dev/null; then
        echo "${R}demo-ui: could not stop the daemon; stop it yourself with:${N}" >&2
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
    echo "${R}demo-ui: the daemon (pid ${DAEMON_PID}) did not stop within 15 s; stop it with:${N}" >&2
    if ((JAILER)); then echo "    sudo kill -KILL ${DAEMON_PID}" >&2; else echo "    kill -KILL ${DAEMON_PID}" >&2; fi
    DAEMON_PID=""
    return 1
  fi
  wait "${DAEMON_PID}" 2> /dev/null || true
  DAEMON_PID=""
  echo "${G}demo-ui: the daemon stopped; every clone destroyed.${N}"
}
trap 'stop_daemon || true; rm -f "${LOG}"' EXIT

echo
echo "${Y}\$ ${DAEMON_CMD} &${N}"
if ((JAILER)); then
  sudo -n bin/sandboxd serve --jailer --state-dir "${STATE}" --socket-group "$(id -g)" --max-concurrent 8 > "${LOG}" 2>&1 &
else
  bin/sandboxd serve --state-dir "${STATE}" "${SOCKET_ARGS[@]}" --max-concurrent 8 > "${LOG}" 2>&1 &
fi
DAEMON_PID=$!
for _ in $(seq 600); do
  daemon_answers && break
  daemon_alive || break
  sleep 0.1
done
daemon_answers || {
  cat "${LOG}" >&2
  die "the daemon did not start (its log is above)"
}
sed 's/^/   /' "${LOG}"

# --- The page, in the foreground: Ctrl-C ends it ------------------------------
args=(ui --listen "${LISTEN}" --state-dir "${STATE}")
[[ -z "${RECORD}" ]] || args+=(--record "${RECORD}")
echo
echo "${Y}\$ bin/sandboxctl ${args[*]}${N}"
status=0
bin/sandboxctl --socket "${SOCKET}" "${args[@]}" || status=$?
if [[ -n "${RECORD}" && -f "${RECORD}" ]]; then
  echo
  echo "The recording is ${RECORD}. Play it back, or make one HTML file of it:"
  echo "    bin/sandboxctl ui --replay ${RECORD}"
  echo "    bin/sandboxctl ui export --from ${RECORD}"
fi
exit "${status}"
