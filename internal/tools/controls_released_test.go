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

package tools

import (
	"slices"
	"strings"
	"testing"

	"github.com/pkalin/shadewright/internal/redact/released"
)

// Removing control characters from shown text uncovers nothing the release before hid (#852): for every input of
// the control corpus and every place that shows it, no text of eight bytes or more that the redactor of that
// release (package released, kept unchanged) hid in the input is in the output, even once the controls of both
// are removed. Under -race every seventh case runs (seven is prime to the twelve cases of a control, so every
// position is sampled).
func TestControlCorpusHidesWhatTheReleaseBeforeHid(t *testing.T) {
	cases := controlCases()
	if raceDetector {
		cases = slices.Collect(func(yield func(controlCase) bool) {
			for i := 0; i < len(cases); i += 7 {
				if !yield(cases[i]) {
					return
				}
			}
		})
	}
	checked, failed := 0, 0
	for _, e := range controlEntries(t) {
		for _, c := range cases {
			var hidden []string
			for _, sp := range released.Spans(c.in) {
				if h := corpusStrip(c.in[sp.Start:sp.End]); len(h) >= 8 {
					hidden = append(hidden, h)
				}
			}
			if len(hidden) == 0 {
				continue
			}
			checked++
			out := corpusStrip(e.call(t, c.in))
			for _, h := range hidden {
				if strings.Contains(out, h) {
					failed++
					if failed <= 10 {
						t.Errorf("%s, %s: shows %q, which the release before hid\n  in: %q\n out: %q", e.name, c.name, h, c.in, out)
					}
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("the release before hid nothing in the corpus: the check checks nothing")
	}
	t.Logf("%d outputs checked against what the release before hid", checked)
}
