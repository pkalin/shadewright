# ADR-0011: Policy as a cluster-scoped CRD

- Status: Accepted
- Date: 2026-11-01

## Context

Policy decides what the agent may do. It must be owned by the cluster admin, versioned so approvals can be bound to a revision, and impossible for the analyzer or a request author to change.

## Decision

A cluster-scoped `RemediationPolicy` CRD. Each object lists allowed action types, a target selector (namespaces and labels; the kinds an action may target are fixed per action by the catalog, not set in the policy), parameter bounds, the Git paths it may patch, and whether review is always required. Its UID and `metadata.generation` together are the policy revision included in every plan digest; the UID matters because a deleted and recreated policy can reach the same generation again. Only the admin role can write it; the analyzer and executor can only read it. The automation kill switch lives in a cluster-scoped config object next to it.

## Consequences

- Policy is visible and auditable with `kubectl`, validated by its schema, and protected by plain RBAC (and admission rules if needed).
- A policy change invalidates in-flight approvals that referenced the old generation.
- Policy is not in Git by default; teams that want that can manage the CRs through their GitOps repo like any other resource (but never through Shadewright's own PRs).

## Alternatives considered

- ConfigMap: no schema, no generation semantics, easier to edit by mistake.
- Policy files in the fleet repo: the agent writes to that repo, so it would sit next to the thing it guards.

## Amendments

- 2026-11-04: the target selector has no `kinds`. Each action type fixes the kinds it may target in the catalog ([design 5.1](../design/5-api.md#51-remediationrequest)), so a policy cannot widen them; the Decision now matches the CRD and the design. This narrows the original wording and widens nothing ([#244](https://github.com/pkalin/shadewright/issues/244)).
