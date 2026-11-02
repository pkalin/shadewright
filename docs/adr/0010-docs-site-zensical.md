# ADR-0010: Docs site built with Zensical on GitHub Pages

- Status: Accepted
- Date: 2026-11-01

## Context

The design, ADRs and user docs should be a website at `shadewright.io`, built from Markdown in the operator repo. Material for MkDocs, the usual choice, entered maintenance mode in November 2025 with fixes promised for about a year; its team now builds Zensical, which keeps the same Markdown and theme features.

## Decision

- Build `docs/` with Zensical (`zensical.toml`), pinned to an exact version, and publish it with GitHub Actions to GitHub Pages, with the custom domain `shadewright.io`.
- Markdown stays plain and portable (no Zensical-only syntax beyond admonitions and code blocks), so switching back to MkDocs or another generator stays cheap.

## Consequences

- No hosting cost; the site deploys on every push to `main` that touches `docs/`.
- Zensical is pre-1.0 (0.0.74 when this ADR was written): expect breaking changes, pin the version and upgrade on purpose.

## Alternatives considered

- MkDocs with Material: mature, but in maintenance mode.
- Hugo or Astro: more flexible, but more to maintain for a docs-only site.
