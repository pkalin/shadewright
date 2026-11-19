#!/usr/bin/env bash
# Installs the Helm chart (dist/chart) as an analyzer-only install on the current Kubernetes context: the Kind
# cluster of the Chart workflow's chart-testing job, whose analyzer image was built from this checkout and loaded
# under the appVersion tag (make chart-ct-cluster, or make test-chart-analyzer-only, which builds that one image).
# Values: executor.enabled=false, no verifier namespace, one watched namespace (demo), no action policy, no
# triage model. The executor and verifier images are never pulled, so a cluster without them is enough. It checks
# that
#   - the release installs and the analyzer Deployment becomes Available,
#   - no Deployment, ServiceAccount or Pod of the executor or the verifier exists (by name and by label), and
#     the metrics-auth binding does not name them,
#   - a Deployment in demo that can never start (its image does not exist) gets an Incident, which needs no model
#     (the analysis is off without --triage-model),
#   - no RemediationRequest exists once the Incident does (without a policy the analyzer proposes nothing, and
#     no executor would set a phase).
# The Incident can take a few minutes (the collector holds a pod's consequence for ConsequenceGrace before it
# decides): the wait is bounded by INCIDENT_TIMEOUT seconds (default 420) and a failed read is retried, never
# read as "no Incident". Then it uninstalls and deletes the kept CRDs, so a later install starts clean. Usage:
#   hack/chart-analyzer-only-install-test.sh <kubectl> <helm>
# Locally: make test-chart-analyzer-only
set -euo pipefail

KUBECTL="${1:-kubectl}"
HELM="${2:-helm}"
NS=shadewright-analyzer-only
WATCHED=demo-analyzer-only
REL=shadewright
CHART="$(cd "$(dirname "$0")/.." && pwd)/dist/chart"
INCIDENT_TIMEOUT="${INCIDENT_TIMEOUT:-420}"

# shellcheck disable=SC2329 # invoked by the EXIT trap below
cleanup() {
  local rc=$?
  if [ "${rc}" -ne 0 ]; then
    echo "::group::diagnostics of ${NS} (exit ${rc})" >&2
    "${KUBECTL}" -n "${NS}" get pods,incidents.remediation.shadewright.io,remediationrequests.remediation.shadewright.io -o wide >&2 || true
    "${KUBECTL}" -n "${WATCHED}" get pods >&2 || true
    "${KUBECTL}" -n "${NS}" describe pods >&2 || true
    "${KUBECTL}" -n "${NS}" logs deployment/shadewright-analyzer --tail=200 >&2 || true
    "${KUBECTL}" -n "${NS}" logs deployment/shadewright-analyzer --previous --tail=200 >&2 || true
    echo "::endgroup::" >&2
  fi
  "${HELM}" uninstall "${REL}" --namespace "${NS}" --wait >/dev/null 2>&1 || true
  "${KUBECTL}" delete crd -l "app.kubernetes.io/instance=${REL}" --ignore-not-found >/dev/null 2>&1 || true
  "${KUBECTL}" delete namespace "${NS}" "${WATCHED}" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  return "${rc}"
}
trap cleanup EXIT

"${KUBECTL}" create namespace "${WATCHED}" --dry-run=client -o yaml | "${KUBECTL}" apply -f -

"${HELM}" install "${REL}" "${CHART}" --namespace "${NS}" --create-namespace \
  --set image.allowTag=true \
  --set clusterId=chart-analyzer-only \
  --set executor.enabled=false \
  --set "analyzer.watchNamespaces={${WATCHED}}" \
  --set-json 'verifier.watchNamespaces=[]' \
  --wait --timeout 5m

fail=0
check() { # description, command...
  local what="$1"
  shift
  if "$@"; then echo "ok: ${what}"; else echo "FAIL: ${what}" >&2; fail=1; fi
}

check "Deployment shadewright-analyzer is Available" \
  "${KUBECTL}" -n "${NS}" wait --for=condition=Available deployment/shadewright-analyzer --timeout=120s

# A failed read is an error (set -e stops here), not an empty answer.
absent="$("${KUBECTL}" get deployments,serviceaccounts,pods,roles,rolebindings,clusterroles,clusterrolebindings,leases,services \
  --all-namespaces -o name | grep -E 'shadewright-(executor|verifier)' || true)"
check "no executor or verifier object exists by name" test -z "${absent}"
labelled="$("${KUBECTL}" get deployments,serviceaccounts,pods --all-namespaces \
  -l "app.kubernetes.io/instance=${REL},app.kubernetes.io/component in (executor,verifier)" -o name)"
check "no executor or verifier Deployment, ServiceAccount or Pod exists by label" test -z "${labelled}"
subjects="$("${KUBECTL}" get clusterrolebinding "${REL}-metrics-auth-rolebinding" -o jsonpath='{.subjects[*].name}')"
check "the metrics-auth binding names the analyzer" grep -qw shadewright-analyzer <<<"${subjects}"
check "the metrics-auth binding names no executor or verifier" test -z "$(grep -oE 'shadewright-(executor|verifier)' <<<"${subjects}" || true)"

# A Deployment whose image cannot be pulled: its pod waits in ErrImagePull and ImagePullBackOff, a cause the
# collector reports once ConsequenceGrace has passed. The registry name is reserved (RFC 6761) and never resolves.
"${KUBECTL}" -n "${WATCHED}" apply -f - <<YAML
apiVersion: apps/v1
kind: Deployment
metadata:
  name: broken
spec:
  replicas: 1
  selector:
    matchLabels: {app: broken}
  template:
    metadata:
      labels: {app: broken}
    spec:
      containers:
      - name: broken
        image: registry.invalid/shadewright/broken:1
YAML

deadline=$((SECONDS + INCIDENT_TIMEOUT))
incidents=""
while :; do
  # A failed read is a retry, not "no Incident".
  if out="$("${KUBECTL}" -n "${NS}" get incidents.remediation.shadewright.io -o name 2>/dev/null)"; then
    incidents="${out}"
    [ -n "${incidents}" ] && break
  fi
  if [ "${SECONDS}" -ge "${deadline}" ]; then break; fi
  sleep 10
done
check "the broken workload in ${WATCHED} has an Incident" test -n "${incidents}"
"${KUBECTL}" -n "${NS}" get incidents.remediation.shadewright.io -o wide || true

requests="$("${KUBECTL}" get remediationrequests.remediation.shadewright.io --all-namespaces -o name)"
check "no RemediationRequest exists" test -z "${requests}"

exit "${fail}"
