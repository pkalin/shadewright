# Security policy

Shadewright is pre-release software (0.x). The `main` branch and the latest release are supported; there are
no maintained older lines yet.

## Reporting a vulnerability

Report privately through GitHub: [open a security advisory for this repository](https://github.com/pkalin/shadewright/security/advisories/new).
The form is private: only the maintainer sees it. Do not open a public issue or pull request for a
vulnerability, and do not describe it in a pull request that fixes it.

Include what you can: the affected component and version or commit, how to reproduce it, and what an attacker
gains. A proof of concept against your own cluster and repositories is welcome; never test against clusters,
repositories or accounts you do not own.

What to expect: an acknowledgement within 7 days, and a fix or a decision within 90 days. A confirmed
vulnerability gets a GitHub security advisory, a CVE where it applies, and a release. The reporter is credited
unless they prefer not to be.

## Scope

In scope: the operator in this repository, its container images and Helm chart, and the reference lab in
[shadewright-azure](https://github.com/pkalin/shadewright-azure) (it has its own advisory form). The workloads
in [shadewright-fleet](https://github.com/pkalin/shadewright-fleet) are broken on purpose; their problems are
not vulnerabilities.

The invariants in [AGENTS.md](AGENTS.md) are what the project promises: a model's output is never
authorization, actions are a closed catalog, changes go through Git only, everything from the cluster is
untrusted input, and the analyzer and the executor have separate identities. A way to break one of them is a
vulnerability even without a working exploit, for example a request that makes the executor change a file its
policy does not allow, a path by which cluster data reaches a model unredacted, or a token or key that ends up
in a log, a status or Git.

Not in scope: vulnerabilities in dependencies that are already public (open an issue, or a pull request that
updates the dependency), and findings that need `cluster-admin` or a compromised maintainer account to begin
with.
