---
title: Limit the namespaces it watches
---

# Limit the namespaces it watches

The analyzer reads events and pods only in the namespaces you name. An empty list watches none, never all. Background: [data handling](../data-handling.md#what-it-reads) and [design 3.1](../design.md#31-collecting). The flag is in the [configuration reference](../reference/configuration.md#analyzer-only).

## With the Helm chart

List the namespaces. Each must exist; the chart gives each a read-only RoleBinding.

```bash
helm upgrade shadewright oci://ghcr.io/pkalin/charts/shadewright --version <version> \
  --reuse-values --set 'analyzer.watchNamespaces={team-a,team-b}'
```

## With the kustomize install

Set `--watch-namespaces=team-a,team-b` in the analyzer's container args, and for each namespace add a RoleBinding that binds the ClusterRole `shadewright-analyzer-watch-role` to the ServiceAccount `shadewright-analyzer` of the operator namespace (`config/watch` is the example). Without the binding the API server refuses the analyzer's reads there and nothing is observed.

## Check it

```bash
kubectl -n shadewright-system logs deploy/shadewright-analyzer | grep "Watching"
```

It lists the namespaces, or says `Watching no namespaces`. The gauge `shadewright_analyzer_watched_namespaces` shows the count. If nothing appears, see [no Incident appears](../troubleshooting.md#no-incident-appears).

## What this does not limit

Watched namespaces are separate from target namespaces. What a request may change is set by the [policy](write-a-policy.md) (`targets.namespaces`).

!!! note "Not yet run"
    The `helm upgrade` command is not yet run for this guide (the release name and chart location are the ones used in [install](../install.md); adjust to your install). The log line and the gauge are described in the troubleshooting page and the metrics reference.
