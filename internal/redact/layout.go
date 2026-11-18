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
// it looks like ("--token", "--verbose" hides "--verbose"). A piece marked Whole is hidden whole. The work is
// linear in the length of the pieces.
func (r *Redactor) Pieces(ps []Piece) []string {
	var b strings.Builder
	var data []string
	for _, p := range ps {
		tick(1)
		b.WriteString(p.Text)
		if p.Data {
			data = append(data, p.Text)
		}
	}
	joined := r.spans(b.String())
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
		if p.Whole {
			own = append(own, span{start: 0, end: len(p.Text), kind: KindCredential, rule: len(r.rules)})
		}
		own = append(own, r.spans(p.Text)...)
		if s := starts[k]; s >= 0 && s < len(p.Text) {
			own = append(own, span{start: s, end: len(p.Text), kind: KindCredential, rule: len(r.rules)})
		}
		out[i] = render(p.Text, mergeSpans(own))
		k++
		off = end
	}
	return out
}

// Pieces is the package's Pieces with the built-in rules.
func Pieces(ps []Piece) []string { return defaultRedactor.Pieces(ps) }
