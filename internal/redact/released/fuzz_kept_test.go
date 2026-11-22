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

package released

import (
	"strings"
	"testing"
)

// keptFuzzBound is the longest input FuzzReleasedMatchesKeptCopy compares. The kept copy's work grows faster than
// its input on some shapes (#873: "--token,}" repeated, the repeated label unit of FuzzDiffBuild, a staircase of
// "pass: |"), so the fuzzer would slow down on long inputs; a longer input is passed over, not compared.
const keptFuzzBound = 4 << 10

// FuzzReleasedMatchesKeptCopy fuzzes the parity of this package, rewritten by #873 so that its work is linear,
// with the kept copy of the release's code (testdata/v052): on every input of at most keptFuzzBound bytes both
// find the same spans, byte for byte and with the same kinds (decision of 2026-11-21 on #873). The corpus tests
// (kept_test.go, and TestReleasedSpansMatchKeptCopy in package redact) table the same comparison.
func FuzzReleasedMatchesKeptCopy(f *testing.F) {
	for _, s := range []string{
		"pass: |\n  hunter2\n",
		strings.Repeat("--token,}", 12),
		strings.Repeat("-password,", 12),
		strings.Repeat(`TOKEN="a"`, 12) + "\"\n" + strings.Repeat(" ", 300) + "\"\uff1a",
		strings.Repeat(`auth="a"`, 12) + "\uff1ax",
		strings.Repeat(fuzzLabelUnit, 4),
		"password=" + strings.Repeat(`"a",`, 20) + "token=x",
		"a: | pass: |\n   b\n    Qz7aBcDeFgHiJkLmNoPqRsTuV\nc: d\n",
		"metric{" + strings.Repeat(fuzzLabelKeyUnit, 3) + "=" + strings.Repeat(fuzzLabelUnit, 3) + ",pod=web} 1",
	} {
		f.Add(s)
	}
	for i, line := range stairLines {
		var b strings.Builder
		for d := range 4 + i%4 {
			b.WriteString(strings.Repeat(" ", d) + line + "\n")
		}
		f.Add(b.String())
	}
	f.Fuzz(func(t *testing.T, s string) {
		if len(s) > keptFuzzBound {
			return
		}
		if diff := sameAsKept(s); diff != "" {
			t.Error(diff)
		}
	})
}
