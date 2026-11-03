# ADR-0007: Incident state in CRDs for the MVP

- Status: Accepted
- Date: 2026-11-01

## Context

Deduplication state (fingerprints, counters, cooldowns) must survive restarts, or every restart causes an analysis storm. The lab is small and should not need a database.

## Decision

Store incidents as a namespaced `Incident` CR in the operator namespace, bounded in number: fingerprint, counters, first and last seen, correlated owner, and references to analyses and requests. No raw logs, transcripts or large payloads in etcd. Revisit when incident volume or retention needs grow.

## Consequences

- No extra infrastructure; state is visible with `kubectl`.
- etcd size and write rate limit how many incidents can be kept; the collector must batch counter updates.
- An external store becomes an `IncidentStore` implementation later.

## Alternatives considered

- External database (PostgreSQL, Table Storage): more durable and queryable, but more cost and another dependency for a lab.
- In-memory only: loses state on restart.

## Amendments

- 2026-11-03: the decision now says where Incidents live: in the operator namespace, next to the requests that point at them (`IncidentRef` is same-namespace), not in the namespace of the target. The analyzer may write them only there; the target's namespace is part of the fingerprint, not of the object's location ([#155](https://github.com/pkalin/shadewright/issues/155)). This narrows a grant; nothing is widened.
- 2026-11-03: "bounded retention" in the decision is now "bounded in number". The Incident controller keeps at most `--max-incidents` Incidents (500 by default) and refuses a new fingerprint at that cap; it deletes none, and the analyzer has no `delete` on them. When old Incidents should go, and who deletes them, is open (design section 15, question 8). This corrects the text to what is built ([#196](https://github.com/pkalin/shadewright/pull/196)); nothing is widened.
