#!/usr/bin/env bash
# fetch-firecracker.sh: download the pinned Firecracker + jailer release,
# check that it's exactly the file we expect, and put the two programs in
# third_party/firecracker/. Run it with `make firecracker`.
#
# Why a checksum? A SHA-256 checksum is a fingerprint of a file. If even one
# byte changes (a broken download, or someone tampering with the file), the
# fingerprint changes completely. We wrote the expected fingerprint below
# when we chose this version, so any other file is rejected.
set -euo pipefail

VERSION="v1.17.0"
ARCH="x86_64"
# SHA-256 of firecracker-${VERSION}-${ARCH}.tgz, copied from the official
# release page when we pinned this version (2026-11-04).
EXPECTED_SHA256="06094a1108ae9e82aa4c23a775aa92758f53f1175d422270d9d6162cb9ade558"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DEST="${REPO_ROOT}/third_party/firecracker"
TARBALL="firecracker-${VERSION}-${ARCH}.tgz"
URL="https://github.com/firecracker-microvm/firecracker/releases/download/${VERSION}/${TARBALL}"

# Already installed at this version? Then there's nothing to do.
if [[ -x "${DEST}/firecracker" ]] && [[ "$(cat "${DEST}/VERSION" 2>/dev/null)" == "${VERSION}" ]]; then
  echo "firecracker ${VERSION} is already installed in ${DEST}"
  exit 0
fi

WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT # always clean up the temporary folder

echo "==> Downloading ${URL}"
curl -fL --proto '=https' --tlsv1.2 -o "${WORK}/${TARBALL}" "${URL}"

echo "==> Checking the SHA-256 fingerprint"
echo "${EXPECTED_SHA256}  ${WORK}/${TARBALL}" | sha256sum --check --strict -

echo "==> Unpacking"
tar -xzf "${WORK}/${TARBALL}" -C "${WORK}"
RELEASE_DIR="${WORK}/release-${VERSION}-${ARCH}"

# The archive also carries its own fingerprint list (SHA256SUMS) for every
# file inside. Check the three files we keep against it too. Lines in that
# list look like "<hash>  ./firecracker-v1.17.0-x86_64".
KEEP_PATTERN="\./((firecracker|jailer)-${VERSION}-${ARCH}|firecracker_spec-${VERSION}\.yaml)$"
(cd "${RELEASE_DIR}" && grep -E "  ${KEEP_PATTERN}" SHA256SUMS | sha256sum --check --strict -)
if [[ "$(grep -cE "  ${KEEP_PATTERN}" "${RELEASE_DIR}/SHA256SUMS")" -ne 3 ]]; then
  echo "error: expected 3 entries in SHA256SUMS for the files we keep" >&2
  exit 1
fi

mkdir -p "${DEST}"
install -m 0755 "${RELEASE_DIR}/firecracker-${VERSION}-${ARCH}" "${DEST}/firecracker"
install -m 0755 "${RELEASE_DIR}/jailer-${VERSION}-${ARCH}" "${DEST}/jailer"
# The official description of Firecracker's API: every request it accepts.
install -m 0644 "${RELEASE_DIR}/firecracker_spec-${VERSION}.yaml" "${DEST}/firecracker_spec.yaml"
echo "${VERSION}" > "${DEST}/VERSION"

echo "==> Installed into ${DEST}:"
"${DEST}/firecracker" --version | head -1
"${DEST}/jailer" --version | head -1
