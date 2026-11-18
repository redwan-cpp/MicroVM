#!/usr/bin/env bash
# fetch-kernel.sh: download the pinned guest kernel and check its fingerprint.
# Run it with `make guest`, which also builds the root disk.
#
# A microVM needs its own Linux kernel: that is the whole point. The guest
# never uses the host's kernel, so a bug in the guest kernel stays inside the
# VM. We use the small kernel the Firecracker team builds for their own CI.
# It is configured for Firecracker's few virtual devices and nothing else.
set -euo pipefail

KERNEL_VERSION="6.1.155"
# The Firecracker CI folder the kernel comes from. Kernels from older CI
# folders boot fine on newer Firecracker releases.
CI_VERSION="v1.15"
ARCH="x86_64"
URL="https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/${CI_VERSION}/${ARCH}/vmlinux-${KERNEL_VERSION}"

# SHA-256 of that file. Firecracker's CI bucket publishes no checksum list,
# so we recorded this fingerprint the first time we downloaded the file
# (2026-11-18). It catches any later change to the file, but it cannot prove
# the file was right on that first day ("trust on first use").
EXPECTED_SHA256="e20e46d0c36c55c0d1014eb20576171b3f3d922260d9f792017aeff53af3d4f2"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT="${REPO_ROOT}/guest/out"
DEST="${OUT}/vmlinux"

# Already there and still the right file? Then there is nothing to do.
if [[ -f "${DEST}" ]] && echo "${EXPECTED_SHA256}  ${DEST}" | sha256sum --check --status --strict -; then
  echo "guest kernel ${KERNEL_VERSION} is already in ${DEST}"
  exit 0
fi

WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT

echo "==> Downloading ${URL}"
curl -fL --proto '=https' --tlsv1.2 -o "${WORK}/vmlinux" "${URL}"

echo "==> Checking the SHA-256 fingerprint"
echo "${EXPECTED_SHA256}  ${WORK}/vmlinux" | sha256sum --check --strict -

mkdir -p "${OUT}"
install -m 0644 "${WORK}/vmlinux" "${DEST}"
echo "${KERNEL_VERSION}" > "${OUT}/vmlinux.VERSION"
echo "==> Installed guest kernel ${KERNEL_VERSION} into ${DEST}"
