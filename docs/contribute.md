---
title: Contribute
---

# Contribute

How to build and test Shadewright from a clone, without a cluster, a container runtime or a cloud account. The contributor and coding-agent guide, with the rules for commits, pull requests and docs, is [AGENTS.md](https://github.com/pkalin/shadewright/blob/main/AGENTS.md); [CONTRIBUTING.md](https://github.com/pkalin/shadewright/blob/main/CONTRIBUTING.md) is the short version. To use Shadewright instead, start at [Get started](getting-started.md).

## Run the tests without a cluster

You need `git`, `make`, Go and a C compiler (`gcc` or `clang`: `make test` uses the race detector, which needs cgo). The Go release is the `toolchain` line of `go.mod` (1.27.3 when this was written); a newer local Go can report different lint and vet results, so pin it with `GOTOOLCHAIN`.

```bash
git clone https://github.com/pkalin/shadewright.git
cd shadewright
export GOTOOLCHAIN=go1.27.3   # the toolchain line of go.mod
make test                      # unit tests and envtest
```

`make test` regenerates the manifests, formats and vets the code, downloads the pinned `setup-envtest` and a real `kube-apiserver` and `etcd` on first use (it needs network access then), and runs every package except `test/e2e` and `test/chart` with the race detector (`-race`), then `internal/redact` once more without it, for the timing tests that skip under `-race`. `make fuzz` runs each Go fuzz target for 30 s (`FUZZTIME=5m` for longer); CI runs it weekly. The envtest suites start those two binaries locally, so no cluster, Docker or cloud account is involved. The CRD validation rules (CEL), the controllers and the RBAC checks all run against that API server.

Other checks that need no cluster:

```bash
make lint                # golangci-lint, and a check that the Go release matches the Dockerfile
make verify-generated    # regenerates CRDs, RBAC and the API reference; fails on any difference
```

`make test-e2e` creates a kind cluster and is not needed for contributions; CI runs it. The contributor guide is [AGENTS.md](https://github.com/pkalin/shadewright/blob/main/AGENTS.md).
