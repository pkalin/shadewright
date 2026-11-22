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
	"fmt"
	"math/rand/v2"
	"os"
	"strings"
	"testing"
)

// shortItemImages are images of the table, one for each flag of arguments with no command word.
var shortItemImages = map[byte]string{'p': "mongo", 'P': "sqlcmd", 'a': "redis", 'u': "curl"}

// shortItemEndCases are the value of a password flag at the end of its own item (decision of 2026-11-22 on
// #1075): for each flag of arguments with no command word, a flag as the last item of a YAML block "args" before
// a key of its object, an item of the sequence around it or a comment line (no value: all kept), with a value as
// the next item, and glued, after "=" and as an item of its own in a flow list or a JSON array, next to the
// closing bracket, a comma and the next key. secrets holds the secret of a case that has one; the rest of the
// text must come out as main writes it.
func shortItemEndCases() []shortFlagCase {
	rng := rand.New(rand.NewPCG(1075, 4)) //nolint:gosec // a fixed seed keeps the corpus the same on every run
	var out []shortFlagCase
	i := 0
	for _, t := range shortArgsFlags {
		f, img := shortFlag(t), shortItemImages[t.letter]
		for _, nl := range []string{"\n", "\r\n"} {
			for _, in := range []string{
				"- args:\n  - " + f + "\n- name: keepname\n",
				"- args:\n  - " + f + "\n  image: busybox\n",
				"containers:\n- name: app\n  args:\n  - --verbose\n  - " + f + "\n  image: busybox\n",
				"args:\n- " + f + "\nimage: busybox\n",
				"- args:\n  - " + f + "\n  # keep note\n  image: busybox\n",
				"- image: " + img + "\n  args:\n  - " + f + "\n- name: keepname\n",
				"- image: " + img + "\n  args:\n  - " + f + "\n  name: keepname\n",
				"spec:\n  containers:\n  - args:\n    - " + f + "\n  - name: keepname\n",
				"- args: [" + f + "]\n  name: keepname\n",
				`{"args":["` + f + `"],"name":"keepname"}`,
			} {
				out = append(out, shortFlagCase{form: "item end, no value", in: strings.ReplaceAll(in, "\n", nl)})
			}
		}
		for _, form := range []string{
			"- args:\n  - %[1]s\n  - %[2]s\n- name: keepname\n",
			"- image: %[3]s\n  args:\n  - %[1]s\n  - %[2]s\n  name: keepname\n",
			"args: [%[1]s%[2]s]\nimage: busybox\n",
			"args: [%[1]s%[2]s, --verbose]\nimage: busybox\n",
			"args: [--verbose, %[1]s%[2]s]\nname: keepname\n",
			"args: [%[1]s=%[2]s]\nname: keepname\n",
			"args: [%[1]s, %[2]s]\nname: keepname\n",
			"args: [%[1]s,%[2]s]\nname: keepname\n",
			"args: [%[1]s, %[2]s, --verbose]\n",
			"image: %[3]s\nargs: [%[1]s%[2]s]\n",
			"- args: [%[1]s%[2]s]\n  name: keepname\n",
			"{args: [%[1]s%[2]s], name: keepname}\n",
			"args: [[%[1]s%[2]s]]\n",
			`{"args":["%[1]s%[2]s"],"name":"keepname"}`,
			`{"args":["%[1]s","%[2]s"],"image":"busybox"}`,
			`{"args":["--verbose","%[1]s%[2]s"]}`,
		} {
			secret := shortSecret(rng, i)
			i++
			in := fmt.Sprintf(form, f, shortValue(t, secret), img)
			out = append(out, shortFlagCase{form: "item end, value", in: in, secrets: []string{secret}})
		}
	}
	return out
}

// shortItemEndTable holds main's output and redacted ranges for every input of shortItemEndCases (commit
// a9b4120f), written by TestWriteShortItemEndMainTable in a checkout of that commit with this file added and
// SHORTFLAG_ITEMEND_TABLE set to the output path.
const shortItemEndTable = "testdata/shortflag-itemend-main.jsonl"

// TestWriteShortItemEndMainTable writes shortItemEndTable from the redactor it runs with, when
// SHORTFLAG_ITEMEND_TABLE names the output path; otherwise it is skipped.
func TestWriteShortItemEndMainTable(t *testing.T) {
	path := os.Getenv("SHORTFLAG_ITEMEND_TABLE")
	if path == "" {
		t.Skip("SHORTFLAG_ITEMEND_TABLE is not set")
	}
	cases := shortItemEndCases()
	rows := make([]any, 0, len(cases))
	for _, c := range cases {
		row := mainRedaction{In: c.in, Out: String(c.in)}
		for _, sp := range defaultRedactor.spans(nil, c.in) {
			row.Spans = append(row.Spans, [2]int{sp.start, sp.end})
		}
		rows = append(rows, row)
	}
	writeJSONLines(t, path, rows)
}

// The value of a password flag ends at the end of its own item: in every input of shortItemEndCases nothing is
// hidden that main keeps, other than the secret, so the next line's key, the next item and a closing bracket or
// comma stay; nothing main redacted is kept; and the secret is hidden.
func TestShortFlagItemEndAgainstMain(t *testing.T) {
	cases := shortItemEndCases()
	lost, leaked := 0, 0
	n := readJSONLines(t, shortItemEndTable, func(i int, line []byte) {
		var row mainRedaction
		if err := json.Unmarshal(line, &row); err != nil {
			t.Fatalf("line %d: %v", i+1, err)
		}
		if i >= len(cases) || cases[i].in != row.In {
			t.Fatalf("line %d of %s is not input %d of the corpus", i+1, shortItemEndTable, i)
		}
		c := cases[i]
		now := defaultRedactor.spans(nil, row.In)
		for _, want := range row.Spans {
			for b := want[0]; b < want[1]; b++ {
				if !coveredBy([2]int{b, b + 1}, now) {
					t.Errorf("main redacted byte %d, now kept:\n in: %q\nmain: %q", b, row.In, row.Out)
					break
				}
			}
		}
		got := String(row.In)
		if !shortHidden(c, got) {
			leaked++
			t.Errorf("%s: a secret is left:\n in: %q\nout: %q", c.form, row.In, got)
		}
		secret := make([]bool, len(row.In))
		for _, s := range c.secrets {
			for off := 0; ; {
				k := strings.Index(row.In[off:], s)
				if k < 0 {
					break
				}
				for b := off + k; b < off+k+len(s); b++ {
					secret[b] = true
				}
				off += k + 1
			}
		}
		for _, sp := range now {
			for b := sp.start; b < sp.end; b++ {
				if !secret[b] && !coveredBy([2]int{b, b + 1}, mainSpans(row.Spans)) {
					lost++
					if lost <= 10 {
						t.Errorf("%s: byte %d (%q), kept by main, is hidden:\n in: %q\nmain: %q\n now: %q", c.form, b,
							row.In[b], row.In, row.Out, got)
					}
					b = sp.end
				}
			}
		}
	})
	if n != len(cases) {
		t.Fatalf("%s has %d lines, the corpus %d inputs", shortItemEndTable, n, len(cases))
	}
	t.Logf("%d inputs: %d leak a secret, %d spans hide text main keeps", n, leaked, lost)
}

// mainSpans returns the ranges of a row of a main table as spans.
func mainSpans(r [][2]int) []span {
	out := make([]span, len(r))
	for i, x := range r {
		out[i] = span{start: x[0], end: x[1]}
	}
	return out
}
