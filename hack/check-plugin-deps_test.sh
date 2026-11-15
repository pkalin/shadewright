#!/usr/bin/env bash
# Tests hack/check-plugin-deps.sh with given package lists and sizes: a client-go only list under the ceiling
# passes; a denied package, a package below a denied one, a denied package itself (not its leaf packages) or a
# binary over the ceiling fails.
# Run: hack/check-plugin-deps_test.sh (or make test-scripts)
set -euo pipefail

script="$(cd "$(dirname "$0")" && pwd)/check-plugin-deps.sh"
module="$(cd "$(dirname "$0")/.." && go list -m)"
tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT
fail=0

base="k8s.io/client-go/dynamic
k8s.io/client-go/metadata
k8s.io/client-go/tools/clientcmd
${module}/internal/incidents/analysisdue
${module}/internal/gitops/sourcemap/fluxrules
${module}/internal/cmdutil/sharedflags
${module}/cmd/kubectl-shadewright"

check() { # pass|refuse, name, size, extra package
  local want="$1" name="$2" size="$3" extra="$4"
  printf '%s\n%s\n' "${base}" "${extra}" | sed '/^$/d' > "${tmp}/deps"
  if PLUGIN_DEPS_FILE="${tmp}/deps" PLUGIN_SIZE="${size}" "${script}" > /dev/null 2>&1; then
    got=pass
  else
    got=refuse
  fi
  if [ "${got}" = "${want}" ]; then echo "ok: ${name}"; else echo "FAIL: ${name}: ${got}, want ${want}" >&2; fail=1; fi
}

check pass "client-go and leaf packages" 16000000 ""
check pass "at the ceiling" 17600000 ""
check refuse "over the ceiling" 17600001 ""
check refuse "controller-runtime client" 16000000 sigs.k8s.io/controller-runtime/pkg/client
check refuse "controller-runtime root" 16000000 sigs.k8s.io/controller-runtime
check refuse "typed clientset" 16000000 k8s.io/client-go/kubernetes
check refuse "client-go scheme" 16000000 k8s.io/client-go/kubernetes/scheme
check refuse "informers" 16000000 k8s.io/client-go/informers/core/v1
check refuse "apiserver" 16000000 k8s.io/apiserver/pkg/server
check refuse "grpc" 16000000 google.golang.org/grpc
check refuse "cel-go" 16000000 github.com/google/cel-go/cel
check refuse "opentelemetry" 16000000 go.opentelemetry.io/otel
check refuse "prometheus client" 16000000 github.com/prometheus/client_golang/prometheus
check refuse "analyzer" 16000000 "${module}/internal/analyzer"
check refuse "incidents" 16000000 "${module}/internal/incidents"
check refuse "sourcemap" 16000000 "${module}/internal/gitops/sourcemap"
check refuse "cmdutil" 16000000 "${module}/internal/cmdutil"
check pass "a name that only starts like a denied one" 16000000 k8s.io/client-go/kubernetesfoo

exit "${fail}"
