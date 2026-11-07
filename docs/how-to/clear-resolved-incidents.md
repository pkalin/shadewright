---
title: Clear Resolved Incidents at the cap
---

# Clear Resolved Incidents at the cap

The analyzer keeps at most 500 Incidents (`--max-incidents`) and counts every one, `Resolved` ones included. At the cap a new fingerprint is refused. The analyzer has no `delete` on Incidents, so an admin of the operator namespace removes the ones that are safe to delete. Background: [design 3.2](../design/3-observation-and-deduplication.md#32-two-levels-of-deduplication), [ADR-0007](../adr/0007-incident-state-in-crds.md).

<span class="sw-status sw-built">built</span> The analyzer moves an Incident to `Resolved`, and writes `status.resolvedTime` in the same update, when nothing happened to it for an hour and no request naming it is active, or when its request `Succeeded` ([design 3.2](../design/3-observation-and-deduplication.md#incident-lifecycle)); the condition `Lifecycle` says which. Keep the 24 hour safety window described below. Automatic retention with an archive is <span class="sw-status sw-planned">planned</span> ([#458](https://github.com/pkalin/shadewright/issues/458)); until then the steps are manual.

## Know that the cap is near

The gauge `shadewright_incidents` reports Incidents by `phase` <span class="sw-status sw-built">built</span>. At 80% of the cap (400 of 500) the gauge `shadewright_incident_cap_near` turns 1 and the analyzer's Pod gets a Warning event `IncidentCapNear` <span class="sw-status sw-built">built</span>; alert on `max(shadewright_incident_cap_near) == 1` ([metrics reference](../reference/metrics.md)). At the cap, `shadewright_incident_observations_refused_total` rises and the analyzer's Pod gets a Warning event `IncidentCapReached`.

## Steps

1. **Pick.** List `Resolved` Incidents:

   ```bash
   kubectl -n shadewright-system get incidents -o json \
     | jq -r '.items[] | select(.status.phase=="Resolved") | .metadata.name'
   ```

2. **Keep the last 24 hours.** Skip Incidents resolved less than 24 hours ago: their recurrence must still be escalated, not analyzed as a new occurrence. The analyzer writes `status.resolvedTime` in the same status update that sets `Resolved`. This filter lists only Incidents resolved at least 24 hours ago; missing or unreadable times are skipped:

   ```bash
   kubectl -n shadewright-system get incidents -o json \
     | jq -r 'now as $now | .items[]
       | select(.status.phase=="Resolved")
       | (try (.status.resolvedTime | fromdateiso8601) catch null) as $resolved
       | select($resolved != null)
       | select($resolved <= ($now - 86400))
       | .metadata.name'
   ```

   An Incident set `Resolved` by hand, or by an analyzer older than the closure rules, may have no `resolvedTime`: wait 24 hours while it remains `Resolved` before deleting it. Do not use `lastSeen` as the resolution time.

3. **Skip the ones an active request names.** Keep every Incident named by a request that has not reached a final phase (`Succeeded`, `Denied`, `Failed`, `Expired`, `Cancelled`, `Superseded`, `NeedsIntervention`):

   ```bash
   kubectl -n shadewright-system get remediationrequests -o json \
     | jq -r '.items[] | [.spec.incidentRef.name, .status.phase] | @tsv'
   ```

4. **Export.** Keep the evidence with your incident records. An Incident holds the redacted symptom, counters and times, not logs:

   ```bash
   kubectl -n shadewright-system get incident <name> -o yaml > <name>.yaml
   ```

5. **Delete.**

   ```bash
   kubectl -n shadewright-system delete incident <name>
   ```

6. **Restart the analyzer.** It counts Incidents, and finds the latest occurrence of each fingerprint (`incident-<64 hex>-<n>`), only when it starts; deleting an earlier occurrence does not make it open that one again. Deleting the latest occurrence of a fingerprint makes the one before it the latest again: if that one is `Resolved`, the next recurrence more than 24 hours after its `resolvedTime` opens a new occurrence under the deleted name; within 24 hours it escalates that earlier Incident:

   ```bash
   kubectl -n shadewright-system rollout restart deploy/shadewright-analyzer
   ```

7. **Check.** `shadewright_incidents` fell, and `shadewright_incident_observations_refused_total` stopped rising.

If the cap fills again at once, the namespaces produce many fingerprints: see [Limit the namespaces it watches](limit-watched-namespaces.md).

!!! note "Not yet run"
    The commands were moved here from the [troubleshooting](../troubleshooting.md#the-incident-cap-is-reached) entry and were not run again against a live cluster for this guide.
