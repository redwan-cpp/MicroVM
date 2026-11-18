#!/usr/bin/env bash
# boot-vm.sh: boot one Firecracker microVM, run one job in it, destroy it.
#
# This is what sandboxd will do for every run, written out as plain shell so
# every step is visible. Firecracker is controlled through a small REST API
# on a Unix socket. We send it four requests with curl, and a VM boots.
#
# This is a cold boot (kernel starts from zero). Restoring from a snapshot
# arrives in week 3, the jailer in week 3, networking in week 6.
set -euo pipefail

usage() {
  cat <<'EOF'
usage: scripts/boot-vm.sh [options] [-- COMMAND]

  scripts/boot-vm.sh                        boot, show facts about the guest, stop
  scripts/boot-vm.sh -- 'uname -a; ls /'    run a command inside, then destroy the VM
  scripts/boot-vm.sh --vcpus 2 --mem 512    a bigger VM

options:
  --vcpus N        virtual CPUs (default 1)
  --mem MiB        guest memory in MiB (default 256)
  --timeout SEC    destroy the VM after this many seconds (default 30)
  --kernel-log     show the guest kernel's boot messages
  -q, --quiet      hide the API calls
EOF
}

VCPUS=1
MEM_MIB=256
TIMEOUT=30
KERNEL_LOG=0
SHOW_API=1
CMD=""
while (($#)); do
  case "$1" in
    --vcpus) VCPUS="${2:?--vcpus needs a number}"; shift 2 ;;
    --mem) MEM_MIB="${2:?--mem needs a number}"; shift 2 ;;
    --timeout) TIMEOUT="${2:?--timeout needs a number}"; shift 2 ;;
    --kernel-log) KERNEL_LOG=1; shift ;;
    -q | --quiet) SHOW_API=0; shift ;;
    -h | --help) usage; exit 0 ;;
    --) shift; CMD="$*"; break ;;
    *) echo "unknown option: $1" >&2; usage >&2; exit 2 ;;
  esac
done
for n in "${VCPUS}" "${MEM_MIB}" "${TIMEOUT}"; do
  [[ "${n}" =~ ^[0-9]+$ ]] || { echo "error: '${n}' is not a whole number" >&2; exit 2; }
done

# Colours, only when printing to a terminal.
if [[ -t 1 ]]; then
  B=$'\e[1m' D=$'\e[2m' G=$'\e[32m' C=$'\e[36m' Y=$'\e[33m' R=$'\e[31m' N=$'\e[0m'
else
  B="" D="" G="" C="" Y="" R="" N=""
fi

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
FC="${REPO_ROOT}/third_party/firecracker/firecracker"
KERNEL="${REPO_ROOT}/guest/out/vmlinux"
ROOTFS="${REPO_ROOT}/guest/out/rootfs.ext4"

# --- 0. Preflight: is everything we need here? -----------------------------
[[ -x "${FC}" ]] || { echo "error: no Firecracker. Run: make firecracker" >&2; exit 1; }
[[ -f "${KERNEL}" && -f "${ROOTFS}" ]] || { echo "error: no guest image. Run: make guest" >&2; exit 1; }
[[ -r /dev/kvm && -w /dev/kvm ]] || { echo "error: no access to /dev/kvm (try: sudo usermod -aG kvm \$USER, then log in again)" >&2; exit 1; }

# Every run gets its own private folder: API socket, console pipe, logs.
# mktemp -d picks a fresh random name and creates it with mode 0700 in one
# step, so no other user can create it first or look inside. It lives in
# $XDG_RUNTIME_DIR (a per-user folder in RAM) when there is one, and never
# in a fixed folder that several users share.
RUN="$(mktemp -d "${XDG_RUNTIME_DIR:-${TMPDIR:-/tmp}}/microvm-run.XXXXXX")"
SOCK="${RUN}/firecracker.sock"
FIFO="${RUN}/console"
LOG="${RUN}/firecracker.log"
FC_PID=""

cleanup() {
  local status=$?
  if [[ -n "${FC_PID}" ]] && kill -0 "${FC_PID}" 2>/dev/null; then
    kill "${FC_PID}" 2>/dev/null || true
    wait "${FC_PID}" 2>/dev/null || true
  fi
  if ((status != 0)) && [[ -s "${LOG}" ]]; then
    echo "${R}--- last lines of the Firecracker log ---${N}" >&2
    tail -n 15 "${LOG}" >&2
  fi
  rm -rf "${RUN}"
}
trap cleanup EXIT

# Microseconds since 1970, without starting a process (bash 5).
now_us() { echo "${EPOCHREALTIME/./}"; }

# The guest's serial console arrives here, one line at a time. We indent it,
# and turn the "@@" markers from guest/init/demo-init into timings.
#
# Everything on the console is written by the guest, and the guest runs
# untrusted code. So each line is cleaned before it reaches your terminal:
# control characters (escape sequences could move the cursor, rewrite the
# screen or retitle the window) are removed, tabs become spaces.
console_reader() {
  local line t0 code
  while IFS= read -r line || [[ -n "${line}" ]]; do
    line="${line//$'\t'/ }"
    line="${line//[[:cntrl:]]/}"
    case "${line}" in
      *@@guest-init-started@@*)
        t0="$(<"${RUN}/t0")"
        echo $((($(now_us) - t0) / 1000)) > "${RUN}/boot-ms"
        printf '%s  ⏱  the guest is running its first program, %s ms after InstanceStart%s\n' \
          "${G}" "$(<"${RUN}/boot-ms")" "${N}"
        ;;
      *@@exit=*@@*)
        # Only a plain number of 1 to 3 digits counts as an exit code. The
        # guest can print anything, so anything else is ignored.
        code="${line#*@@exit=}"
        code="${code%%@@*}"
        [[ "${code}" =~ ^[0-9]{1,3}$ ]] && echo "${code}" > "${RUN}/exit-code"
        ;;
      *"] reboot: "*) # the kernel's own goodbye; we report the end ourselves
        ((KERNEL_LOG)) && printf '  %s│%s %s\n' "${C}" "${N}" "${line}"
        ;;
      *) printf '  %s│%s %s\n' "${C}" "${N}" "${line}" ;;
    esac
  done
}

# One request to Firecracker's API. Shows it, and stops on any error.
# --max-time: a Firecracker that stops answering must not hang the script.
api() {
  local method="$1" path="$2" body="$3" code
  ((SHOW_API)) && printf '  %s→ %s %s%s %s%s%s\n' "${B}" "${method}" "${path}" "${N}" "${D}" "${body}" "${N}"
  code="$(curl -sS --max-time 5 --unix-socket "${SOCK}" -X "${method}" "http://localhost${path}" \
    -H 'Content-Type: application/json' -d "${body}" -o "${RUN}/api-response" -w '%{http_code}')"
  if [[ "${code}" != 2* ]]; then
    echo "${R}  ← ${code}: $(cat "${RUN}/api-response")${N}" >&2
    exit 1
  fi
  ((SHOW_API)) && printf '  %s← %s OK%s\n' "${D}" "${code}" "${N}"
  return 0
}

# --- 1. Start an empty Firecracker process ----------------------------------
# It has no VM yet: it only opens the API socket and waits for instructions.
mkfifo "${FIFO}"
console_reader < "${FIFO}" &
READER_PID=$!
touch "${LOG}"
"${FC}" --api-sock "${SOCK}" --log-path "${LOG}" --level Info < /dev/null > "${FIFO}" 2>> "${LOG}" &
FC_PID=$!
for _ in $(seq 100); do [[ -S "${SOCK}" ]] && break; sleep 0.02; done
[[ -S "${SOCK}" ]] || { echo "error: Firecracker did not open its API socket" >&2; exit 1; }
echo "${Y}▶ Firecracker is running (pid ${FC_PID}). Building the VM through its API:${N}"

# --- 2. Describe the machine ------------------------------------------------
# Kernel command line: console on the serial port, reboot = exit, and our
# init as PID 1. Firecracker adds "root=/dev/vda ro" for the root drive.
# The i8042 flags skip probing for a mouse and other keyboard-controller
# extras that a microVM doesn't have; the probe alone cost about 0.5 s. We
# keep the controller itself: "reboot=k" uses it to power off.
BOOT_ARGS="console=ttyS0 reboot=k panic=1 pci=off nomodule init=/sbin/demo-init"
BOOT_ARGS+=" i8042.noaux i8042.nomux i8042.nopnp i8042.dumbkbd"
((KERNEL_LOG)) || BOOT_ARGS+=" quiet loglevel=1"
if [[ -n "${CMD}" ]]; then
  # The command travels on the kernel command line, which splits on spaces,
  # so it goes base64-encoded. From week 2 sandbox-agent receives it over vsock.
  CMD_B64="$(printf '%s' "${CMD}" | base64 -w0)"
  ((${#CMD_B64} < 1500)) || { echo "error: command too long for the kernel command line" >&2; exit 2; }
  BOOT_ARGS+=" sandbox_cmd_b64=${CMD_B64}"
fi

api PUT /machine-config "{\"vcpu_count\": ${VCPUS}, \"mem_size_mib\": ${MEM_MIB}}"
api PUT /boot-source "{\"kernel_image_path\": \"${KERNEL}\", \"boot_args\": \"${BOOT_ARGS}\"}"
# The root disk is shared and read-only: no run can change it for the next.
api PUT /drives/rootfs "{\"drive_id\": \"rootfs\", \"path_on_host\": \"${ROOTFS}\", \"is_root_device\": true, \"is_read_only\": true}"

# --- 3. Power on ------------------------------------------------------------
if [[ -n "${CMD}" ]]; then
  echo "${Y}▶ Starting the VM to run:${N} ${B}${CMD}${N}"
fi
now_us > "${RUN}/t0"
api PUT /actions '{"action_type": "InstanceStart"}'

# --- 4. Wait until the guest powers itself off (or the timeout hits) ---------
TIMED_OUT=0
deadline_us=$(($(<"${RUN}/t0") + TIMEOUT * 1000000))
while kill -0 "${FC_PID}" 2>/dev/null; do
  if (($(now_us) >= deadline_us)); then
    TIMED_OUT=1
    echo "${R}  ✗ timeout after ${TIMEOUT} s: destroying the VM${N}"
    kill "${FC_PID}" 2>/dev/null || true
    break
  fi
  sleep 0.01
done
wait "${FC_PID}" 2>/dev/null || true
END_US="$(now_us)"
FC_PID=""
wait "${READER_PID}" 2>/dev/null || true

# --- 5. Report, then delete everything this run created ---------------------
LIFETIME_MS=$(((END_US - $(<"${RUN}/t0")) / 1000))
EXIT_CODE="$(cat "${RUN}/exit-code" 2>/dev/null || echo "?")"
((TIMED_OUT)) && EXIT_CODE=124
echo "${Y}▶ The VM is gone.${N} It lived ${B}${LIFETIME_MS} ms${N} from power-on to destruction; exit code ${B}${EXIT_CODE}${N}."
rm -rf "${RUN}"
[[ -e "${RUN}" ]] || echo "  ${D}Its run folder is deleted too: nothing from this run is left on the host.${N}"

[[ "${EXIT_CODE}" =~ ^[0-9]+$ ]] || exit 1
exit "${EXIT_CODE}"
