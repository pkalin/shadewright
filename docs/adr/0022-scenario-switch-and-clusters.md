# ADR-0022: Switching a scenario on with a per-cluster branch, on Kind and the lab

- Status: Proposed
- Date: 2026-11-03

## Context

Stage 0 is done when every scenario can be reproduced with one command ([design 13](../design.md#13-milestones)). There are six scenarios (#34 to #39), and their harness (#33) needs `make scenario-<name>` and `make scenario-reset` against the current kube context. This ADR decides how that command breaks the cluster and on which clusters it runs. Four facts limit the choice:

- **The broken object must come from Git.** A remediation pull request can be made only for an object that a Flux Kustomization applies from one plain manifest ([ADR-0012](0012-object-to-source-mapping.md), [design 8.2](../design.md#82-making-a-change)). The value in that file must equal the live value, and the path's kustomization file may hold only `apiVersion`, `kind`, `metadata` and `resources`: `components`, patches and generators stop the mapping with `NeedsIntervention`. Spec fields of the Flux Kustomization such as `components`, `patches` and `postBuild` stop it too. `kubectl apply` or `kubectl patch` of a broken manifest therefore cannot produce the positive case (OOM, #36, the v0.1.0 acceptance in #58).
- **The pull request targets the branch Flux tracks.** The `GitRepository` must track a branch (`spec.ref.branch`), and that branch must equal the repository's `targetBranch` in RemediationSettings. `GitBranch` accepts names such as `cluster/lab` and refuses the `shadewright/` prefix, which the GitHub adapter keeps for its own head branches.
- **`main` of `shadewright-fleet` is protected.** Changes reach it only through pull requests with the required checks `Conventional PR title` and `Render manifests`, and the operator's GitHub App never bypasses the ruleset (fleet `AGENTS.md`). `Render manifests` (`scripts/render.sh`) renders every kustomization under `clusters/`, `infrastructure/` and `apps/` with `kubeconform -strict`. It leaves `scenarios/` out on purpose: those files are broken, and the script says the scenario harness renders them.
- **Where things run.** End-to-end tests run on Kind in GitHub Actions, and the AKS lab is applied for a session and destroyed after ([ADR-0008](0008-disposable-azure-lab.md)). On the lab, Flux is the AKS extension: `lab/flux.tf` in `shadewright-azure` creates a `GitRepository` named `fleet` that tracks `var.fleet_branch` (default `main`) and polls every 60 seconds, plus a Kustomization `lab` that applies `./clusters/lab` with the Azure values as `postBuild` substitutions. `clusters/lab` applies `infrastructure` and then `apps`, both with `prune: true`. The demo workloads go into `apps/demo/`, which has no substitutions, so ADR-0012 can map them.

The criteria, from #32: one command switches a scenario on and one command resets to healthy; it works on Kind without Azure; branch protection on `main` stays intact; the agent's pull request still targets the branch Flux tracks. Two more come from the invariants: changes go through Git only (invariant 3), and the lab is paid only while it runs.

### Options for switching a scenario on

| | Switch | Reset | Kind without Azure | `main` protection | Agent's PR targets the tracked branch | ADR-0012 can map the object | Render check |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 1. `kubectl apply` or `patch` of the broken object | One command | One command, or wait for Flux to correct the drift | Yes | Intact | Not applicable: no mapping | No: the file and the live value differ | Not involved |
| 2. A pull request to `main` that swaps a plain file in `apps/demo/` | A PR, then a review and a merge | Another PR | Only for one cluster at a time, since every cluster tracking `main` breaks together | Intact, but every scenario uses the admin bypass to merge the maintainer's own PR | Yes (`main`) | Yes | Runs on the scenario; the invalid-manifest scenario (#39) cannot pass it |
| 3. Kustomize components in `apps/demo/`, switched on by a commit | One commit (on `main`, a PR as in option 2) | One commit | As option 2 | As option 2 | Yes | No: `components` in the kustomization file stops the mapping | As option 2 |
| 4. One Flux Kustomization per scenario (path `./scenarios/<name>`), created or resumed with `kubectl` or `flux` | One command | Delete or suspend it | Yes | Intact | Yes | Only if the file in `scenarios/<name>/` is in the policy's `allowedPaths`, and the same Deployment from `apps` and from the scenario makes two Kustomizations fight over it | The file the agent patches is in `scenarios/`, which the render check skips |
| 5. Each cluster's Flux tracks its own branch, which the harness rebuilds from `main` plus one scenario commit (**recommended**) | One command: commit and push to that branch | One command: point the branch at `main` again | Yes: Kind reads the same public repository | Intact: the harness never writes to `main` | Yes: the cluster's branch | Yes: the broken file is a plain manifest in `apps/demo/` | Stays a gate on `main` and on the agent's PRs; scenario commits are rendered by the harness |

Option 1 is what the design already rules out: a patched object no longer equals its file, and Flux reapplies the file on its next reconcile. Option 4 creates Flux objects outside Git and keeps the broken file outside `apps/demo/`, where #34 to #39 say only the demo workloads change and #58 limits the policy to `apps/demo`. Options 2 and 3 keep the most realistic setup (the agent's PR goes to protected `main`), but each switch and reset is a reviewed pull request, a scenario on one cluster breaks every cluster that tracks `main`, CI cannot switch scenarios at all, and #39 (a manifest that does not render) can never merge without bypassing a required check.

### Options for where scenarios run

- **Kind only:** free and needs no Azure, but has no workload identity, Key Vault signer or managed model ([ADR-0008](0008-disposable-azure-lab.md)), so it cannot run the diagnosis and pull request loop of #58.
- **The lab only:** the real loop, but every iteration on a scenario costs node hours, and CI cannot run it.
- **Both:** Kind reproduces symptoms and checks the healthy baseline, locally and in CI; the lab runs the full loop when a session is up.

For Kind, Flux can read the public `shadewright-fleet` repository over HTTPS, as on the lab, or a Git server inside the Kind cluster. A local server works offline and in CI without any write token, but it is one more component to pin, its URL differs from the lab's, and no pull request can be opened against it (the only `GitProvider` is GitHub).

## Decision

Proposed:

1. **Each cluster tracks its own branch.** Flux on a cluster tracks `cluster/<cluster>` (`cluster/lab` for the AKS lab, `cluster/kind` for a local Kind cluster), never `main`. Between scenarios that branch points at the same commit as `main`. RemediationSettings on that cluster set `targetBranch` to the same branch, so the agent's pull requests go there, as ADR-0012 requires.
2. **A scenario is one commit on that branch.** `scenarios/<name>/` holds `README.md`, `expected.yaml` and the broken versions of files under `apps/demo/`, at the same relative paths. `make scenario-<name>`:
    - reads the branch from the `GitRepository` `flux-system/fleet` in the current kube context and refuses unless it starts with `cluster/`, so it can never push `main` or another cluster's branch;
    - starts from the current `origin/main`, copies the scenario's files over `apps/demo/` (only paths inside `apps/demo/`), renders `apps/` with the same tools and versions as `scripts/render.sh` (a render failure is expected only for scenarios that declare it, such as #39), and commits;
    - pushes that commit to the cluster's branch with `--force-with-lease`, then may ask Flux to fetch now (`flux reconcile source git fleet`, which only sets an annotation).
3. **Reset is pushing `main` to the branch.** `make scenario-reset` points the cluster's branch at the current `origin/main` with `--force-with-lease`. It is a no-op when they are already equal, and `prune: true` on `apps` removes anything a scenario added. One scenario is on at a time per cluster.
4. **Scenarios run on both Kind and the lab.** Kind (pinned Kind and Flux versions, #33) reproduces every symptom and runs the baseline check in CI, without Azure. The lab runs the diagnosis and the agent's pull request (#58). Kind reads the public repository from GitHub like the lab, so the two clusters differ only in their branch and entry point; a Git server inside Kind is not used for now. The CI baseline job points Flux at the head of the pull request under test, read-only, and does not push.
5. **The cluster branches get their own ruleset.** `cluster/*` requires a pull request with one approval and the `Render manifests` check, with only the maintainer (the harness's identity) allowed to bypass it for the harness's direct pushes and force pushes. The operator's GitHub App is never a bypass actor there either, so its pull requests to `cluster/lab` wait for a person, as they would on `main`.

## Consequences

- One command switches a scenario on and one resets it, on any cluster whose Flux tracks a `cluster/` branch, and nothing about a scenario reaches `main`. `main` keeps its ruleset, and every change to it is still a reviewed pull request that renders.
- The broken object is a plain manifest in `apps/demo/` delivered by the `apps` Kustomization, so ADR-0012 maps it and the OOM pull request (#36, #58) changes one line in `apps/demo/`, inside the policy's `allowedPaths`.
- Invariant 3 holds: the scenario, the agent's change and the reset are all Git commits that Flux applies; no object is changed with `kubectl` except the reconcile annotation. The agent still writes only `shadewright/` branches and opens pull requests against the branch Flux tracks.
- The agent's pull requests target `cluster/lab`, not `main`. The demo of branch rules and, at stage 4, of `CODEOWNERS` and auto-merge is only as strong as the `cluster/*` ruleset, which therefore has to mirror the rules on `main` for the App. A fix merged into `cluster/lab` is discarded at the next reset, on purpose: the scenario stays reproducible.
- Scenario commits are pushed directly, so no required check gates them; the harness renders them instead. `Render manifests` still runs on the agent's pull requests to `cluster/lab`, because its `pull_request` trigger has no branch filter, and the `cluster/*` ruleset makes it required there.
- A reset while an agent pull request is open moves its base branch under it. The operator must treat that as a conflict ([design 8.4](../design.md#84-branch-protection): a changed target stops automation), which is also a stage 3 test case.
- Kind needs an entry point of its own (`clusters/kind/`) that applies `apps/` and the infrastructure the scenarios need (KEDA, and Prometheus for the OOM evidence, [ADR-0017](0017-prometheus-in-the-lab.md)) without the Azure substitutions. `infrastructure/` is still empty, so its layout has to keep the Azure-only parts (the Shadewright install with workload identity) apart from the rest.
- The lab switches from `main` to `cluster/lab` by setting `fleet_branch` in the lab's git-ignored `terraform.tfvars`, or by changing its default; either is a Terraform change that goes through plan and an explicit approval. The lab still costs node hours only while a session is up, and scenario work starts on Kind.
- Running scenarios (not only the baseline) in CI would need a token that can push `cluster/` branches from a workflow; this ADR does not grant one.

## Alternatives considered

- **`kubectl apply` or `patch` of a broken manifest** (option 1): rejected by design 13 and 8.2; the object no longer matches its file, and Flux reverts it.
- **A pull request to `main` per scenario** (option 2): the most realistic target branch, but switching is not one command, it breaks every cluster at once, it cannot run in CI, and #39 cannot pass the required render check.
- **Kustomize components** (option 3): `components` in the kustomization file is outside what ADR-0012 maps, so the positive case would end in `NeedsIntervention`.
- **One Flux Kustomization per scenario, switched with `kubectl`** (option 4): Flux objects created outside Git, the broken file outside `apps/demo/` and outside the render check, and two Kustomizations owning one Deployment.
- **A Git server inside Kind:** offline and token-free, but no pull requests and one more component; worth revisiting if CI must run scenarios without a write token.
- **One branch per scenario, with Flux pointed at it:** switching a scenario would change the `GitRepository` (on the lab, the Flux configuration in Terraform), which is slow and puts Flux configuration on the scenario path.

## Amendments

None.
