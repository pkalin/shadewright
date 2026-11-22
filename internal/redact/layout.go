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
// with it (objectEnds), so a block or a flow collection that the value opens takes no later entry (sixth
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
	if size <= MaxEntry && tooManyParts(len(ps), size, PieceBytes) {
		return withheldStrings(len(ps)), false
	}
	if out, ok := metered(r, size, f); ok {
		return out, true
	}
	return withheldStrings(len(ps)), false
}

// piecesWithin is PiecesWithin; with joined true it also hides, cut at each piece's ends, everything String hides
// in the answer as written, with no match ended at a line break of a value (Strings, whose answer is the strings
// joined by line breaks: what String hides joined, Strings hides apart).
// With fits not nil, the form of the markers is the first at which fits holds (PiecesFitting) instead.
func (r *Redactor) piecesWithin(wm *meter.Meter, ps []Piece, limit int, joined bool, fits func([]string) bool) []string {
	n := 0
	for _, p := range ps {
		if p.Data {
			n++
		}
	}
	data := make([]string, 0, n) // sized once: the heap of an answer of many pieces is a few words a piece
	for _, p := range ps {
		tick(wm, 1)
		if p.Data {
			data = append(data, p.Text)
		}
	}
	starts := seqValues(wm, data)
	l := newAnswerLayout(wm, ps, n)
	// own holds, by data piece, the spans found in its written text; reading 1 first. A value repeated in the
	// pieces (a label in the series lines of several metric templates) is read once: ownSpans depends on the text
	// only.
	read := make(map[string][]span)
	// The long windows the rules read in the values alone, read again in the answer only where something else
	// stands around them (windows.go).
	ids := &windowIDs{}
	k, matches := 0, 0
	for i, p := range ps {
		tick(wm, 1)
		w := l.w(i)
		if !p.Data {
			continue
		}
		if hiddenByObject(wm, p) {
			l.add(i, span{start: 0, end: len(w.text), kind: KindCredential, rule: len(r.rules)})
		} else {
			found, ok := read[p.Text]
			if !ok {
				found = r.ownSpansWith(wm, p.Text, ids)
				read[p.Text] = found
			}
			matches += len(found)
			for _, sp := range found {
				tick(wm, 1)
				l.add(i, w.carry(wm, sp))
			}
			if s := starts[k]; s >= 0 && s < len(p.Text) {
				l.add(i, w.carry(wm, span{start: s, end: len(p.Text), kind: KindCredential, rule: len(r.rules)}))
			}
		}
		k++
	}
	l.compact(wm)
	// The spans of reading 1 stand for the matches of the rules on the values: an answer that holds more than its
	// bytes allow is withheld before reading 2 and the reading of the release before (heapGate).
	heapGate(matches, len(l.answer))
	// Reading 2: the answer as the tool writes it, each match ended at the line breaks of the values.
	// Each match is ended as it is found (lineKinds, then objectEnds) and merged with the others as
	// they come (matchList); with joined, also as it was found.
	wsize := func(i int) int { return int(l.off[i+1] - l.off[i]) }
	answer := l.answer
	kinds := valueLineKinds(wm, ps, l.w, answer)
	objects := newObjectEnds(wm, ps, wsize)
	var whole *matchList
	if joined {
		whole = r.newMatchList()
	}
	reading2 := r.newMatchList()
	r.eachRuleMatch(wm, answer, ids, func(start, end, rule int) {
		tick(wm, 1)
		if whole != nil {
			whole.add(wm, start, end, rule)
		}
		end = kinds.endAtValueLine(start, end)
		if objects != nil {
			end = objects.cut(wm, start, end)
		}
		reading2.add(wm, start, end, rule)
	})
	widen := func(i int, sp span) span { return l.w(i).widen(wm, sp) }
	text := func(i int) string { return l.w(i).text }
	if joined {
		// What String hides in the answer, the reading of the release before included, as String reads it: a match
		// is not ended at the line breaks of the values (#845, fifteenth decision, P2).
		r.releasedStringInto(wm, answer, false, nil, whole)
		r.cutAnswer(wm, ps, l.add, settle(wm, answer, whole.spans(wm)), widen, text)
		l.compact(wm)
	}
	heapGate(reading2.added, len(answer)) // before the reading of the release before (reading 3)
	r.cutAnswer(wm, ps, l.add, settle(wm, answer, reading2.spans(wm)), widen, text)
	l.compact(wm)
	r.addReleasedPieces(wm, ps, l) // reading 3
	l.finish(wm)
	out := make([]string, len(ps))
	if fits != nil {
		for level := range markerLevels {
			tick(wm, 1)
			for i := range ps {
				tick(wm, 1)
				out[i] = l.w(i).render(wm, l.spansOf(i), level)
			}
			if fits(out) {
				break
			}
		}
		return out
	}
	level := fitLevel(wm, len(ps), l.w, l.spansOf, limit)
	for i := range ps {
		tick(wm, 1)
		out[i] = l.w(i).render(wm, l.spansOf(i), level)
	}
	return out
}

// answerLayout is the answer the pieces make as written, kept so that an answer of many short pieces holds a few
// words a piece (#873, decision of 2026-11-21 on the heap): the answer once, where each piece starts in it, and
// for each data piece the map of its units and the spans found in it.
type answerLayout struct {
	answer string
	off    []int32 // where piece i starts in answer; the last is the length of answer
	data   []int32 // the index of piece i among the data pieces, -1 for a piece the tool writes
	units  []*units
	// spans holds the spans of the readings merged so far, grouped by data piece and merged within each (mergeSpans),
	// from[d] saying where those of data piece d start: a piece without one holds four bytes for them. pending holds
	// the spans of the reading under way, in the order they were found, and owner the data piece of each, until
	// compact merges them in (#1087: merged after each reading, the spans of all readings are not held at once). A
	// span is held in sixteen bytes (ruleMatch, its kind in kinds) until finish writes the spans out.
	spans   []ruleMatch
	from    []int32
	pending []ruleMatch
	owner   []int32
	kinds   []string
	final   []span
}

// newAnswerLayout writes the pieces ps, n of which are data pieces, as the answer writes them.
func newAnswerLayout(wm *meter.Meter, ps []Piece, n int) *answerLayout {
	l := &answerLayout{off: make([]int32, len(ps)+1), data: make([]int32, len(ps)), units: make([]*units, 0, n),
		from: make([]int32, n+1)}
	total := 0
	for _, p := range ps {
		total += writtenLen(p)
	}
	var raw strings.Builder
	raw.Grow(total) // sized once, so the answer is not held twice while it grows
	for i, p := range ps {
		tick(wm, 1)
		w := write(wm, p)
		l.data[i] = -1
		if p.Data {
			l.data[i] = int32(len(l.units)) // #nosec G115 -- an entry is at most MaxEntry bytes
			l.units = append(l.units, w.units)
		}
		raw.WriteString(w.text)
		l.off[i+1] = int32(raw.Len()) // #nosec G115 -- an answer is at most six times an entry
	}
	l.answer = raw.String()
	return l
}

// w returns piece i as the answer writes it.
func (l *answerLayout) w(i int) written {
	w := written{text: l.answer[l.off[i]:l.off[i+1]]}
	if d := l.data[i]; d >= 0 {
		w.units = l.units[d]
	}
	return w
}

// add adds a span of the reading under way to data piece i. The span is on the piece's bytes, so it names a
// merged range by its own end (span.named).
func (l *answerLayout) add(i int, sp span) {
	m := ruleMatch{start: int32(sp.start), end: int32(sp.end), rule: int16(sp.rule), kind: kindIndex(&l.kinds, sp.kind)} // #nosec G115 -- as in newAnswerLayout; a redactor has far fewer than 32767 rules
	l.pending, l.owner = append(l.pending, m), append(l.owner, l.data[i])
}

// compact merges the spans of the reading under way into those of the readings before, piece by piece, once
// that reading has added all its own: each piece's spans are those of the readings so far merged (mergeSpans),
// the same ranges with the same names as when every reading's spans are merged at once.
func (l *answerLayout) compact(wm *meter.Meter) {
	if len(l.pending) == 0 {
		return
	}
	pending, owner := l.pending, l.owner
	grouped := true
	for x := 1; x < len(owner) && grouped; x++ {
		tick(wm, 1)
		grouped = owner[x-1] <= owner[x]
	}
	if !grouped {
		// Grouped by data piece, each piece's spans in the order they were found. A reading adds its spans piece
		// after piece, so this is not needed today.
		at := make([]int32, len(l.from))
		for _, d := range l.owner {
			tick(wm, 1)
			at[d+1]++
		}
		for d := 1; d < len(at); d++ {
			tick(wm, 1)
			at[d] += at[d-1]
		}
		pending, owner = make([]ruleMatch, len(l.pending)), make([]int32, len(l.owner))
		for x, d := range l.owner {
			tick(wm, 1)
			pending[at[d]], owner[at[d]] = l.pending[x], d
			at[d]++
		}
	}
	out := make([]ruleMatch, 0, len(l.spans)+len(pending))
	from := make([]int32, len(l.from))
	p := 0
	for d := range len(l.from) - 1 {
		tick(wm, 1)
		start := len(out)
		out = append(out, l.spans[l.from[d]:l.from[d+1]]...)
		q := p
		for q < len(pending) && int(owner[q]) == d {
			tick(wm, 1)
			q++
		}
		if q > p {
			out = append(out, pending[p:q]...)
			out = out[:start+len(mergeMatches(wm, out[start:]))]
			p = q
		}
		from[d+1] = int32(len(out)) // #nosec G115 -- as in newAnswerLayout
	}
	if 4*len(out) < 3*cap(out) {
		out = append([]ruleMatch(nil), out...) // what merged away is not held
	}
	l.spans, l.from, l.pending, l.owner = out, from, nil, nil
}

// finish merges the last reading in and writes the spans out, for spansOf.
func (l *answerLayout) finish(wm *meter.Meter) {
	l.compact(wm)
	l.final = make([]span, len(l.spans))
	for x, m := range l.spans {
		tick(wm, 1)
		l.final[x] = span{start: int(m.start), end: int(m.end), kind: l.kinds[m.kind], rule: int(m.rule)}
	}
	l.spans = nil
}

// spansOf returns the spans of piece i, nil for a piece the tool writes; once finish has run.
func (l *answerLayout) spansOf(i int) []span {
	d := l.data[i]
	if d < 0 {
		return nil
	}
	return l.final[l.from[d]:l.from[d+1]]
}

// addReleasedPieces adds to l reading 3 of piecesWithin: what the release before hid on the pieces as they were
// read, each span carried to the pieces it covers.
func (r *Redactor) addReleasedPieces(wm *meter.Meter, ps []Piece, l *answerLayout) {
	hidden := r.releasedPieces(wm, ps) // sorted and merged
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
				l.add(i, l.w(i).carry(wm, sp))
			}
		}
		off = end
	}
}

// fitLevel returns the first level of the markers (markerAt) at which the n pieces written w(i), with the sorted,
// disjoint spans of each, spans(i), replaced (written.render), are at most limit bytes long; the last level when
// none is, 0 when limit is negative.
func fitLevel(wm *meter.Meter, n int, w func(i int) written, spans func(i int) []span, limit int) int {
	level := 0
	for ; limit >= 0 && level < markerLevels-1; level++ {
		tick(wm, 1)
		size := 0
		for i := range n {
			tick(wm, 1)
			wi := w(i)
			size += len(wi.text)
			for _, sp := range spans(i) {
				tick(wm, 1)
				size += len(markerAt(wm, sp.kind, wi.size(wm, sp, level), level)) - (sp.end - sp.start)
			}
		}
		if size <= limit {
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
	if level == 0 || !w.mapped() || sp.end <= sp.start {
		return sp.end - sp.start
	}
	tick(wm, 1)
	_, x := w.unitOfText(sp.start)
	n := 1
	for ; x < sp.end; x += w.textUnit(x) {
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

// cutAnswer adds with add, by data piece, the spans of a reading of the whole answer (sorted, disjoint, on the
// text of that reading, where piece i is written text(i)), cut at each piece's ends and carried to the piece's
// written text by to. A span that begins before a piece takes it from its first byte that is no blank: the blanks
// before that stay as the indentation they are, as for a value a key outside it takes. A piece the
// object hides whole takes nothing more: it is hidden whole already. The work is linear in the length of the
// text and the number of pieces and spans.
func (r *Redactor) cutAnswer(wm *meter.Meter, ps []Piece, add func(i int, sp span), spans []span, to func(i int, sp span) span, text func(i int) string) {
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
					add(i, to(i, sp))
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

// Kinds of the bytes of the answer for endAtValueLines, two bits each.
const (
	byteSolid     = iota // any other byte
	byteBlank            // a space or a tab, or a line break the tool writes
	byteLineBreak        // a byte of the unit a line break of a value is written as
)

// kindBlock is the number of bytes of the answer a block of lineKinds summarises.
const kindBlock = 64

// lineKinds holds the kind of each byte of an answer, two bits a byte, and for each block of kindBlock bytes the
// first block at or after it that holds a byte of each kind that ends a search (byteSolid, byteLineBreak): about
// three eighths of a byte for each byte of the answer, so the heap it holds stays a small part of the bound of an
// entry (#873, decision of 2026-11-21 on the heap).
type lineKinds struct {
	n     int
	bits  []uint64
	solid []int32 // the first block at or after b with a solid byte, len(solid)-1 for none
	brk   []int32 // the same for a byte of a line break of a value
}

// newLineKinds returns the kinds of an answer of n bytes, every byte solid until set.
func newLineKinds(n int) *lineKinds {
	return &lineKinds{n: n, bits: make([]uint64, (n+31)/32)}
}

func (l *lineKinds) set(x int, k byte) { l.bits[x/32] |= uint64(k) << (2 * (x % 32)) }

func (l *lineKinds) at(x int) byte {
	return byte(l.bits[x/32]>>(2*(x%32))) & 3 // #nosec G115 -- the low two bits are kept
}

// index builds the summaries of the blocks, once every kind is set: one step a byte.
func (l *lineKinds) index(wm *meter.Meter) {
	nb := (l.n + kindBlock - 1) / kindBlock
	l.solid, l.brk = make([]int32, nb+1), make([]int32, nb+1)
	l.solid[nb], l.brk[nb] = int32(nb), int32(nb) // #nosec G115 -- an entry is at most MaxEntry bytes
	for x := l.n - 1; x >= 0; x-- {
		tick(wm, 1)
		b := x / kindBlock
		if x == min(l.n, (b+1)*kindBlock)-1 { // the last byte of its block: the block starts as the one after it
			l.solid[b], l.brk[b] = l.solid[b+1], l.brk[b+1]
		}
		switch l.at(x) {
		case byteSolid:
			l.solid[b] = int32(b) // #nosec G115 -- as above
		case byteLineBreak:
			l.brk[b] = int32(b) // #nosec G115 -- as above
		}
	}
}

// next returns the first byte at or after x of kind k (byteSolid or byteLineBreak), n for none: a scan of at most
// two blocks.
func (l *lineKinds) next(x int, k byte) int {
	blocks := l.solid
	if k == byteLineBreak {
		blocks = l.brk
	}
	b := x / kindBlock
	if b >= len(blocks)-1 {
		return l.n
	}
	if int(blocks[b]) == b {
		for ; x < min(l.n, (b+1)*kindBlock); x++ {
			if l.at(x) == k {
				return x
			}
		}
	}
	b = int(blocks[b+1])
	if b >= len(blocks)-1 {
		return l.n
	}
	for x = b * kindBlock; l.at(x) != k; x++ {
	}
	return x
}

// endAtValueLines ends each match of reading 2 on the answer as written (the pieces ws joined) at the first line
// break of a value after the match's first byte that is no blank and no line break (#845, tenth decision of
// 2026-11-18). A line break inside a value, written "\n" in JSON or a space on a line, ends a line for it exactly
// as it does for the reading of that value alone, so a value hidden to the end of its line stops there and the
// lines after it stay, as main kept them. A match still reaches across what the encoding puts side by side (a
// key and its value, the items of a list, a flag and its argument), and a value that begins on the line after
// its key is still taken: the line breaks before the match's first other byte do not end it. The matches are
// changed in place and returned. The work is linear in the length of the answer and the number of matches; the
// kinds of the bytes are held only when there is a match, in about three eighths of a byte per byte of the answer
// (lineKinds). The steps are counted as when every byte held its kind in a byte and two offsets, with or without a
// match, so the budget of an entry did not move with the heap it holds.
func endAtValueLines(wm *meter.Meter, ps []Piece, ws func(i int) written, answer string, matches []span) []span {
	kinds := valueLineKinds(wm, ps, ws, answer)
	for i := range matches {
		tick(wm, 1)
		matches[i].end = kinds.endAtValueLine(matches[i].start, matches[i].end)
	}
	return matches
}

// valueLineKinds returns the kinds of the bytes of the answer as written (the pieces ws joined) that
// endAtValueLines reads, indexed. They are built whether or not a match comes, in about three eighths of a byte per
// byte of the answer, so that a reading can end each match as it is found (#1087).
func valueLineKinds(wm *meter.Meter, ps []Piece, ws func(i int) written, answer string) *lineKinds {
	n := len(answer)
	kinds := newLineKinds(n)
	set := func(x int, k byte) {
		if k != byteSolid {
			kinds.set(x, k)
		}
	}
	off := 0
	for i, p := range ps {
		tick(wm, 1)
		w := ws(i)
		if !p.Data {
			for j := range len(w.text) {
				tick(wm, 1)
				if c := w.text[j]; c == ' ' || c == '\t' || c == '\n' || c == '\r' {
					set(off+j, byteBlank)
				}
			}
			off += len(w.text)
			continue
		}
		w.eachUnit(func(vs, ve, us, ue int) {
			for j := vs; j < ve; {
				tick(wm, 1)
				c, size := utf8.DecodeRuneInString(p.Text[j:])
				if j+size > ve {
					size = ve - j // a byte of a run of invalid bytes, one unit in JSON
				}
				k := byte(byteSolid)
				switch {
				case c == utf8.RuneError && size == 1:
				case isLineBreak(wm, c):
					k = byteLineBreak
				case c == ' ' || c == '\t':
					k = byteBlank
				}
				for x := us; x < ue; x++ {
					tick(wm, 1)
					set(off+x, k)
				}
				j += size
			}
		})
		off += len(w.text)
	}
	kinds.index(wm)
	return kinds
}

// endAtValueLine returns where endAtValueLines ends a match from start to end.
func (l *lineKinds) endAtValueLine(start, end int) int {
	if q := l.next(min(start, l.n), byteSolid); q < end {
		return min(end, l.next(q, byteLineBreak))
	}
	return end
}

// objectEnds are the data pieces the object hides whole, in order. A span of a reading of the whole answer that
// reaches into one ends at that piece's end (cut): the object says where the value ends, so a block or a flow
// collection that the value opens, or a key inside it, takes no later piece (#845, sixth decision of 2026-11-18,
// point 2). The work is logarithmic in the number of hidden pieces for each span; the allocation is linear in that
// number.
type objectEnds struct{ starts, ends []int }

// newObjectEnds returns the pieces of ps the object hides whole, in a reading where piece i is size(i) bytes
// long, or nil when there is none.
func newObjectEnds(wm *meter.Meter, ps []Piece, size func(i int) int) *objectEnds {
	var o objectEnds
	off := 0
	for i, p := range ps {
		tick(wm, 1)
		if n := size(i); hiddenByObject(wm, p) && n > 0 {
			o.starts, o.ends = append(o.starts, off), append(o.ends, off+n)
		}
		off += size(i)
	}
	if len(o.starts) == 0 {
		return nil
	}
	return &o
}

// cut returns where a span from start to end ends: at the end of the first hidden piece it
// reaches into.
func (o *objectEnds) cut(wm *meter.Meter, start, end int) int {
	tick(wm, 1)
	// The first hidden piece that ends after the span's start: the pieces are in order, so their ends are
	// sorted, and a search holds nothing for each byte of the answer.
	lo, hi := 0, len(o.ends)
	for lo < hi {
		tick(wm, 1)
		if m := int(uint(lo+hi) >> 1); o.ends[m] > start {
			hi = m
		} else {
			lo = m + 1
		}
	}
	if h := lo; h < len(o.starts) && o.starts[h] < end {
		return min(end, o.ends[h])
	}
	return end
}

// hiddenByObject reports whether the object says the data piece p is hidden whole: it is marked Whole or its
// key is a secret key.
func hiddenByObject(wm *meter.Meter, p Piece) bool {
	return p.Data && (p.Whole || (p.Key != "" && secretKey(wm, p.Key)))
}

// unitStep is the distance, in bytes, between two checkpoints of the map of a written piece's units.
const unitStep = 64

// written is a piece as the answer writes it, with the units it was written in: each character of a data piece
// (an invalid byte counts as one, and in JSON a run of them) is one unit of the value and one of the answer.
//
// The map from bytes to units is kept as checkpoints, one every unitStep bytes of the value and of the text, and
// the units between two checkpoints are read again from the value or from the text when a span is carried or
// widened (unitOfValue, unitOfText): about a sixteenth of a byte for each byte, where an offset for each byte of
// the value and two for each byte of the text held about twenty times the value for a value of characters JSON
// writes as six-byte escapes (#873, decision of 2026-11-21 on the heap). What a span is carried or widened to is
// the same.
type written struct {
	text string
	// units maps the bytes of a piece written in another form than AsIs; nil when text is the value, so that a
	// piece of an answer of many short pieces holds a text and a pointer.
	*units
}

// units is the map of a written piece's units: the value, its form, and for each checkpoint b past the first of
// the value, where in the text (unitText[b-1]) and in the value (unitValue[b-1]) the unit holding byte b*unitStep
// of the value starts; the first is 0 in both, so a short value holds no checkpoint.
type units struct {
	form                Form
	value               string
	unitText, unitValue []int32
	// textBack[b-1] is how many bytes before byte b*unitStep of the text the unit holding it starts: a unit is at
	// most six bytes long.
	textBack []uint8
}

// mapped reports whether w's text is not its value, so that its offsets are mapped by units.
func (w written) mapped() bool { return w.units != nil }

// write writes the piece p as the answer writes it.
func write(wm *meter.Meter, p Piece) written {
	if !p.Data || p.Form == AsIs || p.Text == "" {
		return written{text: p.Text} // an empty value has no byte a span is carried from
	}
	n := len(p.Text)
	if plain(p.Form, p.Text) {
		// Every unit is one byte written as itself: the text is the value and every span stays as it is, so the piece
		// holds no map (the steps are those of the loop below: a unit, its byte in the value and in the text).
		tick(wm, 3*n)
		return written{text: p.Text}
	}
	w := written{units: &units{form: p.Form, value: p.Text}}
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
				if !invalidAt(p.Text, i+size) {
					break
				}
				size++
			}
			b.WriteByte('?')
		} else {
			writeUnit(wm, &b, p.Form, p.Text[i:i+size], c, size)
		}
		to := b.Len()
		for (len(w.unitText)+1)*unitStep < i+size {
			w.unitText, w.unitValue = append(w.unitText, int32(from)), append(w.unitValue, int32(i)) // #nosec G115 -- an entry is at most MaxEntry bytes
		}
		for (len(w.textBack)+1)*unitStep < to {
			w.textBack = append(w.textBack, uint8((len(w.textBack)+1)*unitStep-from)) // #nosec G115 -- a unit is at most six bytes
		}
		tick(wm, size+to-from) // as many steps as an offset for each byte of the value and two for each of the text
		i += size
	}
	w.text = b.String()
	return w
}

// plain reports whether every byte of s is ASCII that form f writes as itself: no control character, no DEL on
// a line, no quote or backslash in JSON.
func plain(f Form, s string) bool {
	for i := range len(s) {
		switch c := s[i]; {
		case c >= utf8.RuneSelf, c < 0x20:
			return false
		case f == Line && c == 0x7f, f == JSON && (c == '"' || c == '\\'):
			return false
		}
	}
	return true
}

// writtenLen returns the length of the piece p as the answer writes it (write), without writing it.
func writtenLen(p Piece) int {
	if !p.Data || p.Form == AsIs {
		return len(p.Text)
	}
	n := 0
	for i := 0; i < len(p.Text); {
		c, size := utf8.DecodeRuneInString(p.Text[i:])
		n += unitWidth(p.Form, c, size)
		if p.Form == JSON && c == utf8.RuneError && size == 1 {
			for i+size < len(p.Text) && invalidAt(p.Text, i+size) {
				size++
			}
		}
		i += size
	}
	return n
}

// invalidAt reports whether the byte of s at i is an invalid byte: one that starts no valid UTF-8 sequence.
func invalidAt(s string, i int) bool {
	c, size := utf8.DecodeRuneInString(s[i:])
	return c == utf8.RuneError && size == 1
}

// unitWidth is the length writeUnit writes for the character c of size bytes in form f.
func unitWidth(f Form, c rune, size int) int {
	invalid := c == utf8.RuneError && size == 1
	switch f {
	case Line:
		switch {
		case invalid, unicode.IsControl(c) || c == ' ' || c == ' ':
			return 1
		}
	case JSON:
		switch {
		case invalid:
			return 1
		case c == '"' || c == '\\' || c == '\n' || c == '\r' || c == '\t' || c == '\b' || c == '\f':
			return 2
		case c < 0x20 || c == ' ' || c == ' ':
			return 6
		}
	}
	return size
}

// eachUnit calls f with each unit of w in order: its bytes [vs, ve) in the value and [us, ue) in the text. With
// form AsIs a unit is a character, or an invalid byte, and both ranges are the same.
func (w written) eachUnit(f func(vs, ve, us, ue int)) {
	if !w.mapped() {
		for i := 0; i < len(w.text); {
			_, size := utf8.DecodeRuneInString(w.text[i:])
			f(i, i+size, i, i+size)
			i += size
		}
		return
	}
	pos := 0
	for i := 0; i < len(w.value); {
		c, size := utf8.DecodeRuneInString(w.value[i:])
		width := unitWidth(w.form, c, size)
		if w.form == JSON && c == utf8.RuneError && size == 1 {
			for i+size < len(w.value) && invalidAt(w.value, i+size) {
				size++
			}
		}
		f(i, i+size, pos, pos+width)
		i += size
		pos += width
	}
}

// unitOfValue returns where in the text the unit holding byte j of the value starts and ends: at most unitStep
// bytes of the value read from the checkpoint before j, and a run of invalid bytes only up to j.
func (w written) unitOfValue(j int) (us, ue int) {
	x := j - j%unitStep
	i, pos := 0, 0
	if b := j / unitStep; b > 0 {
		i, pos = int(w.unitValue[b-1]), int(w.unitText[b-1])
	}
	for {
		c, size := utf8.DecodeRuneInString(w.value[i:])
		if w.form == JSON && c == utf8.RuneError && size == 1 {
			// A run of invalid bytes, one unit "?": byte p is in it, and j is when every byte up to j is invalid.
			p := max(i, x)
			for p < j && invalidAt(w.value, p+1) {
				p++
			}
			if p >= j {
				return pos, pos + 1
			}
			i, pos = p+1, pos+1
			continue
		}
		width := unitWidth(w.form, c, size)
		if j < i+size {
			return pos, pos + width
		}
		i, pos = i+size, pos+width
	}
}

// textUnit returns the length of the unit that starts at byte s of the text, read from the text: in JSON an
// escape ("\u" and four digits, or "\" and one character), otherwise one character (the text is valid UTF-8).
func (w written) textUnit(s int) int {
	if w.form == JSON && w.text[s] == '\\' {
		if w.text[s+1] == 'u' {
			return 6
		}
		return 2
	}
	_, size := utf8.DecodeRuneInString(w.text[s:])
	return size
}

// unitOfText returns where the unit holding byte y of the text starts and ends: at most unitStep bytes read from
// the checkpoint before y.
func (w written) unitOfText(y int) (us, ue int) {
	s := 0
	if b := y / unitStep; b > 0 {
		s = b*unitStep - int(w.textBack[b-1])
	}
	for {
		l := w.textUnit(s)
		if y < s+l {
			return s, s + l
		}
		s += l
	}
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
	if w.mapped() {
		sp.start, _ = w.unitOfText(sp.start)
		_, sp.end = w.unitOfText(sp.end - 1)
	}
	return sp
}

// carry carries a span of the value to the units of the written text it was written as.
func (w written) carry(_ *meter.Meter, sp span) span {
	if w.mapped() {
		sp.start, _ = w.unitOfValue(sp.start)
		_, sp.end = w.unitOfValue(sp.end - 1)
	}
	return sp
}

// Pieces is the package's Pieces with the built-in rules.
func Pieces(ps []Piece) []string { return defaultRedactor.Pieces(ps) }

// Written returns s as the answer writes it in form f, with nothing redacted: what a redactor that takes no
// decision on pieces writes after it redacted the value alone.
func Written(f Form, s string) string { return write(nil, Piece{Text: s, Data: true, Form: f}).text }
