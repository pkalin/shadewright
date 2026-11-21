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

`make test` regenerates the manifests, formats and vets the code, downloads the pinned `setup-envtest`, `gotestsum` and a real `kube-apiserver` and `etcd` on first use (it needs network access then), and runs every package except `test/e2e` and `test/chart` with the race detector (`-race`), then the whole of `internal/redact`, of the kept reading of the release before (`internal/redact/released`), of the meter of the work budget (`internal/redact/meter`), of the log source (`internal/providers/logs/kube`), of the context builder (`internal/incidentcontext`) and of the read tools (`internal/tools`) once more without it and with the build tag `redactwork` (`internal/redact` in six test binaries, each well inside the 20 minute limit of a binary on a hosted runner: five run named groups of its longest tests and the last every other test, so a renamed one cannot drop out; they and the run of the other packages go side by side, `hack/side-by-side.sh`, which prints each one's output whole once all are done and fails when any fails; CI runs `make test REDACTWORK_IN_TEST=no` and each of those runs in a job of its own, the list from `make redactwork-list`, since side by side on one hosted runner they passed their limit), for the timing tests and the 100,000 seeded keys of the key-name test, which skip under `-race`, the generated corpora (block values, connection strings, and those compared with main or with the release before), which check a seeded sample of about one input in 128 under `-race` (the context builder's generated tallies and metric blocks one in 8) (the block values and connection strings through every entry point, run in parallel; under `-race` the tests of `internal/redact` that count no work and share no state also run in parallel with each other) and every input without it, and the linearity tests, which count redaction work only in a build with that tag (the product build spends each step on the work budget of its call and has no shared counter). `make fuzz` runs each Go fuzz target for 30 s (`FUZZTIME=5m` for longer), those of `internal/redact`, `internal/redact/released` and `internal/incidentcontext` with the tag `redactwork` (the packages with a file built only with it; the log source has no fuzz target, and the targets of `internal/tools` run without it), so that the differential targets of those two packages also check the budget of each entry (P3); every target that reaches redaction fails an input under 64 KiB that redaction withholds for work (P6), so a reading whose work grows faster than the text is a finding; CI runs it weekly, one job per target. The envtest suites start those two binaries locally, so no cluster, Docker or cloud account is involved. The CRD validation rules (CEL), the controllers and the RBAC checks all run against that API server. The run with the race detector and the wide mapping run go through the pinned `gotestsum` (`GOTESTSUM_VERSION` in the Makefile, installed into `bin/`) with the same packages, flags and timeouts as before, in the format `standard-verbose` (one line per test, then the usual `--- FAIL`, `FAIL` and `ok` lines) and with no rerun of a failed test. It writes a junit file and a JSON log per line (`unit-race`, `wide-mapping`) under `test-reports/` (git-ignored; `TEST_REPORT_DIR` changes it). The `test` job in CI uploads that folder as the `test-reports` artifact, also when a test failed, and lists the tests slower than 3 s (the 30 slowest) in its summary; locally `bin/gotestsum tool slowest --jsonfile test-reports/unit-race.json --threshold 3s --num 30` prints the same list. The envtest suites start those two binaries locally, so no cluster, Docker or cloud account is involved. The CRD validation rules (CEL), the controllers and the RBAC checks all run against that API server. The runs with the tag `redactwork` use plain `go test` and write no such files.

Other checks that need no cluster:

```bash
make lint                # golangci-lint, a check that the Go release matches the Dockerfile, and the kubectl plugin's dependency and size check
make verify-generated    # regenerates CRDs, RBAC and the API reference; fails on any difference
```

`make test-e2e` creates a kind cluster and is not needed for contributions; CI runs it. The contributor guide is [AGENTS.md](https://github.com/pkalin/shadewright/blob/main/AGENTS.md).
