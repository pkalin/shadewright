#!/usr/bin/env bash
# Decides whether a CI job group has to run, from the files a pull request changes.
# Usage: hack/ci-changed.sh <group> <workflow-file>
#   group:         go | chart | images
#   workflow-file: path of the calling workflow, e.g. .github/workflows/lint.yml (a change to it always runs it)
# Environment:
#   EVENT_NAME  github.event_name; anything but "pull_request" (push to main, tags, schedule, manual run) runs everything
#   BASE_SHA    base commit of the pull request; HEAD is compared with it (the checkout is the merge commit)
# Prints "run=true" or "run=false" (append it to $GITHUB_OUTPUT). Fails open: when the diff cannot be computed it
# says run=true, so a problem here never lets a change skip its checks.
set -euo pipefail

group="${1:?group}"
workflow="${2:?workflow file}"

go_paths='^(.*\.go|go\.mod|go\.sum|api/|cmd/|internal/|config/|test/|hack/|Dockerfile|\.dockerignore|Makefile|\.golangci[^/]*|\.custom-gcl\.yml|PROJECT|docs/reference/)'
case "$group" in
go) pattern="$go_paths" ;;
# The image build context also takes the licence files (see .dockerignore).
images) pattern="${go_paths%)}|LICENSE|NOTICE)" ;;
# The chart is compared with config/default, rendered by Go tests and installed by scripts in hack/.
chart) pattern='^(dist/chart/|config/|test/|hack/|api/|go\.mod|go\.sum|Makefile|ct\.yaml|PROJECT)' ;;
*)
  echo "unknown group: $group" >&2
  exit 2
  ;;
esac

if [ "${EVENT_NAME:-}" != "pull_request" ]; then
  echo "run=true"
  exit 0
fi

# Always needed to tell the workflow file and this script apart from the rest.
self='^('"${workflow//./\\.}"'|hack/ci-changed\.sh)$'

if [ -z "${BASE_SHA:-}" ] || ! git cat-file -e "${BASE_SHA}^{commit}" 2>/dev/null; then
  git fetch --no-tags --depth=1 origin "${BASE_SHA:-}" >&2 || true
fi
if ! files="$(git diff --name-only --no-renames "${BASE_SHA:-}" HEAD 2>/dev/null)"; then
  echo "cannot diff against ${BASE_SHA:-unset}: running everything" >&2
  echo "run=true"
  exit 0
fi

echo "changed files:" >&2
printf '%s\n' "$files" | sed 's/^/  /' >&2
if printf '%s\n' "$files" | grep -Eq "$pattern|$self"; then
  echo "run=true"
else
  echo "run=false"
fi
