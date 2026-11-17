/*
Copyright 2026 The Shadewright Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package redact

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// The no-over-redaction corpus of #840 (decision of 2026-11-17): ordinary Kubernetes messages (Events, kubelet
// and container runtime errors, logfmt and JSON log lines, tracebacks) that name secrets, tokens and auth in
// prose. Each comes out exactly as main redacts it, byte for byte: most of them whole, a few with the value main
// already removed ("secret: ***"). The rules of #840 may not take a word more. The first four were damaged by
// b2d4d3eb, which took a deeper next line or the text after two blanks for a flattened block.
var ordinaryMessages = []string{
	"Error: secret: db-creds not found\n  in namespace prod\n  referenced by deployment/web",
	"auth: unauthorized  registry=acr.io",
	"Traceback (most recent call last):\n  File \"/app/db.py\", line 12, in connect\n    conn = psycopg2.connect(dsn)\n" +
		"psycopg2.OperationalError: FATAL: password: authentication failed for user \"app\"\n" +
		"  hint: check the DATABASE_URL secret",
	"E1117 10:00:00.000000       1 auth.go:42] token: expired  for user system:serviceaccount:prod:web  retrying",
	`MountVolume.SetUp failed for volume "db-creds" : secret "db-creds" not found`,
	`Error: secret "web-credentials" not found`,
	"Error: couldn't find key password in Secret prod/db-creds",
	`Failed to pull image "acr.io/web:1.2": rpc error: code = Unknown desc = failed to authorize: failed to fetch oauth token: unexpected status: 401 Unauthorized`,
	"Back-off restarting failed container web in pod web-7d9f8b6c5d-x2x9k_prod(1b4c)",
	"0/3 nodes are available: 3 Insufficient memory. preemption: 0/3 nodes are available: 3 No preemption victims found for incoming pod.",
	"Readiness probe failed: HTTP probe failed with statuscode: 503",
	`Liveness probe failed: Get "http://web:8080/healthz": context deadline exceeded (Client.Timeout exceeded while awaiting headers)`,
	"Failed to create pod sandbox: rpc error: code = Unknown desc = failed to setup network for sandbox \"3f2a\": plugin type=\"azure-vnet\" failed (add)",
	"Startup probe failed: token: expired  retrying in 10s",
	`Error from server (Forbidden): secrets "db-creds" is forbidden: User "system:serviceaccount:prod:web" cannot get resource "secrets" in API group "" in the namespace "prod"`,
	"failed to sync secret cache: timed out waiting for the condition",
	"Unable to attach or mount volumes: unmounted volumes=[secret-volume], unattached volumes=[secret-volume kube-api-access-x7k2]: timed out waiting for the condition",
	`serviceaccount "web" not found; token: projected volume not ready`,
	`E1117 10:00:00.000000    1 kubelet.go:2412] "Error syncing pod, skipping" err="failed to \"StartContainer\" for \"web\" with CrashLoopBackOff: \"back-off 5m0s restarting failed container=web pod=web-0_prod(1b4c)\"" pod="prod/web-0"`,
	"W1117 10:00:00.000000    1 reflector.go:535] k8s.io/client-go/informers/factory.go:150: failed to list *v1.Secret: secrets is forbidden: User \"system:node:aks-1\" cannot list resource \"secrets\"",
	`time="2026-11-17T10:00:00Z" level=error msg="PullImage \"acr.io/web:1.2\" failed" error="failed to resolve reference \"acr.io/web:1.2\": unexpected status from HEAD request: 401 Unauthorized"`,
	`time="2026-11-17T10:00:00Z" level=error msg="auth: failed for registry acr.io" error="pull access denied, repository does not exist or may require authorization"`,
	`level=warn msg="password: authentication failed" user=app db=orders`,
	`level=info msg="token refreshed" expires_in=3600 audience=api`,
	`level=error msg="secret: not mounted" path=/etc/creds  retry=3`,
	`{"level":"error","msg":"auth: invalid credentials","user":"app"}`,
	`{"level":"warn","msg":"token: expired, retrying","attempt":3}`,
	`{"ts":"2026-11-17T10:00:00Z","msg":"config loaded","keys":["host","port"],"secretRef":"db-creds"}`,
	"panic: runtime error: invalid memory address or nil pointer dereference\n[signal SIGSEGV: segmentation violation code=0x1 addr=0x0 pc=0x4a1b2c]\n\ngoroutine 1 [running]:\nmain.loadSecret()\n\t/app/main.go:42 +0x1d",
	"java.lang.IllegalStateException: secret: missing\n\tat com.acme.Config.load(Config.java:42)\n\tat com.acme.Main.main(Main.java:10)",
	"Caused by: org.postgresql.util.PSQLException: FATAL: password authentication failed for user \"app\"\n\tat org.postgresql.core.v3.ConnectionFactoryImpl.doAuthentication(ConnectionFactoryImpl.java:693)",
	"Warning  Failed  3m  kubelet  Error: secret \"db\" not found",
	"[auth] token: refresh failed (attempt 3/5)  next try in 30s",
	"pass: 12 fail: 0  skipped: 1",
	"auth: failed for user bob  retrying in 5s",
	"sig: verification failed  key=prod-signing",
	"Error: secret:\n  name: db-creds\n  namespace: prod",
	"error validating data: ValidationError(Deployment.spec.template.spec.containers[0].env[0].valueFrom.secretKeyRef): missing required field \"key\"",
	"Error: failed to create containerd task: failed to create shim task: OCI runtime create failed: runc create failed: unable to start container process: exec: \"/app\": permission denied",
	"Pod sandbox changed, it will be killed and re-created.",
	"Container image \"acr.io/web:1.2\" already present on machine",
	"Successfully assigned prod/web-0 to aks-nodepool1-1234",
	"Scaled up replica set web-5d4f8b to 3",
	"Created container web",
	"Readiness probe failed: Get \"http://10.244.1.5:8080/ready\": dial tcp 10.244.1.5:8080: connect: connection refused",
	"Back-off pulling image \"acr.io/web:1.2\"\n  reason: ImagePullBackOff\n  message: auth: unauthorized",
	"OOMKilled: container web exceeded its memory limit (512Mi)\n  last state: terminated\n  exit code: 137",
	"ExternalSecret prod/db: could not get secret data from provider: token: permission denied  key=db/password",
	"cert-manager: Issuing certificate as Secret does not exist\n  secret: web-tls\n  issuer: letsencrypt",
	"FailedGetResourceMetric: failed to get cpu utilization: unable to get metrics for resource cpu: no metrics returned from resource metrics API",
	"helm: release web failed: secret: \"sh.helm.release.v1.web.v3\" already exists",
	"flux: kustomization prod/web: decryption failed: sops: failed to get the data key\n  hint: check the sops-age secret",
	"vault: permission denied  path=secret/data/web  token: invalid or expired",
	"WARNING: password: too short (min 12 chars)  policy=default",
	"Note: the token: header is deprecated; use Authorization",
	"keda: scaler error: auth: missing triggerAuthentication  trigger=prometheus",
	"az: AADSTS7000215: Invalid client secret provided. Ensure the secret being sent in the request is the client secret value.",
}

// ordinaryLayouts are the ways a message reaches the redactor: as written, with its line breaks as CRLF, with
// a prefix on its first line, and with its line breaks flattened to blanks (as a source may have done).
var ordinaryLayouts = []func(string) string{
	func(s string) string { return s },
	func(s string) string { return strings.ReplaceAll(s, "\n", "\r\n") },
	func(s string) string { return "Warning  Failed  12s  kubelet  " + s },
	func(s string) string { return strings.ReplaceAll(s, "\n", " ") },
	func(s string) string { return "2026-11-17T10:00:00Z stderr F " + strings.ReplaceAll(s, "\n", "\n  ") },
}

// ordinaryCorpus returns every message in every layout.
func ordinaryCorpus() []string {
	out := make([]string, 0, len(ordinaryMessages)*len(ordinaryLayouts))
	for _, m := range ordinaryMessages {
		for _, l := range ordinaryLayouts {
			out = append(out, l(m))
		}
	}
	return out
}

// ordinaryMainTable holds, for each input of the corpus in order, its hash and main's output (commit
// 0dcfec7a). It was written once by running TestWriteOrdinaryMainTable in a checkout of that commit with
// ORDINARY_MAIN_TABLE set to the output path; regenerate it only from that commit.
const ordinaryMainTable = "testdata/ordinary-main.jsonl"

// ordinaryMainRow is one line of ordinaryMainTable.
type ordinaryMainRow struct {
	Hash string `json:"h"`
	Out  string `json:"o"`
}

// TestWriteOrdinaryMainTable writes ordinaryMainTable from the redactor it runs with, when ORDINARY_MAIN_TABLE
// names the output path; otherwise it is skipped.
func TestWriteOrdinaryMainTable(t *testing.T) {
	path := os.Getenv("ORDINARY_MAIN_TABLE")
	if path == "" {
		t.Skip("ORDINARY_MAIN_TABLE is not set")
	}
	corpus := ordinaryCorpus()
	rows := make([]any, 0, len(corpus))
	for _, in := range corpus {
		rows = append(rows, ordinaryMainRow{Hash: formHash(in), Out: String(in)})
	}
	writeJSONLines(t, path, rows)
}

// Every ordinary message comes out byte for byte as main redacts it.
func TestOrdinaryMessagesRedactAsOnMain(t *testing.T) {
	gen := ordinaryCorpus()
	failed, whole := 0, 0
	n := readJSONLines(t, ordinaryMainTable, func(i int, line []byte) {
		var row ordinaryMainRow
		if err := json.Unmarshal(line, &row); err != nil {
			t.Fatalf("line %d: %v", i+1, err)
		}
		if i >= len(gen) || formHash(gen[i]) != row.Hash {
			t.Fatalf("%s line %d does not match its generator: regenerate it from the commit it names", ordinaryMainTable, i+1)
		}
		if row.Out == gen[i] {
			whole++
		}
		if got := String(gen[i]); got != row.Out {
			failed++
			if failed <= 20 {
				t.Errorf("in:   %q\nmain: %q\nnow:  %q", gen[i], row.Out, got)
			}
		}
	})
	if n != len(gen) {
		t.Fatalf("%s has %d lines, the corpus %d inputs", ordinaryMainTable, n, len(gen))
	}
	if failed > 0 {
		t.Errorf("%d of %d ordinary messages differ from main", failed, n)
	}
	if whole < n/2 {
		t.Fatalf("corpus too weak: main keeps only %d of %d messages whole", whole, n)
	}
}
