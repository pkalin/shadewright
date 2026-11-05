# 11. Model

This section covers how the analyzer is set up against a managed model endpoint (11.1) and which model does which job (11.2). Prices, dates and other numbers that matter only when you build or compare are in the table "Details" at the end (11.3).

## 11.1. Setup

The analyzer runs locally during development and in the cluster later, always against a managed endpoint. Seven rules apply.

1. **Own resource and budget.** A separate Foundry resource and budget for the lab.
2. **A capable model.** It is available in the chosen region and deployment type, with structured outputs and tool calling.
3. **Pay-per-token first.** No provisioned throughput without measured steady load.
4. **Entra ID authentication.** The identity gets the minimum data-plane role for inference.
5. **Settings in config, not in code** (below).
6. **Failures are handled and tested.** Timeouts, throttling, malformed output and an unavailable model.
7. **Quality is measured first.** It is measured on recorded scenarios before any automation is enabled.

### Rule 5: what is configured where

- Deployment name, model ID and version, API version and TPM/RPM limits are config.
- The endpoint, deployment name, API version and daily token cap of the triage and escalation models are fields of `RemediationSettings` `spec.analysis.models` (5.4), with no key.
- TPM/RPM limits are set on the deployment itself.
- The model ID and version come from the reply and are checked against the evaluation list (11.2).

### Why the endpoint is guarded

The endpoint decides where the analyzer's Entra ID token and the redacted context go. So the analyzer will:

- refuse a host outside an operator-held allow-list of suffixes (a flag);
- refuse private, loopback and link-local addresses when dialing;
- request the Cognitive Services audience only.

The last two are built in the Foundry adapter ([#75](https://github.com/pkalin/shadewright/issues/75)). **Planned.** The allow-list is not built, and nothing reads the `spec.analysis.models` fields yet ([#76](https://github.com/pkalin/shadewright/issues/76)).

### Where inference runs

Global deployments can process data outside the resource's region. The resource region alone does not guarantee where inference happens ([deployment types](https://learn.microsoft.com/en-us/azure/foundry/foundry-models/concepts/deployment-types)).

### The lab region

The lab uses Sweden Central ([ADR-0016](../adr/0016-lab-region-sweden-central.md)). It was checked on 2026-11-01 on the lab subscription with `az cognitiveservices model list`.

- It is the only EU region listed with both OpenAI and Anthropic models.
- Anthropic models there are offered as Global Standard only.
- Azure OpenAI token prices are the same in every EU and US region. Global Standard is about 10% cheaper than Data Zone Standard (figures in 11.3).
- The lab node price in Sweden Central is the lowest of the EU regions compared. East US 2 was cheaper but outside the EU (figures in 11.3).

## 11.2. Model strategy

Three layers share the work.

- **Small model.** It summarizes context, classifies the symptom and proposes a known runbook. It makes one structured call on a prepared context, without tools.
- **Stronger model.** It takes hard incidents, ambiguous dependencies and multi-step analysis, with read tools and fixed step, token and spend budgets. It is an escalation, not the default.
- **Deterministic code.** It enforces policy, computes bounds, builds the diff and decides whether execution is allowed.

### When the stronger model is used

Deterministic signals trigger it, never model output:

- the context was truncated or a source could not be fetched;
- the small model's output failed schema validation after one retry.

At most one escalation per incident fingerprint per cooldown window. A runbook match (a rule with no model call) never reaches the model at all.

### The `ModelProvider` interface

`ModelProvider` is built as `Complete(ctx, CompletionRequest) (Completion, error)`.

- In: messages, tool definitions, an optional response schema and `MaxOutputTokens`.
- Out: `Content` (a string), tool calls, `Usage`, `UsageUnknown` (a billed reply whose usage could not be read) and a `Model` field.
- Throttling and transient failures (408, 429, 5xx, timeouts) come back as `ErrRetryable`; an unknown deployment is `ErrNotFound`, final.
- A reply that breaks the request's contract is `ErrMalformedOutput`, returned with the reply's `Usage` and `Model`.

<span class="sw-status sw-built">built</span> The interface, a fake and the Foundry adapter for OpenAI-format models (`internal/providers/model/foundry`, [#75](https://github.com/pkalin/shadewright/issues/75)). The adapter:

- is bound to one endpoint, deployment and API version, the fields of `spec.analysis.models` (rule 5); there is no fallback to another deployment;
- authenticates with workload identity through Entra ID, with the standard library, and sends a token only when its audience is Cognitive Services;
- gives every call a deadline and each attempt its own, and retries network failures, timeouts, 408, 429 and 5xx a bounded number of times with backoff, honouring `retry-after-ms` and `Retry-After`;
- returns an unknown deployment (404) as `ErrNotFound`, and a refused identity (401, 403, or a token for another audience) as `ErrForbidden`;
- accepts only a DNS host name in the endpoint, and connects only to public unicast addresses, so an endpoint that resolves to a private, loopback, link-local, shared or reserved address, or to an IPv6 address embedding one (NAT64, 6to4) or one whose embedded IPv4 cannot be read (Teredo), gets no token;
- reads at most 1 MiB of a reply and checks it as untrusted input. A reply cut off, filtered or refused, content that is not a JSON object when a schema was sent, or a call of a tool the request did not offer is `ErrMalformedOutput`, returned with the reply's usage because it was billed;
- reports the token usage of every call (input, cached input and output) with the reply and to an `OnCall` hook, and marks a call whose billed reply had no readable usage (too large, cut short, not a chat completion) as `UsageUnknown`, so the budget counts it conservatively;
- puts no URL, token, prompt or reply text in its errors and logs nothing.

`go test -tags foundrysmoke` runs it against a real deployment by hand (see the package documentation).

**Planned.** Wiring the adapter into the analysis loop with the host allow-list above (#76). A deployment field on the request and a typed timeout error. A Foundry adapter for the Anthropic family.

### Evaluation gate

A change to a prompt, model or schema must pass the scenario evaluation before merge.

- **In CI** it runs against a fake model. It covers plumbing, schemas and refusal paths only. It does not test the prompt's defences or model quality.
- **Quality** is gated by a recorded real-model evaluation, keyed by model ID and version plus the prompt hash, stored in this repository (for example `test/eval/results/`).
- **Recorded contexts** from the fleet scenarios live here too (for example `test/eval/contexts/`). The fleet's `scenarios/*/expected.yaml` are the expectations, and each result pins the fleet commit it read them from.

**It fails closed.** The analyzer embeds the list of evaluated (model, prompt hash) pairs. It refuses automatic analysis, with the reason reported, when the configured deployment's model (or `Completion.Model`) has no passing record. Details in [ADR-0023](../adr/0023-how-the-analyzer-calls-the-model.md).

### Choosing concrete models

Concrete models are chosen by benchmark on the scenario set. Measure diagnosis accuracy, plan usefulness, false fixes, latency and cost per resolved incident.

## 11.3. Details

| Topic | Detail |
| --- | --- |
| Lab region check | 2026-11-01, `az cognitiveservices model list`, lab subscription |
| GPT-5.4 mini input price | 0.6733 EUR per 1 M tokens Global, 0.7406 EUR Data Zone (Azure Retail Prices API, 2026-11-01) |
| Node `Standard_B2as_v2`, Sweden Central | 0.0698 EUR/h, lowest of the EU regions compared |
| Node, North Europe | 0.0733 EUR/h |
| Node, East US 2 | 0.0675 EUR/h, cheaper but outside the EU (same date) |
| Escalation limit | At most one per incident fingerprint per cooldown window |
| Retry before escalation | One retry after a schema validation failure |
