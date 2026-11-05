#!/usr/bin/env bash
# Resolves the digests of the executor and analyzer images of a release tag and verifies each one (cosign
# signature and SPDX SBOM attestation) against the identity of the Images workflow on that very tag. Prints
# `executor=<digest>` and `analyzer=<digest>` on stdout (the format of $GITHUB_OUTPUT); progress goes to stderr.
# The Chart workflow runs this once, in its `images` job: the install test and the publish job both use these
# digests, so the chart that is published points at the images that were tested. Usage:
#   hack/resolve-image-digests.sh <tag, with v> <owner/repository> <owner>
# The Images workflow of the same tag runs in parallel, so this waits for each image and for a signature that
# verifies: every POLL_SECONDS (30), at most WAIT_SECONDS (1800) in all, then fails naming the image it waited for.
# Needs docker (buildx imagetools) and cosign, both already logged in to ghcr.io where the packages are private.
set -euo pipefail

if [ "$#" -ne 3 ]; then
  echo "usage: $0 <tag> <owner/repository> <owner>" >&2
  exit 2
fi
tag="$1" repository="$2"
owner="$(printf '%s' "$3" | tr '[:upper:]' '[:lower:]')"
poll="${POLL_SECONDS:-30}" wait_for="${WAIT_SECONDS:-1800}"
issuer="https://token.actions.githubusercontent.com"
identity="https://github.com/${repository}/.github/workflows/images.yml@refs/tags/${tag}"
deadline=$(( $(date +%s) + wait_for ))

found_executor="" found_analyzer=""
for image in executor analyzer; do
  ref="ghcr.io/${owner}/shadewright-${image}"
  d=""
  until [ -n "${d}" ]; do
    got="$(docker buildx imagetools inspect "${ref}:${tag#v}" --format '{{json .Manifest.Digest}}' 2>/dev/null | tr -d '"' || true)"
    if printf '%s' "${got}" | grep -Eq '^sha256:[a-f0-9]{64}$' && cosign verify \
        --certificate-identity "${identity}" --certificate-oidc-issuer "${issuer}" \
        "${ref}@${got}" > /dev/null 2>&1 && cosign verify-attestation --type spdxjson \
        --certificate-identity "${identity}" --certificate-oidc-issuer "${issuer}" \
        "${ref}@${got}" > /dev/null 2>&1; then
      d="${got}"
    elif [ "$(date +%s)" -ge "${deadline}" ]; then
      echo "no image ${ref}:${tag#v} signed and attested by ${identity} after $((wait_for / 60)) minutes; re-run this job once the Images workflow of ${tag} has finished" >&2
      exit 1
    else
      echo "waiting for ${ref}:${tag#v} and its signature" >&2
      sleep "${poll}"
    fi
  done
  echo "${ref}@${d}" >&2
  printf -v "found_${image}" '%s' "${d}"
done
echo "executor=${found_executor}"
echo "analyzer=${found_analyzer}"
