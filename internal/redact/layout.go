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
// piece the tool writes comes back as it was.
//
// What is hidden is the union of three readings of the same answer (#845, ninth decision of 2026-11-18). The
// readings only find spans; nothing is replaced until all three have run, and no reading takes another's
// replaced text as its only input, so a rule that hides a carrier (a connection string in a map key) never takes
// away the signal another rule reads after it (the word "authorization" that hides the value):
//
//  1. Each piece alone. A data piece loses what String redacts of the value as it was read (with its line
//     breaks: a block under a key in the value is found by its lines), as main redacted each value before it
//     wrote its answer; the value the rule for the strings of a sequence (seqValues) finds in it, read over the
//     values in their order (a key, a separator or a flag in one value and its value in a later one; the string
//     after a secret flag is its value, whatever it looks like: "--token", "--verbose" hides "--verbose"); for a
//     map key (MapKey), what String hides of the key as the answer writes it; and all of it when it is marked
//     Whole or its Key is a secret key (SecretKey).
//  2. The whole answer as the tool writes it: what String redacts of it, cut at each piece's ends and widened
//     to whole units (an escape is hidden whole or kept whole).
//  3. The whole answer with the markers of reading 1 in place, the markers main's first pass made (what String
//     redacts of each value alone as it was read; main redacted no map key alone): what String redacts of it,
//     carried back to the units the markers stand for, and all of a data piece from its first byte that is no
//     blank when a match takes it as the value of a key outside it (valueOfKey: a header's value after its name
//     in the field before).
//
// main hid what readings 1 and 3 find, so the union shows nothing main hid; reading 2 can only add. Where the
// object says a value ends (Whole, a secret Key), a match of readings 2 and 3 that reaches into it ends with it
// (endAtObjectValues), so a block or a flow collection that the value opens takes no later entry (sixth
// decision, point 2). A key found inside a value hides as far as String hides it on the answer and on the value
// alone, and no further: on a line of the answer (Line) its value ends where the rule ends it on that line, in
// JSON a line break is "\n" and the next blank ends it, as main read each answer (sixth decision, point 3). The
// work is linear in the length of the pieces (three passes over the answer), apart from the sorts of the matches
// (mergeSpans, valueOfKey): m log m comparisons for m matches, and m is at most linear in that length.
func (r *Redactor) Pieces(ps []Piece) []string {
	var data []string
	for _, p := range ps {
		tick(1)
		if p.Data {
			data = append(data, p.Text)
		}
	}
	starts := seqValues(data)
	ws := make([]written, len(ps))
	var raw strings.Builder
	for i, p := range ps {
		tick(1)
		ws[i] = write(p)
		raw.WriteString(ws[i].text)
	}
	// own holds, by piece, the spans found in its written text; reading 1 first.
	own := make([][]span, len(ps))
	marks := make([]marked, len(ps))
	k := 0
	for i, p := range ps {
		tick(1)
		w := ws[i]
		marks[i] = marked{text: w.text}
		if !p.Data {
			continue
		}
		var alone []span
		if hiddenByObject(p) {
			own[i] = []span{{start: 0, end: len(w.text), kind: KindCredential, rule: len(r.rules)}}
		} else {
			for _, sp := range r.spans(p.Text) {
				tick(1)
				alone = append(alone, w.carry(sp))
			}
			own[i] = append(own[i], alone...)
			if p.MapKey {
				// main redacted no key alone: the key keeps no marker for reading 3.
				for _, sp := range r.spans(w.text) {
					tick(1)
					own[i] = append(own[i], w.widen(sp))
				}
				alone = nil
			}
			if s := starts[k]; s >= 0 && s < len(p.Text) {
				own[i] = append(own[i], w.carry(span{start: s, end: len(p.Text), kind: KindCredential, rule: len(r.rules)}))
			}
		}
		marks[i] = w.mark(mergeSpans(alone))
		k++
	}
	// Reading 2: the answer as the tool writes it.
	wsize := func(i int) int { return len(ws[i].text) }
	r.cutAnswer(ps, own, endAtObjectValues(ps, wsize, r.spans(raw.String())), func(i int, sp span) span { return ws[i].widen(sp) }, wsize)
	// Reading 3: the answer with the markers of reading 1.
	var b strings.Builder
	for _, m := range marks {
		tick(1)
		b.WriteString(m.text)
	}
	text := b.String()
	msize := func(i int) int { return len(marks[i].text) }
	matches := endAtObjectValues(ps, msize, r.ruleMatches(text))
	firsts := firstBytes(ps, marks)
	whole := valueOfKey(firsts, slices.Clone(matches))
	r.cutAnswer(ps, own, settle(text, matches), func(i int, sp span) span {
		if marks[i].from == nil {
			return ws[i].widen(sp)
		}
		return marks[i].back(sp)
	}, msize)
	out := make([]string, len(ps))
	off := 0
	for i, p := range ps {
		tick(1)
		w := ws[i]
		if p.Data && whole[i] && len(w.text) > 0 {
			// The value of a key outside the piece: all of it from its first byte that is no blank, the blanks
			// before that kept as the indentation they are.
			v := marks[i].back(span{start: firsts[i] - off, end: firsts[i] - off + 1}).start
			own[i] = append(own[i], span{start: v, end: len(w.text), kind: KindCredential, rule: len(r.rules)})
		}
		off += len(marks[i].text)
		out[i] = w.text
		if p.Data {
			out[i] = render(w.text, mergeSpans(own[i]))
		}
	}
	return out
}

// cutAnswer adds to own, by data piece, the spans of a reading of the whole answer (sorted, disjoint, on the
// text of that reading), cut at each piece's ends (of length size(i) in that text) and carried to the piece's
// written text by to. A piece the object hides whole takes nothing more: it is hidden whole already. The work is
// linear in the number of pieces and spans.
func (r *Redactor) cutAnswer(ps []Piece, own [][]span, spans []span, to func(i int, sp span) span, size func(i int) int) {
	off, next := 0, 0
	for i, p := range ps {
		tick(1)
		end := off + size(i)
		if p.Data && !hiddenByObject(p) {
			for next < len(spans) && spans[next].end <= off {
				tick(1)
				next++
			}
			for j := next; j < len(spans) && spans[j].start < end; j++ {
				tick(1)
				sp := spans[j]
				sp.start, sp.end = max(sp.start, off)-off, min(sp.end, end)-off
				if sp.end > sp.start {
					own[i] = append(own[i], to(i, sp))
				}
			}
		}
		off = end
	}
}

// endAtObjectValues cuts each span of a reading of the whole answer (pieces of length size(i) in that reading's
// text) that reaches into a data piece the object hides whole (hiddenByObject) at that piece's end: the object
// says where the value ends, so a block or a flow collection that the value opens, or a key inside it, takes no
// later piece (#845, sixth decision of 2026-11-18, point 2). The spans are changed in place and returned. The work
// and the allocation are linear in the length of the text and the number of spans.
func endAtObjectValues(ps []Piece, size func(i int) int, spans []span) []span {
	var starts, ends []int // the hidden pieces, in order
	off := 0
	for i, p := range ps {
		tick(1)
		if n := size(i); hiddenByObject(p) && n > 0 {
			starts, ends = append(starts, off), append(ends, off+n)
		}
		off += size(i)
	}
	if len(starts) == 0 {
		return spans
	}
	// first[x] is the first hidden piece that ends after byte x, len(starts) for none.
	first := make([]int, off+1)
	j := len(starts)
	for x := off; x >= 0; x-- {
		tick(1)
		for j > 0 && ends[j-1] > x {
			tick(1)
			j--
		}
		first[x] = j
	}
	for i := range spans {
		tick(1)
		if h := first[min(spans[i].start, off)]; h < len(starts) && starts[h] < spans[i].end {
			spans[i].end = min(spans[i].end, ends[h])
		}
	}
	return spans
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

// marked is the written text of a piece with the markers of reading 1 in place, as reading 3 reads it: from and
// to give, for each byte of text, the unit of the written text it stands for (a byte of a marker stands for the
// whole span the marker replaced). nil from and to: text is the written text.
type marked struct {
	text     string
	from, to []int
}

// mark returns w with each of the sorted, disjoint spans of its text replaced by its marker. The work and the
// allocation are linear in the length of w.
func (w written) mark(spans []span) marked {
	if len(spans) == 0 {
		return marked{text: w.text}
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
	m := marked{from: make([]int, 0, n), to: make([]int, 0, n)}
	pos := 0
	keep := func(to int) {
		for pos < to {
			tick(1)
			s, e := unit(pos)
			b.WriteString(w.text[s:e])
			for range e - s {
				tick(1)
				m.from, m.to = append(m.from, s), append(m.to, e)
			}
			pos = e
		}
	}
	for _, sp := range spans {
		tick(1)
		keep(sp.start)
		mk := marker(sp.kind, sp.end-sp.start)
		b.WriteString(mk)
		for range len(mk) {
			tick(1)
			m.from, m.to = append(m.from, sp.start), append(m.to, sp.end)
		}
		pos = sp.end
	}
	keep(n)
	m.text = b.String()
	return m
}

// back carries a span of the marked text to the units of the written text it stands for.
func (m marked) back(sp span) span {
	if m.from != nil {
		sp.start, sp.end = m.from[sp.start], m.to[sp.end-1]
	}
	return sp
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
func firstBytes(ps []Piece, ws []marked) []int {
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
