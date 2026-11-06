---
title: "Lab runbook: the OOM scenario produces one reviewed pull request"
---

# Lab runbook: the OOM scenario produces one reviewed pull request

This is the acceptance run for v0.1.0 ([design 13](design/13-milestones.md#13-milestones), stage 2): on the lab cluster, a
hand-written `RemediationRequest` for the OOM scenario produces exactly one correct pull request in the fleet
repository, applying it again opens nothing new, and a change of the policy's bounds closes the pull request and
ends the request `Superseded`. Decisions behind it: [ADR-0006](adr/0006-github-app-key-in-key-vault.md) (the GitHub
App and its Key Vault key), [ADR-0008](adr/0008-disposable-azure-lab.md) (the lab is applied for a session and
destroyed after), [ADR-0012](adr/0012-object-to-source-mapping.md) (finding the file in Git) and
[ADR-0022](adr/0022-scenario-switch-and-clusters.md) (the scenario switch and the `cluster/lab` branch).

The fleet installs both the analyzer and the executor, but the analyzer does no work in this run: the request is
written by hand, the analyzer runs without `--triage-model` and the settings sample has `analysis.maxAnalysesPerDay: 0`,
so the model is never called. The optional [evaluation run](#evaluation-run-the-first-analysis-with-a-real-model) at
the end turns the analysis on for one analysis.

!!! warning "Costs money and changes real systems"
    Steps 1 and 11 create and destroy Azure resources (about 0.10 EUR per hour while the lab is up, see
    [shadewright-azure, Cost](https://github.com/pkalin/shadewright-azure#cost)). Steps 6 to 9 open and close a real
    pull request in your fleet repository. Run them yourself; nothing here is meant for CI or an agent.

Commands run from the repository named at the start of each step. `<owner>` is your GitHub account. Nothing below
puts a subscription, tenant or object ID, a vault name or a URL of yours into a Git-tracked file: the values go
into the samples on your machine only, and the samples in Git keep their `REPLACE` placeholders.

## Before you start

- The one-time setup of [shadewright-azure](https://github.com/pkalin/shadewright-azure#first-run) is done: state
  storage, foundation, and the GitHub App registered with its key imported and the executor's `sign` grant
  (`github_app_key_imported = true`).
- The fleet repository has the branch `cluster/lab` and its two rulesets (ADR-0022, decision 5). The scenario
  harness never creates a branch, so create it once from `main` if it is missing.
- `cluster/lab` is reset to `main`'s tree (`make scenario-reset CLUSTER=lab` in `shadewright-fleet`; it needs the
  kube context of the lab, so step 2 runs it once the lab is up). A branch that predates the vendored agent install
  has no agent for Flux to apply, and `Path guard` reads its workflow, allow list and `CODEOWNERS` from the base
  branch, so an old cluster branch also fails the agent's pull request. The reset is a no-op when the trees are equal.
- `lab/terraform.tfvars` in `shadewright-azure` (git-ignored) has `flux_enabled = true`, `fleet_repository_url`
  set to your fleet repository and `fleet_branch = "cluster/lab"`. Flux must track `cluster/lab`, and
  the settings sample's `targetBranch` must be the same branch, or the source mapping denies the request (`Denied`, reason `BranchMismatch`).
- `az`, `kubectl`, `kubelogin`, `terraform`, `curl`, `gh`, `jq` and `make` are installed, and `gh auth status` shows an account that can
  read your fleet repository.

## 1. Bring the lab up

In `shadewright-azure`:

```bash
make lab-cost    # price per hour for the current settings; changes nothing
make lab-up      # init, plan, plan summary and price, then asks
```

Expect a plan with only creates and no Flux resources (the extension is gone),
the price per hour, and the prompt. Read the plan, then type `yes` (lower case) to apply exactly that saved plan.
Anything else applies nothing. `make lab-up` then installs Flux in the same run (about two minutes: the pinned manifest, the `GitRepository` `fleet` and the root Kustomization `lab`); if that step fails, the cluster is up and billed, so run `make lab-flux` (it asks for a typed yes) to repeat it. Then fetch credentials for the session:

```bash
eval "$(terraform -chdir=lab output -raw get_credentials)"
kubelogin convert-kubeconfig -l azurecli
kubectl get nodes
```

Expect one node in `Ready`. Note the time: the lab bills until step 11.

## 2. Reset `cluster/lab` to `main`

Flux tracks `cluster/lab`, and that branch may still hold an old tree that predates the agent install, so right
after `make lab-up` Flux applies it and there is no agent. Reset it before checking anything else. In
`shadewright-fleet`, with the kube context of the lab:

```bash
make tools                        # once, if bin/ is missing
make scenario-reset CLUSTER=lab   # cluster/lab holds main's tree
```

Expect one commit on `cluster/lab` whose tree equals `main`'s, or the message that the branch "already holds this tree" if it does. Flux picks
it up within about two minutes (the sync interval is 60 seconds for the repository, 120 for the Kustomization). On
the first reset of a branch that predates the agent install, the `shadewright` Kustomization is created only then
and waits for `infrastructure` and for its Deployments, so allow 5 to 10 minutes and rerun step 3 until all four
Kustomizations are ready.

## 3. Check Flux and the cluster

```bash
kubectl -n flux-system get gitrepository fleet -o jsonpath='{.spec.ref.branch}{"\n"}'
kubectl -n flux-system get kustomizations
kubectl -n demo get pods
```

Expect `cluster/lab`; the Kustomization that applies `./clusters/lab` (`lab`, created by `make lab-up`), `infrastructure`,
`shadewright` and `apps`, all `Ready=True` (the first sync takes a few minutes); and the demo pods `web` and `worker` running. If `infrastructure` is not ready, wait: `shadewright` and
`apps` depend on it.

## 4. Check the agent

The fleet installs the agent: the Flux Kustomization `shadewright` (`clusters/lab/shadewright.yaml`) applies
`infrastructure/shadewright-lab`, which patches the vendored install in `infrastructure/shadewright` (the
operator's `config/default` and `config/watch`, built at the commit in its `VENDOR` file). It applies the four
CRDs, the namespace `shadewright-system`, the analyzer and executor Deployments with their ServiceAccounts, RBAC and
NetworkPolicies, and a RoleBinding that lets the analyzer watch the `demo` namespace. Both images are pinned by
digest (also in `VENDOR`).

```bash
kubectl -n flux-system get kustomization shadewright
kubectl get crd | grep remediation.shadewright.io
kubectl -n shadewright-system get deploy,sa
kubectl -n shadewright-system get sa shadewright-executor -o jsonpath='{.metadata.annotations}{"\n"}'
kubectl -n shadewright-system get deploy shadewright-executor -o jsonpath='{.spec.template.spec.containers[0].image}{"\n"}'
```

Expect:

- the Kustomization `shadewright` `Ready=True` (it waits for its Deployments);
- the four CRDs (`incidents`, `remediationpolicies`, `remediationrequests`, `remediationsettings`);
- the Deployments `shadewright-analyzer` and `shadewright-executor` with all pods ready, and the ServiceAccounts
  `shadewright-analyzer` and `shadewright-executor`;
- the `shadewright-executor` ServiceAccount annotated `azure.workload.identity/client-id` with the executor
  identity's client ID, and an image reference ending in `@sha256:...`.

There is nothing to set by hand. The two client IDs reach the install as the substitutions `ANALYZER_CLIENT_ID` and
`EXECUTOR_CLIENT_ID`, which `lab/flux.tf` in `shadewright-azure` fills from the foundation outputs
`analyzer_identity` and `executor_identity` (the Kustomization of `./clusters/lab` forwards them to `shadewright`). To compare
the annotation with the source of truth, in `shadewright-azure`:

```bash
terraform -chdir=foundation output -json executor_identity | jq -r .client_id
```

The federated credentials of the lab name the namespace `shadewright-system` and those two ServiceAccount names
(`lab/federation.tf`), so renaming either side breaks the workload identity. If the Kustomization is not ready,
`kubectl -n flux-system describe kustomization shadewright` names the reason. Stop here if the checks above do not pass.

## 5. Switch the OOM scenario on

In `shadewright-fleet`, with the kube context of the lab:

```bash
make scenario-oom CLUSTER=lab
```

Expect the harness to confirm that the `GitRepository` tracks exactly `cluster/lab`, render the result and push one
commit to `cluster/lab`. It refuses on any other branch. Within about two minutes:

```bash
kubectl -n demo get pods -l app.kubernetes.io/name=web
kubectl -n demo get pods -l app.kubernetes.io/name=web \
  -o jsonpath='{.items[*].status.containerStatuses[*].lastState.terminated.reason}{"\n"}'
kubectl -n demo get deployment web -o jsonpath='{.spec.template.spec.containers[0].resources.limits.memory}{"\n"}'
```

Expect a pod in `CrashLoopBackOff` with a growing restart count, the last state reason `OOMKilled`, and the limit
`48Mi`. That `48Mi` is the `previous` value of the request.

## 6. Fill and apply the samples

On a lab whose fleet `main` has `infrastructure/shadewright-lab-config` (shadewright-fleet #38), the settings and the
`demo-memory` policy are already applied by Flux, with `maxAnalysesPerDay: 10` and the analysis on: skip applying
those two samples and see the [evaluation run](#evaluation-run-the-first-analysis-with-a-real-model). The Incident and
request samples below still apply.

The settings sample sets `spec.analysis.maxAnalysesPerDay`, which the CRDs of v0.1.0 do not have (they drop it,
and analysis would default to 100). So the fleet's vendored install (`infrastructure/shadewright/`, see its
`VENDOR` file for the commit) must be vendored from a `shadewright` commit at or after the move of that field
before you apply the sample; regenerate it with `scripts/vendor-shadewright.sh` in the fleet repository if it is
older.

In `shadewright`, copy the lab samples to a directory outside Git (so the real values never reach a commit) and fill
them. Each value comes from here:

| Placeholder | Where | Value from |
| --- | --- | --- |
| `url` in the settings | `remediation_v1alpha1_remediationsettings.yaml` | `https://github.com/<owner>/shadewright-fleet`, lower case, no `.git` (`fleet_repository_url` of `lab/terraform.tfvars`) |
| `appClientID` | `remediation_v1alpha1_remediationsettings.yaml` | The GitHub App's settings page, "Client ID" |
| `installationID` | `remediation_v1alpha1_remediationsettings.yaml` | The number at the end of the URL of the App's installation page (`gh api` cannot read it without the App's JWT) |
| `keyID` | `remediation_v1alpha1_remediationsettings.yaml` | `<uri>keys/<github_app_key>` from `terraform -chdir=foundation output -json key_vault` (`uri` ends with `/`; `github_app_key` is the key name, `github-app` by default) |
| `incidentRef.uid`, `targetRef.uid` | `remediation_v1alpha1_incident.yaml`, `remediation_v1alpha1_remediationrequest.yaml` | Commands in the file's header comment, after step 5 and the Incident below |

```bash
mkdir -p /tmp/shadewright-lab && cp config/samples/lab/*.yaml /tmp/shadewright-lab/
# edit remediation_v1alpha1_remediationsettings.yaml (url, appClientID, installationID, keyID)
kubectl apply -f /tmp/shadewright-lab/remediation_v1alpha1_remediationsettings.yaml
kubectl apply -f /tmp/shadewright-lab/remediation_v1alpha1_remediationpolicy.yaml
kubectl get remediationsettings default
kubectl get remediationpolicy demo-memory
```

Expect both created. Applying them checks the schema and CEL rules only; it proves nothing about GitHub, the
signer or the mapping. No controller writes the conditions of a policy, and the conditions of the settings hold only
the analyzer's detection (`UnknownModules`, `DetectionFailed`), so an empty status here is not a health check.
The executor builds the repository and its signer when a request first needs them, and a problem shows in the
conditions of the request (see the end of this step and step 7). Reasons to look for:

- `RepositoryNotConfigured` (`Denied`): the URL of the Flux `GitRepository` equals no `url` in the settings'
  `repositories`, or more than one. The match is exact, so a case difference or a `.git` suffix is enough;
- `RepositoryNotInPolicy` (`Denied`): the matching repository is not the policy's `gitOps.repositoryRef`, which
  includes a `repositoryRef` that names no settings entry;
- `BranchMismatch` (`Denied`): the branch Flux tracks is not the repository's `targetBranch`;
- `RepositoryUnusable` (`NeedsIntervention`): the settings of the repository (URL, client ID, installation ID, the
  Key Vault `keyID` or a missing signer) cannot be used to open it; fix the settings and apply a new request;
- `GitForbidden` or `GitRejected`: GitHub refused the App (permissions, installation, ruleset); the request ends
  `NeedsIntervention` before a pull request exists;
- no reason at all: a signer that cannot be built (no workload identity) or a Key Vault or GitHub error that
  may pass is retried quietly. Source mapping opens the repository and its signer before the full policy
  decision (after the field checks, design 6.2), so the request stays `Pending`, not `PolicyChecked`. The pull request deadline starts only after an `Allowed`
  decision, so nothing ends such a request. Read the executor log
  (`kubectl -n shadewright-system logs deploy/shadewright-executor`) and fix the identity or the Key Vault
  access it names.

The Incident: if the collector already made one for the OOMKilled `web` container
(`kubectl -n shadewright-system get incidents`), use its name and UID in the request. An OOMKilled container opens
its Incident at the first kill (as does a KEDA ceiling); other Warning events (a scaler or an HPA while Prometheus starts) open one only when they
are seen again 2 to 15 minutes later, or arrive with a series count of 2 or more, so a settled baseline shows no other Incident
([design 3.2](design/3-observation-and-deduplication.md#when-a-symptom-opens-an-incident)). Otherwise write the sample:

```bash
UID_WEB=$(kubectl -n demo get deployment web -o jsonpath='{.metadata.uid}')
# put $UID_WEB in remediation_v1alpha1_incident.yaml (subject.uid)
# and remediation_v1alpha1_remediationrequest.yaml (targetRef.uid)
kubectl apply -f /tmp/shadewright-lab/remediation_v1alpha1_incident.yaml
INC_UID=$(kubectl -n shadewright-system get incident web-oomkilled -o jsonpath='{.metadata.uid}')
# put $INC_UID in remediation_v1alpha1_remediationrequest.yaml (incidentRef.uid)
```

Then the request:

```bash
kubectl apply -f /tmp/shadewright-lab/remediation_v1alpha1_remediationrequest.yaml
kubectl -n shadewright-system get remediationrequest web-memory-001 -w
```

Expect the phases `Pending`, `PolicyChecked`, `PRCreated`, then `AwaitingApproval`, in well under a minute. A
watch prints a line only when the object changes and the controller can move through a phase between two reads, so
it may not show every phase (`Pending` in particular); `AwaitingApproval` at the end is what counts. Any
other end (`Denied`, `NeedsIntervention`) names its reason in the conditions:

```bash
kubectl -n shadewright-system get remediationrequest web-memory-001 -o jsonpath='{range .status.conditions[*]}{.type}={.status} {.reason}: {.message}{"\n"}{end}'
```

`rr` is the short name of `remediationrequest`. Do not delete the request until step 10.

Repeating: applying the settings, the policy, the Incident and the request again is safe (the request's spec is
immutable, so an edited file is refused, not applied). The reads (`get`, `gh pr list`, `gh pr view`) can be run any
number of times. A request does not retry after it ends in `Denied` or `NeedsIntervention`: fix the cause and apply
a request with a new name. Steps 1, 2, 5 and 9 change the cloud, a branch or the policy, so run each once per session; step 9 in particular ends the request.

## 7. Check that there is exactly one pull request, and that it is correct

```bash
kubectl -n shadewright-system get rr web-memory-001 -o jsonpath='{.status.pullRequest.url}{"\n"}{.status.pullRequest.headSHA}{"\n"}'
gh pr list --repo <owner>/shadewright-fleet --state all --json number,title,headRefName,baseRefName,state
```

Expect exactly one pull request:

- Head branch `shadewright/<request UID>`, base `cluster/lab`, state open, title
  `fix: set the memory limit of container web of demo/web to 64Mi`.
- `headRefName` equal to `shadewright/` plus `kubectl -n shadewright-system get rr web-memory-001 -o jsonpath='{.metadata.uid}'`.

```bash
gh pr diff <number> --repo <owner>/shadewright-fleet
gh pr view <number> --repo <owner>/shadewright-fleet --json body,files,commits --jq '{files: [.files[].path], commits: (.commits|length)}'
gh pr view <number> --repo <owner>/shadewright-fleet --json body --jq .body
```

Expect:

- One file, `apps/demo/web-deployment.yaml`, one commit, and a diff of exactly one line pair:
  `-              memory: 48Mi` and `+              memory: 64Mi` under `resources.limits` of container `web`. The memory
  request (`32Mi`) and everything else are untouched.
- A body that names the incident by its full name, `incident-` and 64 hex digits (not `[REDACTED:token]`), in its
  opening sentence and in the evidence. The executor shows the name only after reading that Incident back, so a
  redacted name means the read failed (the executor's log says so) or the request names no such Incident. The
  body also holds the change, scope, evidence (the request, its UID, the incident, the policy revision
  `<policy name>/<generation>`, the plan digest), expected effect, risk, rollback and `Approval required: yes`,
  and a last line `<!-- shadewright request-uid=... plan-digest=sha256:... -->` whose UID and digest match
  `.metadata.uid` and `.status.planDigest` of the request.
- The required checks run on the pull request, and it is not merged: a person reviews it. Do not merge it before
  step 9 (merge, delivery and verification are out of scope for v0.1.0).

The policy revision in the body is `.status.policyRevision` of the request, which is the policy name and its
generation: `demo-memory/1` for a policy applied once and never edited, a higher number otherwise (a reapply with
changes bumps it). Read it from the run instead of assuming it:

```bash
kubectl -n shadewright-system get rr web-memory-001 -o jsonpath='{.status.policyRevision}{"\n"}'
kubectl get remediationpolicy demo-memory -o jsonpath='demo-memory/{.metadata.generation}{"\n"}'
```

Expect the two lines to agree, and the body to carry the same revision.

Also check that the head SHA in the request status equals the pull request's head
(`gh pr view <number> --repo <owner>/shadewright-fleet --json headRefOid`).

## 8. Apply the request again and see nothing new

```bash
kubectl apply -f /tmp/shadewright-lab/remediation_v1alpha1_remediationrequest.yaml
kubectl -n shadewright-system rollout restart deployment/shadewright-executor
kubectl -n shadewright-system rollout status deployment/shadewright-executor
gh pr list --repo <owner>/shadewright-fleet --state all --json number,headRefName
gh api repos/<owner>/shadewright-fleet/branches --paginate --jq '.[].name'
```

Expect `kubectl apply` to report `unchanged` (the spec is immutable, so a changed file would be refused, not
applied). After the restart the executor finds the existing branch and pull request through the branch name and the
marker (invariant 5), so the list still shows the one pull request, the same head SHA, and one `shadewright/` branch.
The request is back in `AwaitingApproval`.

## 9. Change the policy bounds and see `Superseded`

Narrow the bounds so that the plan's digest changes (the policy's generation is part of it):

```bash
kubectl patch remediationpolicy demo-memory --type json \
  -p '[{"op":"replace","path":"/spec/actions/0/setContainerMemoryLimit/max","value":"96Mi"}]'
kubectl -n shadewright-system get rr web-memory-001 -w
```

While the pull request waits, the plan is checked at most five minutes after the last full recheck, so allow up to
five minutes. A nudge (`kubectl -n shadewright-system annotate rr web-memory-001 touched=$(date +%s) --overwrite`)
only makes the executor look at the pull request; it does not bring the plan check forward.
Expect the phase `Superseded`, the condition `PlanFresh=False` with the reason and message naming the policy, and
the pull request closed, not merged, with its branch left in place:

```bash
gh pr view <number> --repo <owner>/shadewright-fleet --json state,closedAt,mergedAt
kubectl -n shadewright-system get rr web-memory-001 -o jsonpath='{.status.phase}{"\n"}'
```

Expect `state` `CLOSED` and `mergedAt` null. A new plan is a new request (a new name, same sample).

## 10. Check the App's permissions against ADR-0006

ADR-0006 lists exactly four: `contents: write`, `pull_requests: write`, `issues: write`, `metadata: read`, on the
fleet repository only, with no webhook and never on a ruleset bypass list. In `shadewright-azure`:

```bash
KV=$(terraform -chdir=foundation output -json key_vault | jq -r .name)
scripts/github-app-check.sh "$KV" <app-client-id>
```

Expect the App's name, the permissions above and no others, no events, and one installation with
`repository_selection` `selected`. The script builds the App's JWT with the key in Key Vault, which `gh` cannot do
for you, because `/app` endpoints do not accept a user token. If you export that JWT as `JWT`, the same data with `gh`:

```bash
gh api /app/installations/<installation-id> -H "Authorization: Bearer $JWT" \
  --jq '{permissions, events, repository_selection, suspended_at}'
```

The rulesets of the fleet repository, as you:

```bash
gh api repos/<owner>/shadewright-fleet/rulesets --jq '.[] | {id, name, target, enforcement}'
gh api repos/<owner>/shadewright-fleet/rulesets/<id> --jq '{name, conditions, bypass_actors}'
```

Expect: none of the rulesets (for `main` and for `cluster/*`)
listing the App among `bypass_actors`, and the maintainer's role as the only bypass actor of the two pull request
rulesets. Write down any difference; it is a finding, not something to fix inside this run.

## 11. Record the run and tear down

Before the lab is destroyed, collect the facts for the field log:

```bash
kubectl -n shadewright-system get rr web-memory-001 -o yaml > /tmp/shadewright-lab/rr-final.yaml
```

Remove the personal values from anything you quote. In the field log of the day, record: the date and times (lab
up, scenario on, pull request opened, `Superseded`, lab down), the cost shown by `make lab-up`, the pull request
number and head SHA, the one-line diff, the phase sequence of the request, the result of step 8 (one pull request
before and after), the observed time to `Superseded`, the permissions found in step 10 and every deviation. The log
links public commits and pull requests by URL.

Then reset the scenario and destroy the lab. In `shadewright-fleet`:

```bash
make scenario-reset CLUSTER=lab
```

Expect one commit on `cluster/lab` with `main`'s tree. Close the pull request of the run if it is still open
(`gh pr close <number> --repo <owner>/shadewright-fleet`), which leaves its branch; delete the branch
`shadewright/<uid>` by hand if you want it gone. In `shadewright-azure`:

```bash
make lab-down    # plan of the destroy, asks; applies on "yes", then lab-check
```

Expect a destroy plan for the `lab` root only and, after `yes`, a passing `lab-check`: nothing of the lab left, and
no hourly-billed resource outside it. Remove `/tmp/shadewright-lab`, which holds your filled samples.

Issue [#58](https://github.com/pkalin/shadewright/issues/58) closes once the field log holds the recorded run.

## Evaluation run: the first analysis with a real model

Optional, in a lab that is up. A real model reads a real context of the OOM scenario's Incident
([#76](https://github.com/pkalin/shadewright/issues/76)). It is an **evaluation run, not production use**: the
evaluation harness and its fail-closed check ([#78](https://github.com/pkalin/shadewright/issues/78)) are not built,
and they are the gate before any release that starts the loop
([ADR-0025](adr/0025-analysis-claims-units-and-evaluation-manifest.md), decision 7). A proposal it makes is a
`RemediationRequest` with `approvalRequired: true`, which the executor checks against policy like a hand-written one;
do not approve it as a fix.

Since shadewright-fleet [#37](https://github.com/pkalin/shadewright-fleet/pull/37) and
[#38](https://github.com/pkalin/shadewright-fleet/pull/38), the fleet's `main` turns it on with nothing applied by hand:

- `infrastructure/shadewright-lab` sets `analyzer.analysis` in the HelmRelease values: `triageModel` (the model of the
  foundation deployment `triage`, as its replies name it), `endpointSuffixes` limited to the lab's own AI Services
  host (`${MODEL_ENDPOINT_SUFFIX}`) and `actionPolicies` (`SetContainerMemoryLimit: demo-memory`).
- `infrastructure/shadewright-lab-config` holds the `RemediationSettings` (`maxAnalysesPerDay: 10`, deployment
  `triage`, `maxTokensPerDay: 200000`) and the `demo-memory` policy, applied after the chart. The repository, branch,
  App IDs, Key Vault key and model endpoint are Flux substitutions from shadewright-azure `lab/flux.tf`, which needs
  `github_app_client_id` and `github_app_installation_id` in its git-ignored `lab/terraform.tfvars`.

1. Bring the lab up (`make lab-up` in shadewright-azure), then reset `cluster/lab` to `main`
   (`make scenario-reset CLUSTER=lab` in the fleet). Check that Flux applied everything and the analyzer started the loop:

   ```bash
   kubectl -n flux-system get kustomizations          # lab, infrastructure, apps, shadewright, shadewright-config Ready
   kubectl get remediationsettings,remediationpolicies
   kubectl -n shadewright-system logs deploy/shadewright-analyzer | grep -E 'The analysis is (on|off)'
   ```

   The settings arrive minutes after the cluster starts, so any baseline Incident already open is analyzed as soon as
   they do, and each takes one of the day's analyses ([#423](https://github.com/pkalin/shadewright/issues/423)).

2. Switch the OOM scenario on (`make scenario-oom CLUSTER=lab`) and wait for its Incident to be analyzed:

   ```bash
   kubectl -n shadewright-system get incidents -w
   kubectl -n shadewright-system logs deploy/shadewright-analyzer | grep -E 'Analyzed Incident|Refused|Dropped'
   ```

   Expect the Incident to go `Analyzing` and then `Proposed` (its `status.remediationRequests` names the request,
   which carries the label `remediation.shadewright.io/incident-uid`) or `Escalated`, with the condition `Analyzed`
   and its reason. `UnexpectedModel` means the reply named another model than `triageModel`: the `Refused` log line
   names both. `EndpointNotAllowed`, `ModelForbidden` or `ModelNotFound` point at the endpoint, the role assignment or
   the deployment name.

3. Record the outcome for the field log: the reason, the call and token counts, the models, the prompt version, the
   summary and, for a `MissingData` hand-off, the `missingData` list from the `Analyzed Incident` line, and the
   request's spec if one was made. A wrong or unsupported proposal is a
   finding for #78, not something to fix in the run. The summary and `missingData` are in the log from
   [#422](https://github.com/pkalin/shadewright/issues/422) on. With the record store set
   (`analyzer.analysis.recordStore.azureBlob.account`, the `analysis_store` output of `foundation`), the same
   fields are in the record named by the Incident's `status.analysisRecords`, in the `analyses` container
   ([#77](https://github.com/pkalin/shadewright/issues/77)); a record not written is logged (such as `Failed to write the
   analysis record`), counted in `shadewright_analysis_records_total` and reported with a Warning event
   `AnalysisRecordNotWritten` on the Incident, and changes nothing else.

4. To stop the analysis in a lab that stays up, set `maxAnalysesPerDay: 0` in
   `infrastructure/shadewright-lab-config/remediationsettings.yaml` on `cluster/lab`. Otherwise `make lab-down`.

First run, 2026-11-06 (v0.3.0-rc.1, `gpt-5.4-mini-2026-03-17`): the OOM Incident was analyzed 2 min 21 s after it
was created and handed off with `MissingData` (one call, no tool calls, 2,610 tokens); see the fleet's
`scenarios/oom/README.md`.
