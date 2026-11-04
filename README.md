<h1 align="center">
  <img src="docs/assets/wordmark.png" alt="Shadewright" width="520">
</h1>

<p align="center">
  A safe-remediation controller for Kubernetes clusters run with GitOps: it turns untrusted proposals, from rules or an LLM, into typed, bounded changes tied to policy and to the object's real source in Git, delivers them only as reviewed pull requests, and verifies the effect.
</p>

> [!WARNING]
> Early. Built so far: the API and its validation, the policy check with source mapping and the freshness check, the YAML patchers, the semantic diff check, the GitHub and Key Vault adapters, the pull request step (branch, commit and pull request, up to a person's review), the kill switch (`spec.paused`, one gate right before every Git write), the collector (events and pod status to deduplicated observations) and the Incident controller (one bounded `Incident` per fingerprint, marked due for analysis within a daily budget), and the module registries with the generic and AKS platforms and the cloudflared, Flux, KEDA, Kyverno and Secrets Store CSI driver integration modules (cloudflared replicas that are not ready, failing Flux objects, KEDA trigger errors and replica ceilings, and failing Kyverno policy report results, policies not Ready and refused admissions, as symptoms; secrets volumes that cannot be mounted, diagnosed from the kubelet's mount events). All of them are in the analyzer, which detects them and reports them in the settings status; nothing feeds the diagnosis rules yet, and the symptom sources are not consumed yet. Not built: the analysis, auto-merge, delivery and the verification. The [design](docs/design.md) says what holds at each step; the [decisions](docs/adr/index.md) say why.

## What it does

1. **Observes** events and object status in the namespaces you choose, and deduplicates them into incidents.
2. **Diagnoses** with rules first, then a model that has read-only tools and redacted context. No evidence, no proposal.
3. **Proposes** a typed `RemediationRequest` from a closed catalog of actions, checked by deterministic policy.
4. **Opens a pull request** in your GitOps repository, with the evidence and a rollback plan. Your branch rules decide what merges.
5. **Verifies** the result after the GitOps controller (Flux today) applies it, and escalates when the symptom stays.

It never changes your workloads or any other object it watches: it writes only its own control state (Incidents, RemediationRequests and their status, the remediation settings status, an analysis budget ConfigMap, leader election Leases and Events), and a model's answer is never authorization.

## Repositories

| Repository | Contents |
| --- | --- |
| [shadewright](https://github.com/pkalin/shadewright) | The operator (Go, Kubebuilder) and the docs at [shadewright.io](https://shadewright.io) |
| [shadewright-azure](https://github.com/pkalin/shadewright-azure) | Terraform for a disposable reference lab in Azure |
| [shadewright-fleet](https://github.com/pkalin/shadewright-fleet) | The lab's GitOps repository and incident scenarios |

## Development

```bash
make manifests generate   # after changing API types
make lint-fix
make test                 # unit and envtest, no cluster needed
make run                  # against the current kubeconfig context
make docker-build-images  # executor and analyzer images for the host platform
```

Images (distroless, non-root, linux/amd64 and linux/arm64) are published from `main` and release tags to `ghcr.io/pkalin/shadewright-executor` and `ghcr.io/pkalin/shadewright-analyzer`.

Contributor and coding-agent guide: [AGENTS.md](AGENTS.md).

## The name

*Shade* for the dark elves' half-light it works from, *wright* (Old English *wyrhta*, as in *shipwright*) for a
craftsman: it builds fixes as reviewed changes instead of casting them onto the cluster.
[The longer story](https://shadewright.io/name/).

## License

Apache License 2.0, see [LICENSE](LICENSE).
