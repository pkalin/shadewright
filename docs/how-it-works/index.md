---
title: How it works
---

# How it works

Shadewright watches a Kubernetes cluster for symptoms, works out a fix, and proposes it as a pull request to your GitOps repository. A person reviews and merges it, your GitOps controller delivers it, and Shadewright checks that the symptom went away. This section follows one symptom from start to end in seven short pages.

[![How Shadewright works: five steps from a symptom to a verified change, in three identities, with the systems it talks to.](../assets/diagrams/how-it-works.svg)](../assets/diagrams/how-it-works.svg "Open full size")

## The five steps

1. **Observe.** The analyzer reads Warning events and pod status in the namespaces you name. Repeated symptoms of one workload become one `Incident`, a small object in the cluster. [Incidents](incidents.md).
2. **Diagnose.** Rules first, then a model that sees redacted input and read-only tools. It can only write a typed proposal, a `RemediationRequest`. [Diagnosis](diagnosis.md).
3. **Check.** The executor compares the proposal with a fixed list of allowed changes and with a policy your cluster admin wrote. Plain code decides, not the model. [Policy](policy.md).
4. **Open a pull request.** The executor finds the file that defines the workload, changes one value and opens a pull request. Your branch rules and your reviewers decide whether it merges. [The pull request](pull-request.md).
5. **Verify.** After the merge and the rollout, a separate component reads the pods and the executor ends the request as succeeded, failed or inconclusive. [Verification](verification.md).

## What never happens

- Shadewright does not change a workload directly. It writes only its own objects: Incidents, requests and verification reports with their status, the analysis counter, the platform detection in the settings status, leader election leases and Events. Outside the cluster it writes analysis records to a blob container when you configure one, and the pull request.
- It does not merge. Automatic merging is <span class="sw-status sw-planned">planned</span> and not built: today a person merges every pull request.
- It does not run an action outside the fixed list, and it does not run shell commands.
- It does not treat a model's answer as approval. The model's output is a proposal, nothing more.

How these guarantees are kept, and by whom, is on the [safety model](safety.md) page.

## What is built today

<span class="sw-status sw-built">built</span> The whole path from an Incident to a pull request that waits for a person, and the checks after the merge.

Two parts are off by default. The model call starts only when the analyzer runs with `--triage-model`, and until its evaluation check exists it is meant for evaluation runs in a lab, so in a normal install a person (or a test) writes the request. The steps after the merge run only with `--verify-outcomes`.

Two things are <span class="sw-status sw-planned">planned</span>: runbooks, which answer a known problem without a model, and automatic merging.

The list of changes has two entries today: a container's memory limit and the maximum replica count of a KEDA `ScaledObject`.

## Try it without risk

The recommended first install is observe-only: the analyzer records Incidents and nothing can write to Git. See [install in observe-only mode](../install.md).

## In depth

- [Architecture](../design/2-architecture.md#21-flow): the flow, the components and what each may read and write.
- [The life of a request](../request-lifecycle.md): every phase a request passes through.
- [ADR-0003](../adr/0003-gitops-only-typed-remediation.md): why changes go through Git as typed actions.
- [What it is and is not](../overview.md).
