# Install and qualify an OpenAI-compatible endpoint

The adapter supports Chat Completions and Responses. Servers differ in schema,
tool and usage support. Qualify the exact model, server version and API with the
[evaluation harness](../evaluation.md#openai-compatible-models-and-servers) before
using it in a lab. A successful HTTP request is not qualification.
[ADR-0042](../adr/0042-openai-compatible-model-endpoints.md) defines the provider boundary.

## Configuration

For the OpenAI API, create an inference key outside Git and provision it into an
existing Secret named `compatible-model-key` in the operator namespace. The chart
never contains or creates the key. Save these values as `compatible-values.yaml`, alongside the normal image,
cluster ID and watched-namespace values of the [install guide](../install.md):

```yaml
analyzer:
  analysis:
    disableFoundry: true
    triageModel: gpt-6.1-sol
    openAICompatible:
      endpoints:
        - url: https://api.openai.com/v1
          inferenceAPI: Responses
          apiKey:
            secretName: compatible-model-key
            secretKey: api-key
```

Edit `RemediationSettings/default` with an authorized cluster-admin workflow:

```yaml
spec:
  analysis:
    models:
      triage:
        provider: OpenAICompatible
        endpoint: https://api.openai.com/v1
        deployment: gpt-6.1-sol
        inferenceAPI: Responses
        maxTokensPerDay: 500000
```

`triageModel` must be the model ID replies actually name, including any snapshot
suffix. Set it from a recorded response before qualification. `deployment` is the
server's request model name. Do not set `apiVersion`. The API base includes `/v1`;
this adapter adds only `/responses` or `/chat/completions`.

`openAICompatible.endpoints` declares complete bases and their APIs. The Foundry
`endpointSuffixes` list never authorizes a compatible endpoint. Each optional key
belongs to its endpoint only. Two endpoints can have different keys, even when
one gateway routes accounts by path. Settings must match a parsed declared base
and API; another path, port or host is refused before a call. Duplicate bases,
provider host overlap and malformed URLs fail chart rendering and startup.

For a keyless Service use its verified HTTPS base, for example
`https://model.inference.svc.cluster.local/v1`, and name its destination pods:

```yaml
analyzer:
  analysis:
    disableFoundry: true
    triageModel: local-model
    openAICompatible:
      endpoints:
        - url: https://model.inference.svc.cluster.local/v1
          inferenceAPI: Responses
          service:
            namespace: inference
            podSelector:
              app: model
            port: 8443
```

The Service exposes TLS on port 443 and routes it to the selected pods on target
port 8443 in this example. The chart renders one egress rule per entry, with both
the namespace and pod selectors in the same peer. Cilium CIDR rules may exclude
in-cluster pods, so the public HTTPS rule alone is insufficient. Verify the
Service selector and target port against your deployment.

Leave `apiKey` empty for no Authorization header. Provision a serving certificate
trusted by the analyzer's system roots and valid for that Service DNS name.
Certificates without a trusted issuer are refused. The chart adds no custom trust
volume: use an operator-built analyzer image with the internal CA in its system
root bundle when the Service uses a private CA.
The operator owns certificate distribution, server authentication and ingress
policy. HTTP, non-443 ports, IP literals and metadata/loopback addresses remain
refused. No environment proxy or redirect is used.

## Kind installation with a dummy key

Use an isolated Kind cluster with the locally built analyzer, executor and verifier
images loaded into it. The image build and Kind node image must already be
available when working offline. This exercise proves installation and the mount,
not live inference. Never give the dummy key access to a real account.

```bash
app_version="$(awk '/^appVersion:/ {gsub(/"/, "", $2); print $2}' dist/chart/Chart.yaml)"
make docker-build-images \
  IMG_ANALYZER="shadewright-analyzer:${app_version}" \
  IMG_EXECUTOR="shadewright-executor:${app_version}" \
  IMG_VERIFIER="shadewright-verifier:${app_version}"
kind create cluster --name compatible-provider
kind load docker-image "shadewright-analyzer:${app_version}" \
  "shadewright-executor:${app_version}" "shadewright-verifier:${app_version}" \
  --name compatible-provider
kubectl --context kind-compatible-provider create namespace demo
kubectl --context kind-compatible-provider create namespace shadewright-system
kubectl --context kind-compatible-provider -n shadewright-system create secret generic \
  compatible-model-key --from-literal=api-key=FAKE-KIND-KEY
```

Use the [source-checkout image options](../install.md) to render all three local
images and the compatible values above. Set an explicit `clusterId` and disable
image pulls when the images are loaded. Keep `settings.spec.analysis.models`
unset during this dummy-key installation: the analyzer can start with its triage
flag while no deployment is selected, so no inference request is made. A key
source requires a triage model at chart render time.

Save this as `local-images.yaml` (the chart uses its `appVersion` tag):

```yaml
image:
  allowTag: true
clusterId: compatible-provider
executor:
  image:
    repository: shadewright-executor
    pullPolicy: Never
analyzer:
  watchNamespaces: [demo]
  image:
    repository: shadewright-analyzer
    pullPolicy: Never
verifier:
  watchNamespaces: [demo]
  image:
    repository: shadewright-verifier
    pullPolicy: Never
```

```bash
helm template shadewright dist/chart -n shadewright-system \
  -f local-images.yaml -f compatible-values.yaml > compatible-install.yaml
helm install shadewright dist/chart --kube-context kind-compatible-provider \
  -n shadewright-system -f local-images.yaml -f compatible-values.yaml \
  --wait --timeout 5m
kubectl --context kind-compatible-provider -n shadewright-system rollout status \
  deployment/shadewright-analyzer --timeout=180s
kubectl --context kind-compatible-provider -n shadewright-system get deployment \
  shadewright-analyzer -o yaml
```

Confirm the compatible key mount is whole and read-only, only the analyzer gets
it, `--disable-foundry=true` is set, the endpoint declaration contains the exact base and its own key path and no Azure
workload-identity label is present. The chart tests compare all RBAC objects
before and after a key source and require them to be identical. Update the dummy
Secret to exercise projected-file refresh; real rotation follows
[identity and rotation](../identity-rotation.md#openai-compatible-credential).
Clean up with `kind delete cluster --name compatible-provider`.

## Qualify before enabling analysis

Run both ordinary scenario evaluation and `EVAL_FORCE_ESCALATION=1` with Responses
on a gpt-6.x model. Retain each report and manifest, the exact model ID, endpoint
hash and server version. Inspect refused-output and unknown-usage counts as well
as time, tokens and cost per passed run. A server that ignores tools or schemas
must fail closed; do not loosen validators to make it pass.
