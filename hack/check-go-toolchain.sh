#!/usr/bin/env bash
# Fails when the Go release of the Dockerfile builder stage (the Go that builds the released image) differs
# from the `toolchain` line of go.mod, which setup-go in CI reads to pick the Go that runs lint, tests and
# govulncheck. Dependabot bumps the Dockerfile tag; change the go.mod toolchain line in the same pull request.
set -euo pipefail

cd "$(dirname "$0")/.."

image="$(sed -n -E 's/^FROM[[:space:]]+(--platform=[^[:space:]]+[[:space:]]+)?golang:([0-9]+\.[0-9]+\.[0-9]+)(@sha256:[0-9a-f]{64})?[[:space:]]+AS[[:space:]]+builder[[:space:]]*$/\2/p' Dockerfile | head -n1)"
toolchain="$(sed -n -E 's/^toolchain[[:space:]]+go([0-9]+\.[0-9]+\.[0-9]+)[[:space:]]*$/\1/p' go.mod | head -n1)"

if [ -z "$image" ]; then
  echo "error: could not read the Go version of the builder stage from the Dockerfile" >&2
  exit 1
fi
if [ -z "$toolchain" ]; then
  echo "error: go.mod has no 'toolchain go<x.y.z>' line (the Dockerfile builder uses go${image})" >&2
  exit 1
fi
if [ "$image" != "$toolchain" ]; then
  echo "error: Dockerfile builder uses go${image} but go.mod says toolchain go${toolchain}; make them equal" >&2
  exit 1
fi
echo "Go toolchain ${image} matches the Dockerfile builder"
