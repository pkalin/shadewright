# ADR-0021: Amending accepted ADRs

- Status: Accepted
- Date: 2026-11-02

## Context

[ADR-0001](0001-record-architecture-decisions.md) says an accepted ADR is never edited except for its status. In practice two accepted ADRs were edited on 2026-11-02: [ADR-0012](0012-object-to-source-mapping.md) gained implementation details that the decision left open, and [ADR-0006](0006-github-app-key-in-key-vault.md) was brought in line with [ADR-0014](0014-notifications-github-issues-first.md), which had added a permission. Neither reversed its decision, and a separate ADR for each would scatter one decision over several pages. The strict rule was not followed, so it gave a false sense of history.

## Decision

An accepted ADR may be amended when the change clarifies it, records what a later ADR changed, or corrects a statement that turned out to be wrong, as long as the decision itself stands.

- The text may be updated in place, so a reader of the Decision sees what holds now.
- Every amendment gets a dated entry in an `## Amendments` section at the end of the ADR: the date, what changed, and why (with a link to the later ADR or the PR).
- A later ADR that changes an earlier one links to it, and the earlier one links back.
- Reversing or replacing a decision still needs a new ADR that supersedes the old one, as ADR-0001 says.

## Consequences

- The Decision section of an ADR is current, and its history is in the Amendments section and in Git.
- Reviewers check that an amendment does not quietly reverse a decision; if it does, it needs a superseding ADR instead.

## Alternatives considered

- Keep the strict rule and add an amending ADR for every clarification: correct history, but a reader has to collect one decision from several pages, and small corrections tend not to be made at all.

## Amendments

None.
