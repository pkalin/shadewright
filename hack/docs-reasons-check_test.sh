#!/usr/bin/env bash
# Tests hack/docs-reasons-check.py: the real page passes, and a removed row, a row for a reason that does not
# exist, a duplicate anchor and a removed condition type heading each fail.
# Run: hack/docs-reasons-check_test.sh (or make test-scripts)
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
script="$root/hack/docs-reasons-check.py"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
fail=0

# expect <name> <pass|fail> <page> [text the output must mention]
expect() {
  if python3 -I "$script" --root "$root" --page "$3" > "$tmp/out" 2>&1; then got=pass; else got=fail; fi
  if [ "$got" != "$2" ]; then echo "FAIL: $1: want $2, got $got" >&2; cat "$tmp/out" >&2; fail=1; return; fi
  if [ -n "${4:-}" ] && ! grep -q -- "$4" "$tmp/out"; then echo "FAIL: $1: output lacks '$4'" >&2; cat "$tmp/out" >&2; fail=1; return; fi
  echo "ok: $1"
}

page="$root/docs/reasons.md"
expect "the page passes" pass "$page"

grep -v 'id="q-circuitopen"' "$page" > "$tmp/p1.md"
expect "a removed row fails" fail "$tmp/p1.md" "reason CircuitOpen is in the code"

{ cat "$page"; echo '| <span id="x-nosuchreason"></span>`NoSuchReason` | a | b |'; } > "$tmp/p2.md"
expect "a row for a missing reason fails" fail "$tmp/p2.md" "row NoSuchReason"

{ cat "$page"; echo '| <span id="q-circuitopen"></span>`CircuitOpen` | a | b |'; } > "$tmp/p3.md"
expect "a duplicate anchor fails" fail "$tmp/p3.md" "used twice"

grep -v '^### RevertsStopped' "$page" > "$tmp/p4.md"
expect "a removed condition heading fails" fail "$tmp/p4.md" "condition type RevertsStopped"

exit "$fail"
