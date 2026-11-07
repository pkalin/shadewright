---
title: Traces reference
---

# Traces reference

The analyzer can export one OpenTelemetry trace per analysis ([#90](https://github.com/pkalin/shadewright/issues/90)): a span for each phase, model call and tool call, with token counts and durations. Tracing is **off by default**. The executor makes no traces.

## Turning it on

The analyzer reads the standard OpenTelemetry environment variables. Unlike the OpenTelemetry default, an unset `OTEL_TRACES_EXPORTER` means no traces.

| Variable | Meaning |
| --- | --- |
| `OTEL_TRACES_EXPORTER` | `otlp` turns tracing on. Unset or `none`: off. Any other value stops the start |
| `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` or `OTEL_EXPORTER_OTLP_ENDPOINT` | The collector's OTLP gRPC endpoint, such as `http://otel-collector.observability.svc:4317` (`http` sends plaintext gRPC, `https` TLS). Required with `otlp`; the chart requires the port |
| `OTEL_EXPORTER_OTLP_TRACES_PROTOCOL` or `OTEL_EXPORTER_OTLP_PROTOCOL` | Unset or `grpc`. `http/protobuf` is not supported and stops the start |
| `OTEL_SDK_DISABLED` | `true` turns tracing off whatever else is set |
| `OTEL_SERVICE_NAME`, `OTEL_RESOURCE_ATTRIBUTES` | The resource; `service.name` is `shadewright-analyzer` unless these name another |
| `OTEL_TRACES_SAMPLER`, `OTEL_TRACES_SAMPLER_ARG` | The sampler; the default records every analysis |
| `OTEL_BSP_*`, `OTEL_EXPORTER_OTLP_TIMEOUT`, `OTEL_EXPORTER_OTLP_HEADERS`, `OTEL_EXPORTER_OTLP_CERTIFICATE`, `OTEL_EXPORTER_OTLP_COMPRESSION` | Read by the batch processor and the exporter as the OpenTelemetry specification says |

With the Helm chart, set the endpoint and open the egress to the collector:

```yaml
analyzer:
  tracing:
    endpoint: http://otel-collector.observability.svc:4317
networkPolicy:
  analyzer:
    traceCollector:
      enabled: true
      namespace: observability
      # podLabels and port default to the opentelemetry-collector chart and 4317
```

`analyzer.tracing.endpoint` sets `OTEL_TRACES_EXPORTER=otlp`, `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` and `service.version` (the chart's `appVersion`) on the analyzer only. `networkPolicy.analyzer.traceCollector` adds one rule to `allow-analyzer-egress` (never to the executor's policy), to the collector Pods you name in one namespace on one port. With the NetworkPolicies on, the chart refuses an endpoint without that rule, since the default-deny policy would drop every export. If the collector's namespace has its own ingress policy, allow the analyzer there too.

The log line `Analysis tracing` at start says whether tracing is on. A tracing value the analyzer does not support stops the start with `Invalid tracing configuration`.

## It never slows or fails an analysis

Spans go to a bounded in-memory queue (2048 spans by default, `OTEL_BSP_MAX_QUEUE_SIZE`); when it is full new spans are dropped, never waited for. A background goroutine exports them in batches with a timeout. The gRPC connection is made lazily, so a collector that is down or unreachable does not hold up the start; an export that fails is logged (`OpenTelemetry reported an error`, at most once a minute with the number of errors not logged since) and changes nothing else. When the analyzer stops it flushes what is queued for at most 3 seconds, then drops the rest.

## What a span carries

Spans carry only IDs, counts, durations, fixed result labels from code, and the model and deployment names the operator configured (invariant 4). Never a prompt, a context block, log or event text, a tool's arguments or answer, a reply of the model, an error message or a token. A failure is recorded as the span's error status with a fixed description, never with the error's text, since an error can quote what it failed on. A tool the model asked for that the analysis did not offer is named `unknown`. `internal/analysis/trace_test.go` plants a secret and prompt text in the context, the tool results, the tool arguments, the replies and an error, and fails if any span name, attribute, status or event holds it.

## Spans

```text
shadewright.analysis                 the whole analysis
├── shadewright.analysis.context     the context builder
├── shadewright.analysis.reserve     the unit of the daily analysis budget
├── shadewright.analysis.triage      the triage phase
│   └── shadewright.model.call       each model call, retries included
└── shadewright.analysis.escalation  the escalation phase
    ├── shadewright.model.call
    └── shadewright.tool.call        each tool call the model asked for
```

An analysis that ends early (a runbook matched, no model configured, the budget spent) has only the spans of the steps it ran.

| Span | Attributes |
| --- | --- |
| `shadewright.analysis` | `shadewright.incident.name`, `shadewright.incident.uid`, `shadewright.analysis.run` (the claim's `lastAnalysisTime`), `shadewright.analysis.reason` (how the analysis loop ended), `shadewright.analysis.escalated`, `shadewright.analysis.proposed` (the loop made a plan), `shadewright.analysis.model_calls`, `shadewright.analysis.tool_calls`, `shadewright.analysis.tokens` (charged), `shadewright.analysis.models` (the models that answered), `shadewright.analysis.prompt_version`. Error status only when the analysis was stopped (shutdown or lost leadership). The span ends with the loop, before the controller creates the request: when that create fails, the `Analyzed` condition says `RequestNotCreated` while the span still says the loop's reason and `proposed` true |
| `shadewright.analysis.context` | `shadewright.result` (`ok`, `failed`, `canceled`), `shadewright.context.blocks`, `shadewright.context.bytes`, `shadewright.context.truncated`, `shadewright.context.failed_sources`, `shadewright.context.pods` |
| `shadewright.analysis.reserve` | `shadewright.result` (`ok`, `spent`, `unavailable`, `canceled`) |
| `shadewright.model.call` | `gen_ai.operation.name` (`chat`), `shadewright.model.role` (`triage`, `escalation`), `gen_ai.request.model` (the deployment name), `gen_ai.response.model` (the model the reply named), `gen_ai.request.max_tokens`, `gen_ai.usage.input_tokens` and `gen_ai.usage.output_tokens` (when reported), `shadewright.model.charged_tokens`, `shadewright.model.usage_unknown` (the reply reported no usage), `shadewright.model.attempt`, `shadewright.model.tool_calls` (how many the reply asked for), `shadewright.result` (`ok`, `retry`, `canceled` or the hand-off reason, such as `ModelUnavailable` or `InvalidOutput`) |
| `shadewright.tool.call` | `gen_ai.tool.name` (or `unknown`), `shadewright.result` (`ok`, `refused`, `failed`, `canceled`), `shadewright.tool.status` (`ok`, `refused`, `not found`, `not available`, `could not fetch`), `shadewright.tool.quotable` (the answer may be cited as evidence), `shadewright.tool.outside_attribution` |

The `gen_ai.*` names follow the OpenTelemetry semantic conventions for generative AI. The Incident name and UID are the same values the analyzer's log lines and the analysis record carry; use them to go from a trace to the Incident (`kubectl shadewright status`) or to the record.
