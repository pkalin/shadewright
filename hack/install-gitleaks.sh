#!/usr/bin/env bash
# Installs the pinned gitleaks release from the official GitHub release, checked against a sha256 kept in this file.
# Used by secret-scan.yml. gitleaks/gitleaks-action is not used: it asks organisations for a licence key (a new
# secret), and a checked binary needs neither the key nor a third-party action. To bump: take the new version,
# read the sha256 of gitleaks_<version>_linux_x64.tar.gz from the release's gitleaks_<version>_checksums.txt, and
# change both values together.
# Usage: hack/install-gitleaks.sh <target dir>    (the binary is <target dir>/gitleaks; linux amd64 only)
set -euo pipefail

GITLEAKS_VERSION=8.30.1
GITLEAKS_SHA256=551f6fc83ea457d62a0d98237cbad105af8d557003051f41f3e7ca7b3f2470eb

if [ "$#" -ne 1 ]; then
  echo "usage: $0 <target dir>" >&2
  exit 2
fi
target="$1"
archive="gitleaks_${GITLEAKS_VERSION}_linux_x64.tar.gz"
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT

# Retries ride out short outages of the release CDN (503 and 504 on 2026-11-08, #543).
curl -fsSL --proto '=https' --connect-timeout 20 --max-time 60 --retry 4 --retry-delay 10 \
  -o "${work}/${archive}" "https://github.com/gitleaks/gitleaks/releases/download/v${GITLEAKS_VERSION}/${archive}"
echo "${GITLEAKS_SHA256}  ${work}/${archive}" | sha256sum --check --strict
mkdir -p "${target}"
tar -xzf "${work}/${archive}" -C "${target}" gitleaks
"${target}/gitleaks" version
