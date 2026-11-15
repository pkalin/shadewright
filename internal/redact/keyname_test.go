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
	words := passphraseWords
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
		// A comma directly after the flag name: a YAML flow list of plain items.
		"args: [--client-secret, " + k + ", --region, west]": " --region, west]",
		"args: [--client-secret," + k + " , --region]":       " , --region]",
		"run --token " + k + "'x' --region y":                " --region y",
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
		"set ClientSecret correcthorsebattery now",
		"secret-volume (ro)",
		"volumes=[secret-volume kube-api-access-x]",
	} {
		if out := String(in); out != in {
			t.Errorf("changed useful text:\n in: %q\nout: %q", in, out)
		}
	}
}

// Known leaks after a key name, kept by decision and documented in design 10.3 and threat model G4.4 (#585,
// #708): names the rules do not read (Unicode escapes, here-documents, short flags, one-word names), separators
// they do not take, and values the extent rule does not reach. The test fails when one no longer leaks, so the
// list and the docs are updated with the change that closes it.
func TestKeyNameKnownLeaks(t *testing.T) {
	k := leakKey()
	for name, in := range map[string]string{
		"unicode-escaped key":       `{"\u0070assword": "` + k + `"}`,
		"unicode-escaped flag":      `["--\u0070assword", "` + k + `"]`,
		"here-doc":                  "app login <<EOF\n" + k + "\nEOF",
		"here-doc after stdin flag": "docker login -u bob --password-stdin <<EOF\n" + k + "\nEOF",
		"here-string":               `docker login -u bob --password-stdin <<< "` + k + `"`,
		"short flag":                "mysql -u root -p " + k,
		"one-word name and a blank": "vault write auth/x secret " + k,
		"one-word token name":       "app set token " + k,
		// The separator after a flag takes no JSON escape for whitespace ("\n"): such an escape is the start
		// of the value.
		"escaped json lines": `"args":"[\n  \"--client-secret\",\n  \"` + k + `\"\n]"`,
		// Only a double quote escaped as JSON inside a string opens a quoted value: after "\'" the value is
		// the quote and one token.
		"escaped single quote": `--password \'horse battery ` + k + `\' --region x`,
		// A plain item of a YAML block sequence after a flag item loses only its first token, as the same
		// line form ("- hunter2abc --region west") is pinned by TestKeyNameRulesHoldAtTheirEdges; after an
		// anchor or a tag the rest of the line goes (TestValueExtentAfterAKeyName).
		"block sequence item with blanks": "args:\n- --client-secret\n- horse battery " + k + "\n- --region",
		// The extent rule reads at most maxValueExtent bytes past the start of a value: the part of a longer
		// value after that bound stays.
		"part of a value past the bound": "args:\n- --password\n- |\n" +
			strings.Repeat("  "+strings.Repeat("a", 60)+"\n", 2*maxValueExtent/62) + "  " + k + "\n",
		// An unquoted value ends at a quote, also one that belongs to the value.
		"quote inside a value": "run --password ab'" + k + " --region x",
		// A closed quote glued to an unquoted value ends it too: the value may sit inside a JSON or logfmt
		// string whose fields follow the quote (plainWordEnd).
		"quote glued to a value": `run --password ab"cd ` + k + `" --region x`,
		// After a key name (not a flag), a comma glued to a quoted piece ends the shell word, as in
		// "password=\"abc\",user=bob" the fields after the comma stay.
		"comma glued to a quoted value": `TOKEN="abc",` + k + ` next=x`,
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
		"set client_secret hunter2x now":                                   "hunter2x",
		"set ClientSecret \"Password!\" now":                               "Password!",
		"set client_secret \"horse battery staple\" now":                   "horse battery staple",
		"set client_secret hunter3hunter3h now":                            "hunter3hunter3h",
		"set client_secret correcthorsebatterystaple x":                    "correcthorsebatterystaple",
		"set aws_secret_access_key abc,DEF123/ghiJKL456/mnoPQR789+stu now": "DEF123/ghiJKL456/mnoPQR789+stu",
		takenValueLine: "myPassword123",
	} {
		if out := String(in); !strings.Contains(out, word) {
			t.Errorf("value after a blank no longer leaks; drop it here and from design 10.3 and threat model G4.4:\n in: %q\nout: %q",
				in, out)
		}
	}
}

// takenValueLine is a compound name, a short password as its value (no opaque token, so it stays) and a field
// after it that a match starting inside the value would take.
const takenValueLine = "client_secret myPassword123 reason=FailedMount"

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
	{in: takenValueLine, main: takenValueLine},
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

// Inputs where the code and the three stated rules differed (#585 review), with the whole output: a flag
// whose quoted value starts inside the previous value and runs past it, an escaped quote that does not
// close, vertical tab and form feed as the separator, and a YAML list dash after a line end of "\r" alone.
func TestKeyNameRulesHoldAtTheirEdges(t *testing.T) {
	for in, want := range map[string]string{
		"--password,\"--token 'horse battery staple\" TAILSECRET' --region west": "--password,\"[REDACTED:credential]' --region west",
		`--password \"horse battery staple --region west`:                        `--password *** battery staple --region west`,
		"--password\vAbc123 --region west":                                       "--password\v*** --region west",
		"--password\fAbc123 --region west":                                       "--password\f*** --region west",
		"--password\r  - hunter2abc --region west":                               "--password\r  - [REDACTED] --region west",
		"app --client-secret \\\r  hunter2abc --region west":                     "app --client-secret \\\r  [REDACTED] --region west",
	} {
		if got := String(in); got != want {
			t.Errorf("String(%q)\n got %q\nwant %q", in, got, want)
		}
	}
}

// Over-redaction kept by decision (#585 review), pinned so that a change to it is seen: a flag's value that
// is itself a compound key name takes the opaque token after it, as the compound rule reads the text on its
// own; and a double-quoted value goes on past a backslash before its line end, as in YAML.
func TestKeyNameAcceptedOverRedaction(t *testing.T) {
	for in, want := range map[string]string{
		// An escaped array whose string ends before it closes: the enclosing string's quote ends the group.
		`{"log":"{\"apiKey\":[\"abc\"","x":"]"}`:                             `{"log":"{\"apiKey\":***,"x":"]"}`,
		`{"log":"{\"apiKey\":[\"abc","x":"\"]"}`:                             `{"log":"{\"apiKey\":***,"x":"\"]"}`,
		"--password,client_secret 20261114T100000Z0 reason=FailedMount":      "--password,[credential] [credential] reason=FailedMount",
		"--password \"abc\\\nreason=FailedMount node=worker\" --region west": "--password \"[REDACTED:credential]\" --region west",
	} {
		if got := String(in); got != want {
			t.Errorf("String(%q)\n got %q\nwant %q", in, got, want)
		}
	}
}

// Text after a value that TestValueExtentAfterAKeyName checks is kept.
const (
	listItemTail   = "\n- --region"
	quotedItemTail = "\"\n- --region"
	dquoteFlagTail = `" --region x`
	squoteFlagTail = `' --region x`
	jsonFieldTail  = `, "u": "x"}`
	flowRegionTail = ", --region]"
)

// The extent rule (#708): after a secret key name or flag, a value of more than one token goes whole, through
// the end of its group (quotes over line ends, brackets, braces, "$(" with quotes inside), its YAML list or
// flow item, its block scalar lines, or its quoted argument, with glued shell pieces, anchors, tags and JSON
// comments before it; and what follows the value stays. Each input leaked on main and before this change.
func TestValueExtentAfterAKeyName(t *testing.T) {
	k := leakKey()
	for in, kept := range map[string]string{
		`args: ["--client-secret=horse battery ` + k + `"]`:                                 `"]`,
		`{"args":"[\"--client-secret=horse battery ` + k + `\",\"--x\"]"}`:                  `\",\"--x\"]"}`,
		`args: ['--client-secret=horse battery ` + k + `', '--x']`:                          `', '--x']`,
		"args: [--client-secret, horse battery " + k + flowRegionTail:                       flowRegionTail,
		"args: [--a, --client-secret, horse battery " + k + "]\nnext: x":                    "]\nnext: x",
		"args:\n- --client-secret\n- |-\n  horse\n  battery " + k + "\n- --region":          listItemTail,
		"args:\n  - --client-secret\n  - >\n    horse battery\n    " + k + "\n  - --region": "\n  - --region",
		"args:\n- --client-secret\n- &pw horse battery " + k + "\n- --region":               listItemTail,
		"args:\n- --client-secret\n- !!str horse battery " + k + "\n- --region":             listItemTail,
		"args:\n- --password\n- &pw !!str \"horse\n  battery " + k + "\"\n- --region":       quotedItemTail,
		"args:\n- --client-secret\n- &pw !!str " + k + "\n- --region":                       listItemTail,
		"args: [--client-secret, !!str horse battery " + k + ", --region]":                  ", --region]",
		"--password \"horse\nbattery " + k + "\" --region x":                                dquoteFlagTail,
		"--password 'horse\nbattery ''" + k + "' --region x":                                squoteFlagTail,
		"args:\n- --password\n- \"horse\n  battery " + k + "\"\n- --region":                 quotedItemTail,
		`--client-secret="horse"'battery ` + k + `' --region x`:                             squoteFlagTail,
		`--password "horse"'battery '` + k + ` --region x`:                                  flagAfter,
		`PASSWORD="horse"battery'` + k + `' next=x`:                                         `' next=x`,
		"--password $(echo horse battery " + k + ") --region x":                             flagAfter,
		`--password "$(vault read -field=v "secret/horse battery ` + k + `")" --region x`:   dquoteFlagTail,
		"--password `cat horse battery " + k + "` --region x":                               flagAfter,
		"TOKEN=$(printf '%s' \"horse battery " + k + "\") next=x":                           " next=x",
		`{"password": ["horse", "battery ` + k + `"], "u": "x"}`:                            jsonFieldTail,
		`{"token": {"a": "horse", "b": {"c": "battery ` + k + `"}}, "u": "x"}`:              jsonFieldTail,
		"{\"password\": [\n  \"horse\",\n  \"battery " + k + "\"\n],\n\"u\": \"x\"}":        ",\n\"u\": \"x\"}",
		`{\"apiKey\":[\"horse\",\"battery ` + k + `\"],\"u\":1}`:                            `,\"u\":1}`,
		`{\"apiKey\":{\"a\":[\"horse ]\",\"` + k + `\"]},\"u\":1}`:                          `,\"u\":1}`,
		"{\"password\": // rotated\n  \"horse battery " + k + "\", \"u\": \"x\"}":           jsonFieldTail,
		"{\"password\": /* old\n */ \"horse battery " + k + "\", \"u\": \"x\"}":             jsonFieldTail,
	} {
		checkRedacted(t, in, []string{k, k[:12], k[len(k)-12:], passphraseWords[1]}, []string{kept})
	}
}

// The bounds of the extent rule (#708): a group that does not close within maxValueExtent bytes, or that
// meets a closer that does not match, gives no extent, and the value is the one it was before (one token, or
// the text to its format's boundary), so a stray quote or bracket does not take the text after it. A flow
// item is only a flag after "[" or ",", so "--password, --token" in prose loses only the next token.
func TestValueExtentBounds(t *testing.T) {
	far := strings.Repeat("x", maxValueExtent)
	for in, want := range map[string]string{
		"--password \"hunter2abc\n" + far + "\" --region x": "--password [REDACTED]\n" + far + "\" --region x",
		"--password $(hunter2abc ] next --region x":         "--password [credential] ] next --region x",
		`{"password": [abc, ` + far + `]}`:                  `{"password": ***, ` + far + `]}`,
		"use --password, now --region west":                 "use --password, *** --region west",
		// An escaped array whose string ends before it closes: the enclosing string's quote ends the group.
		`{"log":"{\"apiKey\":[\"abc\"","x":"]"}`:                        `{"log":"{\"apiKey\":***,"x":"]"}`,
		`{"log":"{\"apiKey\":[\"abc","x":"\"]"}`:                        `{"log":"{\"apiKey\":***,"x":"\"]"}`,
		"--password,client_secret 20261114T100000Z0 reason=FailedMount": "--password,[credential] [credential] reason=FailedMount",
	} {
		if got := String(in); got != want {
			t.Errorf("String(%.80q)\n got %.120q\nwant %.120q", in, got, want)
		}
	}
	// A block scalar longer than the bound loses its first maxValueExtent bytes or so; the rest stays, a known
	// leak documented in design 10.3.
	long := "args:\n- --password\n- |\n" + strings.Repeat("  "+strings.Repeat("a", 60)+"\n", 2*maxValueExtent/62) + "  TAILSECRET\n"
	if out := String(long); !strings.Contains(out, "TAILSECRET") || strings.Contains(out, "\n  aaaa") && strings.Index(out, "\n  aaaa") < 40 {
		t.Errorf("block scalar past the bound:\nout: %.200q", out)
	}
}

// Text after a value that TestValueExtentReachesTheWholeValue checks is kept, and the words of its values.
const (
	regionWest = " --region west"
	nextX      = " next=x"
)

var passphraseWords = []string{"horse", "battery", "staple"}

// The extent rule at its edges (#708): a flow scalar on an indented next line, a block scalar behind an anchor,
// an escaped blank and a backtick substitution inside double quotes in a shell word, a literal bracket inside
// "$(", closers glued to a word with text after them, an unclosed quote after a flag at the end of the input,
// and ":=". Each input kept part of its value before this change.
func TestValueExtentReachesTheWholeValue(t *testing.T) {
	for in, kept := range map[string]string{
		"args: [--password, horse\n  battery staple, --region, west]":            ", --region, west]",
		"args: [--password, !!str horse\n  battery staple, --region, west]":      ", --region, west]",
		"args:\n- --password\n- &pw |-\n  horse\n  battery\n- --region\n- west":  "\n- --region\n- west",
		"args:\n- --password\n- !!str >\n  horse\n  battery\n- --region\n- west": "\n- --region\n- west",
		`--password "horse"\ battery --region west`:                              regionWest,
		"--password \"`printf \"%s\" \"horse battery\"`\" --region west":         regionWest,
		"--password $(printf '%s' horse[battery) --region west":                  regionWest,
		"--password $(echo ${X} {horse battery) --region west":                   regionWest,
		"--password $(cat horse)}battery --region west":                          regionWest,
		"--password $(cat horse)]}battery --region west":                         regionWest,
		"--password $()}horsebattery --region west":                              regionWest,
		"--password \"horse battery staple --region west":                        regionWest,
		`--password "horse battery staple`:                                       "--password ",
		"--password \"horse battery staple\nreason: FailedMount":                 "\nreason: FailedMount",
		`password := "horse battery staple" next=x`:                              nextX,
		"password := horse;battery next=x":                                       nextX,
		"password: \"horse\n  battery staple\"\nnext: x":                         "\nnext: x",
		"  password: \"horse\n\n    battery staple\"\n  next: x":                 "\n  next: x",
	} {
		checkRedacted(t, in, passphraseWords, []string{kept})
	}
	// Closers at the end of a word stay with the text after them.
	for in, want := range map[string]string{
		`args: ["--password", "abc"], next: x`:  `args: ["--password", "***"], next: x`,
		"--password $(cat pw)} --region west":   "--password ***} --region west",
		"--password $()}Wc3":                    "--password ***",
		"--password $(cat pw)}Hunter2Secret9 x": "--password [REDACTED:credential] x",
	} {
		if got := String(in); got != want {
			t.Errorf("String(%q)\n got %q\nwant %q", in, got, want)
		}
	}
}

// What the extent rule must not take (#708): an unclosed block comment does not extend a value, and a quote
// that does not close on its line continues only on lines indented deeper than its key, so the fields after
// a stray quote stay. Each input lost these parts before this change, and main kept them.
func TestValueExtentKeepsEvidence(t *testing.T) {
	for in, want := range map[string]string{
		"{\"password\": /* x\n\"u\": \"EVIDENCE\", \"reason\": \"FailedMount\"}": "{\"password\": ***\n\"u\": \"EVIDENCE\", \"reason\": \"FailedMount\"}",
		"apiKey: \"abc\ntwo: \"def\"\nthree: ghi":                                "apiKey: ***\ntwo: \"def\"\nthree: ghi",
		"password: 'abc\nreason: EVIDENCE it's broken":                           "password: ***\nreason: EVIDENCE it's broken",
		"  password: \"abc\n  reason: \"EVIDENCE\"":                              "  password: ***\n  reason: \"EVIDENCE\"",
		"msg=password=\"abc\nreason: \"EVIDENCE\" next":                          "msg=password=***\nreason: \"EVIDENCE\" next",
	} {
		if got := String(in); got != want {
			t.Errorf("String(%q)\n got %q\nwant %q", in, got, want)
		}
	}
}

// Shell syntax ("$(", backticks, backslash escapes, glued words) applies only where the value is a shell word;
// inside a JSON or YAML quoted string, or a YAML plain scalar, the text is literal (#708). And the value is
// never shorter than main's (TestExtentStillRedactsWhatMainRedacted). Each input lost or leaked part of its
// value before this change.
func TestValueExtentShellOnlyInShellWords(t *testing.T) {
	for in, want := range map[string]string{
		// main redacted the whole token: a comma glued to a substitution stays in the value.
		"--password $(echo),hunter2 --region west": "--password [credential] --region west",
		// A JSON string is literal: its closing quote ends the value, and the next argument stays.
		`["--password", "$(", "EVIDENCE"]`: `["--password", "**", "EVIDENCE"]`,
		// An escaped parenthesis does not close the substitution.
		`--password $(printf %s \) horse battery) --region west`: "--password [REDACTED:credential] --region west",
		// One JSON argument: its single quotes are literal, and the value runs to the argument's end.
		`["--password='horse battery' staple", "--region"]`: `["--password='[connection-string]", "--region"]`,
		// An apostrophe inside a YAML plain scalar opens no quote.
		"{password: [horse'battery, staple], u: x}": "{password: [REDACTED:credential], u: x}",
		// Outside a shell, parentheses are plain text.
		"{password: [horse), battery staple], u: x}": "{password: [REDACTED:credential], u: x}",
	} {
		if got := String(in); got != want {
			t.Errorf("String(%q)\n got %q\nwant %q", in, got, want)
		}
	}
}
