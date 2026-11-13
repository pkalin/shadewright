#!/usr/bin/env bash
# Adds the digests of a release and the commands that verify them to the body of its GitHub release (the
# release-notes job of the Chart workflow, #84). Usage:
#   hack/release-verify-notes.sh <owner/repo> <tag> <executor digest> <analyzer digest> <verifier digest> <chart digest> < body > new body
# The section sits between two HTML comment markers. A body that already has it gets it replaced, so a rerun
# of the job leaves exactly one copy; anything else in the body is kept as it is. The identities are pinned to
# the tag, so a signature of another tag or branch does not verify. Digests must be sha256: and 64 lower case
# hex digits; the repository, the tag and the digests are validated before they reach the text.
set -euo pipefail

if [ "$#" -ne 6 ]; then
  echo "usage: $0 <owner/repo> <tag> <executor digest> <analyzer digest> <verifier digest> <chart digest>" >&2
  exit 2
fi
repo="$1" tag="$2" executor="$3" analyzer="$4" verifier="$5" chart="$6"

printf '%s' "${repo}" | grep -Eq '^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$' || { echo "bad repository '${repo}'" >&2; exit 1; }
printf '%s' "${tag}" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$' || { echo "bad tag '${tag}'" >&2; exit 1; }
for d in "${executor}" "${analyzer}" "${verifier}" "${chart}"; do
  printf '%s' "${d}" | grep -Eq '^sha256:[a-f0-9]{64}$' || { echo "not a digest: '${d}'" >&2; exit 1; }
done

owner="$(printf '%s' "${repo%%/*}" | tr '[:upper:]' '[:lower:]')"
repo_re="${repo//./\\.}"
tag_re="${tag//./\\.}"
version="${tag#v}"
start='<!-- shadewright:verify:start -->'
end='<!-- shadewright:verify:end -->'

pre=""
if [ "${version}" != "${version%%-*}" ]; then
  pre="This is a pre-release: it is not the latest release and its images and chart are tagged \`${version}\` only (no \`latest\`, no \`X.Y\` tag). Install by digest as below: Helm does not pick pre-releases by itself.

"
fi

section="$(cat <<TEXT
${start}
## Artifacts and verification

${pre}| Artifact | Reference |
| --- | --- |
| Executor image | \`ghcr.io/${owner}/shadewright-executor@${executor}\` |
| Analyzer image | \`ghcr.io/${owner}/shadewright-analyzer@${analyzer}\` |
| Verifier image | \`ghcr.io/${owner}/shadewright-verifier@${verifier}\` |
| Helm chart | \`oci://ghcr.io/${owner}/charts/shadewright\` version \`${version}\`, \`${chart}\` |

The release workflow checked the keyless cosign signature of all three images and the signed SPDX SBOM of each platform image (linux/amd64 and linux/arm64), and the chart runs exactly those images by digest. The Images workflow also records a GitHub build provenance attestation, and the chart is signed keyless. Verify by digest, never by tag; the commands below check all of it again. You need [Docker](https://docs.docker.com/engine/install/), jq, [cosign](https://docs.sigstore.dev/cosign/system_config/installation/) 3.x or later (the signatures are stored as Sigstore bundles, which cosign 2.x does not find) and the [GitHub CLI](https://cli.github.com/). Details: [verify a release](https://shadewright.io/upgrade-uninstall/#verify-a-release).

\`\`\`bash
ISSUER=https://token.actions.githubusercontent.com
IMAGES_IDENTITY='^https://github\.com/${repo_re}/\.github/workflows/images\.yml@refs/tags/${tag_re}\$'
CHART_IDENTITY='^https://github\.com/${repo_re}/\.github/workflows/chart\.yml@refs/tags/${tag_re}\$'

for ref in \\
  ghcr.io/${owner}/shadewright-executor@${executor} \\
  ghcr.io/${owner}/shadewright-analyzer@${analyzer} \\
  ghcr.io/${owner}/shadewright-verifier@${verifier}; do
  cosign verify --certificate-identity-regexp "\$IMAGES_IDENTITY" --certificate-oidc-issuer "\$ISSUER" "\$ref"
  # One SBOM per platform image, attested on the digest of each (needs docker and jq).
  for arch in amd64 arm64; do
    child="\$(docker buildx imagetools inspect --raw "\$ref" | jq -r --arg a "\$arch" \\
      '.manifests[] | select(.platform.os == "linux" and .platform.architecture == \$a) | .digest')"
    cosign verify-attestation --type spdxjson --certificate-identity-regexp "\$IMAGES_IDENTITY" \\
      --certificate-oidc-issuer "\$ISSUER" "\${ref%@*}@\$child" > /dev/null
  done
  gh attestation verify "oci://\$ref" --repo ${repo} \\
    --signer-workflow ${repo}/.github/workflows/images.yml
done

cosign verify --certificate-identity-regexp "\$CHART_IDENTITY" --certificate-oidc-issuer "\$ISSUER" \\
  ghcr.io/${owner}/charts/shadewright@${chart}

# Install the chart you verified: Helm accepts the digest in the reference, so nothing can move in between.
helm install shadewright oci://ghcr.io/${owner}/charts/shadewright@${chart} \\
  --namespace shadewright-system --create-namespace
\`\`\`
${end}
TEXT
)"

body="$(cat)"
# Drop an earlier copy of the section (markers included), then append the current one.
body="$(printf '%s\n' "${body}" | awk -v s="${start}" -v e="${end}" '
  { line = $0; sub(/\r$/, "", line) }
  line == s { skip = 1; next }
  skip && line == e { skip = 0; next }
  !skip { print }
')"
# Trim trailing blank lines.
body="$(printf '%s' "${body}" | sed -e :a -e '/^[[:space:]]*$/{$d;N;ba' -e '}')"
if [ -n "${body}" ]; then
  printf '%s\n\n%s\n' "${body}" "${section}"
else
  printf '%s\n' "${section}"
fi
