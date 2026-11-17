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
	"unicode"

	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/providers/model/openaiwire"
)

// controlInserts are put inside the key: every Unicode Cc code point alone (C0, DEL, C1), and terminal
// sequences built from them (7-bit and 8-bit CSI, OSC and DCS, with and without visible parameters, cut
// short or unterminated). Storage removes the Cc code points except newline and tab (#852) and keeps the
// rest of a sequence as visible text.
var controlInserts = func() []string {
	var out []string
	for r := rune(0); r <= 0x9f; r++ {
		if unicode.Is(unicode.Cc, r) {
			out = append(out, string(r))
		}
	}
	return append(out,
		"\x1b[0m", "\x1b[38;5;196m", "\x1b[?25l", "\x1b[", "\u009b0m", "\u009bm", "\u009b",
		"\x1b]0;t\x07", "\x1b]8;;https://e.test/\x1b\\", "\x1b]0;t", "\x1b]\x07", "\u009d0;t\u009c", "\u009d\u009c",
		"\x1bPq#0\x1b\\", "\x1bP\x1b\\", "\u0090q\u009c", "\u0090\u009c", "\x1b\x07\u009c", "\r\x08\x7f",
	)
}()

// keyShape places the key with its insertions (m) in one slot, split at byte c where it is split:
// the message text, an argument value, an argument object key, two text parts (Responses) or the text and
// an argument value (Chat Completions), and the text and an argument object key.
type keyShape struct {
	name  string
	parts func(api API, m string, c int) (texts []string, args string)
}

var keyShapes = []keyShape{
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

// stored returns s as it is stored: without the Cc code points other than newline and tab (#852). It is
// written from that rule, not from the scan.
func stored(s string) string {
	return strings.Map(func(r rune) rune {
		if r != '\n' && r != '\t' && unicode.Is(unicode.Cc, r) {
			return -1
		}
		return r
	}, s)
}

// keyStored reports whether the key is contiguous in the stored form of s.
func keyStored(s, key string) bool { return strings.Contains(stored(s), key) }

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
// and argument object keys, and with Cc code points or sequences of them inserted inside it, whenever the
// key is contiguous in the stored form of any string the parser returns (or of the raw bytes of a reply
// that is not JSON), the scan refuses the reply.
func TestStoredKeyCorpus(t *testing.T) {
	r := rand.New(rand.NewPCG(834, 1117)) // #nosec G404 -- reproducible adversarial corpus
	type variant struct {
		cuts    []int
		inserts []string
	}
	var variants []variant
	for _, ins := range controlInserts {
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
			inserts[i] = controlInserts[r.IntN(len(controlInserts))]
		}
		slices.Sort(cuts)
		variants = append(variants, variant{cuts, inserts})
	}

	cases, storedCases, brokenCases := 0, 0, 0
	for _, api := range []API{APIChatCompletions, APIResponses} {
		for vi, v := range variants {
			m := insertAt(secretKey, v.cuts, v.inserts)
			// The raw bytes of a reply that is not JSON.
			raw := []byte(`{"output":"` + m)
			cases++
			if keyStored(string(raw), secretKey) {
				storedCases++
				if !replyContainsKey(raw, api, secretKey) {
					t.Fatalf("%s: variant %d (%q) in a raw reply keeps the key in stored text but passed", api, vi, m)
				}
			} else {
				brokenCases++
			}
			for _, shape := range keyShapes {
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
				keep := false
				for _, s := range returnedStrings(completion) {
					keep = keep || keyStored(s, secretKey)
				}
				if !keep {
					brokenCases++
					continue
				}
				storedCases++
				if !replyContainsKey(data, api, secretKey) {
					t.Fatalf("%s: %s, variant %d (%q) split at %d keeps the key in stored text but passed", api, shape.name, vi, m, c)
				}
			}
		}
	}
	// The corpus holds both outcomes, so the property is not true by construction alone.
	if storedCases < 10000 || brokenCases < 1000 {
		t.Fatalf("corpus too narrow: %d cases, %d keep the key in stored text, %d do not", cases, storedCases, brokenCases)
	}
	t.Logf("%d cases, %d keep the key in stored text, %d do not", cases, storedCases, brokenCases)
}

// Documented behaviour: an escape sequence with visible parameters inside the key is not refused. Storage
// removes only the ESC, so the stored text shows "[0m" inside the key and no reader sees the key whole.
func TestEscapeSequenceInsideKeyIsNotRefused(t *testing.T) {
	m := secretKey[:3] + "\x1b[0m" + secretKey[3:]
	if keyStored(m, secretKey) || !strings.Contains(stored(m), "[0m") {
		t.Fatalf("stored form %q", stored(m))
	}
	for _, api := range []API{APIChatCompletions, APIResponses} {
		data, err := json.Marshal(decoyReply(api, []string{"before " + m + " after"}, decoyPlainArgs))
		if err != nil {
			t.Fatal(err)
		}
		if replyContainsKey(data, api, secretKey) {
			t.Fatalf("%s: key broken by the visible remains of an escape sequence was refused", api)
		}
		// A sequence with nothing visible left after storage is refused.
		hidden := secretKey[:3] + "\u009d\u009c" + secretKey[3:]
		data, err = json.Marshal(decoyReply(api, []string{"before " + hidden + " after"}, decoyPlainArgs))
		if err != nil {
			t.Fatal(err)
		}
		if !replyContainsKey(data, api, secretKey) {
			t.Fatalf("%s: key with an empty 8-bit OSC inside passed", api)
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
