#!/usr/bin/env bash
# Fails when kubectl-shadewright links a package of the operator's server-side stack, or when its stripped
# linux/amd64 binary grows past the ceiling below (#756). The plugin is a read-only client: client-go's dynamic and
# metadata clients, clientcmd and its auth plugins, the API types it reads and its own code. Raising the ceiling
# or allowing a denied package is a decision for a pull request to state, not a side effect of an import.
# Usage: hack/check-plugin-deps.sh    (run by make lint)
# Environment, for hack/check-plugin-deps_test.sh only:
#   PLUGIN_DEPS_FILE  a file with the package list to check instead of `go list -deps`
#   PLUGIN_SIZE       the binary size in bytes to check instead of building it
set -euo pipefail

cd "$(dirname "$0")/.."

export GOFLAGS=-mod=readonly
export GOWORK=off

pkg=./cmd/kubectl-shadewright
module="$(go list -m)"

# The stripped linux/amd64 binary measured 16,015,520 bytes when this check was added (#756); the ceiling is about
# 10% above it.
max_bytes=17600000

# Packages (and every package below them) the plugin must not link.
denied=(
  sigs.k8s.io/controller-runtime
  k8s.io/apiserver
  k8s.io/client-go/informers
  k8s.io/client-go/kubernetes
  google.golang.org/grpc
  github.com/google/cel-go
  go.opentelemetry.io
  github.com/prometheus/client_golang
  "${module}/internal/analyzer"
)
# Packages the plugin must not link, though packages below them are allowed (the leaf packages it reads).
denied_exact=(
  "${module}/internal/incidents"
  "${module}/internal/gitops/sourcemap"
  "${module}/internal/cmdutil"
)

if [ -n "${PLUGIN_DEPS_FILE:-}" ]; then
  deps="$(cat "${PLUGIN_DEPS_FILE}")"
else
  deps="$(CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go list -deps "${pkg}")"
fi

fail=0
for d in "${denied[@]}"; do
  if found="$(printf '%s\n' "${deps}" | grep -E "^${d//./\\.}(/|$)")"; then
    echo "error: ${pkg} links ${d}:" >&2
    printf '%s\n' "${found}" | sed 's/^/  /' >&2
    fail=1
  fi
done
for d in "${denied_exact[@]}"; do
  if printf '%s\n' "${deps}" | grep -Fxq "${d}"; then
    echo "error: ${pkg} links ${d}" >&2
    fail=1
  fi
done

if [ -n "${PLUGIN_SIZE:-}" ]; then
  size="${PLUGIN_SIZE}"
else
  tmp="$(mktemp -d)"
  trap 'rm -rf "${tmp}"' EXIT
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o "${tmp}/kubectl-shadewright" "${pkg}"
  size="$(wc -c < "${tmp}/kubectl-shadewright" | tr -d ' ')"
fi
if [ "${size}" -gt "${max_bytes}" ]; then
  echo "error: the stripped linux/amd64 kubectl-shadewright is ${size} bytes, above the ceiling of ${max_bytes}" \
    "(hack/check-plugin-deps.sh); find the import that grew it, or raise the ceiling in the pull request and say why" >&2
  fail=1
fi

if [ "${fail}" -ne 0 ]; then
  exit 1
fi
echo "kubectl-shadewright: $(printf '%s\n' "${deps}" | wc -l | tr -d ' ') packages, none denied; ${size} bytes stripped" \
  "(linux/amd64), ceiling ${max_bytes}"
