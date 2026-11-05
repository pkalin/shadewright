#!/usr/bin/env bash
# Decides which CI job groups have to run, from the files a pull request changes. Called once by the `changes`
# job of .github/workflows/ci.yml, which passes the result to the jobs it calls.
# Usage: hack/ci-changed.sh    (prints go=, images= and chart= lines, each true or false; append to $GITHUB_OUTPUT)
# Environment:
#   EVENT_NAME  github.event_name; anything but "pull_request" (push to main, manual run) runs everything
#   BASE_SHA    base commit of the pull request; HEAD is compared with it (the checkout is the merge commit)
# Groups: go (lint, test, e2e, govulncheck), images (image build), chart (chart lint, render, install).
# A change to ci.yml, to a workflow it calls or to this script runs every group. Fails open: when the diff
# cannot be computed everything runs, so a problem here never lets a change skip its checks.
set -euo pipefail

go_paths='.*\.go|go\.mod|go\.sum|api/.*|cmd/.*|internal/.*|config/.*|test/.*|hack/.*|Dockerfile|\.dockerignore|Makefile|\.golangci[^/]*|\.custom-gcl\.yml|PROJECT|docs/reference/.*'
go_re="^(${go_paths})$"
# The image build context also takes the licence files (see .dockerignore).
images_re="^(${go_paths}|LICENSE|NOTICE)$"
# The chart is compared with config/default, rendered by Go tests and installed by scripts in hack/; Trivy scans it
# and config/ with the ignores of .trivyignore.yaml. The install tests build both images from the checkout and run
# them with the chart's flags, so the binaries' sources and the image build are chart inputs too (the licence
# files only change the image content, not what the chart runs).
chart_re='^(dist/chart/.*|config/.*|test/.*|hack/.*|api/.*|cmd/.*|internal/.*|.*\.go|go\.mod|go\.sum|Dockerfile|\.dockerignore|Makefile|ct\.yaml|\.trivyignore\.yaml|PROJECT)$'
# ci.yml and the workflows it calls, and this script.
self_re='^(\.github/workflows/(ci|lint|test|test-e2e|vulncheck|workflow-lint|images-build|chart-test)\.yml|hack/ci-changed\.sh)$'

emit() { # go images chart
  echo "go=$1"
  echo "images=$2"
  echo "chart=$3"
}

if [ "${EVENT_NAME:-}" != "pull_request" ]; then
  emit true true true
  exit 0
fi

if [ -z "${BASE_SHA:-}" ] || ! git cat-file -e "${BASE_SHA}^{commit}" 2>/dev/null; then
  git fetch --no-tags --depth=1 origin "${BASE_SHA:-}" >&2 || true
fi
if ! files="$(git diff --name-only --no-renames "${BASE_SHA:-}" HEAD 2>/dev/null)"; then
  echo "cannot diff against ${BASE_SHA:-unset}: running everything" >&2
  emit true true true
  exit 0
fi

echo "changed files:" >&2
printf '%s\n' "$files" | sed 's/^/  /' >&2

match() { printf '%s\n' "$files" | grep -Eq "$1" && echo true || echo false; }

if [ "$(match "$self_re")" = true ]; then
  emit true true true
else
  emit "$(match "$go_re")" "$(match "$images_re")" "$(match "$chart_re")"
fi
