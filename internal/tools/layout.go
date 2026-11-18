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

package tools

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/pkalin/shadewright/internal/incidentcontext"
	"github.com/pkalin/shadewright/internal/redact"
)

// A tool result is one text, decided once (#845, third decision of 2026-11-18), and the text decided on is the
// text the tool returns (sixth decision of 2026-11-18). Each read tool writes its answer as pieces (answer): the
// text it writes (a header line, a field name, JSON punctuation) and each value from a source, written as the
// answer writes it (redact.Form: on a line of text, or as the content of a JSON string). The redactor takes one
// decision on those bytes (redact.Redactor.Pieces) and the answer is the decided pieces, joined: no field,
// list or map is decided by itself, and the answer is not redacted again. Each value carries the key the object
// gives it (a field name, a map key, a label's name), and a secret key hides the whole value (#845, fifth
// decision of 2026-11-18).

// pieceRedactor is a [Redactor] that takes one decision on an answer written as pieces (*redact.Redactor's
// Pieces).
type pieceRedactor interface {
	Pieces(ps []redact.Piece) []string
}

// answer is a tool's answer being written as pieces.
type answer struct {
	pieces []redact.Piece
}

// text appends text the tool writes and returns the index of its piece.
func (a *answer) text(t string) int {
	a.pieces = append(a.pieces, redact.Piece{Text: t})
	return len(a.pieces) - 1
}

// value appends a value from a source, written on a line of the answer, under the key the object gives it (a
// field name, a map key, a label's name; empty for none), and returns the index of its piece.
func (a *answer) value(key, v string) int {
	a.pieces = append(a.pieces, redact.Piece{Text: v, Data: true, Form: redact.Line, Key: key})
	return len(a.pieces) - 1
}

// decide takes the one decision on the pieces with the redactor and returns each piece as decided, made valid
// UTF-8. A redactor without Pieces redacts each value on its own, then writes it in its form, and hides a value
// marked Whole.
func (s *Set) decide(ps []redact.Piece) []string {
	var out []string
	if pr, ok := s.redactor.(pieceRedactor); ok {
		out = pr.Pieces(ps)
	} else {
		out = make([]string, len(ps))
		for i, p := range ps {
			out[i] = p.Text
			switch {
			case p.Whole:
				out[i] = redact.Marker(redact.KindCredential, len(redact.Written(p.Form, p.Text)))
			case p.Data:
				out[i] = redact.Written(p.Form, s.redactor.String(p.Text))
			}
		}
	}
	for i := range out {
		out[i] = strings.ToValidUTF8(out[i], "?")
	}
	return out
}

// lines takes the one decision on a in which every value is written on a line, and returns each piece as
// shown: a value longer than 1 KiB once decided is left out whole (incidentcontext.Shown), and the text the
// answer is made of.
func (s *Set) lines(a *answer) ([]string, string) {
	out := s.decide(a.pieces)
	var b strings.Builder
	for i, p := range a.pieces {
		if p.Data {
			out[i] = incidentcontext.Shown(out[i])
		}
		b.WriteString(out[i])
	}
	return out, b.String()
}

// errUnwritten means a value of an object did not reach its JSON encoding as it was laid out; the answer is not
// given (fail closed).
var errUnwritten = errors.New("a value of the object was not written as laid out")

// slot is a value of an object laid out for its JSON answer: the piece it becomes, the setter that writes the
// stand-in for it into the object, and the map whose key it is (-1 for a value that is no map key).
type slot struct {
	piece redact.Piece
	set   func(string)
	mapID int
}

// object lays out the values of an object for its JSON answer.
type object struct {
	slots []slot
	after []func() // run once every stand-in is set, in order (a map rebuilt from its keys)
	maps  int
	whole int // how many collections under a secret-named field (redact.SecretCollection) enclose the values laid out now
}

// jsonAnswer returns the JSON answer for the object v (a settable value that owns everything it reaches, a deep
// copy), decided as one text: each string of the object is replaced by a stand-in no value holds, the object is
// encoded once without HTML escapes and with an indent of one space, and the answer is the encoding with each
// stand-in replaced by its value as decided, written as the content of a JSON string. So the text decided on is
// the text returned, byte for byte. Two keys of a map that are decided alike stay two entries: the later one in
// the encoding's order (by the key as read) gets " (2)", " (3)" and so on (distinctKeys).
func (s *Set) jsonAnswer(v reflect.Value) (string, error) {
	var o object
	o.lay(v, "")
	prefix := standInPrefix(o.slots)
	width := len(strconv.Itoa(len(o.slots)))
	for i, sl := range o.slots {
		sl.set(fmt.Sprintf("%s%0*d", prefix, width, i))
	}
	for _, f := range o.after {
		f()
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", " ")
	if err := enc.Encode(v.Addr().Interface()); err != nil {
		return "", fmt.Errorf("encoding the object: %w", err)
	}
	encoded := buf.String()
	var ps []redact.Piece
	var maps []int
	for rest := encoded; ; {
		i := strings.Index(rest, prefix)
		if i < 0 {
			ps = append(ps, redact.Piece{Text: rest})
			maps = append(maps, -1)
			break
		}
		end := i + len(prefix) + width
		if end > len(rest) {
			return "", errUnwritten
		}
		n, err := strconv.Atoi(rest[i+len(prefix) : end])
		if err != nil || n < 0 || n >= len(o.slots) {
			return "", errUnwritten
		}
		ps = append(ps, redact.Piece{Text: rest[:i]}, o.slots[n].piece)
		maps = append(maps, -1, o.slots[n].mapID)
		rest = rest[end:]
	}
	for _, p := range ps {
		if !p.Data && strings.ContainsRune(p.Text, standIn) {
			return "", errUnwritten
		}
	}
	ps, maps = marshaledValues(ps, maps)
	out := s.decide(ps)
	distinctKeys(out, maps, o.maps)
	return strings.Join(out, ""), nil
}

// marshaledValues splits each JSON string in the text of ps that is no object key into a value of its own,
// under the field name before it: the text of a value that encodes itself (json.Marshaler: a quantity, a time,
// an int-or-string's string), whose fields lay does not enter, so that it is decided as a value like any
// other and not left as text the tool writes. mapOf is extended alike. The work is linear in the length of the
// text.
func marshaledValues(ps []redact.Piece, mapOf []int) ([]redact.Piece, []int) {
	var out []redact.Piece
	var outMaps []int
	key := ""
	for i, p := range ps {
		if p.Data {
			out, outMaps = append(out, p), append(outMaps, mapOf[i])
			continue
		}
		t, last := p.Text, 0
		for j := 0; j < len(t); j++ {
			if t[j] != '"' {
				continue
			}
			end := j + 1
			for end < len(t) && t[end] != '"' {
				if t[end] == '\\' {
					end++
				}
				end++
			}
			if end >= len(t) {
				break
			}
			after := end + 1
			for after < len(t) && t[after] == ' ' {
				after++
			}
			if after < len(t) && t[after] == ':' {
				key = t[j+1 : end]
				j = end
				continue
			}
			var raw string
			if err := json.Unmarshal([]byte(t[j:end+1]), &raw); err != nil {
				j = end
				continue
			}
			out = append(out, redact.Piece{Text: t[last : j+1]}, redact.Piece{Text: raw, Data: true, Form: redact.JSON, Key: key})
			outMaps = append(outMaps, -1, -1)
			last, j = end, end
		}
		out, outMaps = append(out, redact.Piece{Text: t[last:]}), append(outMaps, -1)
	}
	return out, outMaps
}

// marshaler is the interface of a type that writes its own JSON.
var marshaler = reflect.TypeFor[json.Marshaler]()

// encodesItself reports whether values of t write their own JSON (json.Marshaler, on the value or its pointer):
// a stand-in written into one of its fields would change how the rest of it is written (a quantity's format).
func encodesItself(t reflect.Type) bool {
	return t.Implements(marshaler) || reflect.PointerTo(t).Implements(marshaler)
}

// standIn is the character the stand-ins of jsonAnswer are made of: a private use character, which
// encoding/json writes as it is.
const standIn = ''

// standInPrefix returns the start of every stand-in: one standIn more than the longest run of it in any value,
// so no value holds it and the encoding of a value, inside its quotes, never runs into one.
func standInPrefix(slots []slot) string {
	longest := 0
	for _, sl := range slots {
		run := 0
		for _, c := range sl.piece.Text {
			if c != standIn {
				run = 0
				continue
			}
			run++
			longest = max(longest, run)
		}
	}
	return strings.Repeat(string(standIn), longest+1)
}

// distinctKeys renames, in place, the keys of each map that are decided alike, so a map keeps as many entries as
// the object has (#845, fifth decision of 2026-11-18): the first key keeps its text, each later one gets " (n)",
// n from 2 on, skipping a name that a key of the map already has or a renamed key took. One counter per decided
// key (sixth decision of 2026-11-18, point 5): a name is tried at most once for each key that takes or skips it,
// so the work is linear in the number of keys and the bytes of their names. out are the pieces as decided and
// mapOf the map each piece is a key of (-1 for none); the names stay the same on every call, as the order of the
// keys does.
func distinctKeys(out []string, mapOf []int, maps int) {
	if maps == 0 {
		return
	}
	taken := make([]map[string]bool, maps)
	for i, m := range mapOf {
		if m < 0 {
			continue
		}
		if taken[m] == nil {
			taken[m] = map[string]bool{}
		}
		taken[m][out[i]] = true
	}
	used := make([]map[string]bool, maps)
	next := make([]map[string]int, maps)
	for i, m := range mapOf {
		if m < 0 {
			continue
		}
		if used[m] == nil {
			used[m], next[m] = map[string]bool{}, map[string]int{}
		}
		k := out[i]
		if !used[m][k] {
			used[m][k] = true
			continue
		}
		n := max(next[m][k], 2)
		name := k + " (" + strconv.Itoa(n) + ")"
		for taken[m][name] || used[m][name] {
			n++
			name = k + " (" + strconv.Itoa(n) + ")"
		}
		next[m][k] = n + 1
		used[m][name] = true
		out[i] = name
	}
}

// jsonName returns the name encoding/json gives a struct field, and whether its fields are inlined (an embedded
// struct without a name) or it is left out ("-").
func jsonName(f reflect.StructField) (name string, inline, skip bool) {
	tag := f.Tag.Get("json")
	if tag == "-" {
		return "", false, true
	}
	name, _, _ = strings.Cut(tag, ",")
	if name == "" && f.Anonymous {
		return "", true, false
	}
	if name == "" {
		name = f.Name
	}
	return name, false, false
}

// add records a string of the object under the key the object gives it.
func (o *object) add(key, v string, set func(string), mapID int) {
	o.slots = append(o.slots, slot{
		piece: redact.Piece{Text: v, Data: true, Form: redact.JSON, Whole: o.whole > 0, Key: key},
		set:   set, mapID: mapID,
	})
}

// lay records the strings reachable from v, under the field name (empty for the root or an inlined struct), with
// a setter that writes a stand-in into v. v must be settable and own everything it reaches (a deep copy):
// strings are written in place and maps are rebuilt. Unexported fields (a time, a quantity) hold no free text
// and keep their encoding; so do empty strings, numbers and booleans. A map without string keys and an array
// are left out of the answer: none holds text in the objects the tools read, and one that did would not be
// decided (fail closed).
func (o *object) lay(v reflect.Value, name string) {
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			o.lay(v.Elem(), name)
		}
	case reflect.String:
		if v.Len() > 0 && v.CanSet() {
			o.add(name, v.String(), v.SetString, -1)
		}
	case reflect.Struct:
		if encodesItself(v.Type()) {
			// Its encoding is split into values after it is written (marshaledValues).
			return
		}
		o.under(name, func() {
			named := secretNamed(v)
			for sf, f := range v.Fields() {
				if !sf.IsExported() {
					continue
				}
				n, inline, skip := jsonName(sf)
				switch {
				case skip:
				case inline:
					o.lay(f, "")
				case named && (n == "value" || n == "values"):
					o.whole++
					o.lay(f, n)
					o.whole--
				default:
					o.lay(f, n)
				}
			}
		})
	case reflect.Slice:
		o.under(name, func() {
			for i := range v.Len() {
				o.lay(v.Index(i), "")
			}
		})
	case reflect.Map:
		if v.Len() == 0 {
			return
		}
		if v.Type().Key().Kind() != reflect.String {
			if v.CanSet() {
				v.SetZero()
			}
			return
		}
		o.under(name, func() { o.mapEntries(v) })
	case reflect.Array:
		if v.CanSet() {
			v.SetZero()
		}
	}
}

// secretNamed reports whether the struct v names a secret key in a field "key" or "name" (redact.SecretKey):
// then its fields "value" and "values" hold that key's value, as the object says (a selector's requirement
// {key, operator, values}, an HTTP header {name, value}, a toleration {key, value}), and are hidden whole
// (#845, fifth decision of 2026-11-18: where the object gives the key, the rule uses it). Fields of other
// names keep what the answer shows of them.
func secretNamed(v reflect.Value) bool {
	for sf, f := range v.Fields() {
		if !sf.IsExported() || f.Kind() != reflect.String {
			continue
		}
		if n, _, _ := jsonName(sf); (n == "key" || n == "name") && redact.SecretKey(f.String()) {
			return true
		}
	}
	return false
}

// under runs body for the values of a field: under a name that redact.SecretCollection reports, every value
// body records is hidden whole, as main hid that collection.
func (o *object) under(name string, body func()) {
	if name != "" && redact.SecretCollection(name) {
		o.whole++
		defer func() { o.whole-- }()
	}
	body()
}

// mapEntries records the keys and values of the map v, in the order encoding/json writes them (by key), each
// value under its key, and rebuilds v from the stand-ins once they are set. The sort takes O(n log n)
// comparisons of n keys, each reading at most the bytes two keys share at their start; encoding/json sorts the
// same keys again when the answer is encoded, as it did on main.
func (o *object) mapEntries(v reflect.Value) {
	type entry struct {
		k, e reflect.Value
		key  string
	}
	es := make([]entry, 0, v.Len())
	for it := v.MapRange(); it.Next(); {
		k := reflect.New(v.Type().Key()).Elem()
		k.Set(it.Key())
		e := reflect.New(v.Type().Elem()).Elem()
		e.Set(it.Value())
		es = append(es, entry{k, e, k.String()})
	}
	slices.SortFunc(es, func(a, b entry) int { return keyCompare(a.key, b.key) })
	id := o.maps
	o.maps++
	for _, en := range es {
		o.add("", en.key, en.k.SetString, id)
		if en.e.Kind() == reflect.String {
			if en.e.Len() > 0 {
				o.add(en.key, en.e.String(), en.e.SetString, -1)
			}
			continue
		}
		secret := redact.SecretCollection(en.key)
		if secret {
			o.whole++
		}
		o.lay(en.e, "")
		if secret {
			o.whole--
		}
	}
	o.after = append(o.after, func() {
		c := reflect.MakeMapWithSize(v.Type(), len(es))
		for _, en := range es {
			c.SetMapIndex(en.k, en.e)
		}
		v.Set(c)
	})
}

// keyCompare compares two map keys for the sort of mapEntries; a test counts its calls.
var keyCompare = strings.Compare
