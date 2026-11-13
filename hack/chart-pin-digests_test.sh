#!/usr/bin/env bash
# Tests hack/chart-pin-digests.sh on a copy of the chart: the digests it is given are the ones the rendered
# Deployments run, each image keeps its own, and a bad digest fails and leaves the chart untouched. A stub helm
# that renders another pair, or the right digests on the wrong images, makes the check fail.
# Needs the pinned helm of the chart tests (HELM names it): run with make test-chart-pin, which the chart-test
# workflow does; it is not part of test-scripts because that runs without a pinned helm.
set -euo pipefail

cd "$(dirname "$0")"
HELM="${HELM:-helm}"
fail=0
tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT
ex="sha256:$(printf 'a%.0s' $(seq 64))"
an="sha256:$(printf 'b%.0s' $(seq 64))"
vf="sha256:$(printf 'd%.0s' $(seq 64))"
other="sha256:$(printf 'c%.0s' $(seq 64))"

check() { # name, condition result (0 ok)
  if [ "$2" != 0 ]; then echo "FAIL: $1" >&2; fail=1; else echo "ok: $1"; fi
}

mkdir "${tmp}/bin"
stub_helm() { # executor, analyzer and verifier digest: a helm that renders these
  printf '#!/usr/bin/env bash\necho "image: \\"x/shadewright-executor@%s\\""\necho "image: \\"x/shadewright-analyzer@%s\\""\necho "image: \\"x/shadewright-verifier@%s\\""\n' "$1" "$2" "$3" > "${tmp}/bin/helm"
  chmod +x "${tmp}/bin/helm"
}

cp -R ../dist/chart "${tmp}/chart"
HELM="${HELM}" ./chart-pin-digests.sh "${tmp}/chart" "${ex}" "${an}" "${vf}" > /dev/null
rendered="$("${HELM}" template t "${tmp}/chart" --set clusterId=t)"
check "executor runs the given digest" "$(grep -qF "shadewright-executor@${ex}\"" <<<"${rendered}" && echo 0 || echo 1)"
check "analyzer runs the given digest" "$(grep -qF "shadewright-analyzer@${an}\"" <<<"${rendered}" && echo 0 || echo 1)"
check "verifier runs the given digest" "$(grep -qF "shadewright-verifier@${vf}\"" <<<"${rendered}" && echo 0 || echo 1)"

cp -R ../dist/chart "${tmp}/chart2"
r=0; HELM="${HELM}" ./chart-pin-digests.sh "${tmp}/chart2" "bad" "${an}" "${vf}" 2>/dev/null || r=1
check "bad digest refused" "$([ "${r}" = 1 ] && echo 0 || echo 1)"
check "refusal leaves the chart" "$(cmp -s "${tmp}/chart2/values.yaml" ../dist/chart/values.yaml && echo 0 || echo 1)"

# A helm that renders another pair than the one pinned: the mismatch is refused, for each image.
cp -R ../dist/chart "${tmp}/chart3"
for which in executor analyzer verifier; do
  e="${ex}" a="${an}" v="${vf}"
  case "${which}" in executor) e="${other}" ;; analyzer) a="${other}" ;; verifier) v="${other}" ;; esac
  stub_helm "${e}" "${a}" "${v}"
  cp ../dist/chart/values.yaml "${tmp}/chart3/values.yaml"
  r=0; HELM="${tmp}/bin/helm" ./chart-pin-digests.sh "${tmp}/chart3" "${ex}" "${an}" "${vf}" 2>/dev/null || r=1
  check "a rendered ${which} digest that differs is refused" "$([ "${r}" = 1 ] && echo 0 || echo 1)"
done
# The right digests on the wrong images (a swapped pair).
stub_helm "${an}" "${ex}" "${vf}"
cp ../dist/chart/values.yaml "${tmp}/chart3/values.yaml"
r=0; HELM="${tmp}/bin/helm" ./chart-pin-digests.sh "${tmp}/chart3" "${ex}" "${an}" "${vf}" 2>/dev/null || r=1
check "a swapped pair is refused" "$([ "${r}" = 1 ] && echo 0 || echo 1)"

exit "${fail}"
