#!/usr/bin/env bash
# Installs the pinned Trivy release from the official GitHub release, checked against a sha256 kept in this file.
# Used by chart-test.yml (trivy config) and trivy.yml (image scan), so the version lives in one place.
# Trivy is installed from the release archive, not through aquasecurity/trivy-action: in March 2026 an attacker
# force-pushed 76 of 77 trivy-action tags and all setup-trivy tags to malicious code (GHSA-69fq-xp46-6x23,
# CVE-2026-33634), and trivy v0.69.4 to v0.69.6 were affected too. A binary pinned by a checksum written here
# needs none of those actions. To bump: take the new version, read the sha256 of trivy_<version>_Linux-64bit.tar.gz
# from the release's trivy_<version>_checksums.txt, and change both values together.
# Usage: hack/install-trivy.sh <target dir>    (the binary is <target dir>/trivy; linux amd64 only)
set -euo pipefail

TRIVY_VERSION=0.76.1
TRIVY_SHA256=c7cbe3137fb8a48a17574bede3dd6beb16742d1e9772929940fc90082c39d9c0

if [ "$#" -ne 1 ]; then
  echo "usage: $0 <target dir>" >&2
  exit 2
fi
target="$1"
archive="trivy_${TRIVY_VERSION}_Linux-64bit.tar.gz"
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT

# Retries ride out short outages of the release CDN (503 and 504 on 2026-11-08, #543).
curl -fsSL --proto '=https' --connect-timeout 20 --max-time 60 --retry 4 --retry-delay 10 \
  -o "${work}/${archive}" "https://github.com/aquasecurity/trivy/releases/download/v${TRIVY_VERSION}/${archive}"
echo "${TRIVY_SHA256}  ${work}/${archive}" | sha256sum --check --strict
mkdir -p "${target}"
tar -xzf "${work}/${archive}" -C "${target}" trivy
"${target}/trivy" --version | head -n 1
