#!/usr/bin/env bash
# Tests hack/check-chart-docs.sh with small README tables: complete, a value without a description, a value
# without a section, no table. Run: hack/check-chart-docs_test.sh (or make test-scripts)
set -euo pipefail

cd "$(dirname "$0")"
fail=0
head=$'### General\n\n| Key | Type | Default | Description |\n|-----|------|---------|-------------|\n| a.b | bool | `true` | Does b. |'

expect() { # name, want (0 or 1), input
  local got=0
  ./check-chart-docs.sh <<<"$3" > /dev/null 2>&1 || got=1
  if [ "$got" != "$2" ]; then
    echo "FAIL: $1 (exit $got, want $2)" >&2
    fail=1
  else
    echo "ok: $1"
  fi
}

expect "all described" 0 "$head"
expect "empty description" 1 "$head"$'\n| a.c | string | `""` |  |'
expect "no section" 1 "$head"$'\n\n### Other Values\n\n| Key | Type | Default | Description |\n|-----|------|---------|-------------|\n| a.d | int | `1` | Does d. |'
expect "no table" 1 "no table here"

exit "$fail"
