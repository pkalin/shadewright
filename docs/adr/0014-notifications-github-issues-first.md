# ADR-0014: Notifications through GitHub issues first

- Status: Accepted
- Date: 2026-11-01

## Context

Some incidents end without a PR: no evidence, a refused action, or an unsupported source. A human still needs to hear about them. Microsoft Teams is the maintainer's preferred channel, but posting to Teams needs a work or school account with Workflows; it is not yet clear that the maintainer's account allows it.

## Decision

- Every outcome is also recorded as Kubernetes Events and CR status.
- The first `Notifier` adapter opens or updates one GitHub issue per incident in the fleet repository, through the same GitHub App (which then also needs `issues: write` on that repository).
- A Teams adapter (Workflows webhook) comes later behind the same interface, once an account that supports it is available. Slack and a generic webhook can follow.

## Consequences

- Notifications work with nothing but the GitHub App, and they are public in the lab.
- Issue bodies contain only redacted data, the same as PR bodies.

## Alternatives considered

- Teams first: blocked on account type.
- Events only: easy to miss.
