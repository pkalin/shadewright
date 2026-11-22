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
	"strings"
	"testing"

	"github.com/pkalin/shadewright/internal/redact/redacttest"
	"github.com/pkalin/shadewright/internal/redact/released"
)

func init() { diffBodies["FuzzDiffStringShown"] = diffStringShown }

// FuzzDiffStringShown checks the two decisions on a text shown without its control characters (#852):
// StringShownChecked, which the reviewer hint is decided with, against the release's String (its one pass where
// origin/main called StringKeeping there), and StringTwiceShownChecked, which incidentcontext.Value, Redact and
// RenderLog decide with, against the release's reading where origin/main redacted a text twice (P1, P3, P4, P5),
// and:
//
//	P6: what this package's rules find in the text without its controls is hidden in the decided text once its
//	    controls are removed (WithoutControls), as the caller shows it: removing the controls after the decision
//	    uncovers nothing the rules would hide in the text as shown.
func FuzzDiffStringShown(f *testing.F) {
	diffSeeds := append(diffSeeds,
		redacttest.RawSeed(0, "pass\x1bword=\x01"),
		redacttest.RawSeed(0, "to\x00ken: \x01\rnext: x"),
		redacttest.RawSeed(0, "password: >\r  \x01\rnext: x"),
	)
	for _, s := range diffSeeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		fails, counted := diffStringShown(data)
		checkDiff(t, "FuzzDiffStringShown", data, counted, fails)
	})
}

func diffStringShown(data []byte) ([]string, bool) {
	in := redacttest.Decode(data, 1).Joined("")
	const keep = "api"
	var shown, twice string
	var okShown, okTwice bool
	totalShown, releaseShown, counted := measureWork(func() { shown, okShown = StringShownChecked(in, keep) })
	totalTwice, releaseTwice, _ := measureWork(func() { twice, okTwice = StringTwiceShownChecked(in, nil) })
	fails := workFails("StringShown", totalShown, releaseShown, len(in))
	fails = append(fails, workFails("StringTwiceShown", totalTwice, releaseTwice, len(in))...)
	fails = append(fails, withheldFails("StringShown", in, []string{shown}, okShown)...)
	fails = append(fails, withheldFails("StringTwiceShown", in, []string{twice}, okTwice)...)
	shownView := WithoutControls(strings.ToValidUTF8(in, "?"))
	rules := redacttest.Hiding(shownView, render(nil, shownView, settle(nil, shownView, defaultRedactor.ruleMatches(nil, shownView))))
	if okShown {
		fails = append(fails, decidedFails("StringShown", []string{shown},
			[]string{defaultRedactor.stringShown(nil, in, []string{keep})})...)
		rec := redacttest.NewRecorder()
		rec.Wrap(released.String)(in)
		fails = append(fails, leaks(rec, "StringShown", shown)...)
		fails = append(fails, utf8Fails("StringShown", in, shown)...)
		out := WithoutControls(strings.ToValidUTF8(shown, "?"))
		if l := rules.Leaks(out); len(l) > 0 && !strings.Contains(in, keep) {
			fails = append(fails, redacttest.LeakLine("P6", "StringShown shows what the rules hide in the text as shown", l, out))
		}
	}
	if okTwice {
		fails = append(fails, decidedFails("StringTwiceShown", []string{twice},
			[]string{defaultRedactor.stringTwiceShown(nil, in, nil)})...)
		rec := redacttest.NewRecorder()
		releasedTwice(rec)(in)
		fails = append(fails, leaks(rec, "StringTwiceShown", twice)...)
		fails = append(fails, utf8Fails("StringTwiceShown", in, twice)...)
		out := WithoutControls(strings.ToValidUTF8(twice, "?"))
		if l := rules.Leaks(out); len(l) > 0 {
			fails = append(fails, redacttest.LeakLine("P6", "StringTwiceShown shows what the rules hide in the text as shown", l, out))
		}
	}
	return fails, counted
}
