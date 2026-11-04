#!/usr/bin/env bash
# Refuses to publish from a release tag whose commit is not on main (the publish jobs of the Images and Chart
# workflows, before anything is pushed or signed). A v* tag can be pushed by anyone with write access to any
# commit, and the signature identity only says "this workflow on a v* tag", so without this check a tag on a
# feature branch would publish signed images and a chart. Usage:
#   hack/require-on-main.sh [commit [main ref]]     (defaults: HEAD, origin/main)
# The checkout must have main's history (actions/checkout with fetch-depth: 0 on tags).
set -euo pipefail

commit="${1:-HEAD}" main="${2:-origin/main}"
if ! git rev-parse --verify --quiet "${main}^{commit}" > /dev/null; then
  echo "cannot find ${main}: the checkout has no history of main" >&2
  exit 1
fi
if ! git merge-base --is-ancestor "${commit}" "${main}"; then
  echo "refusing to publish: $(git rev-parse --short "${commit}") is not an ancestor of ${main}; tag a commit that is on main" >&2
  exit 1
fi
echo "$(git rev-parse --short "${commit}") is on ${main}"
