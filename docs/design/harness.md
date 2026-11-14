# The analyzer as a harness

The analyzer calls a model, but the model is the smallest part of what makes it safe. Around it is a harness: code and rules that decide what the model is told, what it sees, what it can touch and how its work is checked. The model reasons. Nothing it says is authorization: its only output is a proposal, and a deterministic policy check outside the model decides whether it goes any further (invariant 1).

[![The analyzer as a harness around the model. In the middle, the model proposes only. Guides, what it follows: the analysis prompt and output schema, the invariants and ADRs, the closed action catalog with bounded parameters. Context, what it sees: the context builder with redacted, bounded, delimited blocks from pod logs, Prometheus metrics and the Incident, which is made from Events, the allow-listed fields of a KEDA ScaledObject that is the subject, and the bounds of the policy that will decide a proposal. Tools, what it touches: read-only tools such as list_events, read_git_file (planned), the analyzer's RBAC (read-only on the cluster, writes only its Incidents, budget and proposals), the analysis budget and time limit. Checks, how it is checked: malformed output refused, the evaluation harness and injection corpus, OpenTelemetry traces, analysis records. Below, outside the model: the RemediationRequest proposal, the policy check with the plan digest, the executor's GitOps pull request, a person merges, Flux deploys, and the verifier samples the pods and writes a report only for requests in Verifying, which needs the executor flag --verify-outcomes, off by default; the executor reads the report as untrusted evidence and decides whether the symptom cleared).](../assets/diagrams/harness.svg)](../assets/diagrams/harness.svg "Open full size")

Dashed boxes are designed, not built. The teal box is GitHub, outside the cluster; Flux runs in the cluster.

## Where each box is described

| Box | Status | Design section or code |
| --- | --- | --- |
| Prompt and output schema | <span class="sw-status sw-built">built</span> | [3.3, the prompt and the result](3-observation-and-deduplication.md#the-prompt-and-the-result); `internal/analysis/prompts` |
| Invariants and ADRs | <span class="sw-status sw-built">built</span> | [10. Security](10-security.md), [ADR-0003](../adr/0003-gitops-only-typed-remediation.md), [ADR-0023](../adr/0023-how-the-analyzer-calls-the-model.md) |
| Action catalog | <span class="sw-status sw-built">built</span> | [10.4, bounded actions](10-security.md#104-bounded-actions); `policy.CheckAction` |
| Context builder | <span class="sw-status sw-built">built</span> | [3.3, the context builder](3-observation-and-deduplication.md#the-context-builder); `internal/incidentcontext`, `internal/redact` |
| Pod logs, metrics, the Incident (made from Events) | <span class="sw-status sw-built">built</span> | [3.1](3-observation-and-deduplication.md#31-collecting), [4.1](4-tools.md#41-query_metrics), [4.2](4-tools.md#42-get_pod_logs) |
| Read-only tools | <span class="sw-status sw-built">built</span> | [4. Tools](4-tools.md); `internal/tools` |
| `read_git_file` | <span class="sw-status sw-planned">planned</span> | [4. Tools](4-tools.md), [#373](https://github.com/pkalin/shadewright/issues/373) |
| Analyzer RBAC | <span class="sw-status sw-built">built</span> | [10.1](10-security.md#101-least-privilege-rbac), [10.11](10-security.md#1011-what-each-identity-can-read-and-write) |
| Budget and time limit | <span class="sw-status sw-built">built</span> | [3.2, the analysis budget](3-observation-and-deduplication.md#the-analysis-budget), [the loop and its limits](3-observation-and-deduplication.md#the-loop-and-its-limits) |
| Malformed output refused | <span class="sw-status sw-built">built</span> | [3.3, the prompt and the result](3-observation-and-deduplication.md#the-prompt-and-the-result); `internal/analysis/result.go` |
| Eval harness and injection corpus | <span class="sw-status sw-built">built</span>, the analyzer's own gate <span class="sw-status sw-planned">planned</span> | [11.2, evaluation gate](11-model.md#evaluation-gate), [evaluation](../evaluation.md); `test/eval` |
| OpenTelemetry traces | <span class="sw-status sw-built">built</span>, off by default | [Traces reference](../reference/traces.md) |
| Analysis records | <span class="sw-status sw-built">built</span>, with a record store | [ADR-0015](../adr/0015-analysis-records-outlive-the-lab.md); `internal/analysis/record.go` |
| Proposal, policy check, GitOps PR | <span class="sw-status sw-built">built</span>, up to `Merged`, then `WaitingForDelivery` and `Verifying` ending `Failed` or `Inconclusive` behind `--verify-outcomes` (off by default) | [6. Reconciliation](6-reconciliation.md), [8. GitOps on GitHub](8-gitops-on-github.md) |
| Person merges (GitHub), Flux deploys (in the cluster) | not part of Shadewright | [8. GitOps on GitHub](8-gitops-on-github.md) |
| Verifier | <span class="sw-status sw-built">built</span>, writes reports only with `--verify-outcomes` on (off by default); the executor reads them as untrusted evidence and decides `Cleared` (<span class="sw-status sw-built">built</span>, #683) | [10.9, verification](10-security.md#109-verification) |
