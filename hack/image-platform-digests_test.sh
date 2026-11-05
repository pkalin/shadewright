#!/usr/bin/env bash
# Tests hack/image-platform-digests.sh with a stub docker that prints an index manifest: the linux children
# are printed, BuildKit attestation manifests are ignored, and a missing or duplicate platform, a non digest
# reference and missing arguments fail.
# Run: hack/image-platform-digests_test.sh (or make test-scripts)
set -euo pipefail

script="$(cd "$(dirname "$0")" && pwd)/image-platform-digests.sh"
tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT
mkdir "${tmp}/bin"
idx="sha256:$(printf 'c%.0s' $(seq 64))"
amd="sha256:$(printf 'a%.0s' $(seq 64))"
arm="sha256:$(printf 'b%.0s' $(seq 64))"
att="sha256:$(printf 'd%.0s' $(seq 64))"
ref="ghcr.io/owner/shadewright-executor@${idx}"
fail=0

check() { # name, condition result (0 ok)
  if [ "$2" != 0 ]; then echo "FAIL: $1" >&2; fail=1; else echo "ok: $1"; fi
}

cat > "${tmp}/bin/docker" <<'EOT'
#!/usr/bin/env bash
cat "${INDEX_JSON}"
EOT
chmod +x "${tmp}/bin/docker"
export PATH="${tmp}/bin:${PATH}"

entry() { printf '{"digest":"%s","platform":{"os":"%s","architecture":"%s"}}' "$1" "$2" "$3"; }
export INDEX_JSON="${tmp}/index.json"

printf '{"manifests":[%s,%s,%s]}' "$(entry "${amd}" linux amd64)" "$(entry "${arm}" linux arm64)" "$(entry "${att}" unknown unknown)" > "${INDEX_JSON}"
out="$("${script}" "${ref}")"
check "amd64 child printed" "$([ "$(sed -n 's/^amd64=//p' <<<"${out}")" = "${amd}" ] && echo 0 || echo 1)"
check "arm64 child printed" "$([ "$(sed -n 's/^arm64=//p' <<<"${out}")" = "${arm}" ] && echo 0 || echo 1)"
check "attestation manifest ignored" "$(grep -q "${att}" <<<"${out}" && echo 1 || echo 0)"

printf '{"manifests":[%s]}' "$(entry "${amd}" linux amd64)" > "${INDEX_JSON}"
r=0; "${script}" "${ref}" > /dev/null 2>&1 || r=1
check "missing arm64 refused" "$([ "${r}" = 1 ] && echo 0 || echo 1)"

printf '{"manifests":[%s,%s,%s]}' "$(entry "${amd}" linux amd64)" "$(entry "${att}" linux amd64)" "$(entry "${arm}" linux arm64)" > "${INDEX_JSON}"
r=0; "${script}" "${ref}" > /dev/null 2>&1 || r=1
check "two amd64 images refused" "$([ "${r}" = 1 ] && echo 0 || echo 1)"

printf '{"config":{}}' > "${INDEX_JSON}"
r=0; "${script}" "${ref}" > /dev/null 2>&1 || r=1
check "a single image manifest (not an index) refused" "$([ "${r}" = 1 ] && echo 0 || echo 1)"

r=0; "${script}" "ghcr.io/owner/shadewright-executor:v1" > /dev/null 2>&1 || r=1
check "a tag is refused" "$([ "${r}" = 1 ] && echo 0 || echo 1)"
r=0; "${script}" > /dev/null 2>&1 || r=1
check "missing arguments refused" "$([ "${r}" = 1 ] && echo 0 || echo 1)"
exit "${fail}"
