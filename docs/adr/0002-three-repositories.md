# ADR-0002: Three repositories

- Status: Accepted
- Date: 2026-11-01

## Context

The project has three parts with different owners of change, release cadences and permissions: the operator code, the lab infrastructure, and the GitOps repository the operator writes to.

## Decision

- `shadewright`: the operator (Go module, CRDs, Helm chart, docs site).
- `shadewright-azure`: Terraform for a reference lab in Azure.
- `shadewright-fleet`: the GitOps repository Flux syncs to the lab, and the only repository the operator's GitHub App is installed on.

## Consequences

- The GitHub App can be scoped to one repository that holds nothing but deployable config. An injected or buggy plan cannot touch the operator's own code, workflows or infrastructure.
- CI identities are separate: image publishing in `shadewright`, Azure federation in `shadewright-azure`.
- The operator has semver releases; infra and fleet change continuously.
- Cross-repo changes need coordinated PRs.

## Alternatives considered

- One monorepo: simpler to start, but the App would need write access to the same repo as the code and workflows, and path filters would be the only barrier.
- Two repos (fleet inside infra): mixes human Terraform and agent PRs in one repo with one set of rules.
