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

Each of the three components serves two checks on port 8081, from the node, with no authentication and no cluster data in the answer:

- `/healthz` answers as soon as the process runs. The liveness probe asks it, so a slow or unreachable API server never gets a Pod restarted.
- `/readyz` (check `cache-sync`) passes once the component's caches have synced, and keeps passing. The startup probe and the readiness probe ask it. It does not depend on the leader election Lease: controller-runtime syncs the caches before it runs leader election, so a standby replica turns Ready as soon as the leader does.

The startup probe asks every 2 seconds and allows 150 failures, so a component has five minutes to sync its caches before the kubelet restarts it. Liveness (every 20 seconds) and readiness (every 10 seconds) start only after the startup probe has passed, with a 2 second timeout and 3 failures each. These values are fixed in the chart and in `config/manager`. A Pod that stays not Ready: see [troubleshooting](troubleshooting.md#a-component-pod-stays-not-ready).
