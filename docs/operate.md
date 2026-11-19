---
title: Operator manual
---

# Operator manual

This manual is for the team that installs and runs Shadewright: configuring it, setting the policies it works within, and the tasks that come after, such as upgrades, key rotation and stopping it. Reviewing proposals is in the [user guide](use.md); the settings themselves are in the [configuration reference](reference/configuration.md). To install, begin with [install in observe-only mode](install.md).

In the order the work comes:

- [Check an install with kubectl](how-to/check-install.md), then [limit the namespaces it watches](how-to/limit-watched-namespaces.md).
- [Write or tighten a policy](how-to/write-a-policy.md) and [turn reviewer hints on](how-to/turn-on-reviewer-hints.md).
- [Pause and resume writes](how-to/pause-and-resume.md) and the [emergency stop](emergency.md): what a pause does not undo.
- [Clear Resolved Incidents at the cap](how-to/clear-resolved-incidents.md).
- [Upgrade and uninstall](upgrade-uninstall.md): the order, open pull requests and finalizers, and how to verify a release.
- [Identity rotation](identity-rotation.md): the GitHub App key, the installation, federated credentials and Key Vault key versions.
- [Data handling](data-handling.md): what is stored, sent to the model, published and retained.
- [Qualify compatible inference](how-to/qualify-openai-compatible.md) and [evaluate the model](evaluation.md).
- [Lab runbook: the OOM scenario](lab-oom-acceptance.md) and [reviewer hint on OOM](lab-oom-hint.md): acceptance runs in the reference lab in Azure.
- [Troubleshooting](troubleshooting.md): every condition reason with its cause and fix.
- [Metrics](reference/metrics.md): what the three components export.

## Health probes

Each of the three components serves two checks on port 8081, from the node, with no authentication and no cluster data in the answer. The probe server starts with the manager, after the component has read its flags and mapped the kinds it caches; until then a probe gets `connection refused`, not an HTTP error.

- `/healthz` answers as soon as the probe server runs. The liveness probe asks it, so a slow or unreachable API server never gets a running Pod restarted.
- `/readyz` (check `cache-sync`) passes once the informers of the kinds the component cannot work without have done their first list and watch. The startup probe and the readiness probe ask it. Once it has passed it keeps passing for the life of the container: it reports that the component was able to start, not that the API server answers now. A later outage of the API server therefore does not take every replica out of rotation; nothing serves traffic through readiness here, and the component's own retries cover the outage.

The kinds each component waits for:

| Component | Kinds |
| --- | --- |
| executor | RemediationRequests in the operator namespace |
| analyzer | Incidents and RemediationRequests in the operator namespace, RemediationSettings, and, with at least one watched namespace, `events.k8s.io` Events and Pods in the watched namespaces |
| verifier | RemediationRequests in the operator namespace |

These are the kinds each component may list and watch through its own grants; a kind it may only `get` (the executor's Incidents and VerificationReports, the verifier's Pods, which it may only `list`) has no informer and is not waited for. The add-on kinds of the analyzer's modules (Flux, KEDA, Kyverno and the others) are never waited for: a module whose CRD is not installed or whose grant is off does not keep a Pod not Ready, and shows in the settings status instead ([troubleshooting](troubleshooting.md#no-incident-appears)). Neither are HorizontalPodAutoscalers nor Nodes.

The check does not depend on the leader election Lease. Every replica opens the same informers the leader's controllers use, so a standby turns Ready once it has synced them, whether or not it ever leads.

The startup probe asks every 2 seconds and allows 150 failures, so a component has five minutes to sync before the kubelet restarts the container; a component that cannot list one of its kinds (forbidden, or the API server unreachable) keeps retrying and stays not Ready until then. Liveness (every 20 seconds) and readiness (every 10 seconds) start only after the startup probe has passed, with a 2 second timeout and 3 failures each. These values are fixed in the chart and in `config/manager`. A Pod that stays not Ready: see [troubleshooting](troubleshooting.md#a-component-pod-stays-not-ready).
