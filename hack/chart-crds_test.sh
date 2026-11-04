#!/usr/bin/env bash
# Tests hack/chart-crds.sh on a small CRD: the wrapper, annotation and labels, a new CRD, a removed CRD, a CRD
# of an unexpected shape, no CRDs at all, and that the real chart is up to date.
# Run: hack/chart-crds_test.sh (or make test-scripts)
set -euo pipefail

cd "$(dirname "$0")"
fail=0
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

ok() { echo "ok: $1"; }
bad() { echo "FAIL: $1" >&2; fail=1; }

crd() { # plural
  printf -- '---\napiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\nmetadata:\n  annotations:\n    controller-gen.kubebuilder.io/version: v0\n  name: %s.x.example.io\nspec:\n  group: x.example.io\n' "$1"
}

mkdir -p "$work/src" "$work/dst"
crd foos > "$work/src/x.example.io_foos.yaml"
./chart-crds.sh "$work/src" "$work/dst" 2> /dev/null
out="$work/dst/foos.x.example.io.yaml"
want="$(printf '%s\n' '{{- if .Values.crd.enabled }}' 'apiVersion: apiextensions.k8s.io/v1' 'kind: CustomResourceDefinition' 'metadata:' \
  '  annotations:' '    {{- if .Values.crd.keep }}' '    "helm.sh/resource-policy": keep' '    {{- end }}' \
  '    controller-gen.kubebuilder.io/version: v0' '  labels:' \
  '    {{- include "shadewright.labels" (dict "component" "" "context" $) | nindent 4 }}' \
  '  name: foos.x.example.io' 'spec:' '  group: x.example.io' '{{- end }}')"
if [ "$(cat "$out")" = "$want" ]; then ok "wrapper, annotation and labels"; else bad "wrapper, annotation and labels"; fi

./chart-crds.sh "$work/src" "$work/dst" 2> /dev/null
if [ "$(cat "$out")" = "$want" ]; then ok "idempotent"; else bad "idempotent"; fi

crd bars > "$work/src/x.example.io_bars.yaml"
./chart-crds.sh "$work/src" "$work/dst" 2> /dev/null
if [ -f "$work/dst/bars.x.example.io.yaml" ]; then ok "new CRD gets a template"; else bad "new CRD gets a template"; fi

rm "$work/src/x.example.io_foos.yaml"
msg="$(./chart-crds.sh "$work/src" "$work/dst" 2>&1)"
if [ ! -e "$out" ] && grep -q "removed .*foos.x.example.io.yaml" <<<"$msg"; then ok "removed CRD is reported and its template removed"; else bad "removed CRD is reported"; fi

printf 'kind: CustomResourceDefinition\n' > "$work/src/x.example.io_odd.yaml"
if ./chart-crds.sh "$work/src" "$work/dst" > /dev/null 2>&1; then bad "unexpected shape fails"; else ok "unexpected shape fails"; fi
rm "$work/src/x.example.io_odd.yaml"
rm -f "$work/dst/odd.x.example.io.yaml"

mkdir "$work/empty"
if ./chart-crds.sh "$work/empty" "$work/dst" > /dev/null 2>&1 && [ -e "$work/dst/bars.x.example.io.yaml" ]; then bad "no CRDs fails and keeps the templates"; else ok "no CRDs fails and keeps the templates"; fi

# The real chart must match what the script generates from config/crd/bases.
mkdir "$work/real"
./chart-crds.sh "$PWD/../config/crd/bases" "$work/real" 2> /dev/null
if diff -r "$work/real" ../dist/chart/templates/crd > /dev/null; then ok "chart CRDs are up to date"; else bad "chart CRDs are stale: run make chart-crds"; fi

exit "$fail"
