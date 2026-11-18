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
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// This corpus does not share a list of characters with the implementation: the runes put inside the key are
// drawn by code point from the whole Unicode range (every plane, every general category, unassigned code
// points, noncharacters and the surrogate range), and the answer is known by construction. A rune outside
// the key's own alphabet (ASCII letters, digits and the punctuation the key contains) inserted between the
// key's characters leaves the key readable, so the reply must be refused. The reply places the key, whole or
// split, in every string position a reply has.

// corpusKeys are keys of the shapes the check must handle: the package's marker, a long project key, a key
// with several punctuation characters (JSON quote and backslash included), and a key of the minimum length.
var corpusKeys = []string{
	secretKey,
	// Built from parts: a literal of this shape in the source is reported by secret scanning.
	"sk-" + "proj-" + "Q7mZp2Lx9TbV4nWc8Rk3" + "Ys6Hd1Gf5Jq0AeUoIiXy",
	`ak_live.9f"T\x~2!Q#z$Wq%^&*(r)+=[k]{p}|;:',<>?/` + "`" + `m`,
	"Zq8Vn3Lr6Tc1Xw4B",
}

// keyAlphabet reports whether r survives the check's view for key: ASCII letters, digits and the key's
// punctuation. Written here from the decision, not taken from the implementation.
func keyAlphabet(key string, r rune) bool {
	if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
		return true
	}
	return r < utf8.RuneSelf && strings.ContainsRune(key, r)
}

// codePointPool draws code points from the whole range: from each of the 17 planes, from every general
// category table, and fixed edges (C0 and C1 controls, the surrogate range, noncharacters, the last code
// point, unassigned blocks, and the runes the earlier lists missed).
func codePointPool(r *rand.Rand) []rune {
	var pool []rune
	for plane := range rune(17) {
		for range 12 {
			pool = append(pool, plane<<16|r.Int32N(1<<16))
		}
	}
	for _, table := range unicode.Categories {
		var all []rune
		for _, rg := range table.R16 {
			for c := rune(rg.Lo); c <= rune(rg.Hi) && len(all) < 4096; c += rune(rg.Stride) {
				all = append(all, c)
			}
		}
		for _, rg := range table.R32 {
			for c := rg.Lo; c <= rg.Hi && len(all) < 8192; c += rg.Stride {
				all = append(all, rune(c)) // #nosec G115 -- a Unicode table holds code points, below 0x110000
			}
		}
		for range 3 {
			if len(all) > 0 {
				pool = append(pool, all[r.IntN(len(all))])
			}
		}
	}
	pool = append(pool,
		0x00, 0x01, 0x08, 0x09, 0x0a, 0x0d, 0x1b, 0x1f, ' ', '.', '"', '\\', '/', '<', 0x7f, 0x80, 0x85, 0x9b, 0x9f, 0xa0, 0xad,
		0x034f, 0x0378, 0x061c, 0x115f, 0x1160, 0x17b4, 0x17b5, 0x180e, 0x200b, 0x2028, 0x2064, 0x206f, 0x3164,
		0xd800, 0xdbff, 0xdc00, 0xdfff, 0xe000, 0xfdd0, 0xfe00, 0xfe0f, 0xfeff, 0xffa0, 0xfff9, 0xfffd, 0xfffe, 0xffff,
		0x1d173, 0x2fffe, 0x3ffff, 0xe0001, 0xe0020, 0xe0100, 0xe01ef, 0xeffff, 0x10fffd, 0x10ffff,
	)
	return pool
}

// jsonLiteral encodes runes as a JSON string. Escaped writes every rune outside printable ASCII as \u
// escapes (astral ones as surrogate pairs, a surrogate code point as one lone escape); otherwise runes are
// written as UTF-8 and a surrogate code point as its raw three bytes, which a decoder reads as U+FFFD.
func jsonLiteral(rs []rune, escaped bool) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, c := range rs {
		switch {
		case c == '"' || c == '\\':
			b.WriteByte('\\')
			b.WriteRune(c)
		case c < 0x20 || escaped && c >= 0x7f:
			if c > 0xffff {
				hi, lo := 0xd800+(c-0x10000)>>10, 0xdc00+(c-0x10000)&0x3ff
				fmt.Fprintf(&b, `\u%04x\u%04x`, hi, lo)
			} else {
				fmt.Fprintf(&b, `\u%04x`, c)
			}
		case c >= 0xd800 && c <= 0xdfff:
			b.Write([]byte{0xe0 | byte(c>>12), 0x80 | byte(c>>6)&0x3f, 0x80 | byte(c)&0x3f}) // #nosec G115 -- c is a surrogate, below 0x10000
		default:
			b.WriteRune(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// position builds a whole reply of one API around one or two JSON string literals (a and b, b empty when
// the key is not split).
type position struct {
	name  string
	split bool
	build func(a, b string) string
}

// posStreamDelta is a streamed chunk, a shape the caller does not decode.
const posStreamDelta = "stream-delta"

const (
	chatHead  = `{"model":"m","choices":[{"finish_reason":"stop","message":{`
	chatTail  = `}}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`
	respHead  = `{"model":"m","status":"completed","output":[`
	respTail  = `],"usage":{"input_tokens":3,"output_tokens":2}}`
	respText  = `{"type":"message","role":"assistant","content":[{"type":"output_text","text":%s}]}`
	respCall  = `{"type":"function_call","call_id":"c1","name":%s,"arguments":%s}`
	chatCalls = `"tool_calls":[{"id":"c1","type":"function","function":{"name":%s,"arguments":%s}}]`
)

// argsWith returns a tool-call arguments literal: JSON text holding lit as a value, encoded as a string.
func argsWith(lit string) string {
	return jsonLiteral([]rune(`{"command":`+lit+`,"n":1}`), false)
}

func positions(api API) []position {
	q := func(s string) string { return jsonLiteral([]rune(s), false) }
	if api == APIChatCompletions {
		return []position{
			{"message-content", false, func(a, _ string) string { return chatHead + `"content":` + a + chatTail }},
			{"message-refusal", false, func(a, _ string) string { return chatHead + `"content":null,"refusal":` + a + chatTail }},
			{"tool-name", false, func(a, _ string) string {
				return chatHead + `"content":null,` + fmt.Sprintf(chatCalls, a, q(`{}`)) + chatTail
			}},
			{"arguments-raw", false, func(a, _ string) string {
				return chatHead + `"content":null,` + fmt.Sprintf(chatCalls, q("f"), a) + chatTail
			}},
			{"arguments-value", false, func(a, _ string) string {
				return chatHead + `"content":null,` + fmt.Sprintf(chatCalls, q("f"), argsWith(a)) + chatTail
			}},
			{posStreamDelta, false, func(a, _ string) string {
				return `{"model":"m","choices":[{"index":0,"delta":{"content":` + a + `}}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`
			}},
			{"unknown-nested", false, func(a, _ string) string {
				return chatHead + `"content":"ok","x_meta":{"deep":[{"note":` + a + `}]}` + chatTail
			}},
			{"unknown-top", false, func(a, _ string) string {
				return `{"system_fingerprint":` + a + `,` + chatHead[1:] + `"content":"ok"` + chatTail
			}},
			{"object-key", false, func(a, _ string) string {
				return chatHead + `"content":"ok","x":{` + a + `:1}` + chatTail
			}},
			{"split-content-arguments", true, func(a, b string) string {
				return chatHead + `"content":` + a + `,` + fmt.Sprintf(chatCalls, q("f"), argsWith(b)) + chatTail
			}},
			{"split-choices", true, func(a, b string) string {
				return `{"model":"m","choices":[{"message":{"content":` + a + `}},{"message":{"content":` + b + `}}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`
			}},
			{"split-refusal-unknown", true, func(a, b string) string {
				return chatHead + `"content":null,"refusal":` + a + `,"x_note":` + b + chatTail
			}},
		}
	}
	return []position{
		{"text", false, func(a, _ string) string { return respHead + fmt.Sprintf(respText, a) + respTail }},
		{"refusal-part", false, func(a, _ string) string {
			return respHead + `{"type":"message","content":[{"type":"refusal","refusal":` + a + `}]}` + respTail
		}},
		{"tool-name", false, func(a, _ string) string { return respHead + fmt.Sprintf(respCall, a, q(`{}`)) + respTail }},
		{"arguments-raw", false, func(a, _ string) string { return respHead + fmt.Sprintf(respCall, q("f"), a) + respTail }},
		{"arguments-value", false, func(a, _ string) string {
			return respHead + fmt.Sprintf(respCall, q("f"), argsWith(a)) + respTail
		}},
		{posStreamDelta, false, func(a, _ string) string {
			return `{"type":"response.output_text.delta","item_id":"i","output_index":0,"delta":` + a + `}`
		}},
		{"unknown-nested", false, func(a, _ string) string {
			return respHead + fmt.Sprintf(respText, q("ok")) + `],"metadata":{"trace":[{"v":` + a + `}]},"x":[]` + respTail[1:]
		}},
		{"object-key", false, func(a, _ string) string {
			return respHead + fmt.Sprintf(respText, q("ok")) + `],"metadata":{` + a + `:true},"x":[]` + respTail[1:]
		}},
		{"split-text-parts", true, func(a, b string) string {
			return respHead + `{"type":"message","content":[{"type":"output_text","text":` + a + `},{"type":"output_text","text":` + b + `}]}` + respTail
		}},
		{"split-text-arguments", true, func(a, b string) string {
			return respHead + fmt.Sprintf(respText, a) + `,` + fmt.Sprintf(respCall, q("f"), argsWith(b)) + respTail
		}},
		{"split-name-unknown", true, func(a, b string) string {
			return respHead + fmt.Sprintf(respCall, a, q(`{}`)) + `],"metadata":{"v":` + b + `},"x":[]` + respTail[1:]
		}},
	}
}

// interleave puts gaps[i] after the key's i-th character (gaps has len(key)-1 entries).
func interleave(key string, gaps [][]rune) []rune {
	out := make([]rune, 0, len(key)*3)
	for i, c := range key {
		out = append(out, c)
		if i < len(gaps) {
			out = append(out, gaps[i]...)
		}
	}
	return out
}

type unicodeCase struct {
	name  string
	api   API
	key   string
	reply string
}

// unicodeCorpus builds the positive cases: every pool rune between every pair of the key's characters, and
// seeded mixes of one to three pool runes per gap, in every position, encoded raw and escaped, split at a
// seeded cut where the position takes two literals.
func unicodeCorpus() []unicodeCase {
	r := rand.New(rand.NewPCG(834, 4)) // #nosec G404 -- reproducible adversarial corpus
	pool := codePointPool(r)
	var out []unicodeCase
	for ki, key := range corpusKeys {
		var foreign []rune
		for _, c := range pool {
			if !keyAlphabet(key, c) {
				foreign = append(foreign, c)
			}
		}
		variants := make([][]rune, 0, len(foreign)+64)
		for _, c := range foreign {
			gaps := make([][]rune, len(key)-1)
			for i := range gaps {
				gaps[i] = []rune{c}
			}
			variants = append(variants, interleave(key, gaps))
		}
		for range 64 {
			gaps := make([][]rune, len(key)-1)
			for i := range gaps {
				for range r.IntN(4) {
					gaps[i] = append(gaps[i], foreign[r.IntN(len(foreign))])
				}
			}
			variants = append(variants, interleave(key, gaps))
		}
		for _, api := range []API{APIChatCompletions, APIResponses} {
			for _, pos := range positions(api) {
				for vi, v := range variants {
					escaped := (vi+ki)%2 == 1
					// Surround the key with ordinary text so its edges are glued to other characters too.
					whole := append(append([]rune("see: "), v...), []rune(" (end)")...)
					reply := pos.build(jsonLiteral(whole, escaped), "")
					if pos.split {
						cut := 5 + 1 + r.IntN(len(v)-2)
						reply = pos.build(jsonLiteral(whole[:cut], escaped), jsonLiteral(whole[cut:], !escaped))
					}
					out = append(out, unicodeCase{fmt.Sprintf("%s/%s/key%d/variant%d", api, pos.name, ki, vi), api, key, reply})
				}
			}
		}
	}
	return out
}

// Property: a key with any rune outside its alphabet between its characters, in any string position of
// either API, whole or split across two fields, is refused.
func TestUnicodeInsertCorpus(t *testing.T) {
	cases := unicodeCorpus()
	if len(cases) < 20000 {
		t.Fatalf("corpus too narrow: %d cases", len(cases))
	}
	missed := 0
	for _, c := range cases {
		if !json.Valid([]byte(c.reply)) {
			t.Fatalf("%s: the corpus built a reply that is not JSON: %q", c.name, c.reply)
		}
		if scanReply([]byte(c.reply), c.api, c.key) != scanKey {
			missed++
			if missed <= 5 {
				t.Errorf("%s: key passed the scan: %q", c.name, c.reply)
			}
		}
	}
	if missed > 0 {
		t.Fatalf("%d of %d cases passed the scan", missed, len(cases))
	}
	t.Logf("%d cases", len(cases))
}

// differentKey changes one character in the middle of key to another of its alphabet, so the result has
// the key's shape and is never the key.
func differentKey(key string) string {
	b := []byte(key)
	i := len(b) / 2
	switch c := b[i]; {
	case c >= '0' && c <= '9':
		b[i] = '0' + (c-'0'+1)%10
	case c >= 'a' && c <= 'z':
		b[i] = 'a' + (c-'a'+1)%26
	case c >= 'A' && c <= 'Z':
		b[i] = 'A' + (c-'A'+1)%26
	default:
		b[i] = 'Q'
	}
	return string(b)
}

// ordinaryText writes about n bytes of prose: words, numbers, the key's punctuation, non-Latin scripts and
// line breaks, as a model or a tool would write it.
func ordinaryText(r *rand.Rand, n int) string {
	words := []string{"the", "pod", "web-app", "restarts", "OOMKilled", "limit", "256Mi", "raise", "to", "512Mi",
		"deployment.yaml", "kubectl", "describe", "Back-off", "image", "ghcr.io/acme/api:1.4.2", "sk", "proj", "ak_live",
		"CrashLoopBackOff", "żółć", "数据库", "ошибка", "🙂", "e=mc²", "#42", "(see", "logs)", "key:", "token=", "\n"}
	var b strings.Builder
	for b.Len() < n {
		b.WriteString(words[r.IntN(len(words))])
		b.WriteByte(" .,;-_"[r.IntN(6)])
		if r.IntN(9) == 0 {
			fmt.Fprintf(&b, "%d ", r.Uint32())
		}
	}
	return b.String()
}

// Property: replies that do not hold the key stay clean: long ordinary text in every position, and a key of
// the same shape with the same insertions.
func TestUnicodeCorpusNegatives(t *testing.T) {
	r := rand.New(rand.NewPCG(834, 5)) // #nosec G404 -- reproducible corpus
	pool := codePointPool(r)
	cases := 0
	for ki, key := range corpusKeys {
		other := differentKey(key)
		if other == key {
			t.Fatal("differentKey returned the key")
		}
		for _, api := range []API{APIChatCompletions, APIResponses} {
			for _, pos := range positions(api) {
				if pos.name == posStreamDelta || pos.name == "split-choices" {
					continue // not a shape the caller decodes: refused as the wrong type, not as clean
				}
				for vi := range 24 {
					gaps := make([][]rune, len(other)-1)
					for i := range gaps {
						if c := pool[r.IntN(len(pool))]; !keyAlphabet(key, c) {
							gaps[i] = []rune{c}
						}
					}
					text := ordinaryText(r, 64+r.IntN(1<<14))
					lit := []rune(text[:len(text)/2] + " " + string(interleave(other, gaps)) + " " + text[len(text)/2:])
					if vi%3 == 0 {
						lit = []rune(ordinaryText(r, 1<<15))
					}
					reply := pos.build(jsonLiteral(lit, vi%2 == 1), "")
					if pos.split {
						cut := len(lit) / 2
						reply = pos.build(jsonLiteral(lit[:cut], false), jsonLiteral(lit[cut:], true))
					}
					if got := scanReply([]byte(reply), api, key); got != scanClean {
						t.Fatalf("%s/%s/key%d/%d: clean reply refused (%d)", api, pos.name, ki, vi, got)
					}
					cases++
				}
			}
		}
	}
	t.Logf("%d clean cases", cases)
}

// The reported cases: one rune between every character of the key in chat content passed the scan at the
// time of the finding.
func TestReportedInvisibleRunesBetweenKeyCharacters(t *testing.T) {
	for _, c := range []rune{0x034f, 0xfe00, 0xfe0f, 0xe0100, 0x115f, 0x1160, 0x3164, 0xffa0, 0x17b4} {
		gaps := make([][]rune, len(secretKey)-1)
		for i := range gaps {
			gaps[i] = []rune{c}
		}
		reply := positions(APIChatCompletions)[0].build(jsonLiteral(interleave(secretKey, gaps), false), "")
		if scanReply([]byte(reply), APIChatCompletions, secretKey) != scanKey {
			t.Errorf("%U between the key's characters passed the scan", c)
		}
	}
}
