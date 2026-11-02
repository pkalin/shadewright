# Shadewright: guide for coding agents

Shadewright is a Kubernetes operator that watches a cluster, groups repeated symptoms into incidents, asks an LLM for a diagnosis, and proposes a fix as a typed `RemediationRequest`. Approved fixes are applied only as pull requests to the GitOps repo; the GitOps controller (Flux first, Argo CD later) deploys them, and Shadewright verifies the effect.

## Invariants (read before changing anything)

These are the reason the project exists. A change that weakens one needs an ADR in `docs/adr/` first.

1. **Model output is never authorization.** The analyzer may only submit a proposal. Deterministic policy code decides whether it may run; the executor acts only on a plan whose digest matches the approved one.
2. **Actions are a closed catalog.** Each action is an enum value with typed, bounded parameters, a policy rule, a GitOps patcher and a test that it is refused outside its bounds. No free-form commands, no `exec`, no direct `apply` against the cluster.
3. **Changes go through Git only.** Branch and PR, never a push to a protected branch, never bypassing branch protection or required checks. No changes to policies, pipelines, RBAC or secrets through the remediation path.
4. **Everything from the cluster is untrusted input.** Events, logs, annotations, Git contents and tool results may contain prompt injection. They never change instructions, tools, permissions or the action catalog. Secrets and tokens are redacted before storage and before the model sees anything.
5. **Reconciliation is idempotent and never assumes exactly once.** Branch names and PR markers derive from the request UID and plan digest, so a restart finds the existing PR instead of opening another.
6. **Separate identities.** Analyzer: read-only cluster access plus model inference. Executor: CR status plus the Git App. Never `cluster-admin`, never one token for both.

## Commits

Every commit on `main` follows [Conventional Commits](https://www.conventionalcommits.org/en/v1.0.0/):
`type(scope)!: summary`. Pull requests are squash-merged with the pull request title as the commit message,
so the **pull request title** is what counts; a required check (`Conventional PR title`) validates it.

- Types: `feat`, `fix`, `perf`, `refactor`, `test`, `docs`, `build`, `ci`, `chore`, `revert`.
- Scope is optional; when used it is one of: `api`, `analyzer`, `policy`, `gitops`, `platforms`, `integrations`, `docs`, `ci`, `deps`.
- Summary in English, imperative, starting lower case, no full stop: `feat(policy): refuse decreases by default`.
- `!` after the type or scope, plus a `BREAKING CHANGE:` footer in the description, for incompatible changes
  (API fields, CRD schema, flags, Helm values).
- Releases are generated from these commits (release-please, epic #22): `feat` gives a minor version, `fix` and
  `perf` a patch, `!` or a `BREAKING CHANGE:` footer a breaking release (a minor one while the version is
  0.x). `revert` is listed too; `refactor`, `test`, `docs`, `build`, `ci` and `chore` stay out of the changelog.
- The release workflow keeps one release pull request open (opened by the `shadewright-release` App). Merging it
  is the release: it tags `vX.Y.Z`, updates `CHANGELOG.md` and creates the GitHub release. Never tag by hand and
  never edit `CHANGELOG.md` or `.release-please-manifest.json` outside that pull request.
- Merge the release pull request when the milestone of that version is done, not on every change.

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
date and a one-line goal; the design stages (`docs/design.md` section 13) are tracked by the epics instead.
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

- Pick an open issue labelled `agent-ready` whose "Depends on" issues are closed. Read the linked design
  sections and the ADRs the issue and its epic list first.
- One issue, one branch (`<issue-number>-<short-slug>`), one pull request with `Closes #<n>` in the body.
- Stay inside the issue's scope. If something outside it needs to change, or the issue is ambiguous, say so
  in the pull request or the issue instead of guessing.
- Meet every acceptance criterion and run the "How to verify" commands before opening the pull request.
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
  `govulncheck ./...` when dependencies change.
- **Containers.** Distroless or scratch base images, non-root user, read-only root filesystem, all
  capabilities dropped, resource requests and limits set.
- **Tests.** Every bug fix gets a test that fails without it; every new API rule gets a refusal test.

## Design choices

- Providers sit behind interfaces so the project stays generic: `GitProvider` (GitHub first, Azure DevOps later), `DeliveryVerifier` (Flux first, Argo CD later), `ModelProvider` (Microsoft Foundry first). Do not import a provider SDK outside its adapter package.
- Planned layout beyond the Kubebuilder scaffold: `cmd/analyzer`, `internal/{collector,incidents,context,analysis,policy,remediation,gitops,verification,tools}`, `test/scenarios`.
- `Incident` keeps deduplication state (fingerprint, counters, timestamps) and must survive restarts. Never put raw logs or large payloads in a CR.

## Commands

```bash
make manifests generate   # after editing api/**/*_types.go or markers
make api-docs             # regenerate docs/reference/api.md from the API types; CI fails if it is stale
make lint-fix             # golangci-lint, config in .golangci.yml
make test                 # unit + envtest (real kube-apiserver and etcd, no cluster needed)
KUBEBUILDER_ASSETS="$(bin/setup-envtest use 1.37.0 --bin-dir "$PWD/bin" -p path)" \
  go test ./internal/controller/... -ginkgo.focus "RemediationRequest"   # one spec; run `make test` once first, the bin dir must be absolute
make test-e2e             # creates an isolated Kind cluster; CI runs it, not needed locally
make build                # bin/manager
make run                  # controller against the current kubeconfig context
```

Tests use Ginkgo and Gomega; envtest setup is in `internal/controller/suite_test.go`.

Docs site (Zensical, published to shadewright.io by `.github/workflows/docs.yml`):

```bash
python3 -m venv .venv && .venv/bin/pip install -r requirements-docs.txt
.venv/bin/zensical serve    # preview on localhost:8000
.venv/bin/zensical build --clean   # must say "No issues found"
```

## Docs and decisions

- `docs/design.md` is the system design; its section 15 lists decisions made and open questions. `docs/adr/` holds decisions (template and index there). A new page or ADR must also be added to `nav` in `zensical.toml`.
- When a change implements or alters a design decision, update the design or add an ADR in the same PR.
- Diagrams are hand-written SVG in `docs/assets/diagrams/`, in the style of `architecture.svg`: dark background `#0e0e16`, boxes `#1d1b2b`, violet `#7c24fc` and `#a879ff` for the main flow, teal for systems outside the cluster, a `<title>` and `<desc>`, and alt text in the page. Wrap the image in a link to itself so it opens full size. No ASCII art and no Mermaid; formulas stay as code blocks. Render with headless Chrome and look at the result before committing.

## Kubebuilder rules

- Never edit generated files: `config/crd/bases/*`, `config/rbac/role.yaml`, `config/webhook/manifests.yaml`, `**/zz_generated.*.go`, `PROJECT`. Regenerate them.
- Never remove `// +kubebuilder:scaffold:*` markers; the CLI injects code there.
- Scaffold with the CLI (`kubebuilder create api`, `kubebuilder create webhook`), not by hand. External types (Flux, KEDA): `--controller=true --resource=false --external-api-path=... --external-api-module=...`.
- Status uses `metav1.Condition` with `observedGeneration`; `phase` is only a summary. Use the `/status` subresource.
- Reconcile is idempotent: re-fetch before update, rely on optimistic concurrency, use `.Owns()` / `.Watches()` rather than polling, and add a finalizer only for real external cleanup.
- Logging follows the Kubernetes style: capital first letter, no trailing period, past tense, name the object type, balanced key-value pairs.
- RBAC comes from `+kubebuilder:rbac` markers. Keep them minimal and per controller.

References: [Kubebuilder book](https://book.kubebuilder.io), [good practices](https://book.kubebuilder.io/reference/good-practices.html), [API conventions](https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md), [controller-runtime FAQ](https://github.com/kubernetes-sigs/controller-runtime/blob/main/FAQ.md).
