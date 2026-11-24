# ADR-0048: Redaction moves to a shared module in two stages

- Status: Proposed
- Date: 2026-11-24

## Context

Epic [#1132](https://github.com/shadewright-labs/shadewright/issues/1132) moves the redaction of invariant 4 ([design 10.3](../design/10-security.md#103-redaction)) out of `internal/redact` into the public module `github.com/shadewright-labs/armory`, so that the other Shadewright Labs products use the same rules and a rule fix is written once. One of them waits on this decision before it changes its own rules.

The package is close to standing alone, but not yet. Its production code imports no other Shadewright package outside its own tree. It does import `github.com/prometheus/client_golang/prometheus` and `sigs.k8s.io/controller-runtime/pkg/metrics` (in `budget.go` and `heaplimit.go`, which register `shadewright_redaction_withheld_total` and `shadewright_redaction_heap_waits_total` in `init`), `sigs.k8s.io/controller-runtime/pkg/log` and `github.com/go-logr/logr` (`heaplimit.go`), and through them most of the Kubernetes client tree. Three exported names serve only Shadewright: `IsFluxRevision` (one production caller, `internal/modules`), `WarnIfMemoryUnderMeasured` (called from `cmd/analyzer`; it compares the Go memory limit with the constant `MeasuredGoMemoryLimit`, 90 percent of the analyzer's 256 Mi), and `OverLargestEntry` (the read tools and the context builder, sized by etcd's default request limit). Of the test files in `internal/redact`, 19 import other Shadewright packages (`tools`, `incidentcontext`, `incidents/fingerprint`, `modules`, `providers` and two of its subpackages); 18 are in package `redact_test`, and `diff_fuzz_test.go` is in package `redact` and uses `providers.LeadingContinuation`. Those tests also use helpers that exist only in package `redact`'s test files: `EntryCorpus`, `RaceKeeps` and `ParallelUnderRace` in `export_test.go`, `CanonCorpus` and `CanonIndex` in `export_canon_test.go`.

`internal/redact/released`, the reading of the release before #845, is a production dependency: `New` builds a `released.Redactor` into every `Redactor` (`redact.go`, field `before`), and `work_counted.go` counts its work. Shadewright's differential fuzz targets outside the package import it too (`internal/tools/diff_main_test.go`, `internal/incidentcontext/diff_fuzz_test.go`). Its retirement is issue [#922](https://github.com/shadewright-labs/shadewright/issues/922), still open.

**How many files import it.** On `origin/main` at `98152d18`, counted with `git ls-files '*.go'` and a grep for the quoted import path: 97 files import exactly `internal/redact` (75 outside its directory, 26 of them production files), and 82 files outside the directory import it or one of its subpackages (`meter`, `released`, `redacttest`). The epic's figure of 82 is the second set.

The module's repository exists, is public and is empty: today it has no tag, no ruleset and no workflow. No Shadewright workflow, Makefile target or Dockerfile sets `GOFLAGS`, `GOWORK`, `GOSUMDB`, `GONOSUMDB` or `GOPRIVATE`.

## Decision

The points below were decided on 2026-11-24 under the maintainer's standing authority, and revised the same day after a design review and a red team. The maintainer accepts or rejects them by merging the change of this ADR's status.

**1. Two stages.** Stage A happens inside Shadewright and changes no behaviour:

- the package stops importing `prometheus`, controller-runtime and `logr` (`budget.go`, `heaplimit.go`). `New` takes payload-free hooks as an option (a withheld entry with its reason, a heap wait); nil means none. A hook is called after the decision is made and cannot change it. A thin Shadewright adapter wires the hooks to the existing counters under the same names, and it is wired in every binary that links the package (today `cmd/main.go`, the executor, `cmd/analyzer` and `cmd/intake`), each with a test that the metric series start at zero as today;
- `IsFluxRevision` moves to `internal/modules`; `WarnIfMemoryUnderMeasured` and the constant `MeasuredGoMemoryLimit` move to the adapter;
- `OverLargestEntry` and `LargestEntry` stay in the package, next to the measured heap bound in `budget.go`, so that another product cannot pass larger entries than were measured. Only its count with reason `size` goes to the adapter, through the hooks;
- the test files that import other Shadewright packages move into an integration test package. `diff_fuzz_test.go` is split, and its library half takes the continuation rule as a callback. The helpers they use (`EntryCorpus`, `CanonCorpus`, `CanonIndex`, `RaceKeeps`, `ParallelUnderRace`) move to `redacttest`;
- a test fails when the packages import anything outside the allow-list of point 9.

Stage B starts only when the checklist "Before stage B starts" below is complete. The package moves to `armory` as `armory/redact` with its Git history; `meter` and `released` go under the module's `internal/`, and `redacttest` stays public.

**2. The reading of the release before.** `released` moves unchanged, with its historical semantics, and its production reader stays unexported. Go refuses an import of another module's `internal/`, so the differential tests of consumers reach that reading through `redacttest`. Its retirement stays with #922, and is a minor release recorded as a behaviour change: without that reading the module may redact less for some consumer, so it falls under point 3's promise only by an explicit exception written in that release's notes.

**3. Public surface and contract.** `armory/redact` exports what Shadewright calls today and nothing more. Scanners, spans, rule tables and meters stay unexported. The tag-gated test API (`Work`, `WorkOfReleased`, `WorkOfExpressions`, `Unmetered`, in `work_counted.go`) is public only under the build tag `redactwork`. `Tick` is exported in every build because the read tools call it in production; it does nothing without the tag.

The contract promises the signatures, that an entry that cannot be redacted within its bounds is withheld whole, bounded work, and that what is hidden only grows: a patch or minor release never shows a byte that the release before hid, on the module's corpora and on any consumer's pinned corpus. It does not promise the bytes of the marker, so a rule that redacts more ships as a patch release. Shadewright enforces the promise itself: a pull request that changes the pinned version runs the differential comparison between the old pin and the new pin over Shadewright's own corpus, and fails on any byte newly shown.

**4. Governance of rules.** [ADR-0015](0015-analysis-records-outlive-the-lab.md) and its amendments keep governing the redaction guarantees Shadewright relies on. A module change that alters one of them needs an amendment accepted in Shadewright before Shadewright bumps to that release.

**5. Versioning.** Tags `v0.x.y` on commits of the protected `main`, never moved or deleted. The module's `go` line stays at or below the lowest consumer's (Shadewright's is `go 1.26.0`; the `toolchain` line is not part of this rule). Before `v0.1.0`, the module's repository gets a ruleset on `v*` tags that covers creation, update and deletion and whose only bypass is the release workflow's identity.

**6. What a consumer verifies.** The checksum database: `GOSUMDB=sum.golang.org`, and the module never matched by `GONOSUMDB` or `GOPRIVATE`. Shadewright's CI compares the module's lines in `go.sum` with the checksum database's answer. Stage A sets `GOFLAGS=-mod=readonly` and `GOWORK=off` in Shadewright's builds; no workflow or Dockerfile sets them today. On a pull request that changes the pinned version, a required check verifies the release's attestation: a signed manifest with the module path, version, commit and `h1:` hashes, checked against the identity of the module's release workflow and compared with the lines in `go.sum`. The pull request cannot edit that check: it runs from a trigger that uses the base branch's workflow, reads `go.mod` and `go.sum` as data only and never builds the head, and it refuses a pseudo-version, a `replace` of the module and a `go.work`. Go itself verifies neither tag signatures nor attestations, so this check exists only in Shadewright's CI.

**7. Pinning.** An exact version in `go.mod`. The module is excluded from the Dependabot group `go-minor-patch`, so it gets its own pull request, and it is exempt from the cooldown. The bump pull request carries the source diff from the old tag to the new one for the review.

**8. Disclosure.** A fix for a leak goes through a private security advisory on the module's repository. Every consumer is bumped before the advisory is published, and the leaky versions are then retracted. Consumer CI fails when the pinned version is retracted.

**9. Dependency ceiling.** The module's production packages (`redact`, `meter`, `released`) may import only `golang.org/x/sync/semaphore`, each other and these 21 standard library packages, the ones the code imports today: `bytes`, `context`, `encoding/base64`, `errors`, `fmt`, `io`, `iter`, `math`, `math/bits`, `net/netip`, `regexp`, `regexp/syntax`, `runtime/debug`, `slices`, `strconv`, `strings`, `sync`, `sync/atomic` (only with `redactwork`), `time`, `unicode`, `unicode/utf8`. `redacttest` has its own list, the 16 it imports today (`crypto/sha256`, `encoding/hex`, `fmt`, `os`, `path/filepath`, `regexp`, `runtime`, `runtime/debug`, `runtime/metrics`, `strconv`, `strings`, `sync`, `sync/atomic`, `testing`, `time`, `unicode/utf8`), plus what the moved helpers import, named in stage A's pull request. A new package needs a change of the list in review. The check runs for each GOOS a consumer releases (Shadewright's images: `linux`), with and without `-tags redactwork`, in both repositories.

**10. Tests in two tiers.** The module's own corpora, differential runs against its previous release, fuzz targets, `TestEveryLoopCountsItsWork` and the check that production code never lowers the work budget (`TestLoweredBudgetOnlyInTests`, today in `lowbudget_test.go`) gate a module release; the module's CI runs the corpora, the `redactwork` jobs and weekly fuzzing. Shadewright keeps the entry-point corpora and their fixtures in its own `testdata`, owned by Shadewright, and a module bump never changes them: the consumer's gate must not change in the same release as the rules it gates. Shadewright runs them and its fuzz targets with `-tags redactwork` against the version it pins, together with its own copy of the low-budget check. Stage B's pull request carries a migration inventory: every test, every `testdata` file and every fuzz seed directory (today `FuzzDiffReplaceFrom`, `FuzzDiffString`, `FuzzDiffStrings`, `FuzzDiffStringTwice`, `FuzzString` and `released`'s `FuzzReleasedMatchesKeptCopy`), and where each lives after the move. A check fails on a seed directory with no fuzz target of that name, and CI compares the count of tests before and after under both tag sets.

**11. Governance of releases.** Releases come only from the release workflow. Every change to the module gets the review a change to `internal/redact` gets today. The organisation has one maintainer, so two-person approval is not available. The compensating controls are an independent review of every module change, the attestation check, the differential comparison of point 3 and the entry-point tests in Shadewright, which gate every bump.

## Before stage B starts

- Stage A is merged and has been through one Shadewright release.
- The attestation check (point 6), the bump pull request's tag-to-tag diff (point 7), the disclosure path (point 8) and the dependency ceiling in both repositories (point 9) are in place.
- The module's repository has the `v*` tag ruleset of point 5 and its own CI of point 10.
- Shadewright's builds set `GOFLAGS=-mod=readonly` and `GOWORK=off`.

## How the invariants hold

Invariant 4: no rule changes in either stage, and the full corpus and differential runs are green before and after each. After stage B, a release that shows a byte the pinned one hid fails Shadewright's bump. The other invariants are not touched.

## Consequences

One rule fix becomes two pull requests and a release, and an urgent fix waits for both, plus the advisory when it is a leak. All consumers fail together on one bad release, where separate copies would fail one at a time; the attestation check, the differential comparison between pins and the entry-point tests in each consumer stand against that. The module gets a public API earlier than the code might like, `redacttest` included, and `released` carries Shadewright's history into it until #922 removes it. `New` gains an option. Stage A is useful even if stage B never happens: the package no longer pulls the Kubernetes client tree into its build.

## Alternatives considered

- No move. Each product keeps its own copy of the rules, and fixes drift between them. It comes back if no second product adopts the module within a few releases.
- A nested module inside the Shadewright repository. A rule fix and the entry-point tests stay in one pull request, but other products would depend on a module whose tags share the operator's repository, rulesets and release flow. It comes back if two pull requests and a release prove too slow for urgent fixes.
- A copied subtree with a sync check. There is no version to manage, but every fix is applied by hand in each copy, and the sync check only reports drift. It comes back if a consumer needs rules that diverge.
- Vendoring the module, with Shadewright's corpus as the gate. Every byte that ships is in Shadewright's tree, but the vendored copy still comes from a release that needs the same checks, and it adds a diff of the whole package to every bump. The differential comparison between pins gives the same gate without the copy. It comes back if the checksum database or the attestation cannot be relied on.
- Other products import the operator module. Go refuses an import of `internal/` from another module, and exporting the package would bring the operator's Kubernetes and model provider dependencies into every consumer's build. It does not come back while the operator module carries those dependencies.

## Red team

- Stage A could change metric series or `init` order: the hooks run after the decision, and each binary's test starts the series at zero (point 1).
- Another product could pass entries larger than the heap was measured for: the size cap stays in the module (point 1).
- A release that hides less would be a valid patch: the contract promises that what is hidden only grows, and Shadewright checks it between pins (point 3).
- Retiring `released` could hide less for some consumer: it is a minor release with a written exception (point 2).
- A module change could alter a guarantee of ADR-0015 without Shadewright deciding it: it needs an amendment first (point 4).
- A tag could be created, moved or deleted outside the release workflow: the tag ruleset, required before `v0.1.0` (point 5).
- The bump pull request could edit the check that guards it: the check runs from the base branch and reads the files as data (point 6).
- A pseudo-version, `replace`, `go.work` or tampered `go.sum` line could bypass the pin: the check refuses each, CI compares `go.sum` with the checksum database, and builds run read-only (point 6).
- A leak fix could be public before consumers have it: the private advisory and retraction (point 8).
- A release could pull in a new package unnoticed: the standard library allow-list and the tag-to-tag diff (points 7 and 9).
- A test, fixture or seed could be lost in the move: the inventory, the seed check and the counts under both tag sets (point 10).
- A module release could weaken the consumer's gate at the same time: the entry-point corpora stay in Shadewright, and the low-budget check runs in both repositories (point 10).
- The test API could leak into the product build: it is public only under `redactwork` (point 3).

## Amendments

None.
