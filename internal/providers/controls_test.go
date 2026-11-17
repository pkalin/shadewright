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

package providers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"reflect"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// The corpus of these tests is generated, and every expected answer is known by construction: an input is a
// list of segments, each either kept text or a control character, and the expected output is the kept
// segments joined. Nothing here shares a list with the implementation: the code points are the ones the
// Unicode standard puts in category Cc, written out from the standard's ranges and checked against package
// unicode.

// controlCodePoints are every code point of Unicode category Cc: C0 (U+0000 to U+001F), DEL (U+007F) and C1
// (U+0080 to U+009F).
func controlCodePoints() []rune {
	var cc []rune
	for r := rune(0); r <= 0x1f; r++ {
		cc = append(cc, r)
	}
	cc = append(cc, 0x7f)
	for r := rune(0x80); r <= 0x9f; r++ {
		cc = append(cc, r)
	}
	return cc
}

// removedCodePoints are the control code points storage removes: every one but newline and tab.
func removedCodePoints() []rune {
	var out []rune
	for _, r := range controlCodePoints() {
		if r != '\n' && r != '\t' {
			out = append(out, r)
		}
	}
	return out
}

// The written-out ranges are exactly category Cc, over the whole code space.
func TestControlCodePointsAreCategoryCc(t *testing.T) {
	want := map[rune]bool{}
	for _, r := range controlCodePoints() {
		want[r] = true
	}
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if unicode.Is(unicode.Cc, r) != want[r] {
			t.Fatalf("U+%04X: in Cc %v, in the written-out ranges %v", r, unicode.Is(unicode.Cc, r), want[r])
		}
	}
	if len(want) != 65 {
		t.Fatalf("%d control code points, want 65", len(want))
	}
}

// keptRune draws a code point that storage keeps: printable ASCII, newline, tab, and any other assigned or
// unassigned code point outside Cc and the surrogates, with the neighbours of the control ranges (U+00A0,
// U+00FF, U+0100) and format characters drawn often.
func keptRune(rng *rand.Rand) rune {
	switch rng.IntN(6) {
	case 0:
		return 0x20 + rng.Int32N(0x7f-0x20)
	case 1:
		return []rune{'\n', '\t', '\\', '"', '[', ']', ';', 'u', '0', '1', 'b'}[rng.IntN(11)]
	case 2:
		return 0xa0 + rng.Int32N(0x60) // Latin-1 after C1
	case 3:
		return []rune{0x200b, 0x200d, 0x2028, 0x2029, 0xfeff, 0xfffd, 0x034f, 0xe0001, 0x1f600}[rng.IntN(9)]
	default:
		for {
			r := rng.Int32N(unicode.MaxRune + 1)
			if !unicode.Is(unicode.Cc, r) && utf8.ValidRune(r) {
				return r
			}
		}
	}
}

func keptText(rng *rand.Rand, maxRunes int) string {
	var b strings.Builder
	for range rng.IntN(maxRunes + 1) {
		b.WriteRune(keptRune(rng))
	}
	return b.String()
}

// segment is one part of a generated input: kept text, or one control code point.
type segment struct {
	text    string
	control rune
	isCtrl  bool
}

// genSegments builds an input of n segments, about a third of them removed control code points.
func genSegments(rng *rand.Rand, n int) []segment {
	removed := removedCodePoints()
	segs := make([]segment, 0, n)
	for range n {
		if rng.IntN(3) == 0 {
			segs = append(segs, segment{control: removed[rng.IntN(len(removed))], isCtrl: true})
			continue
		}
		segs = append(segs, segment{text: keptText(rng, 6)})
	}
	return segs
}

// join returns the input of segs and the output expected of it.
func join(segs []segment) (in, want string) {
	var i, w strings.Builder
	for _, s := range segs {
		if s.isCtrl {
			i.WriteRune(s.control)
			continue
		}
		i.WriteString(s.text)
		w.WriteString(s.text)
	}
	return i.String(), w.String()
}

// hasRemovedControl reports whether s holds a code point storage removes, or a byte that is not UTF-8.
func hasRemovedControl(s string) bool {
	if !utf8.ValidString(s) {
		return true
	}
	return strings.ContainsFunc(s, func(r rune) bool { return unicode.Is(unicode.Cc, r) && r != '\n' && r != '\t' })
}

// Every removed code point, in every position of a string (alone, first, between two characters, last,
// repeated, between every character of a word), is removed and nothing else is.
func TestStripControlsEveryCodePointEveryPosition(t *testing.T) {
	const word = "k3y-W0rd"
	for _, c := range removedCodePoints() {
		cs := string(c)
		cases := map[string]string{
			cs:                       "",
			cs + word:                word,
			word[:3] + cs + word[3:]: word,
			word + cs:                word,
			cs + cs + word + cs + cs: word,
			strings.Join(strings.Split(word, ""), cs): word,
			"\n" + cs + "\t": "\n\t",
		}
		for in, want := range cases {
			if got := StripControls(in); got != want {
				t.Errorf("U+%04X: StripControls(%q) = %q, want %q", c, in, got, want)
			}
		}
	}
	for _, keep := range []string{"\n", "\t", "a\nb\tc", "\n\n\t\t"} {
		if got := StripControls(keep); got != keep {
			t.Errorf("StripControls(%q) = %q, newline and tab stay", keep, got)
		}
	}
}

// Escape sequences and overwrites: the control code points go and their remains stay as visible text, so the
// letters a terminal would hide are shown.
func TestStripControlsEscapeSequencesLeaveVisibleRemains(t *testing.T) {
	const esc, bel, bs, cr = "\x1b", "\a", "\b", "\r"
	const csi8, osc8, dcs8, st8 = "\u009b", "\u009d", "\u0090", "\u009c"
	for _, tc := range []struct{ in, want string }{
		{"key" + esc + "[31mHIDDEN" + esc + "[0m", "key[31mHIDDEN[0m"},
		{"key" + csi8 + "8mHIDDEN" + csi8 + "0m", "key8mHIDDEN0m"},
		{"key" + esc + "[2K" + esc + "[1Gfake", "key[2K[1Gfake"},
		{esc + "]0;title" + bel + "text", "]0;titletext"},
		{esc + "]8;;http://x" + esc + "\\link" + esc + "]8;;" + esc + "\\", `]8;;http://x\link]8;;\`},
		{osc8 + "0;title" + st8 + "text", "0;titletext"},
		{esc + "Pq#0;2;0;0;0" + esc + "\\after", "Pq#0;2;0;0;0\\after"},
		{dcs8 + "payload" + st8 + "after", "payloadafter"},
		{"secret" + bs + bs + bs + bs + bs + bs + "xxxxxx", "secretxxxxxx"},
		{"secret" + cr + "xxxxxx", "secretxxxxxx"},
		{"sec" + esc + "7ret" + esc + "8XX", "sec7ret8XX"},
		{"a" + esc + "[1;31;42;5;7;8mb", "a[1;31;42;5;7;8mb"},
	} {
		if got := StripControls(tc.in); got != tc.want {
			t.Errorf("StripControls(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	// Every control code point as the introducer, inside and as the terminator of a sequence.
	for _, c := range removedCodePoints() {
		cs := string(c)
		for _, tc := range []struct{ in, want string }{
			{cs + "[31m" + cs + "x", "[31mx"},
			{esc + "]0;" + cs + "t" + bel, "]0;t"},
			{esc + "P" + cs + "q" + esc + "\\", "Pq\\"},
		} {
			if got := StripControls(tc.in); got != tc.want {
				t.Errorf("U+%04X: StripControls(%q) = %q, want %q", c, tc.in, got, tc.want)
			}
		}
	}
}

// Generated inputs: the output is the kept segments joined, and text without removed code points comes back
// byte for byte.
func TestStripControlsGeneratedCorpus(t *testing.T) {
	rng := rand.New(rand.NewPCG(852, 1)) // #nosec G404 -- a seeded corpus, not a secret
	for i := range 20000 {
		in, want := join(genSegments(rng, 1+rng.IntN(12)))
		if got := StripControls(in); got != want {
			t.Fatalf("input %d: StripControls(%q) = %q, want %q", i, in, got, want)
		}
		if got := StripControls(want); got != want {
			t.Fatalf("input %d: StripControls changed text without controls: %q to %q", i, want, got)
		}
	}
}

// Bytes that are not UTF-8 never pass: each becomes U+FFFD, as encoding/json and the Kubernetes API do with
// them, so a lone C1 byte (0x80 to 0x9F, which an 8-bit terminal reads as a control) is never stored. The
// invalid bytes are lone continuation bytes and bytes that never start a sequence, so each is invalid wherever
// it stands and the expected output is known.
func TestStripControlsInvalidUTF8(t *testing.T) {
	var invalid []byte
	for b := 0x80; b <= 0xbf; b++ {
		invalid = append(invalid, byte(b))
	}
	invalid = append(invalid, 0xc0, 0xc1)
	for b := 0xf5; b <= 0xff; b++ {
		invalid = append(invalid, byte(b))
	}
	rng := rand.New(rand.NewPCG(852, 2)) // #nosec G404 -- a seeded corpus, not a secret
	for i := range 20000 {
		var in, want strings.Builder
		for _, s := range genSegments(rng, 1+rng.IntN(10)) {
			if rng.IntN(4) == 0 {
				in.WriteByte(invalid[rng.IntN(len(invalid))])
				want.WriteRune(utf8.RuneError)
			}
			if s.isCtrl {
				in.WriteRune(s.control)
				continue
			}
			in.WriteString(s.text)
			want.WriteString(s.text)
		}
		got := StripControls(in.String())
		if got != want.String() {
			t.Fatalf("input %d: StripControls(%q) = %q, want %q", i, in.String(), got, want.String())
		}
		if hasRemovedControl(got) {
			t.Fatalf("input %d: %q holds a control or a byte that is not UTF-8", i, got)
		}
	}
}

// jsonValue is a generated JSON value and the value expected once its controls are removed.
type jsonValue struct {
	text string
	want any
}

// encodeKept writes s as the inside of a JSON string literal, choosing per rune between the raw form and an
// escape where JSON allows both, so the corpus holds every way a kept character can be written.
func encodeKept(rng *rand.Rand, b *strings.Builder, s string) {
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			if rng.IntN(2) == 0 {
				fmt.Fprintf(b, `\u%04x`, r)
			} else {
				b.WriteByte('\\')
				b.WriteRune(r)
			}
		case r == '\n':
			b.WriteString([]string{`\n`, `\u000a`, `\u000A`}[rng.IntN(3)])
		case r == '\t':
			b.WriteString([]string{`\t`, `\u0009`}[rng.IntN(2)])
		case r == '/' && rng.IntN(2) == 0:
			b.WriteString(`\/`)
		case r > 0xffff && rng.IntN(2) == 0:
			hi, lo := utf16Pair(r)
			fmt.Fprintf(b, `\u%04x\u%04X`, hi, lo)
		case r <= 0xffff && rng.IntN(4) == 0:
			fmt.Fprintf(b, `\u%04x`, r)
		default:
			b.WriteRune(r)
		}
	}
}

func utf16Pair(r rune) (rune, rune) {
	r -= 0x10000
	return 0xd800 + (r>>10)&0x3ff, 0xdc00 + r&0x3ff
}

// encodeControl writes a removed control code point inside a JSON string literal: raw (which JSON allows
// for DEL and C1, and refuses for C0), as a \u escape in either case, or as its short escape.
func encodeControl(rng *rand.Rand, b *strings.Builder, c rune) {
	short := map[rune]string{'\b': `\b`, '\f': `\f`, '\r': `\r`}
	switch rng.IntN(4) {
	case 0:
		b.WriteRune(c)
	case 1:
		fmt.Fprintf(b, `\u%04x`, c)
	case 2:
		fmt.Fprintf(b, `\u%04X`, c)
	default:
		if s, ok := short[c]; ok {
			b.WriteString(s)
		} else {
			fmt.Fprintf(b, `\u%04x`, c)
		}
	}
}

// genString returns a JSON string literal of generated segments, with or without controls, and its expected
// decoded value.
func genString(rng *rand.Rand, controls bool) jsonValue {
	var b, want strings.Builder
	b.WriteByte('"')
	for _, s := range genSegments(rng, rng.IntN(8)) {
		if s.isCtrl {
			if controls {
				encodeControl(rng, &b, s.control)
			}
			continue
		}
		encodeKept(rng, &b, s.text)
		want.WriteString(s.text)
	}
	b.WriteByte('"')
	return jsonValue{text: b.String(), want: want.String()}
}

// genJSON returns a generated JSON value of at most depth levels, with JSON whitespace between its tokens.
func genJSON(rng *rand.Rand, depth int, controls bool) jsonValue {
	ws := func() string { return []string{"", " ", "\n", "\t", " \n "}[rng.IntN(5)] }
	k := rng.IntN(7)
	if depth == 0 {
		k = rng.IntN(4)
	}
	switch k {
	case 0, 1:
		return genString(rng, controls)
	case 2:
		n := rng.IntN(2000) - 1000
		return jsonValue{text: fmt.Sprint(n), want: json.Number(fmt.Sprint(n))}
	case 3:
		return []jsonValue{{"true", true}, {"false", false}, {"null", nil}, {"-1.5e3", json.Number("-1.5e3")}}[rng.IntN(4)]
	case 4, 5:
		obj := map[string]any{}
		var b strings.Builder
		b.WriteString("{" + ws())
		n := 0
		for range rng.IntN(5) {
			key := genString(rng, controls)
			if _, dup := obj[key.want.(string)]; dup {
				continue
			}
			v := genJSON(rng, depth-1, controls)
			if n > 0 {
				b.WriteString("," + ws())
			}
			b.WriteString(key.text + ws() + ":" + ws() + v.text + ws())
			obj[key.want.(string)] = v.want
			n++
		}
		b.WriteString("}")
		return jsonValue{text: b.String(), want: obj}
	default:
		arr := []any{}
		var b strings.Builder
		b.WriteString("[" + ws())
		for i := range rng.IntN(5) {
			v := genJSON(rng, depth-1, controls)
			if i > 0 {
				b.WriteString("," + ws())
			}
			b.WriteString(v.text + ws())
			arr = append(arr, v.want)
		}
		b.WriteString("]")
		return jsonValue{text: b.String(), want: arr}
	}
}

func decodeJSON(t *testing.T, data []byte) any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("the output does not parse: %v\n%s", err, data)
	}
	return v
}

// The decoded string values of a JSON document lose their controls in every form JSON can write them (raw,
// \u escapes in both cases, \b \f \r), the document still parses to the expected value, and a document whose
// strings hold no controls comes back byte for byte. Keys are strings too. The kept text holds backslashes,
// quotes and the letters of escapes ("\\u001b" written as text), which must survive as text.
func TestStripControlsJSONGeneratedCorpus(t *testing.T) {
	rng := rand.New(rand.NewPCG(852, 3)) // #nosec G404 -- a seeded corpus, not a secret
	for i := range 5000 {
		state := *rng
		with := genJSON(rng, 3, true)
		got := StripControlsJSON([]byte(with.text))
		if !reflect.DeepEqual(decodeJSON(t, got), with.want) {
			t.Fatalf("document %d: %s\nbecame %s\nwant the value %#v", i, with.text, got, with.want)
		}
		if bytes.ContainsFunc(got, func(r rune) bool { return unicode.Is(unicode.Cc, r) && !unicode.IsSpace(r) }) {
			t.Fatalf("document %d: the output holds a raw control: %q", i, got)
		}
		// The same document without its controls, generated from the same state, is unchanged.
		*rng = state
		without := genJSON(rng, 3, false)
		if got := StripControlsJSON([]byte(without.text)); string(got) != without.text {
			t.Fatalf("document %d: a document without controls changed:\n%s\nbecame %s", i, without.text, got)
		}
	}
}

// Neighbours an escape-aware rule could get wrong.
func TestStripControlsJSONNeighbours(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		// An escaped backslash followed by the letters of an escape is text, not a control.
		{`{"a":"\\u001b[31m"}`, `\u001b[31m`},
		{`{"a":"\\\u001b[31m"}`, `\[31m`},
		{`{"a":"\\r\\n"}`, `\r\n`},
		// A control between the halves of a surrogate pair: the halves stay unpaired, as encoding/json reads
		// them without the control, so removal never joins two characters into a new one.
		{`{"a":"\ud83d\u001b\ude00"}`, "��"},
		// A raw control next to an escaped one, in a literal JSON refuses until the raw one is gone.
		{"{\"a\":\"x\x1b\\u001by\"}", "xy"},
		// A key, and a value that holds a quote right after a removed control.
		{`{"k\u0007ey":"\u0007\"q\""}`, `"q"`},
		// Short escapes of kept characters stay.
		{`{"a":"\n\t\/\""}`, "\n\t/\""},
	} {
		got := StripControlsJSON([]byte(tc.in))
		var v map[string]string
		if err := json.Unmarshal(got, &v); err != nil {
			t.Fatalf("StripControlsJSON(%s) = %s, which does not parse: %v", tc.in, got, err)
		}
		for _, s := range v {
			if s != tc.want {
				t.Errorf("StripControlsJSON(%s) = %s, value %q, want %q", tc.in, got, s, tc.want)
			}
		}
		for k := range v {
			if hasRemovedControl(k) {
				t.Errorf("StripControlsJSON(%s): key %q holds a control", tc.in, k)
			}
		}
	}
}

// Text that is not JSON, or JSON that is broken, is never refused or repaired into another value: whatever the
// input, the output holds no raw control and no byte that is not UTF-8, and when it parses, no decoded string
// holds a control. Random bytes drawn mostly from the characters of JSON and of escapes.
func TestStripControlsJSONNeverLetsAControlThrough(t *testing.T) {
	alphabet := []byte("{}[]:,\"\\u0019bBfFrRnt\x1b\x00\x07\x7f\xc2\x9b\x80\xff\n\t aZ1")
	rng := rand.New(rand.NewPCG(852, 4)) // #nosec G404 -- a seeded corpus, not a secret
	for i := range 50000 {
		in := make([]byte, rng.IntN(40))
		for j := range in {
			in[j] = alphabet[rng.IntN(len(alphabet))]
		}
		got := StripControlsJSON(in)
		if !utf8.Valid(got) || bytes.ContainsFunc(got, func(r rune) bool {
			return unicode.Is(unicode.Cc, r) && r != '\n' && r != '\t'
		}) {
			t.Fatalf("input %d %q: output %q holds a control or a byte that is not UTF-8", i, in, got)
		}
		var v any
		if json.Unmarshal(got, &v) == nil && anyString(v, hasRemovedControl) {
			t.Fatalf("input %d %q: output %q decodes to a string with a control", i, in, got)
		}
	}
}

// anyString reports whether f holds for a key or a string value of v.
func anyString(v any, f func(string) bool) bool {
	switch x := v.(type) {
	case string:
		return f(x)
	case []any:
		for _, e := range x {
			if anyString(e, f) {
				return true
			}
		}
	case map[string]any:
		for k, e := range x {
			if f(k) || anyString(e, f) {
				return true
			}
		}
	}
	return false
}

// completionFields lists every field of a Completion that can hold text, as a path, and says how
// WithoutControls treats it. A field added to Completion or ToolCall that can hold a string, bytes or anything
// that may contain them fails TestWithoutControlsCoversEveryField until it is listed here.
const (
	kindJSON = "json"
	kindText = "text"
)

var completionFields = map[string]string{
	"Content":             kindJSON,
	"ToolCalls.ID":        kindText,
	"ToolCalls.Name":      kindText,
	"ToolCalls.Arguments": kindJSON,
	// Model is not cleaned: each adapter accepts only a name matching a fixed ASCII pattern (validModel),
	// and the loop compares it with the configured name. Removing characters could turn a refused name
	// into the configured one.
	"Model": "exempt",
	// ProviderBlocks is opaque: the loop never reads, logs or stores it, and hands it back unchanged to the
	// adapter that made it (the thinking blocks the Anthropic API signs).
	"ProviderBlocks": "exempt",
}

// textPaths walks t and returns the path of every field that is not a number or a bool.
func textPaths(t reflect.Type, prefix string) []string {
	var out []string
	for f := range t.Fields() {
		p := prefix + f.Name
		ft := f.Type
		if ft.Kind() == reflect.Slice && ft.Elem().Kind() == reflect.Struct {
			ft = ft.Elem()
		}
		switch {
		case ft == reflect.TypeFor[Opaque]():
			out = append(out, p)
		case ft.Kind() == reflect.Struct:
			out = append(out, textPaths(ft, p+".")...)
		case ft.Kind() == reflect.Bool, ft.Kind() >= reflect.Int && ft.Kind() <= reflect.Float64:
		default:
			out = append(out, p)
		}
	}
	return out
}

// Every field of a Completion that can hold text is cleaned or listed as exempt with its reason, and a field
// listed as cleaned is: filled with every removed code point, raw and escaped, it comes back without them,
// and its JSON still parses.
func TestWithoutControlsCoversEveryField(t *testing.T) {
	paths := textPaths(reflect.TypeFor[Completion](), "")
	seen := map[string]bool{}
	for _, p := range paths {
		seen[p] = true
		if _, ok := completionFields[p]; !ok {
			t.Errorf("field %s of Completion can hold text and is neither cleaned nor exempt", p)
		}
	}
	for p := range completionFields {
		if !seen[p] {
			t.Errorf("completionFields lists %s, which Completion does not have", p)
		}
	}
	var raw, esc strings.Builder
	for _, c := range removedCodePoints() {
		raw.WriteString("x" + string(c))
		fmt.Fprintf(&esc, `y\u%04x`, c)
	}
	text := raw.String()
	doc := `{"k` + esc.String() + `":["` + esc.String() + `", "` + strings.ReplaceAll(text, "\x00", "") + `"]}`
	in := Completion{Content: doc, Model: "m", ToolCalls: []ToolCall{
		{ID: text, Name: text, Arguments: json.RawMessage(doc)},
		{ID: text, Name: text, Arguments: json.RawMessage(doc)},
	}}
	got := WithoutControls(in)
	check := func(path, kind string, v []byte) {
		switch kind {
		case kindText:
			if hasRemovedControl(string(v)) || !strings.Contains(string(v), "xxxx") {
				t.Errorf("%s = %q, want the text without its controls", path, v)
			}
		case kindJSON:
			var d any
			if err := json.Unmarshal(v, &d); err != nil {
				t.Errorf("%s = %s does not parse: %v", path, v, err)
			} else if anyString(d, hasRemovedControl) {
				t.Errorf("%s = %s still decodes to a control", path, v)
			}
		}
	}
	check("Content", completionFields["Content"], []byte(got.Content))
	for _, c := range got.ToolCalls {
		check("ToolCalls.ID", completionFields["ToolCalls.ID"], []byte(c.ID))
		check("ToolCalls.Name", completionFields["ToolCalls.Name"], []byte(c.Name))
		check("ToolCalls.Arguments", completionFields["ToolCalls.Arguments"], c.Arguments)
	}
	// The input is not changed in place: an adapter's slices stay as they were.
	if in.ToolCalls[0].Name != text || string(in.ToolCalls[0].Arguments) != doc {
		t.Error("WithoutControls changed its input")
	}
}

// A Completion without controls comes back equal, its slices shared, and Model and ProviderBlocks are never
// touched.
func TestWithoutControlsLeavesCleanRepliesAlone(t *testing.T) {
	in := Completion{Content: `{"summary":"ok\n"}`, Model: "m-1", Usage: Usage{InputTokens: 3},
		ToolCalls:      []ToolCall{{ID: "c1", Name: "list_events", Arguments: json.RawMessage(`{"namespace":"shop"}`)}},
		ProviderBlocks: NewOpaque([]byte("\x1b[31m")), UsageUnknown: true}
	got := WithoutControls(in)
	if !reflect.DeepEqual(got, in) || &got.ToolCalls[0] != &in.ToolCalls[0] {
		t.Errorf("WithoutControls(%+v) = %+v, want it unchanged", in, got)
	}
	in.Model = "m\x1b"
	if got := WithoutControls(in); got.Model != in.Model || string(got.ProviderBlocks.Bytes()) != "\x1b[31m" {
		t.Errorf("Model %q and ProviderBlocks %q changed", got.Model, got.ProviderBlocks.Bytes())
	}
}

// FuzzStripControlsJSON: whatever the input, the output is UTF-8 without a removed control, raw or decoded,
// a second pass changes nothing, and an input without a backslash or a control comes back unchanged.
func FuzzStripControlsJSON(f *testing.F) {
	for _, s := range []string{`{"a":"\u001b[31m"}`, "{\"a\":\"x\x1b\\u001by\"}", `{"a":"\ud83d\u001b\ude00"}`,
		`["\\u0007", "\b\f\r"]`, "\xc2\x9b\x80\xff", `{"k\u0000":1,"k":2}`} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, in []byte) {
		got := StripControlsJSON(in)
		if !utf8.Valid(got) || hasRemovedControl(string(got)) {
			t.Fatalf("%q: output %q holds a control or a byte that is not UTF-8", in, got)
		}
		var v any
		if json.Unmarshal(got, &v) == nil && anyString(v, hasRemovedControl) {
			t.Fatalf("%q: output %q decodes to a string with a control", in, got)
		}
		if again := StripControlsJSON(got); !bytes.Equal(again, got) {
			t.Fatalf("%q: a second pass changed %q to %q", in, got, again)
		}
		if utf8.Valid(in) && !bytes.ContainsRune(in, '\\') && !hasRemovedControl(string(in)) && !bytes.Equal(got, in) {
			t.Fatalf("%q: changed without a control to %q", in, got)
		}
	})
}
