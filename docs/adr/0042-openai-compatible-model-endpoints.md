# ADR-0042: OpenAI-compatible model endpoints and an optional API key

- Status: Proposed
- Date: 2026-11-16

## Context

Issue #834 extends the model boundary of ADR-0023 and the static credential rule of
ADR-0037. Foundry already speaks Chat Completions and Responses, but binds them to
an Entra identity. Operators also need the OpenAI API, gateways and models served
inside a cluster. A protocol name is not a claim that every server enforces schemas.
The decision of 2026-11-16 is one configurable provider, not one fixed vendor host.

## Decision

[![The analyzer selects one provider after redaction. Foundry uses Entra, Anthropic uses its own key, and an OpenAI-compatible endpoint uses a separate optional key bound to one install-time host. Both OpenAI wire formats share one parser. Every proposal still passes typed validation and policy.](../assets/diagrams/openai-compatible.svg)](../assets/diagrams/openai-compatible.svg "Open full size")

1. `provider: OpenAICompatible` takes `endpoint`, `deployment` (the model name) and
   `inferenceAPI` (`ChatCompletions`, the default, or `Responses`). `apiVersion`
   is absent: these endpoints take no Azure version query. The base includes the
   server's API prefix, for example `https://api.openai.com/v1`. The adapter appends
   `/chat/completions` or `/responses` using a parsed URL. Existing Foundry requests,
   recorded scenarios and goldens keep their exact bytes and semantics.
2. The same install-time `--model-endpoint-suffixes` bounds the destination of
   redacted context. HTTPS, a lower-case DNS host, port 443, no credentials, query,
   fragment or dot path segments are required. No proxy and no redirects, including
   same-host redirects. TLS verification stays on, with TLS 1.2 or newer.
3. Plain HTTP is refused, including for a Service. A future exception would require
   a separate accepted decision with an authenticated, encrypted transport boundary,
   explicit destination binding, credential protection and tests against interception
   and DNS rebinding. A namespace name alone is insufficient.
4. Public endpoints retain Foundry's public-address checks at dial time. A cluster
   Service may use private addresses only when its exact DNS name is also listed in
   the install-time `--openai-compatible-service-hosts` (not in a custom resource).
   Only `service.namespace.svc` or `service.namespace.svc.cluster.local` names qualify.
   This grants private unicast addresses only, never loopback, link-local, multicast,
   unspecified or reserved addresses. HTTPS and the suffix allow-list still apply.
   The operator must provision a trusted serving certificate and access controls.
5. Authentication is optional. No key file means no Authorization header. With a
   key, `--openai-compatible-api-key-file` and an exact
   `--openai-compatible-api-key-host` are required together. The host must match the
   endpoint exactly, so a settings edit cannot send that key to another allowed host.
   A Secret or CSI volume is mounted whole and read-only in the analyzer only, with
   no Secret API reads, new RBAC, environment credential or custom resource field.
   The bounded regular file is read at every attempt; rotation needs no restart.
   Unavailable or empty files are retryable, malformed files are refused. Errors
   and call reports contain fixed text, never keys, paths, headers or response text.
6. Provider credentials are separate. Foundry never receives either static key;
   OpenAICompatible never receives a token source or the Anthropic key. Anthropic's
   fixed host and Microsoft's Azure model host suffixes are refused by the new
   provider. Foundry additionally refuses `api.openai.com` and the install-time
   compatible key host and Service hosts. No fallback between providers exists.
   `--disable-foundry` prevents Azure token-source creation;
   the existing Foundry installation remains supported, while compatible-only
   installations set it and need no Azure client ID. Blob records still require Azure identity.
7. Missing or unusable usage is refused as malformed output with `UsageUnknown`, as
   in Foundry. It is never a zero-token success. The loop charges its conservative
   input estimate (one token per two bytes sent) plus the output bound. The compatible
   adapter never marks a host refusal as unbilled: a gateway may bill a 429. Valid
   reported input, cached input and output usage map to the existing token budget.
   Persistent daily token reservation across analyses remains #208, not a new claim.
8. Responses and tool calls remain untrusted. Strict response schemas are requested,
   and the same typed result and tool argument validators refuse invalid answers.
   Existing Foundry tool strictness is preserved byte for byte. A server that ignores
   tools or schemas is not repaired or retried with weaker schemas: it fails the same
   validation and bounded hand-off rules. Operators must qualify each model, server,
   API and prompt combination with the evaluation harness, including forced escalation.
   A transport smoke result is not a passing scenario evaluation.

## Consequences

On acceptance this amends ADR-0037 decision 2 and its 2026-11-16 amendment:
the suffix list is shared and Foundry may be disabled. Anthropic retains its fixed
host and separate key. Invariants 1 through 5 keep their validation, authorization
and Git boundaries.
Invariant 6 registers the analyzer's optional OpenAI-compatible API key under this
ADR, subject to every existing static credential condition. Executor and verifier
receive no key or new privilege. Anyone who can edit settings can choose among
install-time allowed destinations, but cannot move a key to another host.
Use narrow suffixes, a dedicated inference credential and a server-side spend cap.
An endpoint operator can see redacted context. Check retention, residency and that
training is opt-in before allowing a vendor; the Azure region promise does not
cover this provider. A compromised analyzer can read its mounted credentials.
Standard NetworkPolicy permits HTTPS addresses, not DNS host identities.

### Adversarial analysis

| Attack | Control and remaining boundary |
| --- | --- |
| Settings points at an attacker host | Install-time suffix list, exact key-host binding and provider host exclusions; admins own those installation controls |
| Redirect or proxy steals a key | No redirects of any status and no environment proxy |
| Public DNS rebinds into the cluster or metadata | Address checks on each connection; private addresses only for explicitly named Service hosts, never metadata or loopback |
| Service impersonation | Verified TLS with the Service DNS name; cluster operators own certificates and Service routing |
| Error or output echoes the key | Fixed errors, bounded discarded error bodies, no wrapped transport or filesystem errors; scan joined text and decoded structured content/arguments before returning a completion; adversarial error-path tests |
| Server ignores schema or invents a tool | Shared parser, typed argument/result validation, closed catalog and deterministic policy; no output repair |
| Server omits usage or returns 429 | Unknown usage costs the upper bound; no compatible-host unbilled exception |
| Key rotation races a call | Read per attempt, bounded retry, no cached key; update source before revoking old key |

### What is not built

Plain HTTP, arbitrary private-network gateways, custom authentication headers,
provider fallback, credential discovery, Azure tokens on compatible endpoints and
new permissions and chart-managed custom CA mounts are excluded. Cluster Service
certificates need a trusted root, provisioned in an operator-built analyzer image
when using an internal CA. The runtime qualification gate and persistent daily
token counter remain planned. Acceptance of this Proposed ADR, a passing live
OpenAI gpt-6.x scenario evaluation (also #501), and a Kind install with a dummy key
are merge prerequisites. Offline tests do not stand in for those observations.

## Alternatives considered

- A fixed-host OpenAI provider simplifies credential binding but excludes gateways
  and cluster servers, contrary to the decision.
- Reusing Foundry with a key mixes credential types and Azure URL/version semantics;
  it risks sending an Entra token or key to the wrong destination.
- Sending a key to every allowed suffix lets an ordinary settings edit move it.
  A separate exact host binding keeps the credential decision at install time.
- Rejecting all private addresses excludes cluster inference. Explicit Service DNS
  names give that use a bounded exception without an HTTP mode or new RBAC.

## Amendments

None.
