#!/usr/bin/env bash
# build-rootfs.sh: build the guest's root disk image, guest/out/rootfs.ext4.
# Run it with `make guest`, which also fetches the kernel.
#
# The root disk is a file on the host that holds an ext4 filesystem. Firecracker
# shows it to the guest as /dev/vda. We fill it with Alpine Linux's
# "minirootfs": a 3.7 MB tarball with a shell and basic tools, and nothing
# that starts on its own. On top go our init script and sandbox-agent
# (bin/sandbox-agent from `make build`; `make guest` builds it first), and
# the empty folder /work, where each clone's scratch disk is mounted.
#
# Rootless: no sudo. Files in a Linux root filesystem must belong to root
# (uid 0). We get that without being root by running the build inside a
# *user namespace* (`unshare --map-root-user --map-auto`), where our own user
# shows up as uid 0. mmdebstrap's unshare mode uses the same trick.
set -euo pipefail
export PATH="${PATH}:/usr/sbin:/sbin" # mkfs.ext4 lives in sbin

ALPINE_VERSION="3.24.4"
ALPINE_BRANCH="v3.24"
ARCH="x86_64"
TARBALL="alpine-minirootfs-${ALPINE_VERSION}-${ARCH}.tar.gz"
URL="https://dl-cdn.alpinelinux.org/alpine/${ALPINE_BRANCH}/releases/${ARCH}/${TARBALL}"
# SHA-256 from Alpine's own release list (latest-releases.yaml and the
# .sha256 file next to the tarball), checked when we pinned it (2026-11-18).
EXPECTED_SHA256="cdaa38cb8de3537967ee5b6c9b6cd00cc962da2b018e6eea48696bb274923012"
# Alpine takes about 8 MB and sandbox-agent about 11 MB; the rest is headroom.
IMAGE_SIZE="64M"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT="${REPO_ROOT}/guest/out"
CACHE="${OUT}/cache"
INIT="${REPO_ROOT}/guest/init/demo-init"
AGENT="${REPO_ROOT}/bin/sandbox-agent"
DEST="${OUT}/rootfs.ext4"

[[ -x "${AGENT}" ]] || { echo "error: no ${AGENT}. Run: make build (or make guest)" >&2; exit 1; }
mkdir -p "${CACHE}"

# 1. Download the tarball once, and check it every time.
if [[ ! -f "${CACHE}/${TARBALL}" ]]; then
  echo "==> Downloading ${URL}"
  curl -fL --proto '=https' --tlsv1.2 -o "${CACHE}/${TARBALL}.part" "${URL}"
  mv "${CACHE}/${TARBALL}.part" "${CACHE}/${TARBALL}"
fi
echo "==> Checking the SHA-256 fingerprint"
echo "${EXPECTED_SHA256}  ${CACHE}/${TARBALL}" | sha256sum --check --strict -

# 2. Unpack it, add our init and the agent, and pack it into an ext4 image,
#    all inside the user namespace. `mkfs.ext4 -d DIR` copies DIR into the
#    new filesystem, so no mounting (and no root) is needed.
STAGE="$(mktemp -d "${OUT}/rootfs-stage.XXXXXX")"
cleanup() {
  # Files in STAGE belong to "root" in the namespace, so delete them there too.
  unshare --map-root-user --map-auto rm -rf "${STAGE}" 2>/dev/null || rm -rf "${STAGE}"
  rm -f "${DEST}.tmp"
}
trap cleanup EXIT

echo "==> Building ${DEST} (Alpine ${ALPINE_VERSION}, ${IMAGE_SIZE})"
unshare --map-root-user --map-auto -- bash -euo pipefail -c '
  tarball="$1" stage="$2" init="$3" agent="$4" image="$5" size="$6"
  tar -xzf "${tarball}" -C "${stage}" --numeric-owner
  install -m 0755 -o 0 -g 0 "${init}" "${stage}/sbin/demo-init"
  install -D -m 0755 -o 0 -g 0 "${agent}" "${stage}/usr/local/bin/sandbox-agent"
  # The mount point of each clone'"'"'s own scratch disk (the agent mounts it;
  # the root disk itself is read-only).
  install -d -m 0755 -o 0 -g 0 "${stage}/work"
  echo microvm > "${stage}/etc/hostname"
  mkfs.ext4 -q -F -L rootfs -d "${stage}" "${image}" "${size}"
' _ "${CACHE}/${TARBALL}" "${STAGE}" "${INIT}" "${AGENT}" "${DEST}.tmp" "${IMAGE_SIZE}"

# -f: the snapshot store hard-links the old disk and makes it 0444, and mv
# would otherwise stop to ask before replacing a read-only file.
mv -f "${DEST}.tmp" "${DEST}"
echo "${ALPINE_VERSION}" > "${OUT}/rootfs.VERSION"

# How much room is left on the disk? (dumpe2fs reads the image file; no
# root needed.) The root disk is read-only in the VM, so this is only
# room for future additions to the image.
free_mib="$(dumpe2fs -h "${DEST}" 2>/dev/null | awk -F: '
  /^Free blocks:/ {free = $2} /^Block size:/ {size = $2}
  END {printf "%d", free * size / 1048576}')"
echo "==> Built ${DEST} (${IMAGE_SIZE}, ${free_mib} MiB free)"
