#!/usr/bin/env bash
# Prints the digest of each platform image (child manifest) of a multi-platform image index, one line per
# platform in the form `amd64=sha256:...` and `arm64=sha256:...`. The Images workflow attests one SBOM per
# child digest (#341), and the release steps verify them, so all of them resolve the children the same way:
# from the index manifest, by digest, never by tag. BuildKit also adds attestation manifests (platform
# unknown/unknown) to the index; they are ignored. Fails unless each platform has exactly one image.
# Needs docker (buildx imagetools) and jq. Usage:
#   hack/image-platform-digests.sh <registry/name@sha256:index digest>
set -euo pipefail

if [ "$#" -ne 1 ]; then
  echo "usage: $0 <image@sha256:digest>" >&2
  exit 2
fi
ref="$1"
printf '%s' "${ref}" | grep -Eq '^[A-Za-z0-9._/:-]+@sha256:[a-f0-9]{64}$' || { echo "not an image by digest: '${ref}'" >&2; exit 1; }

raw="$(docker buildx imagetools inspect --raw "${ref}")"
for arch in amd64 arm64; do
  d="$(jq -r --arg a "${arch}" '[.manifests[]? | select(.platform.os == "linux" and .platform.architecture == $a) | .digest] | if length == 1 then .[0] else empty end' <<<"${raw}")"
  printf '%s' "${d}" | grep -Eq '^sha256:[a-f0-9]{64}$' || { echo "${ref} has no single linux/${arch} image" >&2; exit 1; }
  echo "${arch}=${d}"
done
