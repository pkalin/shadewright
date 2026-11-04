#!/usr/bin/env bash
# Copies the CRDs controller-gen generates (config/crd/bases) into the Helm chart's templates, adding what the
# chart needs: the crd.enabled wrapper, the helm.sh/resource-policy: keep annotation under crd.keep, and the
# chart labels. A CRD with no template yet gets one; a template whose CRD is gone is removed and reported.
# Usage: hack/chart-crds.sh [SRC_DIR [DST_DIR]]   (make chart-crds; also run by make manifests)
set -euo pipefail

cd "$(dirname "$0")/.."
src="${1:-config/crd/bases}"
dst="${2:-dist/chart/templates/crd}"

shopt -s nullglob
sources=("$src"/*.yaml)
if [ "${#sources[@]}" -eq 0 ]; then
  echo "error: no CRDs in $src: run 'make manifests' first" >&2
  exit 1
fi
mkdir -p "$dst"

wanted=()
for f in "${sources[@]}"; do
  base="$(basename "$f" .yaml)"
  case "$base" in
    *_*) ;;
    *) echo "error: $f is not named <group>_<plural>.yaml" >&2; exit 1 ;;
  esac
  out="${base#*_}.${base%%_*}.yaml" # <group>_<plural> -> <plural>.<group>
  tmp="$(mktemp)"
  if ! awk '
    NR == 1 { if ($0 != "---") bad = "does not start with ---"; print "{{- if .Values.crd.enabled }}"; next }
    $0 == "  annotations:" && !ann {
      ann = 1; print
      print "    {{- if .Values.crd.keep }}"
      print "    \"helm.sh/resource-policy\": keep"
      print "    {{- end }}"
      next
    }
    $0 ~ /^  name: / && !lab {
      lab = 1
      print "  labels:"
      print "    {{- include \"shadewright.labels\" (dict \"component\" \"\" \"context\" $) | nindent 4 }}"
    }
    { print }
    END {
      if (!ann) bad = "has no metadata.annotations"
      if (!lab) bad = "has no metadata.name"
      if (bad != "") { print "error: " FILENAME " " bad > "/dev/stderr"; exit 1 }
      print "{{- end }}"
    }' "$f" > "$tmp"; then
    rm -f "$tmp"
    exit 1
  fi
  mv "$tmp" "$dst/$out"
  chmod 0644 "$dst/$out"
  wanted+=("$out")
done

for t in "$dst"/*.yaml; do
  name="$(basename "$t")"
  keep=0
  for w in "${wanted[@]}"; do [ "$w" = "$name" ] && keep=1; done
  if [ "$keep" = 0 ]; then
    rm -f "$t"
    echo "removed $t: its CRD is no longer in $src" >&2
  fi
done
