#!/usr/bin/env bash
# Tests hack/ci-changed.sh in a scratch repository: which changed files run which job groups.
# Run: hack/ci-changed_test.sh (or make test-scripts)
set -euo pipefail

script="$(cd "$(dirname "$0")" && pwd)/ci-changed.sh"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
cd "$tmp"
git init -q .
export GIT_AUTHOR_NAME=t GIT_COMMITTER_NAME=t GIT_AUTHOR_EMAIL=t GIT_COMMITTER_EMAIL=t
mkdir -p docs api dist/chart .github/workflows hack
for f in docs/a.md api/x.go dist/chart/values.yaml .github/workflows/lint.yml .github/workflows/docs.yml .github/workflows/ci.yml README.md LICENSE hack/ci-changed.sh; do echo 1 > "$f"; done
git add -A
git commit -q -m base
base="$(git rev-parse HEAD)"
fail=0

# check <name> <expected "go images chart"> <event> <file to change>
check() {
  git reset -q --hard "$base"
  if [ -n "$4" ]; then
    mkdir -p "$(dirname "$4")"
    echo 2 >> "$4"
    git add -A
    git commit -q -m change
  fi
  got="$(EVENT_NAME="$3" BASE_SHA="$base" "$script" 2>/dev/null | sed 's/^[a-z]*=//' | tr '\n' ' ' | sed 's/ $//')"
  if [ "$got" = "$2" ]; then
    echo "ok: $1"
  else
    echo "FAIL: $1: want '$2', got '$got'" >&2
    fail=1
  fi
}

pr=pull_request
check "docs only skips everything" "false false false" $pr docs/a.md
check "readme only skips everything" "false false false" $pr README.md
check "generated api docs run go and images" "true true false" $pr docs/reference/api.md
check "go file runs everything" "true true true" $pr api/x.go
# The chart install tests build and run the images, so every input of the binaries and the image build runs them.
check "executor main runs everything" "true true true" $pr cmd/main.go
check "analyzer main runs everything" "true true true" $pr cmd/analyzer/main.go
check "internal package runs everything" "true true true" $pr internal/cmdutil/manager.go
check "root go file runs everything" "true true true" $pr tools.go
check "dockerfile runs everything" "true true true" $pr Dockerfile
check "dockerignore runs everything" "true true true" $pr .dockerignore
check "chart only runs chart" "false false true" $pr dist/chart/values.yaml
check "chart ci values run chart" "false false true" $pr dist/chart/ci/default-values.yaml
check "ct config runs chart" "false false true" $pr ct.yaml
check "trivy ignore file runs chart" "false false true" $pr .trivyignore.yaml
check "chart test workflow runs everything" "true true true" $pr .github/workflows/chart-test.yml
check "licence runs images only" "false true false" $pr LICENSE
check "ci.yml runs everything" "true true true" $pr .github/workflows/ci.yml
check "a called workflow runs everything" "true true true" $pr .github/workflows/lint.yml
check "an unrelated workflow runs nothing" "false false false" $pr .github/workflows/docs.yml
check "this script runs everything" "true true true" $pr hack/ci-changed.sh
check "push runs everything" "true true true" push ""
check "manual run runs everything" "true true true" workflow_dispatch ""

# An unknown base fails open.
git reset -q --hard "$base"
got="$(EVENT_NAME=$pr BASE_SHA=0000000000000000000000000000000000000000 "$script" 2>/dev/null | tr '\n' ' ')"
if [ "$got" = "go=true images=true chart=true " ]; then echo "ok: unknown base fails open"; else echo "FAIL: unknown base: $got" >&2; fail=1; fi

exit "$fail"
