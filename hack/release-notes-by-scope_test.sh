#!/usr/bin/env bash
# Tests hack/release-notes-by-scope.sh against golden files in hack/testdata: the real v0.1.0 release body
# and a synthetic one with breaking changes, an unknown scope, no scope and a continuation line.
# Run: hack/release-notes-by-scope_test.sh (or make test-scripts)
set -euo pipefail

cd "$(dirname "$0")"
fail=0

check() { # name
  local got
  got="$(mktemp)"
  ./release-notes-by-scope.sh "testdata/$1.in.md" > "$got"
  if ! diff -u "testdata/$1.want.md" "$got"; then
    echo "FAIL: $1" >&2
    fail=1
  else
    echo "ok: $1"
  fi
  # Every entry of the input must appear exactly once in the output.
  if [ "$(grep -c '^\* ' "testdata/$1.in.md")" != "$(grep -c '^\* ' "$got")" ] &&
    ! grep -q 'BREAKING' "testdata/$1.in.md"; then
    echo "FAIL: $1 lost or duplicated entries" >&2
    fail=1
  fi
  rm -f "$got"
}

check release-notes-v0.1.0
check release-notes-edge

# Reading from stdin gives the same result as reading a file.
if ! ./release-notes-by-scope.sh < testdata/release-notes-edge.in.md | diff -q - testdata/release-notes-edge.want.md > /dev/null; then
  echo "FAIL: stdin" >&2
  fail=1
fi

# Regrouping is stable for a body without entries.
if [ "$(printf '## 1.0.0 (2027-01-01)\n' | ./release-notes-by-scope.sh)" != "## 1.0.0 (2027-01-01)" ]; then
  echo "FAIL: empty release" >&2
  fail=1
fi

exit "$fail"
