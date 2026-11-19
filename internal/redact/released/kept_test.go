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

package released

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	v052 "github.com/pkalin/shadewright/internal/redact/released/testdata/v052"
)

// #873 makes the work of this reading linear on the shapes where it was not (block scalars under a bare short
// key at ever deeper indentation, and a value of many glued fields after a key name) without changing what it
// redacts. The reference is the code before that change, kept in testdata/v052: these tests check that both find
// the same spans, byte for byte and with the same kinds, on generated inputs of both shapes and of their
// neighbours. The corpora of package redact (35,503 inputs) are compared the same way there
// (TestReleasedSpansMatchKeptCopy), and with the stored results of the release (TestReleasedReproducesTheReleaseBefore).

// TestKeptCopyIsUnchanged pins the copy the differential tests compare with: an edit to it would change the
// reference without any other test noticing.
func TestKeptCopyIsUnchanged(t *testing.T) {
	for name, want := range map[string]string{
		"redact.go": "00ea551dc08a73a45b9b4f2fe4266be6ab61e1cc82c27402b60858a659cfba9e",
		"extent.go": "e0c5bd4d3c9cfdeb809c103c7ccbc670469df7c507751bf69c6420cff5aa03d1",
		"work.go":   "d363d1ee0351edfd6373908c1095b660f3726e729aa074ee3d19ffb39c9cce99",
	} {
		b, err := os.ReadFile(filepath.Join("testdata", "v052", name))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(b)
		if got := hex.EncodeToString(sum[:]); got != want {
			t.Errorf("testdata/v052/%s changed: sha256 %s, want %s", name, got, want)
		}
	}
}

// sampled returns n, or about one in sixteen of n (at least one) under -race, whose instrumentation makes the
// reading 10 to 20 times slower; make test runs this package again without it.
func sampled(n int) int {
	if raceDetector {
		return max(1, n/16)
	}
	return n
}

// sameAsKept returns a failure when Spans differs from the kept copy on s, or "".
func sameAsKept(s string) string {
	got, want := Spans(s), v052.Spans(s)
	if slices.EqualFunc(got, want, func(a Span, b v052.Span) bool {
		return a.Start == b.Start && a.End == b.End && a.Kind == b.Kind
	}) {
		return ""
	}
	clip := s
	if len(clip) > 300 {
		clip = clip[:300] + "..."
	}
	return fmt.Sprintf("on %q (%d bytes): spans %v, the release before %v", clip, len(s), got, want)
}

// checkSameAsKept compares every input and stops after ten failures.
func checkSameAsKept(t *testing.T, name string, inputs []string) {
	t.Helper()
	fails := 0
	for _, s := range inputs {
		if f := sameAsKept(s); f != "" {
			t.Errorf("%s: %s", name, f)
			if fails++; fails == 10 {
				t.Fatalf("%s: stopping after ten differences", name)
			}
		}
	}
	t.Logf("%s: %d inputs", name, len(inputs))
}

// Parts of the generated block texts: key names (bare short keys, long secret keys, plain words and their
// look-alikes), what follows the colon of a header, content lines, and line ends.
var (
	blockKeys = []string{"pass", "PASS", "auth", "sig", "sas", "pre-auth", "db_pass", "password", "token", "storageSas",
		"x", "a", "pass\u200b", "\"pass\"", "'password'"}
	blockAfterColon = []string{": |", ": >-", ": |2+", ": '|", ": | x", ": |  # c", " : >", ":", ": ", ": | pass: |", ": |\r",
		":|", ": >1"}
	blockContents = []string{"plain words here", "x", "hunter2", "# a comment", "pass: value", "- item", "-", "\"quoted\"",
		"'q'", "key: |", "a: | pass: |", "? pass", "--token abc", "pass=", "\u00e9t\u00e9"}
	blockEnds = []string{"\n", "\n", "\n", "\n", "\n", "\n", "\r\n", "\r\r\n", "\r", "\n\n", "\n \n", "\n\t\n"}
)

// secretLike returns a token that the reading counts as a secret on a line of its own (looksLikeSecret): mixed
// case letters and digits, built from parts so that no credential-shaped literal is in the source.
func secretLike(r *rand.Rand) string {
	const alnum = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789"
	var b strings.Builder
	b.WriteString("Qz7")
	for range 21 {
		b.WriteByte(alnum[r.IntN(len(alnum))])
	}
	return b.String()
}

// pick returns a random element of xs.
func pick(r *rand.Rand, xs []string) string { return xs[r.IntN(len(xs))] }

// indentOf returns n columns of indentation, mostly blanks, sometimes with a tab.
func indentOf(r *rand.Rand, n int) string {
	if n > 0 && r.IntN(8) == 0 {
		return strings.Repeat(" ", n-1) + "\t"
	}
	return strings.Repeat(" ", n)
}

// blockText returns a generated YAML-like text of headers, content and blank lines at random depths, in one of
// four orders of depth: random, ever deeper (the staircase of #873), ever shallower, or up and down.
func blockText(r *rand.Rand) string {
	var b strings.Builder
	lines := 1 + r.IntN(60)
	order := r.IntN(4)
	for i := range lines {
		depth := r.IntN(12)
		switch order {
		case 1:
			depth = i
		case 2:
			depth = lines - i
		case 3:
			depth = i % 7
		}
		b.WriteString(indentOf(r, depth))
		if r.IntN(6) == 0 {
			b.WriteString("- ")
		}
		switch k := r.IntN(10); {
		case k < 5:
			b.WriteString(pick(r, blockKeys) + pick(r, blockAfterColon))
		case k < 7:
			b.WriteString(pick(r, blockContents))
		case k < 8:
			b.WriteString(secretLike(r))
		case k < 9:
			b.WriteString(pick(r, blockKeys) + ": " + secretLike(r))
		}
		b.WriteString(pick(r, blockEnds))
	}
	if r.IntN(2) == 0 {
		b.WriteString("tail: visible")
	}
	return b.String()
}

// stairText returns the staircase of line (each line one column deeper than the one before) of n lines, with
// blank lines and a content line put in at random.
func stairText(r *rand.Rand, line string, n int) string {
	var b strings.Builder
	for i := range n {
		b.WriteString(strings.Repeat(" ", i))
		b.WriteString(line)
		b.WriteString(pick(r, blockEnds))
		if r.IntN(10) == 0 {
			b.WriteString(strings.Repeat(" ", i+1+r.IntN(3)))
			if r.IntN(2) == 0 {
				b.WriteString(secretLike(r))
			} else {
				b.WriteString(pick(r, blockContents))
			}
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// Parts of the generated values of glued fields after a key name: quoted pieces, commas, a field after a comma
// with each separator and with key names spelled with the characters a case-insensitive key name also reads
// (KELVIN SIGN for "k", LONG S for "s"), escaped blanks, groups and the look-alike colon of the fuzz input.
var (
	commaPrefixes = []string{"", "x ", "--", "{", "\"", "- ", "env "}
	commaKeys     = []string{"password", "TOKEN", "pass", "api_key", "apiKey", "secret", "x"}
	commaSeps     = []string{"=", ": ", ":=", " = ", "='", "=\""}
	commaPieces   = []string{"\"a\"", "'b'", ",", "b", ",password=", ",PASSWORD :", ",api_\u212aey=", ",\u017fecret=",
		",token\t=", ",x=", ",x= ", ",x==", ", ", " ", "$(c)", "`c`", "TOKEN=", "\\ ", ",\ufe55", ",\"q\"", ")", "}x",
		"\\!", ",abc=", ",user=bob", ",db_password ", ",db_password\t", "\n", ",-", ",a.b-c:", ",\u212a", ",\u017f",
		",pass", ",pass:", ",\x89", "\x89", ",\u212a\u017f=", ",K:", ",passwordK=", "SECRETPROBE0005"}
)

// commaText returns a generated value of glued fields after a key name.
func commaText(r *rand.Rand, pieces int) string {
	var b strings.Builder
	b.WriteString(pick(r, commaPrefixes) + pick(r, commaKeys) + pick(r, commaSeps))
	for range pieces {
		b.WriteString(pick(r, commaPieces))
	}
	return b.String()
}

// The reading finds the spans of the release before on generated block texts and staircases.
func TestBlocksMatchKeptCopy(t *testing.T) {
	r := rand.New(rand.NewPCG(873, 1)) // #nosec G404 -- reproducible generated texts
	inputs := make([]string, 0, 4200)
	for range sampled(4000) {
		inputs = append(inputs, blockText(r))
	}
	for range sampled(200) {
		inputs = append(inputs, stairText(r, pick(r, stairLines), 5+r.IntN(80)))
	}
	checkSameAsKept(t, "blocks", inputs)
}

// stairLines are the lines of the staircases of package redact (canonStairLines) that #873 is about, and their
// neighbours.
var stairLines = []string{"pass: |", "pass: | pass: |", "pass: | x", "pass: '|", "a: | pass: |", "- pass: |", "x pass: |",
	"pass:", "? pass", "- value: |", "pass\u200b: |", "pass\uff1a |", "password: |", "auth: >-", "sig: |2+", "pass: |\r",
	"pass: |  # c"}

// The reading finds the spans of the release before on generated values of glued fields, short ones and ones
// longer than the extent rule's bound (maxValueExtent), so that a field after a comma near that bound is read.
func TestCommaFieldsMatchKeptCopy(t *testing.T) {
	r := rand.New(rand.NewPCG(873, 2)) // #nosec G404 -- reproducible generated texts
	inputs := make([]string, 0, 4100)
	for range sampled(4000) {
		inputs = append(inputs, commaText(r, 1+r.IntN(40)))
	}
	for range sampled(60) {
		inputs = append(inputs, commaText(r, 1000+r.IntN(2000)))
	}
	checkSameAsKept(t, "comma fields", inputs)
}

// fuzzLabelUnit is the unit the label value of the FuzzDiffBuild input testdata/fuzz/FuzzDiffBuild/bb930588237836fd
// (package incidentcontext) repeats, and fuzzLabelKeyUnit the one of its label name.
const (
	fuzzLabelUnit    = "SECRETPROBE0005TOKEN=\"a\",b,\ufe55"
	fuzzLabelKeyUnit = "{TOKEN='SECRETPROBE0000}SECRETPROBE0001\": \"\x16SECRETPROBE0002SECRETPROBE0003"
)

// The reading finds the spans of the release before on the units of the fuzz input, repeated past the extent
// bound, in the line the context builder writes for a series.
func TestFuzzLabelMatchesKeptCopy(t *testing.T) {
	sizes := []int{1, 2, 3, 50, 147, 148, 149, 300}
	if raceDetector {
		sizes = sizes[:4]
	}
	inputs := make([]string, 0, 3*len(sizes))
	for _, n := range sizes {
		key, value := strings.Repeat(fuzzLabelKeyUnit, n/4+1), strings.Repeat(fuzzLabelUnit, n)
		inputs = append(inputs, value, key, "metric{"+key+"="+value+",pod=web} 1")
	}
	checkSameAsKept(t, "fuzz label", inputs)
}

// Answers known by construction, on both the reading and the kept copy: a block scalar under a bare short key
// whose lines hold a secret deep in a staircase is redacted from its first line to its last, the secret with it,
// and the line after the block stays; a field after a comma glued to a quoted value ends the value, so the
// value is redacted and the field after it stays.
func TestKnownAnswers(t *testing.T) {
	r := rand.New(rand.NewPCG(873, 3)) // #nosec G404 -- reproducible generated texts
	for i := range sampled(200) {
		secret := secretLike(r)
		depth := 1 + r.IntN(60)
		var b strings.Builder
		for d := range depth {
			b.WriteString(strings.Repeat(" ", d) + "pass: |\n")
		}
		b.WriteString(strings.Repeat(" ", depth+r.IntN(3)) + secret + "\n")
		b.WriteString("tail: visible\n")
		text := b.String()
		for name, out := range map[string]string{"reading": String(text), "kept": render(text, toSpans(v052.Spans(text)))} {
			if strings.Contains(out, secret) || !strings.HasSuffix(out, "\ntail: visible\n") {
				t.Fatalf("%s, staircase %d of depth %d: %q", name, i, depth, out)
			}
		}
		field := pick(r, []string{"user=bob", "api_\u212aey=" + secret, "\u017fecret: " + secret, "next: x"})
		text = "password=\"" + secret + "\"," + field
		head, _, _ := strings.Cut(field, "=")
		head, _, _ = strings.Cut(head, ":")
		for name, out := range map[string]string{"reading": String(text), "kept": render(text, toSpans(v052.Spans(text)))} {
			if strings.Contains(out, secret) || !strings.Contains(out, ","+head) {
				t.Fatalf("%s, fields %q: %q", name, text, out)
			}
		}
	}
}

// toSpans converts the spans of the kept copy, so that render writes them as the reading does.
func toSpans(in []v052.Span) []span {
	out := make([]span, 0, len(in))
	for _, x := range in {
		out = append(out, span{start: x.Start, end: x.End, kind: x.Kind})
	}
	return out
}

// commaKeyReach cuts the text commaSecretKey reads after the characters a key name is made of and the separator
// after them. Checked for every character: one inside a key name keeps the match exactly when commaKeyReach
// reads past it, and one right after the name keeps it exactly when it is a blank, ":", "=" or such a character.
func TestCommaKeyReachCoversEveryMatch(t *testing.T) {
	// Under -race: every ASCII character, the two that fold to one, and one in 61 of the rest.
	from, step := rune(0), rune(1)
	if raceDetector {
		from, step = 0x80, 61
		for r := range rune(0x80) {
			checkCommaKeyReach(t, r)
		}
		checkCommaKeyReach(t, '\u212a')
		checkCommaKeyReach(t, '\u017f')
	}
	for r := from; r <= 0x10ffff; r += step {
		if r < 0xd800 || r > 0xdfff {
			checkCommaKeyReach(t, r)
		}
	}
}

// checkCommaKeyReach is TestCommaKeyReachCoversEveryMatch on one character.
func checkCommaKeyReach(t *testing.T, r rune) {
	t.Helper()
	c := string(r)
	inName := "x" + c + "password="
	reads := commaKeyReach(inName, 0, len(inName)) == len(inName)
	if got := commaSecretKey.MatchString(inName); got != reads {
		t.Errorf("U+%04X inside a key name: the expression matches %v, commaKeyReach reads past it %v", r, got, reads)
	}
	after := "password" + c + "="
	sep := r == ' ' || r == '\t' || r == ':' || r == '='
	reach := commaKeyReach(after, 0, len(after))
	if got := commaSecretKey.MatchString(after); got != (sep || reach == len(after)) {
		t.Errorf("U+%04X after a key name: the expression matches %v, commaKeyReach reads %d of %d bytes", r, got,
			reach, len(after))
	}
	if got, cut := commaSecretKey.MatchString(after), commaSecretKey.MatchString(after[:reach]); got != cut {
		t.Errorf("U+%04X after a key name: the expression matches %v on the text, %v on the part commaKeyReach gives", r, got, cut)
	}
}

// The line index gives the answers of the line by line reading it replaces (indentedLines, and
// blockLooksLikeSecret on the block found), for every position and for key columns around the indentation of
// the generated texts, asked in random order so that the lines judged for blockLooksLikeSecret are extended in
// any order. The spans tests above cannot see every answer, because another rule often redacts the same lines.
func TestLineIndexMatchesLineByLine(t *testing.T) {
	r := rand.New(rand.NewPCG(873, 4)) // #nosec G404 -- reproducible generated texts
	cols := []int{-1, 0, 1, 2, 3, 4, 5, 6, 8, 11, 12, 13, 100}
	for i := range sampled(1500) {
		s := blockText(r)
		if i%5 == 0 {
			s = stairText(r, pick(r, stairLines), 5+r.IntN(40))
		}
		x := newLineIndex(s)
		for _, pos := range r.Perm(len(s) + 1) {
			c := cols[r.IntN(len(cols))]
			wantStart, wantEnd := indentedLines(s, pos, c)
			start, end, first, last := x.indentedLines(pos, c)
			if start != wantStart || end != wantEnd {
				t.Fatalf("indentedLines(%q, %d, %d) = %d, %d, the index %d, %d", s, pos, c, wantStart, wantEnd, start, end)
			}
			if start < 0 {
				continue
			}
			if got, want := x.anyLooksLikeSecret(first, last), blockLooksLikeSecret(s, start, end); got != want {
				t.Fatalf("blockLooksLikeSecret(%q, %d, %d) = %v, the index %v", s, start, end, want, got)
			}
		}
	}
}
