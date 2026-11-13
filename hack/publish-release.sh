#!/usr/bin/env bash
# Publish only after Chart verified the images and chart, wrote their notes, and Release plugin uploaded
# every signed asset. Missing assets leave the release a draft. A rerun of a complete release is a no-op.
# Usage: publish-release.sh <owner/repo> <tag> <executor digest> <analyzer digest> <verifier digest> <chart digest>
set -euo pipefail

if [ "$#" -ne 6 ]; then
  echo "usage: $0 <owner/repo> <tag> <executor digest> <analyzer digest> <verifier digest> <chart digest>" >&2
  exit 2
fi
repo="$1" tag="$2"
# Reuse boundary validation and require the exact verification section for this tag and these digests.
expected="$("$(dirname "$0")/release-verify-notes.sh" "$@" < /dev/null)"
assets="$(jq -cn --arg v "${tag#v}" '
  ["linux", "darwin"] as $os | ["amd64", "arm64"] as $arch |
  [($os[] as $o | $arch[] as $a | "kubectl-shadewright_\($v)_\($o)_\($a).tar.gz"),
   "checksums.txt", "checksums.txt.sigstore.json"]')"
# At most 30 minutes for the parallel plugin build/sign/upload workflow. Tests override the wait only.
deadline=$(( $(date +%s) + ${RELEASE_WAIT_SECONDS:-1800} ))
while true; do
  if release="$(gh release view "${tag}" --repo "${repo}" --json tagName,isDraft,body,assets)" &&
    jq -e --arg tag "${tag}" --arg notes "${expected}" --argjson names "${assets}" '
      .tagName == $tag and (.isDraft == true or .isDraft == false) and
      (.body | contains($notes)) and
      (.assets as $assets | all($names[]; . as $name |
        any($assets[]; .name == $name and .state == "uploaded" and .size > 0)))
    ' <<< "${release}" > /dev/null; then
    if [ "$(jq -r .isDraft <<< "${release}")" = true ]; then
      gh release edit "${tag}" --repo "${repo}" --draft=false
      echo "published ${tag}: verified images, chart digests and all six plugin assets present"
    else
      echo "release ${tag} is already published with all required artifacts"
    fi
    exit 0
  fi
  if [ "$(date +%s)" -ge "${deadline}" ]; then
    echo "release ${tag} is incomplete: missing release, exact digest/verification notes or uploaded plugin assets; leaving draft unpublished. Fix the failed tag workflow and re-run Publish the complete release" >&2
    exit 1
  fi
  echo "waiting for release ${tag}: digest/verification notes and all six plugin assets required"
  sleep 15
done
