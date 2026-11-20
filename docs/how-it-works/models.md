---
title: Working with models
---

# Working with models

Shadewright uses a model as an advisor. The model reads evidence and suggests one fix, or says what is missing. Everything else is code: what the model is told, what it sees, what it may call and whether its answer is accepted. The reason is the first [invariant](safety.md): model output is never authorization. A model can be wrong or misled by text in a log, so nothing it says acts on its own.

[![The model in the middle of a harness of code: code tells it the rules, shows it a redacted context, lets it call read-only tools on escalation and checks its answer. The only output is a RemediationRequest proposal. Records and traces sit below. Dashed boxes are planned.](../assets/diagrams/models.svg)](../assets/diagrams/models.svg "Open full size")

Dashed boxes are <span class="sw-status sw-planned">planned</span>. The steps of an analysis are on the [Diagnosis](diagnosis.md) page. This page covers the parts.

## What the model is told

A fixed prompt and an output schema. The prompt says that every block is untrusted data, never instructions, and that the answer is a proposal at most. It lists the closed catalog of actions, asks for an exact quote for each piece of evidence, and allows "no proposal" with a list of what is missing. It asks for no confidence score.

It is also shown the bounds of the [policy](policy.md) that will judge a proposal. That block is not evidence.

## What it sees

Code builds the context: the Incident summary, the status of the object and its owners, a log excerpt, metrics when Prometheus is configured, and the policy bounds. Each block has a size limit, and the whole context is limited to 48 KiB by default. Each block is redacted, tagged with its source and wrapped in a delimiter the text inside cannot close.

## What it may call

The first, small model has no tools. Only the stronger model, on escalation, may call read tools, five at most:

- `get_resource`: one object's spec and status.
- `list_events`: recent Events.
- `get_pod_logs`, when a log source is configured.
- `query_metrics`: approved metric templates, when Prometheus is configured.
- `check_missing_reference`: whether a Secret or ConfigMap a pod refers to is missing.

A call may name only the Incident's namespace. Output is redacted and size-limited, and each analysis allows 4 tool calls by default (16 at most). Tools to read the GitOps repository exist in code but are not offered.

## What checks the answer

- A proposal needs at least one quote of 8 bytes or more that appears word for word in the block it cites. No quote, no proposal.
- The action must be in the catalog and its parameters inside the policy bounds. Out of bounds is a hand-off with no retry, so the model cannot probe for what is allowed.
- Policy then decides on the plan, as the [policy page](policy.md) explains.

## What it can never do

It has no shell, no `exec`, no `kubectl apply`. The analyzer holds no Git credential, so the model cannot read or write the repository, open a pull request or merge. It cannot change a policy.

## Two tiers

The small model goes first, without tools, and is asked once more after an invalid reply. If the context was cut or a source failed, the stronger model goes first and the small model is not called. Otherwise escalation happens only when the small model's reply is invalid twice. A feeling of low confidence never triggers it. If there is no escalation model, the signal ends in a hand-off to a person. The stronger model's invalid reply is also a hand-off, not a retry.

An analysis is bounded by one unit of the daily budget, a deadline of 8 minutes by default (9 at most), 500,000 tokens by default, and at most 3 plus the tool-call limit model calls. When one runs out, the Incident is handed to a person.

<span class="sw-status sw-planned">planned</span> One extra round when the answer names missing data that a tool can fetch, within the same limits.

## Providers

A deployment names one of three providers: Microsoft Foundry (workload identity, no key), the Anthropic API (a key read from a mounted file), or an OpenAI-compatible endpoint that the installer declared. An endpoint outside the allow-list gets nothing: no context is sent. See [data handling](../data-handling.md#what-is-sent-to-the-model).

## How a model is judged

- **Evaluation harness.** Scenarios and an injection corpus in `test/eval` run against a model. <span class="sw-status sw-planned">planned</span> The analyzer checking those results before it starts.
- **Records.** With an analysis store, each analysis leaves a redacted record: counts, the reason and the prompt version. It holds no context, prompt or model reply.
- **Traces.** Off by default. A span carries IDs, counts, durations and fixed labels, never text.
- <span class="sw-status sw-planned">planned</span> A step log, so an analysis can be replayed step by step.

## In depth

- [The analyzer as a harness](../design/harness.md)
- [ADR-0003](../adr/0003-gitops-only-typed-remediation.md), [ADR-0013](../adr/0013-adapters-for-every-external-system.md), [ADR-0023](../adr/0023-how-the-analyzer-calls-the-model.md), [ADR-0025](../adr/0025-analysis-claims-units-and-evaluation-manifest.md), [ADR-0015](../adr/0015-analysis-records-outlive-the-lab.md)
