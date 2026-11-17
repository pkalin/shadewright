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
   The existing 443/6443 rule is unchanged. Foundry stays on 443. Ranges the
   install declares as in-cluster (chart value
   `analyzer.analysis.openAICompatible.inClusterCIDRs`, flag
   `--openai-compatible-in-cluster-cidrs`, decided 2026-11-17) count like the
   private ranges: the dial guard of every compatible endpoint that is not a
   declared Service refuses an address in them, after DNS, for every resolved
   address and before any byte of the request or key is sent, and the IPv4 ones
   are added to the except list of the rule for declared ports other than 443
   (that rule allows no IPv6 address). On 443 the dial guard alone enforces the
   private and declared ranges: that rule has no except list, since it carries
   the traffic to the API server at its in-cluster address. An entry must be a
   canonical CIDR without host bits, not a /0, and an IPv4 range in IPv4 form
   (no IPv6 range overlapping the IPv4-mapped, IPv4-compatible, NAT64 or 6to4
   forms, which the guard already reads by their IPv4 address); the chart
   refuses any other entry at render and the analyzer at start
   ([rule](../reference/configuration.md#in-cluster-ranges)).
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

The credential echo check searches an allow-list view of the reply for the key
before any completion is returned. The view keeps only ASCII letters, digits and
the punctuation characters the key itself contains, and drops every other
character, of any Unicode category, assigned or not. A character inserted
between the key's characters (a zero-width space, U+034F, a variation selector,
a Hangul filler or a visible letter of another script) is dropped by
construction, so the check does not depend on a list of characters a screen
does not show. The view is built over every decoded string value of the reply in
document order as one stream, so a key split across fields, parts or tool-call
arguments is found too; a second stream adds the object keys in their place,
each of the two is also built with the numbers of the document in their place
(a key split into a number), so a number never interrupts a stream of strings, and
the fields the completion parser reads (text parts, content, arguments) are
joined once more on their own. JSON text inside a string, such as tool-call
arguments and structured content, is decoded up to three levels and takes the
string's place in the streams; its text as written is searched as well. The raw
bytes are searched the same way, and a reply that is not JSON is scanned only as
raw bytes.

The check does not model terminal escape sequences. Model text is stored without
control characters: every code point of Unicode category Cc (C0, DEL, C1) except
newline and tab is removed where model output enters the analysis loop, for every
provider ([#852](https://github.com/pkalin/shadewright/issues/852)). What is
stored is then what any terminal shows, and the view drops a superset of what
storage removes, so a key that is contiguous in stored text is contiguous in the
view and is refused. A sequence with visible parameters inside the key, such as
`ESC [0m`, is not refused: storage removes the `ESC`, and the stored text shows
`[0m` between the key's characters, so no reader sees the key whole. A grammar
of terminal behaviour in the scan was tried and dropped: it is a list of what a
screen hides again, and it diverged from what a reader sees field by field. This
provider must not ship before #852. Every pass is linear in the size of the
reply; the tests bound the work at a fixed multiple of it.

The key must be printable ASCII and at least 16 bytes; anything else is refused
as malformed when the key file is read, before any call, so the view never
searches for a key it cannot hold or one too short to search. The check fails
closed: the scan of every value runs whatever the typed decode of content and
arguments does, and a reply whose content or argument fields have a wrong JSON
type is refused with its own fixed text, "the reply has a field of the wrong
type", as malformed output like an echo (the completion parser refuses such a
reply too).

The check has a stated limit. A key that comes back re-encoded (base64, percent
encoding, homoglyphs, reordered characters) or split across two replies is not
found, and no check of this kind can find it. The endpoint is given the key at
install and is trusted with it: the check stops an echo from reaching status,
Events, logs and pull requests in a form a reader can use, and is not a defence
against an endpoint that hides the key. The check covers terminal tricks only
together with #852: without that change, control characters and escape sequences
inserted into the key could hide letters from a raw terminal that the view keeps.
Outputs remain untrusted: the redactor
checks records and telemetry, typed result and tool argument validation and
deterministic policy constrain proposals, and credentials never authorize a
remediation. Operators must trust the endpoint with its dedicated,
spend-limited credential.

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

For this provider only, ADR-0042 amends decision 9 of
[ADR-0023](0023-how-the-analyzer-calls-the-model.md) (its amendment of
2026-11-17 links back here): every usage counts at least the loop's input
estimate, at most the ceiling `C` of decision 7; a reply that reports usage above
`C` is refused and charged `C`; and a 429 or other refusal of a compatible host
is never treated as unbilled, so a failed sent call costs `C`. Foundry and
Anthropic accounting is unchanged.

Two trust assumptions remain:

- The pod and Service ranges of the cluster are known to the install. The dial
  guard and the egress except list treat the private, shared (100.64.0.0/10) and
  link-local ranges as in-cluster, plus the ranges declared in
  `inClusterCIDRs` (decision 3). A cluster whose pods or Services use other
  routable addresses must declare them; an undeclared range is treated as public,
  and a declared public endpoint whose DNS points into it would get the key and
  the redacted context. The analyzer does not read the ranges from Nodes: see
  Alternatives.
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
| Public DNS rebinds into the cluster or metadata | Address checks on each connection, against the private ranges and the declared in-cluster ranges; private addresses only for explicitly named Service hosts, never metadata or loopback |
| Service impersonation | Verified TLS with the Service DNS name; cluster operators own certificates and Service routing. Pod creation in the Service's namespace, or edits of the Service or its EndpointSlices, bypass it (a pod can mount the serving certificate): only cluster administrators may create pods there |
| Error or output echoes the key | Fixed errors, bounded discarded error bodies, no wrapped transport or filesystem errors; before a completion is returned, search an allow-list view of the reply (ASCII letters, digits and the key's punctuation, every other character dropped) over the raw body and over every decoded string, number and object key in document order, with nested JSON decoded; control characters are removed from model text before storage (#852), so a key whole in stored text is whole in the view; a wrong-typed decoy field fails closed with its own fixed text. A re-encoded key (base64, percent encoding, homoglyphs) is not found: the endpoint is trusted with the key |
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
- Reading the in-cluster ranges from the cluster (`spec.podCIDRs` of the Nodes,
  and the Service range) instead of declaring them. Not chosen: the analyzer's
  `list` on Nodes is granted for platform detection only (invariant 6), and using
  it to decide where a key may go widens what that grant is for; the Service
  range would need a further grant (the `ServiceCIDR` objects, where the cluster
  serves them); and a range read at
  run time changes with the cluster while the declaration is fixed at install
  and checked at start. The install declares the ranges instead, with no RBAC
  change.
- A fixed-host OpenAI provider excludes gateways and cluster servers.
- Reusing Foundry with a key mixes identity and Azure URL/version semantics.
- Refusing all private addresses excludes cluster inference; explicit Service
  declarations provide a bounded TLS and egress exception without new RBAC.

## Amendments

None.
