# ADR-0048: Redaction moves to a shared module in two stages

- Status: Proposed
- Date: 2026-11-24

## Context

Epic [#1132](https://github.com/shadewright-labs/shadewright/issues/1132) moves the redaction of invariant 4 ([design 10.3](../design/10-security.md#103-redaction)) out of `internal/redact` into the public module `github.com/shadewright-labs/armory`, so that the other Shadewright Labs products use the same rules and a rule fix is written once. One of them waits on this decision before it changes its own rules.

The package is close to standing alone, but not yet. Its production code imports no other Shadewright package outside its own tree. It does import `sigs.k8s.io/controller-runtime/pkg/metrics` (in `budget.go` and `heaplimit.go`, which register `shadewright_redaction_withheld_total` and `shadewright_redaction_heap_waits_total` in `init`), `sigs.k8s.io/controller-runtime/pkg/log` and `github.com/go-logr/logr` (`heaplimit.go`), and through them most of the Kubernetes client tree. Three exported names serve only Shadewright: `IsFluxRevision` (one production caller, `internal/modules`), `MeasuredGoMemoryLimit` with `WarnIfMemoryUnderMeasured` (the analyzer's 256 Mi limit, called from `cmd/analyzer`), and `OverLargestEntry` (the read tools and the context builder, sized by etcd's default request limit). Of the test files in `internal/redact`, 19 import other Shadewright packages (`tools`, `incidentcontext`, `incidents/fingerprint`, `modules`, `providers` and two of its subpackages); 18 are in package `redact_test`, and `diff_fuzz_test.go` is in package `redact` and uses `providers.LeadingContinuation`.

`internal/redact/released`, the reading of the release before #845, is a production dependency: `New` builds a `released.Redactor` into every `Redactor` (`redact.go`, field `before`), and `work_counted.go` counts its work. Its retirement is issue [#922](https://github.com/shadewright-labs/shadewright/issues/922), still open.

**How many files import it.** On `origin/main` at `98152d18`, counted with `git ls-files '*.go'` and a grep for the quoted import path: 97 files import exactly `internal/redact` (75 outside its directory, 26 of them production files), and 82 files outside the directory import it or one of its subpackages (`meter`, `released`, `redacttest`). The epic's figure of 82 matches the second set. Both are correct; the second is the number of Shadewright files a switch of import path touches, before stage A moves any tests out of the directory.

## Decision

The points below were decided on 2026-11-24 under the maintainer's standing authority. The maintainer accepts or rejects them by merging the change of this ADR's status.

**1. Two stages.** Stage A happens inside Shadewright and changes no behaviour:

- the package stops importing controller-runtime and `logr`: it calls payload-free hooks (a withheld entry with its reason, a heap wait), and a thin Shadewright adapter wires them to the existing Prometheus counters under the same names, with every series of `withheldTotal` started at zero as today;
- `IsFluxRevision` moves to `internal/modules`; `MeasuredGoMemoryLimit`, `WarnIfMemoryUnderMeasured` and `OverLargestEntry` move to the adapter, which keeps counting an entry over `LargestEntry` with reason `size`;
- the test files that import other Shadewright packages move into an integration test package; `diff_fuzz_test.go` is split, and its library half takes the continuation rule as a callback;
- a test fails when `go list -deps` of the package lists anything beyond the standard library, `golang.org/x/sync` and the package's own subpackages.

Stage B starts only after stage A has merged and been through one release. The package moves to `armory` as `armory/redact` with its Git history; `meter` and `released` go under the module's `internal/`, and `redacttest` stays public.

**2. The reading of the release before.** `released` moves unchanged, with its historical semantics. Its retirement stays with #922.

**3. Public surface.** `armory/redact` exports what Shadewright calls today and nothing more. Scanners, spans, rule tables and meters stay unexported. The contract promises the signatures, that an entry that cannot be redacted within its bounds is withheld whole, and bounded work. It does not promise which bytes match or the bytes of the marker, so a rule that redacts more ships as a patch release.

**4. Versioning.** Tags `v0.x.y` on commits of the protected `main`, never moved or deleted. The module's `go` line stays at or below the lowest consumer's (Shadewright's is `go 1.26.0`; the `toolchain` line is not part of this rule).

**5. What a consumer verifies.** The checksum database: `GOSUMDB=sum.golang.org`, and the module never matched by `GONOSUMDB` or `GOPRIVATE`. Builds run with `GOFLAGS=-mod=readonly` and `GOWORK=off`, and `go.mod` holds no `replace` for it. On a pull request that changes the pinned version, CI verifies the release's attestation: a signed manifest with the module path, version, commit and `h1:` hashes, checked against the identity of the module's release workflow and compared with the lines in `go.sum`. Go itself verifies neither tag signatures nor attestations, so this last check exists only as a job in Shadewright's CI.

**6. Pinning.** An exact version in `go.mod`. The module is excluded from the Dependabot group `go-minor-patch`, so it gets its own pull request, and keeps the cooldown. An urgent fix is a patch release through the same gates plus a bump opened by hand.

**7. Tests in two tiers.** The module's own corpora, differential runs against its previous release, fuzz targets and `TestEveryLoopCountsItsWork` gate a module release. Shadewright keeps the entry-point corpora and fuzz targets and runs them with `-tags redactwork` against the version it pins, together with the check that production code never lowers the work budget (today `lowbudget_test.go`). Stage B's pull request carries a migration inventory (every test and fixture, and where it lives after the move), and its CI compares the count of tests before and after.

**8. Governance.** Releases come only from the protected release workflow, and a tag ruleset protects `v*`. Every change to the module gets the review a change to `internal/redact` gets today. The organisation has one maintainer, so two-person approval is not available. The compensating controls are an independent review of every module change and the entry-point tests in Shadewright, which gate every bump.

## How the invariants hold

Invariant 4: no rule changes in either stage, and the full corpus and differential runs are green before and after each. The other invariants are not touched.

## Consequences

One rule fix becomes two pull requests and a release, and an urgent fix waits for both. All consumers fail together on one bad release, where separate copies would fail one at a time; the attestation check and the entry-point tests in each consumer are what stand against that. The module gets a public API earlier than the code might like, and `released` carries Shadewright's history into it until #922 removes it. Stage A is useful even if stage B never happens: the package no longer pulls the Kubernetes client tree into its build.

## Alternatives considered

- No move. Each product keeps its own copy of the rules, and fixes drift between them. It comes back if no second product adopts the module within a few releases.
- A nested module inside the Shadewright repository. A rule fix and the entry-point tests stay in one pull request, but other products would depend on a module whose tags share the operator's repository, rulesets and release flow. It comes back if two pull requests and a release prove too slow for urgent fixes.
- A copied subtree with a sync check. There is no version to manage, but every fix is applied by hand in each copy, and the sync check only reports drift. It comes back if a consumer needs rules that diverge.
- Other products import the operator module. Go refuses an import of `internal/` from another module, and exporting the package would bring the operator's Kubernetes and model provider dependencies into every consumer's build. It does not come back while the operator module carries those dependencies.

## Questions for the red team

1. Does stage A change any behaviour, including metric series, the `size` count of `OverLargestEntry` and package `init` order?
2. Does `released`, moved unchanged into the module's `internal/` and still read in production, bring a risk into the module that only #922 would remove?
3. Is the public surface small enough, and can a rule change that redacts more still break a caller?
4. Can a tag be moved, deleted or created outside the release workflow, and does the `go` line rule hold?
5. Can a release reach Shadewright's build without the checksum database, the attestation check or the read-only flags?
6. Can Dependabot, a transitive requirement raised by minimum version selection, or the urgent path change the pinned version without the gates?
7. Can a test or fixture be lost in the move without the inventory or the count showing it, and does the consumer tier still prove the work bound?
8. With one maintainer, what can a compromised account or release workflow do before the controls notice?
9. Does any alternative cost less than the move for the same result?
10. Are the costs above complete?

## Amendments

None.
