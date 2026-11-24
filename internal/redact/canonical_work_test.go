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

// The cost of redacting the canonical view corpus, by work count ([Work]), not by a clock (#825): the work
// String takes on each form, per byte, logged so that the fix of #845 can state its budget against it, and
// bounded like every other form (checkWork): what String adds to the reading of the release before is at most
// ownWorkPerByte per byte, and eight times the input costs at most about eight times as much.

// canonFormText returns the inputs of form joined by line breaks, repeated until the text has at least n
// bytes.
func canonFormText(cases []canonCase, form string, n int) string {
	var b strings.Builder
	for b.Len() < n {
		for _, c := range cases {
			if c.form != form {
				continue
			}
			b.WriteString(c.in)
			b.WriteByte('\n')
			if b.Len() >= n {
				break
			}
		}
	}
	return b.String()
}

// canonWork returns the work ([Work]) of String on text, and the size of text.
func canonWork(t *testing.T, text string) (work int64, size int) {
	return countWork(t, func() { String(text) }), len(text)
}

// String's work on each form of the corpus is linear in the size of the text.
func TestCanonCorpusWorkIsLinear(t *testing.T) {
	cases := canonCorpus()
	total, size := int64(0), 0
	for _, c := range cases {
		total += countWork(t, func() { String(c.in) })
		size += len(c.in)
	}
	t.Logf("String on the %d inputs one by one: %d steps for %d bytes, %.1f per byte", len(cases), total, size,
		float64(total)/float64(size))
	logMainWork(t, "corpus", total, size)
	sizes := map[string]int{}
	for _, c := range cases {
		sizes[c.form] += len(c.in) + 1
	}
	for _, form := range canonForms {
		// The smaller text holds every input of the form once (at least 16 KiB), the larger eight times as
		// much, so both hold the same mix: a form whose later inputs have something to fold (the whole text is
		// then read twice) costs more per byte than its first inputs alone, which is no growth with size.
		n := max(16<<10, sizes[form])
		small := sampleWork(t, canonFormText(cases, form, n))
		large := sampleWork(t, canonFormText(cases, form, 8*n))
		logMainWork(t, "form "+form+" once", small.total, small.size)
		logMainWork(t, "form "+form+" eight times", large.total, large.size)
		checkWork(t, "form "+form, []workSample{small, large})
	}
}

// canonStairLines are the lines of the staircase inputs (#845): each line indented one space more than the one
// before, so that every line opens a block holding all the lines after it. A bare short key refuses that block
// (no line looks like a secret), and a rule that read the block again for each header inside it was quadratic.
var canonStairLines = []string{
	"pass: |", "pass: | pass: |", "pass: | x", "a: | pass: |", "- pass: |", "x pass: |", "pass: '|", "pass:",
	"? pass", "- value: |", "pass\u200b: |", "pass\uff1a |",
	// The third round's folds: a key below "?" or "? |", a format character at the end of a key, a colon
	// look-alike of the confusables list.
	"? |", "? |\n\n", "?", "- ? >- # c", "pass\u200d", "pass\u2e2c |",
}

// canonStair returns the staircase of line with at least n bytes.
func canonStair(line string, n int) string {
	var b strings.Builder
	for i := 0; b.Len() < n; i++ {
		b.WriteString(strings.Repeat(" ", i))
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// The staircases cost the same per byte at 64 KiB, 256 KiB and 1 MiB (the log read's bound; the first line
// only, the others at the two smaller sizes, to keep the run short), as checkWork bounds it: what String adds
// to the reading of the release before is linear and at most ownWorkPerByte per byte, and that reading alone is
// linear and at most maxWorkPerByte per byte except on the shapes of releasedSuperlinear. Main's cost on
// "pass: |" is logged next to it (canonMainWork).
func TestCanonStairWorkIsLinear(t *testing.T) {
	for li, line := range canonStairLines {
		sizes := []int{64 << 10, 256 << 10, 1 << 20}
		if li > 0 {
			sizes = sizes[:2]
		}
		samples := make([]workSample, 0, len(sizes))
		for _, n := range sizes {
			w := sampleWorkAlone(t, canonStair(line, n))
			samples = append(samples, w)
			if li == 0 {
				logMainWork(t, fmt.Sprintf("stair %q %d", line, n), w.total, w.size)
			}
		}
		checkWork(t, fmt.Sprintf("stair %q", line), samples)
	}
}

// repeatedQuotedKeys is a line of n quoted strings, each a secret key with a block indicator and a word
// (#845, decision of 2026-11-18): each key looked through every later key of the line for one in its own
// string that takes a value, n*(n-1)/2 comparisons in a loop that counted none of them.
func repeatedQuotedKeys(n int) string { return strings.Repeat("\"password: | pear\" ", n) }

// The line of repeated quoted keys costs the same per byte at 512, 1,024 and 2,048 keys, as checkWork bounds it.
func TestRepeatedQuotedKeysWorkIsLinear(t *testing.T) {
	samples := make([]workSample, 0, 3)
	for _, n := range []int{512, 1024, 2048} {
		w := sampleWork(t, repeatedQuotedKeys(n))
		samples = append(samples, w)
		logMainWork(t, fmt.Sprintf("repeated quoted keys %d", n), w.total, w.size)
	}
	checkWork(t, "repeated quoted keys", samples)
}
