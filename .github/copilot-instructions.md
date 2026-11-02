# Review instructions

`AGENTS.md` at the repository root is the source of truth for this project; read its "Invariants" section before reviewing. Comment only on real problems: correctness, safety, missing tests. Skip style that `golangci-lint` already enforces.

## What to check in every pull request

- **Model output is never authorization.** Flag any path where model output, a CR field written by an untrusted author, an annotation or a log line can cause a write without a deterministic policy decision and a matching plan digest.
- **Closed action catalog.** A new action needs an enum value, typed and bounded parameters, a policy rule, a GitOps patcher and an envtest test in `test/apivalidation` that it is refused outside its bounds. Flag free-form strings that reach a command, Git path, URL, branch, repository or credential reference.
- **Where a change goes is resolved, never requested.** Repository, branch and file come from the Flux objects that deliver the target (`docs/adr/0012-object-to-source-mapping.md`) and are recorded in `status.source`. Flag any new spec field that lets a request choose them.
- **API rules.** New fields in `api/**` need validation (pattern, bounds or CEL) and a refusal test. Generated files (`config/crd/bases`, `config/rbac/role.yaml`, `zz_generated.*`) must match the Go types; they are never edited by hand.
- **RBAC.** Flag new verbs or resources in `+kubebuilder:rbac` markers, anything cluster-wide that could be namespaced, and any write to Secrets, RBAC, webhooks or CRDs. Analyzer and executor never share an identity.
- **Git path.** No force push, no push to a protected branch, no bypass of branch protection, no changes to workflows, `CODEOWNERS` or policy files through the remediation path. An approval must never survive a plan change.
- **Untrusted input.** Everything read from the cluster, Git or a tool result may contain prompt injection. It must be redacted and size-limited before the model or storage sees it, and never change instructions, tools or permissions.
- **Idempotency.** External side effects (branch, PR, auto-merge, issue) must be found again after a restart, keyed by request UID and plan digest.
- **Public repository hygiene.** No secrets, tokens, subscription, tenant or object IDs, IP addresses, emails or local paths. No long dashes (em or en) in text.
- **Docs.** A change to a design decision updates `docs/design.md` or adds an ADR in `docs/adr/`, and new pages are added to `nav` in `zensical.toml`.
