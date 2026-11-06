#!/usr/bin/env bash
# Builds the kubectl-shadewright release archives and checksums.txt (the release-plugin.yml build job).
# Usage: hack/package-plugin.sh <tag> <output dir>
# For each linux and darwin on amd64 and arm64 it runs `make build-plugin` (static: CGO_ENABLED=0, -trimpath,
# version stamped from the tag) and packs the binary with the LICENSE into
# kubectl-shadewright_<version>_<os>_<arch>.tar.gz, where <version> is the tag without its leading v. The archives
# are reproducible (sorted entries, fixed owner and mtime, no gzip timestamp; needs GNU tar). checksums.txt lists
# the archives in `sha256sum --check` format.
set -euo pipefail

tag="${1:?usage: package-plugin.sh <tag> <output dir>}" out="${2:?usage: package-plugin.sh <tag> <output dir>}"
if ! printf '%s' "${tag}" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$'; then
  echo "not a release tag: ${tag}" >&2
  exit 1
fi
version="${tag#v}"
mkdir -p "${out}"
out="$(cd "${out}" && pwd)"
stage="$(mktemp -d)"
trap 'rm -rf "${stage}"' EXIT
mtime="${SOURCE_DATE_EPOCH:-$(git log -1 --format=%ct "${tag}^{commit}" 2> /dev/null || git log -1 --format=%ct HEAD)}"

for os in linux darwin; do
  for arch in amd64 arm64; do
    CGO_ENABLED=0 GOOS="${os}" GOARCH="${arch}" PLUGIN_VERSION="${tag}" make build-plugin
    name="kubectl-shadewright_${version}_${os}_${arch}"
    mkdir -p "${stage}/${name}"
    mv bin/kubectl-shadewright "${stage}/${name}/kubectl-shadewright"
    cp LICENSE "${stage}/${name}/LICENSE"
    tar --sort=name --mtime="@${mtime}" --owner=0 --group=0 --numeric-owner \
      -C "${stage}/${name}" -cf - kubectl-shadewright LICENSE | gzip -n -9 > "${out}/${name}.tar.gz"
  done
done

(cd "${out}" && sha256sum kubectl-shadewright_*.tar.gz > checksums.txt)
cat "${out}/checksums.txt"
