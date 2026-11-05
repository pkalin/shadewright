---
title: Overview
---

# Overview

What Shadewright is, what it is not, and how far it has got. For the one-sentence version and the five steps, see the [home page](index.md).

## What it is

A safe-remediation controller for Kubernetes clusters run with GitOps. It watches events and object status in the namespaces you choose, groups repeated symptoms into incidents, and turns a proposal (from rules, or from an LLM) into a typed, bounded change. The change is checked by deterministic policy against the object's real source in Git and delivered only as a reviewed pull request to your GitOps repository.

## What it is not

- Not a controller that edits your workloads: it writes only its own control state (Incidents, RemediationRequests and their status, the remediation settings status, an analysis budget ConfigMap, leader election Leases and Events).
- Not an auto-fixer that trusts a model: a model's answer is never authorization, and everything read from the cluster is untrusted input.
- Not a replacement for review: your branch rules decide what merges, and your GitOps controller delivers it.

## How it works

Observe, diagnose, propose, open a pull request, verify: the five steps are on the [home page](index.md#how-it-works), the flow with its components is in the [design](design/2-architecture.md#2-architecture), and the order in which a request moves is in [design 6](design/6-reconciliation.md#6-reconciliation).

## Requirements and compatibility

A cluster whose API server supports CEL validation rules, Flux delivering the target from a Git repository on GitHub, and, for pull requests, a GitHub App whose key is held in Azure Key Vault. The tested versions are in [Compatibility](reference/compatibility.md).

## Status

The project is early. The [status section](index.md#status) of the home page lists what is built and what is not, the [threat model](threat-model.md) marks each guarantee as built or planned, and the [milestones](design/13-milestones.md#13-milestones) say what comes next.

## Where to go next

- To try it: [Get started](getting-started.md).
- To run it: [Operate](operate.md).
- For the reasoning: [Design and decisions](design-and-decisions.md).
