# 11. Model

## 11.1. Setup

The analyzer runs locally during development and in the cluster later, always against a managed endpoint.

1. A separate Foundry resource and budget for the lab.
2. A model available in the chosen region and deployment type, with structured outputs and tool calling.
3. Pay-per-token first. No provisioned throughput without measured steady load.
4. Entra ID authentication and the minimum data-plane role for inference.
5. Deployment name, model ID and version, API version and TPM/RPM limits in config, not in code. The endpoint, deployment name, API version and daily token cap of the triage and escalation models are fields of `RemediationSettings` `spec.analysis.models` (5.4), with no key. The endpoint decides where the analyzer's Entra ID token and the redacted context go, so the analyzer will refuse a host outside an operator-held allow-list of suffixes (a flag), refuse private, loopback and link-local addresses when dialing, and request the Cognitive Services audience only (planned, [#76](https://github.com/pkalin/shadewright/issues/76)); the model ID and version come from the reply and are checked against the evaluation list (11.2); TPM/RPM limits are set on the deployment itself. Planned: nothing reads these fields yet ([#76](https://github.com/pkalin/shadewright/issues/76)).
6. Timeouts, throttling, malformed output and an unavailable model are handled and tested.
7. Quality is measured on recorded scenarios before any automation is enabled.

Global deployments can process data outside the resource's region; the resource region alone does not guarantee where inference happens ([deployment types](https://learn.microsoft.com/en-us/azure/foundry/foundry-models/concepts/deployment-types)).

For the lab: Sweden Central ([ADR-0016](../adr/0016-lab-region-sweden-central.md)). Checked 2026-11-01 on the lab subscription with `az cognitiveservices model list`: it is the only EU region listed with both OpenAI and Anthropic models, and Anthropic models there are offered as Global Standard only. Azure OpenAI token prices are the same in every EU and US region; Global Standard is about 10% cheaper than Data Zone Standard (for example GPT-5.4 mini input: 0.6733 EUR per 1 M tokens Global, 0.7406 EUR Data Zone, Azure Retail Prices API, 2026-11-01). The lab node price in Sweden Central (0.0698 EUR/h for `Standard_B2as_v2`) is the lowest of the EU regions compared, with North Europe at 0.0733; East US 2 (0.0675) was cheaper but outside the EU (same date).

## 11.2. Model strategy

- Small model: summarize context, classify the symptom, propose a known runbook. One structured call on a prepared context, without tools.
- Stronger model: hard incidents, ambiguous dependencies, multi-step analysis, with read tools and fixed step, token and spend budgets. Used as an escalation, not by default. Deterministic signals trigger it, never model output: the context was truncated or a source could not be fetched, or the small model's output failed schema validation after one retry. At most one escalation per incident fingerprint per cooldown window. A runbook match (a rule with no model call) never reaches the model at all.
- Deterministic code: enforce policy, compute bounds, build the diff, decide whether execution is allowed.

`ModelProvider` is built as `Complete(ctx, CompletionRequest) (Completion, error)`: messages, tool definitions, an optional response schema and `MaxOutputTokens` in; `Content` (a string), tool calls, `Usage` and a `Model` field out; throttling and unavailability as `ErrRetryable`. Planned: a deployment field, typed malformed-output and timeout errors, and usage reported on failure (11.1). Planned too: Foundry adapters for the OpenAI and Anthropic families. Today only the interface and a fake exist.

Evaluation gate. A change to a prompt, model or schema must pass the scenario evaluation before merge. In CI it runs against a fake model and covers plumbing, schemas and refusal paths only; it does not test the prompt's defences or model quality. Quality is gated by a recorded real-model evaluation keyed by model ID and version plus the prompt hash, stored in this repository (for example `test/eval/results/`). Its recorded contexts from the fleet scenarios live here too (for example `test/eval/contexts/`); the fleet's `scenarios/*/expected.yaml` are the expectations, and each result pins the fleet commit it read them from. The analyzer embeds the list of evaluated (model, prompt hash) pairs and refuses automatic analysis, with the reason reported, when the configured deployment's model (or `Completion.Model`) has no passing record. It fails closed. Details in [ADR-0023](../adr/0023-how-the-analyzer-calls-the-model.md).

Concrete models are chosen by benchmark on the scenario set. Measure diagnosis accuracy, plan usefulness, false fixes, latency and cost per resolved incident.
