#!/usr/bin/env bash
# Pins the image digests it is given into the chart before it is packaged (the publish job of the Chart workflow)
# and checks that the chart really renders them: the Deployments must run exactly these two digests, so the
# published chart cannot carry any other pair than the one the install test ran. Usage:
#   hack/chart-pin-digests.sh <chart dir> <executor digest> <analyzer digest>
# Needs helm. The digests are validated by hack/chart-set-digests.sh.
set -euo pipefail

if [ "$#" -ne 3 ]; then
  echo "usage: $0 <chart dir> <executor digest> <analyzer digest>" >&2
  exit 2
fi
chart="$1" executor="$2" analyzer="$3"
"$(dirname "$0")/chart-set-digests.sh" "${chart}/values.yaml" "${executor}" "${analyzer}"
rendered="$(helm template publish-check "${chart}" --set clusterId=publish-check)"
for d in "${executor}" "${analyzer}"; do
  grep -qF "@${d}\"" <<<"${rendered}" || { echo "the chart does not run ${d}" >&2; exit 1; }
done
