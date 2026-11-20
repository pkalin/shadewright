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

// trace is a text as the release before held it at one of its passes, traced back to the text decided on: byte
// i stands for the bytes [lo[i], hi[i]) of that text. A marker stands for all the bytes it replaced. lo and hi
// never decrease along the text.
type trace struct {
	text   string
	lo, hi []int32
}

// traceOf returns s, which starts at byte at of the text decided on, as a trace of itself.
func traceOf(s string, at int) trace {
	t := trace{text: s, lo: make([]int32, len(s)), hi: make([]int32, len(s))}
	for i := range len(s) {
		tick(1)
		t.lo[i], t.hi[i] = int32(at+i), int32(at+i+1) // #nosec G115 -- a text is far below 2 GiB
	}
	return t
}

// traceBuilder writes a trace.
type traceBuilder struct {
	b      strings.Builder
	lo, hi []int32
}

// add writes s, every byte of which stands for [lo, hi).
func (tb *traceBuilder) add(s string, lo, hi int32) {
	tb.b.WriteString(s)
	for range len(s) {
		tick(1)
		tb.lo, tb.hi = append(tb.lo, lo), append(tb.hi, hi)
	}
}

// copy writes the bytes [from, to) of t with what they stand for.
func (tb *traceBuilder) copy(t trace, from, to int) {
	tick(to - from)
	tb.b.WriteString(t.text[from:to])
	tb.lo, tb.hi = append(tb.lo, t.lo[from:to]...), append(tb.hi, t.hi[from:to]...)
}

func (tb *traceBuilder) trace() trace { return trace{text: tb.b.String(), lo: tb.lo, hi: tb.hi} }

// releasedReading applies the redactor of the release before and collects what its passes hide, as spans of
// the text decided on. The callers widen those spans to whole characters (onRunes) before they return them: that
// release cut a value at a byte bound (maxValueExtent) and could end a span inside a character, which left the
// rest of the character after the marker (#845, P4).
type releasedReading struct {
	r *released.Redactor
	// rule is the rule index the spans take: after every rule of this package's redactor, so that on a tie a
	// span of this package's rules names the merged range, as it did before the reading was added.
	rule   int
	hidden []span
	// read holds what each text a pass read was found to hold: a value repeated in the pieces of an answer (a
	// label in the series of every metric template) is read by that release's rules once (#873, second round).
	// The spans depend on the text only.
	read map[string][]released.Span
}

// releasedReading returns a reading with the redactor of the release before that has r's extra patterns, or
// nil when r has none (a redactor made of some of the rules only, in a test).
func (r *Redactor) releasedReading() *releasedReading {
	if r.before == nil {
		return nil
	}
	return &releasedReading{r: r.before, rule: len(r.rules)}
}

// pass is one call of String of the release before on t: the spans it finds are hidden, and the text it returns
// has its markers in place.
func (x *releasedReading) pass(t trace) trace {
	found, ok := x.read[t.text]
	if !ok {
		found = x.r.Spans(t.text)
		if x.read == nil {
			x.read = make(map[string][]released.Span)
		}
		x.read[t.text] = found
	} else {
		tick(len(t.text)) // the lookup hashes the text
	}
	if len(found) == 0 {
		return t
	}
	var tb traceBuilder
	pos := 0
	for _, f := range found {
		tick(1)
		tb.copy(t, pos, f.Start)
		lo, hi := t.lo[f.Start], t.hi[f.End-1]
		if hi > lo {
			x.hidden = append(x.hidden, span{start: int(lo), end: int(hi), kind: f.Kind, rule: x.rule})
		}
		tb.add(released.Marker(f.Kind, f.End-f.Start), lo, hi)
		pos = f.End
	}
	tb.copy(t, pos, len(t.text))
	return tb.trace()
}

// redact is incidentcontext.Redact of the release before on t: String, the result made valid UTF-8 (each
// invalid run becomes one "?"), and String again.
func (x *releasedReading) redact(t trace) trace { return x.pass(validTrace(x.pass(t))) }

// validTrace is strings.ToValidUTF8(t, "?") on a trace: each run of invalid bytes becomes one "?" that stands
// for the whole run.
func validTrace(t trace) trace {
	tick(len(t.text))
	if utf8.ValidString(t.text) {
		return t
	}
	var tb traceBuilder
	pos := 0
	for i := 0; i < len(t.text); {
		tick(1)
		c, size := utf8.DecodeRuneInString(t.text[i:])
		if c != utf8.RuneError || size != 1 {
			i += size
			continue
		}
		tb.copy(t, pos, i)
		j := i + 1
		for j < len(t.text) {
			tick(1)
			if c, size := utf8.DecodeRuneInString(t.text[j:]); c != utf8.RuneError || size != 1 {
				break
			}
			j++
		}
		tb.add("?", t.lo[i], t.hi[j-1])
		i, pos = j, j
	}
	tb.copy(t, pos, len(t.text))
	return tb.trace()
}

// maxShownBytes is incidentcontext's bound of a value on a line (maxValueBytes): a longer value, once redacted,
// is left out whole (TestReleasedShownMatchesValue checks the two agree).
const maxShownBytes = 1024

// shownTrace is the rest of incidentcontext.Value of the release before on a value it redacted: a value longer
// than maxShownBytes is left out whole and replaced by a note of its size, which stands for all of it; a
// control character, U+2028 and U+2029 become a space. t is valid UTF-8.
func shownTrace(t trace) trace {
	if len(t.text) > maxShownBytes {
		var tb traceBuilder
		tb.add("("+strconv.Itoa(len(t.text))+" bytes left out)", t.lo[0], t.hi[len(t.text)-1])
		return tb.trace()
	}
	var tb traceBuilder
	pos := 0
	for i := 0; i < len(t.text); {
		tick(1)
		c, size := utf8.DecodeRuneInString(t.text[i:])
		if unicode.IsControl(c) || c == ' ' || c == ' ' {
			tb.copy(t, pos, i)
			tb.add(" ", t.lo[i], t.hi[i+size-1])
			pos = i + size
		}
		i += size
	}
	tb.copy(t, pos, len(t.text))
	return tb.trace()
}

// jsonTrace writes t as the content of a JSON string, as encoding/json does without HTML escapes (writeUnit),
// each character's escape standing for the character.
func jsonTrace(t trace) trace {
	var tb traceBuilder
	var unit strings.Builder
	for i := 0; i < len(t.text); {
		tick(1)
		c, size := utf8.DecodeRuneInString(t.text[i:])
		unit.Reset()
		writeUnit(&unit, JSON, t.text[i:i+size], c, size)
		tb.add(unit.String(), t.lo[i], t.hi[i+size-1])
		i += size
	}
	return tb.trace()
}

// releasedPieces returns what the release before hid in the answer the pieces make, as spans of the pieces'
// texts joined as they were read (a piece the tool writes included): its read tools redacted each value as it
// was read (incidentcontext.Redact; no map key alone, which encoding/json wrote as it was), wrote it into the
// answer (a value on a line as incidentcontext.Value showed it, a value in JSON as encoding/json wrote it), and
// redacted the whole answer the same way before they cut it. A map key also loses what that release hid in the
// same text as a value. nil when r has no such reading.
func (r *Redactor) releasedPieces(ps []Piece) []span {
	x := r.releasedReading()
	if x == nil {
		return nil
	}
	var tb traceBuilder
	var read strings.Builder
	off := 0
	for _, p := range ps {
		tick(1)
		read.WriteString(p.Text)
		t := traceOf(p.Text, off)
		if p.Data {
			if !p.MapKey {
				t = x.redact(t)
			} else {
				// That release wrote a map key as it was, but the same text as a value it redacted alone: what it
				// hides there is hidden in the key too, which keeps its text for the passes over the answer
				// (#845, fifteenth decision: a label key and a condition reason of one object, one text).
				x.redact(t)
			}
			switch p.Form {
			case Line:
				t = shownTrace(t)
			case JSON:
				t = jsonTrace(t)
			}
		}
		tb.copy(t, 0, len(t.text))
		off += len(p.Text)
	}
	x.redact(tb.trace())
	return onRunes(read.String(), x.hidden)
}

// releasedString returns what the release before hid in s, as spans of s: one call of its String when twice
// is false (String, StringKeeping, Replace and Clean, at the places it called String once); when twice is true,
// what it hid where it redacted s, made it valid UTF-8 and redacted it again (incidentcontext.Redact), and, when
// cut is not nil, then kept the part of that text from cut(text) on and redacted that part the same way (a log
// excerpt at get_pod_logs, which RenderLog cut to its newest lines and FitLog redacted again). nil when r has no
// such reading.
func (r *Redactor) releasedString(s string, twice bool, cut func(string) int) []span {
	x := r.releasedReading()
	if x == nil {
		return nil
	}
	if !twice {
		x.pass(traceOf(s, 0))
		return onRunes(s, x.hidden)
	}
	t := x.redact(traceOf(s, 0))
	if cut != nil {
		from := min(max(cut(t.text), 0), len(t.text))
		x.redact(trace{text: t.text[from:], lo: t.lo[from:], hi: t.hi[from:]})
	}
	return onRunes(s, x.hidden)
}

// releasedExcerpt returns what the release before hid in a log as the log source read it (s) when it kept the
// bytes from from on: it redacted all of s (Replace), wrote what the cut kept (a match the cut runs through as
// the marker of its part kept), made that valid UTF-8 and redacted it again (String). With drop not nil, also
// what it hid where the reader of that excerpt dropped its first drop(text) bytes and redacted the rest as
// incidentcontext.Redact did, twice (RenderLog, then get_pod_logs fitting the text again), each time on the text
// with that release's markers in place (#845, fifteenth decision). nil when r has no such reading.
func (r *Redactor) releasedExcerpt(s string, from int, drop func(string) int) []span {
	x := r.releasedReading()
	if x == nil {
		return nil
	}
	from = min(max(from, 0), len(s))
	t := traceOf(s, 0)
	var tb traceBuilder
	pos := from
	for _, f := range x.r.Spans(s) {
		tick(1)
		if f.End > f.Start {
			x.hidden = append(x.hidden, span{start: f.Start, end: f.End, kind: f.Kind, rule: x.rule})
		}
		if f.End <= from {
			continue
		}
		start := max(f.Start, from)
		tb.copy(t, pos, start)
		tb.add(released.Marker(f.Kind, f.End-start), int32(start), int32(f.End)) // #nosec G115 -- a text is far below 2 GiB
		pos = f.End
	}
	tb.copy(t, pos, len(s))
	ex := x.pass(validTrace(tb.trace()))
	if drop != nil {
		d := min(max(drop(ex.text), 0), len(ex.text))
		x.redact(x.redact(trace{text: ex.text[d:], lo: ex.lo[d:], hi: ex.hi[d:]}))
	}
	return onRunes(s, x.hidden)
}

// StringTwice is String for a text the release before redacted, made valid UTF-8 and redacted again
// (incidentcontext.Redact, where incidentcontext.Value and the read tools called it): it hides what the rules of
// this package find in s and what that release hid there. With cut not nil it also hides what that release hid
// when it then kept the part of its text from cut(text) on (cut is given the text as that release held it) and
// redacted that part the same way: RenderLog for a log excerpt that get_pod_logs fitted again (#845, thirteenth
// decision of 2026-11-18).
func (r *Redactor) StringTwice(s string, cut func(string) int) string {
	return render(s, settle(s, append(r.ruleMatches(s), r.releasedString(s, true, cut)...)))
}

// StringTwice is the package's StringTwice with the built-in rules.
func StringTwice(s string, cut func(string) int) string { return defaultRedactor.StringTwice(s, cut) }

// ReplaceFrom is Replace for a log the log source cuts to the bytes from from on, where the release before
// redacted all of s, kept that part and redacted it again (releasedExcerpt): the matches are those of Replace
// and what that release hid, and repl and between are called as Replace calls them, over all of s. With drop not
// nil the matches also hold what that release hid downstream of the log source, on its own excerpt: the reader
// dropped the first drop(text) bytes of the excerpt and redacted the rest twice over (incidentcontext.RenderLog
// and get_pod_logs), so that what it hid there is decided on s as it was read, never on text this package's
// rules already replaced (#845, fifteenth decision).
func (r *Redactor) ReplaceFrom(s string, from int, drop func(string) int, repl func(kind, text string) string,
	between func(string) string) string {
	return r.replaceSpans(s, settle(s, append(r.ruleMatches(s), r.releasedExcerpt(s, from, drop)...)), repl, between)
}

// ReleasedText returns s as the release before wrote it where it redacted a text, made it valid UTF-8 and
// redacted it again (incidentcontext.Redact then), with that release's markers, or s when r has no such reading.
// It decides nothing: a caller orders lines the way that release ordered them (the series lines of a metrics
// block, sorted by their text as written), so that the reading of that release reads them in its order.
func (r *Redactor) ReleasedText(s string) string {
	if r.before == nil {
		return s
	}
	return r.before.String(strings.ToValidUTF8(r.before.String(s), "?"))
}
