#!/usr/bin/env bash
# Tests hack/release-verify-notes.sh and hack/chart-release-version.sh (the release steps of the Chart
# workflow, #84): the section is appended once and replaced on a rerun, the identities are pinned to the tag,
# bad input is refused, and a pre-release tag packages from the tag while a plain tag must equal Chart.yaml.
# Run: hack/release-verify-notes_test.sh (or make test-scripts)
set -euo pipefail

cd "$(dirname "$0")"
fail=0
tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT

ok() { echo "ok: $1"; }
bad() { echo "FAIL: $1" >&2; fail=1; }

d1="sha256:$(printf '1%.0s' {1..64})"
d2="sha256:$(printf '2%.0s' {1..64})"
d3="sha256:$(printf '3%.0s' {1..64})"
d4="sha256:$(printf '4%.0s' {1..64})"
notes() { ./release-verify-notes.sh pkalin/shadewright "$1" "$d1" "$d2" "$d4" "$d3"; }
original=$'## 0.2.0 (2027-01-01)\n\n* feat: something\n'

# Appended after the original body, which is kept.
printf '%s' "${original}" | notes v0.2.0 > "${tmp}/one.md"
head -n 3 "${tmp}/one.md" | diff -q - <(printf '%s' "${original}") > /dev/null && ok "body kept" || bad "body kept"
grep -qF "shadewright-executor@${d1}" "${tmp}/one.md" && grep -qF "shadewright-analyzer@${d2}" "${tmp}/one.md" &&
  grep -qF "shadewright-verifier@${d4}" "${tmp}/one.md" &&
  grep -qF "charts/shadewright@${d3}" "${tmp}/one.md" && ok "digests" || bad "digests"
grep -qF 'images\.yml@refs/tags/v0\.2\.0$' "${tmp}/one.md" && grep -qF 'chart\.yml@refs/tags/v0\.2\.0$' "${tmp}/one.md" &&
  ok "identities pinned to the tag" || bad "identities pinned to the tag"
grep -qF 'pre-release' "${tmp}/one.md" && bad "plain release says pre-release" || ok "plain release"

# A rerun replaces the section: the same output, one copy.
notes v0.2.0 < "${tmp}/one.md" > "${tmp}/two.md"
diff -u "${tmp}/one.md" "${tmp}/two.md" && ok "idempotent" || bad "idempotent"
[ "$(grep -c 'shadewright:verify:start' "${tmp}/two.md")" = 1 ] && ok "one section" || bad "one section"

# New digests on a rerun replace the old ones.
./release-verify-notes.sh pkalin/shadewright v0.2.0 "${d3}" "${d3}" "${d3}" "${d3}" < "${tmp}/one.md" > "${tmp}/three.md"
grep -qF "${d1}" "${tmp}/three.md" || grep -qF "${d4}" "${tmp}/three.md" && bad "old digest left" || ok "old digest replaced"

# A body edited in the web UI has CRLF line endings: the markers are still found, so a rerun replaces.
sed 's/$/\r/' "${tmp}/one.md" > "${tmp}/crlf.md"
notes v0.2.0 < "${tmp}/crlf.md" > "${tmp}/crlf2.md"
[ "$(grep -c 'shadewright:verify:start' "${tmp}/crlf2.md")" = 1 ] && ok "CRLF rerun replaces" || bad "CRLF rerun duplicated the section"

# An empty body and a pre-release tag.
notes v0.2.0-rc.1 < /dev/null > "${tmp}/rc.md"
grep -qF 'pre-release' "${tmp}/rc.md" && grep -qF 'tags/v0\.2\.0-rc\.1$' "${tmp}/rc.md" && grep -qF -- "charts/shadewright@${d3}" "${tmp}/rc.md" && ! grep -qF -- "--version" "${tmp}/rc.md" &&
  ok "pre-release" || bad "pre-release"
[ "$(head -n 1 "${tmp}/rc.md")" = '<!-- shadewright:verify:start -->' ] && ok "empty body" || bad "empty body"

# Bad input is refused.
for args in "pkalin/shadewright main ${d1} ${d2} ${d4} ${d3}" "pkalin/shadewright v1.0.0 sha256:abc ${d2} ${d4} ${d3}" "pkalin/shadewright v1.0.0 ${d1} ${d2} bad ${d3}" "pkalin shadewright v1.0.0 ${d1} ${d2} ${d4} ${d3}"; do
  # shellcheck disable=SC2086
  if echo body | ./release-verify-notes.sh ${args} > /dev/null 2>&1; then bad "accepted: ${args}"; else ok "refused: ${args%% sha256*}"; fi
done

# Chart version from the tag.
chart="${tmp}/Chart.yaml"
printf 'version: 0.1.0  # x-release-please-version\nappVersion: "0.1.0"  # x-release-please-version\n' > "${chart}"
[ "$(./chart-release-version.sh v0.1.0 "${chart}")" = $'version=0.1.0\nprerelease=false' ] && ok "plain tag equal" || bad "plain tag equal"
if ./chart-release-version.sh v0.2.0 "${chart}" > /dev/null 2>&1; then bad "plain tag mismatch accepted"; else ok "plain tag mismatch refused"; fi
[ "$(./chart-release-version.sh v0.2.0-rc.1 "${chart}")" = $'version=0.2.0-rc.1\nprerelease=true' ] && ok "rc tag" || bad "rc tag"
[ "$(./chart-release-version.sh v1.0.0-beta "${chart}")" = $'version=1.0.0-beta\nprerelease=true' ] && ok "beta tag" || bad "beta tag"
for t in 0.2.0 v0.2 v0.2.0+build v01.2.3 v0.2.0-rc..1 v0.2.0-01 v0.2.0-rc.1+b vfoo; do
  if ./chart-release-version.sh "${t}" "${chart}" > /dev/null 2>&1; then bad "accepted tag ${t}"; else ok "refused tag ${t}"; fi
done

exit "${fail}"
