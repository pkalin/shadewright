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
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/pkalin/shadewright/internal/redact/released"

	"github.com/pkalin/shadewright/internal/redact/meter"
)

// The reading of the release before (#845, thirteenth decision of 2026-11-18). The redactor of the release
// before #845 is kept unchanged in package released, and every decision of this package also applies it, to the
// same text, at the same places and in the same order of passes as that release applied it: String, Replace,
// StringKeeping and Clean apply one pass, where that release called String once; StringTwice applies what
// incidentcontext.Redact applied (redact, make valid UTF-8, redact again) and, for a log that get_pod_logs fitted
// again, that, a cut and that again; ReplaceFrom what the log source applied (redact, cut, make valid UTF-8,
// redact again); Pieces what the read tools applied to an answer (each value redacted twice, written into the
// answer as the tool wrote it, then the whole answer redacted twice). Each pass of that release read the text
// the pass before it wrote, its markers in place; here each of its texts is traced back to the bytes of the text
// decided on, so the spans its passes found are spans of that text. They are added to the spans of this
// package's own readings, and nothing is replaced until all have run. So everything that release hid is hidden
// because its own code says so, whatever this package's rules do. The reading and package released are retired
// together, by their own issue, once a release has run with both.

// releasedReading applies the redactor of the release before and collects what its passes hide, as spans of
// the text decided on. The callers widen those spans to whole characters (onRunes) before they return them: that
// release cut a value at a byte bound (maxValueExtent) and could end a span inside a character, which left the
// rest of the character after the marker (#845, P4).
type releasedReading struct {
	r *released.Redactor
	// rule is the rule index the spans take: after every rule of this package's redactor, so that on a tie a
	// span of this package's rules names the merged range, as it did before the reading was added.
	rule int
	// hidden holds what the passes hide, as spans of onto, the text decided on, each widened to its characters
	// (onRunes) as it is found, merged as they come (#1087: the spans of every pass were held whole until the
	// end).
	hidden *matchList
	onto   string
	// read holds what each text a pass read was found to hold: a value repeated in the pieces of an answer (a
	// label in the series of every metric template) is read by that release's rules once (#873, second round).
	// The spans depend on the text only. A text longer than maxReadKept bytes is kept only when the passes read
	// pieces (pieces), which repeat: the answer or the string a pass reads whole is read once, and is not held to
	// the end with its spans (#1087).
	read   map[string][]released.Span
	pieces bool
}

// maxReadKept is the longest text whose spans a releasedReading keeps the first time it reads it.
const maxReadKept = 4 << 10

// hide adds the span [lo, hi) of the text decided on, of the kind given, widened to its characters.
func (x *releasedReading) hide(wm *meter.Meter, lo, hi int, kind string) {
	if hi > lo {
		x.hidden.addKind(wm, runeFloor(wm, x.onto, lo), runeCeil(wm, x.onto, hi), x.rule, kind)
	}
}

// releasedReading returns a reading with the redactor of the release before that has r's extra patterns, or
// nil when r has none (a redactor made of some of the rules only, in a test).
func (r *Redactor) releasedReading(_ *meter.Meter, onto string, into *matchList) *releasedReading {
	if r.before == nil {
		return nil
	}
	heapGate(into.added, len(onto))
	return &releasedReading{r: r.before, rule: len(r.rules), hidden: into, onto: onto}
}

// pass is one call of String of the release before on t: the spans it finds are hidden, and the text it returns
// has its markers in place.
func (x *releasedReading) pass(wm *meter.Meter, t trace) trace {
	found, ok := x.read[t.text]
	switch {
	case ok:
		tick(wm, len(t.text)) // the lookup hashes the text
	case len(t.text) > maxReadKept && !x.pieces:
		found = x.r.SpansWithin(wm, t.text)
	default:
		found = x.r.SpansWithin(wm, t.text)
		if x.read == nil {
			x.read = make(map[string][]released.Span)
		}
		x.read[t.text] = found
	}
	if len(found) == 0 {
		return t
	}
	var tb traceBuilder
	pos := 0
	for _, f := range found {
		tick(wm, 1)
		tb.copy(wm, t, pos, f.Start)
		lo, hi := t.lo(f.Start), t.hi(f.End-1)
		x.hide(wm, int(lo), int(hi), f.Kind)
		tb.add(wm, released.MarkerWithin(wm, f.Kind, f.End-f.Start), lo, hi)
		pos = f.End
	}
	tb.copy(wm, t, pos, len(t.text))
	return tb.trace(wm)
}

// redact is incidentcontext.Redact of the release before on t: String, the result made valid UTF-8 (each
// invalid run becomes one "?"), and String again.
func (x *releasedReading) redact(wm *meter.Meter, t trace) trace {
	return x.pass(wm, validTrace(wm, x.pass(wm, t)))
}

// validTrace is strings.ToValidUTF8(t, "?") on a trace: each run of invalid bytes becomes one "?" that stands
// for the whole run.
func validTrace(wm *meter.Meter, t trace) trace {
	tick(wm, len(t.text))
	if utf8.ValidString(t.text) {
		return t
	}
	var tb traceBuilder
	pos := 0
	for i := 0; i < len(t.text); {
		tick(wm, 1)
		c, size := utf8.DecodeRuneInString(t.text[i:])
		if c != utf8.RuneError || size != 1 {
			i += size
			continue
		}
		tb.copy(wm, t, pos, i)
		j := i + 1
		for j < len(t.text) {
			tick(wm, 1)
			if c, size := utf8.DecodeRuneInString(t.text[j:]); c != utf8.RuneError || size != 1 {
				break
			}
			j++
		}
		tb.add(wm, "?", t.lo(i), t.hi(j-1))
		i, pos = j, j
	}
	tb.copy(wm, t, pos, len(t.text))
	return tb.trace(wm)
}

// maxShownBytes is incidentcontext's bound of a value on a line (maxValueBytes): a longer value, once redacted,
// is left out whole (TestReleasedShownMatchesValue checks the two agree).
const maxShownBytes = 1024

// shownTrace is the rest of incidentcontext.Value of the release before on a value it redacted: a value longer
// than maxShownBytes is left out whole and replaced by a note of its size, which stands for all of it; a
// control character, U+2028 and U+2029 become a space. t is valid UTF-8.
func shownTrace(wm *meter.Meter, t trace) trace {
	if len(t.text) > maxShownBytes {
		var tb traceBuilder
		tb.add(wm, "("+strconv.Itoa(len(t.text))+" bytes left out)", t.lo(0), t.hi(len(t.text)-1))
		return tb.trace(wm)
	}
	var tb traceBuilder
	pos := 0
	for i := 0; i < len(t.text); {
		tick(wm, 1)
		c, size := utf8.DecodeRuneInString(t.text[i:])
		if unicode.IsControl(c) || c == ' ' || c == ' ' {
			tb.copy(wm, t, pos, i)
			tb.add(wm, " ", t.lo(i), t.hi(i+size-1))
			pos = i + size
		}
		i += size
	}
	tb.copy(wm, t, pos, len(t.text))
	return tb.trace(wm)
}

// jsonTrace writes t as the content of a JSON string, as encoding/json does without HTML escapes (writeUnit),
// each character's escape standing for the character.
func jsonTrace(wm *meter.Meter, t trace) trace {
	var tb traceBuilder
	n := 0
	for i := 0; i < len(t.text); {
		c, size := utf8.DecodeRuneInString(t.text[i:])
		n += unitWidth(JSON, c, size)
		i += size
	}
	tb.b.Grow(n) // sized once, so the text is not held twice while it grows
	for i := 0; i < len(t.text); {
		tick(wm, 1)
		c, size := utf8.DecodeRuneInString(t.text[i:])
		at := tb.b.Len()
		writeUnit(wm, &tb.b, JSON, t.text[i:i+size], c, size) // each unit written in place, as add writes it
		tick(wm, tb.b.Len()-at)
		tb.push(at, t.lo(i), int32(tb.b.Len()-at), t.hi(i+size-1)-t.lo(i)) // #nosec G115 -- a unit is at most six bytes
		i += size
	}
	return tb.trace(wm)
}

// releasedPieces returns what the release before hid in the answer the pieces make, as spans of the pieces'
// texts joined as they were read (a piece the tool writes included): its read tools redacted each value as it
// was read (incidentcontext.Redact; no map key alone, which encoding/json wrote as it was), wrote it into the
// answer (a value on a line as incidentcontext.Value showed it, a value in JSON as encoding/json wrote it), and
// redacted the whole answer the same way before they cut it. A map key also loses what that release hid in the
// same text as a value. nil when r has no such reading.
func (r *Redactor) releasedPieces(wm *meter.Meter, ps []Piece) []span {
	if r.before == nil {
		return nil
	}
	n := 0
	for _, p := range ps {
		n += len(p.Text)
	}
	var read strings.Builder
	read.Grow(n)
	for _, p := range ps {
		tick(wm, 1)
		read.WriteString(p.Text)
	}
	l := r.newMatchList()
	x := r.releasedReading(wm, read.String(), l)
	x.pieces = true
	var tb traceBuilder
	off := 0
	for _, p := range ps {
		tick(wm, 1)
		t := traceOf(wm, p.Text, off)
		if p.Data {
			if !p.MapKey {
				t = x.redact(wm, t)
			} else {
				// That release wrote a map key as it was, but the same text as a value it redacted alone: what it
				// hides there is hidden in the key too, which keeps its text for the passes over the answer
				// (#845, fifteenth decision: a label key and a condition reason of one object, one text).
				x.redact(wm, t)
			}
			switch p.Form {
			case Line:
				t = shownTrace(wm, t)
			case JSON:
				t = jsonTrace(wm, t)
			}
		}
		tb.copy(wm, t, 0, len(t.text))
		off += len(p.Text)
	}
	x.redact(wm, tb.trace(wm))
	return l.spans(wm)
}

// releasedString returns what the release before hid in s, as spans of s: one call of its String when twice
// is false (String, StringKeeping, Replace and Clean, at the places it called String once); when twice is true,
// what it hid where it redacted s, made it valid UTF-8 and redacted it again (incidentcontext.Redact), and, when
// cut is not nil, then kept the part of that text from cut(text) on and redacted that part the same way (a log
// excerpt at get_pod_logs, which RenderLog cut to its newest lines and FitLog redacted again). nil when r has no
// such reading.
func (r *Redactor) releasedString(wm *meter.Meter, s string, twice bool, cut func(string) int) []span {
	l := r.newMatchList()
	r.releasedStringInto(wm, s, twice, cut, l)
	return l.spans(wm)
}

// releasedStringInto is releasedString with the spans added to l, merged with what it holds.
func (r *Redactor) releasedStringInto(wm *meter.Meter, s string, twice bool, cut func(string) int, l *matchList) {
	x := r.releasedReading(wm, s, l)
	if x == nil {
		return
	}
	if !twice {
		x.pass(wm, traceOf(wm, s, 0))
		return
	}
	t := x.redact(wm, traceOf(wm, s, 0))
	if cut != nil {
		from := min(max(cut(t.text), 0), len(t.text))
		x.redact(wm, t.from(from))
	}
}

// releasedExcerpt returns what the release before hid in a log as the log source read it (s) when it kept the
// bytes from from on: it redacted all of s (Replace), wrote what the cut kept (a match the cut runs through as
// the marker of its part kept), made that valid UTF-8 and redacted it again (String). With drop not nil, also
// what it hid where the reader of that excerpt dropped its first drop(text) bytes and redacted the rest as
// incidentcontext.Redact did, twice (RenderLog, then get_pod_logs fitting the text again), each time on the text
// with that release's markers in place (#845, fifteenth decision). nil when r has no such reading.
func (r *Redactor) releasedExcerpt(wm *meter.Meter, s string, from int, drop func(string) int) []span {
	l := r.newMatchList()
	r.releasedExcerptInto(wm, s, from, drop, l)
	return l.spans(wm)
}

// releasedExcerptInto is releasedExcerpt with the spans added to l, merged with what it holds.
func (r *Redactor) releasedExcerptInto(wm *meter.Meter, s string, from int, drop func(string) int, l *matchList) {
	x := r.releasedReading(wm, s, l)
	if x == nil {
		return
	}
	from = min(max(from, 0), len(s))
	t := traceOf(wm, s, 0)
	var tb traceBuilder
	pos := from
	for _, f := range x.r.SpansWithin(wm, s) {
		tick(wm, 1)
		x.hide(wm, f.Start, f.End, f.Kind)
		if f.End <= from {
			continue
		}
		start := max(f.Start, from)
		tb.copy(wm, t, pos, start)
		tb.add(wm, released.MarkerWithin(wm, f.Kind, f.End-start), int32(start), int32(f.End)) // #nosec G115 -- a text is far below 2 GiB
		pos = f.End
	}
	tb.copy(wm, t, pos, len(s))
	ex := x.pass(wm, validTrace(wm, tb.trace(wm)))
	if drop != nil {
		d := min(max(drop(ex.text), 0), len(ex.text))
		x.redact(wm, x.redact(wm, ex.from(d)))
	}
}

// StringTwice is String for a text the release before redacted, made valid UTF-8 and redacted again
// (incidentcontext.Redact, where incidentcontext.Value and the read tools called it): it hides what the rules of
// this package find in s and what that release hid there. With cut not nil it also hides what that release hid
// when it then kept the part of its text from cut(text) on (cut is given the text as that release held it) and
// redacted that part the same way: RenderLog for a log excerpt that get_pod_logs fitted again (#845, thirteenth
// decision of 2026-11-18).
//
// An entry it withholds (budget.go) is Withheld, as text; [Redactor.StringTwiceChecked] says so with a flag.
func (r *Redactor) StringTwice(s string, cut func(string) int) string {
	out, _ := r.StringTwiceChecked(s, cut)
	return out
}

// StringTwiceChecked is StringTwice with a flag: ok is false when the entry is withheld, and the text is then
// Withheld.
func (r *Redactor) StringTwiceChecked(s string, cut func(string) int) (out string, ok bool) {
	return meteredText(r, len(s), func(wm *meter.Meter) string { return r.stringTwice(wm, s, cut) })
}

// stringTwice is StringTwice on the budget wm.
func (r *Redactor) stringTwice(wm *meter.Meter, s string, cut func(string) int) string {
	l := r.newMatchList()
	r.eachRuleMatch(wm, s, &windowIDs{}, func(start, end, rule int) { l.add(wm, start, end, rule) })
	r.releasedStringInto(wm, s, true, cut, l)
	return render(wm, s, settle(wm, s, l.spans(wm)))
}

// StringTwice is the package's StringTwice with the built-in rules.
func StringTwice(s string, cut func(string) int) string { return defaultRedactor.StringTwice(s, cut) }

// StringTwiceChecked is the package's StringTwiceChecked with the built-in rules.
func StringTwiceChecked(s string, cut func(string) int) (out string, ok bool) {
	return defaultRedactor.StringTwiceChecked(s, cut)
}

// ReplaceFrom is Replace for a log the log source cuts to the bytes from from on, where the release before
// redacted all of s, kept that part and redacted it again (releasedExcerpt): the matches are those of Replace
// and what that release hid, and repl and between are called as Replace calls them, over all of s. With drop not
// nil the matches also hold what that release hid downstream of the log source, on its own excerpt: the reader
// dropped the first drop(text) bytes of the excerpt and redacted the rest twice over (incidentcontext.RenderLog
// and get_pod_logs), so that what it hid there is decided on s as it was read, never on text this package's
// rules already replaced (#845, fifteenth decision).
//
// ok is false when the entry is withheld (budget.go): the text is then Withheld, with neither repl nor between
// called on s.
func (r *Redactor) ReplaceFrom(s string, from int, drop func(string) int, repl func(kind, text string) string,
	between func(string) string) (out string, ok bool) {
	spans, ok := metered(r, len(s), func(wm *meter.Meter) []span {
		l := r.newMatchList()
		r.eachRuleMatch(wm, s, &windowIDs{}, func(start, end, rule int) { l.add(wm, start, end, rule) })
		r.releasedExcerptInto(wm, s, from, drop, l)
		return settle(wm, s, l.spans(wm))
	})
	if !ok {
		return Withheld, false
	}
	return r.replaceSpans(unstopped(), s, spans, repl, between), true
}

// ReleasedText returns s as the release before wrote it where it redacted a text, made it valid UTF-8 and
// redacted it again (incidentcontext.Redact then), with that release's markers, or s when r has no such reading.
// It decides nothing: a caller orders lines the way that release ordered them (the series lines of a metrics
// block, sorted by their text as written), so that the reading of that release reads them in its order.
func (r *Redactor) ReleasedText(s string) string {
	if r.before == nil {
		return s
	}
	out, _ := meteredText(r, len(s), func(wm *meter.Meter) string {
		return r.before.StringWithin(wm, strings.ToValidUTF8(r.before.StringWithin(wm, s), "?"))
	})
	return out
}
