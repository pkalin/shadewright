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

	"github.com/pkalin/shadewright/internal/redact/meter"
)

// The controls view (#852, decisions of 2026-11-22). A control character can split what a reader of the text sees
// as one key and value: "pass" NUL "word=V", a token with an escape inside it, a flag and its value with a C1
// code point between them. The rules read every text as it is (and in its folded view), and then once more without
// its controls: every code point of Unicode category Cc (C0, DEL, C1) except newline and tab is left out, every
// other byte is kept, an invalid one included. What that reading finds is added to what the readings of the text
// as it is found, each match counted on the bytes of the text it was read from, the controls inside it included
// (addControlsView: String, StringTwice, ReplaceFrom, Strings and each piece and the whole answer of Pieces).
// Nothing in the text is changed before the readings run, the readings of the text as it is run first and
// unchanged, and the readings of the release before (before.go) read the text as it is: so the controls view can
// only add to what is hidden, never take away a byte another reading hides. The text keeps its controls; the
// caller that shows a text without them (the model text the analysis loop stores) removes them from the redacted
// text, after every marker is in place, and the markers hold none.
//
// Not read without controls, as on origin/main: the decisions that read a key alone (SecretKey,
// SecretCollection), the forms a tool writes a value in (write), the cut of shown text, and the short entry points
// of the collector (shortentry.go), whose callers redact, change the text and redact again: a fingerprint, a
// symptom and a module's evidence value are decided as before. A secret key name split by a control is not read as
// a secret key, and a secret a control splits in an event note's symptom is shown as on origin/main.
//
// The view is read with its own long windows (windowIDs), so what it reads leaves no trace in the windows the
// other readings of the same decision share. It holds the text without its controls (at most the bytes of the
// text) and one pair of offsets (8 bytes) for each run of controls removed. A match the other readings already
// hold whole is not held a second time (matchList.covers), so on text whose controls hide nothing the view adds
// no match; the heap tests (heapbound_test.go) measure its shapes against HeapWeightPerByte. Its work is linear in
// the length of the text, and the matches read on it are carried back with a binary search over the runs,
// counted on the budget.

// Control reports whether r is a code point the controls view leaves out, and that model text is stored without:
// Unicode category Cc (U+0000 to U+001F, U+007F to U+009F) except newline and tab.
func Control(r rune) bool {
	return (r < 0x20 && r != '\n' && r != '\t') || (r >= 0x7f && r <= 0x9f)
}

// controlLen returns the length of the code point of Control at s[i], or 0.
func controlLen(_ *meter.Meter, s string, i int) int {
	switch c := s[i]; {
	case c < 0x20:
		if c != '\n' && c != '\t' {
			return 1
		}
	case c == 0x7f:
		return 1
	case c == 0xc2:
		if i+1 < len(s) && s[i+1] >= 0x80 && s[i+1] <= 0x9f {
			return 2
		}
	}
	return 0
}

// WithoutControls returns s with every code point of [Control] removed; every other byte, an invalid one
// included, is kept. Text without one comes back as it is. The work is linear in the length of s, and it runs
// outside any budget: callers remove the controls from text a decision inside its budget returned.
func WithoutControls(s string) string {
	v, ok := controlsViewOf(nil, s)
	if !ok {
		return s
	}
	return v.text
}

// hasControl reports whether s holds a code point of [Control].
func hasControl(wm *meter.Meter, s string) bool {
	for i := range len(s) {
		tick(wm, 1)
		if controlLen(wm, s, i) > 0 {
			return true
		}
	}
	return false
}

// controlsGap is one run of controls removed from a text: the byte of the view right after it (at) and how many
// bytes of the text were removed up to and including it (removed).
type controlsGap struct{ at, removed int32 }

// controlsView is the controls view of a text: the text without its controls, and the runs removed.
type controlsView struct {
	text string
	gaps []controlsGap
}

// controlsViewOf returns the controls view of s, and false when s holds no code point of [Control]. The work is
// linear in the length of s; it allocates the view's text and one gap for each run of controls.
func controlsViewOf(wm *meter.Meter, s string) (controlsView, bool) {
	if !hasControl(wm, s) {
		return controlsView{}, false
	}
	var b strings.Builder
	b.Grow(len(s))
	var v controlsView
	for i := 0; i < len(s); {
		tick(wm, 1)
		if n := controlLen(wm, s, i); n > 0 {
			for i += n; i < len(s); i += n {
				tick(wm, 1)
				if n = controlLen(wm, s, i); n == 0 {
					break
				}
			}
			v.gaps = append(v.gaps, controlsGap{at: int32(b.Len()), removed: int32(i - b.Len())}) // #nosec G115 -- an entry is at most MaxEntry bytes
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	v.text = b.String()
	return v, true
}

// orig returns the byte of the text that byte i of the view is.
func (v controlsView) orig(wm *meter.Meter, i int) int {
	lo, hi := 0, len(v.gaps) // the first gap after i
	for lo < hi {
		tick(wm, 1)
		m := int(uint(lo+hi) >> 1)
		if int(v.gaps[m].at) <= i {
			lo = m + 1
		} else {
			hi = m
		}
	}
	if lo == 0 {
		return i
	}
	return i + int(v.gaps[lo-1].removed)
}

// eachControlsMatch calls add with each match of the reading of the controls view v of a text (its rules, in its
// folded view too, with long windows of its own), counted on the bytes of the text it was read from, the controls
// inside it included, with the index of its rule, in no order.
func (r *Redactor) eachControlsMatch(wm *meter.Meter, v controlsView, add func(start, end, rule int)) {
	r.eachRuleMatch(wm, v.text, &windowIDs{}, func(start, end, rule int) {
		tick(wm, 1)
		add(v.orig(wm, start), v.orig(wm, end-1)+1, rule)
	})
}

// addControlsView adds to l, which holds the matches of the readings of s as it is, the matches of the controls
// view of s that l does not already hold whole, when s holds a code point of [Control]; for text without one it
// does nothing. A match l holds whole would add no byte to what is hidden, so it is not held a second time: the
// heap of the view is its text, its gaps and the matches that add to what is hidden (#852, decision of
// 2026-11-22 on the heap). The matches l holds are never changed or dropped.
func (r *Redactor) addControlsView(wm *meter.Meter, s string, l *matchList) {
	v, ok := controlsViewOf(wm, s)
	if !ok {
		return
	}
	l.merge(wm)
	r.eachControlsMatch(wm, v, func(start, end, rule int) {
		if !l.covers(wm, start, end) {
			l.add(wm, start, end, rule)
		}
	})
}

// covers reports whether the matches of l merged so far hold all of [start, end). They are sorted and disjoint
// (mergeMatches), so one binary search finds the only range that can.
func (l *matchList) covers(wm *meter.Meter, start, end int) bool {
	ms := l.m[:l.merged]
	lo, hi := 0, len(ms) // the first range that starts after start
	for lo < hi {
		tick(wm, 1)
		m := int(uint(lo+hi) >> 1)
		if int(ms[m].start) <= start {
			lo = m + 1
		} else {
			hi = m
		}
	}
	return lo > 0 && int(ms[lo-1].end) >= end
}
