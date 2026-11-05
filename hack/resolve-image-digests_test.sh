#!/usr/bin/env bash
# Tests hack/resolve-image-digests.sh with stub docker and cosign: the digests printed are the ones docker
# resolved for each image, an image whose signature or attestation does not verify is never printed (the run
# fails after the wait), and a tag that is not a digest is ignored.
# Run: hack/resolve-image-digests_test.sh (or make test-scripts)
set -euo pipefail

script="$(cd "$(dirname "$0")" && pwd)/resolve-image-digests.sh"
tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT
mkdir "${tmp}/bin"
ex="sha256:$(printf 'a%.0s' $(seq 64))"
an="sha256:$(printf 'b%.0s' $(seq 64))"
cm="sha256:$(printf 'e%.0s' $(seq 64))"
cr="sha256:$(printf 'f%.0s' $(seq 64))"
fail=0

check() { # name, condition result (0 ok)
  if [ "$2" != 0 ]; then echo "FAIL: $1" >&2; fail=1; else echo "ok: $1"; fi
}

# docker prints a digest by image name (an index of two platforms for --raw); cosign fails when COSIGN_FAIL names the subcommand.
cat > "${tmp}/bin/docker" <<EOT
#!/usr/bin/env bash
case "\$*" in
  *--raw*) echo '{"manifests":[{"digest":"${cm}","platform":{"os":"linux","architecture":"amd64"}},{"digest":"${cr}","platform":{"os":"linux","architecture":"arm64"}}]}' ;;
  *shadewright-executor:*) echo "\"\${EXECUTOR_RESOLVED:-${ex}}\"" ;;
  *shadewright-analyzer:*) echo "\"${an}\"" ;;
esac
EOT
cat > "${tmp}/bin/cosign" <<'EOT'
#!/usr/bin/env bash
echo "$*" >> "${COSIGN_LOG}"
[ "$1" != "${COSIGN_FAIL:-}" ]
EOT
chmod +x "${tmp}/bin/docker" "${tmp}/bin/cosign"
# Failure cases wait WAIT_SECONDS in all, polling every POLL_SECONDS: bounded, no busy loop.
export PATH="${tmp}/bin:${PATH}" POLL_SECONDS=1 WAIT_SECONDS=1

export COSIGN_LOG="${tmp}/cosign-ok.log"
out="$("${script}" v1.2.3 Owner/shadewright Owner 2>/dev/null)"
check "executor digest printed" "$([ "$(sed -n 's/^executor=//p' <<<"${out}")" = "${ex}" ] && echo 0 || echo 1)"
check "analyzer digest printed" "$([ "$(sed -n 's/^analyzer=//p' <<<"${out}")" = "${an}" ] && echo 0 || echo 1)"
check "signature on the index, SBOM on each platform image, per image" "$([ "$(grep -c "@sha256:" "${COSIGN_LOG}")" = 6 ] && grep -q "verify-attestation.*@${cm}" "${COSIGN_LOG}" && grep -q "verify-attestation.*@${cr}" "${COSIGN_LOG}" && ! grep -q "verify-attestation.*@${ex}" "${COSIGN_LOG}" && echo 0 || echo 1)"
check "identity is the Images workflow on the tag" "$(grep -qF 'Owner/shadewright/.github/workflows/images.yml@refs/tags/v1.2.3' "${COSIGN_LOG}" && echo 0 || echo 1)"

for sub in verify verify-attestation; do
  export COSIGN_LOG="${tmp}/cosign-${sub}.log"
  r=0; out="$(COSIGN_FAIL="${sub}" "${script}" v1.2.3 Owner/shadewright Owner 2>/dev/null)" || r=1
  check "failing cosign ${sub} refused, nothing printed" "$([ "${r}" = 1 ] && [ -z "${out}" ] && echo 0 || echo 1)"
done

export COSIGN_LOG="${tmp}/cosign-nondigest.log"
r=0; EXECUTOR_RESOLVED="v1.2.3" "${script}" v1.2.3 Owner/shadewright Owner > /dev/null 2>&1 || r=1
check "a non digest is refused" "$([ "${r}" = 1 ] && echo 0 || echo 1)"
export COSIGN_LOG="${tmp}/cosign-args.log"
r=0; "${script}" v1.2.3 > /dev/null 2>&1 || r=1
check "missing arguments refused" "$([ "${r}" = 1 ] && echo 0 || echo 1)"
exit "${fail}"
