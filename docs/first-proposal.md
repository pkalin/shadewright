---
title: Your first proposal, to a reviewed pull request
---

# Your first proposal, to a reviewed pull request

Take a hand-written request through the executor to a pull request that a person reviews. It needs the observe-only install of [the previous step](install.md) and, outside the lab, a GitOps repository delivered by Flux. A request that stops is explained in [troubleshooting](troubleshooting.md).

What is built and what is not is on the [Get started](getting-started.md) page: read its box first.

## From a hand-written request to a reviewed pull request

This is the path that changes something outside the cluster, so read it as a checklist and then follow a runbook. The complete, tested walk-through is the [lab runbook](lab-oom-acceptance.md): it brings up the reference lab in Azure, applies the samples in `config/samples/lab/`, and checks the pull request, the idempotent re-apply and the `Superseded` end.

What a real run needs, all of it checked by the code:

| Need | Why | Where it is checked |
| --- | --- | --- |
| Flux delivers the target: a `Kustomization` whose `status.inventory` lists it, from a `GitRepository` that tracks one branch, with plain manifests | The executor maps the live object to the one file that declares it ([ADR-0012](adr/0012-object-to-source-mapping.md)) and refuses anything else | Source mapping, reasons in [troubleshooting](troubleshooting.md#delivery-from-flux) |
| A `RemediationSettings` named `default` with one repository entry: its URL equal to the `GitRepository` URL, `targetBranch` equal to the branch Flux tracks, GitHub App client ID, installation ID and the Key Vault `keyID` | The model and the request never choose a URL or credentials | Policy check |
| A `RemediationPolicy` that allows the action in the target's namespace, with bounds, the repository by name and `allowedPaths` | Deterministic policy decides, with bounds, what may run | Policy check |
| A GitHub App installed on that repository only, with the five permissions of [ADR-0006](adr/0006-github-app-key-in-key-vault.md) | Branch, commit and pull request are made as the App | First write (`GitForbidden`) |
| The App's private key in Azure Key Vault as a non-exportable key, and a managed identity that may only `sign` with it, federated to the executor ServiceAccount (workload identity on AKS) | The key never enters the cluster. Key Vault is the only signer built | First Git call |
| The request in the operator namespace, with UIDs of the Incident and of the target, `previous` equal to the live value, `desired` inside the policy | A request is immutable, and a changed plan is a new request | Policy check |

The samples show the shape of each object:

```bash
kubectl apply -f config/samples/lab/remediation_v1alpha1_remediationsettings.yaml   # after filling the REPLACE values
kubectl apply -f config/samples/lab/remediation_v1alpha1_remediationpolicy.yaml
kubectl apply -f config/samples/lab/remediation_v1alpha1_remediationrequest.yaml
kubectl -n shadewright-system get remediationrequest -w
```

Expect the phases `Pending`, `PolicyChecked`, `PRCreated`, then `AwaitingApproval`, with one pull request on the branch `shadewright/<request UID>` against the target branch. Read the pull request like any other, and merge it yourself: with `approval: Required` (and also when a policy would allow otherwise, because auto-merge is not built) a person decides. After a merge the request ends `Merged`; delivery tracking and verification are **planned**. Any other end is `Denied`, `NeedsIntervention`, `Expired` or `Superseded` with the reason in the conditions: look it up in [troubleshooting](troubleshooting.md). To stop everything quickly, see the [emergency stop](emergency.md).
