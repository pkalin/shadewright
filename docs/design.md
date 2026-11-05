# Design

Status: draft for the MVP.

**Built**

- The API and its validation.
- The policy check step of section 6, with the source mapping and the freshness check.
- The YAML patchers.
- The semantic diff check of section 8.2.
- The GitHub and Key Vault adapters.
- The pull request step of section 6: branch, commit and pull request, up to `Merged`.
- The kill switch of section 5.4: `spec.paused`, one gate right before every Git write.
- The concurrency limit, the lock per target and the circuit breaker of section 10.7. Retry limits and cooldowns between attempts are not built.
- The collector of section 3.1 and both levels of deduplication of section 3.2, with the Incident controller that keeps them in `Incident` objects.
- The RBAC of sections 2.2 and 10.
- The Prometheus metrics of both components ([reference](reference/metrics.md)).

**Built, but not used by the analyzer yet**

- The Prometheus `MetricsSource` adapter, with its approved query templates, and the pod logs `LogSource` adapter, bounded and redacted. The analyzer creates both from its flags and gives them to the context builder.
- The Microsoft Foundry `ModelProvider` adapter for OpenAI-format models (section 11.2), with bounded retries, workload identity and malformed output refused. Not wired yet.
- The missing reference check (section 4). It infers a missing Secret or ConfigMap without reading either.
- The context builder (section 3.3). It makes a bounded, redacted context from them and the analyzer's reads. The analyzer creates it; the analysis loop calls it, and the analyzer does not start the loop yet (#76).

**Built and wired into the analyzer**

- The registries of platform and integration modules of sections 2.4 and 2.5, with the symptom source contract.
- The generic and AKS platforms, and the EKS and GKE platforms (detection only).
- The cloudflared, Flux, KEDA, Kyverno and Secrets Store CSI driver integrations.
- Platform and integration detection. The analyzer runs it and reports it in the settings status (5.4).
- The symptom source evaluator, with the KEDA HPA source: a replica ceiling becomes an Incident about the `ScaledObject` (3.1, 2.5.3).
**Built, but feeding nothing yet**

- The diagnosis rules.
- The cloudflared, Flux and Kyverno symptom sources, and KEDA's `ScaledObject` and `ScaledJob` sources.

**Not built yet**

- Everything else is design. Diagrams mark unbuilt steps *(planned)*.

Decisions that are already made are recorded in the [ADRs](adr/index.md). This page describes the whole system and lists what is still open.

## Sections

- [1. Goals](design/1-goals.md)
- [2. Architecture](design/2-architecture.md)
- [3. Observation and deduplication](design/3-observation-and-deduplication.md)
- [4. Tools](design/4-tools.md)
- [5. API](design/5-api.md)
- [6. Reconciliation](design/6-reconciliation.md)
- [7. Existing controllers](design/7-existing-controllers.md)
- [8. GitOps on GitHub](design/8-gitops-on-github.md)
- [9. Identity](design/9-identity.md)
- [10. Security](design/10-security.md)
- [11. Model](design/11-model.md)
- [12. Cost](design/12-cost.md)
- [13. Milestones](design/13-milestones.md)
- [14. Testing](design/14-testing.md)
- [15. Decisions made and open questions](design/15-decisions-made-and-open-questions.md)
- [16. Prior art](design/16-prior-art.md)

<script id="design-redirects" type="application/json">{"1-goals":"1-goals/","non-goals-for-the-mvp":"1-goals/","first-scope":"1-goals/","2-architecture":"2-architecture/","21-flow":"2-architecture/","22-components":"2-architecture/","23-provider-interfaces":"2-architecture/","24-cluster-platforms":"2-architecture/","25-cluster-integrations":"2-architecture/","3-observation-and-deduplication":"3-observation-and-deduplication/","31-collecting":"3-observation-and-deduplication/","32-two-levels-of-deduplication":"3-observation-and-deduplication/","33-analysis":"3-observation-and-deduplication/","4-tools":"4-tools/","5-api":"5-api/","51-remediationrequest":"5-api/","52-incident":"5-api/","53-remediationpolicy":"5-api/","54-remediationsettings":"5-api/","6-reconciliation":"6-reconciliation/","7-existing-controllers":"7-existing-controllers/","8-gitops-on-github":"8-gitops-on-github/","81-what-approvalrequired-means":"8-gitops-on-github/","82-making-a-change":"8-gitops-on-github/","83-auto-merge":"8-gitops-on-github/","84-branch-protection":"8-gitops-on-github/","9-identity":"9-identity/","10-security":"10-security/","11-model":"11-model/","111-setup":"11-model/","112-model-strategy":"11-model/","12-cost":"12-cost/","121-formula":"12-cost/","122-illustration-made-up-prices-not-a-quote":"12-cost/","123-controls":"12-cost/","13-milestones":"13-milestones/","14-testing":"14-testing/","15-decisions-made-and-open-questions":"15-decisions-made-and-open-questions/","16-prior-art":"16-prior-art/"}</script>
<script>(function(){var h=decodeURIComponent(location.hash.slice(1)),e=document.getElementById("design-redirects");if(!h||!e)return;var t=JSON.parse(e.textContent)[h];if(t)location.replace(t+"#"+h);})();</script>
