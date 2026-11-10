# ADR-0029: An Azure module with dedicated services, environment and cluster layers, restricted by default and private on request

- Status: Proposed
- Date: 2026-11-10

## Context

Epic [#545](https://github.com/pkalin/shadewright/issues/545) lets someone install Shadewright on an existing AKS cluster with a published Terraform module and a reference architecture. Task [#546](https://github.com/pkalin/shadewright/issues/546) records the decisions the epic made on 2026-11-08, after an assessment, `/breakdown` and `/challenge premortem`. This ADR writes them down with the alternatives; it adds no new decision.

What runs on Azure today is the lab in the `shadewright-azure` repository, written for one subscription of the maintainer:

- `foundation/` is the part that outlives a lab: a Foundry account with model deployments (`foundry.tf`), a Key Vault holding the GitHub App key (`keyvault.tf`), a storage account for analysis records (`analysis-store.tf`), a Log Analytics workspace for the Key Vault audit log (`keyvault.tf`), and one managed identity per component (`identities.tf`).
- `lab/` is the part that is applied for a session and destroyed: the AKS cluster (`aks.tf`), Flux (`flux.tf`) and the federated credentials that bind the identities to the cluster's ServiceAccounts (`federation.tf`).
- Roles are narrow: the analyzer may only create blobs and call the models, the executor may only sign with the one key ([ADR-0006](0006-github-app-key-in-key-vault.md), invariant 6). Both custom roles are defined at subscription or resource group scope (`analysis-store.tf`, `keyvault.tf`).
- Services are reachable over public endpoints restricted by an IP allowlist and the cluster's subnet, with fixed names built from a prefix.

That is right for a lab and wrong for anyone else: another subscription, a locked-down network, a platform team that owns role definitions and DNS, several clusters. The decisions below make the same grants reproducible in a module others can read and test.

## Decision

### 1. What the module is

The module adds what Shadewright needs on Azure to an **existing** cluster. It never creates the cluster, the virtual network, hubs, firewalls or any other part of the adopter's network. Out of scope: other clouds, AKS itself.

It lives in its own repository, `terraform-azurerm-shadewright` (logo, rulesets, CI, release-please), and is published to the Terraform Registry. The lab in `shadewright-azure` uses it, so the lab is the first adopter and the module is tested by the way it is used. Code and the repository are the work of the epic's other tasks; this ADR fixes the shape.

### 2. Dedicated services, always created by the module

The module always creates its own Key Vault, Foundry account with the model deployments, analysis-record storage account and audit log workspace. It does not accept existing ones. The reason is invariant 6 and need to know: every role assignment is scoped to a resource that holds only Shadewright's data, so "the executor may sign with this one key" and "the analyzer may create blobs in this one container" stay true and can be pinned in a test (section 6). On a shared Key Vault or storage account the same grants would sit beside other teams' keys and blobs, and the module could not state what they allow.

### 3. Two layers

[![Environment layer, shared by clusters: Foundry account, Key Vault, analysis storage, audit log, private endpoints and DNS. Cluster layer, one per cluster: analyzer and executor identities, federated credentials, role assignments and Helm values. The adopter brings the cluster and the network.](../assets/diagrams/azure-layers.svg)](../assets/diagrams/azure-layers.svg "Open full size")

- **Environment layer**, once, shared by clusters: Foundry account and model deployments, Key Vault, analysis storage, audit log workspace, private endpoints and (optionally) private DNS zones. It outputs resource IDs, endpoint names, and the zones and records the services expect.
- **Cluster layer**, once per cluster: the analyzer and executor managed identities, a federated credential for each that trusts the cluster's OIDC issuer and one ServiceAccount, the role assignments on the environment layer's resources, and the Helm values for the chart as an output.

The split follows the lifetimes in the lab: the foundation outlives a cluster (records, the key and the audit trail are kept), the federation is recreated with every cluster because the issuer URL changes (`lab/federation.tf`). A second cluster adds a cluster layer and nothing else. Identities are per cluster, so a cluster's pods never hold the token of another cluster's identity and one cluster's grants can be removed alone. Still one identity per component, never one token for both.

[![What the module creates and what the adopter brings: left, the services, endpoints, identities, federated credentials, role assignments and outputs the module creates; right, the cluster's OIDC issuer, the subnet for private endpoints, optional existing DNS zones, names and tags, and optional role definitions the adopter passes in.](../assets/diagrams/azure-module-boundary.svg)](../assets/diagrams/azure-module-boundary.svg "Open full size")

### 4. Private by default, DNS in one of three modes

Public network access is off on every service. The module creates a private endpoint for each service in a subnet **passed in by ID**; the network around the subnet is the adopter's. Private DNS has three modes, one variable:

1. **Create.** The module creates the private DNS zones, links them to the virtual network it is told about, and registers the endpoint records.
2. **Existing.** The adopter passes the IDs of zones that already exist (a hub or a Virtual WAN). The module registers the records in them and creates no zone. The deployer needs the right to join the zone and write records there.
3. **None.** The module creates no zone and no record. Azure Policy in the landing zone registers the records for private endpoints. The module outputs the zones and records it expects so the platform team can check them.

A restricted (not private) setting exists for a quick start: public access on with an allowlist, as in the lab today. It is not the default.

### 5. Deployer permissions per layer, and the landing-zone option

The module states what the identity that runs Terraform needs, per layer, so a platform team can grant exactly that.

| Layer | Deployer needs | Why |
| --- | --- | --- |
| Environment | Create resources in the resource group; join the subnet for private endpoints; write DNS records in the zones (modes create and existing); write role assignments and, by default, role definitions | The services, the endpoints, the DNS records and the roles that scope the grants |
| Cluster | Create managed identities and federated credentials in the resource group; write role assignments on the environment layer's resources | One identity and one credential per component, and the grants of section 6 |

The role assignments are the sensitive part: writing them is a privileged right. The module needs it only for the assignments it lists, so the documentation shows how to restrict the deployer to them (a role assignment condition limited to the module's roles) instead of asking for Owner.

**Landing-zone option.** Many platform teams define custom roles ahead of time and do not let a workload deployer create them. The module can take the role definitions as input, by ID, instead of creating them. The roles stay the ones the pinned test describes (analysis writer: create blobs only; Key Vault signer: sign only). With them passed in, the deployer needs to write role assignments but not role definitions. The module publishes the exact definitions, for the platform team to create. The default stays "the module creates them", because that is what the lab does today and what a single adopter wants.

### 6. Grants are pinned by a test (invariant 6)

A test of the module pins every role assignment it creates: role (or its definition's data actions), scope and principal. The analyzer gets inference on the Foundry account and create-only blob access on the one container; the executor gets sign on the one key. Anything else, a wider scope, an added action, a new assignment, fails the test. Changing the test is a change to invariant 6 and needs the maintainer's review, like a change to the operator's RBAC. This is the Azure side of the rule that the analyzer and the executor never share an identity and that no component gets more than it needs.

### 7. Naming and tags

The module ships naming defaults, close to what the lab uses (a resource type abbreviation, a workload name, a random suffix where Azure needs a globally unique name). Each pattern can be overridden, and each single resource name too, because adopters follow their own convention. Adopter tags are merged with ours; the adopter's win on a key clash, and we add only a small fixed set that says what the resource is for.

### 8. Accepted risk: private mode is not exercised in the lab

The lab runs the restricted, public-endpoint setting; it does not run private endpoints or any of the three DNS modes, because the network that would make them real (hub, firewall, private zones) is the part the module does not create and the lab does not pay for. So the private path can be wrong in ways the lab never shows. We accept that, and reduce it: the module outputs the zones and records it expects, the module's tests cover the plan of each mode, and the documentation gives an in-cluster check (resolve each service's name from a pod and call the endpoint) that an adopter runs after install. The risk stays on the register until an adopter or a priced lab run has exercised private mode.

### 9. Known limitation: an existing model gateway

Some organizations put every model call behind an API Management gateway they own. The module does not support using it in place of its own Foundry account. It is in the backlog of the epic. Until then such an adopter runs the module's account, or does not use the module for the model.

## Consequences

- One reference for what Shadewright creates on Azure and what each identity may do, testable, and the lab and adopters run the same code.
- More resources than a bring-your-own design: an adopter who wants to reuse a Key Vault or a Foundry account cannot. That cost is the price of scoped grants.
- A deployer with rights to write role assignments is needed in every setup. The landing-zone option removes the need to create definitions, not to assign them.
- Two layers mean two applies, with outputs passed from one to the other, in a fixed order, and a destroy that goes the other way.
- The private path ships with less proof than the restricted one (section 8).
- A new repository to maintain: CI, ruleset, releases, Registry publishing.
- Invariant 6 now has an Azure half whose grants live in the module's pinned test; the operator's AGENTS.md text does not change.

## Alternatives considered

- **Bring your own services (an existing Key Vault, Foundry account, storage).** Fewer resources and a natural fit for shared platforms. Not chosen: grants on shared resources cannot be scoped to Shadewright's own data (invariant 6, need to know), the module could not pin what they allow, and a shared Key Vault would hold the App key next to unrelated keys.
- **One instance of everything per cluster.** One layer, simpler. Not chosen: it multiplies Foundry accounts (with their model quota), Key Vaults and Apps' keys for no gain in isolation, since the grants are already per identity, and it ties the records and the audit trail to a cluster that may be recreated (`lab/` is disposable on purpose).
- **A full stack that also creates AKS and the network.** A one-command demo. Not chosen: AKS, networks, hubs and firewalls are the adopter's to decide, and a module that creates them cannot be used on an existing cluster. The lab keeps its own AKS code in `shadewright-azure`.
- **A module inside `shadewright-azure`.** No new repository. Not chosen: the Terraform Registry needs a repository named `terraform-<provider>-<name>` with its own tags and releases, the lab holds lab-only code and values, and a module consumed from a lab repository is not a published, versioned interface.
- **Public endpoints by default, private on request.** Easier to try. Not chosen: a default that exposes the Key Vault and the records to the internet is the wrong default for a tool whose point is least privilege.

## Amendments

None.
