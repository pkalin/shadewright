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
	"encoding/base64"
	"strings"
	"testing"
	"unicode/utf8"
)

// The corpus builds every token-shaped value at run time from obviously synthetic parts, so no realistic
// secret is committed and secret scanners have nothing to match in the source.

func rep(s string, n int) string { return strings.Repeat(s, n) }

func b64(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

var (
	ghToken     = "gh" + "p_" + rep("Synth3tic", 4)
	ghFineGrain = "github" + "_pat_" + rep("Fake0", 6) + "_" + rep("x9", 15)
	glToken     = "gl" + "pat-" + rep("Fak3", 5)
	awsKeyID    = "AK" + "IA" + rep("EXAMPLE0", 2)
	slackToken  = "xo" + "xb-" + "000000000-" + rep("FAKE", 4)
	googleKey   = "AI" + "za" + rep("SyntheticKey", 3)[:35]
	stripeKey   = "sk" + "_live_" + rep("FakeStripe0", 2)
	modelKey    = "sk" + "-proj-" + rep("NotARealKey_", 3)
	npmToken    = "np" + "m_" + rep("Synthetic0", 4)[:36]
	vaultToken  = "hv" + "s." + rep("FakeVaultTok", 2)
	jwtToken    = b64(`{"alg":"HS256","typ":"JWT"}`) + "." + b64(`{"sub":"synthetic-user","iat":0}`) + "." +
		b64("not-a-real-signature-at-all")
	pemBody    = rep("U3ludGhldGljS2V5TWF0ZXJpYWw=", 3)
	pemKey     = "-----BEGIN " + "RSA PRIVATE KEY-----\n" + pemBody + "\n" + pemBody + "\n-----END " + "RSA PRIVATE KEY-----"
	ecKey      = "-----BEGIN " + "EC PRIVATE KEY-----\\n" + pemBody + "\\n-----END " + "EC PRIVATE KEY-----"
	opensshKey = "-----BEGIN " + "OPENSSH PRIVATE KEY-----\n" + pemBody + "\n"
	storageKey = rep("RmFrZUFjY291bnRLZXk=", 4)
	sasSig     = "Fake" + rep("Sig0", 8) + "%3D"
	dbPassword = "Synthetic-Pa55-" + rep("x", 6)
)

type corpusCase struct {
	name    string
	input   string
	secrets []string // must not appear in the output
	kinds   []string // marker kinds that must appear
}

func corpus() []corpusCase {
	return []corpusCase{
		{"github token in log", "fatal: Authentication failed using " + ghToken + " for push",
			[]string{ghToken}, []string{KindToken}},
		{"github fine-grained token", "GH_TOKEN=" + ghFineGrain, []string{ghFineGrain}, nil},
		{"gitlab token", "token " + glToken + " rejected", []string{glToken}, []string{KindToken}},
		{"aws access key id", "AWS_ACCESS_KEY_ID " + awsKeyID, []string{awsKeyID}, []string{KindToken}},
		{"slack token", "posting with " + slackToken, []string{slackToken}, []string{KindToken}},
		{"google api key", `{"apiKeyValue":"x","k":"` + googleKey + `"}`, []string{googleKey}, []string{KindToken}},
		{"stripe key", "charge failed, key " + stripeKey, []string{stripeKey}, []string{KindToken}},
		{"model provider key", "OPENAI_API_KEY " + modelKey, []string{modelKey}, []string{KindToken}},
		{"npm token", "//registry.npmjs.org/:_authToken " + npmToken, []string{npmToken}, []string{KindToken}},
		{"vault token", "X-Vault-Token header was " + vaultToken, []string{vaultToken}, []string{KindToken}},
		{"jwt in log", "request with token " + jwtToken + " expired", []string{jwtToken}, []string{KindJWT}},
		{"bearer header", "Authorization: Bearer " + jwtToken, []string{jwtToken}, []string{KindCredential}},
		{"bearer elsewhere", "curl -H 'X-Auth: Bearer " + rep("opaque", 4) + "'", []string{rep("opaque", 4)},
			[]string{KindCredential}},
		{"basic auth header", `"authorization": "Basic ` + b64("user:"+dbPassword) + `"`,
			[]string{b64("user:" + dbPassword)}, []string{KindCredential}},
		{"pem private key", "key material:\n" + pemKey + "\nend", []string{pemBody}, []string{KindPrivateKey}},
		{"pem key in json string", `{"tls.key":"` + ecKey + `"}`, []string{pemBody}, []string{KindPrivateKey}},
		{"truncated pem key", "log line cut: " + opensshKey, []string{pemBody}, []string{KindPrivateKey}},
		{"postgres url", "dial postgres://orders:" + dbPassword + "@db.internal:5432/orders?sslmode=require",
			[]string{dbPassword}, []string{KindConnectionString}},
		{"redis url with empty user", "REDIS_URL=redis://:" + dbPassword + "@cache:6379/0",
			[]string{dbPassword}, []string{KindConnectionString}},
		{"azure storage connection string",
			"DefaultEndpointsProtocol=https;AccountName=synthetic;AccountKey=" + storageKey + ";EndpointSuffix=core.windows.net",
			[]string{storageKey}, []string{KindConnectionString}},
		{"service bus connection string",
			"Endpoint=sb://synthetic.servicebus.windows.net/;SharedAccessKeyName=Root;SharedAccessKey=" + storageKey,
			[]string{storageKey}, []string{KindConnectionString}},
		{"sql server connection string", "Server=tcp:db,1433;User ID=app;Password=" + dbPassword + ";Encrypt=True",
			[]string{dbPassword}, []string{KindConnectionString}},
		{"sas url", "GET https://synthetic.blob.core.windows.net/c/b?sv=2022-11-02&sp=r&sig=" + sasSig + " 403",
			[]string{sasSig}, []string{KindCredential}},
		{"yaml password", "env:\n  DB_PASSWORD: " + dbPassword + "\n", []string{dbPassword}, []string{KindCredential}},
		{"json client secret", `{"clientSecret": "` + dbPassword + ` with spaces"}`,
			[]string{dbPassword}, []string{KindCredential}},
		{"env api key", "export SERVICE_API_KEY='" + dbPassword + "'", []string{dbPassword}, []string{KindCredential}},
		{"logfmt token", "level=error msg=denied token=" + dbPassword + " user=x", []string{dbPassword},
			[]string{KindCredential}},
		{"email", "notify on-call: synthetic.person+alerts@example.com failed", []string{"synthetic.person"},
			[]string{KindEmail}},
		{"ipv4", "connection refused from 203.0.113.42:443", []string{"203.0.113.42"}, []string{KindIP}},
		{"ipv6", "dial tcp [2001:db8::1]:443: i/o timeout", []string{"2001:db8::1"}, []string{KindIP}},
		{"ipv6 full", "peer fe80:0000:0000:0000:0202:b3ff:fe1e:8329 lost", []string{"fe1e:8329"}, []string{KindIP}},
		{"several in one line",
			"user synthetic@example.org from 198.51.100.7 used " + ghToken + " and " + jwtToken,
			[]string{"synthetic@example.org", "198.51.100.7", ghToken, jwtToken},
			[]string{KindEmail, KindIP, KindToken, KindJWT}},
	}
}

func TestCorpusIsRedacted(t *testing.T) {
	for _, c := range corpus() {
		t.Run(c.name, func(t *testing.T) {
			out := String(c.input)
			for _, s := range c.secrets {
				if strings.Contains(out, s) {
					t.Errorf("output still holds %q:\n%s", s, out)
				}
			}
			for _, k := range c.kinds {
				if !strings.Contains(out, "[REDACTED:"+k+"]") && !strings.Contains(out, "["+k+"]") {
					t.Errorf("output has no %s marker:\n%s", k, out)
				}
			}
			if len(out) > len(c.input) {
				t.Errorf("output grew from %d to %d bytes", len(c.input), len(out))
			}
		})
	}
}

// Text a diagnosis needs must survive: names of missing Secrets, image references, times and pod names.
func TestKeepsUsefulText(t *testing.T) {
	for _, in := range []string{
		`Error: secret "db-creds" not found`,
		`MountVolume.SetUp failed for volume "tls" : secret "orders-tls" not found`,
		"secretName: orders-tls",
		"secretKeyRef:\n  name: db-creds\n  key: password",
		`"tokenSecretRef": "registry-token"`,
		"passwordFile: /etc/app/password",
		"Back-off pulling image \"ghcr.io/example/orders@sha256:" + rep("ab", 32) + "\"",
		"2026-11-02T10:15:30Z 12:30:45 pod orders-worker-7d9f8b6c5-x2x9q OOMKilled",
		"password: \"\"",
		"Failed to get token for service account: no credentials",
	} {
		if out := String(in); out != in {
			t.Errorf("changed useful text:\n in: %s\nout: %s", in, out)
		}
	}
}

func TestShortMatchesDoNotGrow(t *testing.T) {
	for in, want := range map[string]string{
		"pwd=ab":                   "pwd=**",
		"pwd=abcd":                 "pwd=***",
		"password=0123456789":      "password=[REDACTED]",
		"ip 1.2.3.4 end":           "ip [ip] end",
		"token=" + rep("z", 30):    "token=[REDACTED:credential]",
		"a@b.co":                   "***",
		"secret=0123456789ab":      "secret=[credential]",
		"from 192.168.100.200 now": "from [REDACTED:ip] now",
	} {
		if got := String(in); got != want {
			t.Errorf("String(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestOverlappingMatchesMerge(t *testing.T) {
	in := "DATABASE_URL=postgres://app:" + dbPassword + "@10.0.0.5:5432/db"
	got := String(in)
	want := "DATABASE_URL=[REDACTED:connection-string]"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestExtraPatterns(t *testing.T) {
	r, err := New(
		Pattern{Kind: "employee-id", Expr: `\bEMP-[0-9]{6}\b`},
		Pattern{Kind: "ticket-secret", Expr: `ticket=([A-Z]{8})`},
		Pattern{Kind: "empty", Expr: `x*`},
	)
	if err != nil {
		t.Fatal(err)
	}
	got := r.String("owner EMP-123456 ticket=ABCDEFGH, ip 203.0.113.9")
	want := "owner [REDACTED] ticket=***, ip [ip]"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got := String("owner EMP-123456"); got != "owner EMP-123456" {
		t.Errorf("default redactor used an extra pattern: %q", got)
	}
}

func TestExtraPatternErrors(t *testing.T) {
	for _, p := range []Pattern{
		{Kind: "", Expr: "a"},
		{Kind: "Upper", Expr: "a"},
		{Kind: "bad]kind", Expr: "a"},
		{Kind: rep("a", 33), Expr: "a"},
		{Kind: "ok", Expr: "("},
	} {
		if _, err := New(p); err == nil {
			t.Errorf("New(%+v) succeeded, want an error", p)
		}
	}
}

func FuzzString(f *testing.F) {
	for _, c := range corpus() {
		f.Add(c.input)
	}
	for _, s := range []string{
		"", "\x00", "\xff\xfe", "::", ":::::::::", "-----BEGIN PRIVATE KEY-----", "a@b.c", "password=",
		"eyJ.eyJ.", "postgres://:@", "authorization: ", "?sig=&key=", "1.1.1.1.1.1", "[REDACTED:ip]",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		out := String(in)
		if len(out) > len(in) {
			t.Fatalf("output grew from %d to %d bytes", len(in), len(out))
		}
		if utf8.ValidString(in) && !utf8.ValidString(out) {
			t.Fatalf("valid UTF-8 input gave invalid output %q", out)
		}
		if again := String(in); again != out {
			t.Fatalf("not deterministic: %q then %q", out, again)
		}
		if twice := String(out); len(twice) > len(out) {
			t.Fatalf("second pass grew from %d to %d bytes", len(out), len(twice))
		}
	})
}
