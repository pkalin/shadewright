#!/usr/bin/env bash
# Pins the image digests it is given into the chart before it is packaged (the publish job of the Chart workflow)
# and checks that the chart really renders them: the Deployments must run exactly these three digests, so the
# published chart cannot carry any other pair than the one the install test ran. Usage:
#   hack/chart-pin-digests.sh <chart dir> <executor digest> <analyzer digest> <verifier digest>
# Needs helm ($HELM, default helm on PATH; the Makefile passes its HELM). The digests are validated by hack/chart-set-digests.sh.
set -euo pipefail

if [ "$#" -ne 4 ]; then
  echo "usage: $0 <chart dir> <executor digest> <analyzer digest> <verifier digest>" >&2
  exit 2
fi
chart="$1" executor="$2" analyzer="$3" verifier="$4"
"$(dirname "$0")/chart-set-digests.sh" "${chart}/values.yaml" "${executor}" "${analyzer}" "${verifier}"
# The verifier is rendered only with a namespace to watch, so the check names one.
rendered="$("${HELM:-helm}" template publish-check "${chart}" --set clusterId=publish-check --set 'verifier.watchNamespaces={publish-check}')"
# Each image must run its own digest (a swapped pair must not pass).
for pair in "executor ${executor}" "analyzer ${analyzer}" "verifier ${verifier}"; do
  image="${pair%% *}" d="${pair#* }"
  grep -qF "shadewright-${image}@${d}\"" <<<"${rendered}" || { echo "the chart does not run ${d} as the ${image} image" >&2; exit 1; }
done
