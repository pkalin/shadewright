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

package openaicompatible

import (
	"bytes"
	"encoding/json"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/providers/model/openaiwire"
	"github.com/pkalin/shadewright/internal/termtext"
)

// displayInserts are put inside the key: each terminal escape sequence form the kubectl plugin's cleaning
// skips whole (7-bit and 8-bit CSI, OSC, DCS, SOS, PM, APC, two-character ESC sequences, cut or
// unterminated ones), the runes it drops, line and paragraph separators, and invalid UTF-8.
var displayInserts = append([]string{
	"\x1b[0m", "\x1b[38;5;196m", "\x1b[?25l", "\x1b[", "\u009b0m", "\u009b",
	"\x1b]0;t\x07", "\x1b]8;;https://e.test/\x1b\\", "\x1b]0;t", "\u009d0;t\u009c", "\u009d0;t\x07",
	"\x1bPq#0\x1b\\", "\u0090q\u009c", "\x1bXs\x1b\\", "\u0098s\u009c", "\x1b^p\x07", "\u009ep\u009c",
	"\x1b_a\x1b\\", "\u009fa\u009c", "\x1b(B", "\x1b=", "\x1b7", "\x1b", "\x1b\x1b[0m",
	"\u2028", "\u2029", "\t", "\r\n", "\xff", "\xc3", "\xed\xa0\x80",
}, invisibleRunes...)

// displayShape places the key with its insertions (m) in one slot, split at byte c where it is split:
// the message text, an argument value, an argument object key, two text parts (Responses) or the text and
// an argument value (Chat Completions), and the text and an argument object key.
type displayShape struct {
	name  string
	parts func(api API, m string, c int) (texts []string, args string)
}

var displayShapes = []displayShape{
	{"content", func(_ API, m string, _ int) ([]string, string) {
		return []string{"before " + m + " after"}, decoyPlainArgs
	}},
	{"argument", func(_ API, m string, _ int) ([]string, string) {
		return []string{"x"}, jsonText(map[string]string{"s": m})
	}},
	{"argument-key", func(_ API, m string, _ int) ([]string, string) { return []string{"x"}, jsonText(map[string]int{m: 1}) }},
	{"split", func(api API, m string, c int) ([]string, string) {
		if api == APIResponses {
			return []string{m[:c], m[c:]}, decoyPlainArgs
		}
		return []string{m[:c]}, jsonText(map[string]string{"s": m[c:]})
	}},
	{"split-argument-key", func(_ API, m string, c int) ([]string, string) {
		return []string{m[:c]}, jsonText(map[string]int{m[c:]: 1})
	}},
}

// returnedStrings lists what the caller's parser returns from a reply: the content, the raw arguments,
// every string and object key inside them, and the content joined with those in wire order.
func returnedStrings(c providers.Completion) []string {
	out := []string{c.Content}
	joined := c.Content
	for _, call := range c.ToolCalls {
		out = append(out, string(call.Arguments))
		dec := json.NewDecoder(bytes.NewReader(call.Arguments))
		for {
			tok, err := dec.Token()
			if err != nil {
				break
			}
			if s, ok := tok.(string); ok {
				out = append(out, s)
				joined += s
			}
		}
	}
	return append(out, joined)
}

// shown reports whether the plugin's terminal cleaning of s shows the key, ignoring whitespace.
func shown(s, key string) bool { return strings.Contains(termtext.Compact(s), key) }

// insertAt puts each insert into key at its cut (cuts ascending, all inside the key).
func insertAt(key string, cuts []int, inserts []string) string {
	var b strings.Builder
	last := 0
	for i, c := range cuts {
		b.WriteString(key[last:c])
		b.WriteString(inserts[i])
		last = c
	}
	b.WriteString(key[last:])
	return b.String()
}

// Property: for generated replies of both APIs with the key whole or split across parts, argument values
// and argument object keys, and with generated insertions inside it, whenever the plugin's display of any
// string the parser returns (or of the raw bytes of a reply that is not JSON) shows the key, the scan
// refuses the reply.
func TestDisplayedKeyCorpus(t *testing.T) {
	r := rand.New(rand.NewPCG(834, 1117)) // #nosec G404 -- reproducible adversarial corpus
	type variant struct {
		cuts    []int
		inserts []string
	}
	var variants []variant
	for _, ins := range displayInserts {
		for cut := 1; cut < len(secretKey); cut++ {
			variants = append(variants, variant{[]int{cut}, []string{ins}})
		}
	}
	for range 400 {
		n := 2 + r.IntN(3)
		cuts := make([]int, n)
		inserts := make([]string, n)
		for i := range n {
			cuts[i] = 1 + r.IntN(len(secretKey)-1)
			inserts[i] = displayInserts[r.IntN(len(displayInserts))]
		}
		slices.Sort(cuts)
		variants = append(variants, variant{cuts, inserts})
	}

	cases, shownCases, hiddenCases := 0, 0, 0
	for _, api := range []API{APIChatCompletions, APIResponses} {
		for vi, v := range variants {
			m := insertAt(secretKey, v.cuts, v.inserts)
			// The raw bytes of a reply that is not JSON.
			raw := []byte(`{"output":"` + m)
			cases++
			if shown(string(raw), secretKey) {
				shownCases++
				if !replyContainsKey(raw, api, secretKey) {
					t.Fatalf("%s: variant %d (%q) in a raw reply is shown but passed", api, vi, m)
				}
			} else {
				hiddenCases++
			}
			for _, shape := range displayShapes {
				c := 1 + r.IntN(len(m)-1)
				texts, args := shape.parts(api, m, c)
				data, err := json.Marshal(decoyReply(api, texts, args))
				if err != nil {
					t.Fatal(err)
				}
				completion, err := parseCompletion(api, data)
				if err != nil {
					continue // the caller refuses the reply anyway
				}
				cases++
				show := false
				for _, s := range returnedStrings(completion) {
					show = show || shown(s, secretKey)
				}
				if !show {
					hiddenCases++
					continue
				}
				shownCases++
				if !replyContainsKey(data, api, secretKey) {
					t.Fatalf("%s: %s, variant %d (%q) split at %d is shown but passed", api, shape.name, vi, m, c)
				}
			}
		}
	}
	// The corpus holds both outcomes, so the property is not true by construction alone.
	if shownCases < 1000 || hiddenCases < 100 {
		t.Fatalf("corpus too narrow: %d cases, %d shown, %d hidden", cases, shownCases, hiddenCases)
	}
	t.Logf("%d cases, %d show the key, %d do not", cases, shownCases, hiddenCases)
}

// The reported case: an SGR reset inside the key passed the scan, and the plugin printed the whole key.
func TestEscapeSequenceInsideKeyIsRefused(t *testing.T) {
	m := secretKey[:3] + "\x1b[0m" + secretKey[3:]
	for _, api := range []API{APIChatCompletions, APIResponses} {
		data, err := json.Marshal(decoyReply(api, []string{"before " + m + " after"}, decoyPlainArgs))
		if err != nil {
			t.Fatal(err)
		}
		if !replyContainsKey(data, api, secretKey) {
			t.Fatalf("%s: key with an escape sequence inside passed", api)
		}
		if !replyContainsKey([]byte(`{"output":"`+m), api, secretKey) {
			t.Fatalf("%s: raw key with an escape sequence inside passed", api)
		}
		// The same sequence next to the key, not inside it, changes nothing for a visible break.
		broken := secretKey[:3] + "\x1b[0m-" + secretKey[3:]
		data, err = json.Marshal(decoyReply(api, []string{broken}, decoyPlainArgs))
		if err != nil {
			t.Fatal(err)
		}
		if replyContainsKey(data, api, secretKey) {
			t.Fatalf("%s: key broken by a visible character matched", api)
		}
	}
}

func parseCompletion(api API, data []byte) (providers.Completion, error) {
	offered := map[string]bool{decoyTool: true}
	if api == APIResponses {
		return openaiwire.ParseResponses(data, offered, false)
	}
	return openaiwire.ParseResponse(data, offered, false)
}
