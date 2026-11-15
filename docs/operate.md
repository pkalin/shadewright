---
title: Operate
---

# Operate

What keeping Shadewright running takes: moving to a new release, stopping it, rotating its identities, knowing what it stores, and trying it in the reference lab. The settings themselves are in the [configuration reference](reference/configuration.md).

- [Upgrade and uninstall](upgrade-uninstall.md): the order, open pull requests and finalizers, and how to verify a release.
- [Emergency stop](emergency.md): pause the executor and stop the analysis, and what a pause does not undo.
- [Identity rotation](identity-rotation.md): the GitHub App key, the installation, federated credentials and Key Vault key versions.
- [Data handling](data-handling.md): what is stored, sent to the model, published and retained.
- [Metrics](reference/metrics.md): what the three components export.
- [Lab runbook: the OOM scenario](lab-oom-acceptance.md): the acceptance run in the reference lab in Azure.

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
