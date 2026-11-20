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
	"sync"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/pkalin/shadewright/internal/incidentcontext"
	"github.com/pkalin/shadewright/internal/providers"
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

// pieceRedactor is a [Redactor] that takes one decision on an answer written as pieces, with the markers fitted
// to a bound of the answer (*redact.Redactor's PiecesWithin) or to a check of the caller's own answer (its
// PiecesFitting). [New] refuses any other redactor: redacting each value alone would miss what only the whole
// answer shows (a key's signal for the value after it, a match that runs across pieces).
type pieceRedactor interface {
	Redactor
	PiecesWithin(ps []redact.Piece, limit int) ([]string, bool)
	PiecesFitting(ps []redact.Piece, fits func(out []string) bool) ([]string, bool)
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
// UTF-8, its markers fitted so that the pieces joined are at most limit bytes long where they can be (none when
// limit is negative; redact.Redactor.PiecesWithin).
//
// With fits not nil, the markers take the first form at which fits holds of the pieces as decided and made valid
// UTF-8, in place of the limit (redact.Redactor.PiecesFitting).
//
// ok is false when redaction withheld the answer whole: the redactor's flag, never the text of a piece.
func (s *Set) decide(ps []redact.Piece, limit int, fits func(out []string) bool) (out []string, ok bool) {
	if fits != nil {
		out, ok = s.redactor.PiecesFitting(ps, func(out []string) bool {
			valid := make([]string, len(out))
			for i := range out {
				valid[i] = strings.ToValidUTF8(out[i], "?")
			}
			return fits(valid)
		})
	} else {
		out, ok = s.redactor.PiecesWithin(ps, limit)
	}
	for i := range out {
		out[i] = strings.ToValidUTF8(out[i], "?")
	}
	return out, ok
}

// lines takes the one decision on a in which every value is written on a line, and returns each piece as
// shown: a value longer than 1 KiB once decided is left out whole (incidentcontext.Shown), and the text the
// answer is made of; ok is false when redaction withheld the answer whole (the flag of decide), and the
// caller then gives no answer (Result.withhold).
func (s *Set) lines(a *answer) (pieces []string, text string, ok bool) {
	out, decided := s.decide(a.pieces, -1, nil)
	if !decided {
		return nil, "", false
	}
	var b strings.Builder
	for i, p := range a.pieces {
		if p.Data {
			out[i] = incidentcontext.Shown(out[i])
		}
		b.WriteString(out[i])
	}
	return out, b.String(), true
}

// errUnwritten means the JSON the object was encoded as could not be read back as tokens along the object; the
// answer is not given (fail closed).
var errUnwritten = errors.New("the JSON of the object could not be read back along the object")

// jsonAnswer returns the JSON answer for obj, decided as one text (#845, seventh decision of 2026-11-18): obj is
// encoded once, without HTML escapes and with an indent of one space, and the JSON tokens of that output are
// walked in one pass (tokens). Every string token that holds a value is a piece, in its place in the answer, with
// the key the tokens around it give it; a type that writes its own JSON (an IntOrString, a quantity, a time, raw
// content) is tokens like any other. The text between the values (punctuation, field names, numbers, booleans,
// null) is text the tool writes; a number, boolean or null the walk hides is written as a JSON string holding the
// marker. The redactor decides on the pieces, each value written back as the content of a JSON string, so the
// text decided on is the text returned. Two keys of a map that are decided alike stay two entries: the later one
// in the encoding's order gets " (2)", " (3)" and so on (distinctKeys). The markers are fitted to limit bytes
// (decide), the bound the answer is cut to, so the answer is whole JSON whenever its replacements can make it fit
// (#845, thirteenth decision of 2026-11-18), and where they cannot, values are left out until it fits (leaveOut), so
// the answer is valid JSON within limit whatever the replacements and the renamed keys add; leftOut says whether
// anything was. obj is only read.
func (s *Set) jsonAnswer(obj any, limit int) (answer string, leftOut bool, err error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", " ")
	if err := enc.Encode(obj); err != nil {
		return "", false, fmt.Errorf("encoding the object: %w", err)
	}
	t, err := tokens(buf.Bytes(), reflect.ValueOf(obj))
	if err != nil {
		return "", false, err
	}
	// The markers are fitted to the answer as returned: the keys renamed after the decision (distinctKeys) count
	// too (#845, fifteenth decision, P4).
	out, decided := s.decide(t.ps, limit, func(out []string) bool {
		renamed := slices.Clone(out)
		distinctKeys(renamed, t.mapOf, t.maps)
		n := 0
		for _, p := range renamed {
			n += len(p)
		}
		return limit < 0 || n <= limit
	})
	if !decided {
		return "", false, providers.ErrWithheld
	}
	answer, leftOut = leaveOut(out, t, limit)
	return answer, leftOut, nil
}

// leaveOut returns the answer of the pieces decided, its map keys made distinct (distinctKeys), joined: at most
// limit bytes long and valid JSON (none when limit is negative; #845, fifteenth decision, P4). Where the shortest
// markers and the renamed keys still make it longer, values are left out whole, each replaced by a note of its size
// as a value too long for a line is (incidentcontext.LeftOut): the smallest value whose leaving out makes the
// answer fit, else the largest and again. When no value is left to leave out, every map key that is longer than
// its note is left out too, and the keys are made distinct again; when even that does not fit, the answer is one
// such note for the whole answer, as a JSON string. Values and keys are the contents of JSON strings, so a note
// keeps the answer JSON. The work is linear in the length of the pieces, apart from the sort of the values by size
// (n log n comparisons for n values). leftOut says whether anything was left out.
func leaveOut(decided []string, t walked, limit int) (answer string, leftOut bool) {
	out := slices.Clone(decided)
	distinctKeys(out, t.mapOf, t.maps)
	n := joinedLen(out)
	if limit < 0 || n <= limit {
		return strings.Join(out, ""), false
	}
	whole := n
	var values, keys []int
	for i, p := range t.ps {
		redact.Tick(1)
		switch {
		case !p.Data:
		case t.mapOf[i] >= 0:
			keys = append(keys, i)
		default:
			values = append(values, i)
		}
	}
	n = leaveOutOf(out, values, n, limit)
	if n > limit && len(keys) > 0 {
		for i := range values {
			redact.Tick(1)
			decided[values[i]] = out[values[i]]
		}
		for _, i := range keys {
			redact.Tick(1)
			if note := incidentcontext.LeftOut(len(decided[i])); len(note) < len(decided[i]) {
				decided[i] = note
			}
		}
		out = slices.Clone(decided)
		distinctKeys(out, t.mapOf, t.maps)
		n = joinedLen(out)
	}
	if n > limit {
		return strconv.Quote(incidentcontext.LeftOut(whole)), true
	}
	return strings.Join(out, ""), true
}

// leaveOutOf leaves out values of out, the pieces at the indices idx, while the pieces joined, n bytes long, are
// longer than limit: the value whose note saves the fewest bytes that are enough, else the one that saves the most,
// and again. It returns the length of the pieces joined.
func leaveOutOf(out []string, idx []int, n, limit int) int {
	saving := func(i int) int { return len(out[i]) - len(incidentcontext.LeftOut(len(out[i]))) }
	var c []int
	for _, i := range idx {
		redact.Tick(1)
		if saving(i) > 0 {
			c = append(c, i)
		}
	}
	slices.SortStableFunc(c, func(a, b int) int { return saving(a) - saving(b) })
	for n > limit && len(c) > 0 {
		redact.Tick(1)
		k, _ := slices.BinarySearchFunc(c, n-limit, func(i, need int) int { return saving(i) - need })
		if k == len(c) {
			k = len(c) - 1
		}
		i := c[k]
		n -= saving(i)
		out[i] = incidentcontext.LeftOut(len(out[i]))
		c = slices.Delete(c, k, k+1)
	}
	return n
}

// joinedLen is the length of the pieces joined.
func joinedLen(out []string) int {
	n := 0
	for _, p := range out {
		redact.Tick(1)
		n += len(p)
	}
	return n
}

// holds is what a JSON object or array of the answer holds.
type holds int

const (
	// fieldsOf: the fields of a struct. Its keys are field names, text the tool writes.
	fieldsOf holds = iota
	// entriesOf: the entries of a map, or an object inside a value that writes its own JSON. Its keys are values.
	entriesOf
	// itemsOf: the items of a list.
	itemsOf
)

// frame is an object or array of the answer being read.
type frame struct {
	kind holds
	// val is the value of the object the container was encoded from; not valid inside a value that writes its own
	// JSON, where every object is read as entries and every key as a value.
	val   reflect.Value
	mapID int // entriesOf: the map's number for distinctKeys
	// keys are the keys of a map with string keys, by the text encoding/json writes for them (written).
	keys map[string]reflect.Value
	// An object: whether the next string is a key, the key of the member being read, the value it was encoded from
	// (not valid when unknown), and the first piece of that value. A list: the index of the next item.
	wantKey bool
	key     string
	member  reflect.Value
	from    int
	next    int
	// fieldsOf: a field "key" or "name" of kind string names a secret key (redact.SecretKey), and the pieces of
	// the fields "value" and "values", hidden whole when it does once the struct is read.
	named  bool
	values [][2]int
}

// walked is the answer as pieces: each piece, the map each piece is a key of (-1 for none), and the number of
// maps.
type walked struct {
	ps    []redact.Piece
	mapOf []int
	maps  int
}

// tokens reads b, the JSON encoding of root, in one pass and returns it as pieces. A string token that is a value
// or a map key (not a struct's field name, not empty) becomes a data piece written as the content of a JSON
// string, and its key is the key of the member it is the value of (a field name or a map key; none in a list).
// Everything else is text the tool writes, quotes included.
//
// The text of a piece is the string of the object as it was read, which the walk reaches along the tokens (a
// struct's fields by their JSON names, a map's entries by their keys, a list's items in order): the encoding writes
// an invalid byte as U+FFFD, which the redactor reads as a letter where it reads an invalid byte as indentation, so
// the token alone is not the value. The token must be the encoding of that string, or the walk fails (fail closed).
// Inside a value that writes its own JSON there is no string to reach, and the text is what the token decodes to;
// an IntOrString's string is its exported field of that text (ownText).
//
// The key decides alone (redact.SecretKey, in Pieces); the walk marks whole, as main hid them:
//   - every value inside an object or array under a field or map key that redact.SecretCollection reports (the
//     values and keys of a collection under "secrets"), the key itself not;
//   - the fields "value" and "values" of a struct whose field "key" or "name" names a secret key (a selector's
//     requirement {key, operator, values}, an HTTP header {name, value}).
//
// A number, boolean or null is text the tool writes, hidden when it is inside such a range or its own key is a
// secret key, as main hid it (#845, eighth decision of 2026-11-18). A map key is a piece marked MapKey, decided
// also as the answer writes it, as main decided it.
//
// The work is linear in len(b) and the size of root: each byte is read once, each map's keys once, and each piece
// is marked whole by a difference array, not once for each range around it.
func tokens(b []byte, root reflect.Value) (walked, error) {
	k := walker{b: b, root: root}
	for i := 0; i < len(b); {
		redact.Tick(1)
		var err error
		switch c := b[i]; c {
		case ' ', '\n', '\t', '\r', ',', ':':
			i++
		case '{', '[':
			k.open(c)
			i++
		case '}', ']':
			err = k.close()
			i++
		case '"':
			i, err = k.str(i)
		default:
			i = k.literal(i)
		}
		if err != nil {
			return walked{}, err
		}
	}
	return k.finish()
}

// walker is the state of tokens.
type walker struct {
	b      []byte
	root   reflect.Value
	w      walked
	stack  []frame
	ranges [][2]int // pieces hidden whole, [from, to)
	text   int      // the start of the text not yet in a piece
	// literals are the pieces of the numbers, booleans and nulls, in order.
	literals []literalAt
}

// piece adds the data piece p for the bytes b[from:to], after the text before it.
func (k *walker) piece(from, to int, p redact.Piece, mapID int) {
	if from > k.text {
		k.w.ps, k.w.mapOf = append(k.w.ps, redact.Piece{Text: string(k.b[k.text:from])}), append(k.w.mapOf, -1)
	}
	k.w.ps, k.w.mapOf = append(k.w.ps, p), append(k.w.mapOf, mapID)
	k.text = to
}

// top returns the frame being read; nil before the first.
func (k *walker) top() *frame {
	if len(k.stack) == 0 {
		return nil
	}
	return &k.stack[len(k.stack)-1]
}

// current is the value read next in the top frame (root before any), its pointers and interfaces followed.
func (k *walker) current() reflect.Value {
	f := k.top()
	switch {
	case f == nil:
		return direct(k.root)
	case f.kind != itemsOf:
		return f.member
	case !f.val.IsValid() || f.next >= f.val.Len():
		return reflect.Value{}
	}
	return direct(f.val.Index(f.next))
}

// done ends the value of the member being read in the top frame: a string (str, its text v), or another value, a
// container when isContainer.
func (k *walker) done(str bool, v string, isContainer bool) {
	f := k.top()
	if f == nil {
		return
	}
	if f.kind == itemsOf {
		f.next++
		return
	}
	f.wantKey = true
	span := [2]int{f.from, len(k.w.ps)}
	if isContainer && redact.SecretCollection(f.key) {
		k.ranges = append(k.ranges, span)
	}
	if f.kind != fieldsOf {
		return
	}
	switch f.key {
	case "value", "values":
		f.values = append(f.values, span)
	case "key", "name":
		if str && f.member.IsValid() && f.member.Kind() == reflect.String && redact.SecretKey(v) {
			f.named = true
		}
	}
}

// open starts the object or array c opens, read along the current value.
func (k *walker) open(c byte) {
	v := k.current()
	if v.IsValid() && writesItself(v.Type()) {
		v = reflect.Value{}
	}
	f := frame{wantKey: c == '{'}
	switch {
	case c == '[':
		f.kind = itemsOf
		if v.IsValid() && (v.Kind() == reflect.Slice || v.Kind() == reflect.Array) {
			f.val = v
		}
	case v.IsValid() && v.Kind() == reflect.Struct:
		f.kind, f.val = fieldsOf, v
	default:
		f.kind, f.mapID = entriesOf, k.w.maps
		k.w.maps++
		if v.IsValid() && v.Kind() == reflect.Map && v.Type().Key().Kind() == reflect.String {
			f.val, f.keys = v, make(map[string]reflect.Value, v.Len())
			for it := v.MapRange(); it.Next(); {
				redact.Tick(1)
				f.keys[written(it.Key().String())] = it.Key()
			}
		}
	}
	k.stack = append(k.stack, f)
}

// close ends the top frame: a struct whose name is a secret key hides its value fields whole.
func (k *walker) close() error {
	f := k.top()
	if f == nil {
		return errUnwritten
	}
	if f.named {
		k.ranges = append(k.ranges, f.values...)
	}
	k.stack = k.stack[:len(k.stack)-1]
	k.done(false, "", true)
	return nil
}

// str reads the string token at b[i] and returns the index after it.
func (k *walker) str(i int) (int, error) {
	end, ok := stringEnd(k.b, i)
	if !ok {
		return 0, errUnwritten
	}
	v, ok := unquote(k.b[i+1 : end])
	if !ok {
		return 0, errUnwritten
	}
	if f := k.top(); f != nil && f.wantKey {
		return end + 1, k.key(f, i, end, v)
	}
	read, err := asRead(k.current(), v)
	if err != nil {
		return 0, err
	}
	if read != "" {
		key := ""
		if f := k.top(); f != nil && f.kind != itemsOf {
			key = f.key
		}
		k.piece(i+1, end, redact.Piece{Text: read, Data: true, Form: redact.JSON, Key: key}, -1)
	}
	k.done(true, read, false)
	return end + 1, nil
}

// key reads the key v of a member of f, the token at b[i:end+1]: a struct's field name is text, a map's key a
// piece.
func (k *walker) key(f *frame, i, end int, v string) error {
	f.key, f.wantKey, f.member = v, false, reflect.Value{}
	switch {
	case f.kind == fieldsOf:
		f.member = field(f.val, v)
	case f.keys != nil:
		mk, ok := f.keys[v]
		if !ok {
			return errUnwritten
		}
		f.key, f.member = mk.String(), direct(f.val.MapIndex(mk))
		k.piece(i+1, end, redact.Piece{Text: f.key, Data: true, Form: redact.JSON, MapKey: true}, f.mapID)
	default:
		k.piece(i+1, end, redact.Piece{Text: v, Data: true, Form: redact.JSON, MapKey: true}, f.mapID)
	}
	f.from = len(k.w.ps)
	return nil
}

// literal reads the number, true, false or null at b[i] and returns the index after it. The token is a piece of
// text the tool writes, so that a range hidden whole holds it, and is hidden by finish when it is in one or its key
// is a secret key (redact.SecretKey), as main hid "automountServiceAccountToken": true.
func (k *walker) literal(i int) int {
	from := i
	for i < len(k.b) && !strings.ContainsRune(" \n\t\r,:]}", rune(k.b[i])) {
		redact.Tick(1)
		i++
	}
	keyed := false
	if f := k.top(); f != nil && f.kind != itemsOf {
		keyed = redact.SecretKey(f.key)
	}
	k.piece(from, i, redact.Piece{Text: string(k.b[from:i])}, -1)
	k.literals = append(k.literals, literalAt{piece: len(k.w.ps) - 1, keyed: keyed})
	k.done(false, "", false)
	return i
}

// literalAt is the piece of a number, true, false or null, and whether its key is a secret key.
type literalAt struct {
	piece int
	keyed bool
}

// finish returns the pieces once every byte is read, with each piece inside a range hidden whole marked Whole.
func (k *walker) finish() (walked, error) {
	if len(k.stack) > 0 {
		return walked{}, errUnwritten
	}
	w := k.w
	if k.text < len(k.b) {
		w.ps, w.mapOf = append(w.ps, redact.Piece{Text: string(k.b[k.text:])}), append(w.mapOf, -1)
	}
	diff := make([]int, len(w.ps)+1)
	for _, r := range k.ranges {
		redact.Tick(1)
		diff[r[0]]++
		diff[r[1]]--
	}
	in, lit := 0, 0
	for i := range w.ps {
		redact.Tick(1)
		in += diff[i]
		w.ps[i].Whole = in > 0 && w.ps[i].Data
		if lit < len(k.literals) && k.literals[lit].piece == i {
			// A number, boolean or null hidden is written as a JSON string holding the marker, so the answer
			// stays JSON (#845, eighth decision of 2026-11-18, point 2).
			if in > 0 || k.literals[lit].keyed {
				w.ps[i].Text = `"` + redact.Marker(redact.KindCredential, len(w.ps[i].Text)) + `"`
			}
			lit++
		}
	}
	return w, nil
}

// asRead returns the string of the object that the string token decoded to tok was encoded from: v's string when
// v is one (tok must be its encoding), the string of a value that writes its own JSON (ownText), or tok itself
// when there is no string to reach (a byte slice, a value inside raw content).
func asRead(v reflect.Value, tok string) (string, error) {
	switch {
	case !v.IsValid():
		return tok, nil
	case v.Kind() == reflect.String:
		if written(v.String()) != tok {
			return "", errUnwritten
		}
		return v.String(), nil
	case writesItself(v.Type()):
		return ownText(v, tok), nil
	}
	return tok, nil
}

// ownText returns the string of v, a value that writes its own JSON as the string tok: its exported string field
// whose text encodes as tok (an IntOrString's StrVal), or tok when it has none (a quantity, a time).
func ownText(v reflect.Value, tok string) string {
	if v.Kind() != reflect.Struct {
		return tok
	}
	for sf, f := range v.Fields() {
		redact.Tick(1)
		if sf.IsExported() && f.Kind() == reflect.String && f.Len() > 0 && written(f.String()) == tok {
			return f.String()
		}
	}
	return tok
}

// written returns s as encoding/json writes it, decoded again: each invalid byte becomes U+FFFD. The work is linear
// in len(s).
func written(s string) string {
	redact.Tick(len(s))
	if utf8.ValidString(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	for i := 0; i < len(s); {
		redact.Tick(1)
		r, size := utf8.DecodeRuneInString(s[i:])
		b.WriteRune(r) // utf8.RuneError for an invalid byte
		i += size
	}
	return b.String()
}

// direct returns v with its pointers and interfaces followed; not valid for a nil one.
func direct(v reflect.Value) reflect.Value {
	for v.IsValid() && (v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface) {
		if v.IsNil() {
			return reflect.Value{}
		}
		v = v.Elem()
	}
	return v
}

// field returns the field of the struct v that encoding/json writes under name, its pointers followed; not valid
// when there is none or an embedded pointer on the way is nil.
func field(v reflect.Value, name string) reflect.Value {
	if !v.IsValid() {
		return reflect.Value{}
	}
	idx, ok := fieldIndexes(v.Type())[name]
	if !ok {
		return reflect.Value{}
	}
	f, err := v.FieldByIndexErr(idx)
	if err != nil {
		return reflect.Value{}
	}
	return direct(f)
}

// stringEnd returns the index of the quote that ends the JSON string starting at the quote b[start].
func stringEnd(b []byte, start int) (int, bool) {
	for j := start + 1; j < len(b); j++ {
		redact.Tick(1)
		switch b[j] {
		case '\\':
			j++
		case '"':
			return j, true
		}
	}
	return 0, false
}

// unquote returns the string the content of a JSON string decodes to, as encoding/json decodes it: a lone
// surrogate becomes U+FFFD. It fails on an escape JSON does not have.
func unquote(s []byte) (string, bool) {
	if bytes.IndexByte(s, '\\') < 0 {
		redact.Tick(len(s))
		return string(s), true
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		redact.Tick(1)
		c := s[i]
		if c != '\\' {
			b.WriteByte(c)
			continue
		}
		if i++; i >= len(s) {
			return "", false
		}
		switch s[i] {
		case '"', '\\', '/':
			b.WriteByte(s[i])
		case 'b':
			b.WriteByte('\b')
		case 'f':
			b.WriteByte('\f')
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		case 'u':
			r, ok := hex4(s, i+1)
			if !ok {
				return "", false
			}
			i += 4
			if utf16.IsSurrogate(r) {
				r2, ok := hex4(s, i+3)
				if ok && s[i+1] == '\\' && s[i+2] == 'u' {
					if d := utf16.DecodeRune(r, r2); d != utf8.RuneError {
						b.WriteRune(d)
						i += 6
						continue
					}
				}
				r = utf8.RuneError
			}
			b.WriteRune(r)
		default:
			return "", false
		}
	}
	return b.String(), true
}

// hex4 reads the four hex digits of a "\u" escape at s[i:].
func hex4(s []byte, i int) (rune, bool) {
	if i < 0 || i+4 > len(s) {
		return 0, false
	}
	var r rune
	for _, c := range s[i : i+4] {
		var d byte
		switch {
		case '0' <= c && c <= '9':
			d = c - '0'
		case 'a' <= c && c <= 'f':
			d = c - 'a' + 10
		case 'A' <= c && c <= 'F':
			d = c - 'A' + 10
		default:
			return 0, false
		}
		r = r<<4 | rune(d)
	}
	return r, true
}

// marshaler is the interface of a type that writes its own JSON.
var marshaler = reflect.TypeFor[json.Marshaler]()

// writesItself reports whether values of t write their own JSON (json.Marshaler, on the value or its pointer):
// the tokens inside are read without the object.
func writesItself(t reflect.Type) bool {
	return t.Implements(marshaler) || reflect.PointerTo(t).Implements(marshaler)
}

// structFields caches, by struct type, the index of each field by its JSON name (fieldIndexesOf).
var structFields sync.Map

// fieldIndexes returns the index of each field of the struct t by the name encoding/json writes it under.
func fieldIndexes(t reflect.Type) map[string][]int {
	m, ok := structFields.Load(t)
	if !ok {
		m, _ = structFields.LoadOrStore(t, fieldIndexesOf(t, nil, nil))
	}
	return m.(map[string][]int)
}

// fieldIndexesOf returns the index of each field of the struct t by its JSON name, each index after the prefix at,
// the fields of an embedded struct without a name inlined; a field of t hides an inlined one of the same name.
// seen are the structs being inlined around t, which a struct that embeds itself does not inline again.
func fieldIndexesOf(t reflect.Type, at []int, seen []reflect.Type) map[string][]int {
	out := map[string][]int{}
	seen = append(seen, t)
	type embedded struct {
		t   reflect.Type
		idx []int
	}
	var inlined []embedded
	for i := range t.NumField() {
		sf := t.Field(i)
		idx := append(slices.Clone(at), i)
		name, inline, skip := jsonName(sf)
		switch {
		case skip:
		case inline:
			e := sf.Type
			for e.Kind() == reflect.Pointer {
				e = e.Elem()
			}
			if e.Kind() == reflect.Struct && !slices.Contains(seen, e) {
				inlined = append(inlined, embedded{e, idx})
			}
		case sf.IsExported():
			out[name] = idx
		}
	}
	for _, e := range inlined {
		for name, idx := range fieldIndexesOf(e.t, e.idx, seen) {
			if _, ok := out[name]; !ok {
				out[name] = idx
			}
		}
	}
	return out
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
		redact.Tick(1)
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
		redact.Tick(1)
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
			redact.Tick(1)
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
