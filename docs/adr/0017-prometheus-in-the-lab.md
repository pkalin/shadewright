# ADR-0017: Prometheus in the lab

- Status: Accepted
- Date: 2026-11-01

## Context

Several scenarios (KEDA backlog, OOM trends, probe failures) need metrics, and the analyzer's `query_metrics` tool needs a query API. The lab has one small node.

## Decision

Run a minimal Prometheus in the lab cluster from the fleet repo: the Prometheus server with kube-state-metrics and the kubelet/cAdvisor targets, short retention, no Alertmanager or Grafana. The analyzer reaches it through the `MetricsSource` Prometheus adapter, with approved query templates only. Grafana (or a hosted backend) is added later only if a human dashboard is needed.

## Consequences

- Scenarios can rely on real metrics; the same adapter works with any Prometheus-compatible API (Grafana Cloud, Azure Monitor managed Prometheus, Thanos, Mimir).
- Prometheus needs memory on the node; size and retention are tuned in the fleet repo.

## Alternatives considered

- `metrics.k8s.io` only: current CPU and memory, no history or custom metrics.
- Hosted Prometheus from the start: cost or account setup before it is needed.
