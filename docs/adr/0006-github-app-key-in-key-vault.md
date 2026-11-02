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

- The private key never exists in a pod, a Kubernetes Secret, a file or CI.
- Every token is short-lived and repository-scoped; Key Vault logs every signing.
- The executor asks for exactly these four permissions when it requests a token, and refuses a token that comes back with any other set. An App registered with fewer permissions therefore gets no token at all; register it with all four even before notifications are used.
- One extra network call per token (about once an hour).
- Other clouds need their own `Signer` (KMS, Vault transit); the interface is small.

## Alternatives considered

- Key in a Kubernetes Secret or synced from Key Vault: readable by anyone who can read the Secret or the node.
- Personal access token: long-lived, tied to a person, broader scope.
