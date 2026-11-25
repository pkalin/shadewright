# ADR-0050: Vertex AI as a model provider

- Status: Proposed
- Date: 2026-11-25

## Context

The AKS lab needs to compare Gemini with its Foundry models on the same recorded incidents. The analyzer already chooses a provider for each role. The record store and GitHub App signer have their own adapters. This decision covers Vertex inference from clusters outside Google Cloud, including the AKS lab, and from GKE. [Adapter boundaries](0013-adapters-for-every-external-system.md)

## Decision

### 1. Add an opt-in Vertex provider

Add `VertexAI` to the `ModelProvider` enum and to the analyzer factory. The adapter implements `providers.ModelProvider.Complete` with Vertex `generateContent`. In `ModelDeployment`, require the existing `endpoint` and `deployment` fields. Extend the CEL exemption from the Foundry `apiVersion` requirement and forbid both `apiVersion` and `inferenceAPI` for Vertex. The regional base includes the API version, project and location. There are no separate settings fields for them. Omitting the provider still selects Foundry.

Routing Gemini through OpenAICompatible would lose the native function parts and introduce a different credential path. A separate adapter costs a token exchange, a message translator and tests for both.

### 2. Give the analyzer direct federated access

For an install that enables Vertex, select exactly one identity source at install time. The chart and analyzer startup reject configuration for the other source. If the selected source fails, the call fails. Both sources grant prediction directly to the analyzer's exact Kubernetes ServiceAccount and forbid key files and Google service account impersonation.

#### Outside Google Cloud

Mount a separate, short-lived projected token read-only in the analyzer. Bind the workload identity pool provider to the cluster's OIDC issuer and the Google projection's exact audience. Map `google.subject=assertion.sub` and set the provider's attribute condition to `assertion.sub == 'system:serviceaccount:<namespace>:<analyzer-serviceaccount>'`. Use the actual namespace and account at installation. Check the subject fits Google's 127-byte limit. The existing token projected for Azure Entra ID has audience `api://AzureADTokenExchange`. Both Foundry inference and Azure Blob can use that projection.

Exchange the Google projection only at `POST https://sts.googleapis.com/v1/token`, with the install-time STS audience `//iam.googleapis.com/projects/{number}/locations/global/workloadIdentityPools/{pool}/providers/{provider}`. In the dedicated inference project, grant only `aiplatform.endpoints.predict` to the exact federated subject (`principal://iam.googleapis.com/projects/{number}/locations/global/workloadIdentityPools/{pool}/subject/{subject}`). The provider condition rejects another subject at exchange time. The IAM binding limits what the accepted subject can do. Use a pool whose issuer trust is exclusive to that cluster so another provider cannot mint the same principal.

Direct access from outside Google Cloud costs a projection and a trust binding per cluster. Service account impersonation would add an identity, a token-creation grant and a call to `iamcredentials.googleapis.com`. A key file would leave a reusable credential in the pod. A namespace-wide binding or the predefined Vertex AI User role would grant more access than one analyzer needs.

#### On GKE

Use Workload Identity Federation for GKE. In the same dedicated inference project, grant only `aiplatform.endpoints.predict` to the principal for the analyzer's exact Kubernetes ServiceAccount, selected by UID in the GKE workload identity pool. The GKE metadata server exchanges the workload's token and returns a short-lived federated access token to the analyzer.

The metadata endpoint is link-local. The analyzer's chart NetworkPolicy in `dist/chart/templates/network-policy/allow-analyzer-egress.yaml` has no metadata egress rule today. The provider HTTP clients' public-address dial guard (`openaiwire.PublicControl`, which delegates to `Guard{}.Control`) refuses link-local addresses. Use a dedicated metadata client that makes only `GET http://169.254.169.254/computeMetadata/v1/instance/service-accounts/default/token` with `Metadata-Flavor: Google`, a deadline, no proxy and no redirects. Keep the public-address guard on the Vertex client.

Add a chart value, off by default, that allows analyzer egress only to the metadata server address and TCP port required by the installed GKE dataplane. GKE Dataplane V2 uses `169.254.169.254/32:80` for the policy rule. The other documented token egress rule uses `169.254.169.252/32:988`. Render only the rule for the selected dataplane. The network rule limits the destination and port. The metadata client limits the HTTP path. [Workload Identity Federation for GKE](https://docs.cloud.google.com/kubernetes-engine/docs/concepts/workload-identity)

### Change to invariant 6

The proposed addition follows the blob container grant in the analyzer clause of invariant 6 in `AGENTS.md`.

> If ADR-0050 is accepted, an analyzer configured with `provider: VertexAI` may obtain a federated access token for its exact ServiceAccount from one install-time source. Outside Google Cloud it may exchange its separately projected token only at Google STS. On GKE it may fetch the token only from the GKE metadata server's token endpoint. Neither source uses a key file or impersonation. Its exact principal may use only `aiplatform.endpoints.predict` in the dedicated project, at the declared EU regional endpoint and with the model pinned for the role.

### 3. Pin the regional destination and model before acquiring a token

Both identity sources use the same rules for project, location and model pins. The install declares one project, one EU region and one exact Gemini 2.5 model ID per enabled Vertex role. Validate the declaration at chart render and analyzer startup. On each settings selection the factory requires the exact base below and `deployment == ExpectedModels[role]`, with a nonempty pin, before acquiring a Google token or sending any context. Construct the request from validated installation values.

```text
endpoint = https://{location}-aiplatform.googleapis.com/v1/projects/{project}/locations/{location}
POST {endpoint}/publishers/google/models/{model}:generateContent
```

Keep STS and Vertex on separate clients. Both disable proxies (`Proxy: nil`), require TLS 1.2 or later, refuse every redirect and use the public-address dial guard (`PublicControl`, with the install's additional in-cluster ranges enforced) on every resolved address. Bound each call with a deadline. Reserve `googleapis.com` and its subdomains in the compatible adapter's `ReservedHost` and the factory's `separateProviderHost` checks, even if a Foundry suffix would otherwise allow the host. Anthropic already has a fixed host. The projected token goes only to STS. Vertex receives only the federated access token from the selected source. No token appears in logs, Events, records, status, traces or errors.

A shared list would let a settings editor spend the escalation model's quota on triage before the reply check could refuse it. A hostname check alone would leave the project path open to substitution. Changing a pin, project or region requires an install change. The global endpoint cannot promise the chosen EU region, and the EU multi-region endpoint (`aiplatform.eu.rep.googleapis.com`) lets Google choose among regions. Reject both, including as fallbacks. A regional outage stops the call. Gemini 3 is unavailable to this installation until Google offers it in a qualifying region.

### 4. Disable project caching and account for retained context

Send only the bounded incident context and read-tool results that have already passed through the shared redactor. Set the inference project's `CacheConfig.disableCache` to `true` before use. Leave request and response logging, explicit caching and grounding off. The project administrator sets these controls outside the analyzer. The analyzer has only prediction access.

Google does not train models on customer data without permission. As in ADR-0037, Shadewright makes inference requests only and never opts in or submits feedback. If training becomes the default, stop using Vertex until this decision is revisited. Abuse monitoring may still retain flagged prompts and responses in-region for up to 90 days unless an exemption applies. That includes any sensitive text missed by `internal/redact`. Leaving the default cache on would retain another copy. Turning it off gives up cache savings, and disabling request logging gives up provider-side prompt inspection. [Training rule](0037-the-anthropic-api-as-a-second-model-provider.md) and [redaction limits](../data-handling.md)

### 5. Translate tool turns and validate the answer locally

Map the system prompt to `systemInstruction`. Combine adjacent user-side messages into one Vertex user turn, preserving part order. This covers consecutive user messages on a triage retry and tool results followed by the final escalation instruction. Translate only the loop's offered read tools to `functionDeclarations`. On parsing a model turn, refuse control characters in call IDs and names before making a replay block. The loop's `boundCall` still checks for controls in arguments, offered tools and scope before a read.

Match each tool result's `ToolCallID` to the preceding assistant message's `ToolCalls` for its name and position. Assign a distinct local ID by position when the wire supplies none. Return a `functionResponse` with that name and a JSON object such as `{"result":"<redacted tool text>"}`. Keep synthetic IDs off the wire. Refuse missing, duplicate or unmatched correlations, even when two calls share a name.

Keep ordered model parts and their original signatures in opaque `ProviderBlocks` for that model and conversation only. Before replay, verify the part types and the number, order, identities, names and arguments of the function calls against `Message.ToolCalls`. Refuse a mismatch before sending. Replay matching parts unchanged. The loop copies the blocks without inspecting, logging or storing their contents.

For tool-free calls, request `application/json` with the supported subset of the generated response schema. On Gemini 2.5, every request offering tools omits both `responseMimeType` and the wire response schema, including the last escalation turn. The system prompt carries the generated schema. Every final answer goes through `decodeReply` and the shared Go checks, including string length bounds omitted by `schemaOf`. Pass the returned `modelVersion` through as `Completion.Model` for the exact role check. An extra formatting turn would spend another call. Invalid final escalation output ends in hand-off.

### 6. Bound and charge each inference attempt

Before sending, set `generationConfig.maxOutputTokens` to the role's output spend `O = OutputSpend(MaxOutputTokens)`. The Vertex adapter must enforce one billed output bound for candidate and thinking tokens together and expose it through `providers.OutputBounder`. A model that cannot honor that bound is ineligible.

Use one token per byte of input for the Vertex estimate `I`, including instructions, messages, tool definitions, response schema and replayed parts. Budget for the encoded input, including any expansion in the Vertex translation. Punctuation-heavy logs can exceed the existing estimate of one token per two bytes. Use the same estimate in the adapter and the loop. Apply `CompatibleUsageCeiling(I, O)` to obtain `C = I + O + ceil((I + O) / 10)`.

| Token count | Vertex usage metadata |
| --- | --- |
| Input | `promptTokenCount + toolUsePromptTokenCount` |
| Cached input, already included in input | `cachedContentTokenCount` |
| Output | `candidatesTokenCount + thoughtsTokenCount` |

For each possibly billed attempt, validate counts and their sum without overflow. Use `totalTokenCount` as a consistency check. Charge `max(I, R)` for usable reported input plus output `R <= C`, even on a zero report or malformed answer. Refuse an over-ceiling report as `ErrMalformedOutput`, discard its usage and charge `C`. Missing or unusable usage also invalidates the reply and costs `C`. A failed sent attempt with unknown usage costs `C`, including a timeout or a 429. An attempt that provably sent no inference request costs zero.

Extend the adapter's `AttemptsReported` convention and the loop's ceiling, refusal and floor checks to Vertex. Report a `BilledAttempt` for each attempt that might have reached the service, including failures before a successful retry. Before each call require room for `C` in both the analysis and the deployment's daily budget. Cap adapter attempts at the lesser of its `MaxAttempts` and the loop's `MaxBilledAttempts` (the smaller remaining budget divided by `C`, rounded down). Charging once per logical call would miss billed retries. The floor can exhaust the budget early, and a service that underreports output can still undercount the bill. This extends decision 9 of ADR-0023 using the per-attempt rule of ADR-0042. [Analysis spend](0023-how-the-analyzer-calls-the-model.md) and [compatible provider accounting](0042-openai-compatible-model-endpoints.md)

### 7. Gate automatic analysis on a passing evaluation

Qualify each role against its exact ADR-0025 manifest digest. Identify the provider, regional deployment, model ID and version alongside the prompt, tools, schema, catalog, context builder, redaction settings, scenario rubric, thresholds and repeats. A changed input invalidates the pass. Refuse automatic analysis without a matching pass. The runtime manifest gate is planned. Until it exists, Vertex is for on-demand lab evaluation only. A successful transport test cannot establish diagnosis quality. Qualifying a new pin costs a real-model evaluation, including escalation with tools. [Evaluation manifest](0025-analysis-claims-units-and-evaluation-manifest.md)

Use the existing Normal Incident event `EventModelProviderChanged` on the first call for a role that returns after sending since startup or a switch to that provider. Record the provider for each eligible role in `RecordProviders` (`providers.triage`, `providers.escalation`). Azure Blob can still receive the analysis record through `AnalysisStore.Put`, and the executor can still sign through Azure Key Vault.

Triage has no tools. Escalation gets at most `maxToolSteps + 1` turns. The analysis gets at most `3 + maxToolSteps` model calls including loop retries. Adapter retries within those calls cost tokens under decision 6. A typed proposal still requires deterministic policy and approval of the plan digest before the executor can open a GitOps pull request. A new action tool or an extra turn would change the evaluation and the remediation boundary.

## Alternatives considered

| Alternative | Reason for rejection |
| --- | --- |
| Global or EU multi-region inference | Neither pins processing to the single region selected for the lab. |
| Impersonate a Google service account or mount its key | Direct federation gives the exact Kubernetes subject prediction access without an intermediate identity or a long-lived secret. |
| Let settings choose either role's model from a shared list | The post-call model check happens after the context has left the cluster. |
| Rely on wire JSON enforcement | Tool turns on Gemini 2.5 cannot request JSON. |
| Charge only the final reply | This misses earlier attempts the service may have billed. |

## Consequences

The lab can compare providers on the same recorded scenarios. A compromised analyzer could spend the prediction grant elsewhere within the dedicated project. The role pin constrains the analyzer's normal requests. Project access reviews and spend controls must account for the grant itself.

## Google documentation checked on 2026-11-25

- The regional publisher route in decision 3 serves `gemini-2.5-pro`, `gemini-2.5-flash` and `gemini-2.5-flash-lite` in `europe-west1`, `europe-west4`, `europe-west8`, `europe-west9`, `europe-north1`, `europe-central2` and `europe-southwest1`. `europe-west3` omits Flash-Lite. London and Zurich are outside the EU. Gemini 3 previews are global-only. [generateContent](https://docs.cloud.google.com/vertex-ai/generative-ai/docs/reference/rest/v1/projects.locations.publishers.models/generateContent) and [model locations](https://docs.cloud.google.com/vertex-ai/generative-ai/docs/learn/locations)
- External Kubernetes federation supports the STS exchange, exact subject mapping and direct principal grant in decision 2. Prediction requires `aiplatform.endpoints.predict`. [Kubernetes federation](https://docs.cloud.google.com/iam/docs/workload-identity-federation-with-kubernetes), [supported products](https://docs.cloud.google.com/iam/docs/federated-identity-supported-products) and [Vertex permissions](https://docs.cloud.google.com/vertex-ai/docs/general/access-control)
- Request and response logging, grounding and explicit caching default off. Implicit caching and the 24-hour in-memory cache default on. The project control is `PATCH .../projects/{project}/cacheConfig` with `disableCache: true`. The training terms and the separate abuse-monitoring retention in decision 4 apply even when request logging is off. [Context caching](https://docs.cloud.google.com/vertex-ai/generative-ai/docs/context-cache/context-cache-overview) and [data governance](https://docs.cloud.google.com/vertex-ai/generative-ai/docs/data-governance)
- Gemini 2.5 rejects function declarations together with a JSON response type (HTTP 400). Thought signatures are optional on 2.5. Gemini 3 requires them on function-call turns and signs only the first call of a parallel group. The usage fields in decision 6 distinguish cached input, tool-use input and thinking tokens. [Structured output](https://docs.cloud.google.com/vertex-ai/generative-ai/docs/multimodal/control-generated-output), [thought signatures](https://docs.cloud.google.com/vertex-ai/generative-ai/docs/thought-signatures) and [usage metadata](https://docs.cloud.google.com/vertex-ai/generative-ai/docs/reference/rest/v1/projects.locations.publishers.models/generateContent#usagemetadata)

## What the first slice does not build

The first slice builds the STS path for clusters outside Google Cloud. The GKE identity path, including its metadata client and chart egress value, is built with the Google Cloud reference environment epic (#1160) because there is no GKE cluster before that epic. The first slice also leaves out the reference environment, GCS analysis records and Cloud KMS signing.

## Open checks before the first run

Use synthetic input for the wire checks. Complete the project checks before sending incident data.

- Confirm access to each exact pin in the chosen region, the returned model version, and the applicable processing and storage terms. Read back the project cache setting and confirm logging, grounding, explicit caching and training opt-in are off.
- Verify whether the billing account qualifies for an abuse-monitoring exemption under invoiced billing and whether Google has granted it for this project. Record the applicable retention and access rules for flagged content.
- Test STS with the analyzer's issuer, audience and subject. Check rejection of another ServiceAccount and of the Azure projection. Verify prediction with the single permission. Test role swaps, path changes, reserved hosts, private dial addresses and redirects before either client releases credentials or context.
- Exercise tool-free output, tool turns, repeated tool names, signature replay and mismatched parts. Verify that an invalid final answer ends escalation within its turn limit.
- Check the combined output cap against actual candidate and thinking usage. Reconcile tool-use and cached input with totals. Exercise punctuation-heavy input, zero and over-ceiling reports, a timeout followed by success and a retry with insufficient budget.

## Amendments

None.
