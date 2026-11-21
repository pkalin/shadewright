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

`make test` regenerates the manifests, formats and vets the code, downloads the pinned `setup-envtest`, `gotestsum` and a real `kube-apiserver` and `etcd` on first use (it needs network access then), and runs every package except `test/e2e` and `test/chart` with the race detector (`-race`), then the whole of `internal/redact` and of the log source (`internal/providers/logs/kube`) once more without it and with the build tag `redactwork` (no test selected by name, so a renamed one cannot drop out), for the timing tests and the 100,000 seeded keys of the key-name test, which skip under `-race`, the generated corpora of block values and connection strings, which check a seeded sample of about one input in sixteen under `-race` (through every entry point, run in parallel) and every input without it, and the linearity tests, which count redaction work only in a build with that tag (the product build has no counter). `make fuzz` runs each Go fuzz target for 30 s (`FUZZTIME=5m` for longer); CI runs it weekly. Each of those test lines runs through the pinned `gotestsum` (`GOTESTSUM_VERSION` in the Makefile, installed into `bin/`) with the same packages, flags and timeouts as before, in the format `standard-verbose` (one line per test, then the usual `--- FAIL`, `FAIL` and `ok` lines) and with no rerun of a failed test. It writes a junit file and a JSON log per line (`unit-race`, `redactwork`, `wide-mapping`) under `test-reports/` (git-ignored; `TEST_REPORT_DIR` changes it). The `test` job in CI uploads that folder as the `test-reports` artifact, also when a test failed, and lists the tests slower than 3 s (the 30 slowest) in its summary. It also uploads the coverage profile `cover.out` as the `coverage` artifact, and a separate `coverage upload` job sends it to Codecov when the repository secret `CODECOV_TOKEN` is set (without it the job passes and uploads nothing; the report is informational, never a gate); locally `bin/gotestsum tool slowest --jsonfile test-reports/unit-race.json --threshold 3s --num 30` prints the same list. The envtest suites start those two binaries locally, so no cluster, Docker or cloud account is involved. The CRD validation rules (CEL), the controllers and the RBAC checks all run against that API server.

Other checks that need no cluster:

```bash
make lint                # golangci-lint, a check that the Go release matches the Dockerfile, and the kubectl plugin's dependency and size check
make verify-generated    # regenerates CRDs, RBAC and the API reference; fails on any difference
```

`make test-e2e` creates a kind cluster and is not needed for contributions; CI runs it. The contributor guide is [AGENTS.md](https://github.com/pkalin/shadewright/blob/main/AGENTS.md).
