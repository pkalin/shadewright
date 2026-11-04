# ADR-0016: Lab region Sweden Central

- Status: Accepted
- Date: 2026-11-01

## Context

The lab needs one region for AKS, Key Vault, storage and the model deployment. The region does not have to be North Europe.

Checked 2026-11-01 on the lab subscription (`az cognitiveservices model list`) and with the Azure Retail Prices API (EUR, pay-as-you-go):

- Sweden Central is the only EU region listed with both OpenAI and Anthropic models. North Europe lists OpenAI models only.
- Azure OpenAI token prices are identical across EU and US regions; Global Standard is about 10% cheaper than Data Zone Standard.
- Lab node `Standard_B2as_v2` (2 vCPU, 8 GiB): 0.0698 EUR/h in Sweden Central, 0.0733 in North Europe, 0.0675 in East US 2.

## Decision

Run the whole lab in Sweden Central, so cluster, identities, storage and model are in one region, and the node price is the lowest of the EU candidates compared (Sweden Central and North Europe, 2026-11-01); East US 2 was cheaper still.

## Consequences

- One region to reason about for latency and data location of the resources.
- Anthropic models there are offered as Global Standard only, so inference may run outside the EU; acceptable for lab data, which is synthetic and redacted. Data Zone deployments of OpenAI models are available when EU processing matters.
- Third-party models may be billed through Azure Marketplace, which a credit-based subscription may not cover; check on the first deployment.

## Alternatives considered

- North Europe: no Anthropic models and a slightly higher node price.
- East US 2: cheapest node, but data and latency outside the EU for a European maintainer.
