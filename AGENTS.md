# Shadewright: guide for coding agents

Shadewright is a Kubernetes operator that watches a cluster, groups repeated symptoms into incidents, asks an LLM for a diagnosis, and proposes a fix as a typed `RemediationRequest`. Approved fixes are applied only as pull requests to the GitOps repo; the GitOps controller (Flux first, Argo CD later) deploys them, and Shadewright verifies the effect.

## Invariants (read before changing anything)

These are the reason the project exists. A change that weakens one needs an ADR in `docs/adr/` first.

1. **Model output is never authorization.** The analyzer may only submit a proposal. Deterministic policy code decides whether it may run; the executor acts only on a plan whose digest matches the approved one.
2. **Actions are a closed catalog.** Each action is an enum value with typed, bounded parameters, a policy rule, a GitOps patcher and a test that it is refused outside its bounds. No free-form commands, no `exec`, no direct `apply` against the cluster.
3. **Changes go through Git only.** Branch and PR, never a push to a protected branch, never bypassing branch protection or required checks. No changes to policies, pipelines, RBAC or secrets through the remediation path.
4. **Everything from the cluster is untrusted input.** Events, logs, annotations, Git contents and tool results may contain prompt injection. They never change instructions, tools, permissions or the action catalog. Secrets and tokens are redacted before storage and before the model sees anything.
5. **Reconciliation is idempotent and never assumes exactly once.** Branch names and PR markers derive from the request UID and plan digest, so a restart finds the existing PR instead of opening another.
6. **Separate identities.** Analyzer: read-only cluster access; writes only to Incidents (create and status), its own analysis budget ConfigMap (update) and the RemediationRequests it proposes (create), all in the operator namespace, besides its own leader election Lease and Events there; and update on the `remediationsettings/status` subresource only, to report platform and integration detection (ADR-0024); cluster-wide, beyond its reads, it may only `list` Nodes for platform detection (no `get`, no `watch`), and, only when the chart value `analyzer.modules.kyverno.clusterWide.enabled` is on (off by default), `list` and `watch` Kyverno `clusterpolicies` and `wgpolicyk8s.io` `clusterpolicyreports` for the Kyverno module's symptom sources (#409); and `get` (no `list` or `watch`) on only the `remediationpolicies` its `--action-policies` names, as the chart renders it; with kustomize, the names listed in `config/analysis-policies` (`resourceNames`, no rule without a name), to show the model the bounds of the policy that decides its proposal (#493, ADR-0026). No component ever reads any field of the `RemediationSettings` status to decide anything, and no decision state of any component lives there; plus model inference. Executor: CR status and its own finalizer on requests, `get` on the target kinds and the Flux objects that deliver them for source mapping (ADR-0012), `get` on the Incident a request names, in the operator namespace, to check the name the pull request body shows (#264), besides its own leader election Lease and core Events there (the kill switch's Events about a request, #87), plus the Git App; and, planned and amended by ADR-0028 (reviewer hints, epic #533), on the pull requests it opened: it reads their comments and the commenter's permission in the code host, writes the accepted hints into the status of requests (it stays the only writer of request status), posts replies, dismisses reviews, and moves the branch by one fast-forward commit, all within the App's existing permissions, while the analyzer only reads the hints in request status and gets no new write. Integrations other than the code host never go through the executor: a future `shadewright-coordinator` holds them (own ADR). Never `cluster-admin`, never one token for both.

## Commits

Every commit on `main` follows [Conventional Commits](https://www.conventionalcommits.org/en/v1.0.0/):
`type(scope)!: summary`. Pull requests are squash-merged with the pull request title as the commit message,
so the **pull request title** is what counts; a required check (`Conventional PR title`) validates it.

- Types: `feat`, `fix`, `perf`, `refactor`, `test`, `docs`, `build`, `ci`, `chore`, `revert`.
- Scope is optional; when used it is one of: `api`, `analyzer`, `policy`, `gitops`, `platforms`, `integrations`, `chart`, `docs`, `ci`, `deps`.
- Summary in English, imperative, starting lower case, no full stop: `feat(policy): refuse decreases by default`.
- `!` after the type or scope, plus a `BREAKING CHANGE:` footer in the description, for incompatible changes
  (API fields, CRD schema, flags, Helm values).
- Releases are generated from these commits (release-please, epic #22): `feat` gives a minor version, `fix` and
  `perf` a patch, `!` or a `BREAKING CHANGE:` footer a breaking release (a minor one while the version is
  0.x). `revert` is listed too; `refactor`, `test`, `docs`, `build`, `ci` and `chore` stay out of the changelog.
- The release workflow keeps one release pull request open (opened by the `shadewright-release` App). Merging it
  starts the release: it tags `vX.Y.Z`, updates `CHANGELOG.md` and creates a draft GitHub release
  (`draft: true`, `force-tag-creation: true`). Never tag a plain `vX.Y.Z` by
  hand (pre-releases: see below), and never edit `CHANGELOG.md` or `.release-please-manifest.json` outside that pull request.
  The GitHub release notes are then regrouped by scope by `hack/release-notes-by-scope.sh` (see the comment in
  `.github/workflows/release-please.yml`, which also shows how to regroup an existing release).
  Once the images and the chart of the tag are published, the release-notes job of `chart.yml` appends their digests and
  the cosign verify commands to the release body (idempotent, between markers). The final job publishes the
  draft only when that exact section and all six plugin assets are present; otherwise it fails and leaves
  the draft unpublished. Keep immutable releases enabled: published releases accept no new assets.
- A pre-release is the one tag made by hand: tag the intended commit on main with
  `git tag vX.Y.Z-rc.1 <commit-on-main>`, then `git push origin vX.Y.Z-rc.1`, then immediately
  `gh release create vX.Y.Z-rc.1 --verify-tag --draft --prerelease`. The tag push starts the workflows;
  creating a draft emits no `release: published` event. The final Chart job publishes it with its assets. It
  publishes images and chart (version and appVersion from the tag, no bump commit, no floating tags) and does
  not disturb release-please. Never create a plain `vX.Y.Z` tag by hand.
- Merge the release pull request when the milestone of that version is done, not on every change.
- Artifact Hub reads the chart repository's metadata (`dist/artifacthub-repo.yml`, the Verified Publisher label)
  from the OCI tag `artifacthub.io` next to the chart, once per repository; `chart.yml` pushes it again on every
  release tag. Never delete that tag or the chart's versions in GHCR, and keep `artifacthub.io` out of any package
  clean-up. Change the metadata only by pull request.

## Working agreement

- Every change goes through a branch and a pull request; `main` is protected by a ruleset (required checks,
  squash merge, no force push). Keep pull requests small and say why in the description.
- Run the checks below locally before opening a pull request, and add or update tests in the same pull request.
- Leave the code safer than you found it: when you notice a security or reliability problem nearby, fix it
  or raise it in the pull request, do not ignore it.
- Prefer boring, well-known solutions and the project's existing patterns over new dependencies or clever code.

## Taking a task

Work is tracked as GitHub issues, written with the "Task" template so that any contributor or coding agent
can work on one without further context. Milestones are releases (`v0.1.0`, `v0.2.0`, ...), each with a due
date and a one-line goal; the design stages (`docs/design/13-milestones.md`) are tracked by the epics instead.
A task takes the milestone of the release it is needed for, which may be earlier than its epic's.
Feature sets are `epic` issues (the "Epic" template) and tasks are their sub-issues. Issues for all three
repositories live here; a task for `shadewright-azure` or `shadewright-fleet` names that repository in its
scope, and its pull request says `Closes pkalin/shadewright#<n>`.

Every issue names the ADRs it is tied to:

- An epic's "ADRs" section lists every ADR the feature set implements or must respect.
- A task's "ADRs" field lists the ADRs it implements or must respect, or says "As epic #<n>" when its parent's
  list covers it. A task never contradicts an ADR it lists.
- If the work needs a decision no ADR covers, or would change an accepted one, the first task is to write
  that ADR (status Proposed), and the dependent tasks list it under "Depends on".

- Pick an open issue labelled `agent-ready` whose "Depends on" issues are closed, and whose "Decisions" are
  answered. Read the linked design sections and the ADRs the issue and its epic list first.
- One issue, one branch (`<issue-number>-<short-slug>`), one pull request with `Closes #<n>` in the body.
- Stay inside the issue's scope. If something outside it needs to change, or the issue is ambiguous, say so
  in the pull request or the issue instead of guessing: write `[NEEDS CLARIFICATION: <question>]` under "Open
  points" and stop at that point.
- Meet every acceptance criterion and run the "How to verify" commands before opening the pull request. Before
  asking for review, converge: check each criterion and scope item of the issue against the code and list it in
  the pull request with its evidence (a test, a command and its result), or as "not met" with a follow-up issue.
- A bug fix shows the symptom reproduced before the fix (a test or a command) and a verdict: verified, partial
  (what remains, with a follow-up issue) or failed. A test that fails without the fix is required too, but it
  proves the test; the verdict is about the symptom that was reported.
- If the work touches an invariant, say which one and how it still holds.

## Engineering and security practices

- **Untrusted input everywhere.** Validate at the boundary: CRD schema and CEL first, then typed parsing in
  Go. Never build shell commands, Git refs, paths or URLs by string concatenation from cluster data or model
  output; use typed APIs and allow-lists.
- **Secrets.** Never log, print, put in events, status, conditions or error messages anything that may hold a
  secret or token. Redact before storing and before anything reaches a model.
- **Errors and timeouts.** Wrap errors with context (`fmt.Errorf("...: %w", err)`), never `panic` in a
  reconcile path, and give every external call a `context` with a deadline and bounded retries.
- **Least privilege.** Minimal RBAC per controller, read-only by default, namespaced where possible; no
  `cluster-admin`, no wildcard verbs or resources.
- **Supply chain.** Pin GitHub Actions by commit SHA, keep `go.sum` committed, prefer the standard library and
  existing dependencies, and check new ones for maintenance and licence (Apache-2.0 compatible). Run
  `make vulncheck` (govulncheck) when dependencies change; the `govulncheck` CI job (Vulncheck workflow, also
  weekly) runs it with the Go release of the Dockerfile's builder stage and fails on any finding the code reaches.
  Trivy is installed by `hack/install-trivy.sh` from the official release with a pinned sha256, never through
  `aquasecurity/trivy-action` (its tags were force-pushed to malicious code in March 2026, GHSA-69fq-xp46-6x23); bump
  the version and the checksum together.
- **Static security checks.** `gosec` runs inside golangci-lint (`make lint`, the lint job of every Go pull
  request). Suppress a gosec finding on its line with `// #nosec G<nnn> -- <reason>` (that rule only), any
  other linter with `//nolint:<linter> // <reason>`; `nolintlint` fails a `nolint` without a linter or a
  reason, or one that no longer suppresses anything. A path-and-rule exclusion in `.golangci.yml` needs a
  comment saying why; never disable a rule for the whole code base.
- **Containers.** Distroless or scratch base images, non-root user, read-only root filesystem, all
  capabilities dropped, resource requests and limits set.
  The Dockerfile builds both binaries once (builder pinned by digest, Go release equal to go.mod's toolchain
  line) and has the targets `executor`, `analyzer` (one binary each) and `local` (both, the default). The
  Images workflow builds `executor` and `analyzer` for linux/amd64 on pull requests without pushing, and for
  linux/amd64 and linux/arm64 and pushes `ghcr.io/pkalin/shadewright-{executor,analyzer}` from main and `v*` tags only, in the one job
  that has `packages: write`. `make docker-build-images` builds both for the host platform.
- **CI scope.** `.github/workflows/ci.yml` is the one graph of a pull request and of a push to main. Its
  `changes` job runs `hack/ci-changed.sh` once on the files the pull request changes; the jobs that call the
  workflows holding the checks (lint, test, e2e, govulncheck, image build, chart tests, docs check, CodeQL, workflow lint) get
  `run: true|false` and skip when their paths did not change. A skipped job counts as passing. `ci-ok` needs
  all of them and is the single required CI check: it fails when `changes` failed or any job failed or was
  cancelled. The required checks are `ci-ok`, `Conventional PR title` and `Dependency review`. A new job goes
  into the `needs` of `ci-ok`. Never add trigger-level `paths` to `ci.yml`: a workflow that never starts leaves
  its required check pending forever. A path a job depends on that is missing from the script's groups lets a
  change skip its check, so extend the groups and `hack/ci-changed_test.sh` when a job starts reading a new
  directory. Concurrency (cancel a superseded pull request run) lives in `ci.yml` only. Called workflows
  (`lint.yml`, `test.yml`, `test-e2e.yml`, `vulncheck.yml`, `workflow-lint.yml`, `images-build.yml`,
  `chart-test.yml`, `docs-check.yml`) have no triggers of their own, except the weekly `vulncheck.yml` schedule and the push, weekly and manual triggers of `codeql.yml`. `images.yml`
  (publishes from main and `v*` tags) and `chart.yml` (runs the chart tests, publishes, then adds the digests to the release, on `v*` tags only) and
  `release-plugin.yml` (builds, signs and attaches the `kubectl-shadewright` archives to the release, on `v*` tags only)
  stay separate, because their signatures are verified against those file names.
  `codeql.yml` (CodeQL for `go` and `actions`) runs on pushes to main, weekly and by hand, and `ci.yml` calls it
  as the `codeql` job on pull requests whose files match the `codeql` group of `hack/ci-changed.sh` (Go code,
  `go.mod`, `go.sum`, workflows); docs-only pull requests skip it. `ci-ok` needs it, so the `code_scanning`
  ruleset rule stays off (it would block a pull request with no analysis). It needs the repository's code
  scanning default setup switched off (uploads from an advanced setup are refused while it is on).
  Scans that are not part of the pull request graph: `trivy.yml` scans the published executor and analyzer images (SARIF to code scanning, a category per
  image, findings do not fail the run, scanner errors do) on `v*` tags, after the Images workflow pushed them,
  and weekly against the latest release. `scorecard.yml` (OpenSSF Scorecard, results published for the README
  badge) runs on pushes to main, weekly and on branch protection changes, and contains only the steps the action
  allows with `publish_results`. `fuzz.yml` runs `make fuzz` weekly and by hand, and uploads any failing input as an
  artifact. The Trivy misconfiguration scan of the rendered chart and `config/` is the
  `trivy-config` job of `chart-test.yml`, so it runs only when the `chart` group changed; HIGH and CRITICAL
  findings fail it unless `.trivyignore.yaml` lists them with a statement of why.
- **Tests.** Every bug fix gets a test that fails without it; every new API rule gets a refusal test.
  `make test` runs with the race detector (`-race`), then runs the `internal/redact` timing tests, which skip
  under `-race`, once more without it. Dynamic analysis is the race detector and the Go fuzz targets: a `Fuzz`
  function in a parser or matcher of untrusted input goes into `FUZZ_TARGETS` in the Makefile (`make fuzz` fails
  when one is missing or an entry names no such function, `hack/fuzz-targets.sh`), and `fuzz.yml` runs each for a
  few minutes weekly. A failing input it finds is committed to the package's `testdata/fuzz/` with the fix.

## Design choices

- Providers sit behind interfaces so the project stays generic: `GitProvider` (GitHub first, Azure DevOps later), `DeliveryVerifier` (Flux first, Argo CD later), `ModelProvider` (Microsoft Foundry first). Do not import a provider SDK outside its adapter package.
- Layout beyond the Kubebuilder scaffold: `cmd/analyzer`, `cmd/kubectl-shadewright` (the read-only kubectl plugin, run by people with their own kubeconfig), `internal/analyzer` (with the collector), `internal/{incidents,incidentcontext,tools,analysis,policy,gitops,redact,providers,modules,groupversion,cmdutil}` (`incidentcontext` is the context builder, not named `context` so that it never shadows the standard library package; `tools` holds the read tools for the model; `analysis` is the analysis loop and its controller), `internal/testutil` (helpers for tests only), `test/eval` (the evaluation harness: recorded scenarios replayed through the analysis loop, docs/evaluation.md); planned: `internal/{remediation,verification}`, `test/scenarios`.
- `Incident` keeps deduplication state (fingerprint, counters, timestamps) and must survive restarts. Never put raw logs or large payloads in a CR.

## Commands

Go toolchain: the Dockerfile builder stage (`golang:<x.y.z>`, bumped by Dependabot) and the `toolchain` line of `go.mod` (read by setup-go in CI for lint, tests and govulncheck) must name the same release; `make lint` runs `hack/check-go-toolchain.sh` and fails when they differ, so a Dependabot bump of the image needs the `toolchain` line changed in the same pull request. Never raise the `go` line for this. Locally run `GOTOOLCHAIN=go<x.y.z> make lint test` with that release (a newer local Go can report different lint and vet results).

```bash
make manifests generate   # after editing api/**/*_types.go or markers; manifests also runs chart-crds
make chart-crds           # copy config/crd/bases into dist/chart/templates/crd (hack/chart-crds.sh); a removed CRD's template is deleted
make api-docs             # regenerate docs/reference/api.md from the API types
make verify-generated     # regenerate all of the above from scratch; fails on any diff, new or deleted file (CI runs it)
make lint-fix             # golangci-lint, config in .golangci.yml
make vulncheck            # govulncheck ./...; standard library findings depend on the local Go patch release
make test                 # unit + envtest (real kube-apiserver and etcd, no cluster needed); race detector, needs a C compiler
make fuzz                 # each Go fuzz target for FUZZTIME (default 30s), or only FUZZ=<dir>:<FuzzName>; CI runs it weekly (fuzz.yml)
KUBEBUILDER_ASSETS="$(bin/setup-envtest use 1.37.0 --bin-dir "$PWD/bin" -p path)" \
  go test ./internal/controller/... -ginkgo.focus "RemediationRequest"   # one spec; run `make test` once first, the bin dir must be absolute
make test-e2e             # installs the pinned kind (KIND_VERSION) into bin/ and creates an isolated cluster; CI runs it, not needed locally
make build                # bin/manager and bin/analyzer
make build-plugin         # bin/kubectl-shadewright, the read-only `kubectl shadewright status` plugin
make run                  # controller against the current kubeconfig context
```

Tests use Ginkgo and Gomega; envtest setup is in `internal/controller/suite_test.go`.

Docs site (Zensical, published to shadewright.io by `.github/workflows/docs.yml`):

```bash
python3 -m venv .venv && .venv/bin/pip install --require-hashes -r requirements-docs.txt
.venv/bin/zensical serve    # preview on localhost:8000
.venv/bin/zensical build --clean   # must say "No issues found"
make docs-check             # build, then check internal links, heading anchors and published URLs (hack/docs-check.py; the `docs-check` CI job runs it on pull requests; `docs.yml` only builds and publishes)
make docs-requirements      # after editing requirements-docs.in: regenerate the hash-locked requirements-docs.txt
```

## Docs and decisions

- Published URLs are kept: `hack/docs-published-paths.txt` lists every page path of shadewright.io, and the `docs-check` job of the PR graph (`docs-check.yml`, group `docs` of `hack/ci-changed.sh`) fails when a listed path no longer resolves to a page or a redirect page. Adding a page needs no change; moving or deleting one means a redirect, or removing its line from the list in the same pull request, which makes it a visible decision. Regenerate the list from a build with `python3 -I hack/docs-check.py list site`. External links are not checked on pull requests; `docs-links.yml` reports the ones that do not answer weekly, without failing.
- `docs/design.md` is the index of the system design, one page per section in `docs/design/` (named from the section heading, e.g. `docs/design/6-reconciliation.md`); section 15 (`docs/design/15-decisions-made-and-open-questions.md`) lists decisions made and open questions. `docs/adr/` holds decisions (template and index there). A new page or ADR must also be added to `nav` in `zensical.toml`, in the section that matches what a reader does with it.
- When a change implements or alters a design decision, update the design or add an ADR in the same PR.
- Docs are written with the code, in the same PR, not later:
  - Every package has a doc comment, and every exported identifier is documented.
  - A change that users see (CRD fields and their validation, flags, permissions, metrics, install or upgrade steps, behaviour of an action) updates the user docs under `docs/` in the same PR. Fields are documented in the Go types; the generated API reference picks them up.
  - A feature that can be tried in the lab comes with its runbook, or an update to an existing one.
  - The PR description says which docs changed, or why none needed to.
- Overview diagrams (architecture, phases, identity) are hand-written SVG in `docs/assets/diagrams/`, in the style of `architecture.svg`: dark background `#0e0e16`, boxes `#1d1b2b`, violet `#7c24fc` and `#a879ff` for the main flow, teal for systems outside the cluster, a `<title>` and `<desc>`, and alt text in the page. Wrap the image in a link to itself so it opens full size. Keep that link: it is the no-JS fallback, and `docs/javascripts/diagram-zoom.js` turns it, and every Mermaid block, into a zoomable overlay (no per-diagram markup needed). Render with headless Chrome and look at the result before committing.
- Sequence, state and small flow diagrams next to the text they explain are Mermaid code blocks in the Markdown. Give them no colours or `init` directives: the docs site themes them from `--md-mermaid-*` in `docs/stylesheets/brand.css`, and GitHub renders them in the reader's light or dark theme. Mark steps that are designed but not built yet (*(planned)* in sequences, dashed boxes with `classDef planned stroke-dasharray: 5 5` in flowcharts). Check that each diagram renders on the docs site (`zensical build` and a browser) before committing. No ASCII art; formulas stay as code blocks.
- Theme: the site has a dark and a light scheme (`docs/stylesheets/brand.css`; keep text at 4.5:1 or better in both and state the ratio of any new colour). Mark built and planned items with `<span class="sw-status sw-built">built</span>` and `<span class="sw-status sw-planned">planned</span>`. Acronyms get a tooltip from `includes/glossary.md`. `overrides/` holds the announcement bar, the 404 page and the link preview tags. Nothing may load from a third-party host: check with a browser's network log after theme changes.
- If a pull request changes a flow, a set of states or a permission that a diagram shows, it updates the diagram in the same pull request.

## Kubebuilder rules

- Never edit generated files: `config/crd/bases/*`, `dist/chart/templates/crd/*` (made by `make chart-crds`), `config/rbac/role.yaml`, `config/webhook/manifests.yaml`, `**/zz_generated.*.go`, `PROJECT`. Regenerate them.
- Never remove `// +kubebuilder:scaffold:*` markers; the CLI injects code there.
- Scaffold with the CLI (`kubebuilder create api`, `kubebuilder create webhook`), not by hand. External types (Flux, KEDA): `--controller=true --resource=false --external-api-path=... --external-api-module=...`.
- Status uses `metav1.Condition` with `observedGeneration`; `phase` is only a summary. Use the `/status` subresource.
- Reconcile is idempotent: re-fetch before update, rely on optimistic concurrency, use `.Owns()` / `.Watches()` rather than polling, and add a finalizer only for real external cleanup.
- Logging follows the Kubernetes style: capital first letter, no trailing period, past tense, name the object type, balanced key-value pairs.
- RBAC comes from `+kubebuilder:rbac` markers. Keep them minimal and per controller.

References: [Kubebuilder book](https://book.kubebuilder.io), [good practices](https://book.kubebuilder.io/reference/good-practices.html), [API conventions](https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md), [controller-runtime FAQ](https://github.com/kubernetes-sigs/controller-runtime/blob/main/FAQ.md).
