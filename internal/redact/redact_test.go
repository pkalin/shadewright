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
	"time"
	"unicode/utf8"
)

// The corpus builds every token-shaped value at run time from obviously synthetic parts, so no realistic
// secret is committed and secret scanners have nothing to match in the source.

func rep(s string, n int) string { return strings.Repeat(s, n) }

// Text that must survive after a redacted value.
const (
	nextField = "\nnext: field"
	endText   = " end"
)

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
	// tail is secret material that must not survive after an escape, punctuation, a space or a line break.
	tail  = "Remaining" + "SyntheticValue" + "42"
	tail2 = "Second" + "SyntheticValue" + "43"
	// wordTail is secret material without digits, so the long-token rule cannot catch it by accident.
	wordTail = "Remaining" + "SyntheticValue"
	// opaque is a secret value long enough for the full marker.
	opaque = "Synthetic" + "OpaqueValue42"
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
		// Regression cases from review: secret material after an escape, punctuation, a space or a newline.
		{"json escaped quote", `{"password":"prefix\"` + tail + `"}`, []string{tail}, []string{KindCredential}},
		{"yaml doubled single quote", "password: 'prefix''" + tail + "'", []string{tail}, nil},
		{"ampersand in env value", "DB_PASSWORD=prefix&" + tail, []string{tail}, nil},
		{"comma and semicolon in value", "API_TOKEN=prefix," + tail + ";" + tail2, []string{tail, tail2}, nil},
		{"quoted account key", `AccountName=synthetic;AccountKey="` + tail + `";EndpointSuffix=core.windows.net`,
			[]string{tail}, []string{KindConnectionString}},
		{"password with space in connection string", "Server=example;Password=prefix " + tail + ";Encrypt=True",
			[]string{tail}, nil},
		{"sigv4 authorization", "Authorization: AWS4-HMAC-SHA256 Credential=synthetic/scope, SignedHeaders=host, Signature=" + tail,
			[]string{tail}, []string{KindCredential}},
		{"digest authorization", `Authorization: Digest username="synthetic", realm="r", nonce="` + tail2 +
			`", uri="/", response="` + tail + `"`, []string{tail, tail2}, []string{KindCredential}},
		{"quoted authorization in json", `{"Authorization":"Digest nonce=\"` + tail2 + `\", response=\"` + tail + `\"","x":1}`,
			[]string{tail, tail2}, nil},
		{"yaml literal block", "password: |-\n  synthetic-" + tail + nextField, []string{tail}, []string{KindCredential}},
		{"yaml folded block nested", "db:\n  password: >2+ # comment\n     one-" + tail + "\n\n     two-" + tail2 + "\n  user: app",
			[]string{tail, tail2}, nil},
		{"yaml block in list item", "- token: |\n    " + tail + "\n- token: x", []string{tail}, nil},
		{"ipv6 trailing double colon", "peer 2001:db8:1:2:3:4:5:: lost", []string{"2001:db8:1:2:3:4:5"}, []string{KindIP}},
		{"ipv6 leading double colon", "peer ::1:2:3:4:5:6:7 lost", []string{"1:2:3:4:5:6:7"}, nil},
		{"ipv6 followed by colon", "dial 2001:db8::7: refused", []string{"2001:db8::7"}, nil},
		// Second review round: a label glued to an IPv6 address, and per-format value boundaries.
		{"ipv6 after source label", "source:2001:db8::1 dropped", []string{"2001:db8::1"}, []string{KindIP}},
		{"ipv6 after remote label", "remote:2001:db8:0:0:0:0:0:2 dropped", []string{"2001:db8:0:0:0:0:0:2"}, nil},
		{"ipv6 after face label", "face:2001:db8::3", []string{"2001:db8::3"}, nil},
		{"compact json reference first", `{"tokenExpiry":3600,"clientSecret":"` + opaque + `"}`, []string{opaque}, nil},
		{"compact json reference last", `{"clientSecret":"` + opaque + `","tokenExpiry":3600}`, []string{opaque}, nil},
		{"compact json unquoted secret", `{"tokenExpiry":3600,"apiKey":` + tail + `,"x":1}`, []string{tail}, nil},
		{"escaped json in a string", `"{\"tokenExpiry\":1,\"clientSecret\":\"` + opaque + `\"}"`, []string{opaque}, nil},
		{"pretty json", "{\n  \"tokenExpiry\": 3600,\n  \"clientSecret\": \"" + opaque + "\"\n}", []string{opaque}, nil},
		{"yaml flow reference first", "{tokenExpiry: 3600, clientSecret: " + opaque + "}", []string{opaque}, nil},
		{"yaml flow reference last", "{clientSecret: " + opaque + ", tokenExpiry: 3600}", []string{opaque}, nil},
		{"env reference next to secret", "tokenExpiry=3600,clientSecret=" + opaque, []string{opaque}, nil},
		{"yaml plain scalar with spaces", "token: prefix " + tail, []string{tail}, []string{KindCredential}},
		{"yaml multi-line plain scalar", "auth:\n  token: prefix\n    " + tail + "\n    " + tail2 + "\n  user: app",
			[]string{tail, tail2}, nil},
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

// Text around a redacted value must survive: the next YAML field, the next connection string field.
func TestKeepsTextAroundValues(t *testing.T) {
	for in, want := range map[string]string{
		"password: |-\n  synthetic-" + tail + nextField:              "password: |-\n  [REDACTED:credential]\nnext: field",
		"db:\n  password: |\n    a-" + tail + "\n    b\n  user: app": "db:\n  password: |\n    [REDACTED:credential]\n  user: app",
		"Server=example;Password=prefix " + tail + ";Encrypt=True":   "Server=example;Password=[REDACTED:connection-string];Encrypt=True",
		"password: |\nnext: field":                                   "password: *\nnext: field",
		`{"tokenExpiry":3600,"clientSecret":"` + opaque + `"}`:       `{"tokenExpiry":3600,"clientSecret":"[REDACTED:credential]"}`,
		`{"clientSecret":"` + opaque + `","tokenExpiry":3600}`:       `{"clientSecret":"[REDACTED:credential]","tokenExpiry":3600}`,
		"{tokenName: x, apiKey: " + opaque + "}":                     "{tokenName: x, apiKey: [REDACTED:credential]",
		"token: prefix " + tail:                                      "token: [REDACTED:credential]",
		"auth:\n  token: prefix\n    " + tail + "\n  user: app":      "auth:\n  token: [REDACTED:credential]\n  user: app",
		"credentials:   \n  name: app\n":                             "credentials:   \n  name: app\n",
		"src 2001:db8::1, ok":                                        "src [ip], ok",
		"secretName: |\n  orders-tls\n":                              "secretName: |\n  orders-tls\n",
		// Issue #114.
		`"{\"clientSecret\":\"prefix,` + tail + `\"}"`:    `"{\"clientSecret\":\"[REDACTED:credential]\"}"`,
		"password:\n  " + opaque + nextField:              "password:\n  [REDACTED:credential]\nnext: field",
		"password:  # set by CI\n  " + opaque + nextField: "password:  # set by CI\n  [REDACTED:credential]\nnext: field",
		"token:\n  \"" + opaque + "\"\nnext: field":       "token:\n  \"[REDACTED:credential]\"\nnext: field",
		"{\"password\":\n  \"" + opaque + "\"}":           "{\"password\":\n  \"[REDACTED:credential]\"}",
		"retry with Basic " + b64("u:p") + " now":         "retry with Basic *** now",
		"secretName:\n  orders-tls\n":                     "secretName:\n  orders-tls\n",
		// Second review of #189.
		"password:\n- " + opaque + "\n- " + tail + nextField:                                          "password:\n- [REDACTED:credential]\nnext: field",
		"password:\n# set by CI\n  " + opaque + nextField:                                             "password:\n# set by CI\n  [REDACTED:credential]\nnext: field",
		"credentials:\n  user: admin\n  secretRef:\n    name: db\n  value: '" + opaque + "'\nnext: x": "credentials:\n  user: ***\n  secretRef:\n    name: db\n  value: '[REDACTED:credential]'\nnext: x",
		"- name: DB_PASSWORD\n  value: " + opaque + "\n- name: MODE\n  value: debug":                  "- name: DB_PASSWORD\n  value: [REDACTED:credential]\n- name: MODE\n  value: debug",
		`{"name":"DB_PASSWORD","value":"` + opaque + `"}`:                                             `{"name":"DB_PASSWORD","value":"[REDACTED:credential]"}`,
		`"{\\\"token\\\":\\\"a,` + wordTail + `\\\"}"`:                                                `"{\\\"token\\\":\\\"[REDACTED:credential]\\\"}"`,
		"DB_PASS=" + opaque + " next":                                                                 "DB_PASS=[REDACTED:credential] next",
		"auth: " + opaque + " and auth: failed":                                                       "auth: [REDACTED:credential] and auth: failed",
	} {
		if got := String(in); got != want {
			t.Errorf("String(%q)\n got %q\nwant %q", in, got, want)
		}
	}
}

// gapCase is a regression case from the last review of #110 and from #111 (issue #114): the secrets must be
// gone and the neighbouring text kept.
type gapCase struct {
	name    string
	input   string
	secrets []string // must not appear in the output
	kept    []string // must appear in the output
}

var (
	// randomToken is a base64 token without a known prefix; hexToken a 40-character hex token.
	randomToken = b64("synthetic-random-" + "token-0123456789")
	hexToken    = rep("a1b2c3d4e5", 4)
	// otherJWT is a JWT whose payload is not a JSON object, so it does not start with "eyJ".
	otherJWT = b64(`{"alg":"HS256"}`) + "." + b64("synthetic payload") + "." + b64("not-a-signature")
)

func gapCases() []gapCase {
	return []gapCase{
		// Escaped embedded JSON: the value ends at its own escaped closing quote, not at a comma.
		{"escaped json value with comma", `"{\"clientSecret\":\"prefix,` + tail + `\"}"`,
			[]string{"prefix", tail}, []string{`"{\"clientSecret\":\"`, `\"}"`}},
		{"escaped json value with escaped quote", `"{\"clientSecret\":\"a\\\"b,` + tail + `\",\"x\":1}"`,
			[]string{tail}, []string{`\",\"x\":1}"`}},
		{"unterminated escaped json value", `"{\"clientSecret\":\"prefix,` + tail + `" next`,
			[]string{tail}, []string{`" next`}},
		// YAML value on the next line, with and without trailing spaces after the colon.
		{"yaml value on next line", "password:\n  " + opaque + nextField,
			[]string{opaque}, []string{"password:\n  ", nextField}},
		{"yaml value on next line after spaces", "db:\n  password:   \n    " + opaque + "\n    " + tail + "\n  user: app",
			[]string{opaque, tail}, []string{"db:\n  password:   \n    ", "\n  user: app"}},
		{"yaml quoted value on next line", "- token:\n    '" + opaque + "'\n- token: x",
			[]string{opaque}, []string{"- token:\n    '", "'\n- token: "}},
		// URL credentials without a password.
		{"url user and empty password", "git clone https://" + opaque + ":@github.com/org/repo failed",
			[]string{opaque}, []string{"git clone ", " failed"}},
		{"url user only", "fetch https://" + opaque + "@git-server:3000/org/repo: 403",
			[]string{opaque}, []string{"fetch ", " 403"}},
		// Short Basic and Bearer credentials.
		{"short basic", "header Basic " + b64("u:p") + " rejected", []string{b64("u:p")}, []string{"header Basic ", " rejected"}},
		{"short padded basic", "header Basic " + base64.StdEncoding.EncodeToString([]byte("ab:c")) + " rejected",
			[]string{base64.StdEncoding.EncodeToString([]byte("ab:c"))}, []string{"header Basic ", " rejected"}},
		{"bearer opaque token", "with Bearer " + rep("x9", 5) + ", retry", []string{rep("x9", 5)}, []string{"with Bearer ", ", retry"}},
		// Whitespace, including newlines, on both sides of the colon in JSON and YAML flow mappings.
		{"json newline after colon", "{\"password\":\n  \"" + opaque + "\"}",
			[]string{opaque}, []string{"{\"password\":\n  \"", "\"}"}},
		{"json spaces around colon", `{"x":1, "password" : "` + opaque + `"}`, []string{opaque}, []string{`{"x":1, "password" : "`}},
		{"yaml flow newline before colon", "{token\n: " + opaque + ", next: field}", []string{opaque}, []string{", next: field}"}},
		// From #111: long random tokens without a known prefix.
		{"base64 token without prefix", "api returned 401 for " + randomToken + " today",
			[]string{randomToken}, []string{"api returned 401 for ", " today"}},
		{"hex token", "using key " + hexToken + ".", []string{hexToken}, []string{"using key ", "."}},
		// From #111: short key names.
		{"pass key", "login pass=" + opaque + " user=app", []string{opaque}, []string{"login pass=", " user=app"}},
		{"auth key", "auth=" + opaque + " ok", []string{opaque}, []string{" ok"}},
		{"auth yaml key", "auth: " + opaque + nextField, []string{opaque}, []string{nextField}},
		{"sig key outside query", "webhook rejected with sig: " + opaque + endText, []string{opaque},
			[]string{"webhook rejected with sig: ", endText}},
		// From #111: a JWT whose payload does not start with "eyJ".
		{"jwt with other payload", "token " + otherJWT + " expired", []string{otherJWT}, []string{" expired"}},
		// Second review of #189: YAML layouts the next-line rule missed.
		{"compact sequence at key indent", "password:\n- " + opaque + nextField,
			[]string{opaque}, []string{"password:\n- ", nextField}},
		{"comment before next-line value", "password:\n# set by CI\n  " + opaque + nextField,
			[]string{opaque}, []string{nextField}},
		// Short keys as the last segment of an env or snake key.
		{"db pass env", "DB_PASS=" + opaque + " next", []string{opaque}, []string{"DB_PASS=", " next"}},
		{"mysql root pass yaml", "MYSQL_ROOT_PASS: " + opaque + nextField, []string{opaque}, []string{nextField}},
		{"basic auth key", "basic_auth: " + opaque + nextField, []string{opaque}, []string{nextField}},
		{"bare auth with secret-shaped value", "auth: " + opaque + nextField, []string{opaque}, []string{nextField}},
		{"quoted bare auth key", `{"auth": "failed for ` + tail + `"}`, []string{tail}, nil},
		// base64url tokens hold "-".
		{"base64url token with hyphens", "token rejected: " + base64URLToken + endText,
			[]string{"Synth3ticAbC", "dEf0123XyZ98", "76QwErTy54321"}, []string{"token rejected: ", endText}},
		{"hex token after a hyphen", "id key-" + hexToken + endText, []string{hexToken}, []string{endText}},
		// Escaped embedded JSON, pretty-printed and escaped twice.
		{"pretty escaped json", `"{\n  \"clientSecret\": \"` + opaque + `\"\n}"`, []string{opaque}, []string{`\"\n}"`}},
		{"tab escaped json", `"{\t\"token\" : \"` + opaque + `\"}"`, []string{opaque}, nil},
		{"double escaped json", `"{\\\"clientSecret\\\":\\\"prefix,` + wordTail + `\\\",\\\"x\\\":1}"`,
			[]string{wordTail}, []string{`\\\",\\\"x\\\":1}"`}},
		// Generic children of a secret key, and the Kubernetes env forms.
		{"children of a secret mapping", "credentials:\n  user: admin\n  value: " + opaque + nextField,
			[]string{opaque, "admin"}, []string{"credentials:\n  user: ", nextField}},
		{"children of a secret sequence", "tokens:\n  - id: 1\n    value: '" + opaque + "'\n  - " + tail + nextField,
			[]string{opaque, tail}, []string{nextField}},
		{"kubernetes env yaml", "env:\n- name: DB_PASSWORD\n  value: " + opaque + "\n- name: LOG_LEVEL\n  value: debug",
			[]string{opaque}, []string{"- name: DB_PASSWORD\n  value: ", "- name: LOG_LEVEL\n  value: debug"}},
		{"kubernetes env yaml quoted and indented", "    env:\n      - name: \"API_TOKEN\"\n        value: \"" + opaque + "\"\n",
			[]string{opaque}, []string{"        value: \""}},
		{"kubernetes env json", `[{"name":"API_TOKEN","value":"` + opaque + `"},{"name":"MODE","value":"debug"}]`,
			[]string{opaque}, []string{`{"name":"MODE","value":"debug"}`}},
		{"kubernetes env yaml flow", "env: [{name: DB_PASSWORD, value: " + opaque + "}]", []string{opaque}, nil},
		// Review of #192: a secret key nested below a kept reference child or a bare short key, block
		// values of a bare short key, a quoted passphrase after one, a value starting with "#" after "="
		// and a token after an image digest.
		{"secret key below a reference child", "credentials:\n  secretRef:\n    name: db-creds\n    token:\n      - " + opaque + nextField,
			[]string{opaque}, []string{"secretRef:\n    name: db-creds\n    token:\n      - ", nextField}},
		{"secret key below a reference child at the end", "credentials:\n  secretRef:\n    token:\n      - abc123secret\n",
			[]string{"abc123secret"}, []string{"secretRef:\n    token:\n      - "}},
		{"secret keys below nested reference children", "credentials:\n  secretRef:\n    token:\n      keyRef:\n        password:\n          " +
			opaque + "\n      user: admin\n  user: root" + nextField,
			[]string{opaque, "admin", "root"}, []string{"keyRef:\n        password:\n          ", nextField}},
		{"bare pass block scalar", "pass: |\n  s3cr3t!pw99\n", []string{"s3cr3t!pw99"}, []string{"pass: |\n  "}},
		{"bare auth sequence", "auth:\n  - tok3n!value99\n", []string{"tok3n!value99"}, []string{"auth:\n  - "}},
		{"bare auth mapping", "auth:\n  mode: oidc\n  key: k3y!" + opaque + nextField,
			[]string{opaque}, []string{"mode: oidc", nextField}},
		{"secret key under a bare auth", "auth:\n  mode: oidc\n  password:\n    - hunter2" + nextField,
			[]string{"hunter2"}, []string{"mode: oidc", "password:\n    - ", nextField}},
		{"quoted passphrase after bare pass", `pass: "correct horse battery staple"` + nextField,
			[]string{"correct horse battery staple"}, []string{`pass: "`, `"` + nextField}},
		{"comment-like value after an equals sign", "API_TOKEN= #" + opaque + "\n  indented: line",
			[]string{opaque}, []string{"\n  indented: line"}},
		{"token after an image digest", "image x@sha256:" + rep("a1", 32) + "-" + randomToken + endText,
			[]string{randomToken}, []string{"sha256:" + rep("a1", 32) + "-", endText}},
		// Issue #194: what stays redacted next to the prose that is now kept.
		{"allow-listed hyphenated short key with a secret value", "pre-auth: " + opaque + nextField, []string{opaque}, []string{nextField}},
		{"allow-listed hyphenated short key with a secret block", "two-pass:\n  " + opaque + nextField, []string{opaque}, []string{nextField}},
		{"x header short key", "x-auth: " + wordTail + nextField, []string{wordTail}, []string{nextField}},
		{"capitalised allow-listed prefix", "Pre-Auth: " + wordTail + nextField, []string{wordTail}, []string{nextField}},
		{"capitalised hyphenated short key", "Proxy-Auth: " + wordTail + nextField, []string{wordTail}, []string{nextField}},
		{"hyphenated key with a secret word", "token-auth: yes" + nextField, []string{"yes"}, []string{nextField}},
		{"dotted short key", "app.auth: " + wordTail + nextField, []string{wordTail}, []string{nextField}},
		{"dotted allow-listed prefix", "app.pre-auth: " + wordTail + nextField, []string{wordTail}, []string{nextField}},
		{"short secret before a comma", "auth: hunter2," + endText, []string{"hunter2"}, []string{endText}},
		{"symbol inside a word", "auth: unauth!orized" + endText, []string{"unauth!orized"}, []string{endText}},
		// Basic and Bearer stay strict by decision (#194): any value of minSchemeWord bytes or more counts,
		// so a long word after them is redacted (a known over-redaction), and a full stop after a short value
		// of letters is a symbol there, not punctuation.
		{"long word after basic", "basic internationalization" + endText, []string{"internationalization"}, []string{"basic ", endText}},
		{"long lower-case letters after bearer", "bearer " + rep("opaque", 3) + endText, []string{rep("opaque", 3)}, []string{endText}},
		{"short letters and a full stop after bearer", "Bearer abcdefghij." + endText, []string{"abcdefghij"}, []string{"Bearer ", endText}},
	}
}

// base64URLToken is a base64url token whose hyphens cut it into parts shorter than the long-token minimum.
var base64URLToken = "Synth3ticAbC-" + "dEf0123XyZ98-" + "76QwErTy54321-Zz"

func TestGapCases(t *testing.T) {
	checkGapCases(t, gapCases())
}

// redactedOnMainCases are values that the redactor removed before #194 and that a review of the first
// changes for #194 found passing through. Whatever prose they resemble, they must stay redacted: a kebab-case
// key that names what a credential is for, letters outside ASCII, and a letter-only value with one trailing
// punctuation mark. The last ones include prose ("auth: déjàvuxx", "auth: unauthorized,") that cannot be told
// apart from a password: over-redaction kept by decision (#194, threat model G4.4).
func redactedOnMainCases() []gapCase {
	const shortPassword, letterToken, auth = "hunter2", "letteronlytoken", "auth: "
	values := []struct{ key, value string }{
		// Kebab-case keys ending in a short key, outside the prose prefixes.
		{"db-pass: ", "supersecret"},
		{"db-pass: ", shortPassword},
		{"admin-pass: ", "s3cr3t"},
		{"signing-key-pass: ", "correcthorse"},
		{"db-pass:\n  ", shortPassword},
		{"pre-x-auth: ", "secretvalue"},
		{"proxy-auth: ", letterToken},
		{"api-auth: ", letterToken},
		{"two-x-pass: ", letterToken},
		// Letters outside ASCII count as symbols.
		{auth, "motdepassé"},
		{auth, "Passwörter"},
		{auth, "contraseña"},
		{auth, "déjàvuxx"},
		// One trailing punctuation mark counts as a symbol.
		{auth, "Passwort."},
		{auth, "Sunshine:"},
		{auth, "abcdefgh;"},
		{auth, "unauthorized,"},
	}
	cases := make([]gapCase, 0, len(values))
	for _, c := range values {
		in := c.key + c.value
		cases = append(cases, gapCase{"redacted on main " + in, in + nextField,
			[]string{strings.TrimRight(c.value, ",.;:")}, []string{nextField}})
	}
	return cases
}

// Differential check against main before #194: every input it redacted stays redacted.
func TestStillRedactsWhatMainRedacted(t *testing.T) {
	checkGapCases(t, redactedOnMainCases())
}

func checkGapCases(t *testing.T, cases []gapCase) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := String(c.input)
			for _, s := range c.secrets {
				if strings.Contains(out, s) {
					t.Errorf("output still holds %q:\n%s", s, out)
				}
			}
			for _, k := range c.kept {
				if !strings.Contains(out, k) {
					t.Errorf("output lost %q:\n%s", k, out)
				}
			}
			if len(out) > len(c.input) {
				t.Errorf("output grew from %d to %d bytes", len(c.input), len(out))
			}
		})
	}
}

// The new rules keep text that only resembles a secret: empty values, nested mappings, short paths, names
// made of words, image digests and keys that merely contain a short secret word.
func TestGapRulesKeepUsefulText(t *testing.T) {
	for _, in := range []string{
		"password:\nnext: field",
		"password:   \n",
		"credentials:\n  name: app",
		"credentials:\n  - name: app\n    secretRef:\n      name: db-creds\n      key: password",
		"auth:\n  {name: app}",
		"- name: DB_PASSWORD_FILE\n  value: /etc/secret/pw",
		"- name: LOG_LEVEL\n  value: debug",
		// Bare short keys followed by a colon keep diagnostic text.
		"pass: 12 fail: 0",
		"auth: failed for user bob",
		"sig: invalid",
		"auth:\n  mode: oidc",
		// Ordinary prose after Basic and Bearer.
		"failed basic health check",
		"missing Bearer token",
		"Bearer authentication failed",
		"Basic realm required",
		// Hyphenated names, UUIDs and image names are not tokens.
		"pod orders-worker-7d9f8b6c5-x2x9q-and-a-long-suffix-0123 restarted",
		"uid 550e8400-e29b-41d4-a716-446655440000 deleted",
		"image registry.example.com/team/orders-worker-service-v2:1.2.3-rc1",
		"kube-system-controller-manager-node-0123456789 ready",
		"author: someone",
		"bypass=on",
		"signal: SIGTERM",
		"oauth=enabled",
		"/var/lib/kubelet/pods/volumes/kubernetes.io~secret/tls",
		"ReplicaSetControllerManagerDeploymentName",
		"image ghcr.io/example/orders@sha512:" + rep("ab12", 32),
		// Review of #192.
		"image ghcr.io/example/orders@sha256:" + rep("a1", 32) + "-amd64",
		"credentials: # set by CI\n  secretRef:\n    name: db-creds\n",
		"auth:\n  - failed\n",
		"pass: |\n  12 of 12\n",
		`auth: "failed"`,
		"Basic\nnext",
		`{"x":1}`,
		// Issue #194: a short key after a prose word and a hyphen. Letter-only values with accents or a
		// trailing punctuation mark, and a long word after Basic or Bearer, stay redacted by decision
		// (TestStillRedactsWhatMainRedacted, TestGapCases).
		"pre-auth: ok",
		"two-pass: true",
		"multi-sig: enabled",
		"post-auth: failed for user bob",
		"re-auth: required",
		"single-pass:\n  12 of 12\n",
		// Scripts without ASCII letters are not judged by letters ("auth: विफलहुआ", combining marks).
		"auth: विफलहुआ",
		"auth: 认证失败。",
	} {
		if out := String(in); out != in {
			t.Errorf("changed useful text:\n in: %q\nout: %q", in, out)
		}
	}
}

// skipLarge skips the timing tests on large inputs in short mode and under the race detector, whose
// instrumentation makes these single-goroutine scans 10 to 20 times slower than the time limit assumes. make test
// runs this package a second time without -race so that they still run.
func skipLarge(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("large inputs")
	}
	if raceDetector {
		t.Skip("timing test: runs without -race")
	}
}

// Repeating the shapes the scanning rules look for must cost linear time: each value is scanned once and
// keys inside a value already redacted are skipped. Each input takes about a second; a quadratic scan of
// them takes minutes.
func TestRepeatedPatternsStayLinear(t *testing.T) {
	skipLarge(t)
	const size = 1 << 20
	for name, unit := range map[string]string{
		"yaml empty headers":       "password:\n",
		"yaml headers over values": "password:\n  password:\n",
		"yaml blank lines":         "password:\n\n\n\n",
		"escaped json values":      `,\"password\":\"x`,
		"escaped json backslashes": `,\"token\":\"\\\\`,
		"flow newlines":            "{password\n:\n",
		"long token parts":         rep("ab12", 6) + "/",
		"userinfo":                 "https://a@",
		"env entries":              "- name: DB_PASSWORD\n",
		"secret mapping children":  "credentials:\n  a: b\n  ref:\n    c: d\n",
		"escape runs":              `{\"token\":\"\\\\\\\"`,
		"hyphen runs":              rep("Ab1-", 8) + "/",
		"comment headers":          "credentials: # c\n  a: b\n",
		"bare key children":        "auth:\n  password:\n    x\n",
		"references over headers":  "credentials:\n  secretRef:\n    token:\n      - x\n",
	} {
		in := rep(unit, size/len(unit))
		start := time.Now()
		out := String(in)
		if d := time.Since(start); d > 10*time.Second {
			t.Errorf("%s: %d bytes took %s", name, len(in), d)
		}
		if len(out) > len(in) {
			t.Errorf("%s: output grew from %d to %d bytes", name, len(in), len(out))
		}
	}
}

// Nested entries whose lines all belong to the first one: each match reads a bounded part of them.
func TestNestedBlocksStayLinear(t *testing.T) {
	skipLarge(t)
	for name, line := range map[string]func(indent string) string{
		"env entries":   func(in string) string { return in + "- name: DB_PASSWORD\n" },
		"empty headers": func(in string) string { return in + "password:\n" },
		"references":    func(in string) string { return in + "secretRef:\n" },
		// Each secret header sits below a reference child of the one before.
		"headers below references": func(in string) string { return in + in + "token:\n" + in + in + " secretRef:\n" },
		"comment headers":          func(in string) string { return in + "token: # c\n" },
	} {
		var b strings.Builder
		for i := 0; b.Len() < 1<<20; i++ {
			b.WriteString(line(strings.Repeat(" ", i)))
		}
		in := b.String()
		start := time.Now()
		out := String(in)
		if d := time.Since(start); d > 10*time.Second {
			t.Errorf("%s: %d bytes took %s", name, len(in), d)
		}
		if len(out) > len(in) {
			t.Errorf("%s: output grew from %d to %d bytes", name, len(in), len(out))
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
	for _, c := range gapCases() {
		f.Add(c.input)
	}
	for _, s := range []string{
		"", "\x00", "\xff\xfe", "::", ":::::::::", "-----BEGIN PRIVATE KEY-----", "a@b.c", "password=",
		"eyJ.eyJ.", "postgres://:@", "authorization: ", "?sig=&key=", "1.1.1.1.1.1", "[REDACTED:ip]",
		"password: |\n", "a:\n password: >-\n  x\n password: |\n   y", "Authorization: \"\\", "1:2:3:4:5:6:7:8:",
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

// A Flux revision is kept only when it is the whole value (IsFluxRevision); inside text, the commit hash is
// redacted like any other long hex run, so "sha1:" written in front of a secret keeps nothing.
func TestFluxRevisions(t *testing.T) {
	const commit = "1f3c5e7a9b2d4f6081a3c5e7f9b1d3f5a7c9e1b3"
	for _, in := range []string{
		"main@sha1:" + commit,
		"refs/heads/main@sha1:" + commit,
		"cluster/kind@sha1:" + commit,
		"sha1:" + commit,
	} {
		if !IsFluxRevision(in) {
			t.Errorf("IsFluxRevision(%q) = false", in)
		}
	}
	for _, in := range []string{
		"",
		commit,
		"main@sha1:" + commit + "a",
		"main@sha1:" + strings.ToUpper(commit),
		"main@sha1:" + commit + " failed",
		"token main@sha1:" + commit,
		"main@sha1:" + commit + "-" + randomToken,
		"main@sha1:" + commit + "\n",
		"ma in@sha1:" + commit,
		"main@@sha1:" + commit,
		"main@sha256:" + commit,
		strings.Repeat("a", 256) + "@sha1:" + commit,
	} {
		if IsFluxRevision(in) {
			t.Errorf("IsFluxRevision(%q) = true", in)
		}
	}
	checkGapCases(t, []gapCase{
		{name: "sha1 prefix written before a secret", input: "Bearer sha1:" + commit, secrets: []string{commit}},
		{name: "revision inside a message", input: "Applied revision: main@sha1:" + commit, secrets: []string{commit}},
		{name: "at sign and prefix before a secret", input: "x@sha1:" + commit + " end", secrets: []string{commit}},
	})
}

// StringKeeping keeps only whole occurrences of the verified strings; everything else is redacted as String
// redacts it (#264).
func TestStringKeeping(t *testing.T) {
	name := "incident-" + rep("9f86d081", 8)
	hexToken := rep("3f1c9a7b", 5)
	in := "change for incident `" + name + "`.\nincident `" + name + "`, path apps/" + hexToken + "/x.yaml"
	want := "change for incident `" + name + "`.\nincident `" + name + "`, path [REDACTED:token].yaml"
	if out := StringKeeping(in, name); out != want {
		t.Errorf("StringKeeping:\n got: %s\nwant: %s", out, want)
	}
	if out := String(in); strings.Contains(out, name) {
		t.Errorf("String keeps the name, so the test proves nothing:\n%s", out)
	}
	for _, s := range []string{
		"x" + name,
		name + ".example",
		"a." + name,
		name + "-" + rep("ab12", 6),
		name + "0",
		"incident-" + hexToken,
	} {
		in := "incident `" + s + "` end"
		if out := StringKeeping(in, name, ""); !strings.Contains(out, "[REDACTED:token]") || strings.Contains(out, name) {
			t.Errorf("kept a string that is not a whole verified occurrence:\n in: %s\nout: %s", in, out)
		}
	}
	if out, want := StringKeeping("token "+hexToken, ""), String("token "+hexToken); out != want {
		t.Errorf("an empty keep changed the result: %q, want %q", out, want)
	}
}
