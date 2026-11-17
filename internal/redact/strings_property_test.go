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
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
)

// The sequence property of #845 (decision of 2026-11-17): Strings decides on the sequence as a whole, so what
// String decides on the strings joined holds for the strings apart. For generated sequences of keys, flags,
// separators, blank strings, plain words and secrets, no string that Strings returns shows a secret that String
// redacts in the strings joined: by a line break, or written as a sequence in each rendering of the corpus (JSON,
// YAML flow and block sequences, pretty-printed and escaped JSON). A span that runs on through the quotes and
// separators of a rendering (a connection string read to the end of the text) decides nothing about one string
// and is not counted. Joined by a blank, String reads a key and colon at the start as a value to the line's end
// ("password: a b c" loses "a b c"), which a sequence of arguments does not mean, so that join is not a measure.

// seqPropertyParts are the strings a generated sequence is made of, besides its secrets.
var seqPropertyParts = append(slices.Clone(canonFlagKeys),
	"app "+canonFlagKeys[0], canonFlagKeys[0]+"\n", canonFlagKeys[0]+"-file",
	passwordKey, canonTokenKey, dbPasswordKey, clientSecretKey, passwordKey+":", canonTokenKey+":", passwordKey+": |",
	canonTokenKey+": >-", passwordKey+"=", passwordKey+" :", "pass:", "auth:", "secretName:",
	"", " ", "\n", "\t", ":", "=", ": |", "|", ">",
	canonServe, "--port=8080", "-v", "x", "--verbose", "run", "name:", "words here",
)

// seqPropertyCases is the number of generated sequences; under the race detector and in short mode, the first
// one in canonSampleDivisor of them.
const seqPropertyCases = 20000

// seqJoined is a sequence joined into one text, and where each string stands in it.
type seqJoined struct {
	text  string
	items [][2]int
}

// seqJoins are the ways the property joins the strings of a sequence into one text.
func seqJoins(items []string) []seqJoined {
	var out []seqJoined
	join := func(open, sep, quote, close string) {
		var b strings.Builder
		var j seqJoined
		b.WriteString(open)
		for i, it := range items {
			if i > 0 {
				b.WriteString(sep)
			}
			b.WriteString(quote)
			j.items = append(j.items, [2]int{b.Len(), b.Len() + len(it)})
			b.WriteString(it + quote)
		}
		b.WriteString(close)
		j.text = b.String()
		out = append(out, j)
	}
	join("", "\n", "", "")
	for _, rd := range canonSeqRenderings {
		join(rd.open, rd.sep, rd.quote, rd.close)
	}
	return out
}

// seqDecided reports whether String's spans of the joined text j hide the string at index i whole: a span that
// covers it and, in a rendering with quotes, stays inside it. A span that runs on through the quotes and
// separators of the sequence (a value read to the end of the text) is no decision about one string.
func seqDecided(j seqJoined, quoted bool, i int, spans []span) bool {
	it := j.items[i]
	for _, sp := range spans {
		if sp.start >= it[1] || sp.end <= it[0] {
			continue
		}
		if quoted && (sp.start < it[0]-1 || sp.end > it[1]+1) {
			continue
		}
		return true
	}
	return false
}

func TestStringsDecidesAsTheJoinedText(t *testing.T) {
	rng := rand.New(rand.NewPCG(845, 2)) //nolint:gosec // a fixed seed keeps the sequences the same on every run
	cases := seqPropertyCases
	if raceDetector || testing.Short() {
		cases /= canonSampleDivisor
	}
	n, failed, decided := 0, 0, 0
	for range cases {
		var items []string
		for range 2 + rng.IntN(5) {
			switch rng.IntN(4) {
			case 0:
				n++
				v := fmt.Sprintf("Qz%05dxK9w", 40000+n)
				items = append(items, []string{"", "  "}[rng.IntN(2)]+v)
			default:
				items = append(items, canonPick(rng, seqPropertyParts))
			}
		}
		got := Strings(items)
		for ji, j := range seqJoins(items) {
			spans := defaultRedactor.spans(j.text)
			for i, it := range items {
				v := strings.TrimLeft(it, " ")
				if !strings.HasPrefix(v, "Qz") || !seqDecided(j, ji > 0, i, spans) {
					continue
				}
				decided++
				if !strings.Contains(got[i], v) {
					continue
				}
				if failed++; failed <= 10 {
					t.Errorf("String(%q) = %q hides %s, Strings(%q) = %q shows it", j.text, String(j.text), v, items, got)
				}
			}
		}
	}
	if failed > 0 {
		t.Errorf("%d secrets that a joined text hides are shown apart", failed)
	}
	t.Logf("%d sequences, %d secrets, %d hidden by a joined text", cases, n, decided)
	if decided < n {
		t.Fatalf("only %d of %d secrets hidden by a joined text: the generator makes too few keys", decided, n)
	}
}
