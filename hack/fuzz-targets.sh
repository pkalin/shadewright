#!/usr/bin/env bash
# Checks the list of Go fuzz targets (FUZZ_TARGETS in the Makefile) against the tree, both ways: every
# `func FuzzX(f *testing.F)` in a _test.go file under api/, cmd/ and internal/ is listed as <package dir>:FuzzX, and
# every listed target names such a function in that package. A stale or misspelled entry would otherwise fuzz
# nothing and pass, because `go test -fuzz` exits 0 when no fuzz test matches.
# With --tags, prints instead the build tags a fuzz target of the package dir runs with: `redactwork` when a Go
# file of the package is built only with that tag (`//go:build redactwork`), so a target that checks the work
# bound of redaction counts the work while it fuzzes (without the tag nothing is counted and the bound is not
# checked); nothing otherwise. `make fuzz` takes the tags from here, so a package that gains such a file is fuzzed
# with the tag without a second list to keep in step.
# Usage: hack/fuzz-targets.sh <repository root> <package:FuzzName>...
#        hack/fuzz-targets.sh --tags <repository root> <package dir>
set -euo pipefail

if [[ "${1:-}" == "--tags" ]]; then
  cd "$2"
  if grep -lqsE '^//go:build redactwork$' "$3"/*.go; then
    echo "redactwork"
  fi
  exit 0
fi

root="$1"
shift
cd "${root}"

found="$(grep -rEo --include='*_test.go' '^func Fuzz[A-Za-z0-9_]*\(f \*testing\.F\)' api cmd internal 2> /dev/null |
  sed -E 's#^(.*)/[^/]+_test\.go:func (Fuzz[A-Za-z0-9_]*)\(.*$#\1:\2#' | sort -u || true)"
listed="$(printf '%s\n' "$@" | sed '/^$/d' | sort -u)"

missing="$(comm -23 <(printf '%s\n' "${found}" | sed '/^$/d') <(printf '%s\n' "${listed}"))"
stale="$(comm -13 <(printf '%s\n' "${found}" | sed '/^$/d') <(printf '%s\n' "${listed}"))"
fail=0
if [[ -n "${missing}" ]]; then
  echo "Fuzz functions not listed in FUZZ_TARGETS: ${missing//$'\n'/ }" >&2
  fail=1
fi
if [[ -n "${stale}" ]]; then
  echo "FUZZ_TARGETS entries with no such fuzz function in that package: ${stale//$'\n'/ }" >&2
  fail=1
fi
exit "${fail}"
