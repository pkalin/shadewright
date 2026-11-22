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

// Text from the cluster is shown to the model without its control characters (#852): every code point of
// Unicode category Cc (C0, DEL, C1) except newline and tab is removed, the rest is kept. Removing one can join
// what it separated into a secret's form ("pass" ESC "word=V", a token with a NUL in its body), so the decision on
// such text reads it that way too. The controls view of a text is the text without those code points; the rules
// read it as one more view of the same text, with its own folded view, and its matches count on the bytes of the
// text they were read from (shownMatches), as the folded view's do. Nothing is removed before the decision: the
// text as it was read is still read by every rule and by the reading of the release before, so the controls view
// only adds to what is hidden, and a control that is the indentation or the line end placing a value in a block
// still does so for those readings. The caller removes the controls from the decided text only
// (incidentcontext.ShownText), so the text shown is the controls view with every match in it replaced: no byte a
// reading hid comes back. A run of invalid bytes is never joined into a character on the way: the caller replaces
// each run before the controls go ("\xc3" ESC "\xa9" shows "??", not "é").
//
// The controls view holds the text without its controls (at most the bytes of the text) and one pair of
// offsets for each run of controls removed, never an offset for each byte, so a decision that reads it holds at
// most one more copy of the text on the heap (budget.go, HeapPerByte). Its work is linear in the length of the
// text, and the matches read on it are carried back with a binary search over the runs, counted on the budget.
//
// Only text that is shown without its controls is decided this way ([Redactor.StringShownChecked],
// [Redactor.StringTwiceShownChecked]; the Line and JSON forms of a piece, which leave controls out of the answer that
// reading 2 of [Redactor.Pieces] reads). String and the other entry points keep reading the text as it is: where
// the controls stay, the lines a carriage return ends stay apart, and joining them would only hide more of them.

// Control reports whether r is a code point shown text never holds: Unicode category Cc (U+0000 to U+001F,
// U+007F to U+009F) except newline and tab.
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
func WithoutControls(s string) string { return withoutControls(nil, s) }

// withoutControls is WithoutControls on the budget wm.
func withoutControls(wm *meter.Meter, s string) string {
	v, ok := controlsViewOf(wm, s)
	if !ok {
		return s
	}
	return v.text
}

// hasControl reports whether s holds a code point of [Control].
func hasControl(wm *meter.Meter, s string) bool {
	for i := 0; i < len(s); i++ {
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
	removed := 0
	for i := 0; i < len(s); {
		tick(wm, 1)
		if n := controlLen(wm, s, i); n > 0 {
			for i += n; i < len(s); i += n {
				tick(wm, 1)
				if n = controlLen(wm, s, i); n == 0 {
					break
				}
			}
			removed = i - b.Len()
			v.gaps = append(v.gaps, controlsGap{at: int32(b.Len()), removed: int32(removed)}) // #nosec G115 -- an entry is at most MaxEntry bytes
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

// shownMatches returns ruleMatches of s and, when s holds a code point of [Control], ruleMatches of its controls
// view, each counted on the bytes of s it was read from, the controls inside it included.
func (r *Redactor) shownMatches(wm *meter.Meter, s string) []span {
	spans := r.ruleMatches(wm, s)
	if v, ok := controlsViewOf(wm, s); ok {
		n := len(spans)
		spans = append(spans, r.ruleMatches(wm, v.text)...)
		for i := n; i < len(spans); i++ {
			tick(wm, 1)
			spans[i].start, spans[i].end = v.orig(wm, spans[i].start), v.orig(wm, spans[i].end-1)+1
		}
	}
	return spans
}

// StringShownChecked is StringKeepingChecked for a text the model is shown without its control characters: what
// the rules find in s and in s without them (its controls view) is hidden, and what the release before hid in s.
// The result still holds the controls; the caller removes them (incidentcontext.ShownText). ok is false when the
// entry is withheld (budget.go), and out is then Withheld. A reviewer hint is shown this way.
func (r *Redactor) StringShownChecked(s string, keep ...string) (out string, ok bool) {
	return meteredText(len(s), func(wm *meter.Meter) string { return r.stringShown(wm, s, keep) })
}

// stringShown is StringShownChecked on the budget wm.
func (r *Redactor) stringShown(wm *meter.Meter, s string, keep []string) string {
	spans := settle(wm, s, append(r.shownMatches(wm, s), r.releasedString(wm, s, false, nil)...))
	if kept := wholeOccurrences(wm, s, keep); len(kept) > 0 {
		out := spans[:0]
		for _, sp := range spans {
			tick(wm, 1)
			if !covered(wm, sp, kept) {
				out = append(out, sp)
			}
		}
		spans = out
	}
	return render(wm, s, spans)
}

// StringShownChecked is the package's StringShownChecked with the built-in rules.
func StringShownChecked(s string, keep ...string) (out string, ok bool) {
	return defaultRedactor.StringShownChecked(s, keep...)
}

// StringTwiceShownChecked is StringTwiceChecked for a text the model is shown without its control characters: it
// also hides what the rules find in s without them (its controls view). incidentcontext.Value, Redact and
// RenderLog decide with it. The result still holds the controls; the caller removes them
// (incidentcontext.ShownText). ok is false when the entry is withheld, and out is then Withheld.
func (r *Redactor) StringTwiceShownChecked(s string, cut func(string) int) (out string, ok bool) {
	return meteredText(len(s), func(wm *meter.Meter) string {
		return render(wm, s, settle(wm, s, append(r.shownMatches(wm, s), r.releasedString(wm, s, true, cut)...)))
	})
}

// StringTwiceShownChecked is the package's StringTwiceShownChecked with the built-in rules.
func StringTwiceShownChecked(s string, cut func(string) int) (out string, ok bool) {
	return defaultRedactor.StringTwiceShownChecked(s, cut)
}
