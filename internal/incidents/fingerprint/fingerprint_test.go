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

package fingerprint

import (
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

// crdPattern is the Incident spec.fingerprint pattern.
var crdPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

const (
	reasonBackOff = "BackOff"
	deploymentUID = "4f1c2b9e-0d6a-4c1e-9b7d-2a5e8f3c6d10"
	podAUID       = "0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d"
	podBUID       = "9f8e7d6c-5b4a-4392-8170-6f5e4d3c2b1a"
)

func base() Observation {
	return Observation{
		ClusterID:           "lab-weu-1",
		Namespace:           "shop",
		OwnerUID:            deploymentUID,
		ResourceUID:         podAUID,
		Reason:              "FailedMount",
		Type:                "Warning",
		ReportingController: "kubelet",
		Note:                `MountVolume.SetUp failed for volume "creds" : secret "db-creds" not found`,
	}
}

func with(f func(*Observation)) Observation {
	o := base()
	f(&o)
	return o
}

func TestFingerprint(t *testing.T) {
	tests := []struct {
		name string
		a, b Observation
		same bool
	}{
		{
			name: "two pods of the same Deployment with the same error",
			a: with(func(o *Observation) {
				o.ResourceUID = podAUID
				o.Reason = reasonBackOff
				o.Note = `Back-off 10s restarting failed container app in pod web-7d4b9c8f6-q8wzt_shop(` + podAUID + `)`
			}),
			b: with(func(o *Observation) {
				o.ResourceUID = podBUID
				o.Reason = reasonBackOff
				o.Note = `Back-off 5m0s restarting failed container app in pod web-6c9f7b5d48-xk2lp_shop(` + podBUID + `)`
			}),
			same: true,
		},
		{
			name: "different missing Secrets",
			a:    base(),
			b: with(func(o *Observation) {
				o.Note = `MountVolume.SetUp failed for volume "creds" : secret "api-token" not found`
			}),
			same: false,
		},
		{
			name: "different missing ConfigMaps",
			a:    with(func(o *Observation) { o.Note = `configmap "app-config" not found` }),
			b:    with(func(o *Observation) { o.Note = `configmap "feature-flags" not found` }),
			same: false,
		},
		{
			name: "timestamp change",
			a: with(func(o *Observation) {
				o.Note = `Readiness probe failed at 2026-11-02T10:15:30Z: Get "http://10.244.1.17:8080/healthz": context deadline exceeded after 1.5s`
			}),
			b: with(func(o *Observation) {
				o.Note = `Readiness probe failed at 2026-11-03T22:01:02.123+01:00: Get "http://10.244.3.9:8080/healthz": context deadline exceeded after 2s`
			}),
			same: true,
		},
		{
			name: "different images",
			a:    with(func(o *Observation) { o.Note = `Failed to pull image "ghcr.io/acme/web:1.4.2": not found` }),
			b:    with(func(o *Observation) { o.Note = `Failed to pull image "ghcr.io/acme/api:1.4.2": not found` }),
			same: false,
		},
		{
			name: "different error codes",
			a:    with(func(o *Observation) { o.Note = `container app exited with code 137` }),
			b:    with(func(o *Observation) { o.Note = `container app exited with code 1` }),
			same: false,
		},
		{
			name: "different dependency kinds",
			a:    with(func(o *Observation) { o.Note = `dial tcp 10.0.0.5:5432: connect: connection refused (postgres)` }),
			b:    with(func(o *Observation) { o.Note = `dial tcp 10.0.0.5:6379: connect: connection refused (redis)` }),
			same: false,
		},
		{
			name: "different owners",
			a:    base(),
			b:    with(func(o *Observation) { o.OwnerUID = "11111111-2222-4333-8444-555555555555" }),
			same: false,
		},
		{
			name: "pods without an owner fall back to their own UID",
			a:    with(func(o *Observation) { o.OwnerUID = "" }),
			b:    with(func(o *Observation) { o.OwnerUID = ""; o.ResourceUID = podBUID }),
			same: false,
		},
		{
			name: "different clusters",
			a:    base(),
			b:    with(func(o *Observation) { o.ClusterID = "lab-weu-2" }),
			same: false,
		},
		{
			name: "different namespaces",
			a:    base(),
			b:    with(func(o *Observation) { o.Namespace = "shop-staging" }),
			same: false,
		},
		{
			name: "different reasons",
			a:    base(),
			b:    with(func(o *Observation) { o.Reason = "FailedAttachVolume" }),
			same: false,
		},
		{
			name: "different actions",
			a:    base(),
			b:    with(func(o *Observation) { o.Action = "Mounting" }),
			same: false,
		},
		{
			name: "different types",
			a:    base(),
			b:    with(func(o *Observation) { o.Type = "Normal" }),
			same: false,
		},
		{
			name: "different reporting controllers",
			a:    base(),
			b:    with(func(o *Observation) { o.ReportingController = "csi-driver" }),
			same: false,
		},
		{
			name: "field boundaries are not ambiguous",
			a:    with(func(o *Observation) { o.Reason = "Back"; o.Action = "Off" }),
			b:    with(func(o *Observation) { o.Reason = reasonBackOff; o.Action = "" }),
			same: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fa, fb := Fingerprint(tt.a), Fingerprint(tt.b)
			for _, f := range []string{fa, fb} {
				if !crdPattern.MatchString(f) {
					t.Fatalf("fingerprint %q does not match the CRD pattern", f)
				}
			}
			if (fa == fb) != tt.same {
				t.Errorf("same = %v, want %v\n a: %q\n b: %q", fa == fb, tt.same, Normalize(tt.a), Normalize(tt.b))
			}
			if Fingerprint(tt.a) != fa {
				t.Error("fingerprint is not deterministic")
			}
		})
	}
}

func TestNormalize(t *testing.T) {
	tests := []struct {
		name, note, want string
	}{
		{
			name: "pod name and ReplicaSet hash",
			note: `Created pod: web-7d4b9c8f6-q8wzt in ReplicaSet web-7d4b9c8f6`,
			want: `Created pod: web-<pod> in ReplicaSet web-<hash>`,
		},
		{
			name: "DaemonSet and Job pod suffixes",
			note: `pod kube-proxy-x2x9k and job pod backup-28394817-zb5wq failed`,
			want: `pod kube-proxy-<pod> and job pod backup-28394817-<pod> failed`,
		},
		{
			name: "StatefulSet ordinal is kept",
			note: `pod postgres-0 is not ready`,
			want: `pod postgres-0 is not ready`,
		},
		{
			name: "timestamps",
			note: `last seen 2026-11-02T10:15:30.123456Z, started 2026-11-02 09:00:00 +0000 UTC, at 14:03:07, epoch 1793612130`,
			want: `last seen <time>, started <time>, at <time>, epoch <time>`,
		},
		{
			name: "durations",
			note: `Back-off 5m0s restarting failed container, timeout 250ms, retry in 30 seconds`,
			want: `Back-off <duration> restarting failed container, timeout <duration>, retry in <duration>`,
		},
		{
			name: "IPs",
			note: `dial tcp 10.96.0.10:53: i/o timeout; from [fd00::1]:8080 and 2001:db8:0:0:0:0:2:1`,
			want: `dial tcp <ip>: i/o timeout; from <ip> and <ip>`,
		},
		{
			name: "UIDs and digests",
			note: `pod uid 0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d image ghcr.io/acme/web@sha256:` + strings.Repeat("ab", 32),
			want: `pod uid <uid> image ghcr.io/acme/web@sha256:<digest>`,
		},
		{
			name: "image name, error code and missing Secret are kept",
			note: `Failed to pull image "myregistry.azurecr.io/platform/payments2service/api:v1.2.3": rpc error: code = NotFound; secret "acr-pull" not found`,
			want: `Failed to pull image "myregistry.azurecr.io/platform/payments2service/api:v1.2.3": rpc error: code = NotFound; secret "acr-pull" not found`,
		},
		{
			name: "OOMKilled exit code",
			note: `Container app was OOMKilled, exit code 137`,
			want: `Container app was OOMKilled, exit code 137`,
		},
		{
			name: "whitespace and control characters",
			note: "line one\n\tline\x00two\u200b  end ",
			want: `line one line two end`,
		},
		{
			name: "input angle brackets cannot forge placeholders",
			note: `value <redacted> and <pod>`,
			want: `value (redacted) and (pod)`,
		},
		{
			name: "invalid UTF-8",
			note: "bad \xff\xfe byte",
			want: `bad ? byte`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Normalize(Observation{Note: tt.note}); got != tt.want {
				t.Errorf("Normalize()\n got: %q\nwant: %q", got, tt.want)
			}
		})
	}
}

func TestNormalizeRedacts(t *testing.T) {
	tests := []struct {
		name, note, secret string
	}{
		{"connection string URL", `cannot connect to postgres://app:Hunter2pass@db.shop.svc:5432/orders`, "Hunter2pass"},
		{"Azure connection string", `DefaultEndpointsProtocol=https;AccountName=acme;AccountKey=bWFkZXVwa2V5MTIzNDU2Nzg5MA==;EndpointSuffix=core.windows.net`, "bWFkZXVwa2V5MTIzNDU2Nzg5MA=="},
		{"password key", `login failed: password=s3cr3t!`, "s3cr3t!"},
		{"quoted password", `{"password": "opensesame"}`, "opensesame"},
		{"bearer header", `Authorization: Bearer abc.def.ghi`, "abc.def.ghi"},
		{"bare bearer", `sent bearer QWxhZGRpbjpvcGVu`, "QWxhZGRpbjpvcGVu"},
		{"JWT", `token eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0In0.c2lnbmF0dXJl rejected`, "eyJhbGciOiJIUzI1NiJ9"},
		{"GitHub token", `using ghp_` + strings.Repeat("A1", 18), "ghp_"},
		{"AWS key", `key AKIAIOSFODNN7EXAMPLE denied`, "AKIAIOSFODNN7EXAMPLE"},
		{"Slack token", `xoxb-123456789012-abcdefghij`, "xoxb-123456789012"},
		{"long random token", `api returned 401 for Zm9vYmFyYmF6cXV4MTIzNDU2Nzg5MGFiY2RlZg`, "Zm9vYmFyYmF6cXV4MTIzNDU2Nzg5MGFiY2RlZg"},
		{"hex token", `token mismatch ` + strings.Repeat("0f", 20), strings.Repeat("0f", 20)},
		{"private key", "-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA\n-----END RSA PRIVATE KEY-----", "MIIEowIBAAKCAQEA"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Normalize(Observation{Note: tt.note})
			if strings.Contains(got, tt.secret) {
				t.Errorf("secret %q kept in %q", tt.secret, got)
			}
			if !strings.Contains(got, placeholderRedacted) {
				t.Errorf("no redaction marker in %q", got)
			}
			if containsSecret(got) {
				t.Errorf("output %q still matches a redaction rule", got)
			}
		})
	}
}

func TestNormalizeLength(t *testing.T) {
	notes := []string{
		strings.Repeat("word ", 1000),
		strings.Repeat("ż", 2000),
		strings.Repeat("x", maxInputLength*2),
		strings.Repeat("a ", 600) + "password=" + strings.Repeat("p", 2000),
		strings.Repeat("1s ", 2000),
	}
	for i, note := range notes {
		got := Normalize(Observation{Note: note})
		if len(got) > MaxSymptomLength {
			t.Errorf("note %d: length %d > %d", i, len(got), MaxSymptomLength)
		}
		if !utf8.ValidString(got) {
			t.Errorf("note %d: invalid UTF-8", i)
		}
		if containsSecret(got) {
			t.Errorf("note %d: output matches a redaction rule", i)
		}
	}
}

func TestTruncateCanCreateMatch(t *testing.T) {
	// A word that becomes a token only when cut must still be redacted.
	got := finish(strings.Repeat("a ", 500) + "AKIA" + strings.Repeat("B", 40))
	if containsSecret(got) || len(got) > MaxSymptomLength {
		t.Errorf("finish() = %q", got)
	}
}

const canary = "ghp_C4n4ryC4n4ryC4n4ryC4n4ryC4n4ryC4n4"

func FuzzNormalize(f *testing.F) {
	seeds := [][2]string{
		{"", ""},
		{`secret "db-creds" not found`, `in pod web-7d4b9c8f6-q8wzt`},
		{"password=", "token:"},
		{"-----BEGIN PRIVATE KEY-----", ""},
		{"postgres://u:p@h", "2026-11-02T10:15:30Z 10.0.0.1:80 5m0s"},
		{"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.", "\xff\x00<redacted>"},
		{strings.Repeat("A1", 600), strings.Repeat("ż ", 600)},
	}
	for _, s := range seeds {
		f.Add(s[0], s[1])
	}
	f.Fuzz(func(t *testing.T, prefix, suffix string) {
		for _, note := range []string{prefix + suffix, prefix + " " + canary + " " + suffix} {
			o := Observation{Note: note, Reason: prefix, ReportingController: suffix}
			got := Normalize(o)
			if len(got) > MaxSymptomLength {
				t.Fatalf("length %d > %d", len(got), MaxSymptomLength)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("invalid UTF-8: %q", got)
			}
			if containsSecret(got) {
				t.Fatalf("output matches a redaction rule: %q", got)
			}
			if strings.Contains(got, canary) {
				t.Fatalf("canary token kept: %q", got)
			}
			if fp := Fingerprint(o); !crdPattern.MatchString(fp) {
				t.Fatalf("fingerprint %q does not match the CRD pattern", fp)
			}
		}
	})
}
