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
)

// A tool result is one text, decided once (#845, third decision of 2026-11-18). A read tool lays out what it
// shows as one text: the fields in document order, each as a "name: value" line, list items as items, nothing
// escaped or encoded. The decision is taken once on that text ([Redactor.Pieces]) and applied back to the
// values, so a key in one field and its value in a sibling or the next field (an HTTP header's name and value,
// a flag at the end of a command and its value in the arguments) are seen together, as they were by main's
// second pass over the whole answer, and nothing is redacted after the result is encoded.

// Piece is one piece of a text laid out from the fields of a tool result: a value from a source (Data), or the
// layout code writes around the values (indentation, a field name and its colon, a list dash, a line break),
// which is read with them and never changed.
type Piece struct {
	Text string
	Data bool
	// Whole hides a data piece whole: the layout sets it on every value inside a collection under a field
	// whose name SecretCollection reports.
	Whole bool
	// Key is the key the object gives a data piece: a map key, a field name, a metric label's name (#845,
	// fifth decision of 2026-11-18). A key that SecretKey reports hides the piece whole, whatever it holds
	// (line breaks, blanks, markers): the object says where the value ends, the laid-out text does not.
	Key string
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

// Pieces takes the one decision on the text the pieces make, joined as they are, and returns each piece with
// what is redacted in it replaced; a layout piece comes back as it was. A data piece loses what String redacts
// of it in the joined text (cut at its ends), what String redacts of it alone, and the value the rule for the
// strings of a sequence (seqValues) finds in it, read over the data pieces in their order: a key, a separator
// or a flag in one value and its value in a later one. The string after a secret flag is its value, whatever
// it looks like ("--token", "--verbose" hides "--verbose").
//
// A data piece is one value, as the object has it (#845, fifth decision of 2026-11-18). When a data piece holds
// a line break, the joined text and each such piece alone are also read with the line breaks inside data
// pieces written as the JSON escapes "\n" and "\r" (escapedReading), the text main read in its pass over the
// encoded answer, and what that reading hides is hidden too, so a value that begins on one line of a piece runs
// on over its line breaks wherever main's reading ran on. A data piece is hidden whole when it is marked Whole
// or its Key is a secret key (SecretKey), and from its first byte that is no blank to its end when the escaped
// reading takes it as the value of a key outside it (valueOfKey). The work is linear in the length of the
// pieces.
func (r *Redactor) Pieces(ps []Piece) []string {
	var b strings.Builder
	var data []string
	breaks := false
	for _, p := range ps {
		tick(1)
		b.WriteString(p.Text)
		if p.Data {
			data = append(data, p.Text)
			breaks = breaks || strings.ContainsAny(p.Text, "\r\n")
		}
	}
	text := b.String()
	matches := r.ruleMatches(text)
	esc := escapedReading(ps, text, breaks)
	escMatches := matches
	if breaks {
		escMatches = esc.back(r.ruleMatches(esc.text), 0)
		matches = append(matches, escMatches...)
	}
	whole := valueOfKey(esc.firsts, slices.Clone(escMatches))
	joined := settle(text, matches)
	starts := seqValues(data)
	out := make([]string, len(ps))
	off, next, k := 0, 0, 0
	for i, p := range ps {
		tick(1)
		end := off + len(p.Text)
		if !p.Data {
			out[i] = p.Text
			off = end
			continue
		}
		if p.Whole || (p.Key != "" && SecretKey(p.Key)) {
			out[i] = render(p.Text, []span{{start: 0, end: len(p.Text), kind: KindCredential, rule: len(r.rules)}})
			k++
			off = end
			continue
		}
		if whole[i] {
			// The value of a key outside the piece: all of it from its first byte that is no blank, the
			// blanks and line breaks before that kept as the indentation they are.
			v := esc.firsts[i] - off
			out[i] = p.Text[:v] + render(p.Text[v:], []span{{start: 0, end: len(p.Text) - v, kind: KindCredential, rule: len(r.rules)}})
			k++
			off = end
			continue
		}
		var own []span
		for next < len(joined) && joined[next].end <= off {
			tick(1)
			next++
		}
		for j := next; j < len(joined) && joined[j].start < end; j++ {
			tick(1)
			sp := joined[j]
			sp.start, sp.end = max(sp.start, off)-off, min(sp.end, end)-off
			if sp.end > sp.start {
				own = append(own, sp)
			}
		}
		own = append(own, r.spans(p.Text)...)
		if from, to := esc.escapedAt(off), esc.escapedAt(end); to-from != len(p.Text) {
			// The piece alone, its line breaks read escaped.
			for _, sp := range esc.back(r.ruleMatches(esc.text[from:to]), from) {
				tick(1)
				sp.start, sp.end = sp.start-off, sp.end-off
				own = append(own, sp)
			}
		}
		if s := starts[k]; s >= 0 && s < len(p.Text) {
			own = append(own, span{start: s, end: len(p.Text), kind: KindCredential, rule: len(r.rules)})
		}
		out[i] = render(p.Text, mergeSpans(own))
		k++
		off = end
	}
	return out
}

// escaped is the text pieces make with each line break inside a data piece written as its JSON escape, with
// the maps between its offsets and those of the text.
type escaped struct {
	text string
	// from and to give, for each byte of text, the bytes of the original text it was written from: an escape's
	// two bytes both come from its line break.
	from, to []int
	// at gives, for each offset of the original text and its end, the offset in text it was written at; nil when
	// text is the original text.
	at []int
	// firsts gives, for each piece, the offset in the original text of its first byte that is no blank and no
	// line break; -1 for a layout piece or a piece of blanks only.
	firsts []int
}

// escapedReading returns the text the pieces make (joined) with each CR and LF inside a data piece written as
// "\r" and "\n", as JSON escapes them, and the first byte of each piece that is no blank. Layout pieces keep
// their line breaks, so the lines of the layout stay lines. With breaks false (no data piece holds a line
// break) the text is joined itself and only firsts is set.
func escapedReading(ps []Piece, joined string, breaks bool) escaped {
	e := escaped{text: joined, firsts: make([]int, len(ps))}
	var b strings.Builder
	if breaks {
		e.at = make([]int, len(joined)+1)
		e.from, e.to = make([]int, 0, len(joined)), make([]int, 0, len(joined))
	}
	off := 0
	for i, p := range ps {
		tick(1)
		e.firsts[i] = -1
		for j := range len(p.Text) {
			tick(1)
			c := p.Text[j]
			if p.Data && e.firsts[i] < 0 && c != ' ' && c != '\t' && c != '\r' && c != '\n' {
				e.firsts[i] = off + j
			}
			if !breaks {
				continue
			}
			e.at[off+j] = b.Len()
			if p.Data && (c == '\r' || c == '\n') {
				esc := byte('n')
				if c == '\r' {
					esc = 'r'
				}
				b.WriteByte('\\')
				b.WriteByte(esc)
				e.from, e.to = append(e.from, off+j, off+j), append(e.to, off+j+1, off+j+1)
				continue
			}
			b.WriteByte(c)
			e.from, e.to = append(e.from, off+j), append(e.to, off+j+1)
		}
		off += len(p.Text)
	}
	if breaks {
		e.at[len(joined)] = b.Len()
		e.text = b.String()
	}
	return e
}

// escapedAt returns the offset in e.text that the offset pos of the original text was written at.
func (e escaped) escapedAt(pos int) int {
	if e.at == nil {
		return pos
	}
	return e.at[pos]
}

// back maps matches found in e.text from offset base on to the bytes of the original text.
func (e escaped) back(ms []span, base int) []span {
	for i := range ms {
		tick(1)
		start, end := ms[i].start, ms[i].end
		ms[i].start, ms[i].end = e.from[base+start], e.to[base+max(end-1, start)]
	}
	return ms
}

// valueOfKey reports, by piece, whether the escaped reading (escapedReading) takes a data piece as the value of a key
// outside it (a header's name in the field before it, a key and colon in the string before): a match covers the
// first byte of the piece that is no blank (firsts), and the match is a key's value (KindCredential) or began
// before that byte. In the laid-out text only the first line of a value belongs to the key in front of it; the
// object says the value is all of the piece (#845, fifth decision of 2026-11-18). matches are the matches of
// that reading, sorted here. The work is linear in the number of pieces and matches, apart from that sort.
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
