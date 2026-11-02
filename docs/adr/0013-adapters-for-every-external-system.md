# ADR-0013: An interface and adapters for every external system

- Status: Accepted
- Date: 2026-11-01

## Context

Shadewright talks to Git hosting, a GitOps controller, a model endpoint, metrics and log backends, notification channels, blob storage and a signing service. Each has several popular products, and users will run different ones.

## Decision

Every external system is reached through a small Go interface in the core (`GitProvider`, `DeliveryVerifier`, `ModelProvider`, `MetricsSource`, `LogSource`, `Notifier`, `AnalysisStore`, `Signer`), with one adapter package per product. Only adapter packages import vendor SDKs. Each interface has an in-memory fake used by the tests, and adapters are selected by configuration.

## Consequences

- Switching a product means adding an adapter, not changing the controller or the analyzer.
- Core logic is tested without network access.
- Interfaces must be designed around what Shadewright needs, not around the first vendor's API; that takes some care up front.

## Alternatives considered

- Call the first product directly and abstract later: faster at first, but the vendor's model leaks into the core.
