# ADR-0005: Go, Kubebuilder, one module with two binaries

- Status: Accepted
- Date: 2026-11-01

## Context

The controller needs watches, caching, leader election, status conditions and generated CRDs. The analyzer needs a model client and a tool loop, and can be slow.

## Decision

Go with Kubebuilder, `controller-runtime` and `client-go`, in one module (`github.com/pkalin/shadewright`). Two binaries: the remediation controller (`cmd/main.go`) and the analyzer (`cmd/analyzer`), deployed as separate Deployments with separate ServiceAccounts and identities. Tests use envtest; end-to-end tests use Kind in CI.

## Consequences

- One version, one release, shared API types.
- A slow or failing model never blocks reconciles, and a compromised analyzer cannot reach the Git credentials.
- Contributors follow standard Kubebuilder conventions (`make manifests generate test`).

## Alternatives considered

- One binary: simpler deployment, but one identity for analysis and execution.
- Python for the analyzer (rich LLM ecosystem): two languages and two toolchains for one small team.
