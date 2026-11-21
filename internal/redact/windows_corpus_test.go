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
	"maps"
	"math/rand/v2"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The windowed reading of an expression (windows.go, #873 second round) must find exactly what the expression
// finds on the whole text: FindAllStringSubmatchIndex of package regexp is the old logic, kept as the oracle.

// windowExprs are the expressions read by windows: those of the rules and those the scanning rules apply to a
// whole text.
func windowExprs(t *testing.T) map[string]*regexp.Regexp {
	t.Helper()
	r, err := New()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*regexp.Regexp{
		"blockScalarHeader": blockScalarHeader,
		"seqBoundary":       seqBoundary,
		"sepKey":            sepKey,
		"viewKey":           viewKey,
	}
	for i, ru := range r.rules {
		if ru.re != nil {
			out[fmt.Sprintf("rule %d", i)] = ru.re
		}
	}
	return out
}

// windowExprsSharing is the share of the expressions a generated text checks under -race: one in four.
const windowExprsSharing = 4

// windowExprNames returns the names of exprs in order. Under -race it returns one in windowExprsSharing of
// them, the quarter chosen by at, so that the texts checked in turn see every expression; make test runs this
// package again without -race, where every text checks every expression (decision of 2026-11-21 on #873).
func windowExprNames(exprs map[string]*regexp.Regexp, at int) []string {
	names := slices.Sorted(maps.Keys(exprs))
	if !raceDetector {
		return names
	}
	var out []string
	for k, n := range names {
		if k%windowExprsSharing == at%windowExprsSharing {
			out = append(out, n)
		}
	}
	return out
}

// windowKeys are the key words of the generated texts of the windowed reading.
var windowKeys = []string{passwordKey, "aws_secret_access_key", "x_token", "apiKey", authKey, "Bearer ", "name", "value",
	"secretName", passKey, "sig", sasKeyName, "DB_PASS", "AccountKey", "blobSas", clientSecretKey, "Authorization"}

// windowText generates a text of at least windowMinText bytes: fragments of keys, separators, values, look-alike,
// invisible, invalid and multi-byte characters, with long runs of key words (each at least windowMinRun bytes),
// some repeated in other places with other characters around them.
func windowText(rng *rand.Rand) string {
	keys := windowKeys
	seps := []string{":", "=", ": ", "=\"", "\"", "'", ",", "{", "}", "[", "]", " ", "\t", "\n", "\r\n", "  ", "- ",
		"|", ">", "#", "?", "&", "//", "@", ";", `\"`, `\n`, ":=", "--", "x://u:", "\\"}
	odd := []string{"\ufe55", "\uff1a", "\ufeff", "\u200b", "\u212a", "\u017f", "\xbf", "\xe2\x82", "\u00e9", "\u00a0",
		"\u2028", "\xff\xfe", "\U0001f600"}
	const alnum = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/_-."
	value := func() string {
		var b strings.Builder
		for range 1 + rng.IntN(40) {
			b.WriteByte(alnum[rng.IntN(len(alnum))])
		}
		return b.String()
	}
	pick := func(xs []string) string { return xs[rng.IntN(len(xs))] }
	fragment := func() string {
		switch rng.IntN(5) {
		case 0, 1:
			return pick(keys)
		case 2:
			return pick(seps)
		case 3:
			return value()
		default:
			return pick(odd)
		}
	}
	runs := make([]string, 0, 3)
	for range 1 + rng.IntN(3) {
		var unit strings.Builder
		for range 1 + rng.IntN(3) {
			unit.WriteString(pick(keys[:12]))
			unit.WriteString([]string{"", "", "_", "-", "."}[rng.IntN(5)])
		}
		n := windowMinRun + rng.IntN(3*windowMinRun)
		runs = append(runs, strings.Repeat(unit.String(), n/unit.Len()+1))
	}
	var b strings.Builder
	for b.Len() < windowMinText+rng.IntN(3*windowMinText) {
		if rng.IntN(6) == 0 {
			b.WriteString(pick(runs))
			continue
		}
		b.WriteString(fragment())
	}
	return b.String()
}

// Every expression read by windows finds on generated texts exactly what it finds read whole, with its groups.
func TestWindowedFindsWhatTheExpressionFinds(t *testing.T) {
	exprs := windowExprs(t)
	windowedOnes := 0
	for name, re := range exprs {
		if !newWindowed(nil, re).whole {
			windowedOnes++
		}
		_ = name
	}
	if windowedOnes < 10 {
		t.Fatalf("only %d of %d expressions are read by windows", windowedOnes, len(exprs))
	}
	for i := range 160 {
		if !raceKeeps("windowed", i) {
			continue
		}
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			t.Parallel()
			s := windowText(rand.New(rand.NewPCG(873, uint64(i)))) // #nosec G404 G115 -- reproducible generated texts; i is below 160
			for _, name := range windowExprNames(exprs, i) {
				re := exprs[name]
				w := newWindowed(nil, re)
				if got, want := w.findAll(nil, s), re.FindAllStringSubmatchIndex(s, -1); !reflect.DeepEqual(got, want) {
					t.Errorf("%s (%q): windows found %v, the whole text %v in %q", name, re.String(), got, want, s)
				}
			}
		})
	}
}

// The characters around a window decide the assertions at its edges: the same long run after and before a line
// break, a word character, a look-alike or an invalid byte, at the start and the end of the text, is found as the
// whole text finds it.
func TestWindowedEdges(t *testing.T) {
	run := strings.Repeat("aws_secret_access_key", windowMinRun/10)
	around := []string{"", "\n", "x", " ", "\t", ":", "\ufe55", "\xbf", "\u212a", "\"", "- ", "\r\n", "=", ": |\n  v"}
	t.Parallel()
	exprs := windowExprs(t)
	for i, before := range around {
		t.Run(fmt.Sprintf("%q", before), func(t *testing.T) {
			t.Parallel()
			for j, after := range around {
				if !raceKeeps("windowed edges", i*len(around)+j) {
					continue
				}
				s := before + run + after + ": v\n" + strings.Repeat(before+run+after+"\n", windowMinText/len(run)+1) + run
				for name, re := range exprs {
					w := newWindowed(nil, re)
					if got, want := w.findAll(nil, s), re.FindAllStringSubmatchIndex(s, -1); !reflect.DeepEqual(got, want) {
						t.Errorf("%s around %q and %q: windows found %v, the whole text %v", name, before, after, got, want)
					}
				}
			}
		})
	}
}

// Windows read in one text are taken in the next only where they are read alike: a long run read alone, at the
// start of a value, and then in an answer after other characters (as Pieces reads a value and its answer with one
// windowIDs) is found as each text read whole finds it.
func TestWindowedSharedAcrossTexts(t *testing.T) {
	t.Parallel()
	run := strings.Repeat("aws_secret_access_key", windowMinText/20)
	exprs := windowExprs(t)
	for ti, tail := range []string{"\ufe55\ufeff\xbf\u200b", ":", ": |\n  v", "=x", "\n", ""} {
		texts := make([]string, 0, 11)
		texts = append(texts, run+tail)
		for _, lead := range []string{"{", "\n", "x", " ", ",", "- ", "\ufe55", "\"", "password: |\n"} {
			texts = append(texts, strings.Repeat("tmpl"+lead+run+tail+"=\"\",pod=\"p\"} 1\n", 3))
		}
		texts = append(texts, run+tail)
		for _, name := range windowExprNames(exprs, ti) {
			re := exprs[name]
			w := newWindowed(nil, re)
			ids := &windowIDs{}
			for _, s := range texts {
				if got, want := w.findAllWith(nil, s, ids), re.FindAllStringSubmatchIndex(s, -1); !reflect.DeepEqual(got, want) {
					t.Errorf("%s on %.80q after the texts before: windows found %v, the whole text %v", name, s, got, want)
				}
			}
		}
	}
}

// A long run repeated in a text is read once: every expression whose alphabet ends a window where the label of
// FuzzDiffBuild input 81e201e983440dcc ends (its look-alike colon, outside the alphabet of a key name) and before
// it starts reads eight copies of that label, each on its own metric line, with at most twice the bytes of one.
// The expressions of the key names are among them; one whose alphabet holds the rest of the line reads each line.
func TestWindowedReadsARepeatedRunOnce(t *testing.T) {
	label := strings.Repeat("aws_secret_access_key", 1950) + "\ufe55\ufeff\xbf\u200b"
	var b strings.Builder
	for i := range 8 {
		fmt.Fprintf(&b, "template_%d{%s=\"\",pod=\"web-%d\"} 1\n", i, label, i)
	}
	answer := b.String()
	keyRules := 0
	for name, re := range windowExprs(t) {
		w := newWindowed(nil, re)
		if w.whole || w.alpha.in(nil, '\ufe55') || w.later.in(nil, '{') {
			continue
		}
		keyRules++
		bytesRead := 0
		w.onRead = func(n int) { bytesRead += n }
		w.findAll(nil, answer)
		if bytesRead > 2*len(label)+windowMinText {
			t.Errorf("%s: the windowed reading of eight copies read %d bytes with the expression, over twice the %d of one",
				name, bytesRead, len(label))
		}
	}
	if keyRules < 5 {
		t.Errorf("only %d expressions end their windows around the label", keyRules)
	}
}

// A window that begins with a start character after a character of the alphabet is read with that character's
// class (line break, word character, neither): expressions that assert a word boundary or a line start before a
// start character find on each text, alone and after others with one windowIDs, what the whole text finds.
func TestWindowedLeadOfAStartCharacter(t *testing.T) {
	t.Parallel()
	exprs := []*regexp.Regexp{
		regexp.MustCompile(`\b,x+`), regexp.MustCompile(`\B,x+`), regexp.MustCompile(`(?m)^,x+`),
		regexp.MustCompile(`(?m)^x+,?`), regexp.MustCompile(`\Ax+`), regexp.MustCompile(`,x+\b`), regexp.MustCompile(`,x+$`),
	}
	long := strings.Repeat("x", windowMinText)
	texts := make([]string, 0, 48)
	for _, lead := range []string{"", "x", "\n", " ", ",", "y"} {
		for _, tail := range []string{"", "\n", "y", ","} {
			texts = append(texts, lead+","+long+tail, strings.Repeat(lead+","+long+tail, 3))
		}
	}
	for _, re := range exprs {
		w := newWindowed(nil, re)
		if w.whole {
			t.Fatalf("%s is read whole", re)
		}
		ids := &windowIDs{}
		for _, s := range texts {
			if got, want := w.findAllWith(nil, s, ids), re.FindAllStringSubmatchIndex(s, -1); !reflect.DeepEqual(got, want) {
				t.Errorf("%s on %.40q (%d bytes): windows found %v, the whole text %v", re, s, len(s), got, want)
			}
		}
	}
}

// shortWindowLabel is the label name of FuzzDiffBuild input 22905eb52eae1385 (#873, third round): a unit of short
// key words, separators and values, repeated to n bytes and more, so that no expression finds a long window in it.
func shortWindowLabel(n int) string {
	const unit = "TOKEN=\"a\",b,DB_PASSWORD: |+\n  SECRETPROBE0001"
	return strings.Repeat(unit, n/len(unit)+1)
}

// chunkText generates a label made of short windows (fragments of keys, separators, values, look-alike, invisible
// and invalid characters, repeated as a unit or not) and an answer that writes it, or a copy with one fragment
// changed, on several lines with other characters around it, as the metric lines of a context do.
func chunkText(rng *rand.Rand) (label, answer string) {
	frags := append(slices.Clone(windowKeys), ":", "=", ": ", "=\"", "\"", "'", ",", "{", "}", "[", "]", " ", "\t",
		"\n", "\r\n", "  ", "- ", "|+\n  ", ">", "#", "?", "&", "@", `\"`, `\n`, "x://u:", "\ufe55", "\uff1a", "\ufeff",
		"\u200b", "\xbf", "\u00e9", "\u2028", "a", "b", "0001", "10.1.2.3", "fe80::1", "eyJhbGciOi")
	pick := func() string { return frags[rng.IntN(len(frags))] }
	var unit strings.Builder
	for range 3 + rng.IntN(12) {
		unit.WriteString(pick())
	}
	var b strings.Builder
	size := windowMinText + rng.IntN(3*windowMinText)
	for b.Len() < size {
		if rng.IntN(3) == 0 {
			b.WriteString(pick()) // a text that is not one unit repeated
			continue
		}
		b.WriteString(unit.String())
	}
	label = b.String()
	var a strings.Builder
	for i := range 2 + rng.IntN(7) {
		l := label
		if rng.IntN(4) == 0 {
			at := rng.IntN(len(l))
			l = l[:at] + pick() + l[at:]
		}
		fmt.Fprintf(&a, "template_%d{%s%s=\"%s\",pod=\"web-%d\"} 1\n", i, pick(), l, pick(), i)
	}
	return label, a.String()
}

// Every expression read by windows finds in the labels of short windows and their answers what it finds read whole,
// read alone and with one windowIDs for the label and then its answer (as Pieces reads a value and its answer).
func TestWindowedChunksFindWhatTheExpressionFinds(t *testing.T) {
	exprs := windowExprs(t)
	for i := range 160 {
		if !raceKeeps("windowed chunks", i) {
			continue
		}
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			t.Parallel()
			label, answer := chunkText(rand.New(rand.NewPCG(8733, uint64(i)))) // #nosec G404 G115 -- reproducible generated texts; i is below 160
			for _, name := range windowExprNames(exprs, i) {
				re := exprs[name]
				w := newWindowed(nil, re)
				ids := &windowIDs{}
				for _, s := range []string{label, answer, answer + label} {
					if got, want := w.findAllWith(nil, s, ids), re.FindAllStringSubmatchIndex(s, -1); !reflect.DeepEqual(got, want) {
						t.Errorf("%s (%q) on %d bytes: windows found %v, the whole text %v in %q", name, re.String(), len(s), got, want, s)
					}
					if got, want := w.findAll(nil, s), re.FindAllStringSubmatchIndex(s, -1); !reflect.DeepEqual(got, want) {
						t.Errorf("%s (%q) alone on %d bytes: windows found %v, the whole text %v", name, re.String(), len(s), got, want)
					}
				}
			}
		})
	}
}

// A label of short windows repeated in a text is read about once (#873, third round): every expression read by
// windows reads eight copies of the label of FuzzDiffBuild input 22905eb52eae1385, each on its own metric line,
// with at most twice the bytes of one, also when the label was read alone first. An expression whose alphabet holds
// every character of a line and the line break (one window over all the lines, which no reading can cut) is left
// out.
func TestWindowedReadsARepeatedLabelOfShortWindowsOnce(t *testing.T) {
	label := shortWindowLabel(40 << 10)
	var b strings.Builder
	for i := range 8 {
		fmt.Fprintf(&b, "template_%d{%s=\"\",pod=\"web-%d\"} 1\n", i, label, i)
	}
	answer := b.String()
	read := 0
	for name, re := range windowExprs(t) {
		w := newWindowed(nil, re)
		if w.whole || slices.ContainsFunc(longRuns(nil, answer, []*windowed{w})[0], func(r longRun) bool { return r.end-r.start > len(label) }) {
			continue
		}
		read++
		bytesRead := 0
		w.onRead = func(n int) { bytesRead += n }
		w.findAll(nil, answer)
		if bytesRead > 2*len(label)+windowMinText {
			t.Errorf("%s: eight copies of a label of short windows read %d bytes with the expression, over twice the %d of one",
				name, bytesRead, len(label))
		}
		bytesRead = 0
		ids := &windowIDs{}
		w.findAllWith(nil, label, ids)
		w.findAllWith(nil, answer, ids)
		if bytesRead > 2*len(label)+windowMinText {
			t.Errorf("%s: the label alone and then eight copies read %d bytes with the expression, over twice the %d of one",
				name, bytesRead, len(label))
		}
	}
	if read < 10 {
		t.Errorf("only %d expressions are read by windows", read)
	}
}
