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
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// A tool result is one text, decided once (#845, third decision of 2026-11-18), and the text decided on is the
// text the tool returns (sixth decision of 2026-11-18): a tool gives its answer as pieces, the text it writes
// (a field name, JSON punctuation, a header line) and the values from a source, each written as the answer
// writes it (Form). The decision is taken once on those bytes ([Redactor.Pieces]) and the answer is the pieces
// as decided, joined: a pattern that exists only in the answer (a reason "Bearer" and the name of the object
// after it on an Event's line) is seen, and nothing is redacted after the decision.

// Form says how a tool writes a value from a source into its answer. Every form writes each character of the
// value as one unit of its own, so a match on the answer is widened to whole units and a match on the value is
// carried to the units it was written as.
type Form int

const (
	// AsIs writes the value as it is.
	AsIs Form = iota
	// Line writes the value on one line of the answer, as incidentcontext.Shown does: a control character,
	// U+2028 and U+2029 become a space, and an invalid byte becomes "?".
	Line
	// JSON writes the value as the content of a JSON string, as encoding/json does without HTML escapes: '"' and
	// '\\' escaped, a control character as "\n", "\r", "\t", "\b", "\f" or "\u00XX", U+2028 and U+2029 as
	// "\u2028" and "\u2029", and an invalid byte as "\ufffd".
	JSON
)

// Piece is one piece of a tool's answer: a value from a source (Data), or text the tool writes around the
// values (a field name and its colon, JSON punctuation, a line break), which is read with them and never
// changed.
type Piece struct {
	// Text is the text the tool writes, or the value as it was read.
	Text string
	Data bool
	// Form is how a data piece is written into the answer; a piece the tool writes is written as it is.
	Form Form
	// Whole hides a data piece whole: get_resource sets it on every value inside a collection under a field
	// whose name SecretCollection reports.
	Whole bool
	// Key is the key the object gives a data piece: a map key, a field name, a metric label's name (#845,
	// fifth decision of 2026-11-18). A key that SecretKey reports hides the piece whole, whatever it holds
	// (line breaks, blanks, markers): the object says where the value ends, the answer does not.
	Key string
	// MapKey marks a data piece that is a key of a map in the answer, not a value (#845, eighth decision of
	// 2026-11-18). main redacted no key alone, only its answer, where a key is written in its Form (a line break
	// in JSON is "\n"): what String hides of the key as written is hidden too, so a match that the value alone
	// ends at a line break runs on as far as main ran it.
	MapKey bool
}

// secretKeyName is a whole key name that holds a secret: the key part of main's rule for a quoted key.
var secretKeyName = regexp.MustCompile(`(?i)^(?:` + bareKeyExpr + `)$`)

// SecretKey reports whether a key the object gives (a map key, a field name, a metric label's name) is a
// secret key, decided from the key alone by the rule main applied to a quoted key (#845, fifth decision of
// 2026-11-18): the whole key, read in its folded view (an invisible character dropped, a look-alike folded) and
// without blanks around it, is a key name that holds a secret word or ends in a short key name ("auth",
// "pass", "DB_PASS", "storageSas"), and is no reference (isReference). Unlike the rule for prose, which reads a
// bare short key ("auth: failed") only before a value shaped like a credential, it needs no value. The work is
// linear in the length of key.
func SecretKey(key string) bool {
	if f, ok := foldView(key); ok {
		key = f.text
	}
	key = strings.TrimSpace(key)
	return secretKeyName.MatchString(key) && !isReference(key)
}

// secretCollectionName is a field name or map key that holds a secret word, written only in the characters
// of a key name, as main's flow rule read a key before "{" or "[".
var secretCollectionName = regexp.MustCompile(`(?i)^[A-Za-z0-9_.-]*` + secretWordExpr + `[A-Za-z0-9_.-]*$`)

// SecretCollection reports whether every value in a collection (an object, a list or a map) under a field or
// map key of this name is hidden whole: the name holds a secret word and is no reference (isReference)
// (#845, fourth decision of 2026-11-18). main read a tool result as JSON, where such a collection is a flow
// collection after a secret key, and hid it up to its closing bracket, so the name of a Secret a volume or
// imagePullSecrets refers to and a path in its items stayed hidden; the layout writes the collection as a
// block, where a reference child ("secretName", "path") keeps its value as in any text. Whether those names
// may be shown is #860. The work is linear in the length of name.
func SecretCollection(name string) bool {
	return secretCollectionName.MatchString(name) && !isReference(name)
}

// Pieces takes the one decision on the answer the pieces make, each data piece written in its Form and all
// joined as they are, and returns each piece as the answer writes it with what is redacted in it replaced; a
// piece the tool writes comes back as it was. A data piece loses, in the units it was written as:
//
//   - what String redacts of the answer, cut at the piece's ends and widened to whole units (an escape is
//     hidden whole or kept whole);
//   - what String redacts of the value alone, as it was read (with its line breaks: a block under a key in the
//     value is found by its lines), as main redacted each value before it wrote its answer;
//   - the value the rule for the strings of a sequence (seqValues) finds in it, read over the values in their
//     order: a key, a separator or a flag in one value and its value in a later one. The string after a secret
//     flag is its value, whatever it looks like ("--token", "--verbose" hides "--verbose");
//   - for a map key (MapKey), also what String hides of the key as the answer writes it, as main hid it;
//   - all of it when it is marked Whole or its Key is a secret key (SecretKey): the object says where the value
//     ends, and nothing beyond it is hidden: a match of the answer that reaches into such a value ends with it
//     (endAtObjectValues), so a block or a flow collection that the value opens takes no later entry (sixth
//     decision, point 2);
//   - all of it from its first byte that is no blank when a match of the answer takes it as the value of a key
//     outside it (valueOfKey: a header's value after its name in the field before).
//
// A key found inside a value hides as far as String hides it on the answer and on the value alone, and no
// further: on a line of the answer (Line) its value ends where the rule ends it on that line, in JSON a line
// break is "\n" and the next blank ends it, as main read each answer (sixth decision, point 3). The work is
// linear in the length of the pieces, apart from the sorts of the matches (mergeSpans, valueOfKey): m log m
// comparisons for m matches, and m is at most linear in that length.
func (r *Redactor) Pieces(ps []Piece) []string { return r.pieces(ps, false) }

// pieces is Pieces; with joined set, the first step also takes what String hides of the answer as written
// before any marker, cut at each value's ends, so that what String hides in the values joined is hidden in them
// apart whatever the markers change (Strings).
func (r *Redactor) pieces(ps []Piece, joined bool) []string {
	// First each value alone, as main redacted each value before it wrote its answer: what String hides of it as
	// it was read, the value seqValues finds in it, or all of it when the object says so.
	var data []string
	for _, p := range ps {
		tick(1)
		if p.Data {
			data = append(data, p.Text)
		}
	}
	starts := seqValues(data)
	ws := make([]written, len(ps))
	for i, p := range ps {
		tick(1)
		ws[i] = write(p)
	}
	var plain []span
	if joined {
		var b strings.Builder
		for _, w := range ws {
			tick(1)
			b.WriteString(w.text)
		}
		plain = r.spans(b.String())
	}
	var b strings.Builder
	k, off, next := 0, 0, 0
	for i, p := range ps {
		tick(1)
		w := ws[i]
		end := off + len(w.text)
		if p.Data {
			var own []span
			for next < len(plain) && plain[next].end <= off {
				tick(1)
				next++
			}
			for j := next; j < len(plain) && plain[j].start < end; j++ {
				tick(1)
				sp := plain[j]
				sp.start, sp.end = max(sp.start, off)-off, min(sp.end, end)-off
				if sp.end > sp.start {
					own = append(own, w.widen(sp))
				}
			}
			if hiddenByObject(p) {
				own = []span{{start: 0, end: len(w.text), kind: KindCredential, rule: len(r.rules)}}
			} else {
				for _, sp := range r.spans(p.Text) {
					tick(1)
					own = append(own, w.carry(sp))
				}
				if p.MapKey {
					for _, sp := range r.spans(w.text) {
						tick(1)
						own = append(own, w.widen(sp))
					}
				}
				if s := starts[k]; s >= 0 && s < len(p.Text) {
					own = append(own, w.carry(span{start: s, end: len(p.Text), kind: KindCredential, rule: len(r.rules)}))
				}
			}
			w = w.marked(mergeSpans(own))
			k++
		}
		ws[i] = w
		b.WriteString(w.text)
		off = end
	}
	// Then the answer, as written with those markers: what String hides of it, cut at each value's ends and
	// widened to whole units, and a value that a key outside it takes.
	text := b.String()
	matches := r.ruleMatches(text)
	firsts := firstBytes(ps, ws)
	whole := valueOfKey(firsts, slices.Clone(matches))
	settled := settle(text, matches)
	out := make([]string, len(ps))
	off, next = 0, 0
	for i, p := range ps {
		tick(1)
		w := ws[i]
		end := off + len(w.text)
		switch {
		case !p.Data || len(w.text) == 0:
			out[i] = w.text
		case whole[i]:
			// The value of a key outside the piece: all of it from its first byte that is no blank, the
			// blanks before that kept as the indentation they are.
			v := firsts[i] - off
			out[i] = w.text[:v] + render(w.text[v:], []span{{start: 0, end: len(w.text) - v, kind: KindCredential, rule: len(r.rules)}})
		default:
			var own []span
			for next < len(settled) && settled[next].end <= off {
				tick(1)
				next++
			}
			for j := next; j < len(settled) && settled[j].start < end; j++ {
				tick(1)
				sp := settled[j]
				sp.start, sp.end = max(sp.start, off)-off, min(sp.end, end)-off
				if sp.end > sp.start {
					own = append(own, w.widen(sp))
				}
			}
			out[i] = render(w.text, mergeSpans(own))
		}
		off = end
	}
	return out
}

// hiddenByObject reports whether the object says the data piece p is hidden whole: it is marked Whole or its
// key is a secret key.
func hiddenByObject(p Piece) bool { return p.Data && (p.Whole || (p.Key != "" && SecretKey(p.Key))) }

// written is a piece as the answer writes it, with the units it was written in: each character of a data piece
// (an invalid byte counts as one) is one unit of the value and one of the answer.
type written struct {
	text string
	// For a piece written in another form than AsIs: start and end give, for each byte of the value, the
	// offsets in text where the unit holding it starts and ends; unitStart and unitEnd give the same for each
	// byte of text. nil when text is the value.
	start, end, unitStart, unitEnd []int
}

// write writes the piece p as the answer writes it.
func write(p Piece) written {
	if !p.Data || p.Form == AsIs {
		return written{text: p.Text}
	}
	n := len(p.Text)
	w := written{start: make([]int, n), end: make([]int, n)}
	var b strings.Builder
	b.Grow(n)
	for i := 0; i < n; {
		tick(1)
		c, size := utf8.DecodeRuneInString(p.Text[i:])
		from := b.Len()
		writeUnit(&b, p.Form, p.Text[i:i+size], c, size)
		to := b.Len()
		for j := i; j < i+size; j++ {
			tick(1)
			w.start[j], w.end[j] = from, to
		}
		for range to - from {
			tick(1)
			w.unitStart, w.unitEnd = append(w.unitStart, from), append(w.unitEnd, to)
		}
		i += size
	}
	w.text = b.String()
	return w
}

// jsonHex are the digits of a "\u00XX" escape.
const jsonHex = "0123456789abcdef"

// writeUnit writes one unit of a value, the character c of size bytes (raw), in form f.
func writeUnit(b *strings.Builder, f Form, raw string, c rune, size int) {
	invalid := c == utf8.RuneError && size == 1
	switch f {
	case Line:
		switch {
		case invalid:
			b.WriteByte('?')
		case unicode.IsControl(c) || c == '\u2028' || c == '\u2029':
			b.WriteByte(' ')
		default:
			b.WriteString(raw)
		}
	case JSON:
		switch {
		case invalid:
			b.WriteString(`\ufffd`)
		case c == '"' || c == '\\':
			b.WriteByte('\\')
			b.WriteRune(c)
		case c == '\n':
			b.WriteString(`\n`)
		case c == '\r':
			b.WriteString(`\r`)
		case c == '\t':
			b.WriteString(`\t`)
		case c == '\b':
			b.WriteString(`\b`)
		case c == '\f':
			b.WriteString(`\f`)
		case c < 0x20:
			b.WriteString(`\u00`)
			b.WriteByte(jsonHex[c>>4])
			b.WriteByte(jsonHex[c&0xF])
		case c == '\u2028':
			b.WriteString(`\u2028`)
		case c == '\u2029':
			b.WriteString(`\u2029`)
		default:
			b.WriteString(raw)
		}
	default:
		b.WriteString(raw)
	}
}

// marked returns w with each of the sorted, disjoint spans of its text replaced by its marker, a marker one unit
// of the new text. The units of the value it was written from map to the units they were written as, a unit
// inside a marker to the marker.
func (w written) marked(spans []span) written {
	if len(spans) == 0 {
		return w
	}
	n := len(w.text)
	// unit returns the unit of w.text that holds byte j: the bytes of a value written as it is are units of their
	// own, so a span (on character boundaries) is never cut.
	unit := func(j int) (int, int) {
		if w.unitStart == nil {
			return j, j + 1
		}
		return w.unitStart[j], w.unitEnd[j]
	}
	var b strings.Builder
	m := written{unitStart: make([]int, 0, n), unitEnd: make([]int, 0, n)}
	at := make([]int, n+1) // the offset in the new text of each unit start of w.text; a byte in a marker maps to it
	ends := make([]int, n+1)
	pos := 0
	keep := func(to int) {
		for pos < to {
			tick(1)
			s, e := unit(pos)
			from := b.Len()
			b.WriteString(w.text[s:e])
			for j := s; j < e; j++ {
				tick(1)
				at[j], ends[j] = from, b.Len()
				m.unitStart, m.unitEnd = append(m.unitStart, from), append(m.unitEnd, b.Len())
			}
			pos = e
		}
	}
	for _, sp := range spans {
		tick(1)
		keep(sp.start)
		from := b.Len()
		b.WriteString(marker(sp.kind, sp.end-sp.start))
		for j := sp.start; j < sp.end; j++ {
			tick(1)
			at[j], ends[j] = from, b.Len()
		}
		for range b.Len() - from {
			tick(1)
			m.unitStart, m.unitEnd = append(m.unitStart, from), append(m.unitEnd, b.Len())
		}
		pos = sp.end
	}
	keep(n)
	m.text = b.String()
	if w.start != nil {
		m.start, m.end = make([]int, len(w.start)), make([]int, len(w.end))
		for j := range w.start {
			tick(1)
			m.start[j], m.end[j] = at[w.start[j]], ends[w.end[j]-1]
		}
	}
	return m
}

// widen widens a span of the written text to the whole units it touches.
func (w written) widen(sp span) span {
	if w.unitStart != nil {
		sp.start, sp.end = w.unitStart[sp.start], w.unitEnd[sp.end-1]
	}
	return sp
}

// carry carries a span of the value to the units of the written text it was written as.
func (w written) carry(sp span) span {
	if w.start != nil {
		sp.start, sp.end = w.start[sp.start], w.end[sp.end-1]
	}
	return sp
}

// firstBytes returns, for each piece, the offset in the answer of its first written byte that is no blank and
// no line break; -1 for a piece the tool writes or a piece of blanks only.
func firstBytes(ps []Piece, ws []written) []int {
	out := make([]int, len(ps))
	off := 0
	for i, p := range ps {
		tick(1)
		out[i] = -1
		t := ws[i].text
		for j := 0; p.Data && j < len(t); j++ {
			tick(1)
			if c := t[j]; c != ' ' && c != '\t' && c != '\r' && c != '\n' {
				out[i] = off + j
				break
			}
		}
		off += len(t)
	}
	return out
}

// valueOfKey reports, by piece, whether the answer takes a data piece as the value of a key outside it (a
// header's name in the field before it, a key and colon in the string before): a match covers the first byte of
// the piece that is no blank (firsts), and the match is a key's value (KindCredential) or began before that
// byte. In the answer only part of a value may belong to the key in front of it; the object says the value is
// all of the piece (#845, fifth decision of 2026-11-18). matches are the matches of the answer, sorted here: m
// log m comparisons for m matches. The rest of the work is linear in the number of pieces and matches.
func valueOfKey(firsts []int, matches []span) []bool {
	out := make([]bool, len(firsts))
	slices.SortFunc(matches, func(a, b span) int { return a.start - b.start })
	// One sweep over the pieces in order, x growing: before is the furthest end of a match that starts before x;
	// a credential match that starts at x is looked at in its own loop and taken into before at the next piece.
	m, before := 0, 0
	for i, x := range firsts {
		tick(1)
		if x < 0 {
			continue
		}
		for m < len(matches) && matches[m].start < x {
			tick(1)
			before = max(before, matches[m].end)
			m++
		}
		covered := before > x
		for j := m; !covered && j < len(matches) && matches[j].start == x; j++ {
			tick(1)
			covered = matches[j].kind == KindCredential && matches[j].end > x
		}
		out[i] = covered
	}
	return out
}

// Pieces is the package's Pieces with the built-in rules.
func Pieces(ps []Piece) []string { return defaultRedactor.Pieces(ps) }

// Written returns s as the answer writes it in form f, with nothing redacted: what a redactor that takes no
// decision on pieces writes after it redacted the value alone.
func Written(f Form, s string) string { return write(Piece{Text: s, Data: true, Form: f}).text }
