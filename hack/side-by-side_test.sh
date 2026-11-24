#!/usr/bin/env bash
# Tests hack/side-by-side.sh on a throwaway Makefile: the targets run at the same time (the first waits for a file
# only the second writes), each output is printed whole under its own header, and one failing target fails the run
# and is named while the others still run to the end.
# Run: hack/side-by-side_test.sh (or make test-scripts)
set -euo pipefail

script="$(cd "$(dirname "$0")" && pwd)/side-by-side.sh"
tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT
fail=0

# Recipe lines start with a tab. The $$ in them is make's, so single quotes are meant.
# shellcheck disable=SC2016
printf '%s\n' \
	'waiter:' \
	'	@echo waiter line one' \
	'	@for i in $$(seq 50); do [ -f mark ] && break; sleep 0.1; done; [ -f mark ]' \
	'	@echo waiter line two' \
	'marker:' \
	'	@echo marker ran' \
	'	@touch mark' \
	'broken:' \
	'	@echo broken said this' \
	'	@false' \
	>"${tmp}/Makefile"

out="$(cd "${tmp}" && "${script}" waiter marker 2>&1)" && rc=0 || rc=$?
if [[ "${rc}" -eq 0 ]]; then echo "ok: side by side passes"; else echo "FAIL: side by side exit ${rc}: ${out}" >&2; fail=1; fi
# Each output stays in one piece: the header, then that target's lines.
want="$(printf '=== waiter: exit 0, *\nwaiter line one\nwaiter line two\n=== marker: exit 0, *\nmarker ran')"
got="$(grep -v '^side by side:' <<<"${out}" | sed 's/, [0-9]*s$/, */')"
if [[ "${got}" == "${want//\*/*}" ]]; then echo "ok: outputs whole and in order"; else echo "FAIL: output: ${got}" >&2; fail=1; fi

rm -f "${tmp}/mark"
out="$(cd "${tmp}" && "${script}" broken waiter marker 2>&1)" && rc=0 || rc=$?
if [[ "${rc}" -ne 0 ]]; then echo "ok: a failing target fails the run"; else echo "FAIL: a failing target passed" >&2; fail=1; fi
if grep -q '^side by side: failed: broken$' <<<"${out}" && grep -q '^broken said this$' <<<"${out}" \
	&& grep -q '^=== waiter: exit 0' <<<"${out}"; then
	echo "ok: the failing target is named with its output, the others finish"
else
	echo "FAIL: output: ${out}" >&2
	fail=1
fi

exit "${fail}"
