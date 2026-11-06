#!/usr/bin/env bash
# Offline release gate regression tests. The gh stub never connects to GitHub.
set -euo pipefail
cd "$(dirname "$0")/.."
tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT
export FIXTURE="${tmp}/release.json" CALLS="${tmp}/calls" RELEASE_WAIT_SECONDS=0
mkdir "${tmp}/bin"
cat > "${tmp}/bin/gh" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "${CALLS}"
case "$1 $2" in
  'release view') cat "${FIXTURE}" ;;
  'release edit') [ "${FAIL_EDIT:-false}" = false ] ;;
  *) exit 2 ;;
esac
STUB
chmod +x "${tmp}/bin/gh"
export PATH="${tmp}/bin:${PATH}"
d="sha256:$(printf '1%.0s' {1..64})"
fixture() {
  local tag="$1"
  hack/release-verify-notes.sh pkalin/shadewright "${tag}" "$d" "$d" "$d" < /dev/null > "${tmp}/notes"
  jq -n --arg tag "${tag}" --rawfile body "${tmp}/notes" '
    {tagName: $tag, isDraft: true, body: $body, assets:
      [(["linux", "darwin"][] as $o | ["amd64", "arm64"][] as $a |
        {name: "kubectl-shadewright_\($tag | ltrimstr("v"))_\($o)_\($a).tar.gz", state: "uploaded", size: 10}),
       {name: "checksums.txt", state: "uploaded", size: 10},
       {name: "checksums.txt.sigstore.json", state: "uploaded", size: 10}]}' > "${FIXTURE}"
  cp "${FIXTURE}" "${tmp}/complete.json"
}
run_gate() {
  : > "${CALLS}"
  hack/publish-release.sh pkalin/shadewright "$1" "$d" "$d" "$d" > "${tmp}/out" 2>&1
}
refuse() {
  if run_gate v0.2.0; then echo "FAIL: accepted $1" >&2; exit 1; fi
  if grep -q '^release edit' "${CALLS}"; then echo "FAIL: published $1" >&2; exit 1; fi
  echo "ok: refused $1"
}
for tag in v0.2.0 v0.2.0-rc.1; do
  fixture "${tag}"
  run_gate "${tag}"
  grep -q -- '--draft=false' "${CALLS}"
  echo "ok: complete ${tag} published"
done
fixture v0.2.0
for i in 0 1 2 3 4 5; do
  jq --argjson i "$i" 'del(.assets[$i])' "${tmp}/complete.json" > "${FIXTURE}"
  refuse "missing asset $i"
done
for filter in '.assets[0].size = 0' '.assets[0].state = "new"' '.body = ""' \
  '.body |= gsub("sha256:1"; "sha256:2")' '.tagName = "v0.1.0"' 'del(.isDraft)'; do
  jq "${filter}" "${tmp}/complete.json" > "${FIXTURE}"
  refuse "${filter}"
done
printf 'invalid JSON' > "${FIXTURE}"
refuse 'API response invalid'
rm "${FIXTURE}"
refuse 'release not found'
cp "${tmp}/complete.json" "${FIXTURE}"
export FAIL_EDIT=true
if run_gate v0.2.0; then echo 'FAIL: publish API failure ignored' >&2; exit 1; fi
unset FAIL_EDIT
jq '.isDraft = false' "${tmp}/complete.json" > "${FIXTURE}"
run_gate v0.2.0
if grep -q '^release edit' "${CALLS}"; then echo 'FAIL: edited already published release' >&2; exit 1; fi
echo 'ok: complete published release is a no-op'
# The old configuration published immediately, reproducing the immutable upload failure.
python3 - <<'PY'
import json
from pathlib import Path
config = json.loads(Path('release-please-config.json').read_text())['packages']['.']
assert config['draft'] is True
assert config['force-tag-creation'] is True
chart = Path('.github/workflows/chart.yml').read_text()
assert 'needs: [publish, release-notes]' in chart
assert 'run: hack/publish-release.sh' in chart
for workflow in ['images', 'chart', 'release-plugin', 'trivy']:
    assert 'tags: ["v*"]' in Path(f'.github/workflows/{workflow}.yml').read_text()
print('ok: draft/tag configuration and publish gate wired into tag workflows')
PY
