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
)

// The cost of redacting the canonical view corpus, by work count ([Work]), not by a clock (#825): the work
// String takes on each form, per byte, logged so that the fix of #845 can state its budget against it, and
// bounded like every other form: at most maxWorkPerByte per byte, and eight times the input costs at most
// about eight times as much.

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
	for _, form := range canonForms {
		ws, small := canonWork(t, canonFormText(cases, form, 16<<10))
		wl, large := canonWork(t, canonFormText(cases, form, 128<<10))
		perSmall, perLarge := float64(ws)/float64(small), float64(wl)/float64(large)
		t.Logf("%s: %.1f steps per byte on %d bytes, %.1f on %d", form, perSmall, small, perLarge, large)
		if wl > int64(maxWorkPerByte*large) {
			t.Errorf("%s: %d steps for a %d-byte input, more than %d per byte", form, wl, large, maxWorkPerByte)
		}
		if perLarge > 1.125*perSmall+0.5 {
			t.Errorf("%s: %d steps for %d bytes, %d for %d: more than linear", form, ws, small, wl, large)
		}
	}
}
