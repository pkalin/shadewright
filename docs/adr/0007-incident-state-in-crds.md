# ADR-0007: Incident state in CRDs for the MVP

- Status: Accepted
- Date: 2026-11-01

## Context

Deduplication state (fingerprints, counters, cooldowns) must survive restarts, or every restart causes an analysis storm. The lab is small and should not need a database.

## Decision

Store incidents as a namespaced `Incident` CR with bounded retention: fingerprint, counters, first and last seen, correlated owner, and references to analyses and requests. No raw logs, transcripts or large payloads in etcd. Revisit when incident volume or retention needs grow.

## Consequences

- No extra infrastructure; state is visible with `kubectl`.
- etcd size and write rate limit how many incidents can be kept; the collector must batch counter updates.
- An external store becomes an `IncidentStore` implementation later.

## Alternatives considered

- External database (PostgreSQL, Table Storage): more durable and queryable, but more cost and another dependency for a lab.
- In-memory only: loses state on restart.
