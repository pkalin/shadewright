# ADR-0037: The Anthropic API as a second model provider and its API key

- Status: Proposed
- Date: 2026-11-14

## Context

Microsoft Foundry is the only model provider the analyzer can use today ([ADR-0013](0013-adapters-for-every-external-system.md), [ADR-0023](0023-how-the-analyzer-calls-the-model.md)). Claude models on Foundry are outside the lab's credits, so the model comparison of [#503](https://github.com/pkalin/shadewright/issues/503) has only OpenAI-family results. On 2026-11-14 the maintainer decided that the Anthropic API is the first direct vendor path and goes into the lab ([#387](https://github.com/pkalin/shadewright/issues/387)). [#728](https://github.com/pkalin/shadewright/issues/728) builds the adapter and runs it from the evaluation harness; [#729](https://github.com/pkalin/shadewright/issues/729) wires it into the analyzer.

Two things change, and each touches an invariant:

- **A long-lived credential.** Every identity in the system is federated today: the analyzer reaches Foundry with a workload identity token, the executor gets short-lived Git App tokens. The Anthropic API takes a static API key. It is the first secret that does not expire by itself.
- **Redacted context goes to a second place.** With Foundry the context stays inside the Azure resource the operator chose, in the region of [ADR-0016](0016-lab-region-sweden-central.md). With the Anthropic API it is sent to the vendor's own service. Invariant 4 still decides what may be sent (everything is redacted first); it says nothing about where it goes.

The decisions below were taken on 2026-11-14. This ADR writes them down and states what follows from them.

## Decision

[![Where the context goes and where the key lives for each provider: the analyzer builds the same redacted context and calls the provider of the deployment; Foundry is reached with a workload identity token and stays in Azure, the Anthropic API with a key read from a mounted Secret and leaves Azure; executor and verifier never see the key](../assets/diagrams/model-providers.svg)](../assets/diagrams/model-providers.svg "Open full size")

1. **The provider is chosen per `ModelDeployment`.** An optional field `provider` takes `Foundry` (the default, so existing objects keep their meaning) or `Anthropic`. Triage and escalation are separate deployments and may use different providers. There is no fallback between providers at run time: if a call fails and the bounds of [ADR-0023](0023-how-the-analyzer-calls-the-model.md) are used up, the analysis is handed off.

2. **What `Anthropic` requires of a deployment.**
   - `endpoint` must be `https://api.anthropic.com`, enforced by a CEL rule on the object, so the API server refuses anything else.
   - `deployment` holds the model ID exactly as replies name it. The adapter refuses a reply that names another model, as the Foundry adapter does, and the evaluation list of [ADR-0023](0023-how-the-analyzer-calls-the-model.md) point 8 is keyed on that ID.
   - The host must still be listed in the analyzer's `--model-endpoint-suffixes`. There is no built-in allowance for it: an operator who has not listed `api.anthropic.com` has not agreed to send context there.

3. **The key is the one long-lived credential in the system, and it is handled as one.**
   - It lives in a Secret that the operator of the cluster creates. Shadewright's chart never creates it and never contains its value.
   - The chart mounts that Secret read-only into the analyzer pod only, and refuses to render an `Anthropic` deployment without a Secret name. The executor and the verifier never get it.
   - The analyzer gets no RBAC on Secrets. The kubelet mounts the file; the analyzer cannot list or read any other Secret because of this change.
   - The adapter reads the file on every call, so rotation is a Secret update and needs no restart. A missing or empty file is a refusal to start the analysis with a reason that does not name the key.
   - The key never appears in logs, events, status, analysis records, traces or error text. The adapter sends it only in the request header, only to the host the endpoint names, and refuses any other host.

4. **Invariants 1 and 4 hold unchanged.** The context sent is the same redacted context as for Foundry, built by the same code. The reply is untrusted: it is parsed by the same typed code into the same result, and the proposal still goes through the one validator and the policy gate. Model output is never authorization.

5. **A model must pass the evaluation first.** As for any model ([ADR-0025](0025-analysis-claims-units-and-evaluation-manifest.md)), the analyzer refuses automatic analysis with a model that has no passing record for the current prompt hash. The evaluation manifest and every analysis record name the provider as well as the model, so a result cannot be read as belonging to the wrong one.

6. **Data handling is stated, not hidden.** With this provider the redacted context leaves Azure. The region choice of [ADR-0016](0016-lab-region-sweden-central.md) does not cover it. The operator decides this per deployment by creating the Secret and listing the host. The data handling page says so in plain words next to the Foundry description, and says which vendor terms the operator has to check; it does not restate the vendor's retention or region terms as facts of this project (see the open questions).

7. **The lab.** The Secret is created at lab-up from the admin machine, from a key kept outside every repository. It never goes through Git, and never through Terraform state or variables. Lab-down deletes it with the namespace; the key itself is revoked or rotated in the vendor's console when the lab session ends if the spend limit below is not enough on its own.

## Consequences

**A leaked key.** What it allows is inference on the vendor account up to the spend limit set on that key. It gives no access to the cluster, the Git host or any Shadewright state. The cost of a leak is therefore bounded by the limit, not by anything Shadewright enforces. `maxTokensPerDay` of [ADR-0023](0023-how-the-analyzer-calls-the-model.md) limits the analyzer's own use and does not limit someone who holds the key.

**What an operator must set.** A spend limit on the key (or on the workspace that owns it) at the vendor, sized to the daily token limit. The install notes say this is required, and the chart cannot check it. Use a key made for this cluster, never one shared with other work.

**Rotation.** Create a new key, update the Secret, wait for the kubelet to refresh the mounted file (it can take about a minute), revoke the old key. No restart. A call that races the refresh uses the old key until it is revoked; revoke after the first successful call with the new one. A file that is briefly unreadable during the refresh is a retryable error, not a hand-off with a final reason.

**Who can read the key.** Anyone who can `exec` into the analyzer pod, read the Secret in the API, or read the node's kubelet volume. This is the same boundary as for any mounted Secret. The operator limits it with RBAC on Secrets in the operator namespace; the chart does not grant it to anyone.

**NetworkPolicy does not help.** The analyzer's egress rule already allows TCP 443 to any address, and a standard NetworkPolicy cannot tell hosts apart ([design 10](../design/10-security.md), accepted in [#391](https://github.com/pkalin/shadewright/issues/391)). Adding the Anthropic API changes no rule. What keeps the key and the context going to one host is the endpoint check in the adapter, the CEL rule, and the allow-list of suffixes. A compromised analyzer could send what it holds, now including this key, to any HTTPS host. A policy that names hosts needs a CNI with that feature and is not part of this ADR.

**Invariant 6 wording.** `AGENTS.md` lists what the analyzer may do and has no sentence for a mounted file. The build PR has to add this sentence to the analyzer's part of invariant 6, after "plus model inference and creating analysis records in its blob container (ADR-0015, ADR-0029)":

> and, only when a `ModelDeployment` has `provider: Anthropic` (ADR-0037), read-only access to one mounted file, the vendor API key, from a Secret the cluster operator creates; the analyzer has no RBAC on Secrets, the key is the one long-lived credential in the system, and the executor and the verifier never receive it.

This ADR does not edit `AGENTS.md`.

**Other costs.**
- The chart gets a Secret name value and a rule that refuses to render without it; the kustomize manifests in `config/` get no mount, so this provider is Helm only until someone asks.
- The adapter and the analyzer need a test that no error path holds the key.
- There are now two sets of evaluation results per model family, and a model comparison must say which provider produced a row.
- A leak through a heap dump or a core file of the analyzer is possible, as for any process that holds a key in memory for the length of a call.

## Alternatives considered

- **Claude on Microsoft Foundry only.** Keeps one provider and no static key, and the data stays in Azure. Rejected for the lab for now: the models are outside its credits ([#503](https://github.com/pkalin/shadewright/issues/503)). It remains the choice for an operator who needs the data to stay in Azure.
- **A provider setting for the whole `RemediationSettings` instead of per deployment.** Simpler, but it forbids a cheap model from one provider for triage and another provider for escalation, and it makes the data decision coarser than the deployment that carries it.
- **Fallback to the other provider on failure.** It would send context to a provider the operator did not choose for that call, and a bug in one adapter could be hidden by the other. Rejected, as in [#387](https://github.com/pkalin/shadewright/issues/387).
- **The key read by the operator with `get` on a Secret.** Rotation would be a watch or a read per call, and the analyzer would hold RBAC on Secrets, a grant that is one `resourceNames` mistake away from reading the Git App's. A mounted file needs no grant.
- **The key in an environment variable.** Needs a restart to rotate, and shows up in process listings of some debug tools. A file is read on every call instead.
- **The key from a cloud secret store with a federated identity.** Fits the rest of the system, but ties the provider to one cloud's store and adds a dependency the first slice does not need. Possible later behind the same file interface (a CSI driver mounts to the same path).
- **A built-in allowance for `api.anthropic.com`.** Convenient, but it would make sending context to a vendor the default for anyone who sets `provider: Anthropic` without reading the allow-list. Rejected.
- **A vendor SDK.** Plain `net/http`, as the Foundry adapter, so the dependency list does not grow and the host check stays in one place ([#728](https://github.com/pkalin/shadewright/issues/728)).

## Open questions

These need an answer from the vendor's published terms, with a link, before this ADR is accepted. None is stated as fact here.

1. How long the vendor keeps requests and replies sent through the API, whether an operator can ask for zero retention, and whether that setting belongs to the key, the workspace or the organization.
2. Where inference runs and whether the operator can choose a region, so the data handling page can say what ADR-0016 does not cover.
3. Whether the vendor trains on API traffic by default under the terms that apply to the lab account and to a typical operator.
4. Whether a spend limit can be set per key or only per workspace, and how fast it takes effect, since the leak bound rests on it.
5. Whether keys can be scoped (to one model, one workspace) so the leak bound is narrower than the whole account.
6. Whether `deployment` can use an alias that moves to a new model, or only dated IDs. An alias would let the model change under a passing evaluation record; the adapter's check of the reply's model name would catch it, and this needs confirming with a real reply.
7. Whether prompt caching is offered for the models the lab will use, and with what minimum prefix size (decision 5 of #728 depends on it).
8. Whether the `endpoint` pattern of the current API accepts `https://api.anthropic.com` unchanged (no trailing path); the CEL rule and the pattern must agree.

## Amendments

None.
