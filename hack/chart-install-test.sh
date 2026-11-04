#!/usr/bin/env bash
# Installs the Helm chart (dist/chart) on the current Kubernetes context (a Kind cluster, see make
# test-chart-install) with the v0.1.0 images and checks what the API server made of it: the Deployments become
# available, the settings object is created from values, and the two ServiceAccounts hold exactly the
# permissions the design gives them. Usage:
#   hack/chart-install-test.sh <kubectl> <helm>
set -euo pipefail

KUBECTL="${1:-kubectl}"
HELM="${2:-helm}"
NS=shadewright-system
REL=shadewright
CHART="$(cd "$(dirname "$0")/.." && pwd)/dist/chart"
EXECUTOR_DIGEST="${EXECUTOR_DIGEST:-sha256:66df21ea967a22e8e50b73284254b796665ef5e9470b320707900983aca37f9c}"
ANALYZER_DIGEST="${ANALYZER_DIGEST:-sha256:697defe7774b29621f64c967ff93e83b309455c2a72d50b190e179f9ff3fc8e4}"

"${KUBECTL}" create namespace demo --dry-run=client -o yaml | "${KUBECTL}" apply -f -

"${HELM}" install "${REL}" "${CHART}" --namespace "${NS}" --create-namespace \
  --set clusterId=chart-test \
  --set "analyzer.watchNamespaces={demo}" \
  --set "executor.image.digest=${EXECUTOR_DIGEST}" \
  --set "analyzer.image.digest=${ANALYZER_DIGEST}" \
  --set settings.create=true --set settings.spec.paused=true \
  --wait --timeout 5m

# settings.create=true on a fresh install: the settings object is a post-install hook because its CRD comes
# from the same release. An upgrade replaces it again (before-hook-creation) and must not fail either.
"${KUBECTL}" get remediationsettings.remediation.shadewright.io default
"${HELM}" upgrade "${REL}" "${CHART}" --namespace "${NS}" --reuse-values --wait --timeout 5m
"${KUBECTL}" get remediationsettings.remediation.shadewright.io default

"${KUBECTL}" -n "${NS}" get deployments,serviceaccounts,networkpolicies,services

fail=0
# can <sa> <expected yes|no> <kubectl auth can-i args...>
can() {
  local sa="$1" want="$2"
  shift 2
  local got
  got="$("${KUBECTL}" auth can-i "$@" --as "system:serviceaccount:${NS}:${sa}" || true)"
  if [ "${got}" != "${want}" ]; then
    echo "FAIL: ${sa} can-i $* = ${got}, want ${want}" >&2
    fail=1
  fi
}

# Executor: statuses of requests and reads of its targets; nothing on Secrets or Incidents but one get.
can shadewright-executor yes get remediationrequests.remediation.shadewright.io -n "${NS}"
can shadewright-executor yes patch remediationrequests/status.remediation.shadewright.io -n "${NS}"
can shadewright-executor yes get deployments.apps -n demo
can shadewright-executor no create incidents.remediation.shadewright.io -n "${NS}"
can shadewright-executor no update deployments.apps -n demo
can shadewright-executor no get secrets -n "${NS}"
can shadewright-executor no list nodes
# Analyzer: reads the watched namespace, writes Incidents, never a workload or a Secret.
can shadewright-analyzer yes create incidents.remediation.shadewright.io -n "${NS}"
can shadewright-analyzer yes list pods -n demo
can shadewright-analyzer yes list nodes
can shadewright-analyzer no list pods -n kube-system
can shadewright-analyzer no patch remediationrequests.remediation.shadewright.io -n "${NS}"
can shadewright-analyzer no update deployments.apps -n demo
can shadewright-analyzer no get secrets -n demo
can shadewright-analyzer no create configmaps -n "${NS}"

exit "${fail}"
