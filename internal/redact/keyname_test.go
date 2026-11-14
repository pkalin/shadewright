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
// a form it does not belong to) fails here.
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
// holds a secret, as on main (#585).
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

// "sas" counts only in "sas=value" and "sas: value", never after "/", "." or a letter or a digit, so an image
// named sas keeps its tag (#585).
func TestSasKeyOnlyBeforeItsSeparator(t *testing.T) {
	for _, in := range []string{
		`Failed to pull image "ghcr.io/acme/sas:v1.2.3beta"`,
		"image: ghcr.io/acme/sas:v1.2.3beta",
		"pulled acme.sas:v2beta1 and storageSas:x",
		"opts sas:v1 /mnt/sas=v2",
		"az storage blob list --sas x",
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
	} {
		checkRedacted(t, in, []string{k, k[:12], k[len(k)-12:]}, nil)
	}
}

// After a secret key name or flag the value is the whole argument, not its first token (#585): to the closing
// quote of a quoted argument, to the next "," or "]" in a flow sequence, the whole indented block of a block
// scalar, one shell word of glued pieces, and the line end where the end is unclear.
func TestWholeArgumentAfterAKeyName(t *testing.T) {
	words := []string{"horse", "battery", "staple"}
	for name, c := range map[string]struct{ in, kept string }{
		"quoted argument":         {`args: ["--client-secret=horse battery staple", "--region", "x"]`, `"--region", "x"]`},
		"quoted single":           {`args: ['--client-secret=horse battery staple', '--region']`, `'--region']`},
		"quoted flag and value":   {`command: ["app", "--password horse battery staple"]`, `"app"`},
		"quoted env entry":        {`env: ["PASSWORD=horse battery staple", "REGION=x"]`, `env: [`},
		"escaped json":            {`"args":"[\"--client-secret=horse battery staple\",\"--region\"]"`, `\"--region\"]"`},
		"escaped json twice":      {`"a":"{\"args\":\"[\\\"--client-secret=horse battery staple\\\"]\"}"`, `"a":"{\"args\"`},
		"flow sequence":           {"args: [--client-secret, horse battery staple, --region, west]", "--region, west]"},
		"flow sequence last":      {"args: [--client-secret, horse battery staple]", "args: [--client-secret, "},
		"flow item with =":        {"args: [--client-secret=horse battery staple, --region, west]", "--region, west]"},
		"block scalar item":       {"args:\n- --client-secret\n- |-\n  horse battery\n  staple\n- --region\n- west", "- --region\n- west"},
		"folded block scalar":     {"args:\n  - --password\n  - >\n    horse battery\n    staple\n  - --region", "  - --region"},
		"yaml item plain":         {"args:\n- --client-secret\n- horse battery staple\n- --region", "- --region"},
		"yaml item one line":      {"args:\n- --client-secret horse battery staple\n- --region", "- --region"},
		"shell pieces":            {`--client-secret="horse"'battery'"staple" --region x`, flagAfter},
		"shell pieces unquoted":   {`--client-secret=horse'battery'staple --region x`, flagAfter},
		"escaped blank":           {`--client-secret horse\ battery\ staple --region x`, flagAfter},
		"line end inside quotes":  {"--client-secret \"horse battery\nstaple\" --region x", flagAfter},
		"single quotes on lines":  {"--password 'horse\nbattery staple' --region x", flagAfter},
		"unclosed quote":          {`--client-secret "horse battery staple --region x`, ""},
		"unclosed enclosing":      {`args: ["--client-secret=horse battery staple`, "args: ["},
		"key-value shell pieces":  {`PASSWORD="horse"'battery staple' REGION=x`, " REGION=x"},
		"compound shell pieces":   {`aws configure set aws_secret_access_key "horse"' battery staple' x`, " x"},
		"single-dash flag pieces": {`-password 'horse battery staple' -region x`, " -region x"},
	} {
		t.Run(name, func(t *testing.T) {
			var kept []string
			if c.kept != "" {
				kept = []string{c.kept}
			}
			checkRedacted(t, c.in, words, kept)
		})
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
// prod" names a Secret.
func TestSpelledCompoundKeyNames(t *testing.T) {
	k := leakKey()
	for _, in := range []string{
		"login client-secret " + k + " next",
		"login clientSecret " + k + " next",
		"set db-password\t" + k,
		"set apiKey " + k,
	} {
		checkRedacted(t, in, []string{k, k[:12], k[len(k)-12:]}, nil)
	}
	for _, in := range []string{
		"kubectl describe secret db-password -n prod",
		"kubectl get secret mySecret -o yaml",
		"kubectl get secret db-token --namespace prod",
		"deleted Secret/client-secret-7d9f8b6c5",
		"ClientSecretCredential authentication failed",
		"ClientAssertionCredential authentication failed.",
	} {
		if out := String(in); out != in {
			t.Errorf("changed useful text:\n in: %q\nout: %q", in, out)
		}
	}
}

// A JSON or flow secret field whose value is an array or an object loses the whole value (#585).
func TestSecretFieldArraysAndObjects(t *testing.T) {
	words := []string{"horse", "battery", "staple"}
	for in, kept := range map[string]string{
		`{"password": ["horse","battery", "staple"], "user": "x"}`:                                 `, "user": "x"}`,
		`{"token": {"a": "horse battery", "b": ["staple]"]}, "u": "x"}`:                            `, "u": "x"}`,
		"{\n  \"password\": [\n    \"horse\",\n    \"battery staple\"\n  ],\n  \"user\": \"x\"\n}": `"user": "x"`,
		`{\"apiKey\":[\"horse\",\"battery\",\"staple\"],\"u\":1}`:                                  `,\"u\":1}`,
		`{"password": ["horse", "battery staple"`:                                                  `{"password": `,
	} {
		checkRedacted(t, in, words, []string{kept})
	}
}

// Known leaks after a key name, kept by decision and documented in design 10.3 and threat model G4.4 (#585).
// The test fails when one no longer leaks, so the list and the docs are updated with the change that closes it.
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
	} {
		if out := String(in); !strings.Contains(out, k) {
			t.Errorf("%s: no longer leaks; drop it here and from design 10.3 and threat model G4.4:\n in: %q\nout: %q",
				name, in, out)
		}
	}
}
