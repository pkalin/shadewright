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

package redact_test

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/pkalin/shadewright/internal/redact"
)

// Through every entry point, no secret part of a connection string that origin/main kept out of an output
// comes out on this branch (#840, decision of 2026-11-17, fourth). Encoding get_resource without HTML escapes
// showed a raw "<" or ">" in the user info to the connection-string rule, which stopped there: on main only
// the escape had let it match. The log excerpt did not exist on main; it is held to main's redactor, which
// main's log source ran over the excerpt.
func TestConnStringsThroughEveryEntryPointAgainstMain(t *testing.T) {
	gen := connStringCorpus(connStringCorpusSize)
	eps := connEntryPoints()
	eps["kube.Excerpt"] = entryPoints["kube.Excerpt"]
	mainOf := map[string]string{"kube.Excerpt": "redact.String"}
	f, err := os.Open(connStringMainTable)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	n, lost, hidden := 0, 0, 0
	for sc.Scan() {
		var row connMainRow
		if err := json.Unmarshal(sc.Bytes(), &row); err != nil {
			t.Fatalf("line %d: %v", n+1, err)
		}
		if n >= len(gen) || connHash(gen[n].in) != row.Hash {
			t.Fatalf("%s line %d does not match its generator: regenerate it from the commit it names", connStringMainTable, n+1)
		}
		c := gen[n]
		n++
		for name, ep := range eps {
			ref := name
			if m, ok := mainOf[name]; ok {
				ref = m
			}
			want, ok := row.Hidden[ref]
			if !ok {
				t.Fatalf("%s line %d has no entry point %q", connStringMainTable, n, ref)
			}
			hidden += len(want)
			outs := ep(c.in)
			for _, s := range want {
				if leakedSecret(strings.Join(outs, "\n"), []string{s}) != "" {
					lost++
					if lost <= 20 {
						t.Errorf("%s: main hid %q, now kept:\n  in: %q\n out: %q", name, s, c.in, outs)
					}
					break
				}
			}
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if n != len(gen) {
		t.Fatalf("%s has %d lines, the corpus %d inputs", connStringMainTable, n, len(gen))
	}
	if lost > 0 {
		t.Errorf("%d outputs of %d inputs through %d entry points keep a secret part main hid", lost, n, len(eps))
	}
	if hidden < n*len(eps) {
		t.Fatalf("corpus too weak: main hid %d secret parts over %d inputs", hidden, n)
	}
}

// connBuilt is one input of connByConstruction with its known answer: the text before the connection string
// and the text after it survive whole, and the secret parts do not.
type connBuilt struct {
	in, before, after string
	secrets           []string
}

// hunterParts are the secret parts of most built passwords, and neighbourSecret the value of the secret after
// the URL.
const (
	hunterParts     = "Hunter|2Secret"
	neighbourSecret = "Nb7Vq2Zx"
)

// connByConstruction builds texts of a connection string whose user or password holds "<", ">" or both (at
// the start, the end, the middle, doubled) between a known text before it and a known neighbour after it: a
// closing quote, a bracket of the markup around it, and a second secret right after it with each glue. The
// frames and glues are those that end the host part of the rule, as on main: a comma or a bracket after the
// host is read as part of it, before and after this change.
func connByConstruction() []connBuilt {
	passwords := [][2]string{ // the password, and its secret parts separated by "|"
		{"Hunter<2Secret", hunterParts}, {"Hunter>2Secret", hunterParts},
		{"Hunter<>2Secret", hunterParts}, {"<Hunter2Secret", "Hunter2Secret"},
		{"Hunter2Secret>", "Hunter2Secret"}, {"Hunter<<2Secret", hunterParts},
		{"Hunter>>2Secret", hunterParts}, {"Hunter><2Secret", hunterParts},
		{"Hun<ter>2Sec<ret", "Hun|ter|2Sec|ret"}, {"", ""},
	}
	users := [][2]string{{"app", ""}, {"Ap<pUser", "Ap|pUser"}, {"AppUser>", "AppUser"}, {"<AppUser", "AppUser"}}
	schemes := []string{"postgres", "mysql", "mongodb+srv", "redis", "https"}
	tails := []string{"", ":5432/x", "/x?sslmode=require"}
	frames := [][2]string{
		{"", ""}, {"--dsn=", ""}, {`"`, `"`}, {"'", "'"}, {"<", ">"}, {"<url>", "</url>"},
		{`{"url":"`, `"}`}, {"dial ", " failed"}, {"href=<", "> next"},
	}
	neighbours := [][3]string{ // glue, key, secret value
		{"", "", ""}, {"", "password=", neighbourSecret}, {" ", "password=", neighbourSecret}, {"\n", "DB_PASSWORD=", neighbourSecret},
		{"\t", "token: ", neighbourSecret}, {" ", "", "postgres://other:Nb7Vq2Zx@db2/y"},
	}
	var out []connBuilt
	for _, p := range passwords {
		for _, u := range users {
			if p[0] == "" && u[1] == "" {
				continue
			}
			for i, s := range schemes {
				for _, f := range frames {
					for _, nb := range neighbours {
						if f[1] == "" && nb[0] == "" && nb[2] != "" {
							continue // glued to the host, which the rule reads as part of the host
						}
						userinfo := u[0] + ":" + p[0]
						if p[0] == "" {
							userinfo = u[0]
						}
						url := s + "://" + userinfo + "@db" + tails[i%len(tails)]
						var secrets []string
						for part := range strings.SplitSeq(u[1]+"|"+p[1], "|") {
							if part != "" {
								secrets = append(secrets, part)
							}
						}
						b := connBuilt{before: f[0], after: f[1], secrets: secrets}
						if nb[2] != "" {
							b.after += nb[0] + nb[1]
							b.secrets = append(b.secrets, neighbourSecret)
						}
						b.in = f[0] + url + f[1] + nb[0] + nb[1] + nb[2]
						out = append(out, b)
					}
				}
			}
		}
	}
	return out
}

// Each built text comes out as the text before the URL, one connection-string marker, and the text after
// it: no secret part survives, the quote or bracket of the markup around the URL is kept, and the neighbour's
// key is kept while its value is redacted.
func TestConnStringByConstruction(t *testing.T) {
	cases := connByConstruction()
	failed := 0
	for _, c := range cases {
		out := redact.String(c.in)
		// The marker is the longest of its forms that fits in the match ([redact.Marker]).
		marker := ""
		for _, m := range []string{"[REDACTED:" + redact.KindConnectionString + "]", "[" + redact.KindConnectionString + "]"} {
			if strings.HasPrefix(out, c.before+m) {
				marker = m
				break
			}
		}
		bad := ""
		switch {
		case leakedSecret(out, c.secrets) != "":
			bad = "keeps " + leakedSecret(out, c.secrets)
		case marker == "":
			bad = "does not start with the text before the URL and one marker"
		case !strings.HasPrefix(out[len(c.before)+len(marker):], c.after):
			bad = "loses the text after the URL"
		}
		if bad != "" {
			failed++
			if failed <= 20 {
				t.Errorf("String(%q) = %q: %s (want %q, the marker, then %q)", c.in, out, bad, c.before, c.after)
			}
		}
	}
	if failed > 0 {
		t.Errorf("%d of %d built texts fail", failed, len(cases))
	}
}

// A user info that may now hold "<" and ">" still ends at "/", so it never takes the start of a second URL
// that the rule matched on its own before (the URL after a bracket of markup or prose), and the bracket that
// closes markup around a URL stays.
func TestConnStringWiderUserInfoKeepsItsNeighbours(t *testing.T) {
	const m = "[REDACTED:connection-string]"
	for in, want := range map[string]string{
		"https://example.com<postgres://app:Hunter2Secret@db/x":   "https://example.com<" + m,
		"<https://example.com><postgres://app:Hunter<2Secret@db>": "<https://example.com><" + m + ">",
		"see <postgres://app:Hunter<2Secret@db/x> and retry":      "see <" + m + "> and retry",
		"a>b@c http://x<y/z": "a>b@c http://x<y/z",
		"<a href=\"https://app:Hunter>2Secret@h/x\">link</a>":             "<a href=\"" + m + "\">link</a>",
		"postgres://app:Hunter<2Secret@db/x>mysql://app:Q>2Secret@e.prod": m + ">" + m,
		"--dsn=postgres://app:Hunter<2Secret@db:5432/x":                   "--dsn=" + m,
		"<url>redis://:Hunter<2Secret@cache:6379/0</url>":                 "<url>" + m + "</url>",
	} {
		if got := redact.String(in); got != want {
			t.Errorf("String(%q) = %q, want %q", in, got, want)
		}
	}
}
