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
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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
	// What the folded view reads as a key (third round of #845): a format character inside a key or a flag, or
	// between a key and its separator, and colon look-alikes after a key or alone.
	"--pass\u00adword", "--to\u200bken", "--client\u2060-secret", "pass\u200bword=", "pass\u200dword:",
	"pass\ufeffword", "api_key\u200b:", "token\u00ad=", "password\u200b\u200c: |",
	"password\uff1a", "token\u02d0", "password\u2e2c", "password\u0589 |", "client_secret\u05c3",
	"\uff1a", "\u2236", "\u02f8", "\u0703", "\u1361", "\u2e2c", "\uff1a |",
	// The fourth round (decision of 2026-11-18): an explicit key, an env entry's name, a quoted key and a flag
	// with text glued in front, each in one string with its value in the next; the separators and invisible
	// characters of their classes come from seqClassParts.
	"? "+passwordKey, "? "+canonTokenKey, "- name: DB_PASSWORD", "- name: API_TOKEN", `"password":`, `'token':`,
	"--args=--client-secret", `"--password`, ":--password", "=--password", "--args=--token", ":", ": |",
)

// seqClassParts are strings drawn from the classes of the fourth round: a key and a separator run of the class
// (canonSepFound and runs drawn as the corpus draws them), a separator run alone, and a key with an invisible
// character inside it.
func seqClassParts(rng *rand.Rand) []string {
	g := &canonGen{rng: rng}
	var out []string
	for i, run := range canonSepFound {
		key := canonSepKeys[i%len(canonSepKeys)]
		out = append(out, key+run, run)
	}
	for range 40 {
		run := g.sepRun()
		if sepRunPlain(run) {
			continue
		}
		out = append(out, canonPick(rng, canonSepKeys)+run, run)
	}
	for i, x := range canonInvisibles {
		key := canonGlyphKeys[i%len(canonGlyphKeys)]
		at := 1 + i%(len(key)-1)
		out = append(out, key[:at]+x+key[at:]+":", key+x+" :", "--"+key[:at]+x+key[at:])
	}
	return out
}

// seqValuePrefixes are what a value string holds before the value: nothing, indentation, an env entry's value
// key, the colon of an explicit key.
var seqValuePrefixes = []string{"", "  ", "  value: ", ": ", "\t"}

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

// seqDecided reports whether String's spans of the joined text j hide the value of the string at index i,
// which starts at byte at of it: a span that reaches into the value and, in a rendering with quotes, stays
// inside the string. A span that runs on through the quotes and
// separators of the sequence (a value read to the end of the text) is no decision about one string.
func seqDecided(j seqJoined, quoted bool, i, at int, spans []span) bool {
	it := j.items[i]
	for _, sp := range spans {
		if sp.start >= it[1] || sp.end <= it[0]+at {
			continue
		}
		if quoted && (sp.start < it[0]-1 || sp.end > it[1]+1) {
			continue
		}
		return true
	}
	return false
}

// forEachParallel calls f(i) for every i below n on as many workers as the process may run at once. f writes
// only what belongs to its i. No test that counts redaction work calls it: the counter is global.
func forEachParallel(n int, f func(i int)) {
	var next atomic.Int64
	var wg sync.WaitGroup
	for range min(runtime.GOMAXPROCS(0), n) {
		wg.Go(func() {
			for {
				i := int(next.Add(1)) - 1
				if i >= n {
					return
				}
				f(i)
			}
		})
	}
	wg.Wait()
}

func TestStringsDecidesAsTheJoinedText(t *testing.T) {
	rng := rand.New(rand.NewPCG(845, 2)) //nolint:gosec // a fixed seed keeps the sequences the same on every run
	cases := seqPropertyCases
	if raceDetector || testing.Short() {
		cases /= canonSampleDivisor
	}
	parts := append(slices.Clone(seqPropertyParts), seqClassParts(rand.New(rand.NewPCG(845, 4)))...) //nolint:gosec // a fixed seed
	// The sequences are drawn in order from the seeded generator first, then decided on as many workers as the
	// process may run (forEachParallel), each case into its own slot, so the outcome is that of a serial run.
	n := 0
	seqs := make([][]string, cases)
	for c := range seqs {
		var items []string
		for range 2 + rng.IntN(5) {
			switch rng.IntN(4) {
			case 0:
				n++
				v := fmt.Sprintf("Qz%05dxK9w", 40000+n)
				items = append(items, canonPick(rng, seqValuePrefixes)+v)
			default:
				items = append(items, canonPick(rng, parts))
			}
		}
		seqs[c] = items
	}
	type outcome struct {
		decided int
		errs    []string
	}
	outs := make([]outcome, cases)
	forEachParallel(cases, func(c int) {
		items := seqs[c]
		o := &outs[c]
		got := Strings(items)
		for ji, j := range seqJoins(items) {
			spans := defaultRedactor.spans(j.text)
			for i, it := range items {
				at := strings.Index(it, "Qz")
				if at < 0 || !seqDecided(j, ji > 0, i, at, spans) {
					continue
				}
				v := it[at:]
				o.decided++
				if !strings.Contains(got[i], v) {
					continue
				}
				o.errs = append(o.errs, fmt.Sprintf("String(%q) = %q hides %s, Strings(%q) = %q shows it", j.text, String(j.text), v, items, got))
			}
		}
	})
	failed, decided, failedSeqs := 0, 0, 0
	for _, o := range outs {
		decided += o.decided
		if len(o.errs) > 0 {
			failedSeqs++
		}
		for _, e := range o.errs {
			if failed++; failed <= 10 {
				t.Error(e)
			}
		}
	}
	if failed > 0 {
		t.Errorf("%d secrets that a joined text hides are shown apart, in %d sequences", failed, failedSeqs)
	}
	t.Logf("%d sequences, %d secrets, %d hidden by a joined text", cases, n, decided)
	if decided < n {
		t.Fatalf("only %d of %d secrets hidden by a joined text: the generator makes too few keys", decided, n)
	}
}
