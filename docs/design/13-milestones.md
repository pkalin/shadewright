# 13. Milestones

The work goes in six stages. Stages 0 to 4 say when they are done.

The first end-to-end result is: one controlled incident, one documented diagnosis, one safe PR, approval, Flux delivery and a confirmed recovery.

**Stage 0: lab and scenarios**

- **Covers:** a disposable lab cluster, the fleet repo, branch rules and a scenario set.
- **Scenarios:** bad image, missing Secret reference, OOM, unschedulable pod, broken KEDA trigger, invalid manifest.
- **Expected outcome:** every scenario has one, including the cases where the agent must refuse.
- **Done when:** every scenario can be reproduced with one command.

How a scenario is switched on and where it runs is decided in [ADR-0022](../adr/0022-scenario-switch-and-clusters.md):

- Each cluster's Flux tracks its own `cluster/<cluster>` branch.
- A scenario is one commit on that branch.
- Scenarios run on Kind and the lab.
- A push builds the scenario only from `origin/main`.

**Stage 1: observe and diagnose, read-only**

- **Covers:** the collector, fingerprints, aggregation, durable incidents, model integration and the analysis output.
- **No changes** to workloads and **no writes** to Git.
- **Allowed cluster writes:** only the operator's own control state. That is Incidents and their status, an analysis budget ConfigMap, the settings status, a leader election Lease and Events.
- **Done when:**
  - a burst of repeated symptoms makes one incident;
  - a restart does not cause an analysis storm;
  - each diagnosis names its evidence.

**Stage 2: RemediationRequest and PR with review**

- **Covers:** the CRDs, the controller state machine, policy, Git source mapping, typed patches and the GitHub App through Key Vault.
- **Approval:** every request has `approvalRequired: true`.
- **Done when:**
  - one scenario produces exactly one correct PR;
  - repeated reconciles do not open another;
  - an unknown change or stale context stops the process.

**Stage 3: delivery and verification**

- **Covers:** merge, Flux revision and operational effect. Also timeouts, Git conflicts, rejected PRs and incidents that resolved before rollout.
- **Done when:**
  - `Succeeded` appears only after the right revision is applied and stability criteria hold;
  - every failure has a clear status and escalation.
- **Built so far:** the merge receipt (#678), and `WaitingForDelivery` and `Verifying` ending `Failed` or `Inconclusive` (#681), behind the executor flag `--verify-outcomes`, off by default; the verifier, its report and the end `Succeeded` are not built yet (#682, #683).

**Stage 4: controlled auto-merge in the lab**

- **Covers:** one or two narrowly defined change classes with `approvalRequired: false`, matching `CODEOWNERS` and ruleset, no bypass.
- **Done when:**
  - the positive case merges automatically;
  - negative attempts stop on unmet rules, extra diff, exceeded bounds or a swapped plan.

**Stage 5: extensions**

- More integrations and platforms (2.4, 2.5).
- More observability adapters.
- MCP.
- Argo CD, Azure DevOps and GitLab adapters.
- Crossplane.
- More clusters.
- Declarative OCI extension packs.
