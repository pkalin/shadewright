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
	"fmt"
	"strings"
	"testing"
)

// The two forms the decision of 2026-11-17 (second) on #840 adds, built so the answer is known: a block
// flattened onto a line inside a flow collection that closes on the line ("msg: [password: | x]"), and a
// block under its header after more than one "---" or "..." line. The text after the closing bracket and the
// key after the block survive.
func flowMarkerCorpus() []formCase {
	out := make([]formCase, 0, 2*len(formKeys)*len(formIndicators))
	n := 0
	nls := []string{"\n", "\r\n"}
	opens := []struct{ open, close string }{{"[", "]"}, {"{", "}"}, {"{a: [", "], b: c}"}, {"[x, {", "}]"}}
	markers := []string{"---", "...", "--- ", "---\t"}
	for ki, key := range formKeys {
		for ii, ind := range formIndicators {
			v := ki + ii
			w := &formWriter{n: &n}
			o := opens[v%len(opens)]
			w.kept("msg")
			w.text(": " + o.open + key + ": " + ind + strings.Repeat(" ", 1+v%3))
			w.secret()
			if v%2 == 1 {
				w.text(" ")
				w.secret()
			}
			w.text(o.close + " ")
			w.kept("after")
			w.c.in = w.b.String()
			w.c.name = fmt.Sprintf("flow, key %s, indicator %q, open %q", key, ind, o.open)
			out = append(out, w.c)

			nl := nls[v%2]
			w = &formWriter{n: &n}
			w.kept("Back-off")
			w.text(nl + key + ": " + ind + nl)
			for range 2 + v%3 {
				w.text(markers[v%len(markers)] + nl)
			}
			w.text("  ")
			w.secret()
			w.text(nl)
			w.kept("next")
			w.text(": x" + nl)
			w.c.in = w.b.String()
			w.c.name = fmt.Sprintf("markers, key %s, indicator %q, marker %q", key, ind, markers[v%len(markers)])
			out = append(out, w.c)
		}
	}
	return out
}

// Every secret of flowMarkerCorpus is redacted and the text around it survives; at 6ed30275 the flow forms
// leaked.
func TestFlowAndMarkerFormsRedactEverySecret(t *testing.T) {
	failed := 0
	cases := flowMarkerCorpus()
	for _, c := range cases {
		leaked, lost := formLeaks(c, defaultRedactor.spans(nil, c.in))
		if len(leaked)+len(lost) > 0 {
			failed++
			if failed <= 20 {
				t.Errorf("%s:\n  in: %q\n out: %q\n leaked %q, lost %q", c.name, c.in, String(c.in), leaked, lost)
			}
		}
	}
	if failed > 0 {
		t.Errorf("%d of %d inputs leak a secret or lose kept text", failed, len(cases))
	}
}
