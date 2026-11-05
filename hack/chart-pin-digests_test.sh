#!/usr/bin/env bash
# Tests hack/chart-pin-digests.sh on a copy of the chart: the digests it is given are the ones the rendered
# Deployments run, each image keeps its own, and a bad digest fails and leaves the chart untouched. A stub helm
# that renders another pair (a chart that does not carry the pinned digests) makes the check fail.
# Run: hack/chart-pin-digests_test.sh (or make test-scripts)
set -euo pipefail

cd "$(dirname "$0")"
fail=0
tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT
ex="sha256:$(printf 'a%.0s' $(seq 64))"
an="sha256:$(printf 'b%.0s' $(seq 64))"
other="sha256:$(printf 'c%.0s' $(seq 64))"

check() { # name, condition result (0 ok)
  if [ "$2" != 0 ]; then echo "FAIL: $1" >&2; fail=1; else echo "ok: $1"; fi
}

cp -R ../dist/chart "${tmp}/chart"
./chart-pin-digests.sh "${tmp}/chart" "${ex}" "${an}" > /dev/null
rendered="$(helm template t "${tmp}/chart" --set clusterId=t)"
check "executor runs the given digest" "$(grep -qF "shadewright-executor@${ex}\"" <<<"${rendered}" && echo 0 || echo 1)"
check "analyzer runs the given digest" "$(grep -qF "shadewright-analyzer@${an}\"" <<<"${rendered}" && echo 0 || echo 1)"

cp -R ../dist/chart "${tmp}/chart2"
r=0; ./chart-pin-digests.sh "${tmp}/chart2" "bad" "${an}" 2>/dev/null || r=1
check "bad digest refused" "$([ "${r}" = 1 ] && echo 0 || echo 1)"
check "refusal leaves the chart" "$(cmp -s "${tmp}/chart2/values.yaml" ../dist/chart/values.yaml && echo 0 || echo 1)"

# A helm that renders another pair than the one pinned: the mismatch is refused, for each image.
mkdir "${tmp}/bin"
cp -R ../dist/chart "${tmp}/chart3"
for which in executor analyzer; do
  e="${ex}" a="${an}"
  if [ "${which}" = executor ]; then e="${other}"; else a="${other}"; fi
  printf '#!/usr/bin/env bash\necho "image: \\"x/shadewright-executor@%s\\""\necho "image: \\"x/shadewright-analyzer@%s\\""\n' "${e}" "${a}" > "${tmp}/bin/helm"
  chmod +x "${tmp}/bin/helm"
  cp ../dist/chart/values.yaml "${tmp}/chart3/values.yaml"
  r=0; PATH="${tmp}/bin:${PATH}" ./chart-pin-digests.sh "${tmp}/chart3" "${ex}" "${an}" 2>/dev/null || r=1
  check "a rendered ${which} digest that differs is refused" "$([ "${r}" = 1 ] && echo 0 || echo 1)"
done

exit "${fail}"
