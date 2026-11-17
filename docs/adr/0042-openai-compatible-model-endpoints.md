# ADR-0042: OpenAI-compatible model endpoints and an optional API key

- Status: Accepted
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
2. The separate install-time `--openai-compatible-endpoint-suffixes` bounds the destination of
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
   Each chart `openAICompatible.serviceHosts` entry names `host`, `namespace`,
   a nonempty `podSelector` and the pod target `port`. One NetworkPolicy egress rule
   selects those pods in that namespace on that TCP port. The Service exposes 443;
   its target port may differ. No other selector-based access is added.
   The operator must provision a trusted serving certificate and access controls.
5. Authentication is optional. No key file means no Authorization header. With a
   key, `--openai-compatible-api-key-file` and an exact
   `--openai-compatible-api-key-host` are required together. The host must match the
   endpoint exactly, so a settings edit cannot send that key to another allowed host.
   A Secret or CSI volume is mounted whole and read-only in the analyzer only, with
   no Secret API reads, new RBAC, environment credential or custom resource field.
   The bounded regular file is read at every attempt; rotation needs no restart.
   One key binds one host: a second compatible endpoint on a different host is
   refused even when both hosts are allow-listed. The chart permits only `secret`
   or `csi` volume sources and checks key-host coverage and provider exclusions.
   Unavailable or empty files are retryable, malformed files are refused. Errors
   and call reports contain fixed text, never keys, paths, headers or response text.
6. Provider credentials are separate. Foundry never receives either static key;
   OpenAICompatible never receives a token source or the Anthropic key. Anthropic's
   fixed host and Microsoft's Azure model host suffixes are refused by the new
   provider. The Foundry list (`--model-endpoint-suffixes`) remains Foundry-only.
   Each provider refuses any host matching the other provider's list, including
   overlaps. Foundry additionally refuses `api.openai.com` and the install-time
   compatible key host and Service hosts. No fallback between providers exists.
   `--disable-foundry` prevents Azure token-source creation;
   the existing Foundry installation remains supported, while compatible-only
   installations set it and need no Azure client ID. Blob records still require Azure identity.
7. Usage is untrusted. Let `I` be the loop's conservative input estimate (one token
   per two bytes of messages, tool schemas, response schema and provider blocks),
   `O` the request's output bound, and `C = I + O + ceil((I + O) / 10)` the ceiling.
   The 10% margin allows tokenizer variation without accepting an unbounded report.
   For complete, nonnegative reported usage `R = input + output` at or below `C`,
   charge `max(I, R)`, including a zero-token report and malformed output carrying
   usable usage. Cached input is part of input, never added twice. A report above
   `C` is refused as malformed output, its usage discarded (`UsageUnknown`), and
   charged `C`. Missing, negative, incomplete or otherwise unusable usage is also
   refused with `UsageUnknown` and charged `C`. Other failed sent calls, including
   throttling, cost `C`; compatible hosts never mark a refusal unbilled. A call
   whose every attempt sent nothing costs zero. Retried attempts with lost usage
   keep `UsageUnknown` and cost `C`. Foundry and Anthropic accounting is unchanged.
   Persistent daily token reservation across analyses remains #208.

8. Responses and tool calls remain untrusted. Strict response schemas are requested,
   and the same typed result and tool argument validators refuse invalid answers.
   Existing Foundry tool strictness is preserved byte for byte. A server that ignores
   tools or schemas is not repaired or retried with weaker schemas: it fails the same
   validation and bounded hand-off rules. Operators must qualify each model, server,
   API and prompt combination with the evaluation harness, including forced escalation.
   A transport smoke result is not a passing scenario evaluation.

## Consequences

This extends [ADR-0037](0037-the-anthropic-api-as-a-second-model-provider.md) through its dated
2026-11-17 amendment: a third provider has a separate list, the Foundry list stays
Foundry-only, and Foundry may be disabled. Anthropic retains its fixed
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
token counter remain planned. Qualify each model and server with the evaluation
harness and test the key mount in an isolated Kind install with a dummy key.
Offline transport tests do not replace scenario qualification.

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

- 2026-11-17: accepted with the decision on #834: separate per-provider allow-lists,
  an input-estimate floor and a ceiling with a 10% margin on untrusted usage,
  and one selector-based egress rule per named Service. Joined reply text is checked
  for key echoes, compatible errors name their provider, and key volume sources
  are restricted to Secret or CSI.
