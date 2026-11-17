# ADR-0042: OpenAI-compatible model endpoints and an optional API key

- Status: Proposed
- Date: 2026-11-16

## Context

Issue #834 extends the model boundary of [ADR-0023](0023-how-the-analyzer-calls-the-model.md) and the static credential rule of
ADR-0037. Foundry already speaks Chat Completions and Responses, but binds them to
an Entra identity. Operators also need the OpenAI API, gateways and models served
inside a cluster. A protocol name is not a claim that every server enforces schemas.
The decision of 2026-11-16 is one configurable provider, not one fixed vendor host.
On 2026-11-17 the [decision on issue #834](https://github.com/pkalin/shadewright/issues/834#issuecomment-6946338496)
bound the models at install time as well: each endpoint declaration names an exact
base URL and the model names allowed at it, so settings choose neither the base nor
the model alias a gateway routes on.

## Decision

[![The analyzer selects one provider after redaction. Foundry uses Entra, Anthropic uses its own key, and an OpenAI-compatible endpoint uses a separate optional key bound to one install-time base URL and its model list. Both OpenAI wire formats share one parser. Every proposal still passes typed validation and policy.](../assets/diagrams/openai-compatible.svg)](../assets/diagrams/openai-compatible.svg "Open full size")

Three destination choices require install-time binding: the host, the gateway
path and the model alias. A gateway can route an alias to another upstream,
carrying the key and redacted context with it. Settings must
choose no part of the request destination.

1. The install declares each compatible endpoint through chart
   `analyzer.analysis.openAICompatible.endpoints` or the analyzer's
   `--openai-compatible-endpoints` JSON array. Each declaration has an exact `url`
   (scheme, DNS host, port and path prefix), `inferenceAPI` (`ChatCompletions` or
   `Responses`), a nonempty `models` list, optionally `keyFile`, and optionally `service` with `namespace`,
   a `podSelector` of 1 to 32 valid Kubernetes labels and pod target `port`, refused
   otherwise by both chart render and analyzer startup. There is no compatible suffix list.
   Each model name matches `^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`.
   Chart `apiKey` sources are an existing Secret or CSI volume; the chart derives
   the declaration's mounted file path, one separate mount per endpoint.
2. `provider: OpenAICompatible` settings name `endpoint`, `deployment` (model name)
   and `inferenceAPI` (`ChatCompletions` when absent). The analyzer compares the
   parsed base and API against the declarations on every deployment. Scheme and
   DNS case, default port 443 and one trailing slash normalize; unreserved path
   encodings normalize after parsing. Dot segments, encoded separators, userinfo,
   query and fragment are refused, never cleaned into an allowed base. A mismatch
   ends `EndpointNotAllowed`. The deployment model must equal one of that endpoint's
   declared names byte for byte; otherwise it ends `ModelNotAllowed` before any call.
   The request URL is the declared base plus the fixed
   `/chat/completions` or `/responses` path. Nothing from settings is joined into it.
   `apiVersion` is forbidden. CEL remains a shape check, never install authorization.
3. HTTPS with verified TLS 1.2 or newer is required, including for Services. There
   is no proxy or redirect, including a same-host redirect. Public addresses are
   checked at each dial. Only a declaration with `service` permits private unicast
   addresses, and only for its exact `service.namespace.svc` or
   `service.namespace.svc.cluster.local` name. Loopback, metadata, link-local,
   multicast and reserved addresses remain refused. A Service address in
   100.64.0.0/10 is refused too, even for a declared Service: carrier-grade NAT
   and cloud infrastructure share that range (one cloud serves its instance
   metadata at 100.100.100.200), so the dial guard cannot tell a Service there from
   a host outside the cluster. Such a cluster needs the inference Service on an
   RFC 1918 address. Each chart Service declaration
   adds one namespace/pod-selector egress rule on its target TCP port. Operators
   provision serving certificates, access controls and trusted roots. Declared
   HTTPS ports are bounded to 1 through 65535; the chart also opens each declared
   public endpoint port other than 443 in a separate egress rule excluding
   10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, 100.64.0.0/10 and 169.254.0.0/16.
   The existing 443/6443 rule is unchanged. Foundry stays on 443.
4. A key belongs only to its declaration's complete base. Two endpoints can have
   different keys, including on the same gateway host at different paths. An
   endpoint without a key sends no Authorization header. Only the analyzer mounts
   the whole read-only Secret or CSI source, with no subPath, Secret API reads,
   new RBAC, credential environment variable or custom resource field. Each attempt
   rereads its bounded regular file, so rotation needs no restart. Unavailable or
   empty files are retryable; malformed files, including keys shorter than 16 bytes,
   are refused without retry and with a fixed message naming the minimum length.
   Fixed errors and call reports never contain keys, paths, headers or response text.
5. Both chart render and analyzer startup refuse a declaration whose host matches
   the Foundry suffix list, an Azure model host or `api.anthropic.com`; duplicate
   parsed bases (even with different APIs or keys); a base that is a segment path
   prefix of another on the same scheme, host and port; missing, empty or invalid
   model lists; and malformed URL declarations.
   A generated corpus varies settings hosts, ports, paths, case, encodings and dot
   segments and asserts that credentials reach only requests under their own base.
6. The Foundry suffix list remains Foundry-only and never authorizes compatible
   endpoints. Foundry refuses any declared compatible host, as does Anthropic's
   fixed-host boundary. No Entra token or Anthropic key reaches the compatible
   adapter and no compatible key reaches another provider. `--disable-foundry`
   prevents inference token-source creation. Compatible-only installs need no
   Azure client ID for inference; Blob records still require Azure identity.
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

The credential echo check scans raw reply bytes before parsing a completion, then
concatenates parsed strings, numbers and object keys in wire order. Every
comparison removes whitespace and every Unicode control (Cc) and format (Cf)
character from the text and the key, the characters the kubectl plugin drops
when it prints, so a zero-width space, soft hyphen or NUL inside a key cannot
pass the scan and then show the whole key on a screen. It also scans decoded content and arguments, joining their values and
keys across parts. It detects literal and JSON-escaped echoes, including keys split
between content, argument keys and numeric values. It fails closed: the scan of
every parsed value runs whatever the typed decode of content and arguments does, a
reply that is not JSON is scanned as raw bytes, and a reply whose content or
argument fields have a wrong JSON type is refused with its own fixed text, "the
reply has a field of the wrong type", as malformed output like an echo (the
completion parser refuses such a reply too). It does not detect transformed
keys such as base64 or a key split across two replies. Outputs remain untrusted:
the redactor checks records and telemetry, typed result and tool argument validation
and deterministic policy constrain proposals, and credentials never authorize a
remediation. These controls do not promise detection of every transformed secret;
operators must trust the endpoint with its dedicated, spend-limited credential.

## Consequences

This proposes an extension to [ADR-0037](0037-the-anthropic-api-as-a-second-model-provider.md).
ADR-0042 remains Proposed. Anthropic keeps its fixed host and separate key;
Foundry keeps its suffix boundary. Invariants 1 through 5 retain their validation,
authorization and Git boundaries. Invariant 6 covers only the analyzer's optional
endpoint-bound compatible keys under every existing static credential condition.
Executor and verifier receive no key or new privilege. A settings editor can
select declared endpoints but cannot choose a different destination for any key.
Use dedicated inference credentials and server-side spend caps.
An endpoint operator can see redacted context. Check retention, residency and that
training is opt-in before allowing a vendor; the Azure region promise does not
cover this provider. A compromised analyzer can read its mounted credentials.
Standard NetworkPolicy permits HTTPS addresses, not DNS host identities.

Two trust assumptions remain:

- A cluster that uses address space for pods or Services outside RFC 1918 other
  than 100.64.0.0/10 has those addresses treated as public, both by the dial guard
  and by the except list of the egress rule for declared public ports. A declared
  public endpoint could then reach such a pod or Service address.
- The right to create pods in a declared Service's namespace, or to edit that
  Service or its EndpointSlices, is equivalent to holding the key and the redacted
  context. Such a pod can mount the namespace's Secrets, including the Service's
  serving certificate, so verified TLS does not stop it; routing follows the
  Service's own selector and EndpointSlices, not the declared `podSelector`; and
  where the CNI applies `ipBlock` rules to pod addresses, the 443 rule admits any
  pod. Run the inference Service in a namespace where only cluster administrators
  create pods.

### Adversarial analysis

| Attack | Control and remaining boundary |
| --- | --- |
| Settings points at an attacker host | Exact parsed install-time base, API and model matching; requests use declaration data only, including the fixed inference route |
| Redirect or proxy steals a key | No redirects of any status and no environment proxy |
| Public DNS rebinds into the cluster or metadata | Address checks on each connection; private addresses only for explicitly named Service hosts, never metadata or loopback |
| Service impersonation | Verified TLS with the Service DNS name; cluster operators own certificates and Service routing. Pod creation in the Service's namespace, or edits of the Service or its EndpointSlices, bypass it (a pod can mount the serving certificate): only cluster administrators may create pods there |
| Error or output echoes the key | Fixed errors, bounded discarded error bodies, no wrapped transport or filesystem errors; scan the raw body before reply parsing and concatenations, without whitespace, control or format characters, of every parsed string, number and object key, including decoded structured content and arguments, before returning a completion; a wrong-typed decoy field fails closed with its own fixed text |
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

- A host suffix list with a key bound to a host leaves settings in control of the
  path. On a shared gateway that path can select another tenant or upstream.
  This alternative was not chosen: restricting hosts does not bind credentials to
  an account or model upstream. Exact install-time bases and model lists remove
  those destination choices from settings.
- An egress proxy or FQDN egress policy for declared hosts adds an infrastructure
  dependency and does not bind gateway paths or model aliases. Operators may add
  this defense in depth; portable NetworkPolicy and application checks remain
  the shipped boundary.
- A separate credential-holding egress component (a sidecar or small proxy with
  its own identity) that owns the key and forwards only to declared bases. The
  analyzer would never read the key. Not chosen now: it adds a component and an
  identity. It is the next step if a compromised analyzer reading its mounted keys
  becomes the concern.
- A fixed-host OpenAI provider excludes gateways and cluster servers.
- Reusing Foundry with a key mixes identity and Azure URL/version semantics.
- Refusing all private addresses excludes cluster inference; explicit Service
  declarations provide a bounded TLS and egress exception without new RBAC.

## Amendments

None.
