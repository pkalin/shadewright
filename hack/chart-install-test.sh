#!/usr/bin/env bash
# Installs the Helm chart (dist/chart) on the current Kubernetes context (a Kind cluster, see make
# test-chart-install) and checks what the API server made of it: the Deployments become
# available, the settings object is created from values, and the two ServiceAccounts hold the
# permissions the design gives them (a spot-check). Usage:
#   hack/chart-install-test.sh <kubectl> <helm>
# Images: pull request and main runs install the appVersion tag (image.allowTag) of images built from the same
# checkout and loaded into Kind by make test-chart-install: the chart renders the flags of its own commit, which a
# released image may not know (an analyzer exits on an unknown flag). A tag run gets EXECUTOR_DIGEST and
# ANALYZER_DIGEST from the Chart workflow, which resolved and verified them once (hack/resolve-image-digests.sh)
# and publishes the chart with the same pair; giving only one of the two fails.
set -euo pipefail

KUBECTL="${1:-kubectl}"
HELM="${2:-helm}"
NS=shadewright-system
REL=shadewright
CHART="$(cd "$(dirname "$0")/.." && pwd)/dist/chart"

if [ -n "${EXECUTOR_DIGEST:-}" ] || [ -n "${ANALYZER_DIGEST:-}" ]; then
  if [ -z "${EXECUTOR_DIGEST:-}" ] || [ -z "${ANALYZER_DIGEST:-}" ]; then
    echo "set both EXECUTOR_DIGEST and ANALYZER_DIGEST, or neither" >&2
    exit 1
  fi
  for d in "${EXECUTOR_DIGEST}" "${ANALYZER_DIGEST}"; do
    if ! printf '%s' "${d}" | grep -Eq '^sha256:[a-f0-9]{64}$'; then
      echo "EXECUTOR_DIGEST and ANALYZER_DIGEST must be sha256: and 64 lower case hex digits" >&2
      exit 1
    fi
  done
  echo "using the given image digests"
  IMAGE_ARGS=(--set "executor.image.digest=${EXECUTOR_DIGEST}" --set "analyzer.image.digest=${ANALYZER_DIGEST}")
else
  echo "pull request or main run: using the appVersion images built from this checkout"
  IMAGE_ARGS=(--set image.allowTag=true)
fi

# On failure, show why: the Pods, their events and the logs (current and previous) of every container in the
# release namespace. helm --wait reports only "not ready".
# shellcheck disable=SC2329 # invoked by the EXIT trap below
dump() {
  local rc=$?
  if [ "${rc}" -ne 0 ]; then
    echo "::group::diagnostics of ${NS} (exit ${rc})" >&2
    "${KUBECTL}" -n "${NS}" get pods -o wide >&2 || true
    "${KUBECTL}" -n "${NS}" describe pods >&2 || true
    for p in $("${KUBECTL}" -n "${NS}" get pods -o name 2>/dev/null); do
      echo "--- logs ${p}" >&2
      "${KUBECTL}" -n "${NS}" logs "${p}" --all-containers --tail=200 >&2 || true
      echo "--- previous logs ${p}" >&2
      "${KUBECTL}" -n "${NS}" logs "${p}" --all-containers --previous --tail=200 >&2 || true
    done
    echo "::endgroup::" >&2
  fi
  return "${rc}"
}
trap dump EXIT

"${KUBECTL}" create namespace demo --dry-run=client -o yaml | "${KUBECTL}" apply -f -

"${HELM}" install "${REL}" "${CHART}" --namespace "${NS}" --create-namespace \
  --set clusterId=chart-test \
  --set "analyzer.watchNamespaces={demo}" \
  "${IMAGE_ARGS[@]}" \
  --set settings.create=true --set settings.spec.paused=false \
  --wait --timeout 5m

# settings.create=true on a fresh install: the settings object is a post-install hook because its CRD comes
# from the same release.
"${KUBECTL}" get remediationsettings.remediation.shadewright.io default

# The kill switch must survive an upgrade: the chart creates the object only when it does not exist. Set the
# pause by hand (the values say false), upgrade with settings.create=true and check it is untouched.
"${KUBECTL}" patch remediationsettings.remediation.shadewright.io default --type merge -p '{"spec":{"paused":true}}'
"${HELM}" upgrade "${REL}" "${CHART}" --namespace "${NS}" --reuse-values \
  --set settings.create=true --set settings.spec.paused=false --wait --timeout 5m
paused="$("${KUBECTL}" get remediationsettings.remediation.shadewright.io default -o jsonpath='{.spec.paused}')"
if [ "${paused}" != "true" ]; then
  echo "FAIL: helm upgrade changed the settings object: spec.paused = '${paused}', want true" >&2
  exit 1
fi

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
