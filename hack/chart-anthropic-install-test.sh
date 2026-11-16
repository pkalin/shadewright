#!/usr/bin/env bash
# Installs the Helm chart (dist/chart) with Anthropic-only values (test/chart/testdata/anthropic-only-values.yaml:
# a triage model and the API key from a Secret, no Azure client ID, no endpoint suffixes, no record store) on the
# current Kubernetes context: the Kind cluster of the Chart workflow's chart-testing job, whose images were built
# from this checkout. The Go render tests prove what the chart renders for such an install; this proves a cluster
# runs it (#814). It checks that
#   - the release installs and all three Deployments become Available,
#   - the analyzer Pod has not restarted and its log has no line about an Azure token source or a workload
#     identity (the analyzer builds none without --model-endpoint-suffixes),
#   - RemediationSettings with one Anthropic deployment and no apiVersion is accepted by the API server (server
#     side dry run, so the CRD's validation rules run) and the same object with provider Foundry is refused.
# The Secret holds a dummy value, never a real key: the job makes no model call (the analyzer reads the key
# file at each use). Then it uninstalls and deletes the kept CRDs, so a later install starts clean. Usage:
#   hack/chart-anthropic-install-test.sh <kubectl> <helm> [values file]
# Locally: make test-chart-anthropic
set -euo pipefail

KUBECTL="${1:-kubectl}"
HELM="${2:-helm}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
VALUES="${3:-${ROOT}/test/chart/testdata/anthropic-only-values.yaml}"
case "${VALUES}" in /*) ;; *) VALUES="${PWD}/${VALUES}" ;; esac
NS=shadewright-anthropic
REL=shadewright
CHART="${ROOT}/dist/chart"

# shellcheck disable=SC2329 # invoked by the EXIT trap below
cleanup() {
  local rc=$?
  if [ "${rc}" -ne 0 ]; then
    echo "::group::diagnostics of ${NS} (exit ${rc})" >&2
    "${KUBECTL}" -n "${NS}" get pods -o wide >&2 || true
    "${KUBECTL}" -n "${NS}" describe pods >&2 || true
    "${KUBECTL}" -n "${NS}" logs deployment/shadewright-analyzer --tail=200 >&2 || true
    "${KUBECTL}" -n "${NS}" logs deployment/shadewright-analyzer --previous --tail=200 >&2 || true
    echo "::endgroup::" >&2
  fi
  "${HELM}" uninstall "${REL}" --namespace "${NS}" --wait >/dev/null 2>&1 || true
  "${KUBECTL}" delete crd -l "app.kubernetes.io/instance=${REL}" --ignore-not-found >/dev/null 2>&1 || true
  "${KUBECTL}" delete namespace "${NS}" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  return "${rc}"
}
trap cleanup EXIT

"${KUBECTL}" create namespace demo --dry-run=client -o yaml | "${KUBECTL}" apply -f -
"${KUBECTL}" create namespace "${NS}" --dry-run=client -o yaml | "${KUBECTL}" apply -f -
# A dummy value: nothing in this job calls the model.
"${KUBECTL}" -n "${NS}" create secret generic anthropic-key --from-literal=api-key=sk-ant-dummy-not-a-real-key

"${HELM}" install "${REL}" "${CHART}" --namespace "${NS}" -f "${VALUES}" --wait --timeout 5m

fail=0
check() { # description, command...
  local what="$1"
  shift
  if "$@"; then echo "ok: ${what}"; else echo "FAIL: ${what}" >&2; fail=1; fi
}

for d in shadewright-executor shadewright-analyzer shadewright-verifier; do
  check "Deployment ${d} is Available" "${KUBECTL}" -n "${NS}" wait --for=condition=Available "deployment/${d}" --timeout=120s
done

# A failed read is an error (set -e stops here), not an empty answer.
restarts="$("${KUBECTL}" -n "${NS}" get pods -l "app.kubernetes.io/instance=${REL},app.kubernetes.io/component=analyzer" \
  -o jsonpath='{range .items[*].status.containerStatuses[*]}{.restartCount}{"\n"}{end}')"
check "the analyzer has a container status" test -n "${restarts}"
check "the analyzer has not restarted" test -z "$(grep -vx 0 <<<"${restarts}" || true)"

logs="$("${KUBECTL}" -n "${NS}" logs deployment/shadewright-analyzer)"
check "the analyzer log is not empty" test -n "${logs}"
check "the analyzer log has no line about an Azure token source or workload identity" \
  test -z "$(grep -iE 'token source|workload identity|azure' <<<"${logs}" || true)"

settings() { # provider, apiVersion line (may be empty)
  cat <<YAML
apiVersion: remediation.shadewright.io/v1alpha1
kind: RemediationSettings
metadata:
  name: default
spec:
  analysis:
    models:
      triage:
        provider: $1
        endpoint: $2
        deployment: claude-haiku-5-5
        maxTokensPerDay: 1000
YAML
}
anthropic="$(settings Anthropic https://api.anthropic.com)"
foundry="$(settings Foundry https://lab.services.ai.azure.com/)"
check "RemediationSettings with an Anthropic deployment and no apiVersion is accepted" \
  "${KUBECTL}" apply --dry-run=server -f - <<<"${anthropic}"
out="$("${KUBECTL}" apply --dry-run=server -f - <<<"${foundry}" 2>&1 || true)"
check "RemediationSettings with a Foundry deployment and no apiVersion is refused" grep -q "needs apiVersion" <<<"${out}"

exit "${fail}"
