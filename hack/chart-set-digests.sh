#!/usr/bin/env bash
# Writes the image digests of a release into the chart's values.yaml before it is packaged (the publish job of
# the Chart workflow), so the published chart runs the release's signed images by digest. A source checkout
# keeps empty digests and runs the appVersion tag. Usage:
#   hack/chart-set-digests.sh <values.yaml> <executor digest> <analyzer digest> <verifier digest>
# Each digest must be sha256: and 64 lower case hex digits. The file must have exactly one empty
# `digest: ""` under each of executor.image, analyzer.image and verifier.image; anything else (a digest already set, a
# changed layout) fails, so the published chart never carries a digest it was not given.
set -euo pipefail

if [ "$#" -ne 4 ]; then
  echo "usage: $0 <values.yaml> <executor digest> <analyzer digest> <verifier digest>" >&2
  exit 2
fi
values="$1" executor="$2" analyzer="$3" verifier="$4"
for d in "${executor}" "${analyzer}" "${verifier}"; do
  if ! printf '%s' "${d}" | grep -Eq '^sha256:[a-f0-9]{64}$'; then
    echo "not a digest: '${d}'" >&2
    exit 1
  fi
done

tmp="$(mktemp)"
trap 'rm -f "${tmp}"' EXIT
# Tracks the top-level key and whether the line is inside its image: block (two-space indent under the
# component, four-space keys under image).
awk -v ex="${executor}" -v an="${analyzer}" -v vf="${verifier}" '
  /^[^ #]/ { top = $0; sub(/:.*/, "", top); inimage = 0 }
  /^  [^ #]/ { inimage = ($0 ~ /^  image:[ ]*$/) }
  inimage && /^    digest: ""[ ]*$/ {
    if (top == "executor") { print "    digest: \"" ex "\""; n["executor"]++; next }
    if (top == "analyzer") { print "    digest: \"" an "\""; n["analyzer"]++; next }
    if (top == "verifier") { print "    digest: \"" vf "\""; n["verifier"]++; next }
  }
  { print }
  END {
    if (n["executor"] != 1 || n["analyzer"] != 1 || n["verifier"] != 1) {
      printf "want one empty digest under executor.image, analyzer.image and verifier.image, found %d, %d and %d\n", n["executor"], n["analyzer"], n["verifier"] > "/dev/stderr"
      exit 1
    }
  }
' "${values}" > "${tmp}"
cat "${tmp}" > "${values}"
