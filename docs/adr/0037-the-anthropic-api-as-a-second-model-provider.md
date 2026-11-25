# ADR-0037: The Anthropic API as a second model provider and its API key

- Status: Accepted
- Date: 2026-11-14

## Context

Microsoft Foundry is the only model provider the analyzer can use today ([ADR-0013](0013-adapters-for-every-external-system.md), [ADR-0023](0023-how-the-analyzer-calls-the-model.md)). Claude models on Foundry are outside the lab's credits, so the model comparison of [#503](https://github.com/shadewright-labs/shadewright/issues/503) has only OpenAI-family results. On 2026-11-14 the maintainer decided that the Anthropic API is the first direct vendor path and goes into the lab ([#387](https://github.com/shadewright-labs/shadewright/issues/387)). [#728](https://github.com/shadewright-labs/shadewright/issues/728) builds the adapter and runs it from the evaluation harness; [#729](https://github.com/shadewright-labs/shadewright/issues/729) wires it into the analyzer.

Two things change, and each touches an invariant:

- **A static credential.** The identities that authorize calls to external services are federated today: the analyzer reaches Foundry with a workload identity token, the executor gets short-lived Git App tokens. The Anthropic API takes a static API key. It is the first static credential that authorizes a call to an external service and does not expire by itself.
- **Redacted context goes to a second place.** With Foundry the context stays inside the Azure resource the operator chose, in the region of [ADR-0016](0016-lab-region-sweden-central.md). With the Anthropic API it is sent to the vendor's own service. Invariant 4 still decides what may be sent (everything is redacted first); it says nothing about where it goes.

The decisions below were taken on 2026-11-14. This ADR writes them down and states what follows from them.

## Decision

[![Where the context goes and where the key lives for each provider: the analyzer builds the same redacted context and calls the provider of the deployment; Foundry is reached with a workload identity token and stays in Azure, the Anthropic API only at its fixed host with a key read from a mounted file (Secret or CSI volume) and leaves Azure; executor and verifier never see the key](../assets/diagrams/model-providers.svg)](../assets/diagrams/model-providers.svg "Open full size")

1. **The provider is chosen per `ModelDeployment`.** An optional field `provider` takes `Foundry` (the default, so existing objects keep their meaning) or `Anthropic`. It names the API the adapter speaks, not the model family: a Claude model served by Microsoft Foundry is still `provider: Foundry`. Triage and escalation are separate deployments and may use different providers. There is no fallback between providers at run time: if a call fails and the bounds of [ADR-0023](0023-how-the-analyzer-calls-the-model.md) are used up, the analysis is handed off.

2. **The host of an `Anthropic` deployment is fixed in code.**
   - The adapter talks only to `api.anthropic.com`. The operator chooses no host for this provider, and `--model-endpoint-suffixes` never applies to it: that list stays the allow-list of the Foundry path only.
   - The Foundry path refuses `api.anthropic.com` whatever the list says. So the Entra token can never go to the vendor, and the key never to a Foundry host.
   - `endpoint` stays a required field and must equal `https://api.anthropic.com` for `provider: Anthropic`; a CEL rule on the object makes the API server refuse anything else, and a second rule refuses that host on a `Foundry` deployment. Making `endpoint` optional for this provider would need one more rule (required unless `Anthropic`) and a defaulting story; requiring the constant keeps the schema to the existing field plus two rules. The existing pattern already accepts the host alone, with no path (`api/v1alpha1/remediationsettings_types.go`).
   - `deployment` holds the model ID exactly as replies name it. The expected model stays the `--triage-model` / `--escalation-model` flag, checked in the analysis loop (`internal/analysis/loop.go`), not in the adapter. For `Anthropic`, `deployment` must equal that flag's value, and the analyzer refuses to start the analysis otherwise. The evaluation list of [ADR-0023](0023-how-the-analyzer-calls-the-model.md) point 8 is keyed on that ID.

3. **The key is a static credential and is handled as one.** The handling below is meant as the general rule for any adapter whose service has no keyless authentication (see "Invariant 6 wording" under Consequences), with this key as its first use.
   - The analyzer's contract is a file at a path given by a flag. What puts it there is the operator's choice: a Kubernetes Secret, or a CSI volume such as the Secrets Store CSI driver with Azure Key Vault. Shadewright's chart never creates a Secret and never contains the key.
   - The chart takes either the name of an existing Secret or a volume source for that path. The volume is mounted whole, read-only, into the analyzer pod only, with no `subPath` (a `subPath` mount never refreshes). The executor and the verifier never get it.
   - The analyzer gets no RBAC on Secrets in either case. The kubelet or the CSI driver mounts the file; the analyzer cannot list or read any other Secret because of this change.
   - The analyzer is the gate, not the chart: `RemediationSettings` is created only when absent and edited live afterwards, so a chart check cannot see the current deployments. A deployment with `provider: Anthropic` and no key path flag is refused by the analyzer with a named reason. The render check in the chart stays as a convenience at install.
   - The adapter reads the file on every call, so rotation is an update of the source and needs no restart. A missing, empty or unreadable file is retryable within the bounds of [ADR-0023](0023-how-the-analyzer-calls-the-model.md), then a hand-off; never a crash and never a refusal to start. The reason does not name the key.
   - The key never appears in logs, events, status, analysis records, traces or error text. The adapter sends it only in the request header, only to `api.anthropic.com`.

4. **Invariants 1 and 4 hold unchanged.** The context sent is the same redacted context as for Foundry, built by the same code. The reply is untrusted: it is parsed by the same typed code into the same result, and the proposal still goes through the one validator and the policy gate. Model output is never authorization.

5. **A model must pass the evaluation first.** As for any model ([ADR-0025](0025-analysis-claims-units-and-evaluation-manifest.md)), the intent is that the analyzer refuses automatic analysis with a model that has no passing record for the current prompt hash. That run-time check is planned, not built (`docs/design/11-model.md`, "It fails closed"). Until it exists, an `Anthropic` deployment is for lab evaluation only, exactly as a Foundry one is today. The evaluation manifest and every analysis record name the provider as well as the model, so a result cannot be read as belonging to the wrong one.

6. **Data handling is stated, not hidden.** With this provider the redacted context leaves Azure. The region choice of [ADR-0016](0016-lab-region-sweden-central.md) does not cover it. Where data goes is decided per analyzer: it can use this provider only if a key file is present. After that, anyone who may edit `RemediationSettings` can move triage or escalation to this provider, so that permission is also a decision about where context goes. The data handling page says so in plain words next to the Foundry description, and says which vendor terms the operator has to check; it does not restate the vendor's retention or region terms as facts of this project (see Vendor terms below).

7. **The lab.** The key is put in a Secret (or the Key Vault behind a CSI volume) at lab-up from the admin machine, from a key kept outside every repository. It never goes through Git, and never through Terraform state or variables. Lab-down deletes a Secret with the namespace; the key itself is revoked or rotated in the vendor's console when the lab session ends if the workspace spend limit below is not enough on its own.

## Consequences

**A leaked key.** What it allows is inference on the vendor workspace the key belongs to, up to that workspace's spend limit. It gives no access to the cluster, the Git host or any Shadewright state. The cost of a leak is therefore bounded by the limit, not by anything Shadewright enforces. `maxTokensPerDay` of [ADR-0023](0023-how-the-analyzer-calls-the-model.md) limits the analyzer's own use and does not limit someone who holds the key.

**What an operator must set.** A dedicated workspace for Shadewright at the vendor, with a spend limit sized to the daily token limit, and a key of that workspace. The limit is per workspace, not per key, and not available on the default workspace. The install notes say this is required, and the chart cannot check it. Use a key made for this cluster, never one shared with other work.

**Rotation.** Create a new key, update the Secret, wait for the kubelet to refresh the mounted file (it can take about a minute), revoke the old key. No restart. A call that races the refresh uses the old key until it is revoked; revoke after the first successful call with the new one. A file that is briefly unreadable during the refresh is a retryable error, not a hand-off with a final reason.

**Who can read the key.** Anyone who can `exec` into the analyzer pod, read the Secret in the API, or read the node's kubelet volume. This is the same boundary as for any mounted Secret. The operator limits it with RBAC on Secrets in the operator namespace; the chart does not grant it to anyone.

**NetworkPolicy does not help.** The analyzer's egress rule already allows TCP 443 to any address, and a standard NetworkPolicy cannot tell hosts apart ([design 10](../design/10-security.md), accepted in [#391](https://github.com/shadewright-labs/shadewright/issues/391)). Adding the Anthropic API changes no rule. What keeps the key to one host and the Entra token away from it is the host fixed in code, the refusal on the Foundry path and the CEL rules. A compromised analyzer could send what it holds, now including this key, to any HTTPS host. A policy that names hosts needs a CNI with that feature and is not part of this ADR.

**Invariant 6 wording.** `AGENTS.md` lists what each identity may do and has no rule for a static credential. Not every external service offers federated or keyless authentication, and this will not be the last one: a second vendor API, a signed webhook of the Notifier ([#97](https://github.com/shadewright-labs/shadewright/issues/97)). So the rule is written once, as a rule, and each use of it is named. [#734](https://github.com/shadewright-labs/shadewright/pull/734) added this to invariant 6, after the three identities:

> **Static credentials.** Federated or keyless authentication is used wherever the external service offers it. Where it does not, a component may hold a static credential under these conditions, all of them: an ADR names the adapter and the credential; only the one component whose identity makes that call gets it; it reaches the component as a read-only mounted file that the cluster operator provides (a Secret or a CSI volume), never through RBAC on Secrets, an environment variable or a field of a custom resource; it is read at each use, so rotation needs no restart; and it never appears in logs, events, status, records, traces or errors. In use today: the analyzer's Anthropic API key (ADR-0037).

A later adapter that needs a key adds its name to the last sentence in its own ADR; it does not need a new exception. The conditions are the invariant, the list is its register.

This ADR does not edit `AGENTS.md`.

**Vendor terms, checked 2026-11-14.** From the vendor's published pages. The terms can change, so the operator checks them again.
- *Retention.* Inputs and outputs are deleted from the backend within 30 days by default. Zero data retention is an arrangement per organization, requested through sales, and some models are not available under it ([API and data retention](https://platform.claude.com/docs/en/manage-claude/api-and-data-retention), [how long data is stored](https://privacy.claude.com/en/articles/7996866-how-long-do-you-store-my-organization-s-data)).
- *Where inference runs.* The default is `global`, where inference may run in any available geography. The request parameter `inference_geo` accepts `global` and `us` (1.1 times the price). There is no EU option. So redacted context can be processed outside the EU, which the region choice of ADR-0016 does not cover. The adapter sends no `inference_geo`; a setting for it is not part of this ADR ([data residency](https://platform.claude.com/docs/en/manage-claude/data-residency)).
- *Training.* Opt-in at the vendor, not the default. The privacy centre (page dated August 18, 2026) says: "By default, we will not use your inputs or outputs from our commercial products (e.g. Claude for Work, Anthropic API, Claude Gov, etc.) to train our models." The exception on the same page: "If you explicitly report feedback or bugs to us (e.g. via our thumbs up/down feedback button), or otherwise choose to allow us to use your data, then we may use your chats and coding sessions to train our models." ([is my data used for model training](https://privacy.claude.com/en/articles/7996868-is-my-data-used-for-model-training)). The commercial terms (effective Jun 17, 2025), section B, say: "Anthropic may not train models on Customer Content from Services." ([commercial terms](https://www.anthropic.com/legal/commercial-terms)).
- *Spend limit.* Set per workspace in the Console, not per key, and not on the default workspace ([rate limits](https://platform.claude.com/docs/en/api/rate-limits)).
- *Key scoping.* A key belongs to a workspace and can have an expiry and an IP allow-list. No restriction to models or scopes was found ([authentication](https://platform.claude.com/docs/en/manage-claude/authentication)).
- *Model IDs.* Every model ID is a pinned snapshot, including dateless ones such as `claude-haiku-5-5`, so there is no moving alias ([models overview](https://platform.claude.com/docs/en/about-claude/models/overview)).
- *Prompt caching.* The minimum cacheable prefix is 512 tokens for Claude Haiku 5.5, Sonnet 5.5 and Opus 5.5. A shorter prefix is not cached and returns no error ([prompt caching](https://platform.claude.com/docs/en/build-with-claude/prompt-caching)).

**Training rule (decided 2026-11-14).** Use of the redacted context for model training must be opt-in at the vendor, never the default. That holds for the Anthropic API as checked above, so the provider stays. If a vendor's terms change to training by default, the provider is not used until this ADR is revisited. The adapter sends inference requests only and never submits feedback, so nothing Shadewright does opts the operator in. As built, the adapter makes one kind of request: a POST to the messages endpoint (`messagesURL`, in `internal/providers/model/anthropic`), with no other endpoint and no feedback call. An operator who opts in through their own vendor account does so outside Shadewright.

**Other costs.**
- The key is the analyzer's second possible Secret mount, next to the TLS Secret for metrics.
- Azure is still required: the workload identity token source, the `clientId` chart check and the analysis records in Blob stay. Making the token source conditional on a Foundry deployment is not part of this ADR. *Amended 2026-11-16, see [Amendments](#amendments).*
- The chart gets a value for an existing Secret name or a volume source, and a render check as a convenience; the kustomize manifests in `config/` get no mount, so this provider is Helm only until someone asks.
- The adapter and the analyzer need a test that no error path holds the key.
- There are now two sets of evaluation results per model family, and a model comparison must say which provider produced a row.
- A leak through a heap dump or a core file of the analyzer is possible, as for any process that holds a key in memory for the length of a call.

## Alternatives considered

- **Claude on Microsoft Foundry only.** Keeps one provider and no static key, and the data stays in Azure. Rejected for the lab for now: the models are outside its credits ([#503](https://github.com/shadewright-labs/shadewright/issues/503)). It remains the choice for an operator who needs the data to stay in Azure.
- **A provider setting for the whole `RemediationSettings` instead of per deployment.** Simpler, but it forbids a cheap model from one provider for triage and another provider for escalation, and it makes the data decision coarser than the deployment that carries it.
- **Fallback to the other provider on failure.** It would send context to a provider the operator did not choose for that call, and a bug in one adapter could be hidden by the other. Rejected, as in [#387](https://github.com/shadewright-labs/shadewright/issues/387).
- **The key read by the operator with `get` on a Secret.** Rotation would be a watch or a read per call, and the analyzer would hold RBAC on Secrets, a grant that is one `resourceNames` mistake away from reading the Git App's. A mounted file needs no grant.
- **The key in an environment variable.** Needs a restart to rotate, and shows up in process listings of some debug tools. A file is read on every call instead.
- **A gateway in Azure that holds the key and takes the analyzer's workload identity.** The analyzer would carry no static credential. Rejected for now: it is a new Azure component with its own cost and it ties the provider to Azure. It can be added later without changing the adapter contract.
- **The key from a cloud secret store with a federated identity, read by the analyzer.** Would need cloud-specific code in the adapter. A CSI driver gives the same result behind the file interface and is allowed by decision 3.
- **Listing `api.anthropic.com` in `--model-endpoint-suffixes`.** It would make the vendor host operator-chosen and let the Entra token or the key meet the wrong host. Rejected for the fixed host of decision 2.
- **A vendor SDK.** Plain `net/http`, as the Foundry adapter, so the dependency list does not grow and the host check stays in one place ([#728](https://github.com/shadewright-labs/shadewright/issues/728)).

## Open questions

None. The question of training on API data is answered under Vendor terms and the training rule in Consequences.

## Amendments

- 2026-11-16 ([#814](https://github.com/shadewright-labs/shadewright/issues/814)): the token source is conditional on a Foundry deployment. Without `--model-endpoint-suffixes` the analyzer reads no workload identity, builds no token source and refuses every `Foundry` deployment (`ProviderNotConfigured`). The chart does not read `settings.spec` (the settings object is created once and an upgrade never changes it, so the values cannot tell what the cluster holds): `analyzer.identity.azure.clientId` is required exactly when `endpointSuffixes` or a record store is set, and with a triage model at least one of `endpointSuffixes` and the Anthropic API key is required. The analyzer is the gate for the deployments. `apiVersion` may be left out only for provider `Anthropic`. The records in Blob still need the identity.

- 2026-11-17: [ADR-0042](0042-openai-compatible-model-endpoints.md) (Accepted) adds OpenAICompatible as a third provider with install-time `--openai-compatible-endpoints` declarations with nonempty allowed model lists and optional endpoint-bound keys. The Foundry list (`--model-endpoint-suffixes`, chart `endpointSuffixes`) stays Foundry-only. `--disable-foundry` disables Foundry and its token source; compatible-only inference needs no Azure identity. Blob records still require Azure identity. Declarations overlapping the Foundry list fail startup; Foundry refuses declared compatible hosts. Two chart rules of the 2026-11-16 amendment change with it: with a triage model, declared `openAICompatible.endpoints` also satisfy the provider requirement, next to `endpointSuffixes` and the Anthropic API key; and `endpointSuffixes` requires `analyzer.identity.azure.clientId` only when `disableFoundry` is false (a record store still requires it).
- 2026-11-25: [ADR-0050](0050-vertex-ai-as-a-model-provider.md) (Proposed) would extend decision 1 with a fourth provider. Its analyzer exchanges a dedicated projected token for federated prediction access. The existing per-role provider event and analysis record would identify the destination as VertexAI. The same opt-in training rule applies because redacted incident context would leave Azure.
