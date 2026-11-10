---
title: Identity rotation
---

# Identity rotation

Four things identify Shadewright to the outside, and each is rotated on its own: the GitHub App's private key, the App's installation, the federated credentials of the workload identities, and the Key Vault key versions. Today only the executor has an outside identity that matters (the analyzer's model identity is not built yet, [ADR-0023](adr/0023-how-the-analyzer-calls-the-model.md)). The setup is in the `shadewright-azure` README ("GitHub App") and [ADR-0006](adr/0006-github-app-key-in-key-vault.md); this page is only about changing it afterwards.

!!! warning "What is verified"
    The Shadewright side below is read from the code (`internal/providers/registry`, `internal/providers/github/auth`, `internal/policy/freshness`). The Key Vault and GitHub behaviour (a new key version becoming the current one, several keys on one App) is how those services document themselves; it has **not** been exercised in the lab. Do the first rotation in a lab or with `scripts/github-app-check.sh` before relying on it.

## How the identity is used

For each repository in `RemediationSettings` the executor builds a provider the first time it needs it and keeps it. The provider signs a GitHub App JWT (RS256, valid 10 minutes) with the Key Vault key and exchanges it for an installation token scoped to that one repository, with the permissions of ADR-0006 only. The token is cached and replaced five minutes before it expires (GitHub tokens last an hour). The private key never leaves Key Vault. A provider is dropped from the executor when its entry leaves the settings, and a changed entry is a different provider.

## 1. The GitHub App private key

Rotate it on a schedule you choose, and at once if the key may have leaked. Key material cannot leak from the cluster (nothing there holds it), so the usual cause is the `.pem` file that GitHub hands out once.

1. On the App's page generate a **new** private key. The App now has two valid keys.
2. Import it into Key Vault under the **same key name**, then delete the file:

   ```bash
   KV=$(terraform -chdir=foundation output -json key_vault | jq -r .name)
   KEY=$(terraform -chdir=foundation output -json key_vault | jq -r .github_app_key)
   az keyvault key import --vault-name "$KV" --name "$KEY" --pem-file ~/Downloads/<app>.private-key.pem \
     --ops sign --protection software --exportable false
   rm ~/Downloads/<app>.private-key.pem
   ```

   Importing under an existing name adds a **version** to that key. The executor's role assignment is scoped to the key, not to a version, so it covers the new version without a Terraform change.
3. Check the whole path as yourself: `scripts/github-app-check.sh "$KV" <client-id>` in `shadewright-azure`. Expect the App's name, its permissions and one installation.
4. Let Shadewright pick it up (next section), then **delete the old key** on the App's page. Until you do, a leaked old key still works.
5. Disable the old Key Vault version (`az keyvault key set-attributes --vault-name "$KV" --name "$KEY" --version <old> --enabled false`) after a day without errors. Keep it disabled rather than deleted at first; purge protection and a 7-day soft delete are on for the vault in the lab.

### Which version is signed with

The `keyID` in `spec.repositories[].github.signer.azureKeyVault.keyID` has two forms, both accepted by the CRD:

| Form | Signs with | To rotate |
| --- | --- | --- |
| `https://<vault>.vault.azure.net/keys/<name>` | The key's current version, chosen by Key Vault at each call | Import the new version. The next JWT (at most an hour later, when the cached installation token is replaced) is signed with it. No change to the settings, no restart |
| `https://<vault>.vault.azure.net/keys/<name>/<32 hex digits>` | That version only | Import, then change `keyID` to the new version. The executor builds a new provider for the changed entry on the next request that needs Git |

The lab uses the first form. The second is the one to choose if you want a rotation to be an explicit, reviewed change in Git. Changing `keyID` does **not** make a waiting request stale: the freshness check compares the repository URL, the installation and the branch, not the signer. A request mid-way simply continues with the new provider.

In the lab, every signing is recorded in the vault's `AuditEvent` log (operation `KeySign`), which goes to a Log Analytics workspace kept 90 days. Use it to see that the executor signs after a rotation.

## 2. The GitHub App installation

The installation ID (`installationID`) is part of the repository's identity in a plan. Reasons to change it: the App was uninstalled and installed again (a new ID), or moved to another account.

1. **First settle the waiting requests.** The installation is part of a plan's identity (the plan digest covers it, and the executor finds a plan's repository by URL and installation). Delete the requests that wait for review while the old installation still works, so each closes its pull request, and wait until they are gone ([upgrade and uninstall](upgrade-uninstall.md#what-the-finalizer-does)).
2. Install the App again on **the one repository** (selected repositories, not all) and read the new ID (`scripts/github-app-check.sh` prints the installations).
3. Change `installationID` in the settings entry.
4. If requests were still waiting when you changed it, expect them to end `NeedsIntervention` with `RepositoryUnavailable`: the plan no longer matches, and the repository it was made for is not in the settings any more, so Shadewright closes nothing. Their pull requests stay open and are yours to close by hand. Deleting such a request does not block, and does not close the pull request either. Apply new requests afterwards.
5. An installation that is uninstalled or suspended before you change anything shows as `GitForbidden` ([troubleshooting](troubleshooting.md#pull-request-step)): a waiting request stays `PlanFresh` `Unknown` and nothing is written.

The App's client ID (`appClientID`) changes only if you create a new App. It is a different key, a different installation and a new Key Vault key, so treat it as a new setup, not a rotation.

## 3. Federated credentials

A federated credential binds a managed identity to a ServiceAccount and holds no secret: nothing expires and there is nothing to rotate on a schedule. It has to change when one of its three parts changes:

| Part | Value in the manifests | Changes when |
| --- | --- | --- |
| Issuer | The cluster's OIDC issuer URL | The cluster is rebuilt. The lab does this every session, so `shadewright-azure/lab/federation.tf` recreates both credentials with the cluster |
| Subject | `system:serviceaccount:shadewright-system:shadewright-executor` (and `...:shadewright-analyzer`) | The namespace or a ServiceAccount is renamed |
| Audience | `api://AzureADTokenExchange` | Never, for AKS workload identity |

The managed identity's client ID goes into the annotation `azure.workload.identity/client-id` of the ServiceAccount. Replace the identity (new client ID) and you change the annotation in your overlay, restart the executor Deployment, and move the Key Vault role assignment (sign on the one key) to the new identity. A stale credential shows up as a request stuck in `Pending` without a reason, with an authentication error in the executor log ([troubleshooting](troubleshooting.md#no-reason-at-all)); no Git write has happened yet at that point.

## 4. Key Vault key versions and access

- Nothing needs an old version once it is replaced: a JWT lives ten minutes and is used only to get an installation token, and, once per provider, the App's own ID (`GET /app`, before its first comment on a pull request, to recognise its own earlier comments; the ID is kept and never changes), so after the cached token turns over (within an hour) the old version is not needed.
- The executor needs only `sign`. The lab's custom role holds the single data action `Microsoft.KeyVault/vaults/keys/sign/action` and is assigned at the scope of the one key. Rotating the key under the same name keeps that assignment; a **new key name** needs `github_app_key` changed in the foundation and the assignment re-applied.
- Importing and disabling versions needs your own `Key Vault Crypto Officer` (granted to whoever runs Terraform), not the executor's.
- If the vault is deleted, purge protection keeps it, and the key, recoverable for 7 days in the lab.

## After a suspected leak

In this order, because the first step is the one that stops the damage:

1. [Pause](emergency.md) and uninstall the App from the repository (GitHub, Installed GitHub Apps, Uninstall). Its installation tokens stop working at once; the executor can no longer push, open or close anything.
2. Delete the compromised private key on the App's page and disable the matching Key Vault version.
3. Review the `shadewright/*` branches and pull requests, and the `KeySign` entries of the vault's audit log for the period.
4. Generate a new key, import it (section 1), install the App again (section 2), unpause.
