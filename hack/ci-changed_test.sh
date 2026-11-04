#!/usr/bin/env bash
# Tests hack/ci-changed.sh in a scratch repository: which changed files run which job group.
# Run: hack/ci-changed_test.sh (or make test-scripts)
set -euo pipefail

script="$(cd "$(dirname "$0")" && pwd)/ci-changed.sh"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
cd "$tmp"
git init -q .
git config user.email t@example.invalid
git config user.name t
mkdir -p docs api dist/chart .github/workflows hack
for f in docs/a.md api/x.go dist/chart/values.yaml .github/workflows/lint.yml .github/workflows/chart.yml README.md LICENSE; do echo 1 > "$f"; done
git add -A
git commit -q -m base
base="$(git rev-parse HEAD)"
fail=0

# check <name> <expected> <group> <workflow> <event> <file to change>
check() {
  git reset -q --hard "$base"
  if [ -n "$6" ]; then
    mkdir -p "$(dirname "$6")"
    echo 2 >> "$6"
    git add -A
    git commit -q -m change
  fi
  got="$(EVENT_NAME="$5" BASE_SHA="$base" "$script" "$3" "$4" 2>/dev/null)"
  if [ "$got" = "run=$2" ]; then
    echo "ok: $1"
  else
    echo "FAIL: $1: want run=$2, got $got" >&2
    fail=1
  fi
}

pr=pull_request
check "docs only skips go" false go .github/workflows/lint.yml $pr docs/a.md
check "docs only skips chart" false chart .github/workflows/chart.yml $pr docs/a.md
check "docs only skips images" false images .github/workflows/images.yml $pr docs/a.md
check "readme only skips go" false go .github/workflows/lint.yml $pr README.md
check "generated api docs run go" true go .github/workflows/lint.yml $pr docs/reference/api.md
check "go file runs go" true go .github/workflows/lint.yml $pr api/x.go
check "go file runs chart" true chart .github/workflows/chart.yml $pr api/x.go
check "chart only skips go" false go .github/workflows/lint.yml $pr dist/chart/values.yaml
check "chart only runs chart" true chart .github/workflows/chart.yml $pr dist/chart/values.yaml
check "own workflow runs its group" true go .github/workflows/lint.yml $pr .github/workflows/lint.yml
check "other workflow skips the group" false go .github/workflows/lint.yml $pr .github/workflows/chart.yml
check "licence runs images only" true images .github/workflows/images.yml $pr LICENSE
check "licence skips go" false go .github/workflows/lint.yml $pr LICENSE
check "this script runs everything" true chart .github/workflows/chart.yml $pr hack/ci-changed.sh
check "push runs everything" true go .github/workflows/lint.yml push ""
check "schedule runs everything" true go .github/workflows/lint.yml schedule ""

# An unknown base fails open.
git reset -q --hard "$base"
got="$(EVENT_NAME=$pr BASE_SHA=0000000000000000000000000000000000000000 "$script" go .github/workflows/lint.yml 2>/dev/null)"
if [ "$got" = "run=true" ]; then echo "ok: unknown base fails open"; else echo "FAIL: unknown base: $got" >&2; fail=1; fi

exit "$fail"
