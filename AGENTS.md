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

## Working agreement

- Every change goes through a branch and a pull request; `main` is protected by a ruleset (required checks,
  squash merge, no force push). Keep pull requests small and say why in the description.
- Run the checks below locally before opening a pull request, and add or update tests in the same pull request.
- Leave the code safer than you found it: when you notice a security or reliability problem nearby, fix it
  or raise it in the pull request, do not ignore it.
- Prefer boring, well-known solutions and the project's existing patterns over new dependencies or clever code.

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

## Kubebuilder rules

- Never edit generated files: `config/crd/bases/*`, `config/rbac/role.yaml`, `config/webhook/manifests.yaml`, `**/zz_generated.*.go`, `PROJECT`. Regenerate them.
- Never remove `// +kubebuilder:scaffold:*` markers; the CLI injects code there.
- Scaffold with the CLI (`kubebuilder create api`, `kubebuilder create webhook`), not by hand. External types (Flux, KEDA): `--controller=true --resource=false --external-api-path=... --external-api-module=...`.
- Status uses `metav1.Condition` with `observedGeneration`; `phase` is only a summary. Use the `/status` subresource.
- Reconcile is idempotent: re-fetch before update, rely on optimistic concurrency, use `.Owns()` / `.Watches()` rather than polling, and add a finalizer only for real external cleanup.
- Logging follows the Kubernetes style: capital first letter, no trailing period, past tense, name the object type, balanced key-value pairs.
- RBAC comes from `+kubebuilder:rbac` markers. Keep them minimal and per controller.

References: [Kubebuilder book](https://book.kubebuilder.io), [good practices](https://book.kubebuilder.io/reference/good-practices.html), [API conventions](https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md), [controller-runtime FAQ](https://github.com/kubernetes-sigs/controller-runtime/blob/main/FAQ.md).
