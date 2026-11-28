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
	"unicode/utf8"

	"github.com/shadewright-labs/shadewright/internal/redact/meter"
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
// of the collector that a fingerprint is computed from (ReplaceCheckedShort, StringShort): a fingerprint is decided
// as before. A secret key name split by a control is not read as a secret key. The shown short entry point
// (ReplaceCheckedShortShownAndRead), which the stored symptom and a module's evidence value take, reads neither this
// view nor the shown view: it reads the printed text of the entry (printed.go).
//
// The view is read with its own long windows (windowIDs), so what it reads leaves no trace in the windows the
// other readings of the same decision share. It holds the text without its controls (at most the bytes of the
// text) and one pair of offsets (8 bytes) for each run of controls removed. A match the other readings already
// hold whole is not held a second time (matchList.covers), so on text whose controls hide nothing the view adds
// no match; the heap tests (heapbound_test.go) measure its shapes against HeapWeightPerByte. Its work is linear in
// the length of the text, and the matches read on it are carried back with a binary search over the runs,
// counted on the budget.
//
// The shown view (decision of 2026-11-23). Invalid UTF-8 bytes can keep a key and its value from reading as one
// form, and the text is shown with each run of them made one "?" (strings.ToValidUTF8 in the log source, the
// context and the read tools), which joins them for the reader. Text that is not valid UTF-8 is therefore read once
// more as it is shown: without its controls and with each run of invalid bytes as one "?" (shownViewOf). Its
// matches are added the same way as those of the controls view, by the same callers, after it; a match that
// starts at a "?" starts at its run and one that ends with it ends after its run (controlsView.origEnd). Valid
// UTF-8 is not read again, and the collector's short entry points read neither view. Its heap is the view's text
// (at most the bytes of the text) and one gap for each run of controls or of invalid bytes.
//
// The escapes view (#1223, decision of 2026-11-28). The controls view drops the ESC of a colour code and keeps the
// rest ("password" ESC "[0m=V" reads "password[0m=V"), so a key in colours or a token a colour code splits is not
// read as one. Text that holds a complete escape sequence (escapeLen) is therefore read once more as a terminal
// prints it: each complete sequence removed whole, every other control removed, each run of invalid bytes one "?"
// (escapesViewOf). Its matches are added after those of the shown view, by the same callers, and carried back the
// same way. An answer of pieces is also read with each value written in another form than AsIs that holds an ESC
// or a C1 control in its escapes view and in its controls view (eachValuesViewMatch, layout.go): written on a line
// or as JSON, a value loses the ESC of its sequences, and no view of the answer can see them. The collector's short
// entry points read neither. Its heap is the view's text and one gap for each run removed.

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

// controlsGap is one run of controls removed from a text, or one run of invalid bytes written as one "?": the byte
// of the view right after it (at) and how many bytes of the text more than of the view lie before at (removed).
// repaired is set for a run of invalid bytes: the "?" right before at stands for the whole run, so a match that
// ends with it ends at the run's end.
type controlsGap struct {
	at, removed int32
	repaired    bool
}

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
	return viewOf(wm, s, false), true
}

// shownViewOf returns the shown view of s, and false when s is valid UTF-8: s as it is shown, without its
// controls and with each run of invalid bytes (bytes that start no valid UTF-8 sequence) written as one "?", as
// the log excerpts, the context and the read tools show a text made valid UTF-8 (strings.ToValidUTF8). Each run
// is a gap of its own (repaired): a match that starts at its "?" starts at the run, and one that ends with it ends
// after the run. The work is linear in the length of s.
func shownViewOf(wm *meter.Meter, s string) (controlsView, bool) {
	for i := 0; i < len(s); {
		tick(wm, 1)
		c, n := utf8.DecodeRuneInString(s[i:])
		if c == utf8.RuneError && n == 1 {
			return viewOf(wm, s, true), true
		}
		i += n
	}
	return controlsView{}, false
}

// viewOf returns s without its controls, and with each run of invalid bytes written as one "?" when repair is
// set.
func viewOf(wm *meter.Meter, s string, repair bool) controlsView {
	return viewWithout(wm, s, repair, func(i int) int { return controlLen(wm, s, i) })
}

// viewWithout returns s without the runs removed reports a length for at their first byte, and with each run of
// invalid bytes written as one "?" when repair is set. removed returns 0 at a byte that is kept.
func viewWithout(wm *meter.Meter, s string, repair bool, removed func(i int) int) controlsView {
	var b strings.Builder
	b.Grow(len(s))
	var v controlsView
	for i := 0; i < len(s); {
		tick(wm, 1)
		if n := removed(i); n > 0 {
			for i += n; i < len(s); i += n {
				tick(wm, 1)
				if n = removed(i); n == 0 {
					break
				}
			}
			v.gaps = append(v.gaps, controlsGap{at: int32(b.Len()), removed: int32(i - b.Len())}) // #nosec G115 -- an entry is at most MaxEntry bytes
			continue
		}
		if !repair {
			b.WriteByte(s[i])
			i++
			continue
		}
		if !invalidAt(s, i) {
			_, n := utf8.DecodeRuneInString(s[i:])
			b.WriteString(s[i : i+n])
			i += n
			continue
		}
		for i++; i < len(s) && invalidAt(s, i); i++ {
			tick(wm, 1)
		}
		b.WriteByte('?')
		v.gaps = append(v.gaps, controlsGap{at: int32(b.Len()), removed: int32(i - b.Len()), repaired: true}) // #nosec G115 -- an entry is at most MaxEntry bytes
	}
	v.text = b.String()
	return v
}

// eachAddedView calls f with each added view of s: its controls view when s holds a code point of [Control], then
// its shown view when s is not valid UTF-8, then its escapes view when s holds a complete escape sequence. Text
// that is valid UTF-8 without a control has none.
func eachAddedView(wm *meter.Meter, s string, f func(v controlsView)) {
	v, controls := controlsViewOf(wm, s)
	if controls {
		f(v)
	}
	if v, ok := shownViewOf(wm, s); ok {
		f(v)
	}
	if !controls {
		return // an escape sequence starts with a control: ESC or a C1 control
	}
	if v, ok := escapesViewOf(wm, s); ok {
		f(v)
	}
}

// escapesViewOf returns the escapes view of s (#1223), and false when s holds no complete escape sequence
// (escapeLen): s as a terminal prints it, with each complete escape sequence removed whole, every other code point
// of [Control] removed (a lone ESC, an escape that is not complete, a C1 control) and each run of invalid bytes
// written as one "?". A run of sequences and controls is one gap. The work is linear in the length of s: each byte
// is read a bounded number of times (escapeLen).
func escapesViewOf(wm *meter.Meter, s string) (controlsView, bool) {
	noEnd := len(s) + 1
	found := false
	for i := 0; i < len(s) && !found; i++ {
		tick(wm, 1)
		if s[i] == 0x1b || s[i] == 0xc2 {
			found = escapeLen(wm, s, i, &noEnd) > 0
		}
	}
	if !found {
		return controlsView{}, false
	}
	noEnd = len(s) + 1
	return viewWithout(wm, s, true, func(i int) int {
		if n := escapeLen(wm, s, i, &noEnd); n > 0 {
			return n
		}
		return controlLen(wm, s, i)
	}), true
}

// holdsEscape reports whether s holds an ESC or a C1 control (U+0080 to U+009F as UTF-8): a byte that may start an
// escape sequence.
func holdsEscape(wm *meter.Meter, s string) bool {
	for i := range len(s) {
		tick(wm, 1)
		if s[i] == 0x1b || (s[i] == 0xc2 && i+1 < len(s) && s[i+1] >= 0x80 && s[i+1] <= 0x9f) {
			return true
		}
	}
	return false
}

// escapeLen returns the length of the complete escape sequence that starts at s[i], or 0 when none does (ECMA-48,
// in its ESC and its C1 forms, the C1 forms as UTF-8):
//
//   - a CSI (ESC "[" or U+009B), as printed.go reads it (csiAt): parameter bytes, intermediate bytes, one final byte;
//   - a control string (OSC: ESC "]" or U+009D; DCS: ESC "P" or U+0090; SOS: ESC "X" or U+0098; PM: ESC "^" or
//     U+009E; APC: ESC "_" or U+009F) and its terminator: BEL, ST as ESC "\" or as U+009C;
//   - any other escape: ESC, at most one intermediate byte 0x23 to 0x2F, one final byte 0x30 to 0x7E ("ESC ( B",
//     "ESC # 8", "ESC 7"). A space, "!" or a quote after ESC is no intermediate byte here: "ESC" SP '"' and a value
//     would otherwise take the quote and the value's first character, and the view would read the value unquoted.
//
// A sequence that is not complete before the end of s is not one: its ESC or C1 control is a control like any
// other. noEnd is shared by the calls on one text: the first byte from which no control string has a terminator,
// once a search found none (len(s)+1 before), so no byte is searched for a terminator twice and the work stays
// linear in the length of s.
func escapeLen(wm *meter.Meter, s string, i int, noEnd *int) int {
	if i+1 >= len(s) {
		return 0
	}
	var intro byte
	switch c := s[i]; {
	case c == 0x1b:
		intro = s[i+1]
	case c == 0xc2 && s[i+1] >= 0x80 && s[i+1] <= 0x9f:
		intro = s[i+1] - 0x40 // the C1 control's 7-bit form: U+009B is ESC "["
	default:
		return 0
	}
	j := i + 2
	switch intro {
	case '[':
		if n, _, ok := csiAt(wm, s, i, j); ok {
			return n
		}
		return 0
	case ']', 'P', 'X', '^', '_':
		if j >= *noEnd {
			return 0
		}
		for k := j; k < len(s); k++ {
			tick(wm, 1)
			switch {
			case s[k] == 0x07:
				return k + 1 - i
			case (s[k] == 0x1b && k+1 < len(s) && s[k+1] == '\\') || (s[k] == 0xc2 && k+1 < len(s) && s[k+1] == 0x9c):
				return k + 2 - i
			}
		}
		*noEnd = j
		return 0
	}
	if s[i] != 0x1b {
		return 0 // any other C1 control is one control (controlLen)
	}
	j = i + 1
	if s[j] >= 0x23 && s[j] <= 0x2f {
		j++ // one intermediate byte: "ESC ( B", "ESC # 8"
	}
	if j < len(s) && s[j] >= 0x30 && s[j] <= 0x7e {
		return j + 1 - i
	}
	return 0
}

// after returns the index of the first gap of v after byte i of the view.
func (v controlsView) after(wm *meter.Meter, i int) int {
	lo, hi := 0, len(v.gaps)
	for lo < hi {
		tick(wm, 1)
		m := int(uint(lo+hi) >> 1)
		if int(v.gaps[m].at) <= i {
			lo = m + 1
		} else {
			hi = m
		}
	}
	return lo
}

// orig returns the byte of the text that byte i of the view is (the first byte of a run of invalid bytes for its
// "?").
func (v controlsView) orig(wm *meter.Meter, i int) int {
	lo := v.after(wm, i)
	if lo == 0 {
		return i
	}
	return i + int(v.gaps[lo-1].removed)
}

// origEnd returns the end in the text of a match that ends at byte end of the view (end > 0): right after the
// byte of its last byte, or after the whole run of invalid bytes its last byte, a "?", stands for. The controls
// right after a match are not in it.
func (v controlsView) origEnd(wm *meter.Meter, end int) int {
	lo := v.after(wm, end-1)
	if lo < len(v.gaps) && int(v.gaps[lo].at) == end && v.gaps[lo].repaired {
		return end + int(v.gaps[lo].removed)
	}
	if lo == 0 {
		return end
	}
	return end + int(v.gaps[lo-1].removed)
}

// eachControlsMatch calls add with each match of the reading of the controls view v of a text (its rules, in its
// folded view too, with long windows of its own), counted on the bytes of the text it was read from, the controls
// inside it included, with the index of its rule, in no order.
func (r *Redactor) eachControlsMatch(wm *meter.Meter, v controlsView, add func(start, end, rule int)) {
	r.eachRuleMatch(wm, v.text, &windowIDs{}, func(start, end, rule int) {
		tick(wm, 1)
		add(v.orig(wm, start), v.origEnd(wm, end), rule)
	})
}

// addControlsView adds to l, which holds the matches of the readings of s as it is, the matches of the controls
// view of s that l does not already hold whole, when s holds a code point of [Control], then those of its shown
// view, when s is not valid UTF-8; for valid UTF-8 without a control it does nothing. A match l holds whole would add no byte to what is hidden, so it is not held a second time: the
// heap of the view is its text, its gaps and the matches that add to what is hidden (#852, decision of
// 2026-11-22 on the heap). The matches l holds are never changed or dropped.
func (r *Redactor) addControlsView(wm *meter.Meter, s string, l *matchList) {
	eachAddedView(wm, s, func(v controlsView) {
		l.merge(wm)
		r.eachControlsMatch(wm, v, func(start, end, rule int) {
			if !l.covers(wm, start, end) {
				l.add(wm, start, end, rule)
			}
		})
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
