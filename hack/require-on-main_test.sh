#!/usr/bin/env bash
# Tests hack/require-on-main.sh in a throwaway repository: a commit on main (or equal to it) passes, a commit on
# a side branch and a checkout without main fail.
# Run: hack/require-on-main_test.sh (or make test-scripts)
set -euo pipefail

script="$(cd "$(dirname "$0")" && pwd)/require-on-main.sh"
tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT
cd "${tmp}"
git init -q -b main .
git config user.email t@example.invalid
git config user.name t
git config commit.gpgsign false
git commit -q --allow-empty -m one
git commit -q --allow-empty -m two
git update-ref refs/remotes/origin/main HEAD
git checkout -q -b side
git commit -q --allow-empty -m side
fail=0

"${script}" main > /dev/null 2>&1 && echo "ok: main commit" || { echo "FAIL: main commit refused" >&2; fail=1; }
"${script}" main~1 > /dev/null 2>&1 && echo "ok: older main commit" || { echo "FAIL: older main commit refused" >&2; fail=1; }
if "${script}" side > /dev/null 2>&1; then echo "FAIL: side branch commit accepted" >&2; fail=1; else echo "ok: side commit refused"; fi
if "${script}" HEAD > /dev/null 2>&1; then echo "FAIL: HEAD on side accepted" >&2; fail=1; else echo "ok: HEAD on side refused"; fi
if "${script}" main origin/nope > /dev/null 2>&1; then echo "FAIL: missing main accepted" >&2; fail=1; else echo "ok: no main refused"; fi
exit "${fail}"
