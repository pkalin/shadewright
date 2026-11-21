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
	"bytes"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/pkalin/shadewright/internal/redact/redacttest"
	"github.com/pkalin/shadewright/internal/redact/released"
)

// The differential fuzz targets of package redact (#845, fifteenth decision of 2026-11-18), one per entry point:
// String, StringTwice, Strings and ReplaceFrom. Each decodes its input with redacttest.Decode (a grammar of the
// pieces the findings of #845 use, or the raw bytes) and checks, by name:
//
//	P1: every probe secret the redactor of the release before (package released) hid, applied as origin/main
//	    applied it at that entry point, is hidden by the branch.
//	P2: what String hides in the strings joined by line breaks, Strings hides in them apart (Strings only).
//	P3: the work of the entry point, the release's reading included, stays inside the budget of the entry
//	    (budget.go, #873 decision of 2026-11-20), within one counting step (budgetOverrun) (only with -tags
//	    redactwork; without it the property is not checked).
//	P5: the output is the decision without a budget or the entry withheld whole (Withheld, the other strings
//	    empty), never a part of it. A withheld entry shows nothing, so P1 holds of it without the release's
//	    reading, which is then not run: on the inputs where its own work outgrows the text ("--token,}" repeated,
//	    released.tokenEnd) that oracle alone would hold a fuzzing worker past its limit.
//	P6: no entry of an input under redacttest.SmallInput (64 KiB) is withheld for work (#873, decision of
//	    2026-11-20). P3 cannot find a shape whose work grows faster than the text, since the budget stops the work
//	    where P3 would see it pass and P5 accepts the entry withheld; such a shape is withheld at a size the
//	    budget's factor makes small, so P6 fails it and the fuzzer keeps it. It needs no build tag (it reads the
//	    counter shadewright_redaction_withheld_total).
//	P4: the output is valid UTF-8 when the input is, and nothing panics. Valid UTF-8 whatever the input is a
//	    property of the tool results and the incident context (decided 2026-11-19): these entry points keep the
//	    bytes they do not hide, invalid ones included, as the release's String does (released.String("a\xffb")
//	    returns it unchanged).
//
// The inputs known to fail (knownDiffFailures) are seeds in testdata/fuzz: they skip, naming the property, until
// the rule is fixed, and fail once they pass, so that the fix removes them.

// knownDiffFailures are the known failing inputs of the targets of this package, by target and input bytes:
// shapes on which the reading of the release before (package released) grows faster than the text, so the budget
// withholds them under redacttest.SmallInput (P6).
var knownDiffFailures = map[string]map[string]redacttest.KnownFailure{
	"FuzzDiffString":      {string(seedTokenRepeat): knownTokenRepeat},
	"FuzzDiffStringTwice": {string(seedTokenRepeat): knownTokenRepeat},
	"FuzzDiffStrings": {
		string(seedTokenRepeat):           knownTokenRepeat,
		string(seedPairsBeforeABlankLine): knownPairsBeforeABlankLine,
	},
	"FuzzDiffReplaceFrom": {
		string(seedTokenRepeat): knownTokenRepeat,
		// found-845-0fe50aa1ef3f463b in testdata/fuzz: "-password," repeated, 10,250 bytes.
		"10\x060\x052<12": knownTokenRepeat,
	},
}

// knownTokenRepeat is a flag and a comma repeated ("--token,}", "-password,"): the release's reading reads each
// value to the end of the run (released.tokenEnd), about 950 steps per byte at 4 KiB and more with the size (#962).
var knownTokenRepeat = redacttest.KnownFailure{Property: "P6", Reason: "#962, the release's reading of a flag repeated"}

// knownPairsBeforeABlankLine is TOKEN="a" repeated before a long blank line, as Strings reads it: the release's
// reading takes 53, 237 and 868 steps per byte at 3, 6 and 13 KB (measured 2026-11-20 with -tags redactwork, #962).
// The steps per byte grow about four times for twice the text, so the total work about eight times (9 times from
// 3 to 6 KB, 7.9 times from 6 to 13 KB): faster than the square of the text over these sizes.
var knownPairsBeforeABlankLine = redacttest.KnownFailure{
	Property: "P6", Reason: "#962, the release's reading of key pairs before a long blank line",
}

// seedTokenRepeat is "--token,}" repeated to 36 KiB, a seed of diffSeeds.
var seedTokenRepeat = redacttest.RawSeed(0, strings.Repeat("--token,}", 4096))

// checkDiff ends a fuzz input of target ([redacttest.Check]).
func checkDiff(t *testing.T, target string, data []byte, counted bool, fails []string) {
	t.Helper()
	redacttest.Check(t, knownDiffFailures[target], data, counted, fails)
}

// releasedTwice is the release's reading where origin/main redacted a text, made it valid UTF-8 and redacted it
// again (incidentcontext.Redact at origin/main 87e96974), each pass recorded.
func releasedTwice(rec *redacttest.Recorder) func(string) string {
	once := rec.Wrap(released.String)
	return func(s string) string { return once(strings.ToValidUTF8(once(s), "?")) }
}

// leaks returns the P1 failure for out, or nothing.
func leaks(rec *redacttest.Recorder, what, out string) []string {
	return redacttest.LeakFails(rec, what, out)
}

// workFails returns the P3 failure for work of total steps on an entry of size bytes.
func workFails(what string, total, _ int64, size int) []string {
	if limit := budgetOf(size) + budgetOverrun(size); total > limit {
		return []string{fmt.Sprintf("P3: %s took %d steps on %d bytes (%.0f per byte), over its budget %d",
			what, total, size, float64(total)/float64(max(size, 1)), budgetOf(size))}
	}
	return nil
}

// withheldFails returns the P5 failure of out, an entry point's output on in, and ok, its flag: an entry withheld
// (ok false) is Withheld and nothing else (withheldStrings), and an entry decided (ok true) holds Withheld only
// where in spells it, so a caller that reads the flag never sees part of an entry withheld (#873).
func withheldFails(what, in string, out []string, ok bool) []string {
	if !ok {
		if !slices.Equal(out, withheldStrings(len(out))) {
			return []string{fmt.Sprintf("P5: %s withheld the entry but returned %q", what,
				redacttest.Clip(strings.Join(out, "|")))}
		}
		return nil
	}
	if strings.Contains(in, Withheld) {
		return nil
	}
	for _, s := range out {
		if strings.Contains(s, Withheld) {
			return []string{fmt.Sprintf("P5: %s decided the entry and returned the withheld marker: %q", what,
				redacttest.Clip(strings.Join(out, "|")))}
		}
	}
	return nil
}

// utf8Fails returns the P4 failure when out is not valid UTF-8 and in is.
func utf8Fails(what, in, out string) []string {
	if utf8.ValidString(in) && !utf8.ValidString(out) {
		return []string{fmt.Sprintf("P4: %s is not valid UTF-8: %q", what, redacttest.Clip(out))}
	}
	return nil
}

// seedTokenRepeatGrammar is seed 4 of the fifteenth decision written in the grammar: TOKEN="a",b, repeated to
// the size limit, a format character after it.
var seedTokenRepeatGrammar = []byte{1, 0, 21, 12, 0, 7, 2, 0}

// diffSeeds are grammar inputs every target of this file starts from, besides its corpus in testdata/fuzz.
var diffSeeds = [][]byte{
	{1, 0, 14, 0, 1},                  // a key, a separator and a probe
	{1, 0, 15, 0, 0, 0, 3},            // a block scalar under a key
	{1, 1, 18, 2},                     // a flag and its value in the next string
	{1, 1, 19, 0, 6, 3},               // a key word, a look-alike colon and a key word, the probe next
	{1, 0, 20, 0, 0},                  // a private key header and a secret line
	seedTokenRepeatGrammar,            // TOKEN="a",b, repeated, a format character after it
	{1, 0, 23, 0, 0, 7, 0},            // a key, a format character, a look-alike colon, a probe
	{1, 2, 17, 2, 3, 11, 9, 16, 1, 1}, // an explicit key, then a flow map in the next string
	redacttest.RawSeed(0, "password: \x01\n"),
	redacttest.RawSeed(1, "192.168.1.1--password", "\x01"),
	seedPairsBeforeABlankLine, // TOKEN="a" repeated, a quote, a line end, 4,096 blanks, a quote and a fullwidth colon
	{0xf1, 0, 242, 0, 7, 0},   // a run of one invalid byte and an "x", 3,150 times
	// "--token,}" repeated to 36 KiB (#873): the release's reading reads each value to the end of the run
	// (released.tokenEnd), about 4,500 steps per byte there, so the entry is withheld (P3, P5).
	seedTokenRepeat,
}

// seedPairsBeforeABlankLine is the input of TestStringWorkOnRepeatedPairsBeforeALongBlankLine in the grammar (the
// first operation added later, redacttest.extendedOp): 1,024 times TOKEN="a", then a quote, a line end, a line of
// 4,096 blanks and a closing quote with a fullwidth colon after it.
var seedPairsBeforeABlankLine = []byte{0xf1, 0, 240, 8, 5, 7, 7}

// diffBodies are the checks of the fuzz targets of this file, by target: the failures of one input, and whether
// P3 was checked. TestMinimizeCollected runs them too.
var diffBodies = map[string]func(data []byte) (fails []string, counted bool){
	"FuzzDiffString":      diffString,
	"FuzzDiffStringTwice": diffStringTwice,
	"FuzzDiffStrings":     diffStrings,
	"FuzzDiffReplaceFrom": diffReplaceFrom,
}

// fuzzDiff runs the target of that name over its seeds and the fuzzer's inputs.
func fuzzDiff(f *testing.F, target string) {
	for _, s := range diffSeeds {
		f.Add(s)
	}
	body := diffBodies[target]
	f.Fuzz(func(t *testing.T, data []byte) {
		fails, counted := body(data)
		checkDiff(t, target, data, counted, fails)
	})
}

// FuzzDiffString checks String against the release's String on the same text (P1, P3, P4).
func FuzzDiffString(f *testing.F) { fuzzDiff(f, "FuzzDiffString") }

func diffString(data []byte) ([]string, bool) {
	in := redacttest.Decode(data, 1).Joined("")
	var out string
	var ok bool
	before := redacttest.WorkWithheld()
	total, release, counted := measureWork(func() { out, ok = StringChecked(in) })
	fails := workFails("String", total, release, len(in))
	fails = append(fails, redacttest.SmallWithheldFails("String", len(in), before)...)
	if fails = append(fails, withheldFails("String", in, []string{out}, ok)...); !ok {
		return fails, counted
	}
	rec := redacttest.NewRecorder()
	rec.Wrap(released.String)(in)
	fails = append(fails, leaks(rec, "String", out)...)
	fails = append(fails, utf8Fails("String", in, out)...)
	return fails, counted
}

// FuzzDiffStringTwice checks StringTwice against the release's reading where origin/main redacted a text twice
// (P1, P3, P4).
func FuzzDiffStringTwice(f *testing.F) { fuzzDiff(f, "FuzzDiffStringTwice") }

func diffStringTwice(data []byte) ([]string, bool) {
	in := redacttest.Decode(data, 1).Joined("")
	var out string
	var ok bool
	before := redacttest.WorkWithheld()
	total, release, counted := measureWork(func() { out, ok = StringTwiceChecked(in, nil) })
	fails := workFails("StringTwice", total, release, len(in))
	fails = append(fails, redacttest.SmallWithheldFails("StringTwice", len(in), before)...)
	if fails = append(fails, withheldFails("StringTwice", in, []string{out}, ok)...); !ok {
		return fails, counted
	}
	rec := redacttest.NewRecorder()
	releasedTwice(rec)(in)
	fails = append(fails, leaks(rec, "StringTwice", out)...)
	fails = append(fails, utf8Fails("StringTwice", in, out)...)
	return fails, counted
}

// FuzzDiffStrings checks Strings, the strings of a sequence (a container's arguments), against the release's
// reading of them as the read tools of origin/main applied it (each string twice, then the strings joined twice:
// releasedPieces), and against String of the strings joined by line breaks (P1, P2, P3, P4). The knob gives the
// number of strings, one to six.
func FuzzDiffStrings(f *testing.F) { fuzzDiff(f, "FuzzDiffStrings") }

func diffStrings(data []byte) ([]string, bool) {
	dec := redacttest.Decode(data, 6)
	items := dec.Fields[:int(dec.Knob)%6+1]
	joined := strings.Join(items, "\n")
	var out []string
	var ok bool
	before := redacttest.WorkWithheld()
	total, release, counted := measureWork(func() { out, ok = StringsChecked(items) })
	fails := workFails("Strings", total, release, stringsSize(items))
	fails = append(fails, redacttest.SmallWithheldFails("Strings", stringsSize(items), before)...)
	if len(out) != len(items) {
		fails = append(fails, fmt.Sprintf("P4: Strings returned %d strings for %d", len(out), len(items)))
	}
	if fails = append(fails, withheldFails("Strings", joined, out, ok)...); !ok {
		return fails, counted
	}
	rec := redacttest.NewRecorder()
	twice := releasedTwice(rec)
	each := make([]string, len(items))
	for i, s := range items {
		each[i] = twice(s)
	}
	twice(strings.Join(each, "\n"))
	apart := strings.Join(out, "\n")
	fails = append(fails, leaks(rec, "Strings", apart)...)
	if l := redacttest.Hiding(joined, String(joined)).Leaks(apart); len(l) > 0 {
		fails = append(fails, redacttest.LeakLine("P2", "Strings shows what String of the strings joined hides", l, apart))
	}
	fails = append(fails, utf8Fails("Strings", joined, apart)...)
	return fails, counted
}

// FuzzDiffReplaceFrom checks the excerpt of a log (ReplaceFrom, as the log source calls it in
// internal/providers/logs/kube Excerpt) against the excerpt origin/main made (P1, P3, P4): the release's Replace
// over the whole text with the part before the cut dropped, then the release's String over the excerpt. The knob
// gives the bytes kept: 64 times one more than it.
func FuzzDiffReplaceFrom(f *testing.F) { fuzzDiff(f, "FuzzDiffReplaceFrom") }

func diffReplaceFrom(data []byte) ([]string, bool) {
	dec := redacttest.Decode(data, 1)
	in := dec.Fields[0]
	from := excerptFrom([]byte(in), 64*(int(dec.Knob)+1))
	var out string
	ok := true
	before := redacttest.WorkWithheld()
	total, release, counted := measureWork(func() {
		out = strings.ToValidUTF8(excerptOf(in, from, defaultRedactor.replaceFromFunc(from, &ok), Marker), "?")
	})
	fails := workFails("ReplaceFrom", total, release, len(in))
	fails = append(fails, redacttest.SmallWithheldFails("ReplaceFrom", len(in), before)...)
	if fails = append(fails, withheldFails("the excerpt", in, []string{out}, ok)...); !ok {
		return fails, counted
	}
	rec := redacttest.NewRecorder()
	mainOut := released.String(strings.ToValidUTF8(excerptOf(in, from, released.Replace, released.Marker), "?"))
	// What the release hid is what its excerpt does not show of the part it kept.
	rec.Saw(in[from:], mainOut)
	fails = append(fails, leaks(rec, "the excerpt", out)...)
	fails = append(fails, utf8Fails("the excerpt", strings.ToValidUTF8(in, "?"), out)...)
	return fails, counted
}

// TestMinimizeCollected makes the inputs a collecting run wrote (redacttest.CollectEnv) small, when the
// environment variable FUZZ845_MINIMIZE names that directory, and prints each with its signature, and the
// signature of each known failure, so that a run can be counted by kind.
func TestMinimizeCollected(t *testing.T) {
	redacttest.MinimizeCollected(t, diffBodies, knownDiffFailures)
}

// replaceFromFunc is ReplaceFrom with from fixed, in the shape of Replace; it sets *ok to ReplaceFrom's flag.
func (r *Redactor) replaceFromFunc(from int, ok *bool) func(string, func(kind, text string) string, func(string) string) string {
	return func(s string, repl func(kind, text string) string, between func(string) string) string {
		out, decided := r.ReplaceFrom(s, from, nil, repl, between)
		*ok = decided
		return out
	}
}

// excerptFrom is where the log source's cut starts keeping data: the newest whole lines in keep bytes (cut in
// internal/providers/logs/kube, unchanged since origin/main 87e96974).
func excerptFrom(data []byte, keep int) int {
	if len(data) <= keep {
		return 0
	}
	from := len(data) - keep
	if data[from-1] != '\n' {
		i := bytes.IndexByte(data[from:], '\n')
		if i < 0 {
			return len(data)
		}
		from += i + 1
	}
	return from
}

// excerptOf is the replacement of the log source's Excerpt (internal/providers/logs/kube/kube.go at origin/main
// 87e96974): each match and each text between matches keeps only its part from from on, a match by the marker
// of that part.
func excerptOf(s string, from int, replace func(string, func(kind, text string) string, func(string) string) string,
	marker func(string, int) string) string {
	pos := 0
	part := func(n int, f func(start int) string) string {
		start := pos
		pos += n
		if pos <= from {
			return ""
		}
		return f(max(from-start, 0))
	}
	return replace(s,
		func(kind, m string) string {
			return part(len(m), func(skip int) string { return marker(kind, len(m)-skip) })
		},
		func(seg string) string { return part(len(seg), func(skip int) string { return seg[skip:] }) })
}

// The known failures of this package, as directed tests: each skips while it fails, naming the property.

// Strings hides a secret on the line after a private key header when the header is in the string before, as
// String of the strings joined hides it (#845, fifteenth decision, input 2).
func TestStringsHidesTheLineAfterAPrivateKeyHeader(t *testing.T) {
	const secret = "hunter2"
	items := []string{"-----BEGIN PRIVATE KEY-----", "\n" + secret}
	joined := strings.Join(items, "\n")
	if l := String(joined); strings.Contains(l, secret) {
		t.Fatalf("String of the joined strings shows the secret: %q", l)
	}
	if out := Strings(items); strings.Contains(strings.Join(out, "\n"), secret) {
		t.Error(redacttest.LeakLine("P2", "Strings shows what String of the strings joined hides", []string{secret}, strings.Join(out, "\n")))
	}
}

// One format character after 1,024 repeats of TOKEN="a",b, made String add more than ownWorkPerByte per byte
// to the release's reading (#845, fifteenth decision, input 4): each key read its value as the shell word to its
// end, maxValueExtent bytes, again.
func TestStringWorkAfterRepeatedTokens(t *testing.T) {
	in := strings.Repeat(`TOKEN="a",b,`, 1024) + "\nx\u200b"
	checkOwnWork(t, "String", in, func() { String(in) })
}

// Strings of seed 4 in the grammar (TOKEN="a",b, repeated to the size limit), which reads the strings joined, cost
// about 1,382 own steps per byte.
func TestStringsWorkAfterRepeatedTokens(t *testing.T) {
	fails, counted := diffStrings(seedTokenRepeatGrammar)
	if !counted {
		t.Skip("redaction work is counted only with the build tag redactwork")
	}
	for _, f := range fails {
		t.Error(f)
	}
}

// Found by FuzzDiffStringTwice in its ten minute run (#845, fifteenth decision): TOKEN":"<probe>TOKEN: repeated
// on one line of 40,948 bytes with no blank. Each quoted key read its one-word value to the first blank of the rest
// of the line before it cut it at the next key, so the own work grew with the square of the line (259, 435 and
// 788 steps per byte at a quarter, a half and all of it).
func TestStringTwiceWorkOnALineWithNoBlank(t *testing.T) {
	fails, counted := diffStringTwice([]byte("10&00001Blr{"))
	if !counted {
		t.Skip("redaction work is counted only with the build tag redactwork")
	}
	for _, f := range fails {
		t.Error(f)
	}
}

// Found by FuzzDiffReplaceFrom in its three and ten minute runs (#845, fifteenth decision): "-password,"
// repeated, 10,250 bytes with no blank. Each flag read its value, the rest of the run, to the first blank, so the
// own work grew with the square of the run (1,060 steps per byte here). The release's reading of it still grows
// so (#962): the budget withholds it, the known P6 failure of knownDiffFailures, and any other property fails.
func TestReplaceFromWorkOnGluedFlags(t *testing.T) {
	data := []byte("10\x060\x052<12")
	fails, counted := diffReplaceFrom(data)
	if !counted {
		t.Skip("redaction work is counted only with the build tag redactwork")
	}
	checkDiff(t, "FuzzDiffReplaceFrom", data, counted, fails)
}

// checkOwnWork fails when f, the redaction of in by what, adds more than ownWorkPerByte per byte of in to the
// release's reading. It skips without the counter (the build tag redactwork).
func checkOwnWork(t *testing.T, what, in string, f func()) {
	t.Helper()
	total, release, counted := measureWork(f)
	if !counted {
		t.Skip("redaction work is counted only with the build tag redactwork")
	}
	t.Logf("%s on %d bytes: the rest %.1f steps per byte", what, len(in), perByte(total-release, len(in)))
	for _, fail := range workFails(what, total, release, len(in)) {
		t.Error(fail)
	}
}

// Found by FuzzDiffStringTwice and FuzzDiffString (#845, fifteenth decision, the first runs of the fuzzer): the
// unit TOKEN="a",b, with a look-alike colon (U+02D0) after it, repeated. Both String and the reading of the
// release before grow with the square of the input here (own work 2,789, 10,782 and 42,568 steps per byte at
// 64, 128 and 256 repeats; the release's reading 1,442, 5,588 and 22,072), far past seed 4's 1,205. With the
// memory of the shell words read (wordMemo) the rule's own work is about 70 steps per byte at any size.
func TestStringWorkOnTokensWithALookAlikeColon(t *testing.T) {
	for _, n := range []int{128, 512, 2048} {
		in := strings.Repeat("TOKEN=\"a\",b,\u02d0", n)
		checkOwnWork(t, "String", in, func() { String(in) })
	}
}

// TOKEN="a" repeated, then a double quote, a line break, a line of 4,096 blanks and a closing quote with a
// fullwidth colon after it: about 1,208 own steps per byte. Every key within maxValueExtent of the last quote
// read its value as one shell word up to that quoted group, whose end depends on the key's column, so the memory
// of the shell words (wordMemo) went on from the group and each key read the group again.
func TestStringWorkOnRepeatedPairsBeforeALongBlankLine(t *testing.T) {
	in := strings.Repeat(`TOKEN="a"`, 1000) + "\"\n" + strings.Repeat(" ", 4096) + "\"："
	checkOwnWork(t, "String", in, func() { String(in) })
}

// The class of TestStringWorkOnRepeatedPairsBeforeALongBlankLine through the grammar (redacttest.extendedOp): key
// forms, counts, lengths of the blank line and separators, each through every target of this file, whose oracles
// (the release's reading for P1, String joined for P2, the work bound for P3, valid UTF-8 for P4) give the answers.
func TestDiffPairsBeforeABlankLine(t *testing.T) {
	if raceDetector {
		t.Skip("the inputs of 13 KB are read under -race by the seeds of the fuzz targets")
	}
	for _, key := range []byte{8, 0} { // TOKEN, password
		for _, n := range []byte{5} { // 1,024 repeats
			for _, m := range []byte{7, 6} { // 4,096 and 2,047 blanks
				for _, sep := range []byte{7, 0} { // fullwidth colon, ":"
					data := []byte{0xf1, 0, 240, key, n, m, sep}
					for _, target := range slices.Sorted(maps.Keys(diffBodies)) {
						fails, _ := diffBodies[target](data)
						// P6 is the known failure of this class (knownPairsBeforeABlankLine): the release's
						// reading grows about eight times per doubling of the text, faster than its square, and the
						// budget withholds it.
						fails = slices.DeleteFunc(fails, func(f string) bool { return strings.HasPrefix(f, "P6:") })
						if len(fails) > 0 {
							t.Errorf("%s(%v): %s", target, data, strings.Join(fails, "\n"))
						}
					}
				}
			}
		}
	}
}
