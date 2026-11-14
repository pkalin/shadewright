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
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// mainRedactionTable is the differential table of #585: inputs of every key-name form (name=value, name:
// value, JSON and escaped JSON fields, flags with "--", "-" and "=", a name and a blank, env files, Kubernetes
// env entries in block and flow form, YAML blocks and block scalars, query strings, argument lists and flow
// sequences), with every secret word and every reference suffix (also "stdin", "audience", "key_id" and
// "for_audience") spelled with "_", "-" and in camelCase, and a value that the long-token rule keeps
// ("horse battery staple" or a known-leak key). Each line holds the input, the output of main before #585
// (commit f09d6392) and the byte ranges main redacted. It was generated once by a throwaway test run in a
// checkout of that commit, which wrote String and the merged spans of each input; regenerate it only from
// that commit.
const mainRedactionTable = "testdata/main-redaction.jsonl"

// mainRedaction is one line of mainRedactionTable.
type mainRedaction struct {
	In    string   `json:"in"`
	Out   string   `json:"out"`
	Spans [][2]int `json:"spans"`
}

// Everything main redacted before #585 is still redacted: each byte range main replaced lies inside one
// range the redactor replaces now. A rule change that narrows a value (such as a reference suffix applied to
// a form it does not belong to) fails here. It guards against regressions and is expected to pass on main.
func TestStillRedactsWhatMainRedactedForKeyNames(t *testing.T) {
	f, err := os.Open(mainRedactionTable)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<16), 1<<20)
	cases, redacted := 0, 0
	for sc.Scan() {
		var c mainRedaction
		if err := json.Unmarshal(sc.Bytes(), &c); err != nil {
			t.Fatalf("line %d: %v", cases+1, err)
		}
		cases++
		if len(c.Spans) > 0 {
			redacted++
		}
		now := defaultRedactor.spans(c.In)
		for _, want := range c.Spans {
			if !coveredBy(want, now) {
				t.Errorf("main redacted %q, now kept:\n in: %q\nmain: %q\n now: %q",
					c.In[want[0]:want[1]], c.In, c.Out, String(c.In))
			}
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if cases < 1000 || redacted < 500 {
		t.Fatalf("table too small: %d cases, %d redacted on main", cases, redacted)
	}
}

// coveredBy reports whether the byte range r lies inside one of the spans.
func coveredBy(r [2]int, spans []span) bool {
	for _, s := range spans {
		if s.start <= r[0] && r[1] <= s.end {
			return true
		}
	}
	return false
}

// leakKey is a known-leak key (knownLeakKeys) that the long-token rule keeps whole with no key name before
// it, so only the key name can catch it.
func leakKey() string { return seededKey(39781) }

// checkRedacted fails when the output of in still holds any of the secrets, or lost any of the kept parts.
func checkRedacted(t *testing.T, in string, secrets, kept []string) {
	t.Helper()
	out := String(in)
	for _, s := range secrets {
		if strings.Contains(out, s) {
			t.Errorf("value leaks %q:\n in: %q\nout: %q", s, in, out)
		}
	}
	for _, k := range kept {
		if !strings.Contains(out, k) {
			t.Errorf("lost %q:\n in: %q\nout: %q", k, in, out)
		}
	}
}

// "stdin" and "audience" mark a reference only in a flag: as an env name, a key or a JSON field the name
// holds a secret, as on main (#585). It guards against regressions and is expected to pass on main.
func TestFlagOnlyReferenceSuffixes(t *testing.T) {
	k := leakKey()
	for _, in := range []string{
		"env:\n  CLIENT_SECRET_FOR_AUDIENCE=" + k + "\n  REGION=x",
		"PASSWORD_STDIN=" + k + " next=x",
		"export PASSWORD_STDIN=" + k,
		"client_secret_for_audience: " + k,
		`{"passwordStdin": "` + k + `"}`,
		"env:\n- name: PASSWORD_STDIN\n  value: " + k,
		`{"name":"TOKEN_AUDIENCE","value":"` + k + `"}`,
	} {
		checkRedacted(t, in, []string{k, k[:12], k[len(k)-12:]}, nil)
	}
	for _, in := range []string{
		"docker login --password-stdin -u bob",
		"kubectl create token sa --token-audience api",
		"-password-stdin -u bob",
	} {
		if out := String(in); out != in {
			t.Errorf("changed a reference flag:\n in: %q\nout: %q", in, out)
		}
	}
}

// "sas" counts as "--sas", "--source-sas", "storageSas", "StorageSas", "STORAGE_SAS", "sas=" and "sas: ", and as a
// quoted key, never right after "/" or "." and never as the tag of an image named sas (#585).
func TestSasKeyOnlyBeforeItsSeparator(t *testing.T) {
	for _, in := range []string{
		`Failed to pull image "ghcr.io/acme/sas:v1.2.3beta"`,
		"image: ghcr.io/acme/sas:v1.2.3beta",
		"pulled acme.sas:v2beta1 and storageSas:x",
		"opts sas:v1 /mnt/sas=v2",
		"GET /sas: Ab3xyz789q",
		"checks sas: none found",
		"kansas=Ab3xyz789q KANSAS=Ab3xyz789q Arkansas: Ab3xyz789q",
	} {
		if out := String(in); out != in {
			t.Errorf("changed useful text:\n in: %q\nout: %q", in, out)
		}
	}
	k := leakKey()
	for _, in := range []string{
		"AZURE_STORAGE_SAS=" + k + " next=x",
		"sas=" + k,
		"sas: " + k,
		"GET /c/b?sas=" + k + "&x=1",
		`{"sas":"` + k + `"}`,
		"env:\n- name: STORAGE_SAS\n  value: " + k,
		"az storage blob list --sas " + k + " --account-name a",
		"azcopy copy --source-sas " + k + " x",
		"storageSas=" + k,
		"StorageSas: " + k,
		"azure.storage.sas: " + k,
		"set STORAGE_SAS " + k,
	} {
		checkRedacted(t, in, []string{k, k[:12], k[len(k)-12:]}, nil)
	}
}

// After a secret flag or a compound key name, a value that opens with a quote, plain or escaped, runs to its
// closing quote on the same line, with escapes handled; any other value is one token, ending at whitespace,
// the line end or a quote and nothing else, so a ",", "]" or "}" of the surrounding text may go with it
// (#585). What follows the value stays.
func TestQuotedValueAfterAKeyName(t *testing.T) {
	words := []string{"horse", "battery", "staple"}
	for name, c := range map[string]struct{ in, kept string }{
		"double quotes":        {`run --password "horse battery staple" --region x`, `" --region x`},
		"single quotes":        {`run --password 'horse battery staple' --region x`, `' --region x`},
		"escaped quote inside": {`run --password "horse \"battery\" staple" --region z`, `" --region z`},
		"argument list":        {`args: ["--client-secret", "horse battery staple", "--region", "x"]`, `", "--region", "x"]`},
		"escaped json":         {`"args":"[\"--client-secret\",\"horse battery staple\",\"--region\"]"`, `\",\"--region\"]"`},
		"escaped json twice":   {`"a":"{\"args\":\"[\\\"--client-secret\\\",\\\"horse battery staple\\\"]\"}"`, `\\\"]\"}"`},
		"yaml list quoted":     {"args:\n- --client-secret\n- \"horse battery staple\"\n- --region", "\"\n- --region"},
		"single-dash flag":     {`-password 'horse battery staple' -region x`, `' -region x`},
		"crlf continuation":    {"app --client-secret \\\r\n  \"horse battery staple\" \\\r\n  --region x", "--region x"},
	} {
		t.Run(name, func(t *testing.T) {
			checkRedacted(t, c.in, words, []string{c.kept})
		})
	}
	k := leakKey()
	for in, kept := range map[string]string{
		`args: ["--client-secret", ` + k + `, "--region", "west"]`: ` "--region", "west"]`,
		`set client_secret "` + k + `" then`:                       `" then`,
		`{"cmd": "run --token ` + k + `", "u": "x"}`:               ` "u": "x"}`,
		"{run --token " + k + "}\nnext":                            "\nnext",
		"run --token '" + k + " --region x":                        " --region x",
		"[--token " + k + "]next --region y":                       " --region y",
		"run --token " + k + "'x' --region y":                      " --region y",
	} {
		checkRedacted(t, in, []string{k, k[:12], k[len(k)-12:]}, []string{kept})
	}
	// A comma, a bracket or a brace inside the value, or at its start, does not end it.
	for in, secrets := range map[string][]string{
		"run --password p@ss}w0rdx --region x": {"p@ss", "w0rdx"},
		"run --password ]start --region x":     {"start"},
		"run --password }start --region x":     {"start"},
		"run --password abc,def --region x":    {"abc", "def"},
		"run --password ab12]cd34 --region x":  {"ab12", "cd34"},
	} {
		checkRedacted(t, in, secrets, []string{" --region x"})
	}
	// A quote that does not close goes with the value.
	if in, want := "run --token 'hunter2abc --region x", "run --token [REDACTED] --region x"; String(in) != want {
		t.Errorf("String(%q) = %q, want %q", in, String(in), want)
	}
	// A value never starts in the middle of a quoted string it did not open: the quote after the blank closes
	// the argument that holds the flag.
	for _, in := range []string{
		`args: ["--password ", "--region", "x"]`,
		`"args":"[\"--password \",\"--region\"]"`,
		`"a":"{\"args\":\"[\\\"--password \\\",\\\"--region\\\"]\"}"`,
		`"b":"{\"a\":\"{\\\"args\\\":\\\"[\\\\\\\"--password \\\\\\\",\\\\\\\"--region\\\\\\\"]\\\"}\"}"`,
	} {
		if out := String(in); out != in {
			t.Errorf("value taken from the next argument:\n in: %q\nout: %q", in, out)
		}
	}
}

// A key name that holds the word "secret" never takes the reference suffix "id"; "name", "ref", "file" and
// "path" stay references with it (#585).
func TestSecretNamesNeverTakeTheIDSuffix(t *testing.T) {
	k := leakKey()
	for _, in := range []string{
		"client_secret_key_id=" + k + " next=x",
		"clientSecretKeyId: " + k,
		`{"client-secret-key-id": "` + k + `"}`,
		"run --client-secret-key-id " + k,
	} {
		checkRedacted(t, in, []string{k, k[:12], k[len(k)-12:]}, nil)
	}
	for _, in := range []string{
		"secret_name=db-creds", "secretRef: db-creds", "client_secret_file=/etc/app/cs",
		"secret_path=/etc/app/s", "--client-secret-name db-creds", "client_id=app", "key_id=k1",
	} {
		if out := String(in); out != in {
			t.Errorf("changed a reference:\n in: %q\nout: %q", in, out)
		}
	}
}

// Single-dash long flags, as Go programs take them, count like "--" flags; "-p" does not (#585).
func TestSingleDashLongFlags(t *testing.T) {
	k := leakKey()
	for _, in := range []string{
		"app -password " + k + " -region x",
		"app -client-secret=" + k + " -region x",
		"app -token " + k,
		"app -token\n" + k,
		`args: ["-password", "` + k + `"]`,
	} {
		checkRedacted(t, in, []string{k, k[:12], k[len(k)-12:]}, nil)
	}
	for _, in := range []string{"app -p hunter", "my-token-x", "kubectl get secret -o yaml", "re-auth x"} {
		if out := String(in); out != in {
			t.Errorf("changed useful text:\n in: %q\nout: %q", in, out)
		}
	}
}

// A key name of two or more words before a blank counts joined by "-" and in camelCase too, not only by "_"
// (#585). After such a name a value that reads as a flag is kept, as "kubectl describe secret db-password -n
// prod" names a Secret. The names follow the rules of the other forms, and for every name the value counts
// only when it is an opaque token: 16 or more characters of "A-Z a-z 0-9 / + = _ -" with a digit or one of
// "/+=".
func TestSpelledCompoundKeyNames(t *testing.T) {
	k := leakKey()
	for _, in := range []string{
		"login client-secret " + k + " next",
		"login clientSecret " + k + " next",
		"set db-password\t" + k,
		"set apiKey " + k,
		// Names that start upper-case.
		"login ClientSecret " + k + " next",
		"set ApiKey " + k,
		"set AccessKey " + k,
		"set STORAGE_SAS " + k,
		// The same names as in the other forms: a secret word anywhere in the name, and "stdin" is a
		// reference suffix for flags only.
		"vault write auth/approle/login secret_id " + k,
		"set SecretId " + k,
		"export SECRET_KEY_BASE " + k,
		"set password_stdin " + k,
	} {
		checkRedacted(t, in, []string{k, k[:12], k[len(k)-12:]}, nil)
	}
	for in, v := range map[string]string{
		"set client_secret hunter3hunter3hu now":      "hunter3hunter3hu",
		"set db-password hunter/twohunter/ now":       "hunter/twohunter/",
		"set client_secret \"hunter+x9hunterx9\" now": "hunter+x9hunterx9",
		"set ClientSecret correcthorsebat= now":       "correcthorsebat=",
		"set ClientSecret ab_cd-ef_gh-ij_k1 now":      "ab_cd-ef_gh-ij_k1",
	} {
		checkRedacted(t, in, []string{v}, []string{" now"})
	}
	for _, in := range []string{
		"kubectl describe secret db-password -n prod",
		"kubectl get secret mySecret -o yaml",
		"kubectl get secret db-token --namespace prod",
		"deleted Secret/client-secret-7d9f8b6c5",
		"ClientSecretCredential authentication failed",
		"ClientAssertionCredential authentication failed.",
		"refresh token expired",
		"ClientSecret is missing for tenant",
		"AWS_SECRET_ACCESS_KEY is empty",
		"MyPassword Expired",
		"ClientSecret correcthorsebat",
		"ClientSecret --namespace prod",
		"client_secret is missing",
		"db-password Opaque",
		// Not an opaque token: shorter than 16, a byte outside the set, or no digit and none of "/+=".
		"ClientSecretCredential AuthenticationFailed tenant=t1",
		"db-password kubernetes.io/basic-auth 2 5d",
		"DB_PASSWORD CredentialUnavailableError 12345 next",
		"set ClientSecret correcthorsebattery now",
		"secret-volume (ro)",
		"volumes=[secret-volume kube-api-access-x]",
	} {
		if out := String(in); out != in {
			t.Errorf("changed useful text:\n in: %q\nout: %q", in, out)
		}
	}
}

// Known leaks after a key name, kept by decision and documented in design 10.3 and threat model G4.4 (#585):
// after a key name only the first token of a value goes, or the text between its quotes on one line. Taking
// more needs a tokenizer per format (#708). The test fails when one no longer leaks,
// so the list and the docs are updated with the change that closes it.
func TestKeyNameKnownLeaks(t *testing.T) {
	k := leakKey()
	for name, in := range map[string]string{
		"unicode-escaped key":        `{"\u0070assword": "` + k + `"}`,
		"unicode-escaped flag":       `["--\u0070assword", "` + k + `"]`,
		"here-doc":                   "app login <<EOF\n" + k + "\nEOF",
		"here-doc after stdin flag":  "docker login -u bob --password-stdin <<EOF\n" + k + "\nEOF",
		"here-string":                `docker login -u bob --password-stdin <<< "` + k + `"`,
		"short flag":                 "mysql -u root -p " + k,
		"one-word name and a blank":  "vault write auth/x secret " + k,
		"one-word token name":        "app set token " + k,
		"blanks inside the argument": `args: ["--client-secret=horse battery ` + k + `"]`,
		"flow sequence scalar":       "args: [--client-secret, horse battery " + k + ", --region]",
		"block scalar":               "args:\n- --client-secret\n- |-\n  " + k + "\n- --region",
		"anchor":                     "args:\n- --client-secret\n- &pw " + k,
		"tag":                        "args:\n- --client-secret\n- !!str " + k,
		"multi-line quoted scalar":   "--password \"horse\n" + k + "\" --region x",
		"shell concatenated quotes":  `--client-secret="horse"'` + k + `' --region x`,
		"command substitution":       "--password $(echo " + k + ") --region x",
		"array value":                `{"password": ["horse", "` + k + `"], "u": "x"}`,
		"object value":               `{"token": {"a": "horse", "b": "` + k + `"}, "u": "x"}`,
		"escaped array value":        `{\"apiKey\":[\"horse\",\"` + k + `\"],\"u\":1}`,
		"json comment":               "{\"password\": // rotated\n  \"" + k + "\"}",
		// The separator after a flag takes a comma only directly after the flag's closing quote, and no JSON
		// escape for whitespace ("\n"): such a comma or escape is the start of the value.
		"flow sequence of plain items": "args: [--client-secret, " + k + ", --region]",
		"escaped json lines":           `"args":"[\n  \"--client-secret\",\n  \"` + k + `\"\n]"`,
		// An unquoted value ends at a quote, also one that belongs to the value.
		"quote inside a value": "run --password ab'" + k + " --region x",
		// After a compound name and a blank, only an opaque token goes.
		"comma inside a blank-form value": "set aws_secret_access_key abc," + k + " now",
	} {
		if out := String(in); !strings.Contains(out, k) {
			t.Errorf("%s: no longer leaks; drop it here and from design 10.3 and threat model G4.4:\n in: %q\nout: %q",
				name, in, out)
		}
	}
	// After a compound key name and a blank, a value that is no opaque token stays (compoundValueSpans,
	// opaqueToken): shorter than 16 characters, a byte outside "A-Z a-z 0-9 / + = _ -", or no digit and none
	// of "/+=". A password such as "hunter2" or "Password!" is the common case.
	for in, word := range map[string]string{
		"export DB_PASSWORD changeme":                                      "changeme",
		"set ClientSecret Password now":                                    "Password",
		"set client_secret hunter2 now":                                    "hunter2",
		"set ClientSecret \"Password!\" now":                               "Password!",
		"set client_secret \"horse battery staple\" now":                   "horse battery staple",
		"set client_secret hunter3hunter3h now":                            "hunter3hunter3h",
		"set client_secret correcthorsebatterystaple x":                    "correcthorsebatterystaple",
		"set aws_secret_access_key abc,DEF123/ghiJKL456/mnoPQR789+stu now": "DEF123/ghiJKL456/mnoPQR789+stu",
		"client_secret myPassword123 reason=FailedMount":                   "myPassword123",
	} {
		if out := String(in); !strings.Contains(out, word) {
			t.Errorf("value after a blank no longer leaks; drop it here and from design 10.3 and threat model G4.4:\n in: %q\nout: %q",
				in, out)
		}
	}
}

// flagValueLine is a secret flag, its value and the next flag.
const flagValueLine = "run --password hunter2abc --region y"

// evidenceRow is one input of TestKeepsWhatMainKept: main is the output of main before #585 (commit f09d6392)
// and mainSpans the byte ranges it redacted, generated once by a throwaway test run in a checkout of that
// commit. want, when set, is the output of this change, written by hand for a value it is meant to redact
// (why says which).
type evidenceRow struct {
	in, main  string
	mainSpans [][2]int
	want, why string
}

// evidenceRows are realistic inputs that mention a secret word: logfmt and JSON log lines, kubectl describe
// and event lines, curl with a credential in a quoted URL and options after it, volume and Secret names.
var evidenceRows = []evidenceRow{
	{in: "level=warn msg=\"refresh token: expired\" user=bob pod=web-7f9 node=n1",
		main:      "level=warn msg=\"refresh token: *** user=bob pod=web-7f9 node=n1",
		mainSpans: [][2]int{{31, 39}}},
	{in: "level=info msg=\"rotated client secret for app\" app=web ns=prod",
		main: "level=info msg=\"rotated client secret for app\" app=web ns=prod"},
	{in: "ts=2026-11-14T10:00:00Z level=error msg=\"password authentication failed for user \\\"app\\\"\" db=orders",
		main: "ts=2026-11-14T10:00:00Z level=error msg=\"password authentication failed for user \\\"app\\\"\" db=orders"},
	{in: "level=error msg=\"secret \\\"db-creds\\\" not found\" controller=app",
		main: "level=error msg=\"secret \\\"db-creds\\\" not found\" controller=app"},
	{in: "level=warn msg=\"ClientSecretCredential authentication failed\" tenant=t1",
		main: "level=warn msg=\"ClientSecretCredential authentication failed\" tenant=t1"},
	{in: "{\"level\":\"error\",\"msg\":\"failed to read secret db-password: not found\",\"namespace\":\"prod\"}",
		main:      "{\"level\":\"error\",\"msg\":\"failed to read secret db-password: *** found\",\"namespace\":\"prod\"}",
		mainSpans: [][2]int{{59, 62}}},
	{in: "{\"msg\":\"token expired\",\"user\":\"bob\",\"retry\":3}",
		main: "{\"msg\":\"token expired\",\"user\":\"bob\",\"retry\":3}"},
	{in: "{\"msg\":\"api key missing\",\"service\":\"billing\"}",
		main: "{\"msg\":\"api key missing\",\"service\":\"billing\"}"},
	{in: "{\"msg\":\"--password flag is deprecated, use --password-file\",\"component\":\"cli\"}",
		main: "{\"msg\":\"--password flag is deprecated, use --password-file\",\"component\":\"cli\"}",
		want: "{\"msg\":\"--password *** is deprecated, use --password-file\",\"component\":\"cli\"}",
		why:  "the token after a secret flag goes whatever it reads as, also in prose (accepted over-redaction)"},
	{in: "  Warning  FailedMount  2m  kubelet  MountVolume.SetUp failed for volume \"secret-volume\" : secret \"db-password\" not found",
		main: "  Warning  FailedMount  2m  kubelet  MountVolume.SetUp failed for volume \"secret-volume\" : secret \"db-password\" not found"},
	{in: "    Mounts:\n      /etc/secret from secret-volume (ro)\n      /var/run/secrets/kubernetes.io/serviceaccount from kube-api-access-x (ro)",
		main: "    Mounts:\n      /etc/secret from secret-volume (ro)\n      /var/run/secrets/kubernetes.io/serviceaccount from kube-api-access-x (ro)"},
	{in: "  Warning  FailedMount  1m  kubelet  Unable to attach or mount volumes: unmounted volumes=[secret-volume], unattached volumes=[secret-volume kube-api-access-x]",
		main: "  Warning  FailedMount  1m  kubelet  Unable to attach or mount volumes: unmounted volumes=[secret-volume], unattached volumes=[secret-volume kube-api-access-x]"},
	{in: "Error from server (NotFound): secrets \"client-secret\" not found",
		main: "Error from server (NotFound): secrets \"client-secret\" not found"},
	{in: "env:\n- name: DB_PASSWORD\n  valueFrom:\n    secretKeyRef:\n      name: db-creds\n      key: password",
		main: "env:\n- name: DB_PASSWORD\n  valueFrom:\n    secretKeyRef:\n      name: db-creds\n      key: password"},
	{in: "kubectl create secret generic db-password --from-literal=x=y -n prod",
		main: "kubectl create secret generic db-password --from-literal=x=y -n prod"},
	{in: "kubectl get secret db-token --namespace prod -o yaml",
		main: "kubectl get secret db-token --namespace prod -o yaml"},
	{in: "docker login --password-stdin -u bob registry.test",
		main: "docker login --password-stdin -u bob registry.test"},
	{in: "curl \"https://svc.test/?token=x\" --retry 3",
		main:      "curl \"https://svc.test/?token=** --retry 3",
		mainSpans: [][2]int{{30, 32}}},
	{in: "curl -sS \"https://svc.test/api?token=abc123\" --retry 3 -H \"Accept: application/json\"",
		main:      "curl -sS \"https://svc.test/api?token=*** --retry 3 -H \"Accept: application/json\"",
		mainSpans: [][2]int{{37, 44}}},
	{in: "args: [\"--token-ttl\", \"5m\", \"--secret-name\", \"db-creds\"]",
		main: "args: [\"--token-ttl\", \"5m\", \"--secret-name\", \"db-creds\"]"},
	{in: flagValueLine,
		main: flagValueLine,
		want: "run --password [REDACTED] --region y",
		why:  "the value after a secret flag"},
	{in: "args: [\"--client-secret\", \"s3cr3t-value\", \"--region\", \"west\"]",
		main: "args: [\"--client-secret\", \"s3cr3t-value\", \"--region\", \"west\"]",
		want: "args: [\"--client-secret\", \"[credential]\", \"--region\", \"west\"]",
		why:  "the value after a secret flag in an argument list"},
	{in: "login ClientSecret Abc123xyz9Def456 next",
		main: "login ClientSecret Abc123xyz9Def456 next",
		want: "login ClientSecret [credential] next",
		why:  "the value after an upper-case compound key name and a blank"},
	{in: "az storage blob list --sas sv=2024 --account-name acct",
		main: "az storage blob list --sas sv=2024 --account-name acct",
		want: "az storage blob list --sas *** --account-name acct",
		why:  "the value after the flag --sas"},
	// A compound key name before a word of a sentence or a table column keeps the word.
	{in: "level=error msg=\"db_password not set\" component=db",
		main: "level=error msg=\"db_password not set\" component=db"},
	{in: "level=warn msg=\"client_secret expired\" app=web",
		main: "level=warn msg=\"client_secret expired\" app=web"},
	{in: "{\"msg\":\"api_key missing\",\"service\":\"billing\"}",
		main: "{\"msg\":\"api_key missing\",\"service\":\"billing\"}"},
	{in: "level=error msg=\"x-api-key header missing\" path=/v1",
		main: "level=error msg=\"x-api-key header missing\" path=/v1"},
	{in: "  Warning  Failed  3s  kubelet  Error: couldn't find key db_password in Secret prod/db-creds",
		main: "  Warning  Failed  3s  kubelet  Error: couldn't find key db_password in Secret prod/db-creds"},
	{in: "NAME          TYPE     DATA   AGE\ndb-password   Opaque   1      5d",
		main: "NAME          TYPE     DATA   AGE\ndb-password   Opaque   1      5d"},
	{in: "level=info msg=\"serviceAccountToken projected into pod\" pod=web-7f9",
		main: "level=info msg=\"serviceAccountToken projected into pod\" pod=web-7f9"},
	{in: "{\"msg\":\"login failed\",\"password\":\"hunter2\",\"user\":\"bob\"}",
		main:      "{\"msg\":\"login failed\",\"password\":\"***\",\"user\":\"bob\"}",
		mainSpans: [][2]int{{34, 41}}},
	// After a compound name and a blank, a word, a type or a path is no opaque token and stays.
	{in: "level=warn msg=\"ClientSecretCredential AuthenticationFailed\" tenant=t1",
		main: "level=warn msg=\"ClientSecretCredential AuthenticationFailed\" tenant=t1"},
	{in: "NAME          TYPE                       DATA   AGE\ndb-password   kubernetes.io/basic-auth   2      5d",
		main: "NAME          TYPE                       DATA   AGE\ndb-password   kubernetes.io/basic-auth   2      5d"},
	{in: "db-password kubernetes.io/basic-auth 2 5d",
		main: "db-password kubernetes.io/basic-auth 2 5d"},
	{in: "DB_PASSWORD CredentialUnavailableError 12345 next",
		main: "DB_PASSWORD CredentialUnavailableError 12345 next"},
	// An unquoted value ends at a quote, so a key inside a JSON string does not take the next field.
	{in: "{\"message\":\"ClientSecretCredential AuthenticationFailed\",\"reason\":\"FailedMount\"}",
		main: "{\"message\":\"ClientSecretCredential AuthenticationFailed\",\"reason\":\"FailedMount\"}"},
	{in: "{\"cmd\":\"--password abc\",\"reason\":\"FailedMount\"}",
		main: "{\"cmd\":\"--password abc\",\"reason\":\"FailedMount\"}",
		want: "{\"cmd\":\"--password ***\",\"reason\":\"FailedMount\"}",
		why:  "the value after a secret flag inside a JSON string, up to the closing quote"},
	// A match inside a value already taken is skipped, except a secret-named flag.
	{in: "client_secret myPassword123 reason=FailedMount",
		main: "client_secret myPassword123 reason=FailedMount"},
	{in: "client_secret db_password --region west",
		main: "client_secret db_password --region west"},
	{in: "client_secret db_password --region=us-west-2a",
		main: "client_secret db_password --region=us-west-2a"},
	{in: "--password myPassword123 reason=FailedMount",
		main: "--password myPassword123 reason=FailedMount",
		want: "--password [credential] reason=FailedMount",
		why:  "the value after a secret flag, and nothing after it"},
	// The separator after a flag takes no bare comma, no bare "-" and no literal "\n": they are the value.
	{in: "run --password ,,,, --region west",
		main: "run --password ,,,, --region west",
		want: "run --password *** --region west",
		why:  "a bare comma after a flag is its value"},
	{in: "run --password - --region west",
		main: "run --password - --region west",
		want: "run --password * --region west",
		why:  "a bare dash after a flag is its value"},
	{in: `run --password \n\r\t --region west`,
		main: `run --password \n\r\t --region west`,
		want: "run --password *** --region west",
		why:  "literal backslash pairs after a flag are its value"},
}

// Evidence main kept stays (#585): on each input of evidenceRows, the redactor removes no byte that main kept,
// except in rows marked by hand with the output this change is meant to give.
func TestKeepsWhatMainKept(t *testing.T) {
	for _, r := range evidenceRows {
		if r.want != "" {
			if got := String(r.in); got != r.want {
				t.Errorf("%s:\n  in: %q\n got: %q\nwant: %q", r.why, r.in, got, r.want)
			}
			continue
		}
		mainRedacted := make([]bool, len(r.in))
		for _, sp := range r.mainSpans {
			for i := sp[0]; i < sp[1]; i++ {
				mainRedacted[i] = true
			}
		}
		for _, sp := range defaultRedactor.spans(r.in) {
			for i := sp.start; i < sp.end; i++ {
				if !mainRedacted[i] {
					t.Errorf("removed %q, which main kept:\n  in: %q\nmain: %q\n now: %q",
						r.in[sp.start:sp.end], r.in, r.main, String(r.in))
					break
				}
			}
		}
	}
}
