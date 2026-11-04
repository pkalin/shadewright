#!/usr/bin/env bash
# Decides which chart version a release tag publishes (the Chart workflow). Usage:
#   hack/chart-release-version.sh <tag> <Chart.yaml>
# Prints two lines for $GITHUB_OUTPUT: `version=<tag without v>` and `prerelease=true|false`.
#   vX.Y.Z               a release: version and appVersion in Chart.yaml (bumped by release-please) must
#                        equal the tag, otherwise it fails and nothing is published.
#   vX.Y.Z-<pre-release> a semver pre-release (v0.2.0-rc.1): nobody commits a bump for it, so the workflow
#                        packages with --version and --app-version taken from the tag, and Chart.yaml is
#                        not compared.
# Anything else (no leading v, build metadata with +, a leading zero, a missing part) fails.
set -euo pipefail

if [ "$#" -ne 2 ]; then
  echo "usage: $0 <tag> <Chart.yaml>" >&2
  exit 2
fi
tag="$1" chart="$2"

num='(0|[1-9][0-9]*)'
ident='(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)'
if printf '%s' "${tag}" | grep -Eq "^v${num}\.${num}\.${num}-${ident}(\.${ident})*\$"; then
  prerelease=true
elif printf '%s' "${tag}" | grep -Eq "^v${num}\.${num}\.${num}\$"; then
  prerelease=false
else
  echo "the tag '${tag}' is neither vX.Y.Z nor a semver pre-release vX.Y.Z-<identifiers>" >&2
  exit 1
fi

if [ "${prerelease}" = false ]; then
  version="$(sed -n 's/^version: *\([^ #]*\).*$/\1/p' "${chart}" | tr -d '"')"
  app_version="$(sed -n 's/^appVersion: *\([^ #]*\).*$/\1/p' "${chart}" | tr -d '"')"
  if [ "v${version}" != "${tag}" ] || [ "v${app_version}" != "${tag}" ]; then
    echo "chart version ${version} / appVersion ${app_version} do not match the tag ${tag}" >&2
    exit 1
  fi
fi
echo "version=${tag#v}"
echo "prerelease=${prerelease}"
