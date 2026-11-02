# ADR-0001: Record architecture decisions

- Status: Accepted
- Date: 2026-11-01

## Context

Shadewright is a public project whose value depends on safety properties that are easy to erode one convenient change at a time. Contributors, human or AI, need to know why things are the way they are before changing them.

## Decision

Record every significant decision as a short ADR in `docs/adr/`, numbered, using `template.md`. An ADR is never edited after acceptance except for its status; a new ADR supersedes it. Changes that weaken an invariant in `AGENTS.md` need an ADR first. Open questions live in [design section 15](../design.md#15-decisions-made-and-open-questions) until they become ADRs.

## Consequences

Decisions are reviewable in PRs and published on the docs site. Writing an ADR costs a few minutes per decision.

## Alternatives considered

- Decisions in issues only: hard to find later and not versioned with the code.
