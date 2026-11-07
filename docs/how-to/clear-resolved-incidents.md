---
title: Clear Resolved Incidents at the cap
---

# Clear Resolved Incidents at the cap

The analyzer keeps at most 500 Incidents (`--max-incidents`) and counts every one, `Resolved` ones included. At the cap a new fingerprint is refused. The analyzer has no `delete` on Incidents, so an admin of the operator namespace removes the ones that are safe to delete. Background: [design 3.2](../design/3-observation-and-deduplication.md#32-two-levels-of-deduplication), [ADR-0007](../adr/0007-incident-state-in-crds.md).

<span class="sw-status sw-planned">planned</span> The phase `Resolved` is not set by anything yet ([#251](https://github.com/pkalin/shadewright/issues/251)). Until it is built, pick Incidents by `status.lastSeen` instead. Automatic retention with an archive is also planned ([#458](https://github.com/pkalin/shadewright/issues/458)); until then the steps are manual.

## Know that the cap is near

The gauge `shadewright_incidents` reports Incidents by `phase` <span class="sw-status sw-built">built</span>. An alert when their sum passes 80% of the cap (400) is <span class="sw-status sw-planned">planned</span>. At the cap, `shadewright_incident_observations_refused_total` rises and the analyzer's Pod gets a Warning event `IncidentCapReached`.

## Steps

1. **Pick.** List `Resolved` Incidents:

   ```bash
   kubectl -n shadewright-system get incidents -o json \
     | jq -r '.items[] | select(.status.phase=="Resolved") | .metadata.name'
   ```

2. **Skip the ones an active request names.** Keep every Incident named by a request that has not reached a final phase (`Succeeded`, `Denied`, `Failed`, `Expired`, `Cancelled`, `Superseded`, `NeedsIntervention`):

   ```bash
   kubectl -n shadewright-system get remediationrequests -o json \
     | jq -r '.items[] | [.spec.incidentRef.name, .status.phase] | @tsv'
   ```

3. **Export.** Keep the evidence with your incident records. An Incident holds the redacted symptom, counters and times, not logs:

   ```bash
   kubectl -n shadewright-system get incident <name> -o yaml > <name>.yaml
   ```

4. **Delete.**

   ```bash
   kubectl -n shadewright-system delete incident <name>
   ```

5. **Restart the analyzer.** It counts Incidents only when it starts:

   ```bash
   kubectl -n shadewright-system rollout restart deploy/shadewright-analyzer
   ```

6. **Check.** `shadewright_incidents` fell, and `shadewright_incident_observations_refused_total` stopped rising.

If the cap fills again at once, the namespaces produce many fingerprints: see [Limit the namespaces it watches](limit-watched-namespaces.md).

!!! note "Not yet run"
    The commands come from the [troubleshooting](../troubleshooting.md#the-incident-cap-is-reached) entry and were not run again against a live cluster for this guide.
