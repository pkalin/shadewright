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
	"bufio"
	"encoding/json"
	"math/rand/v2"
	"os"
	"strings"
	"testing"
)

// blanksMainTable is the differential table of #769: blanksCorpus inputs (the contexts of the leaks #769
// closes: a plain item with blanks of a block sequence after a flag item, a quote glued to a plain value, a
// comma after a quoted piece, a backslash at the start of a quoted "--name=value" argument, a tagged quoted
// scalar in an array, an escape glued to a quoted piece), with the byte ranges main redacted (commit
// 28a8212c, before #769). It was written once by running this file in a checkout of that commit with
// BLANKS_MAIN_TABLE set to the output path; regenerate it only from that commit.
const blanksMainTable = "testdata/blanks-main.jsonl"

// blanksPrefixes are the contexts the corpus puts before a value.
var blanksPrefixes = []string{
	"args:\n- --client-secret\n- ", "  args:\n    - --password\n    - ", "--password\r  - ", "--password\n- ",
	"run --password ab", "run --password ", "password=ab", `{"cmd":"--password `, `msg="--password `,
	`TOKEN="abc",`, `password="abc",`, `TOKEN='a b',`, `["--client-secret=\\`, `['--client-secret=\`,
	`{"args":"[\"--client-secret=\\\\`, `{password: [!!str "`, `{password: [&a "`, `{password: [&a !!str '`,
	`[--password, !!str "`, `--client-secret "horse"\`, `--client-secret 'a b'\`, `TOKEN="a"\`,
}

// blanksPieces are the grammar pieces of a value in the corpus.
var blanksPieces = []string{
	passphraseWords[0], passphraseWords[1], "Staple9", " ", " ", ",", ", ", `"`, `'`, "]", "[", "}", "!", "#", "$", "&",
	`\`, `\!`, `\n`, `\"`, "\n", "\n  ", "\n- ", "=", ":", "user=", "!!str ", "&a ",
}

// argRegionTail is the text after a secret argument of a JSON list.
const argRegionTail = `", "--region"]`

// blanksTails are the text after a value in the corpus.
var blanksTails = []string{
	regionWest, nextX, `","reason":"x"}`, `" user=bob`, argRegionTail, listItemTail, "], u: x}",
	`"]`, `"]"}`, "", " EVIDENCE", ",user=bob",
}

// blanksCorpus returns n inputs built from the pieces above with a fixed seed.
func blanksCorpus(n int) []string {
	rng := rand.New(rand.NewPCG(769, 1)) //nolint:gosec // a fixed seed keeps the corpus the same on every run
	out := make([]string, 0, n)
	for range n {
		var b strings.Builder
		b.WriteString(blanksPrefixes[rng.IntN(len(blanksPrefixes))])
		for range 1 + rng.IntN(6) {
			b.WriteString(blanksPieces[rng.IntN(len(blanksPieces))])
		}
		b.WriteString(blanksTails[rng.IntN(len(blanksTails))])
		out = append(out, b.String())
	}
	return out
}

// TestWriteBlanksMainTable writes blanksMainTable from the redactor it runs with, when BLANKS_MAIN_TABLE names
// the output path; otherwise it is skipped. Run it only in a checkout of the commit that blanksMainTable names.
func TestWriteBlanksMainTable(t *testing.T) {
	path := os.Getenv("BLANKS_MAIN_TABLE")
	if path == "" {
		t.Skip("BLANKS_MAIN_TABLE is not set")
	}
	f, err := os.Create(path) //nolint:gosec // a path the person running the test names
	if err != nil {
		t.Fatal(err)
	}
	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)
	for _, in := range blanksCorpus(3000) {
		c := mainRedaction{In: in, Out: String(in)}
		for _, sp := range defaultRedactor.spans(nil, in) {
			c.Spans = append(c.Spans, [2]int{sp.start, sp.end})
		}
		if err := enc.Encode(c); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// Nothing main redacted is lost by #769: on every input of a generated corpus of the contexts #769 changes,
// each byte main redacted is still redacted.
func TestBlanksStillRedactsWhatMainRedacted(t *testing.T) {
	// A corpus test that counts no work: it runs beside the others (#873).
	t.Parallel()
	f, err := os.Open(blanksMainTable)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	gen := blanksCorpus(3000)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<16), 1<<20)
	cases, redacted := 0, 0
	for sc.Scan() {
		var c mainRedaction
		if err := json.Unmarshal(sc.Bytes(), &c); err != nil {
			t.Fatalf("line %d: %v", cases+1, err)
		}
		cases++
		checkTableInput(t, blanksMainTable, cases, c.In, gen)
		if len(c.Spans) > 0 {
			redacted++
		}
		now := defaultRedactor.spans(nil, c.In)
		for _, want := range c.Spans {
			for i := want[0]; i < want[1]; i++ {
				if !coveredBy([2]int{i, i + 1}, now) {
					t.Errorf("main redacted byte %d, now kept:\n in: %q\nmain: %q\n now: %q",
						i, c.In, c.Out, String(c.In))
					break
				}
			}
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if cases != len(gen) {
		t.Fatalf("%s has %d lines, its corpus generator %d inputs", blanksMainTable, cases, len(gen))
	}
	if cases < 3000 || redacted < 2000 {
		t.Fatalf("table too small: %d cases, %d redacted on main", cases, redacted)
	}
}

// words writes one to four words of the value with the separators between them.
func (k *knownValue) words(seps ...string) {
	for i := range 1 + k.rng.IntN(4) {
		if i > 0 {
			k.pick(seps...)
		}
		k.word()
	}
}

// escapedPunct are the characters a shell word escapes with a backslash in the corpus: punctuation only, as
// "\n" or "\t" glued to a quoted piece reads as a JSON escape in a log line.
const escapedPunct = "!#$&*?;|<>(){}[]~%^@=+,.:/"

// blanksKnownCorpus returns n inputs of the classes #769 closes, with their known value extent (in each, the
// value is more than its first token and leaked on main).
func blanksKnownCorpus(n int) []knownValue {
	rng := rand.New(rand.NewPCG(769, 2)) //nolint:gosec // a fixed seed keeps the corpus the same on every run
	out := make([]knownValue, n)
	for i := range out {
		k := &out[i]
		k.rng = rng
		switch i % 6 {
		case 0:
			// A plain item with blanks of a block sequence after a flag item: the item is the rest of its
			// line, and the next item stays.
			indent := []string{"", "  ", "    "}[rng.IntN(3)]
			k.b.WriteString(indent + "args:\n" + indent + "- --client-secret\n" + indent + "- ")
			k.word()
			k.b.WriteString(" ")
			k.words(" ", "  ", " : ", "'s ", ", ", " #x")
			k.b.WriteString("\n" + indent + "- ")
			k.keep()
		case 1:
			// A quote glued to a plain value opens a quoted piece of the same shell word.
			k.pick("run --password ", "run --token=", "--client-secret ", "API_TOKEN=", "export DB_PASSWORD=")
			k.word()
			q := []string{`"`, `'`}[rng.IntN(2)]
			k.b.WriteString(q)
			k.words(" ", "  ", ", ", "=")
			k.b.WriteString(q)
			if rng.IntN(2) == 0 {
				k.word()
			}
			k.b.WriteString(" --region ")
			k.keep()
		case 2:
			// A comma after a quoted piece after a key name, glued to more of the value.
			k.pick("TOKEN=", "password=", "export API_TOKEN=")
			k.b.WriteString(`"`)
			k.words(" ")
			k.b.WriteString(`",`)
			k.word()
			if rng.IntN(2) == 0 {
				k.pick("/", "+", ".", "-")
				k.word()
			}
			k.b.WriteString(" ")
			k.keep()
			k.b.WriteString("=x")
		case 3:
			// "--name=value" in a quoted argument whose value opens with a backslash: the argument is the value.
			switch rng.IntN(3) {
			case 0:
				k.b.WriteString(`["--client-secret=\\`)
				k.words(" ", ", ", "'")
				k.b.WriteString(`", "`)
				k.keep()
				k.b.WriteString(`"]`)
			case 1:
				k.b.WriteString(`['--token=\`)
				k.words(" ", ", ", `"`)
				k.b.WriteString(`', '`)
				k.keep()
				k.b.WriteString(`']`)
			default:
				k.b.WriteString(`{"args":"[\"--client-secret=\\\\`)
				k.words(" ", ", ", "'")
				k.b.WriteString(`\", \"`)
				k.keep()
				k.b.WriteString(`\"]"}`)
			}
		case 4:
			// A quoted scalar behind a YAML anchor or tag inside an array holds brackets and commas.
			k.b.WriteString("{password: [")
			k.pick("!!str ", "&a ", "&a !!str ", "!x ", "!!str  ")
			q := []string{`"`, `'`}[rng.IntN(2)]
			k.b.WriteString(q)
			k.words(" ", "]", ", ", "}", "[")
			k.b.WriteString(q + ", ")
			k.word()
			k.b.WriteString("], u: ")
			k.keep()
			k.b.WriteString("}")
		case 5:
			// An escaped punctuation character glued to a quoted piece after a flag.
			k.pick("--client-secret ", "run --password ", "app --token ")
			q := []string{`"`, `'`}[rng.IntN(2)]
			k.b.WriteString(q)
			k.words(" ")
			k.b.WriteString(q + `\` + string(escapedPunct[rng.IntN(len(escapedPunct))]))
			k.word()
			k.b.WriteString(" --region ")
			k.keep()
		}
		k.rng = nil
	}
	return out
}

// checkKnownExtent fails for each input of the corpus where a byte of a word of the value stays or a span
// touches the kept token, and counts the failures by class (the index modulo classes).
func checkKnownExtent(t *testing.T, corpus []knownValue, classes int) {
	t.Helper()
	failed := 0
	byClass := make([]int, classes)
	for i, k := range corpus {
		in := k.b.String()
		now := defaultRedactor.spans(nil, in)
		bad := ""
		for _, w := range k.secret {
			for i := w[0]; i < w[1]; i++ {
				if !coveredBy([2]int{i, i + 1}, now) {
					bad = "value byte kept"
				}
			}
		}
		for _, sp := range now {
			if sp.start < k.kept[1] && k.kept[0] < sp.end {
				bad = "next token lost"
			}
		}
		if bad != "" {
			failed++
			byClass[i%classes]++
			if failed <= 20 {
				t.Errorf("%s:\n in: %q\nout: %q", bad, in, String(in))
			}
		}
	}
	if failed > 0 {
		t.Errorf("%d of %d inputs failed, by class %v", failed, len(corpus), byClass)
	}
}

// The classes #769 closes, against values whose extent is known by construction: no byte of a word of the
// value survives, and the token after the value stays.
func TestBlanksTakeTheKnownValue(t *testing.T) {
	// A corpus test that counts no work: it runs beside the others (#873).
	t.Parallel()
	checkKnownExtent(t, blanksKnownCorpus(3000), 6)
}

// blanksKeptCorpus returns n inputs where the value is one token and the text after it is a field of the
// surrounding format, which must stay: a flag inside a JSON or logfmt string, fields after a comma, a flag
// that is no block sequence item before a dash line, and a JSON escape after a quoted piece.
func blanksKeptCorpus(n int) []knownValue {
	rng := rand.New(rand.NewPCG(769, 3)) //nolint:gosec // a fixed seed keeps the corpus the same on every run
	out := make([]knownValue, n)
	for i := range out {
		k := &out[i]
		k.rng = rng
		switch i % 6 {
		case 0:
			k.pick(`{"cmd":"--password `, `{"msg":"run --client-secret `, `{"cmd":"run --token `)
			k.word()
			k.pick(`","reason":"`, `", "next": "`)
			k.keep()
			k.b.WriteString(`"}`)
		case 1:
			q := []string{`"`, `'`}[rng.IntN(2)]
			k.b.WriteString("msg=" + q)
			k.pick("--password ", "run --token ", "--client-secret ")
			k.word()
			k.b.WriteString(q)
			k.pick(" user=", "  user=")
			k.keep()
		case 2:
			q := []string{`"`, `'`}[rng.IntN(2)]
			k.pick("password=", "TOKEN=", "API_TOKEN=")
			k.b.WriteString(q)
			k.word()
			k.b.WriteString(q)
			k.pick(",user=", ",next=", ", user=", ",next: ")
			k.keep()
		case 3:
			k.pick("--password\r  - ", "--password\n- ", "x --password\n  - ")
			k.word()
			k.b.WriteString(" ")
			k.keep()
		case 4:
			if rng.IntN(2) == 0 {
				k.b.WriteString(`{"log":"run --password '`)
				k.word()
				k.pick(`'\n`, `'\t`, `'\r\n`)
			} else {
				k.b.WriteString(`{"log":"run --password \"`)
				k.word()
				k.pick(`\"\n`, `\"\t`)
			}
			k.keep()
			k.b.WriteString(` next"}`)
		case 5:
			k.b.WriteString("{password: [")
			k.word()
			k.pick(`!x "`, `&y '`)
			k.b.WriteString("], u: ")
			k.keep()
			k.b.WriteString("}")
		}
		k.rng = nil
	}
	return out
}

// What must stay next to the contexts #769 changes stays: the value is one token, and the field after it in
// the surrounding JSON, logfmt or comma-separated text, the next line after a flag that is no item of a block
// sequence, and the text after a JSON escape survive.
func TestBlanksKeepTheFieldsAfterTheValue(t *testing.T) {
	checkKnownExtent(t, blanksKeptCorpus(1200), 6)
}

// The rows #769 removed from TestKeyNameKnownLeaks, with the text after each value that stays.
func TestBlanksClosedLeaks(t *testing.T) {
	k := leakKey()
	for in, kept := range map[string]string{
		"args:\n- --client-secret\n- horse battery " + k + listItemTail:       listItemTail,
		"  args:\n    - --password\n    - horse battery " + k + "\n    - --x": "\n    - --x",
		`run --password ab"cd ` + k + `"` + flagAfter:                         flagAfter,
		`TOKEN="abc",` + k + nextX:                                            nextX,
		`["--client-secret=\\horse ` + k + argRegionTail:                      argRegionTail,
		`{password: [!!str "horse]` + k + `", staple], u: x}`:                 ", u: x}",
		`--client-secret "horse"\!` + k + regionWest:                          regionWest,
	} {
		checkRedacted(t, in, []string{k, k[:12], k[len(k)-12:], passphraseWords[1]}, []string{kept})
	}
}

// A comma glued to a quoted piece after a key name, followed by a secret key name the key rules match on
// their own, ends the value: the second key's value is redacted as on main, also with blanks around its "="
// (#769 fix round: the comma once took the second key name into the first value and its value stayed).
// After a flag that is no item of a block sequence, an indented item on the next line keeps the narrower
// value of one token.
func TestBlanksSecretKeyAfterAComma(t *testing.T) {
	for in, want := range map[string]string{
		`password="abc",DB_PASSWORD= "hidden word" next=x`: `password="***",DB_PASSWORD= "[REDACTED]" next=x`,
		`TOKEN='a b',client_secret = hidden next=x`:        `TOKEN='***',client_secret = *** next=x`,
		`password="abc",x-api-key: "hidden word" next=x`:   `password="***",x-api-key: "[REDACTED]" next=x`,
		`TOKEN="abc",token next=x`:                         `TOKEN="***",token next=x`,
		"--password\n  - a b":                              "--password\n  - * b",
	} {
		if got := String(in); got != want {
			t.Errorf("String(%q)\n got %q\nwant %q", in, got, want)
		}
	}
}

// Over-redaction #769 accepts, pinned so that a change to it is seen: a comment after a plain item of a block
// sequence goes with the item, a comma glued to a quoted piece after a key name takes the word after it when
// that word is no "name=" or "name:" field, and a quote glued to a plain value takes the quoted piece with
// the text glued after it. A key name or flag in the middle of a quoted string (not right after its opening
// quote) reads its value with no bound at that string's closing quote, so a group it opens runs past it.
func TestBlanksAcceptedOverRedaction(t *testing.T) {
	for in, want := range map[string]string{
		"args:\n- --password\n- abc def # rotated\n- --region": "args:\n- --password\n- [credential]\n- --region",
		`TOKEN="abc",later next=x`:                             `TOKEN="[REDACTED] next=x`,
		`run --password ab"cd"ef --region x`:                   `run --password *** --region x`,
		`{"msg":"run --password $(a","x":"b)"}`:                `{"msg":"run --password [credential]"}`,
	} {
		if got := String(in); got != want {
			t.Errorf("String(%q)\n got %q\nwant %q", in, got, want)
		}
	}
}
