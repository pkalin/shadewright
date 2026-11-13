# 11. Model

This section covers how the analyzer is set up against a managed model endpoint (11.1) and which model does which job (11.2). Prices, dates and other numbers that matter only when you build or compare are in the table "Details" at the end (11.3).

The model is one part of the analyzer, not the part that makes it safe. Code and rules around it, a harness, decide what it is told, what it sees, what it can touch and how its work is checked, and nothing it answers is authorization: its only output is a proposal that policy code and a person check outside it, and that a verifier samples after the deploy (built, #682, but it writes reports only when the executor runs with `--verify-outcomes`, off by default; the executor reading them is planned, #683). [The analyzer as a harness](harness.md) maps each part to the section that describes it.

[![The analyzer as a harness around the model: guides, context, tools and checks around a model that proposes only, and the path outside it from the proposal through the policy check, the GitOps pull request, a person merging and Flux deploying to the verifier, which is built and writes reports only with the executor flag --verify-outcomes (off by default), while the executor reading them is planned.](../assets/diagrams/harness.svg)](../assets/diagrams/harness.svg)

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
- The model ID and version come from the reply. The operator names the one each role must answer with (`--triage-model`, `--escalation-model`), and a reply naming another is refused (`UnexpectedModel`); the check against the evaluation list is planned (11.2).

### Why the endpoint is guarded

The endpoint decides where the analyzer's Entra ID token and the redacted context go. So the analyzer:

- refuses a host outside an operator-held allow-list of suffixes (`--model-endpoint-suffixes`, required with `--triage-model`, no default; an entry such as `.<resource>.services.ai.azure.com` allows that one host), and a port other than 443, before it builds a provider or requests a token (`EndpointNotAllowed`);
- refuses private, loopback and link-local addresses when dialing;
- requests the Cognitive Services audience only for the model (the analysis record store asks for its own Storage token, section 9.1).

<span class="sw-status sw-built">built</span> The allow-list in the analyzer (`internal/analyzer/models`, [#76](https://github.com/pkalin/shadewright/issues/76)), the last two in the Foundry adapter ([#75](https://github.com/pkalin/shadewright/issues/75)). The analysis loop reads `spec.analysis.models` when the analyzer runs with `--triage-model`.

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

A proposed action outside the catalog's bounds (it fails `policy.CheckAction`) is neither: it gets no retry and no feedback to the model, so the model cannot search for what the policy allows, and the analysis ends as a hand-off with its own reason, `ActionOutOfBounds` (decided 2026-11-05). Schema failures keep their one corrective retry. What the model does see, before it answers, is the bounds of the RemediationPolicy that will decide its proposal, in the context's policy bounds block (3.3, [#493](https://github.com/pkalin/shadewright/issues/493), decided 2026-11-07, proposed in [ADR-0026](../adr/0026-the-analyzer-shows-the-model-the-policy-bounds.md)): fixed operator configuration, the same for every answer, rendered from the policy's typed fields by the policy package's own checks. It is not a probe: the model learns nothing about its proposal, and nothing clamps a value to the bounds. The policy still decides (invariant 1). The bounds are limits, not a target: the prompt asks for the smallest change the evidence supports and for the summary to name the evidence that sets the size, and the pull request body shows the size of the step beside the policy.

At most one escalation per incident fingerprint per cooldown window. A runbook match (a rule with no model call) never reaches the model at all. <span class="sw-status sw-built">built</span> in the analysis loop (3.3), which the analyzer starts with `--triage-model` and runs with `--escalation-model` ([#76](https://github.com/pkalin/shadewright/issues/76)).

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
- reads at most 1 MiB of a reply and checks it as untrusted input. A reply cut off, filtered or refused, content that is not a JSON object when a schema was sent, or a call of a tool the request did not offer is `ErrMalformedOutput`, returned with the reply's usage because it was billed. A response that arrives after the attempt's or the call's deadline ended, whatever its status (an empty 200, for example), is that timeout, retryable and not counted as billed (#438);
- reports the token usage of every call (input, cached input and output) with the reply and to an `OnCall` hook, and marks a call whose billed reply had no readable usage (too large, cut short, not a chat completion) as `UsageUnknown`, so the budget counts it conservatively;
- puts no URL, token, prompt or reply text in its errors and logs nothing.

`go test -tags foundrysmoke` runs it against a real deployment by hand (see the package documentation).

<span class="sw-status sw-built">built</span> The analyzer gives the analysis loop a Foundry provider per deployment of `spec.analysis.models`, after the host allow-list above, with its workload identity as the token source ([#76](https://github.com/pkalin/shadewright/issues/76)).

**Planned.** A deployment field on the request and a typed timeout error. A Foundry adapter for the Anthropic family.

### Evaluation gate

A change to a prompt, model or schema must pass the scenario evaluation before merge.

- **In CI** it runs against a fake model. It covers plumbing, schemas and refusal paths only. It does not test the prompt's defences or model quality. <span class="sw-status sw-built">built</span> `test/eval` replays each scenario's recorded replies through the real context builder, read tools and analysis loop in `make test`; the test job writes the report into its summary ([Evaluate the model](../evaluation.md), [#78](https://github.com/pkalin/shadewright/issues/78)).
- **Quality** is gated by a recorded real-model evaluation, keyed by the manifest digest of [ADR-0025](../adr/0025-analysis-claims-units-and-evaluation-manifest.md). <span class="sw-status sw-built">built</span> The manual Evaluation workflow runs the same harness against the Foundry deployments and reports the manifest, its digest, the scores, the tokens and an estimated cost. <span class="sw-status sw-planned">planned</span> Storing passing results in this repository (for example `test/eval/results/`).
- **Recorded contexts** from the fleet scenarios live here too: `test/eval/testdata/scenarios/`, with the fleet's `scenarios/*/expected.yaml` copied unchanged from the fleet commit that `source.yaml` names, a rubric per scenario that says how each expected fact is scored, and the scenario's RemediationPolicies (`policy.yaml`): the context shows their bounds, and a plan they refuse fails the scenario ([#493](https://github.com/pkalin/shadewright/issues/493)). <span class="sw-status sw-built">built</span> for all eight scenarios, written by hand from the scenario manifests, and for variants of a scenario that the lab met, such as `oom/rollout`, scored against the scenario's unchanged expectation ([Evaluate the model](../evaluation.md#the-scenario-files)); <span class="sw-status sw-planned">planned</span> replacing them with contexts and replies recorded from lab runs ([ADR-0015](../adr/0015-analysis-records-outlive-the-lab.md)).

**It fails closed.** <span class="sw-status sw-planned">planned</span> The analysis loop computes the prompt hash (`PromptVersion`) but does not check an evaluation list yet (the harness is [#78](https://github.com/pkalin/shadewright/issues/78), the list and the check are its follow-up); until it does, a run with a real model is an evaluation run in the lab, not production use, and that check is the gate before a release that starts the loop ([ADR-0025](../adr/0025-analysis-claims-units-and-evaluation-manifest.md) decision 7). <span class="sw-status sw-built">built</span> The `Completion.Model` check: a reply must name the model the operator configured for its role. <span class="sw-status sw-planned">planned</span> The analyzer embeds the list of evaluated (model, prompt hash) pairs and refuses automatic analysis, with the reason reported, when the configured deployment's model (or `Completion.Model`) has no passing record (a follow-up of [#78](https://github.com/pkalin/shadewright/issues/78)). Details in [ADR-0023](../adr/0023-how-the-analyzer-calls-the-model.md). <span class="sw-status sw-planned">planned</span> [ADR-0025](../adr/0025-analysis-claims-units-and-evaluation-manifest.md) keys the pass on a manifest of every behaviour-affecting input (model and version, prompt and tools hashes, context builder, redaction configuration, rubric, thresholds, repeats), so a change to any of them invalidates it.

### Choosing concrete models

Concrete models are chosen by benchmark on the scenario set. Measure diagnosis accuracy, plan usefulness, false fixes, latency and cost per resolved incident. The defaults from the first benchmark, and the definition of a false fix, are in [ADR-0027](../adr/0027-default-models-for-triage-and-escalation.md): triage `gpt-5.4-mini-2026-03-17` provisional, no escalation default.

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
