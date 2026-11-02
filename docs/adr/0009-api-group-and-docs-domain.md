# ADR-0009: API group and docs domain

- Status: Accepted
- Date: 2026-11-01

## Context

The CRD API group is effectively permanent once users install it. The project owns `shadewright.io`, `shadewright.dev` and `shadewrith.io`.

## Decision

- Primary domain `shadewright.io`: the docs site and the API group `remediation.shadewright.io`. Annotations and labels use the `shadewright.io/` prefix.
- `shadewright.dev` and `shadewrith.io` redirect to `shadewright.io`.

## Consequences

One name to remember. Redirect rules for the other two domains live in DNS config (Cloudflare).

## Alternatives considered

- `shadewright.dev` for the API group: equally valid; one domain for everything is simpler.
