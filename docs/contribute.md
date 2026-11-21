---
title: Contribute
---

# Contribute

How to build and test Shadewright from a clone, without a cluster, a container runtime or a cloud account. The contributor and coding-agent guide, with the rules for commits, pull requests and docs, is [AGENTS.md](https://github.com/pkalin/shadewright/blob/main/AGENTS.md); [CONTRIBUTING.md](https://github.com/pkalin/shadewright/blob/main/CONTRIBUTING.md) is the short version. To use Shadewright instead, start at [Get started](getting-started.md).

## Run the tests without a cluster

You need `git`, `make`, Go and a C compiler (`gcc` or `clang`: `make test` uses the race detector, which needs cgo). The Go release is the `toolchain` line of `go.mod` (1.27.4 when this was written); a newer local Go can report different lint and vet results, so pin it with `GOTOOLCHAIN`.

```bash
git clone https://github.com/pkalin/shadewright.git
cd shadewright
export GOTOOLCHAIN=go1.27.4   # the toolchain line of go.mod
make test                      # unit tests and envtest
```

`make test` regenerates the manifests, formats and vets the code, downloads the pinned `setup-envtest` and a real `kube-apiserver` and `etcd` on first use (it needs network access then), and runs every package except `test/e2e` and `test/chart` with the race detector (`-race`), then the whole of `internal/redact`, of the kept reading of the release before (`internal/redact/released`), of the log source (`internal/providers/logs/kube`) and of the context builder (`internal/incidentcontext`) once more without it and with the build tag `redactwork` (`internal/redact` in six test binaries one after the other, each well inside the 20 minute limit of a binary on a hosted runner: five run named groups of its longest tests and the last every other test, so a renamed one cannot drop out), for the timing tests and the 100,000 seeded keys of the key-name test, which skip under `-race`, the generated corpora (block values, connection strings, and those compared with main or with the release before), which check a seeded sample of about one input in 64 under `-race` (the block values and connection strings through every entry point, run in parallel) and every input without it, and the linearity tests, which count redaction work only in a build with that tag (the product build spends each step on the work budget of its call and has no shared counter). `make fuzz` runs each Go fuzz target for 30 s (`FUZZTIME=5m` for longer), those of `internal/redact` and `internal/incidentcontext` with the tag `redactwork` (the packages with a file built only with it; the log source has no fuzz target, and the targets of `internal/tools` run without it), so that the differential targets of those two packages also check the budget of each entry (P3); every target that reaches redaction fails an input under 64 KiB that redaction withholds for work (P6), so a reading whose work grows faster than the text is a finding; CI runs it weekly, but that run is cancelled at 30 minutes after the first eight targets, before the differential ones, until [#999](https://github.com/pkalin/shadewright/issues/999) is fixed. The envtest suites start those two binaries locally, so no cluster, Docker or cloud account is involved. The CRD validation rules (CEL), the controllers and the RBAC checks all run against that API server.

Other checks that need no cluster:

```bash
make lint                # golangci-lint, a check that the Go release matches the Dockerfile, and the kubectl plugin's dependency and size check
make verify-generated    # regenerates CRDs, RBAC and the API reference; fails on any difference
```

`make test-e2e` creates a kind cluster and is not needed for contributions; CI runs it. The contributor guide is [AGENTS.md](https://github.com/pkalin/shadewright/blob/main/AGENTS.md).
