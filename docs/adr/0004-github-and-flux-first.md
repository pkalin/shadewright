# ADR-0004: GitHub and Flux first, behind interfaces

- Status: Accepted
- Date: 2026-11-01

## Context

The original plan assumed Azure DevOps and Argo CD. The project is public on GitHub, and the maintainer's existing clusters run Flux. Supporting four combinations from the start would slow the MVP.

## Decision

Implement GitHub (`GitProvider`) and Flux (`DeliveryVerifier`) first. Define both as Go interfaces from day one, so Azure DevOps and Argo CD can be added as adapters without touching the controller. Provider SDKs are imported only in their adapter packages.

## Consequences

- The demo works end to end on free GitHub features (rulesets, `CODEOWNERS`, auto-merge, Actions for public repos).
- Azure DevOps-specific behaviour (auto-complete identity, branch policies with path filters) is designed later, against the same `approvalRequired` semantics.

## Alternatives considered

- Azure DevOps first, as in the original plan: harder for outside users to try, and not where the code lives.
- Argo CD first: one more large upstream app in a small lab cluster, and not what the maintainer runs.
