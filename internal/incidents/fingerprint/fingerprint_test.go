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
	kindPod       = "Pod"
	tail          = "SyntheticTail"
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
			name: "missing Secrets with generated-looking names",
			a:    with(func(o *Observation) { o.Note = `secret "db-7d4b9" not found` }),
			b:    with(func(o *Observation) { o.Note = `secret "db-6c9f7" not found` }),
			same: false,
		},
		{
			name: "images with generated-looking names",
			a:    with(func(o *Observation) { o.Note = `Failed to pull image "ghcr.io/acme/web-7d4b9:1.0"` }),
			b:    with(func(o *Observation) { o.Note = `Failed to pull image "ghcr.io/acme/web-6c9f7:1.0"` }),
			same: false,
		},
		{
			name: "bracketed IPv6 addresses with a port",
			a:    with(func(o *Observation) { o.Note = `dial tcp [2001:db8:1::1]:8080: connect: connection refused` }),
			b:    with(func(o *Observation) { o.Note = `dial tcp [2001:db8::1]:8080: connect: connection refused` }),
			same: true,
		},
		{
			name: "IPv4 addresses of different lengths",
			a:    with(func(o *Observation) { o.Note = `dial tcp 10.0.0.1:5432: connection refused` }),
			b:    with(func(o *Observation) { o.Note = `dial tcp 100.100.100.100:5432: connection refused` }),
			same: true,
		},
		{
			name: "redacted secrets of different lengths",
			a:    with(func(o *Observation) { o.Note = `login failed: password=ab` }),
			b:    with(func(o *Observation) { o.Note = `login failed: password=a-much-longer-password-value` }),
			same: true,
		},
		{
			name: "regarding Pod name stripped",
			a: with(func(o *Observation) {
				o.RegardingKind, o.RegardingName = kindPod, "web-7d4b9c8f6-q8wzt"
				o.Note = `Liveness probe of web-7d4b9c8f6-q8wzt failed`
			}),
			b: with(func(o *Observation) {
				o.RegardingKind, o.RegardingName = kindPod, "web-6c9f7b5d48-xk2lp"
				o.Note = `Liveness probe of web-6c9f7b5d48-xk2lp failed`
			}),
			same: true,
		},
		// Last review round.
		{
			name: "five-character segments inside a pod name are not suffixes",
			a:    with(func(o *Observation) { o.Note = `pod web-q8wzt-debug failed` }),
			b:    with(func(o *Observation) { o.Note = `pod web-xk2lp-debug failed` }),
			same: false,
		},
		{
			name: "duration-looking Secret names",
			a:    with(func(o *Observation) { o.Note = `secret "db-10s" not found` }),
			b:    with(func(o *Observation) { o.Note = `secret "db-20s" not found` }),
			same: false,
		},
		{
			name: "regarding Pod name inside a longer ConfigMap name",
			a: with(func(o *Observation) {
				o.RegardingKind, o.RegardingName = kindPod, "web-q8wzt"
				o.Note = `configmap "settings-web-q8wzt" not found`
			}),
			b: with(func(o *Observation) {
				o.RegardingKind, o.RegardingName = kindPod, "web-x2x9k"
				o.Note = `configmap "settings-web-x2x9k" not found`
			}),
			same: false,
		},
		{
			name: "compressed and expanded IPv6 loopback",
			a:    with(func(o *Observation) { o.Note = `dial tcp [::1]:5432: connect: connection refused` }),
			b:    with(func(o *Observation) { o.Note = `dial tcp [0:0:0:0:0:0:0:1]:5432: connect: connection refused` }),
			same: true,
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
			name: "compressed IPv6 in brackets is one placeholder",
			note: `from [2001:db8:1::1]:8080 and [2001:db8::1]:8080`,
			want: `from <ip> and <ip>`,
		},
		{
			name: "generated names outside Pod and ReplicaSet references are kept",
			note: `secret "db-7d4b9" not found; configmap "app-x2x9k" not found; image "acme/web-6c9f7b5d48:1.0"`,
			want: `secret "db-7d4b9" not found; configmap "app-x2x9k" not found; image "acme/web-6c9f7b5d48:1.0"`,
		},
		{
			name: "Pod and ReplicaSet reference forms",
			note: `pod/web-7d4b9c8f6-q8wzt pod="shop/web-7d4b9c8f6-q8wzt" Pod "api-x2x9k" Scaled up replica set web-7d4b9c8f6 to 2`,
			want: `pod/web-<pod> pod="shop/web-<pod>" Pod "api-<pod>" Scaled up replica set web-<hash> to 2`,
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
			note: `value <redacted> and <pod> and [REDACTED:ip] and ***`,
			want: `value (redacted) and (pod) and (REDACTED:ip) and ###`,
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

// TestSymptomKeepsMilliQuantities: a milli quantity after a key ("averageValue=6500m", #479) is evidence the
// Incident and the model must see, so Symptom keeps it, while Normalize still replaces it and the fingerprint
// stays the same when the value changes. Real durations and other quantities are as in Normalize.
func TestSymptomKeepsMilliQuantities(t *testing.T) {
	tests := []struct {
		name, note, symptom, normalized string
	}{
		{
			name:       "KEDA metric value",
			note:       `current averageValue=6500m above target averageValue=50`,
			symptom:    `current averageValue=6500m above target averageValue=50`,
			normalized: `current averageValue=<duration> above target averageValue=50`,
		},
		{
			name:       "after a colon, with and without a space",
			note:       `cpu: 250m, limit:1.5m; Evidence: status.currentMetrics.0.external.current.averageValue=212500m`,
			symptom:    `cpu: 250m, limit:1.5m; Evidence: status.currentMetrics.0.external.current.averageValue=212500m`,
			normalized: `cpu: <duration>, limit:<duration>; Evidence: status.currentMetrics.0.external.current.averageValue=<duration>`,
		},
		{
			name:       "real durations",
			note:       `Back-off 5m0s restarting, timeout=250ms, wait=1m30s, period: 10s, retry in 30 seconds, after 5m`,
			symptom:    `Back-off <duration> restarting, timeout=<duration>, wait=<duration>, period: <duration>, retry in <duration>, after <duration>`,
			normalized: `Back-off <duration> restarting, timeout=<duration>, wait=<duration>, period: <duration>, retry in <duration>, after <duration>`,
		},
		{
			name:       "milli quantity without a key, or glued to a name",
			note:       `value 6500m, = 5m, db-10m`,
			symptom:    `value <duration>, = <duration>, db-10m`,
			normalized: `value <duration>, = <duration>, db-10m`,
		},
		{
			name:       "binary and decimal suffixes",
			note:       `memory=512Mi limit: 2Gi rate=100k size=1G value=6500m`,
			symptom:    `memory=512Mi limit: 2Gi rate=100k size=1G value=6500m`,
			normalized: `memory=512Mi limit: 2Gi rate=100k size=1G value=<duration>`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := Observation{Note: tt.note}
			if got := Symptom(o); got != tt.symptom {
				t.Errorf("Symptom()\n got: %q\nwant: %q", got, tt.symptom)
			}
			if got := Normalize(o); got != tt.normalized {
				t.Errorf("Normalize()\n got: %q\nwant: %q", got, tt.normalized)
			}
		})
	}
	a := Observation{ClusterID: "c", Namespace: "ns", OwnerUID: "u", Reason: "R", Note: "averageValue=6500m"}
	b := a
	b.Note = "averageValue=212500m"
	if Fingerprint(a) != Fingerprint(b) {
		t.Errorf("a changed metric value changed the fingerprint")
	}
}

// TestValueKeepsWholeMilliQuantity: an evidence value is the value of its field, so a value that is a milli
// quantity as a whole is kept, and anything else is as in Symptom.
func TestValueKeepsWholeMilliQuantity(t *testing.T) {
	for in, want := range map[string]string{
		"212500m":           "212500m",
		" 6500m ":           "6500m",
		"1.5m":              "1.5m",
		"50":                "50",
		"512Mi":             "512Mi",
		"5m0s":              "<duration>",
		"250ms":             "<duration>",
		"waited 5m":         "waited <duration>",
		"averageValue=500m": "averageValue=500m",
	} {
		if got := Value(in); got != want {
			t.Errorf("Value(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeRedacts(t *testing.T) {
	tests := []struct {
		name, note, secret string
	}{
		{"connection string URL", `cannot connect to postgres://app:Hunter2pass@db.shop.svc:5432/orders`, "Hunter2pass"},
		{"Azure connection string", `DefaultEndpointsProtocol=https;AccountName=acme;AccountKey=bWFkZXVwa2V5MTIzNDU2Nzg5MA==;EndpointSuffix=core.windows.net`, "bWFkZXVwa2V5MTIzNDU2Nzg5MA=="},
		{"password key", `login failed: password=s3cr3t!`, "s3cr3t!"},
		{"short password", `login failed: password=ab`, "=ab"},
		{"bearer header", `Authorization: Bearer abc.def.ghi`, "abc.def.ghi"},
		{"bare bearer", `sent bearer QWxhZGRpbjpvcGVu`, "QWxhZGRpbjpvcGVu"},
		{"JWT", `token eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0In0.c2lnbmF0dXJl rejected`, "eyJhbGciOiJIUzI1NiJ9"},
		{"GitHub token", `using ghp_` + strings.Repeat("A1", 18), "ghp_"},
		{"private key", "-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA\n-----END RSA PRIVATE KEY-----", "MIIEowIBAAKCAQEA"},
		// Review regressions: redaction runs before UID substitution, so a UID cannot shield a secret.
		{"UID-prefixed password", `password=12345678-1234-1234-1234-123456789abc-swordfish`, "swordfish"},
		{"UID in URL user info", `dial postgres://12345678-1234-1234-1234-123456789abc:swordfish@db:5432/orders`, "swordfish"},
		// Last review round: YAML layout survives until redaction, and long notes are not cut first.
		{"nested YAML block scalar", "db:\n  password: |-\n    " + tail + "\n  user: app", tail},
		{"YAML plain continuation", "auth:\n  token: prefix\n    " + tail + "\n  user: app", tail},
		{"quoted value past the old 64 KiB cut", `{"password": "prefix,` + tail + ` ` + strings.Repeat("word ", 14000) + `"}`, tail},
		{"note over the input limit", `{"password": "prefix,` + tail + ` ` + strings.Repeat("word ", maxInputLength/5) + `"}`, tail},
		// Review regressions: complete quoted values and underscore-prefixed keys.
		{"quoted value with spaces", `{"password": "correct horse battery staple"}`, "horse battery staple"},
		{"quoted value with a comma", `{"password": "sword,fish"}`, ",fish"},
		{"underscore-prefixed password key", `DB_PASSWORD=opensesame`, "opensesame"},
		{"underscore-prefixed client secret key", `AZURE_CLIENT_SECRET=opensesame`, "opensesame"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Normalize(Observation{Note: tt.note})
			if strings.Contains(got, tt.secret) {
				t.Errorf("secret %q kept in %q", tt.secret, got)
			}
			if !strings.Contains(got, placeholderRedacted) {
				t.Errorf("no redaction placeholder in %q", got)
			}
			if unredacted(got) {
				t.Errorf("output %q still changes when redacted", got)
			}
		})
	}
}

// unredacted reports whether redacting s again would change it.
func unredacted(s string) bool {
	return redactText(s) != s
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
		if unredacted(got) {
			t.Errorf("note %d: output changes when redacted", i)
		}
	}
}

func TestFinishRedactsAfterCut(t *testing.T) {
	got := finish(strings.Repeat("a ", 500) + "password=" + strings.Repeat("B", 40))
	if unredacted(got) || len(got) > MaxSymptomLength {
		t.Errorf("finish() = %q", got)
	}
}

var canary = "ghp_" + strings.Repeat("C4n4ry", 7)

func FuzzNormalize(f *testing.F) {
	seeds := [][2]string{
		{"", ""},
		{`secret "db-creds" not found`, `in pod web-7d4b9c8f6-q8wzt`},
		{"password=", "token:"},
		{"-----BEGIN PRIVATE KEY-----", ""},
		{"postgres://u:p@h", "2026-11-02T10:15:30Z 10.0.0.1:80 5m0s"},
		{"averageValue=6500m cpu: 250m", " timeout=5m0s 512Mi"},
		{"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.", "\xff\x00<redacted>"},
		{strings.Repeat("A1", 600), strings.Repeat("ż ", 600)},
	}
	for _, s := range seeds {
		f.Add(s[0], s[1])
	}
	f.Fuzz(func(t *testing.T, prefix, suffix string) {
		for _, note := range []string{prefix + suffix, prefix + " " + canary + " " + suffix} {
			o := Observation{Note: note, Reason: prefix, ReportingController: suffix}
			for _, got := range []string{Normalize(o), Symptom(o), Value(note)} {
				if len(got) > MaxSymptomLength {
					t.Fatalf("length %d > %d", len(got), MaxSymptomLength)
				}
				if !utf8.ValidString(got) {
					t.Fatalf("invalid UTF-8: %q", got)
				}
				if unredacted(got) {
					t.Fatalf("output changes when redacted: %q", got)
				}
				if strings.Contains(got, canary) {
					t.Fatalf("canary token kept: %q", got)
				}
			}
			if fp := Fingerprint(o); !crdPattern.MatchString(fp) {
				t.Fatalf("fingerprint %q does not match the CRD pattern", fp)
			}
		}
	})
}
