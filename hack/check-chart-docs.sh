#!/usr/bin/env bash
# Fails when a value of the chart has no description. helm-docs lists a key that has no "# --" comment with an
# empty Description cell, and puts a key that has no "# @section" under "Other Values". Both are errors here.
# Reads the rendered README on standard input: helm-docs ... --dry-run | hack/check-chart-docs.sh
set -euo pipefail

content="$(cat)"

# Rows of the values tables: "| key | type | default | description |", with an empty last cell.
empty="$(awk -F'|' '/^\| / && $2 !~ /^ *Key *$/ && $(NF-1) ~ /^ *$/ { gsub(/^ +| +$/, "", $2); print $2 }' <<<"$content")"

status=0
if [ -n "$empty" ]; then
	echo "error: these chart values have no description (add a '# --' comment above them in values.yaml):" >&2
	sed 's/^/  /' <<<"$empty" >&2
	status=1
fi
if grep -q '^### Other Values' <<<"$content"; then
	echo "error: some chart values have no '# @section' comment (the README has an 'Other Values' section)." >&2
	status=1
fi
if ! grep -q '^| ' <<<"$content"; then
	echo "error: no values table found" >&2
	status=1
fi
exit "$status"
