#!/usr/bin/env bash
# Runs make targets side by side, one process each, and keeps each target's output in its own file. Once all have
# finished it prints each output whole, in the order given, under a header with its exit status and time, so the
# outputs never interleave. Exits 1 when any target failed (or was killed) and names them.
# Usage: hack/side-by-side.sh <target>...
# Environment:
#   MAKE  the make to call (the Makefile passes $(MAKE)); default make
set -uo pipefail

if [ "$#" -eq 0 ]; then
	echo "usage: $0 <target>..." >&2
	exit 2
fi

make_cmd="${MAKE:-make}"
dir="$(mktemp -d)"
pids=()
cleanup() {
	# On an interrupt: stop each make (a child of its subshell), then the subshell.
	for p in "${pids[@]+"${pids[@]}"}"; do
		pkill -TERM -P "$p" 2>/dev/null
		kill "$p" 2>/dev/null
	done
	rm -rf "$dir"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

i=0
for t in "$@"; do
	echo "side by side: started $t"
	(
		start=$(date +%s)
		"$make_cmd" --no-print-directory "$t" >"$dir/$i.log" 2>&1
		rc=$?
		echo "$rc $(($(date +%s) - start))" >"$dir/$i.rc"
	) &
	pids+=("$!")
	i=$((i + 1))
done
wait
pids=()

failed=()
i=0
for t in "$@"; do
	rc=killed
	secs="?"
	if [ -s "$dir/$i.rc" ]; then
		read -r rc secs <"$dir/$i.rc"
	fi
	echo "=== $t: exit $rc, ${secs}s"
	cat "$dir/$i.log" 2>/dev/null
	if [ "$rc" != 0 ]; then
		failed+=("$t")
	fi
	i=$((i + 1))
done

if [ "${#failed[@]}" -gt 0 ]; then
	echo "side by side: failed: ${failed[*]}" >&2
	exit 1
fi
echo "side by side: all $# passed"
