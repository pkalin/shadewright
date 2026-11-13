#!/usr/bin/env bash
# Installs the Helm chart (dist/chart) with the metrics TLS trust profile on the current Kubernetes context (the Kind
# cluster of the Chart workflow's chart-testing job, whose images were built from this checkout) and scrapes all three
# metrics Services the way Prometheus does, without the Prometheus Operator: from a Pod with a ServiceAccount token
# that the endpoints' TokenReview and SubjectAccessReview accept (the ServiceAccount is bound to the chart's
# metrics-reader ClusterRole), a CA file and a server name (resolved to the Service, so it is also the SNI and the
# name the certificate is checked against). The test makes a throwaway CA and one serving certificate (its SAN is the
# server name) with openssl, stores the certificate in the three Secrets the chart mounts, and checks that
#   - all three Pods become Ready (the mounted key is readable by the non-root user: mode 0444),
#   - the certificate each endpoint presents is the one the test issued (issuer = the test CA),
#   - a scrape with the right CA and server name and a valid token answers 200 with metrics,
#   - a scrape with another CA, and one with another server name, fail the TLS verification (curl exit 60),
#   - a scrape without a token is refused with 401 (the profile keeps TokenReview).
# The curl image is the one of the chart's helm test, pinned by digest in values.yaml. Usage:
#   hack/chart-metrics-tls-test.sh <kubectl> <helm> <values file>
set -euo pipefail

KUBECTL="${1:-kubectl}"
HELM="${2:-helm}"
VALUES="${3:?usage: $0 <kubectl> <helm> <values file>}"
case "${VALUES}" in /*) ;; *) VALUES="${PWD}/${VALUES}" ;; esac
NS=shadewright-mtls
CLIENT_NS=shadewright-mtls-client
REL=shadewright
SERVER_NAME=metrics.shadewright.test
WRONG_NAME=metrics.other.test
PORT=8443
CHART="$(cd "$(dirname "$0")/.." && pwd)/dist/chart"
WORK="$(mktemp -d)"

cleanup() {
  "${HELM}" uninstall "${REL}" --namespace "${NS}" --wait >/dev/null 2>&1 || true
  # crd.keep (the default) keeps the CRDs with the ownership of this release; another release cannot adopt them.
  "${KUBECTL}" delete crd -l "app.kubernetes.io/instance=${REL}" --ignore-not-found >/dev/null 2>&1 || true
  "${KUBECTL}" delete clusterrolebinding shadewright-mtls-scraper --ignore-not-found >/dev/null 2>&1 || true
  "${KUBECTL}" delete namespace "${NS}" "${CLIENT_NS}" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  rm -rf "${WORK}"
}
trap cleanup EXIT

# The curl image of the chart's helm test: repository and digest from values.yaml.
tests_image() { # key
  awk -v k="$1" '/^tests:/{t=1;next} /^[^ #]/{t=0} t && $1==k":"{print $2; exit}' "${CHART}/values.yaml"
}
IMAGE="$(tests_image repository)@$(tests_image digest)"
if ! printf '%s' "${IMAGE}" | grep -Eq '^[a-z0-9./_-]+@sha256:[a-f0-9]{64}$'; then
  echo "no pinned tests.image in values.yaml: '${IMAGE}'" >&2
  exit 1
fi

# ---- Throwaway PKI: the CA the scraper trusts, another CA it does not, one serving certificate -------------------
cd "${WORK}"
make_ca() { # name
  openssl ecparam -genkey -name prime256v1 -noout -out "$1.key" 2>/dev/null
  openssl req -x509 -sha256 -new -key "$1.key" -subj "/CN=$1" -days 1 -out "$1.crt" \
    -addext "basicConstraints=critical,CA:TRUE" -addext "keyUsage=critical,keyCertSign,cRLSign"
}
make_ca shadewright-test-ca
make_ca shadewright-wrong-ca
openssl ecparam -genkey -name prime256v1 -noout -out tls.key 2>/dev/null
openssl req -new -key tls.key -subj "/CN=${SERVER_NAME}" -out tls.csr
printf 'subjectAltName=DNS:%s\nextendedKeyUsage=serverAuth\nkeyUsage=digitalSignature\n' "${SERVER_NAME}" >ext.cnf
openssl x509 -req -sha256 -in tls.csr -CA shadewright-test-ca.crt -CAkey shadewright-test-ca.key -CAcreateserial \
  -days 1 -extfile ext.cnf -out tls.crt 2>/dev/null

# ---- Install with the profile on -------------------------------------------------------------------------------
"${KUBECTL}" create namespace "${NS}" --dry-run=client -o yaml | "${KUBECTL}" apply -f -
for c in executor analyzer verifier; do
  "${KUBECTL}" -n "${NS}" create secret tls "${c}-metrics-tls" --cert=tls.crt --key=tls.key
done
"${HELM}" install "${REL}" "${CHART}" --namespace "${NS}" -f "${VALUES}" \
  --set executor.metricsTLS.secretName=executor-metrics-tls \
  --set analyzer.metricsTLS.secretName=analyzer-metrics-tls \
  --set verifier.metricsTLS.secretName=verifier-metrics-tls \
  --wait --timeout 5m

# ---- The scraper: a ServiceAccount allowed to read /metrics, in a namespace the NetworkPolicy lets in ------------
"${KUBECTL}" apply -f - <<YAML
apiVersion: v1
kind: Namespace
metadata:
  name: ${CLIENT_NS}
  labels:
    metrics: enabled
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: scraper
  namespace: ${CLIENT_NS}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: shadewright-mtls-scraper
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: ${REL}-metrics-reader
subjects:
- kind: ServiceAccount
  name: scraper
  namespace: ${CLIENT_NS}
YAML
"${KUBECTL}" -n "${CLIENT_NS}" create configmap right-ca --from-file=ca.crt="${WORK}/shadewright-test-ca.crt"
"${KUBECTL}" -n "${CLIENT_NS}" create configmap wrong-ca --from-file=ca.crt="${WORK}/shadewright-wrong-ca.crt"
"${KUBECTL}" apply -f - <<YAML
apiVersion: v1
kind: Pod
metadata:
  name: scraper
  namespace: ${CLIENT_NS}
spec:
  serviceAccountName: scraper
  restartPolicy: Never
  securityContext:
    runAsNonRoot: true
    runAsUser: 100
    runAsGroup: 101
    seccompProfile:
      type: RuntimeDefault
  containers:
  - name: curl
    image: ${IMAGE}
    command: ["sleep", "600"]
    securityContext:
      allowPrivilegeEscalation: false
      readOnlyRootFilesystem: true
      capabilities:
        drop: ["ALL"]
    volumeMounts:
    - {name: right-ca, mountPath: /ca/right, readOnly: true}
    - {name: wrong-ca, mountPath: /ca/wrong, readOnly: true}
  volumes:
  - name: right-ca
    configMap: {name: right-ca}
  - name: wrong-ca
    configMap: {name: wrong-ca}
YAML
"${KUBECTL}" -n "${CLIENT_NS}" wait --for=condition=Ready pod/scraper --timeout=120s

fail=0
check() { # description, command...
  local what="$1"
  shift
  if "$@"; then echo "ok: ${what}"; else echo "FAIL: ${what}" >&2; fail=1; fi
}

# in_pod <service> <ca dir|insecure> <server name> <token yes|no> <curl option...>: curl in the scraper Pod to the
# Service's cluster IP, the server name resolved to it, the token read from the Pod's ServiceAccount.
in_pod() {
  local svc="$1" ca="$2" name="$3" token="$4" ip
  shift 4
  ip="$("${KUBECTL}" -n "${NS}" get service "${svc}" -o jsonpath='{.spec.clusterIP}')"
  "${KUBECTL}" -n "${CLIENT_NS}" exec scraper -- sh -c '
    ca="$1" name="$2" ip="$3" token="$4" port="$5"
    shift 5
    if [ "$ca" = insecure ]; then trust="--insecure"; else trust="--cacert /ca/$ca/ca.crt"; fi
    if [ "$token" = yes ]; then
      set -- -H "Authorization: Bearer $(cat /var/run/secrets/kubernetes.io/serviceaccount/token)" "$@"
    fi
    # shellcheck disable=SC2086
    exec curl --silent --max-time 10 $trust --resolve "$name:$port:$ip" "$@" "https://$name:$port/metrics"
  ' _ "${ca}" "${name}" "${ip}" "${token}" "${PORT}" "$@"
}

# scrape <service> <ca> <server name> <token>: "<http code> <curl exit code>".
scrape() {
  local code rc
  set +e
  code="$(in_pod "$@" --output /dev/null --write-out '%{http_code}' 2>/dev/null)"
  rc=$?
  set -e
  echo "${code} ${rc}"
}

for c in executor analyzer verifier; do
  svc="${REL}-${c}-metrics-service"

  # The endpoint serves the certificate the test issued, not the one it makes for itself.
  hello="$(in_pod "${svc}" insecure "${SERVER_NAME}" no --verbose --output /dev/null 2>&1 || true)"
  check "${c}: the presented certificate is issued by the test CA" grep -qE '^\*[[:space:]]+issuer: CN=shadewright-test-ca$' <<<"${hello}"
  check "${c}: the presented certificate is the one in the Secret" grep -qE "^\*[[:space:]]+subject: CN=${SERVER_NAME}$" <<<"${hello}"

  check "${c}: right CA and server name, valid token: 200" test "$(scrape "${svc}" right "${SERVER_NAME}" yes)" = "200 0"
  check "${c}: right CA and server name, no token: 401" test "$(scrape "${svc}" right "${SERVER_NAME}" no)" = "401 0"
  check "${c}: wrong CA: the TLS verification fails" test "$(scrape "${svc}" wrong "${SERVER_NAME}" yes)" = "000 60"
  check "${c}: wrong server name: the TLS verification fails" test "$(scrape "${svc}" right "${WRONG_NAME}" yes)" = "000 60"
  body="$(in_pod "${svc}" right "${SERVER_NAME}" yes)"
  check "${c}: the scrape returns Prometheus metrics" grep -q '^go_goroutines ' <<<"${body}"
done

exit "${fail}"
