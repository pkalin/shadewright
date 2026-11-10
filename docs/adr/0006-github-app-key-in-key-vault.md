# ADR-0006: GitHub App with its private key in Key Vault

- Status: Accepted
- Date: 2026-11-01

## Context

The executor must create branches, commits and PRs, and enable auto-merge, in one repository. GitHub does not accept Microsoft Entra tokens. Personal access tokens are long-lived and tied to a person.

## Decision

- A GitHub App installed on `shadewright-fleet` only, with `contents: write`, `pull_requests: write`, `issues: write` (notifications, added by [ADR-0014](0014-notifications-github-issues-first.md)) and `metadata: read`, never on a ruleset bypass list. [ADR-0028](0028-reviewer-hints-through-the-pull-request.md) adds `checks: write` for the required review check, named in every token request since #539 (amended 2026-11-10, see Amendments).
- The App's private key is imported into Azure Key Vault as a **non-exportable RSA key**. The executor, through workload identity, may only use the key's `sign` operation. It signs the App JWT (RS256) in Key Vault and exchanges it for a one-hour installation token scoped to the one repository.
- The analyzer has no access to the key or the token.

## Consequences

- The private key never exists in a pod, a Kubernetes Secret or CI. The `.pem` file downloaded from GitHub at setup is imported and deleted right away.
- Every token is short-lived and repository-scoped. Key Vault logs every signing as an `AuditEvent`; the lab sends them to a Log Analytics workspace that outlives the cluster (table `AZKVAuditLogs`, operation `KeySign`; [shadewright-azure#7](https://github.com/pkalin/shadewright-azure/pull/7)).
- The executor names exactly these five permissions when it requests a token, so the token never carries more. GitHub refuses the request when the App lacks one of them, so register the App with all five even before notifications or reviewer hints are used (amended 2026-11-10, see Amendments).
- One extra network call per token (about once an hour).
- Other clouds need their own `Signer` (KMS, Vault transit); the interface is small.

## Alternatives considered

- Key in a Kubernetes Secret or synced from Key Vault: readable by anyone who can read the Secret or the node.
- Personal access token: long-lived, tied to a person, broader scope.

## Amendments

- 2026-11-02: `issues: write` added to the permissions, as [ADR-0014](0014-notifications-github-issues-first.md) requires for notifications, with the consequence that the App must be registered with all four ([#134](https://github.com/pkalin/shadewright/pull/134)).
- 2026-11-02: the key does exist as a file briefly at setup; the consequence now says so. Key Vault audit logging is not enabled in the lab yet; the consequence now says so and links the issue.
- 2026-11-03: the audit events are routed since [shadewright-azure#7](https://github.com/pkalin/shadewright-azure/pull/7); the consequence now says where they go.
- 2026-11-03: the audit operation is named `KeySign`, not `Sign`, as Key Vault logs key operations in `Key<action>` form ([Azure Key Vault logging](https://learn.microsoft.com/azure/key-vault/general/logging)); the consequence now names it correctly ([#169](https://github.com/pkalin/shadewright/pull/169)).
- 2026-11-08: `checks: write` planned as a fifth permission, decided by [ADR-0028](0028-reviewer-hints-through-the-pull-request.md) (the required check run `shadewright/review`).
- 2026-11-09: since #539 the executor names `checks: write` in every token request, beside the four above, so the App must be registered with it before it runs a release with #539: GitHub refuses a token request that names a permission the App lacks. The check run is created only on the heads a handoff pushed, which needs reviewer hints, not enabled in the shipped binary.
- 2026-11-10: the 2026-11-09 entry is corrected: with reviewer hints on, the executor creates the check run `shadewright/review` on every head of the pull requests it opened, adopted or took over in a handoff, not only on the heads a handoff pushed. Reviewer hints are off by default and turned on by the executor flag `--reviewer-hints` (chart value `executor.reviewerHints.enabled`, [#631](https://github.com/pkalin/shadewright/issues/631)). The permissions are unchanged.
- 2026-11-10: the Decision and the Consequences now count five permissions, as the token request has named them since #539 (`checks: write` beside the four of design 8.4); they still said four, and the Decision still called `checks: write` planned. Nothing else changes.
