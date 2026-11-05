#!/usr/bin/env bash
# Tests hack/fuzz-targets.sh on a throwaway tree: the exact list passes; a missing, stale or misplaced entry, or the
# same name in a second package, fails; a helper whose name starts with Fuzz is not a target.
# Run: hack/fuzz-targets_test.sh (or make test-scripts)
set -euo pipefail

script="$(cd "$(dirname "$0")" && pwd)/fuzz-targets.sh"
tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT
mkdir -p "${tmp}/internal/a" "${tmp}/internal/b" "${tmp}/cmd"
printf 'package a\n\nfunc FuzzParse(f *testing.F) {}\n\nfunc FuzzSeeds() []string { return nil }\n' > "${tmp}/internal/a/a_test.go"
printf 'package b\n\nfunc FuzzMatch(f *testing.F) {}\n' > "${tmp}/internal/b/b_test.go"
fail=0

pass() { # name, targets...
  local name="$1"
  shift
  if "${script}" "${tmp}" "$@" > /dev/null 2>&1; then echo "ok: ${name}"; else echo "FAIL: ${name} refused" >&2; fail=1; fi
}
refuse() { # name, targets...
  local name="$1"
  shift
  if "${script}" "${tmp}" "$@" > /dev/null 2>&1; then echo "FAIL: ${name} accepted" >&2; fail=1; else echo "ok: ${name}"; fi
}

pass "exact list" internal/a:FuzzParse internal/b:FuzzMatch
refuse "missing target" internal/a:FuzzParse
refuse "stale target" internal/a:FuzzParse internal/b:FuzzMatch internal/b:FuzzGone
refuse "target in the wrong package" internal/a:FuzzParse internal/a:FuzzMatch
refuse "helper listed as a target" internal/a:FuzzParse internal/b:FuzzMatch internal/a:FuzzSeeds
printf 'package main\n\nfunc FuzzParse(f *testing.F) {}\n' > "${tmp}/cmd/main_test.go"
refuse "same name in a second package" internal/a:FuzzParse internal/b:FuzzMatch
pass "both packages listed" internal/a:FuzzParse internal/b:FuzzMatch cmd:FuzzParse
exit "${fail}"
