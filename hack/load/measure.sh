#!/usr/bin/env bash
# Measures the peak memory and the time of the analyzer and the executor under the supported load (design 3.1
# and 8.2, issue #254) on a local Kind cluster, and prints a report. Repeatable: it makes its own Kind cluster
# with the pinned Kind (make kind), builds both images from this checkout and loads them (make
# chart-ct-cluster, the target of the chart-testing job), installs the chart, creates the load, samples
# memory, and deletes the cluster. It points KUBECONFIG at a file of its own, so no other context is touched.
#
# Usage:  hack/load/measure.sh            (about 20 minutes: 10 for the load, 8 of sampling)
# Needs:  docker, go, helm, kubectl, jq, curl.
# Knobs (environment, defaults are the supported load of the design):
#   NAMESPACES=20      watched namespaces (--watch-namespaces)
#   PODS=1000          Pods in them, 40 of which run; the others stay Pending (unschedulable), see below
#   EVENTS=10000       events.k8s.io/v1 Events already in the cluster, 10% Warning (about an hour at 170 a minute)
#   STREAM_PER_MIN=200 Events a minute while sampling, 10% Warning
#   STREAM_MINUTES=5   length of that stream
#   BURST=1000         Events created as fast as the client allows after the stream (a failure storm)
#   KEEP=              set to 1 to keep the cluster and the work directory
#   CLUSTER=shadewright-load
#   WORK=              directory for the samples (default: a new temporary one)
#
# What it measures, per component (analyzer, executor):
#   - working set: the kubelet's container working set (the number the OOM killer's limit is compared with
#     by kubectl top), sampled every 5 s, and the cgroup's own memory.peak after the run (an exact peak
#     since the container started, page cache included, so it is the larger number);
#   - RSS and Go heap in use, from /metrics (process_resident_memory_bytes, go_memstats_heap_inuse_bytes);
#   - time to Ready after a restart over the full load (the initial LIST of every informer), and the
#     reconcile time of the collector, from controller_runtime_reconcile_time_seconds.
# The executor needs a GitHub App, which a Kind cluster does not have, so its pull request path does not run
# here. Its worst-case render is measured by hack/load/render in a container with the executor's memory limit
# (RENDER=0 to skip). The model call is off too (no triageModel), so the analysis context is not measured.
#
# The bulk Pods are Pending, never scheduled (a node selector nothing matches): one node cannot run a
# thousand Pods, and the analyzer's cache holds the object either way. A running Pod's status is a little
# larger (container statuses), so the Pod share of the cache is slightly understated.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

NAMESPACES="${NAMESPACES:-20}"
PODS="${PODS:-1000}"
EVENTS="${EVENTS:-10000}"
STREAM_PER_MIN="${STREAM_PER_MIN:-200}"
STREAM_MINUTES="${STREAM_MINUTES:-5}"
BURST="${BURST:-1000}"
CLUSTER="${CLUSTER:-shadewright-load}"
RENDER="${RENDER:-1}"
WORK="${WORK:-$(mktemp -d)}"
NS=shadewright-system
REL=shadewright
CTX="kind-${CLUSTER}"
export GOTOOLCHAIN="${GOTOOLCHAIN:-go1.27.4}"
export KUBECONFIG="${WORK}/kubeconfig"

make kind >/dev/null
KIND="${ROOT}/bin/kind"
k() { kubectl --context "${CTX}" "$@"; }

# alpine 3.22, pinned by digest: the render runs in it with the executor's limits.
ALPINE="alpine:3.22@sha256:bfe7ceb9f09ec0566c48fb93356e45a47a00be5a1dd14fa5b909e7709c66b034"

# Refuse to start when a cluster of that name exists: the cleanup below would delete it, and kind would not
# write its credentials into the fresh kubeconfig.
if "${KIND}" get clusters 2>/dev/null | grep -qx "${CLUSTER}"; then
  echo "a kind cluster named ${CLUSTER} exists already; delete it (${KIND} delete cluster --name ${CLUSTER}) or set CLUSTER" >&2
  exit 1
fi

SAMPLER=""
cleanup() {
  [ -n "${SAMPLER}" ] && kill "${SAMPLER}" 2>/dev/null || true
  if [ "${KEEP:-}" = 1 ]; then
    echo "kept: cluster ${CLUSTER}, KUBECONFIG=${KUBECONFIG}, samples in ${WORK}"
  else
    "${KIND}" delete cluster --name "${CLUSTER}" >/dev/null 2>&1 || true
    rm -rf "${WORK}/gen" "${WORK}/kubeconfig"
  fi
}
trap cleanup EXIT

echo "== cluster and images (make chart-ct-cluster)"
make chart-ct-cluster CHART_KIND_CLUSTER="${CLUSTER}" CHART_CT_KIND_CLUSTER="${CLUSTER}" >"${WORK}/cluster.log" 2>&1 \
  || { tail -20 "${WORK}/cluster.log"; exit 1; }
NODE="${CLUSTER}-control-plane"
K8S_VERSION="$(k version -o json | jq -r .serverVersion.gitVersion)"
KIND_VERSION="$("${KIND}" version | awk '{print $2}')"
COMMIT="$(git rev-parse HEAD)"
echo "commit ${COMMIT}, Kind ${KIND_VERSION}, Kubernetes ${K8S_VERSION}"

GEN="${WORK}/gen"
go build -o "${GEN}" ./hack/load/gen
PAUSE="$(docker exec "${NODE}" crictl images 2>/dev/null | awk '$1 ~ /pause$/ {print $1":"$2; exit}')"
PAUSE="${PAUSE:-registry.k8s.io/pause:3.10}"

echo "== namespaces and chart"
"${GEN}" namespaces -namespaces "${NAMESPACES}"
watch="$(seq 0 $((NAMESPACES - 1)) | sed 's/^/load-/' | paste -sd, -)"
# The metrics endpoint is plain HTTP (a test only) and the NetworkPolicies are off, so the samples can read
# /metrics through the API server's Pod proxy. Neither changes what the processes hold in memory.
helm install "${REL}" dist/chart --namespace "${NS}" --create-namespace --kube-context "${CTX}" \
  -f dist/chart/ci/default-values.yaml --set "analyzer.watchNamespaces={${watch}}" \
  --set metrics.secure=false --set networkPolicy.enabled=false --wait --timeout 5m >/dev/null 2>&1

pod_of() { k -n "${NS}" get pods -l "app.kubernetes.io/component=$1" -o jsonpath='{.items[0].metadata.name}'; }

echo "== idle sample before the load"
sleep 30

# --- sampling ----------------------------------------------------------------------------------------------
echo "none" >"${WORK}/phase"
: >"${WORK}/samples.tsv"
scrape() { # <component> -> "<rss> <heap>" in bytes from /metrics, or "0 0"
  local pod body
  pod="$(pod_of "$1" 2>/dev/null)" || { echo "0 0"; return; }
  body="$(k get --raw "/api/v1/namespaces/${NS}/pods/http:${pod}:8443/proxy/metrics" 2>/dev/null)" || { echo "0 0"; return; }
  awk '/^process_resident_memory_bytes /{r=$2} /^go_memstats_heap_inuse_bytes /{h=$2} END{printf "%.0f %.0f\n", r, h}' <<<"${body}"
}
sampler() {
  while :; do
    local summary ts phase
    summary="$(k get --raw "/api/v1/nodes/${NODE}/proxy/stats/summary" 2>/dev/null || true)"
    ts="$(date +%s)"
    phase="$(cat "${WORK}/phase")"
    for c in analyzer executor; do
      local ws pod
      pod="$(pod_of "${c}" 2>/dev/null || true)"
      ws="$(jq -r --arg p "${pod}" '[.pods[]|select(.podRef.name==$p)|.containers[]|.memory.workingSetBytes]|first // 0' <<<"${summary:-{\}}" 2>/dev/null || echo 0)"
      printf '%s\t%s\t%s\t%s\t%s\n' "${ts}" "${phase}" "${c}" "${ws}" "$(scrape "${c}")" >>"${WORK}/samples.tsv"
    done
    sleep 5
  done
}
sampler &
SAMPLER=$!
phase() { echo "$1" >"${WORK}/phase"; echo "== $1"; }

phase idle
sleep 30

phase load
"${GEN}" pods -namespaces "${NAMESPACES}" -pods "${PODS}" -running 40 -image "${PAUSE}"
"${GEN}" events -namespaces "${NAMESPACES}" -pods "${PODS}" -count "${EVENTS}" -warning-ratio 0.1
echo "events in the cluster: $(k get events.events.k8s.io -A --no-headers 2>/dev/null | wc -l | tr -d ' '), pods: $(k get pods -A --no-headers | wc -l | tr -d ' ')"

# Restart over the full load: the initial LIST of every informer is the largest allocation of a start.
phase restart
restart_time() { # <component> -> seconds to a Ready rollout
  local t0 t1
  t0="$(date +%s)"
  k -n "${NS}" rollout restart "deployment/${REL}-$1" >/dev/null
  k -n "${NS}" rollout status "deployment/${REL}-$1" --timeout 5m >/dev/null
  t1="$(date +%s)"
  echo $((t1 - t0))
}
ANALYZER_READY="$(restart_time analyzer)"
EXECUTOR_READY="$(restart_time executor)"
sleep 60

phase stream
"${GEN}" stream -namespaces "${NAMESPACES}" -pods "${PODS}" -per-minute "${STREAM_PER_MIN}" -minutes "${STREAM_MINUTES}"
sleep 20

phase burst
"${GEN}" events -namespaces "${NAMESPACES}" -pods "${PODS}" -count "${BURST}" -warning-ratio 0.1
sleep 90

phase settle
sleep 60
kill "${SAMPLER}"; SAMPLER=""

# --- results -------------------------------------------------------------------------------------------------
mib() { awk -v b="$1" 'BEGIN{printf "%.0f", b/1048576}'; }
peak_cgroup() { # <component> -> bytes of memory.peak of the running container
  local id
  id="$(k -n "${NS}" get pod "$(pod_of "$1")" -o jsonpath='{.status.containerStatuses[0].containerID}' | sed 's#.*://##')"
  docker exec "${NODE}" sh -c "cat \$(find /sys/fs/cgroup -name memory.peak -path \"*${id}*\" | head -1)" 2>/dev/null || echo 0
}
echo
echo "== report (commit ${COMMIT}, Kind ${KIND_VERSION}, Kubernetes ${K8S_VERSION}, $(uname -m) host)"
echo "load: ${NAMESPACES} namespaces, ${PODS} Pods (40 running), ${EVENTS} Events at start (10% Warning) + ${STREAM_PER_MIN}/min for ${STREAM_MINUTES} min + burst of ${BURST}; the limit is 128Mi"
echo
printf '%-9s %-9s %12s %10s %10s\n' component phase "workingset_max" "rss_max" "heap_max"
for c in analyzer executor; do
  for p in idle load restart stream burst settle; do
    awk -F'[\t ]' -v c="${c}" -v p="${p}" '$3==c && $2==p {if($4>w)w=$4; if($5>r)r=$5; if($6>h)h=$6; n++}
      END{if(n) printf "%-9s %-9s %11.0fMi %9.0fMi %9.0fMi\n", c, p, w/1048576, r/1048576, h/1048576}' "${WORK}/samples.tsv"
  done
done
echo
for c in analyzer executor; do
  echo "${c}: cgroup memory.peak since the last restart $(mib "$(peak_cgroup "${c}")")Mi, restarts $(k -n "${NS}" get pods -l "app.kubernetes.io/component=${c}" -o jsonpath='{.items[0].status.containerStatuses[0].restartCount}'), OOMKilled: $(k -n "${NS}" get pods -l "app.kubernetes.io/component=${c}" -o jsonpath='{.items[0].status.containerStatuses[0].lastState.terminated.reason}')"
done
echo "time to Ready after a restart over the full load: analyzer ${ANALYZER_READY}s, executor ${EXECUTOR_READY}s (includes the old Pod's shutdown)"
echo "incidents: $(k -n "${NS}" get incidents.remediation.shadewright.io --no-headers 2>/dev/null | wc -l | tr -d ' ')"
body="$(k get --raw "/api/v1/namespaces/${NS}/pods/http:$(pod_of analyzer):8443/proxy/metrics")"
for ctl in collector-events collector-pods; do
  awk -v c="${ctl}" '$0 ~ "^controller_runtime_reconcile_time_seconds_(sum|count)\\{controller=\""c"\"" {if($0 ~ /_sum/)s+=$NF; else n+=$NF}
    END{if(n) printf "analyzer %s: %d reconciles, mean %.2f ms\n", c, n, 1000*s/n}' <<<"${body}"
done

if [ "${RENDER}" = 1 ]; then
  echo
  echo "== worst-case render in a container limited to 512 MiB and 500m CPU (the executor's limits)"
  mkdir -p "${WORK}/render"
  CGO_ENABLED=0 GOOS=linux GOARCH="$(go env GOARCH)" go build -o "${WORK}/render/render" ./hack/load/render
  # At the build budget (4), just over it (5, refused before the render), the old 16 MiB budget (15, refused),
  # 1 and 4 MiB of tiny objects (refused before the render by the document bound), and the document bound of
  # 4,096: tiny objects (docs), padded to the byte budget too (heavy), and one document over it (docs 4096,
  # refused before the render) (design 8.2.2, #488). Untargeted patches at the transform budget (patches 1708),
  # and at the document bound (patches 3072, refused before the render since #626). The build budget with an
  # annotation, an image override or a value of many small nodes at the transform budget (100 percent), and just
  # over it (101, refused before the render) (#495). A value moved into a name copied into 1,000 references,
  # past the estimates: 16 KiB renders, 64 KiB is stopped by the render process's limits (#614, ADR-0035).
  for args in "flat 4" "diamond 4" "flat 5" "diamond 5" "flat 15" "diamond 15" "tiny 1" "tiny 4" \
    "docs 4095" "heavy 4095" "patches 1708" "patches 3072" "docs 4096" "annotations 100" "annotations 101" "images 100" \
    "images 101" "dense 100" "dense 101" "denseseq 100" "denseannotations 100" "chained 16" "chained 64"; do
    out="$(docker run --rm --memory=512m --memory-swap=512m --cpus=0.5 -v "${WORK}/render:/r:ro" "${ALPINE}" \
      sh -c "/r/render ${args}; echo exit=\$?" 2>&1 | tail -2 | tr '\n' ' ')"
    echo "${args}: ${out}"
  done
fi
echo
echo "samples: ${WORK}/samples.tsv (epoch, phase, component, working set, rss, heap in use)"
