#!/usr/bin/env bash
# Tests hack/chart-set-digests.sh on a copy of the chart's values.yaml: both digests land under their own
# component (and the test image keeps its own), a bad digest, a missing argument and a second run fail and
# leave the file as it was. Run: hack/chart-set-digests_test.sh (or make test-scripts)
set -euo pipefail

cd "$(dirname "$0")"
fail=0
tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT
ex="sha256:$(printf 'a%.0s' $(seq 64))"
an="sha256:$(printf 'b%.0s' $(seq 64))"
vf="sha256:$(printf 'd%.0s' $(seq 64))"

check() { # name, condition result (0 ok)
  if [ "$2" != 0 ]; then echo "FAIL: $1" >&2; fail=1; else echo "ok: $1"; fi
}

cp ../dist/chart/values.yaml "${tmp}/values.yaml"
./chart-set-digests.sh "${tmp}/values.yaml" "${ex}" "${an}" "${vf}"
got_ex="$(awk '/^executor:/{c=1} /^[a-z]/ && !/^executor:/{c=0} c && /^    digest:/{print $2; exit}' "${tmp}/values.yaml")"
got_an="$(awk '/^analyzer:/{c=1} /^[a-z]/ && !/^analyzer:/{c=0} c && /^    digest:/{print $2; exit}' "${tmp}/values.yaml")"
got_vf="$(awk '/^verifier:/{c=1} /^[a-z]/ && !/^verifier:/{c=0} c && /^    digest:/{print $2; exit}' "${tmp}/values.yaml")"
check "executor digest set" "$([ "${got_ex}" = "\"${ex}\"" ] && echo 0 || echo 1)"
check "analyzer digest set" "$([ "${got_an}" = "\"${an}\"" ] && echo 0 || echo 1)"
check "verifier digest set" "$([ "${got_vf}" = "\"${vf}\"" ] && echo 0 || echo 1)"
check "three lines changed" "$([ "$(diff ../dist/chart/values.yaml "${tmp}/values.yaml" | grep -c '^>')" = 3 ] && echo 0 || echo 1)"

cp "${tmp}/values.yaml" "${tmp}/before.yaml"
r=0; ./chart-set-digests.sh "${tmp}/values.yaml" "${ex}" "${an}" "${vf}" 2>/dev/null || r=1
check "a second run fails" "$([ "${r}" = 1 ] && echo 0 || echo 1)"
check "a failed run leaves the file" "$(cmp -s "${tmp}/values.yaml" "${tmp}/before.yaml" && echo 0 || echo 1)"

cp ../dist/chart/values.yaml "${tmp}/values.yaml"
for bad in latest "sha256:abc" "${ex}x"; do
  r=0; ./chart-set-digests.sh "${tmp}/values.yaml" "${bad}" "${an}" "${vf}" 2>/dev/null || r=1
  check "bad digest '${bad}' refused" "$([ "${r}" = 1 ] && echo 0 || echo 1)"
done
r=0; ./chart-set-digests.sh "${tmp}/values.yaml" "${ex}" "${an}" 2>/dev/null || r=1
check "missing argument refused" "$([ "${r}" = 1 ] && echo 0 || echo 1)"
check "refusals leave the file" "$(cmp -s "${tmp}/values.yaml" ../dist/chart/values.yaml && echo 0 || echo 1)"

exit "${fail}"
