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

	"github.com/pkalin/shadewright/internal/redact/meter"
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
	// "\u2028" and "\u2029", and a run of invalid bytes as one "?", as the release before made each value valid
	// UTF-8 before it was encoded.
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
	// 2026-11-18). It is read alone like a value; the reading of the release before (releasedPieces) does not
	// redact it alone, as that release wrote a key as it was and redacted it only with its answer.
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
func SecretKey(key string) bool { return secretKey(nil, key) }

// secretKey is SecretKey on the budget wm.
func secretKey(wm *meter.Meter, key string) bool {
	if f, ok := foldView(wm, key); ok {
		key = f.text
	}
	key = strings.TrimSpace(key)
	return secretKeyName.MatchString(key) && !isReference(wm, key)
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
	return secretCollectionName.MatchString(name) && !isReference(nil, name)
}

// Pieces takes the one decision on the answer the pieces make, each data piece written in its Form and all
// joined as they are, and returns each piece as the answer writes it with what is redacted in it replaced; a
// piece the tool writes comes back as it was.
//
// What is hidden is the union of three readings of the same answer (#845, ninth and thirteenth decisions of
// 2026-11-18). The readings only find spans; nothing is replaced until all three have run, and no reading takes
// another's replaced text as its input, so a rule that hides a carrier (a connection string in a map key) never
// takes away the signal another rule reads after it (the word "authorization" that hides the value):
//
//  1. Each piece alone. A data piece loses what the rules of this package find in the value as it was read (with
//     its line breaks: a block under a key in the value is found by its lines); the value the rule for the
//     strings of a sequence (seqValues) finds in it, read over the values in their order (a key, a separator or a
//     flag in one value and its value in a later one; the string after a secret flag is its value, whatever it
//     looks like: "--token", "--verbose" hides "--verbose"); and all of it when it is marked Whole or its Key is
//     a secret key (SecretKey). A map key (MapKey) is a piece like any other.
//  2. The whole answer as the tool writes it: what the rules of this package find in it, each match ended at a
//     line break inside a value as the reading of that value alone ends it (endAtValueLines, tenth decision of
//     2026-11-18), cut at each piece's ends and widened to whole units (an escape is hidden whole or kept whole).
//     A secret on one line of a value does not take the lines after it, and a match still reaches across what
//     the encoding puts side by side (a key and its value, the items of a list, a flag and its argument).
//  3. The reading of the release before (releasedPieces, before.go): its redactor, kept unchanged in package
//     released, applied as its read tools applied it (each value redacted, made valid UTF-8 and redacted again,
//     then written into the answer as they wrote it, and the whole answer redacted the same way), each of its
//     spans carried back to the bytes of the pieces it stands for; a span that begins before a piece takes it from
//     its first byte that is no blank, as reading 2 does, so the indentation of a value stays.
//
// So everything the release before hid is hidden because its own code says so, and readings 1 and 2 can only
// add. Where the object says a value ends (Whole, a secret Key), a match of reading 2 that reaches into it ends
// with it (endAtObjectValues), so a block or a flow collection that the value opens takes no later entry (sixth
// decision, point 2). The work is linear in the length of the pieces (one pass of this package's rules over each
// piece and one over the answer, and four passes of the release before's over the answer, as its tools made
// them), apart from the sorts of the matches (mergeSpans): m log m comparisons for m matches, and m is at most
// linear in that length.
//
// An answer it withholds (budget.go) comes back as one string per piece, the first Withheld and the others empty;
// [Redactor.PiecesWithin] says so with a flag.
func (r *Redactor) Pieces(ps []Piece) []string {
	out, _ := r.PiecesWithin(ps, -1)
	return out
}

// PiecesWithin is Pieces for an answer bounded to limit bytes (none when limit is negative): the decision is
// the same, and when the pieces joined with the usual markers are longer than limit, each replacement takes the
// first shorter form of its marker (marker) that makes them fit, down to "***", so an answer the caller would
// otherwise cut at its limit stays whole whenever it can (#845, thirteenth decision of 2026-11-18): the
// replacements of this package may be longer than those of the release before, which wrote its markers on the
// values before they were escaped. The work is linear in the length of the pieces.
//
// ok is false when it withholds the answer (budget.go): it then comes back as one string per piece, the first
// Withheld and the others empty. A caller tells a withheld answer by ok, never by its text.
func (r *Redactor) PiecesWithin(ps []Piece, limit int) (out []string, ok bool) {
	return r.meteredPieces(ps, func(wm *meter.Meter) []string { return r.piecesWithin(wm, ps, limit, false, nil) })
}

// PiecesFitting is Pieces with each replacement in the first form of its marker (marker, down to "***") at
// which fits holds of the pieces as decided, or the shortest when it holds at none: a caller whose answer is
// longer than the pieces joined (get_resource, which renames two map keys decided alike) checks its own answer
// (#845, fifteenth decision, P4). The decision is the same as Pieces'; fits is called at most once per form,
// so the work is linear in the length of the pieces and in what fits does.
// An answer it withholds comes back as PiecesWithin returns it, ok false, with fits not called on it.
func (r *Redactor) PiecesFitting(ps []Piece, fits func(out []string) bool) (out []string, ok bool) {
	return r.meteredPieces(ps, func(wm *meter.Meter) []string { return r.piecesWithin(wm, ps, -1, false, fits) })
}

// meteredPieces runs f, a decision on ps, on the budget of the pieces' bytes, their keys included (budget.go).
// ok is false when the answer is withheld.
func (r *Redactor) meteredPieces(ps []Piece, f func(wm *meter.Meter) []string) (out []string, ok bool) {
	size := 0
	for _, p := range ps {
		size += len(p.Text) + len(p.Key)
	}
	if out, ok := metered(size, f); ok {
		return out, true
	}
	return withheldStrings(len(ps)), false
}

// piecesWithin is PiecesWithin; with joined true it also hides, cut at each piece's ends, everything String hides
// in the answer as written, with no match ended at a line break of a value (Strings, whose answer is the strings
// joined by line breaks: what String hides joined, Strings hides apart).
// With fits not nil, the form of the markers is the first at which fits holds (PiecesFitting) instead.
func (r *Redactor) piecesWithin(wm *meter.Meter, ps []Piece, limit int, joined bool, fits func([]string) bool) []string {
	var data []string
	for _, p := range ps {
		tick(wm, 1)
		if p.Data {
			data = append(data, p.Text)
		}
	}
	starts := seqValues(wm, data)
	ws := make([]written, len(ps))
	var raw strings.Builder
	for i, p := range ps {
		tick(wm, 1)
		ws[i] = write(wm, p)
		raw.WriteString(ws[i].text)
	}
	// own holds, by piece, the spans found in its written text; reading 1 first. A value repeated in the pieces
	// (a label in the series lines of several metric templates) is read once: ownSpans depends on the text only.
	own := make([][]span, len(ps))
	read := make(map[string][]span)
	// The long windows the rules read in the values alone, read again in the answer only where something else
	// stands around them (windows.go).
	ids := &windowIDs{}
	k := 0
	for i, p := range ps {
		tick(wm, 1)
		w := ws[i]
		if !p.Data {
			continue
		}
		if hiddenByObject(wm, p) {
			own[i] = []span{{start: 0, end: len(w.text), kind: KindCredential, rule: len(r.rules)}}
		} else {
			found, ok := read[p.Text]
			if !ok {
				found = r.ownSpansWith(wm, p.Text, ids)
				read[p.Text] = found
			}
			for _, sp := range found {
				tick(wm, 1)
				own[i] = append(own[i], w.carry(wm, sp))
			}
			if s := starts[k]; s >= 0 && s < len(p.Text) {
				own[i] = append(own[i], w.carry(wm, span{start: s, end: len(p.Text), kind: KindCredential, rule: len(r.rules)}))
			}
		}
		k++
	}
	// Reading 2: the answer as the tool writes it, each match ended at the line breaks of the values.
	wsize := func(i int) int { return len(ws[i].text) }
	answer := raw.String()
	matches := r.ruleMatchesWith(wm, answer, ids)
	widen := func(i int, sp span) span { return ws[i].widen(wm, sp) }
	text := func(i int) string { return ws[i].text }
	if joined {
		// What String hides in the answer, the reading of the release before included, as String reads it: a match
		// is not ended at the line breaks of the values (#845, fifteenth decision, P2).
		whole := append(slices.Clone(matches), r.releasedString(wm, answer, false, nil)...)
		r.cutAnswer(wm, ps, own, settle(wm, answer, whole), widen, text)
	}
	reading2 := endAtValueLines(wm, ps, ws, answer, matches)
	r.cutAnswer(wm, ps, own, settle(wm, answer, endAtObjectValues(wm, ps, wsize, reading2)), widen, text)
	// Reading 3: the release before, on the pieces as they were read.
	hidden := mergeSpans(wm, r.releasedPieces(wm, ps))
	off, h := 0, 0
	for i, p := range ps {
		tick(wm, 1)
		end := off + len(p.Text)
		for h < len(hidden) && hidden[h].end <= off {
			tick(wm, 1)
			h++
		}
		for j := h; p.Data && j < len(hidden) && hidden[j].start < end; j++ {
			tick(wm, 1)
			sp := hidden[j]
			from := max(sp.start, off) - off
			if sp.start < off {
				// A span that begins before the piece takes it from its first byte that is no blank, the blanks before
				// that kept as the indentation they are, as for reading 2 (cutAnswer).
				for from < len(p.Text) && strings.IndexByte(" \t\r\n", p.Text[from]) >= 0 {
					tick(wm, 1)
					from++
				}
			}
			sp.start, sp.end = from, min(sp.end, end)-off
			if sp.end > sp.start { // an empty value has no byte to carry the span to
				own[i] = append(own[i], ws[i].carry(wm, sp))
			}
		}
		off = end
	}
	merged := make([][]span, len(ps))
	for i, p := range ps {
		tick(wm, 1)
		if p.Data {
			merged[i] = mergeSpans(wm, own[i])
		}
	}
	out := make([]string, len(ps))
	if fits != nil {
		for level := range markerLevels {
			tick(wm, 1)
			for i := range ps {
				tick(wm, 1)
				out[i] = ws[i].render(wm, merged[i], level)
			}
			if fits(out) {
				break
			}
		}
		return out
	}
	level := fitLevel(wm, ws, merged, limit)
	for i := range ps {
		tick(wm, 1)
		out[i] = ws[i].render(wm, merged[i], level)
	}
	return out
}

// fitLevel returns the first level of the markers (markerAt) at which the pieces written ws, with the sorted,
// disjoint spans of each replaced (written.render), are at most limit bytes long; the last level when none is,
// 0 when limit is negative.
func fitLevel(wm *meter.Meter, ws []written, merged [][]span, limit int) int {
	level := 0
	for ; limit >= 0 && level < markerLevels-1; level++ {
		tick(wm, 1)
		n := 0
		for i, w := range ws {
			tick(wm, 1)
			n += len(w.text)
			for _, sp := range merged[i] {
				tick(wm, 1)
				n += len(markerAt(wm, sp.kind, w.size(wm, sp, level), level)) - (sp.end - sp.start)
			}
		}
		if n <= limit {
			break
		}
	}
	return level
}

// size is the length a marker of the given level is fitted to for the span sp of w's text: its bytes at level 0,
// as everywhere else; at a shorter level, as the answer must fit, its units (the characters of the value it
// covers, as the release before wrote its markers on the values before they were escaped), so `\"` is one "*".
// The work is linear in the length of sp.
func (w written) size(wm *meter.Meter, sp span, level int) int {
	if level == 0 || w.unitEnd == nil {
		return sp.end - sp.start
	}
	n := 0
	for x := sp.start; x < sp.end; x = w.unitEnd[x] {
		tick(wm, 1)
		n++
	}
	return n
}

// render returns w's text with each of the sorted, disjoint spans replaced by its marker of the given level,
// fitted to size.
func (w written) render(wm *meter.Meter, spans []span, level int) string {
	if level == 0 {
		return render(wm, w.text, spans)
	}
	var b strings.Builder
	pos := 0
	for _, sp := range spans {
		tick(wm, 1)
		b.WriteString(w.text[pos:sp.start])
		b.WriteString(markerAt(wm, sp.kind, w.size(wm, sp, level), level))
		pos = sp.end
	}
	b.WriteString(w.text[pos:])
	return b.String()
}

// cutAnswer adds to own, by data piece, the spans of a reading of the whole answer (sorted, disjoint, on the
// text of that reading, where piece i is written text(i)), cut at each piece's ends and carried to the piece's
// written text by to. A span that begins before a piece takes it from its first byte that is no blank: the blanks
// before that stay as the indentation they are, as for a value a key outside it takes. A piece the
// object hides whole takes nothing more: it is hidden whole already. The work is linear in the length of the
// text and the number of pieces and spans.
func (r *Redactor) cutAnswer(wm *meter.Meter, ps []Piece, own [][]span, spans []span, to func(i int, sp span) span, text func(i int) string) {
	off, next := 0, 0
	for i, p := range ps {
		tick(wm, 1)
		t := text(i)
		end := off + len(t)
		if p.Data && !hiddenByObject(wm, p) {
			for next < len(spans) && spans[next].end <= off {
				tick(wm, 1)
				next++
			}
			lead := -1 // the piece's blanks before its first other byte, counted when a span needs them
			for j := next; j < len(spans) && spans[j].start < end; j++ {
				tick(wm, 1)
				sp := spans[j]
				from := max(sp.start, off) - off
				if sp.start < off {
					if lead < 0 {
						lead = 0
						for lead < len(t) && strings.IndexByte(" \t\r\n", t[lead]) >= 0 {
							tick(wm, 1)
							lead++
						}
					}
					from = lead
				}
				sp.start, sp.end = from, min(sp.end, end)-off
				if sp.end > sp.start {
					own[i] = append(own[i], to(i, sp))
				}
			}
		}
		off = end
	}
}

// isLineBreak reports whether c is a line break: a line feed, a carriage return, a vertical tab, a form feed, a
// next line (U+0085), a line separator (U+2028) or a paragraph separator (U+2029).
func isLineBreak(_ *meter.Meter, c rune) bool {
	switch c {
	case '\n', '\r', '\v', '\f', '\u0085', '\u2028', '\u2029':
		return true
	}
	return false
}

// Kinds of the bytes of the answer for endAtValueLines.
const (
	byteSolid     = iota // any other byte
	byteBlank            // a space or a tab, or a line break the tool writes
	byteLineBreak        // a byte of the unit a line break of a value is written as
)

// endAtValueLines ends each match of reading 2 on the answer as written (the pieces ws joined) at the first line
// break of a value after the match's first byte that is no blank and no line break (#845, tenth decision of
// 2026-11-18). A line break inside a value, written "\n" in JSON or a space on a line, ends a line for it exactly
// as it does for the reading of that value alone, so a value hidden to the end of its line stops there and the
// lines after it stay, as main kept them. A match still reaches across what the encoding puts side by side (a
// key and its value, the items of a list, a flag and its argument), and a value that begins on the line after
// its key is still taken: the line breaks before the match's first other byte do not end it. The matches are
// changed in place and returned. The work and the allocation are linear in the length of the answer and the
// number of matches.
func endAtValueLines(wm *meter.Meter, ps []Piece, ws []written, answer string, matches []span) []span {
	n := len(answer)
	kind := make([]byte, n)
	off := 0
	for i, p := range ps {
		tick(wm, 1)
		w := ws[i]
		if !p.Data {
			for j := range len(w.text) {
				tick(wm, 1)
				if c := w.text[j]; c == ' ' || c == '\t' || c == '\n' || c == '\r' {
					kind[off+j] = byteBlank
				}
			}
			off += len(w.text)
			continue
		}
		for j := 0; j < len(p.Text); {
			tick(wm, 1)
			c, size := utf8.DecodeRuneInString(p.Text[j:])
			s, e := j, j+size
			if w.start != nil {
				s, e = w.start[j], w.end[j]
			}
			k := byte(byteSolid)
			switch {
			case c == utf8.RuneError && size == 1:
			case isLineBreak(wm, c):
				k = byteLineBreak
			case c == ' ' || c == '\t':
				k = byteBlank
			}
			for x := s; x < e; x++ {
				tick(wm, 1)
				kind[off+x] = k
			}
			j += size
		}
		off += len(w.text)
	}
	// nextSolid[x] and nextBreak[x] are the first byte at or after x of that kind, n for none.
	nextSolid, nextBreak := make([]int, n+1), make([]int, n+1)
	nextSolid[n], nextBreak[n] = n, n
	for x := n - 1; x >= 0; x-- {
		tick(wm, 1)
		nextSolid[x], nextBreak[x] = nextSolid[x+1], nextBreak[x+1]
		switch kind[x] {
		case byteSolid:
			nextSolid[x] = x
		case byteLineBreak:
			nextBreak[x] = x
		}
	}
	for i := range matches {
		tick(wm, 1)
		if q := nextSolid[min(matches[i].start, n)]; q < matches[i].end {
			matches[i].end = min(matches[i].end, nextBreak[q])
		}
	}
	return matches
}

// endAtObjectValues cuts each span of a reading of the whole answer (pieces of length size(i) in that reading's
// text) that reaches into a data piece the object hides whole (hiddenByObject) at that piece's end: the object
// says where the value ends, so a block or a flow collection that the value opens, or a key inside it, takes no
// later piece (#845, sixth decision of 2026-11-18, point 2). The spans are changed in place and returned. The work
// and the allocation are linear in the length of the text and the number of spans.
func endAtObjectValues(wm *meter.Meter, ps []Piece, size func(i int) int, spans []span) []span {
	var starts, ends []int // the hidden pieces, in order
	off := 0
	for i, p := range ps {
		tick(wm, 1)
		if n := size(i); hiddenByObject(wm, p) && n > 0 {
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
		tick(wm, 1)
		for j > 0 && ends[j-1] > x {
			tick(wm, 1)
			j--
		}
		first[x] = j
	}
	for i := range spans {
		tick(wm, 1)
		if h := first[min(spans[i].start, off)]; h < len(starts) && starts[h] < spans[i].end {
			spans[i].end = min(spans[i].end, ends[h])
		}
	}
	return spans
}

// hiddenByObject reports whether the object says the data piece p is hidden whole: it is marked Whole or its
// key is a secret key.
func hiddenByObject(wm *meter.Meter, p Piece) bool {
	return p.Data && (p.Whole || (p.Key != "" && secretKey(wm, p.Key)))
}

// written is a piece as the answer writes it, with the units it was written in: each character of a data piece
// (an invalid byte counts as one, and in JSON a run of them) is one unit of the value and one of the answer.
type written struct {
	text string
	// For a piece written in another form than AsIs: start and end give, for each byte of the value, the
	// offsets in text where the unit holding it starts and ends; unitStart and unitEnd give the same for each
	// byte of text. nil when text is the value.
	start, end, unitStart, unitEnd []int
}

// write writes the piece p as the answer writes it.
func write(wm *meter.Meter, p Piece) written {
	if !p.Data || p.Form == AsIs {
		return written{text: p.Text}
	}
	n := len(p.Text)
	w := written{start: make([]int, n), end: make([]int, n)}
	var b strings.Builder
	b.Grow(n)
	for i := 0; i < n; {
		tick(wm, 1)
		c, size := utf8.DecodeRuneInString(p.Text[i:])
		from := b.Len()
		if p.Form == JSON && c == utf8.RuneError && size == 1 {
			// A run of invalid bytes is one unit, written as one "?", as the release before made each value valid
			// UTF-8 before it was encoded (strings.ToValidUTF8 with "?"). A unit is never longer than the bytes it
			// stands for, so an answer of many runs is not longer than the value read: one escape of U+FFFD per
			// byte, then per run (six bytes for one), made an answer of a few KiB of them too long for its bound
			// (#845, fifteenth decision, P4).
			for i+size < n {
				tick(wm, 1)
				if r, sz := utf8.DecodeRuneInString(p.Text[i+size:]); r != utf8.RuneError || sz != 1 {
					break
				}
				size++
			}
			b.WriteByte('?')
		} else {
			writeUnit(wm, &b, p.Form, p.Text[i:i+size], c, size)
		}
		to := b.Len()
		for j := i; j < i+size; j++ {
			tick(wm, 1)
			w.start[j], w.end[j] = from, to
		}
		for range to - from {
			tick(wm, 1)
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
func writeUnit(_ *meter.Meter, b *strings.Builder, f Form, raw string, c rune, size int) {
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
			b.WriteByte('?')
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

// widen widens a span of the written text to the whole units it touches.
func (w written) widen(_ *meter.Meter, sp span) span {
	if w.unitStart != nil {
		sp.start, sp.end = w.unitStart[sp.start], w.unitEnd[sp.end-1]
	}
	return sp
}

// carry carries a span of the value to the units of the written text it was written as.
func (w written) carry(_ *meter.Meter, sp span) span {
	if w.start != nil {
		sp.start, sp.end = w.start[sp.start], w.end[sp.end-1]
	}
	return sp
}

// Pieces is the package's Pieces with the built-in rules.
func Pieces(ps []Piece) []string { return defaultRedactor.Pieces(ps) }

// Written returns s as the answer writes it in form f, with nothing redacted: what a redactor that takes no
// decision on pieces writes after it redacted the value alone.
func Written(f Form, s string) string { return write(nil, Piece{Text: s, Data: true, Form: f}).text }
