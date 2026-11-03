# ADR-0006: GitHub App with its private key in Key Vault

- Status: Accepted
- Date: 2026-11-01

## Context

The executor must create branches, commits and PRs, and enable auto-merge, in one repository. GitHub does not accept Microsoft Entra tokens. Personal access tokens are long-lived and tied to a person.

## Decision

- A GitHub App installed on `shadewright-fleet` only, with `contents: write`, `pull_requests: write`, `issues: write` (notifications, added by [ADR-0014](0014-notifications-github-issues-first.md)) and `metadata: read`, never on a ruleset bypass list.
- The App's private key is imported into Azure Key Vault as a **non-exportable RSA key**. The executor, through workload identity, may only use the key's `sign` operation. It signs the App JWT (RS256) in Key Vault and exchanges it for a one-hour installation token scoped to the one repository.
- The analyzer has no access to the key or the token.

## Consequences

- The private key never exists in a pod, a Kubernetes Secret or CI. The `.pem` file downloaded from GitHub at setup is imported and deleted right away.
- Every token is short-lived and repository-scoped. Key Vault logs every signing as an `AuditEvent`; the lab sends them to a Log Analytics workspace that outlives the cluster (table `AZKVAuditLogs`, operation `Sign`; [shadewright-azure#7](https://github.com/pkalin/shadewright-azure/pull/7)).
- The executor names exactly these four permissions when it requests a token, so the token never carries more. GitHub refuses the request when the App lacks one of them, so register the App with all four even before notifications are used.
- One extra network call per token (about once an hour).
- Other clouds need their own `Signer` (KMS, Vault transit); the interface is small.

## Alternatives considered

- Key in a Kubernetes Secret or synced from Key Vault: readable by anyone who can read the Secret or the node.
- Personal access token: long-lived, tied to a person, broader scope.

## Amendments

- 2026-11-02: `issues: write` added to the permissions, as [ADR-0014](0014-notifications-github-issues-first.md) requires for notifications, with the consequence that the App must be registered with all four ([#134](https://github.com/pkalin/shadewright/pull/134)).
- 2026-11-02: the key does exist as a file briefly at setup; the consequence now says so. Key Vault audit logging is not enabled in the lab yet; the consequence now says so and links the issue.
- 2026-11-03: the audit events are routed since [shadewright-azure#7](https://github.com/pkalin/shadewright-azure/pull/7); the consequence now says where they go.
