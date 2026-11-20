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

// extentMainTable is the differential table of the extent rule (#708): extentCorpus inputs (a key name or
// flag in each of its forms, a value of random grammar pieces, and the text after it), with the byte ranges
// main redacted (commit e00500aa, before #708). It was written once by running this file in a checkout of
// that commit with EXTENT_MAIN_TABLE set to the output path; regenerate it only from that commit.
const extentMainTable = "testdata/extent-main.jsonl"

// extentPrefixes are the key-name and flag forms the corpus puts before a value.
var extentPrefixes = []string{
	"run --password ", "run --password=", "run -password ", "--client-secret ", "app --token \\\n  ",
	"password=", "PASSWORD := ", "msg=password=", "password: ", "  password: ", "apiKey: ",
	`{"password": `, `{"password":`, `{password: `, `{\"apiKey\":`, `["--password", `, `["--password=`,
	`[--password, `, "args:\n- --password\n- ", "args: [--a, --client-secret, ", `'--password' `,
	"set aws_secret_access_key ", "TOKEN=", "--password,",
}

// extentPieces are the grammar pieces of a value in the corpus: words, quotes, brackets, substitutions,
// commas, escapes and line ends.
var extentPieces = []string{
	"horse", passphraseWords[1], "Staple9", " ", " ", ",", ", ", `"`, `'`, "`", "$(", ")", "(", "[", "]", "{", "}",
	`\`, `\ `, `\)`, `\"`, `\\"`, "\n", "\n  ", "\n- ", ";", "&", "=", ":", "#", "/*", "*/", "//", "${X}",
	"''", `""`, "it's",
}

// extentTails are the text after a value in the corpus.
var extentTails = []string{
	" --region west", ` next=x`, `, "u": "x"}`, `", "--region"]`, flowRegionTail, "\nnext: x", "\n- --region",
	"], u: x}", `"]`, "", " EVIDENCE", `\",\"u\":1}`,
}

// extentCorpus returns n inputs built from the pieces above with a fixed seed.
func extentCorpus(n int) []string {
	rng := rand.New(rand.NewPCG(708, 2)) //nolint:gosec // a fixed seed keeps the corpus the same on every run
	out := make([]string, 0, n)
	for range n {
		var b strings.Builder
		b.WriteString(extentPrefixes[rng.IntN(len(extentPrefixes))])
		for range 1 + rng.IntN(7) {
			b.WriteString(extentPieces[rng.IntN(len(extentPieces))])
		}
		b.WriteString(extentTails[rng.IntN(len(extentTails))])
		out = append(out, b.String())
	}
	return out
}

// TestWriteExtentMainTable writes extentMainTable from the redactor it runs with, when EXTENT_MAIN_TABLE
// names the output path; otherwise it is skipped. Run it only in a checkout of the commit that
// extentMainTable names.
func TestWriteExtentMainTable(t *testing.T) {
	path := os.Getenv("EXTENT_MAIN_TABLE")
	if path == "" {
		t.Skip("EXTENT_MAIN_TABLE is not set")
	}
	f, err := os.Create(path) //nolint:gosec // a path the person running the test names
	if err != nil {
		t.Fatal(err)
	}
	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)
	for _, in := range extentCorpus(4000) {
		c := mainRedaction{In: in, Out: String(in)}
		for _, sp := range defaultRedactor.spans(in) {
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

// Guarantee A of the extent rule (#708): on every input of a generated corpus of key names, flags and values
// of every grammar piece, each byte main redacted is still redacted. The rule takes the union of main's value
// and its own extent, so this holds by construction; a change that narrows a value fails here.
func TestExtentStillRedactsWhatMainRedacted(t *testing.T) {
	// A corpus test that counts no work: it runs beside the others (#873).
	t.Parallel()
	f, err := os.Open(extentMainTable)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	gen := extentCorpus(4000)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<16), 1<<20)
	cases, redacted := 0, 0
	for sc.Scan() {
		var c mainRedaction
		if err := json.Unmarshal(sc.Bytes(), &c); err != nil {
			t.Fatalf("line %d: %v", cases+1, err)
		}
		cases++
		checkTableInput(t, extentMainTable, cases, c.In, gen)
		if len(c.Spans) > 0 {
			redacted++
		}
		now := defaultRedactor.spans(c.In)
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
		t.Fatalf("%s has %d lines, its corpus generator %d inputs", extentMainTable, cases, len(gen))
	}
	if cases < 4000 || redacted < 3000 {
		t.Fatalf("table too small: %d cases, %d redacted on main", cases, redacted)
	}
}

// knownValue builds an input whose value extent is known by construction: the words of the value (secret)
// and the token after it (kept), each as a byte range.
type knownValue struct {
	b      strings.Builder
	rng    *rand.Rand
	secret [][2]int
	kept   [2]int
}

// word writes a word of the value.
func (k *knownValue) word() {
	start := k.b.Len()
	k.b.WriteString("Wq")
	for range 1 + k.rng.IntN(4) {
		k.b.WriteByte("abcdefghijklmnopqrstuvwxyz"[k.rng.IntN(26)])
	}
	k.secret = append(k.secret, [2]int{start, k.b.Len()})
}

// pick writes one of the strings.
func (k *knownValue) pick(xs ...string) { k.b.WriteString(xs[k.rng.IntN(len(xs))]) }

// keep writes the token after the value.
func (k *knownValue) keep() {
	k.kept[0] = k.b.Len()
	k.b.WriteString("KEEPME")
	k.kept[1] = k.b.Len()
}

// shellCommand writes the text of a command substitution: words, blanks, quoted words and escapes.
func (k *knownValue) shellCommand(depth int) {
	for i := range 1 + k.rng.IntN(4) {
		if i > 0 {
			k.pick(" ", "  ", " | ", "; ")
		}
		switch k.rng.IntN(6) {
		case 0:
			k.b.WriteString(`"`)
			k.dquoted(depth + 1)
			k.b.WriteString(`"`)
		case 1:
			k.b.WriteString(`'`)
			k.squoted()
			k.b.WriteString(`'`)
		case 2:
			k.pick(`\)`, `\(`, `\$`, `[`, `]`, `{`, `}`, `${X}`)
			k.word()
		default:
			k.word()
		}
	}
}

// dquoted writes the content of a shell double-quoted string.
func (k *knownValue) dquoted(depth int) {
	for range 1 + k.rng.IntN(4) {
		switch k.rng.IntN(6) {
		case 0:
			if depth < 3 {
				k.b.WriteString("$(")
				k.shellCommand(depth + 1)
				k.b.WriteString(")")
				continue
			}
			k.word()
		case 1:
			k.pick(" ", "'", `\"`, "[", "]", "{", "}", ",", ")", "(", `\$`, `\\`)
		default:
			k.word()
		}
	}
}

// squoted writes the content of a shell single-quoted string.
func (k *knownValue) squoted() {
	for range 1 + k.rng.IntN(4) {
		if k.rng.IntN(3) == 0 {
			k.pick(" ", `"`, "$(", ")", "`", `\`, "[", "{", ",")
		}
		k.word()
	}
}

// shellWord writes a shell word of one to three glued pieces. A word that starts with plain text glues no
// quoted piece after it, and after a key name (flag false) a word holds no comma: blanksKnownCorpus covers
// both (#769).
func (k *knownValue) shellWord(flag bool) {
	plain := false
	for i := range 1 + k.rng.IntN(3) {
		if i > 0 && k.rng.IntN(4) == 0 {
			k.b.WriteString(`\ `)
		}
		piece := k.rng.IntN(5)
		if plain && piece < 2 {
			piece = 2
		}
		plain = plain || i == 0 && piece == 4
		switch piece {
		case 0:
			k.b.WriteString(`"`)
			k.dquoted(0)
			k.b.WriteString(`"`)
		case 1:
			k.b.WriteString(`'`)
			k.squoted()
			k.b.WriteString(`'`)
		case 2:
			k.b.WriteString("$(")
			k.shellCommand(0)
			k.b.WriteString(")")
		case 3:
			k.b.WriteString("`")
			k.word()
			k.b.WriteString(" ")
			k.word()
			k.b.WriteString("`")
		default:
			k.word()
			if k.rng.IntN(3) == 0 {
				if flag || plain {
					k.pick(",", ":", "=", ".", "/")
				} else {
					k.pick(":", "=", ".", "/")
				}
				k.word()
			}
		}
	}
}

// jsonString writes the content of a JSON string: literal text with "\"" and "\\" escapes.
func (k *knownValue) jsonString() {
	for range 1 + k.rng.IntN(5) {
		if k.rng.IntN(2) == 0 {
			k.pick(" ", "'", "$(", ")", "`", "[", "]", "{", "}", ",", ":", `\"`, `\\`, "#")
		}
		k.word()
	}
}

// yamlFlowItems writes the items of a YAML flow sequence.
func (k *knownValue) yamlFlowItems() {
	for i := range 1 + k.rng.IntN(3) {
		if i > 0 {
			k.b.WriteString(", ")
		}
		k.yamlFlowItem()
	}
}

// yamlFlowItem writes an item of a YAML flow sequence: a plain scalar (with apostrophes and blanks inside)
// or a quoted one.
func (k *knownValue) yamlFlowItem() {
	switch k.rng.IntN(3) {
	case 0:
		k.b.WriteString(`"`)
		k.jsonString()
		k.b.WriteString(`"`)
	case 1:
		k.b.WriteString("'")
		k.word()
		k.pick("''", " ", ",", `"`)
		k.word()
		k.b.WriteString("'")
	default:
		k.word()
		k.pick("'", " ", "'s ", `"`, "")
		k.word()
	}
}

// knownExtentCorpus returns n inputs of every context with their known value extent.
func knownExtentCorpus(n int) []knownValue {
	rng := rand.New(rand.NewPCG(708, 3)) //nolint:gosec // a fixed seed keeps the corpus the same on every run
	out := make([]knownValue, n)
	for i := range out {
		k := &out[i]
		k.rng = rng
		switch i % 8 {
		case 0:
			k.pick("run --password ", "run --token=", "--client-secret ", "run -token ")
			k.shellWord(true)
			k.b.WriteString(" --region ")
			k.keep()
		case 1:
			k.pick("API_TOKEN=", "export TOKEN=", "msg secret=")
			k.shellWord(false)
			k.b.WriteString(" ")
			k.keep()
			k.b.WriteString("=x")
		case 2:
			k.b.WriteString(`["--password", "`)
			k.jsonString()
			k.b.WriteString(`", "`)
			k.keep()
			k.b.WriteString(`"]`)
		case 3:
			k.b.WriteString(`{"password": "`)
			k.jsonString()
			k.b.WriteString(`", "u": "`)
			k.keep()
			k.b.WriteString(`"}`)
		case 4:
			k.b.WriteString(`["--client-secret=`)
			k.word()
			k.jsonString()
			k.b.WriteString(`", "`)
			k.keep()
			k.b.WriteString(`"]`)
		case 5:
			k.b.WriteString("{password: [")
			k.yamlFlowItems()
			k.b.WriteString("], u: ")
			k.keep()
			k.b.WriteString("}")
		case 6:
			k.b.WriteString("args:\n- --password\n- \"")
			k.jsonString()
			k.b.WriteString("\"\n- ")
			k.keep()
		case 7:
			k.b.WriteString("args: [--password, ")
			k.yamlFlowItem()
			k.b.WriteString(", ")
			k.keep()
			k.b.WriteString("]")
		}
		k.rng = nil
	}
	return out
}

// The extent rule against values whose extent is known by construction (#708): shell words of glued quoted
// pieces, substitutions and escapes after a flag or an env name, JSON strings with shell syntax inside them
// as literal text, quoted arguments, and YAML flow sequences with apostrophes in plain scalars. No byte of a
// word of the value survives, and the token after the value stays.
func TestExtentTakesTheKnownValue(t *testing.T) {
	// A corpus test that counts no work: it runs beside the others (#873).
	t.Parallel()
	failed := 0
	for _, k := range knownExtentCorpus(4000) {
		in := k.b.String()
		now := defaultRedactor.spans(in)
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
			if failed <= 30 {
				t.Errorf("%s:\n in: %q\nout: %q", bad, in, String(in))
			}
		}
	}
	if failed > 0 {
		t.Errorf("%d inputs failed", failed)
	}
}

// A value read from inside a JSON or YAML quoted string never crosses that string's closing quote (#708):
// for generated argument lists whose secret argument holds an unclosed "$(", backtick, "${", bracket, brace,
// blank or escape, the items after the secret argument survive byte for byte and no word of the secret stays.
// The lists are JSON, YAML single-quoted, and JSON escaped inside a JSON string. "--password=" is left out: the
// connection string rule (older than #708) takes "Password=" to the line end.
func TestExtentKeepsTheArgumentsAfterAQuotedSecret(t *testing.T) {
	// A corpus test that counts no work: it runs beside the others (#873).
	t.Parallel()
	rng := rand.New(rand.NewPCG(708, 4)) //nolint:gosec // a fixed seed keeps the corpus the same on every run
	openers := []string{"$(", "`", "${", "[", "{", "(", " ", ")", "]", "}", "$("}
	tails := []string{"EVIDENCE)", "EV`", "EV}", "EV]", regionFlag, "west", "a b)", "$(x)", "x'y"}
	failed := 0
	for i := range 3000 {
		k := &knownValue{rng: rng}
		q, open, sep := `"`, `["`, `", "`
		switch i % 3 {
		case 1:
			q, open, sep = `'`, `['`, `', '`
		case 2:
			q, open, sep = `\"`, `"args": "[\"`, `\", \"`
		}
		k.b.WriteString(open)
		k.pick("--client-secret=", "--token=", "-secret=")
		k.word()
		for range 1 + rng.IntN(3) {
			k.pick(openers...)
			k.word()
		}
		k.b.WriteString(sep)
		tail := k.b.Len() - len(q)
		for j := range 1 + rng.IntN(3) {
			if j > 0 {
				k.b.WriteString(sep)
			}
			item := tails[rng.IntN(len(tails))]
			if q == `'` {
				item = strings.ReplaceAll(item, "'", "''")
			}
			k.b.WriteString(item)
		}
		k.b.WriteString(q + "]")
		if q == `\"` {
			k.b.WriteString(`"`)
		}
		in := k.b.String()
		out := String(in)
		bad := ""
		if !strings.HasSuffix(out, in[tail:]) {
			bad = "items after the secret lost"
		}
		for _, w := range k.secret {
			if strings.Contains(out, in[w[0]:w[1]]) {
				bad = "secret word kept"
			}
		}
		if bad != "" {
			failed++
			if failed <= 30 {
				t.Errorf("%s:\n in: %q\nout: %q", bad, in, out)
			}
		}
	}
	if failed > 0 {
		t.Errorf("%d inputs failed", failed)
	}
}
