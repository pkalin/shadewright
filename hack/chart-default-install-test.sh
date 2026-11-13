#!/usr/bin/env bash
# Installs the Helm chart (dist/chart) with its default values plus dist/chart/ci/default-values.yaml (only
# image.allowTag, since a source checkout has no digests) on the current Kubernetes context: the Kind cluster
# of the Chart workflow's chart-testing job, whose images were built from this checkout and loaded under the
# appVersion tag. It checks what the default install may do. The analyzer is Ready, watches no namespace,
# holds no RoleBinding outside the operator namespace and cannot list Pods or events anywhere (it still lists
# Nodes and reads discovery and the settings cluster-wide, and writes only in the operator namespace plus the
# settings status). The verifier follows the analyzer: no watched namespace, so no pod read either. No ServiceAccount has an Azure workload identity, so the executor cannot reach Key
# Vault to sign for the GitHub App. The cluster ID is the UID of kube-system. Then it runs helm test,
# uninstalls and deletes the kept CRDs, so a later install on the same cluster (ct install) starts clean.
# Usage:
#   hack/chart-default-install-test.sh <kubectl> <helm> <values file>
set -euo pipefail

KUBECTL="${1:-kubectl}"
HELM="${2:-helm}"
VALUES="${3:?usage: $0 <kubectl> <helm> <values file>}"
NS=shadewright-defaults
REL=shadewright
CHART="$(cd "$(dirname "$0")/.." && pwd)/dist/chart"

cleanup() {
  "${HELM}" uninstall "${REL}" --namespace "${NS}" --wait >/dev/null 2>&1 || true
  # crd.keep (the default) keeps the CRDs with the ownership of this release; another release cannot adopt them.
  "${KUBECTL}" delete crd -l "app.kubernetes.io/instance=${REL}" --ignore-not-found >/dev/null 2>&1 || true
  "${KUBECTL}" delete namespace "${NS}" --ignore-not-found --wait=false >/dev/null 2>&1 || true
}
trap cleanup EXIT

"${HELM}" install "${REL}" "${CHART}" --namespace "${NS}" --create-namespace -f "${VALUES}" --wait --timeout 5m

fail=0
check() { # description, command...
  local what="$1"
  shift
  if "$@"; then echo "ok: ${what}"; else echo "FAIL: ${what}" >&2; fail=1; fi
}

uid="$("${KUBECTL}" get namespace kube-system -o jsonpath='{.metadata.uid}')"
args="$("${KUBECTL}" -n "${NS}" get deployment shadewright-analyzer -o jsonpath='{range .spec.template.spec.containers[0].args[*]}{@}{"\n"}{end}')"
check "cluster ID is the kube-system UID" grep -qxF -- "--cluster-id=${uid}" <<<"${args}"
check "the analyzer watches no namespace" grep -qxF -- "--watch-namespaces=" <<<"${args}"
logs="$("${KUBECTL}" -n "${NS}" logs deployment/shadewright-analyzer)"
check "the analyzer logs that it watches no namespaces" grep -q "Watching no namespaces" <<<"${logs}"
vargs="$("${KUBECTL}" -n "${NS}" get deployment shadewright-verifier -o jsonpath='{range .spec.template.spec.containers[0].args[*]}{@}{"\n"}{end}')"
check "the verifier follows the analyzer and watches no namespace" grep -qxF -- "--watch-namespaces=" <<<"${vargs}"
bindings="$("${KUBECTL}" get rolebindings --all-namespaces -l "app.kubernetes.io/instance=${REL}" \
  -o jsonpath='{range .items[*]}{.metadata.namespace}{"\n"}{end}' | sort -u)"
check "no RoleBinding outside ${NS}" test -z "$(grep -vxF "${NS}" <<<"${bindings}" || true)"

# can <sa> <expected yes|no> <kubectl auth can-i args...>
can() {
  local sa="$1" want="$2"
  shift 2
  local got
  got="$("${KUBECTL}" auth can-i "$@" --as "system:serviceaccount:${NS}:${sa}" || true)"
  check "${sa} can-i $* = ${want}" test "${got}" = "${want}"
}
for ns in default kube-system "${NS}"; do
  can shadewright-analyzer no list pods -n "${ns}"
  can shadewright-analyzer no list events.events.k8s.io -n "${ns}"
done
can shadewright-analyzer no list pods --all-namespaces
can shadewright-analyzer yes create incidents.remediation.shadewright.io -n "${NS}"
for ns in default kube-system "${NS}"; do
  can shadewright-verifier no list pods -n "${ns}"
done
can shadewright-verifier no list pods --all-namespaces
can shadewright-verifier yes create verificationreports.remediation.shadewright.io -n "${NS}"

for sa in shadewright-executor shadewright-analyzer shadewright-verifier; do
  ann="$("${KUBECTL}" -n "${NS}" get serviceaccount "${sa}" -o jsonpath='{.metadata.annotations}')"
  check "${sa} has no workload identity annotation" test -z "$(grep -o 'azure.workload.identity' <<<"${ann}" || true)"
  dep="${sa}"
  label="$("${KUBECTL}" -n "${NS}" get deployment "${dep}" -o jsonpath='{.spec.template.metadata.labels.azure\.workload\.identity/use}')"
  check "${dep} Pods do not ask for a workload identity" test -z "${label}"
done

check "helm test passes" "${HELM}" test "${REL}" --namespace "${NS}"

exit "${fail}"
