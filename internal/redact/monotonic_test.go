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
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/pkalin/shadewright/internal/redact/testdata/mainredact"
)

// The wider value extents of #769 are monotonic against the matcher on main (decision of 2026-11-16 on #769):
// each byte of the input that main's redactor (testdata/mainredact, a copy of commit 28a8212c) removes is
// removed here too. Redacting more is allowed and pinned by the over-redaction tests; redacting less fails.

// lostMainBytes returns the first byte of in that main's redactor removes and this one keeps, or -1.
func lostMainBytes(in string) int {
	now := defaultRedactor.spans(nil, in)
	for _, want := range mainredact.Spans(in) {
		for i := want[0]; i < want[1]; i++ {
			if !coveredBy([2]int{i, i + 1}, now) {
				return i
			}
		}
	}
	return -1
}

// hiddenKeyInputs are inputs where a value extent widened by #769 ran over the next secret key, so that key was
// never matched and its value stayed, while main redacts it whole.
var hiddenKeyInputs = []string{
	`TOKEN="abc",/DB_PASSWORD= "hidden word"`,
	`TOKEN="abc"\--password: "hidden word"`,
	`TOKEN=ab"cd DB_PASSWORD= "hidden word"`,
	`token=a"b DB_PASSWORD="hidden secret words" next=x`,
	`msg=x token=a"b secret="Hunter two three"`,
	`token=x'y apiKey='hidden word' z`,
	`token=x"y password: "hidden word" z`,
	`{password: [&a "x], token: "hidden] secret words"}`,
	`{password: [!!str "x], token: "hidden] secret words"}`,
}

// The values of hiddenKeyInputs are redacted whole: no word of them is left.
func TestNoWiderExtentHidesAKey(t *testing.T) {
	for _, in := range hiddenKeyInputs {
		out := String(in)
		for _, w := range []string{hiddenWord, " word", "words", "Hunter", "three"} {
			if strings.Contains(in, w) && strings.Contains(out, w) {
				t.Errorf("String(%q) = %q, %q is still there", in, out, w)
			}
		}
		if i := lostMainBytes(in); i >= 0 {
			t.Errorf("main removed byte %d of %q, now kept: %q", i, in, String(in))
		}
	}
}

// hiddenWord, nlIndent and nlDash are pieces the corpus uses more than once.
const (
	hiddenWord = "hidden"
	nlIndent   = "\n  "
	nlDash     = "\n- "
)

// The pieces of a monotonicCorpus input: a context around the fields, a first key of any name, two or three
// secret-named keys after it, the separators between a key and its value, the glue between fields, value
// pieces with every quote, bracket, escape, substitution, YAML tag and anchor the extent rule reads, and the
// text after the last field.
var (
	monoContexts = [][2]string{
		{"", ""}, {"{", "}"}, {"[", "]"}, {`msg="`, `"`}, {`{"cmd":"`, `"}`}, {"run ", ""}, {"- ", ""},
		{"args: [", "]"}, {"args:\n- ", "\n"}, {"export ", ""}, {`{\"a\":\"`, `\"}`},
	}
	monoFirstKeys = strings.Fields(`TOKEN token password msg --password -password "password" client_secret apiKey
		x args secret user '--token' \"apiKey\"`)
	monoSecretKeys = strings.Fields(`DB_PASSWORD password token apiKey secret client_secret --password -token
		"password" x-api-key AWS_SECRET_ACCESS_KEY --client-secret 'token' \"password\" pass db.password
		aws_secret_access_key clientSecret`)
	monoSeps = []string{
		"=", "= ", ": ", ":", ":=", " := ", " ", " = ", "=\\\n  ", ":\n  ", ":\n- ", nlDash, ", ", ` "`, "\t",
	}
	monoValuePieces = []string{
		plainABC, "ab", "x", passphraseWords[2], `"`, `'`, "`", "[", "]", "{", "}", "(", ")", "&a ", "!!str ", "!x ", `\`, `\!`,
		`\ `, `\"`, `\\`, "$(", " ", ",", ", ", `"a b"`, `'a b'`, "a=b", "#", "/", ";", "|", "\n", nlIndent,
	}
	monoSecretValues = []string{
		`"hidden word"`, `'hidden word'`, hiddenWord, `"hidden] secret words"`, `"Hunter two three"`,
		hiddenWord + " word", `[hidden, word]`, `{a: hidden word}`, `$(hidden word)`, "`hidden word`", `\"hidden word\"`,
	}
	monoGlues = []string{
		"", " ", ",", ", ", ",/", "/", `\`, ";", "&", "\t", "\n", nlIndent, nlDash, `"`, `" `, "', ", "], ", "}, ",
		"|", ".", "-", "=", ":", "!", "#",
	}
	monoTails = []string{"", nextX, " z", " user=bob", nextLineTail, ", u: x", `", "u": 1`, " evidence"}
)

// monotonicCorpus returns n inputs of two or three key/value fields built from the pieces above with a fixed
// seed. The first field's value is one to three random pieces; each later key is secret-named and its value a
// known secret or random pieces.
func monotonicCorpus(n int) []string {
	rng := rand.New(rand.NewPCG(769, 16)) //nolint:gosec // a fixed seed keeps the corpus the same on every run
	one := func(xs []string) string { return xs[rng.IntN(len(xs))] }
	pieces := func(b *strings.Builder) {
		for range 1 + rng.IntN(3) {
			b.WriteString(one(monoValuePieces))
		}
	}
	out := make([]string, 0, n)
	for range n {
		var b strings.Builder
		ctx := monoContexts[rng.IntN(len(monoContexts))]
		b.WriteString(ctx[0])
		b.WriteString(one(monoFirstKeys))
		b.WriteString(one(monoSeps))
		pieces(&b)
		for range 1 + rng.IntN(2) {
			b.WriteString(one(monoGlues))
			b.WriteString(one(monoSecretKeys))
			b.WriteString(one(monoSeps))
			if rng.IntN(2) == 0 {
				b.WriteString(one(monoSecretValues))
			} else {
				pieces(&b)
			}
		}
		b.WriteString(one(monoTails))
		b.WriteString(ctx[1])
		out = append(out, b.String())
	}
	return out
}

// monotonicCorpusSize is the number of inputs TestMonotonicAgainstMain checks.
const monotonicCorpusSize = 12000

// On every input of the generated corpus, each byte main's redactor removes is removed too.
func TestMonotonicAgainstMain(t *testing.T) {
	parallelUnderRace(t)
	n := monotonicCorpusSize
	switch {
	case raceDetector:
		n /= 32 // the full corpus runs without -race (decision of 2026-11-21 on #873)
	case testing.Short():
		n /= 8
	}
	failed, redacted := 0, 0
	for _, in := range monotonicCorpus(n) {
		if len(mainredact.Spans(in)) > 0 {
			redacted++
		}
		if i := lostMainBytes(in); i >= 0 {
			failed++
			if failed <= 20 {
				t.Errorf("main removed byte %d, now kept:\n in: %q\n now: %q", i, in, String(in))
			}
		}
	}
	if failed > 0 {
		t.Errorf("%d of %d inputs lost bytes main removed", failed, n)
	}
	if redacted < n*3/4 {
		t.Fatalf("corpus too weak: main redacted %d of %d inputs", redacted, n)
	}
}

// FuzzMonotonicAgainstMain checks the same property on fuzzed inputs, seeded with the inputs where a wider
// extent hid a key and with a part of the generated corpus.
func FuzzMonotonicAgainstMain(f *testing.F) {
	for _, in := range hiddenKeyInputs {
		f.Add(in)
	}
	for _, in := range monotonicCorpus(200) {
		f.Add(in)
	}
	f.Fuzz(func(t *testing.T, in string) {
		if i := lostMainBytes(in); i >= 0 {
			t.Fatalf("main removed byte %d of %q, now kept: %q", i, in, String(in))
		}
	})
}

// checkTableInput fails the test when line n (from 1) of the main table at path does not hold input n of the
// corpus its generator gives now: a table written from main checks nothing once the generator moved on, so a
// change to the generator needs the table written again from the commit the table names.
func checkTableInput(t *testing.T, path string, n int, in string, gen []string) {
	t.Helper()
	if n > len(gen) || gen[n-1] != in {
		t.Fatalf("%s line %d does not match its corpus generator: regenerate the table from the commit it names", path, n)
	}
}

// Reading each value twice (with and without the extents #769 widens) keeps the work linear: on inputs that
// repeat each hiddenKeyInputs line to 64 KiB and 1 MiB, the work per byte ([Work]) of the larger input is at
// most that of the smaller one and a little more (sixteen times the work per byte would be quadratic). Below
// about 16 KiB the work per byte still grows, as the extent rule's window (maxValueExtent) fills.
func TestMonotonicStaysLinear(t *testing.T) {
	skipLarge(t)
	for _, line := range hiddenKeyInputs {
		unit := line + " "
		small, smallSize := workString(t, line, rep(unit, (64<<10)/len(unit)))
		large, largeSize := workString(t, line, rep(unit, (1<<20)/len(unit)))
		if float64(large)/float64(largeSize) > 1.125*float64(max(small, 1))/float64(smallSize)+0.5 {
			t.Errorf("%q: %d steps for %d bytes, %d for %d: more than linear", line, small, smallSize, large, largeSize)
		}
	}
}
