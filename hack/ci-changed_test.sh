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

# check <name> <expected "go images chart docs codeql"> <event> <file to change>
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
check "docs only skips everything" "false false false true false" $pr docs/a.md
check "readme only skips everything" "false false false false false" $pr README.md
check "generated api docs run go and images" "true true false true false" $pr docs/reference/api.md
check "go file runs everything" "true true true false true" $pr api/x.go
# The chart install tests build and run the images, so every input of the binaries and the image build runs them.
check "executor main runs everything" "true true true false true" $pr cmd/main.go
check "analyzer main runs everything" "true true true false true" $pr cmd/analyzer/main.go
check "internal package runs everything" "true true true false true" $pr internal/cmdutil/manager.go
check "root go file runs everything" "true true true false true" $pr tools.go
check "dockerfile runs everything" "true true true false false" $pr Dockerfile
check "dockerignore runs everything" "true true true false false" $pr .dockerignore
check "chart only runs chart" "false false true false false" $pr dist/chart/values.yaml
check "chart ci values run chart" "false false true false false" $pr dist/chart/ci/default-values.yaml
check "ct config runs chart" "false false true false false" $pr ct.yaml
check "trivy ignore file runs chart" "false false true false false" $pr .trivyignore.yaml
check "chart test workflow runs everything" "true true true true true" $pr .github/workflows/chart-test.yml
check "licence runs images only" "false true false false false" $pr LICENSE
check "ci.yml runs everything" "true true true true true" $pr .github/workflows/ci.yml
check "a called workflow runs everything" "true true true true true" $pr .github/workflows/lint.yml
check "a workflow only runs codeql" "false false false false true" $pr .github/workflows/docs.yml
check "docs workflow runs everything" "true true true true true" $pr .github/workflows/docs-check.yml
check "zensical config runs docs only" "false false false true false" $pr zensical.toml
check "theme overrides run docs only" "false false false true false" $pr overrides/main.html
check "glossary snippet runs docs only" "false false false true false" $pr includes/glossary.md
check "docs requirements run docs only" "false false false true false" $pr requirements-docs.txt
check "published paths list runs everything" "true true true true false" $pr hack/docs-published-paths.txt
check "readme only skips docs" "false false false false false" $pr README.md
check "go.mod runs codeql" "true true true false true" $pr go.mod
check "this script runs everything" "true true true true true" $pr hack/ci-changed.sh
check "push runs everything" "true true true true true" push ""
check "manual run runs everything" "true true true true true" workflow_dispatch ""

# An unknown base fails open.
git reset -q --hard "$base"
got="$(EVENT_NAME=$pr BASE_SHA=0000000000000000000000000000000000000000 "$script" 2>/dev/null | tr '\n' ' ')"
if [ "$got" = "go=true images=true chart=true docs=true codeql=true " ]; then echo "ok: unknown base fails open"; else echo "FAIL: unknown base: $got" >&2; fail=1; fi

exit "$fail"
