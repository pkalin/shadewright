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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
)

// The block-value corpus of #840: a YAML block value under a secret key (a literal or folded block scalar
// with every chomping and indentation indicator, a value on the following lines as a scalar, a list, a
// nested map or a list of block scalars, a quoted scalar over several lines and a plain scalar with
// continuation lines), with spaces or tabs, LF or CRLF, the key at the start of a line, after other text on
// the same line, and with every line break flattened to a blank as the Incident context does, then the end of
// the input, a dedented innocent key or a line of prose. The generator places each secret and each kept word
// at a known offset, so the answer is known by construction.

// blockCase is one input of the corpus with the byte ranges of the secrets the generator placed, which must
// be redacted, and of the text around them that must survive.
type blockCase struct {
	name    string
	in      string
	secrets [][2]int
	keep    [][2]int
	// lined is the input before its line breaks were flattened (in itself when it was not).
	lined string
	// indicator: the value opens with a block scalar indicator; afterText: other text than sequence dashes
	// stands before the key on its line.
	indicator, afterText bool
	// bareQuoted: a quoted or plain scalar over several lines under a bare short key.
	bareQuoted bool
}

// blockWriter builds one blockCase.
type blockWriter struct {
	b    strings.Builder
	c    blockCase
	nl   string // the line break: "\n" or "\r\n"
	step string // one level of indentation: two blanks or a tab
	// value returns the next value the generator places; secret says whether it must be redacted.
	value  func(phrase bool) string
	secret bool
	// child names the keys of a nested map, innocent in both corpora so that only the outer key decides.
	child [2]string
}

func (w *blockWriter) text(s string) { w.b.WriteString(s) }

// val writes the next value, multi-word when phrase is set, and records its range.
func (w *blockWriter) val(phrase bool) {
	start := w.b.Len()
	w.b.WriteString(w.value(phrase))
	if w.secret {
		w.c.secrets = append(w.c.secrets, [2]int{start, w.b.Len()})
	}
}

// kept writes s and records that it must survive.
func (w *blockWriter) kept(s string) {
	start := w.b.Len()
	w.b.WriteString(s)
	w.c.keep = append(w.c.keep, [2]int{start, w.b.Len()})
}

// line starts a new line at the indentation ind plus n steps.
func (w *blockWriter) line(ind string, n int) {
	w.text(w.nl + ind + strings.Repeat(w.step, n))
}

// blockStyle writes the value of key, whose line has the indentation ind, after the key's colon.
type blockStyle struct {
	name  string
	write func(w *blockWriter, ind string)
	// phrase: the style may hold a multi-word value, which a bare short key ("pass") does not count.
	phrase bool
}

// blockIndicators are the block scalar headers of the corpus: literal and folded, with chomping and
// indentation indicators in both orders.
var blockIndicators = []string{"|", ">", "|-", ">+", "|2", ">-", "|2-", ">+1", "|+", ">2"}

// blockStyles returns the value forms of the corpus.
func blockStyles() []blockStyle {
	var styles []blockStyle
	for _, ind := range blockIndicators {
		styles = append(styles, blockStyle{name: "scalar " + ind, phrase: true, write: func(w *blockWriter, k string) {
			w.text(": " + ind)
			w.line(k, 1)
			w.val(false)
			w.line(k, 1)
			w.val(true)
		}})
	}
	return append(styles,
		blockStyle{name: "scalar with comment", write: func(w *blockWriter, k string) {
			w.text(": |- # rotated by CI")
			w.line(k, 1)
			w.val(false)
		}},
		blockStyle{name: "scalar more indented", write: func(w *blockWriter, k string) {
			w.text(": >")
			w.line(k, 1)
			w.val(false)
			w.line(k, 2)
			w.val(false)
		}},
		blockStyle{name: "next-line scalar", write: func(w *blockWriter, k string) {
			w.text(":")
			w.line(k, 1)
			w.val(false)
		}},
		blockStyle{name: "next-line map", write: func(w *blockWriter, k string) {
			w.text(":")
			w.line(k, 1)
			w.text(w.child[0] + ": ")
			w.val(false)
			w.line(k, 1)
			w.text(w.child[1] + ": ")
			w.val(false)
		}},
		blockStyle{name: "nested map", write: func(w *blockWriter, k string) {
			w.text(":")
			w.line(k, 1)
			w.text(w.child[0] + ":")
			w.line(k, 2)
			w.text(w.child[1] + ": ")
			w.val(false)
		}},
		blockStyle{name: "map with block scalar child", write: func(w *blockWriter, k string) {
			w.text(":")
			w.line(k, 1)
			w.text(w.child[0] + ": |")
			w.line(k, 2)
			w.val(false)
		}},
		blockStyle{name: "list", write: func(w *blockWriter, k string) {
			w.text(":")
			w.line(k, 1)
			w.text("- ")
			w.val(false)
			w.line(k, 1)
			w.text("- ")
			w.val(false)
		}},
		blockStyle{name: "compact list", write: func(w *blockWriter, k string) {
			w.text(":")
			w.line(k, 0)
			w.text("- ")
			w.val(false)
		}},
		blockStyle{name: "list of block scalars", write: func(w *blockWriter, k string) {
			w.text(":")
			w.line(k, 1)
			w.text("- |")
			w.line(k, 2)
			w.val(false)
			w.line(k, 1)
			w.text("- >-")
			w.line(k, 2)
			w.val(false)
		}},
		blockStyle{name: "double-quoted over lines", phrase: true, write: func(w *blockWriter, k string) {
			w.text(`: "`)
			w.val(false)
			w.line(k, 1)
			w.val(true)
			w.text(`"`)
		}},
		blockStyle{name: "single-quoted over lines", phrase: true, write: func(w *blockWriter, k string) {
			w.text(": '")
			w.val(false)
			w.line(k, 1)
			w.val(true)
			w.text("'")
		}},
		blockStyle{name: "plain continuation", write: func(w *blockWriter, k string) {
			w.text(": ")
			w.val(false)
			w.line(k, 1)
			w.val(false)
		}},
	)
}

// blockPlacement says where the key stands: lead is the text before it on its line (after the
// indentation), and flat turns every line break into a blank, as the Incident context does.
type blockPlacement struct {
	ind, lead string
	flat      bool
	// first: the key opens the input, with no line of prose before it.
	first bool
}

// blockPlacements returns the key at the start of a line (indented or not) and after other text on its
// line, each also flattened.
func blockPlacements() []blockPlacement {
	out := make([]blockPlacement, 0, 14)
	for _, flat := range []bool{false, true} {
		for _, p := range []blockPlacement{
			{ind: "", first: true}, {ind: ""}, {ind: "  "}, {ind: "", lead: "warning: "}, {ind: "  ", lead: "config error "},
			{ind: "", lead: "level=warn msg="}, {ind: "", lead: "- "},
		} {
			p.flat = flat
			out = append(out, p)
		}
	}
	return out
}

// blockTails are what follows the block: the end of the input, a line break, a dedented innocent key at
// the key line's indentation, or a line of prose. Words in brackets must survive when not flattened.
var blockTails = []string{"", "\n", "\n{ind}[next]: [keepme]\n", "\n[Back-off] restarting"}

// blockNeighbourTails follow a secret block with a second secret key, glued to the last value by each
// separator, quote and line break, so that a block read too far cannot hide it. "<>" is a secret.
var blockNeighbourTails = func() []string {
	out := make([]string, 0, 36)
	for _, glue := range []string{" ", "\t", ",", `"`, "'", "\n", "\n{ind}", "\n{ind}  ", threeBlanks} {
		for _, field := range []string{"token: <>", "apiKey: |\n{ind}  <>", "'password': <>", "secret:\n{ind}  <>"} {
			out = append(out, glue+field)
		}
	}
	return out
}()

// dbPasswordKey, passwordKey and userKey are key names the corpus uses.
const (
	dbPasswordKey = "DB_PASSWORD"
	passwordKey   = "password"
	userKey       = "user"
)

// blockSecretKeys are secret key names of every form the rules know: secret words with "_", "-", "." and
// camelCase, quoted, and the bare short keys, which count only with a value that looks like a secret.
var blockSecretKeys = []string{
	passwordKey, "passwd", dbPasswordKey, "passphrase", "pwd", "secret", "client_secret", tokenKeyName, apiKeyName,
	"api-key", "access_key", "private_key", "credentials", "credential", "connectionString", "conn_str",
	`"password"`, "'token'", passKey, authKey, sigKey, "db.pass", "basic_auth", "storageSas", "AZURE_STORAGE_SAS",
}

// blockInnocentKeys are key names that hold no secret, among them words that contain a short key name.
var blockInnocentKeys = []string{
	"description", "script", "message", "config", "data", "notes", "author", "bypass", "signal", "kansas",
	"passing", "authority",
}

// isBareShortCorpusKey reports whether the corpus key counts only with a value that looks like a secret.
func isBareShortCorpusKey(key string) bool { return isBareShortKey(key) }

// blockCorpus returns the corpus over keys; with secret, the values are secrets (opaque tokens, and
// passphrases where the key counts them), otherwise innocent words.
func blockCorpus(keys []string, secret bool) []blockCase {
	out := make([]blockCase, 0, 1<<14)
	n := 0
	value := func(bare bool) func(bool) string {
		return func(phrase bool) string {
			n++
			switch {
			case !secret && phrase:
				return "keep this line"
			case !secret:
				return "ready"
			case phrase && !bare:
				return fmt.Sprintf("Tr0ub4dor horse %d staple", n)
			default:
				return fmt.Sprintf("Qz%dxK9w", 10000+n)
			}
		}
	}
	i := 0
	for _, st := range blockStyles() {
		for _, pl := range blockPlacements() {
			for _, nl := range []string{"\n", "\r\n"} {
				for _, step := range []string{"  ", "\t"} {
					tails := blockTails
					if secret {
						tails = append(slices.Clip(tails), blockNeighbourTails[i%len(blockNeighbourTails)])
					}
					for _, tail := range tails {
						for j := range 2 {
							key := keys[(i*7+j*3)%len(keys)]
							i++
							out = append(out, writeBlockCase(st, pl, nl, step, tail, key, secret,
								value(isBareShortCorpusKey(key))))
						}
					}
				}
			}
		}
	}
	return out
}

// writeBlockCase builds one case: a line of prose, the key's line, the block and the tail.
func writeBlockCase(st blockStyle, pl blockPlacement, nl, step, tail, key string, secret bool,
	value func(bool) string) blockCase {
	w := &blockWriter{nl: nl, step: step, value: value, secret: secret, child: [2]string{userKey, "host"}}
	if !pl.first {
		w.kept("Back-off")
		w.text(" restarting failed container" + nl)
	}
	w.text(pl.ind)
	if pl.lead != "" {
		w.kept(strings.TrimRight(pl.lead, " =:-"))
		w.text(pl.lead[len(strings.TrimRight(pl.lead, " =:-")):])
	}
	w.text(key)
	// After a sequence dash the key's own column is the base of its block, as YAML has it.
	base := pl.ind
	if pl.lead == "- " {
		base += "  "
	}
	st.write(w, base)
	name := fmt.Sprintf("%s, key %s, ind %q lead %q first %v flat %v, nl %q step %q, tail %q", st.name, key,
		pl.ind, pl.lead, pl.first, pl.flat, nl, step, tail)
	tail = strings.ReplaceAll(strings.ReplaceAll(tail, "\n", nl), "{ind}", pl.ind)
	for {
		if sec := strings.Index(tail, "<>"); sec >= 0 && (strings.IndexByte(tail, '[') < 0 || sec < strings.IndexByte(tail, '[')) {
			w.text(tail[:sec])
			start := w.b.Len()
			w.b.WriteString(w.value(false))
			w.c.secrets = append(w.c.secrets, [2]int{start, w.b.Len()})
			tail = tail[sec+2:]
			continue
		}
		open := strings.IndexByte(tail, '[')
		if open < 0 {
			w.text(tail)
			break
		}
		closing := strings.IndexByte(tail, ']')
		w.text(tail[:open])
		if pl.flat {
			w.text(tail[open+1 : closing])
		} else {
			w.kept(tail[open+1 : closing])
		}
		tail = tail[closing+1:]
	}
	w.c.in = w.b.String()
	w.c.lined = w.c.in
	w.c.indicator = strings.HasPrefix(st.name, "scalar")
	w.c.afterText = pl.lead != "" && pl.lead != "- "
	w.c.bareQuoted = (strings.HasSuffix(st.name, "quoted over lines") || st.name == "plain continuation") &&
		isBareShortCorpusKey(key)
	if pl.flat {
		w.c.in = strings.Map(func(r rune) rune {
			if r == '\n' || r == '\r' {
				return ' '
			}
			return r
		}, w.c.in)
	}
	w.c.name = name
	return w.c
}

// redactedBytes returns, for each byte of s, whether the redactor replaces it.
func redactedBytes(s string, spans []span) []bool {
	out := make([]bool, len(s))
	for _, sp := range spans {
		for i := sp.start; i < sp.end; i++ {
			out[i] = true
		}
	}
	return out
}

// blockLeaks returns the secrets of c that keep a byte, and the kept ranges that lose one.
func blockLeaks(c blockCase, spans []span) (leaked, lost []string) {
	red := redactedBytes(c.in, spans)
	for _, r := range c.secrets {
		for i := r[0]; i < r[1]; i++ {
			if !red[i] {
				leaked = append(leaked, c.in[r[0]:r[1]])
				break
			}
		}
	}
	for _, r := range c.keep {
		for i := r[0]; i < r[1]; i++ {
			if red[i] {
				lost = append(lost, c.in[r[0]:r[1]])
				break
			}
		}
	}
	return leaked, lost
}

// Every secret the generator placed in a block value is redacted, and the text around it survives: the
// prose before the key, the text before it on its line, and, when the lines are not flattened, the dedented
// key or prose after the block. Since the decision of 2026-11-17 on #840:
//   - a value that opens with a block scalar indicator is redacted wherever the key stands, also when its
//     lines were flattened before it reached the redactor;
//   - any other value, under a key first on its line, is redacted in the text with its lines; text that was
//     flattened is checked as the callers produce it, redacted with its lines first, then flattened and
//     redacted again (blockPipeline);
//   - any other value under a key after other text on its line ("warning: credentials:" and a map below) is
//     not required: no YAML has a key there, and reading a block for it took prose for one. Those inputs
//     are only held to what main redacted (TestBlockValueStillRedactsWhatMainRedacted);
//   - nor is the part of a quoted or plain scalar over several lines under a bare short key ("pass: 'x" and
//     the closing line below) after its first line, which main leaves too (TestKeyNameKnownLeaks).
func TestBlockValueCorpusRedactsEverySecret(t *testing.T) {
	cases := blockCorpus(blockSecretKeys, true)
	failed, notRequired := 0, 0
	for _, c := range cases {
		var leaked, lost []string
		switch {
		case c.indicator:
			leaked, lost = blockLeaks(c, defaultRedactor.spans(c.in))
		case c.afterText || c.bareQuoted:
			notRequired++
			continue
		case c.lined != c.in:
			leaked, lost = blockPipelineLeaks(c)
		default:
			leaked, lost = blockLeaks(c, defaultRedactor.spans(c.in))
		}
		if len(leaked)+len(lost) == 0 {
			continue
		}
		failed++
		if failed <= 20 {
			t.Errorf("%s:\n  in: %q\n out: %q\n leaked %q, lost %q", c.name, c.in, String(c.in), leaked, lost)
		}
	}
	if failed > 0 {
		t.Errorf("%d of %d corpus inputs leak a secret or lose kept text", failed, len(cases)-notRequired)
	}
	if notRequired > len(cases)/2 {
		t.Fatalf("%d of %d inputs not required: the corpus checks too little", notRequired, len(cases))
	}
}

// blockPipeline redacts the text with its lines, flattens its line breaks to blanks and redacts it again,
// as fingerprint.Symptom and the context builder's values do.
func blockPipeline(lined string) string {
	return String(strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' {
			return ' '
		}
		return r
	}, String(lined)))
}

// blockPipelineLeaks returns the secrets of c that blockPipeline leaves in its output, and the kept words it
// loses, compared by text.
func blockPipelineLeaks(c blockCase) (leaked, lost []string) {
	out := blockPipeline(c.lined)
	for _, r := range c.secrets {
		if v := c.in[r[0]:r[1]]; strings.Contains(out, v) {
			leaked = append(leaked, v)
		}
	}
	for _, r := range c.keep {
		if v := c.in[r[0]:r[1]]; !strings.Contains(out, v) {
			lost = append(lost, v)
		}
	}
	return leaked, lost
}

// The same block values under innocent keys, with innocent values, come out byte for byte as they went in.
func TestBlockValueCorpusKeepsInnocentBlocks(t *testing.T) {
	cases := blockCorpus(blockInnocentKeys, false)
	failed := 0
	for _, c := range cases {
		if out := String(c.in); out != c.in {
			failed++
			if failed <= 20 {
				t.Errorf("%s:\n  in: %q\n out: %q", c.name, c.in, out)
			}
		}
	}
	if failed > 0 {
		t.Errorf("%d of %d innocent inputs changed", failed, len(cases))
	}
}

// blockMainTable is the differential table of #840: for each input of the secret corpus, in order, a hash
// of the input and the byte ranges main redacted (commit 69dac48d, before #840). The innocent corpus is not
// in it: main keeps each of its inputs byte for byte, as the rule does now (TestBlockValueCorpusKeepsInnocentBlocks).
// It was written once by running TestWriteBlockMainTable in a checkout of that commit with BLOCK_MAIN_TABLE
// set to the output path; regenerate it only from that commit.
const blockMainTable = "testdata/blockvalue-main.jsonl"

// blockMainRow is one line of blockMainTable: the first 8 bytes of the SHA-256 of the input, in hex, and
// the byte ranges main redacted.
type blockMainRow struct {
	Hash  string   `json:"h"`
	Spans [][2]int `json:"s,omitempty"`
}

// blockInputHash returns the hash of in that blockMainTable holds.
func blockInputHash(in string) string {
	sum := sha256.Sum256([]byte(in))
	return hex.EncodeToString(sum[:8])
}

// TestWriteBlockMainTable writes blockMainTable from the redactor it runs with, when BLOCK_MAIN_TABLE names
// the output path; otherwise it is skipped. Run it only in a checkout of the commit blockMainTable names.
func TestWriteBlockMainTable(t *testing.T) {
	path := os.Getenv("BLOCK_MAIN_TABLE")
	if path == "" {
		t.Skip("BLOCK_MAIN_TABLE is not set")
	}
	f, err := os.Create(path) //nolint:gosec // a path the person running the test names
	if err != nil {
		t.Fatal(err)
	}
	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)
	for _, c := range blockCorpus(blockSecretKeys, true) {
		row := blockMainRow{Hash: blockInputHash(c.in)}
		for _, sp := range defaultRedactor.spans(c.in) {
			row.Spans = append(row.Spans, [2]int{sp.start, sp.end})
		}
		if err := enc.Encode(row); err != nil {
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

// Each byte main redacted in the secret corpus is still redacted: the rules of #840 only add.
func TestBlockValueStillRedactsWhatMainRedacted(t *testing.T) {
	f, err := os.Open(blockMainTable)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	gen := blockCorpus(blockSecretKeys, true)
	sc := bufio.NewScanner(f)
	cases, redacted, failed := 0, 0, 0
	for sc.Scan() {
		var row blockMainRow
		if err := json.Unmarshal(sc.Bytes(), &row); err != nil {
			t.Fatalf("line %d: %v", cases+1, err)
		}
		cases++
		if cases > len(gen) || blockInputHash(gen[cases-1].in) != row.Hash {
			t.Fatalf("%s line %d does not match its corpus generator: regenerate the table from the commit it names",
				blockMainTable, cases)
		}
		in := gen[cases-1].in
		if len(row.Spans) > 0 {
			redacted++
		}
		red := redactedBytes(in, defaultRedactor.spans(in))
		for _, want := range row.Spans {
			for i := want[0]; i < want[1]; i++ {
				if !red[i] {
					failed++
					if failed <= 20 {
						t.Errorf("main redacted %q, now kept:\n in: %q\n now: %q", in[want[0]:want[1]], in, String(in))
					}
					break
				}
			}
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if cases != len(gen) {
		t.Fatalf("%s has %d lines, the corpus %d inputs: regenerate it from the commit it names", blockMainTable,
			cases, len(gen))
	}
	if failed > 0 {
		t.Errorf("%d ranges main redacted are kept now", failed)
	}
	if redacted < cases/2 {
		t.Fatalf("table too weak: main redacted %d of %d inputs", redacted, cases)
	}
}

// anyKeyHostileUnits are units that repeat into hostile inputs for the rule for block indicators: many keys
// on one long line in each form it reads (flattened blocks, refused bare keys, keys in strings that close or
// not, in flow collections and after brackets that never close), headers with blocks below, less indented or
// after a document marker, and indentation that deepens or shallows line by line under bare short keys.
var anyKeyHostileUnits = map[string]func(i int) string{
	"flattened scalars on one line": func(int) string { return "password: |   a " },
	"refused bare scalars":          func(int) string { return "pass: |   a b " },
	"refused bare comments":         func(int) string { return "x pass: | # a b " },
	"keys in strings":               func(int) string { return `"m":"token: | a", ` },
	"refused bare keys in strings":  func(int) string { return `"m":"pass: | a b", ` },
	"keys in unclosed strings":      func(int) string { return `"m: token: | a ` },
	"keys in flow collections":      func(int) string { return "{token: | a}, " },
	"keys after unclosed brackets":  func(int) string { return "[{token: | a " },
	"no blank after the indicator":  func(int) string { return "token: |a " },
	"mid-line block scalars":        func(int) string { return "x token: |\n  a\n" },
	"refused bare blocks":           func(int) string { return "x pass: |\n  a b\n" },
	"less indented blocks":          func(int) string { return "    x token: |\n  a\n" },
	"document markers":              func(int) string { return "x token: |\n---\n" },
	"deepening bare headers":        func(i int) string { return strings.Repeat(" ", i%512) + "x pass: |\n" },
	"deepening bare scalars":        func(i int) string { return strings.Repeat(" ", i%512) + "x auth: | a b\n" },
	"deepening headers":             func(i int) string { return strings.Repeat("\t", i%512) + "x token: |\n" },
	"shallowing bare headers":       func(i int) string { return strings.Repeat(" ", 511-i%512) + "x pass: |\n" },
	"comment lines under headers":   func(i int) string { return []string{"x pass: |\n", "#\n", " y auth: |\n"}[i%3] },
	"many keys before a block":      func(i int) string { return []string{"x token: | pass: | ", "\n  a\n"}[min(i%64, 1)] },
}

// anyKeyWork returns the bytes read, as countScan reports them, by String on the hostile input of about n
// bytes built from unit, and the size of that input.
func anyKeyWork(unit func(int) string, n int) (work, size int) {
	var b strings.Builder
	for i := 0; b.Len() < n; i++ {
		b.WriteString(unit(i))
	}
	in := b.String()
	scanned = func(k int) { work += k }
	defer func() { scanned = nil }()
	String(in)
	return work, len(in)
}

// The rule for block indicators reads each line a bounded number of times, whatever the input: on
// every hostile input the bytes read are at most a fixed multiple of its size, and eight times the input
// reads at most nine times as much. Work is counted, not timed, so a loaded machine cannot fail it (#825).
func TestAnyKeyWorkIsLinear(t *testing.T) {
	const perByte = 8
	for name, unit := range anyKeyHostileUnits {
		small, smallSize := anyKeyWork(unit, 16<<10)
		large, largeSize := anyKeyWork(unit, 128<<10)
		if large > perByte*largeSize {
			t.Errorf("%s: read %d bytes of a %d-byte input, more than %d per byte", name, large, largeSize, perByte)
		}
		if float64(large)/float64(largeSize) > 1.125*float64(max(small, 1))/float64(smallSize)+0.5 {
			t.Errorf("%s: %d bytes read for %d, %d for %d: more than linear", name, small, smallSize, large, largeSize)
		}
	}
}

// The cases of #840 by hand: a block value under a secret key and a block indicator after other text on its
// line, and with its line breaks flattened to blanks; the text before the key stays. A value on the next
// lines under a key first on its line is redacted before the callers flatten the text (blockPipeline).
func TestBlockValueAfterTextOrFlattened(t *testing.T) {
	const pw, pass, userValue = "hunter2xyzAB", "Tr0ub4dor9", "admin"
	for in, secrets := range map[string][]string{
		"Back-off restarting\nmsg: password: |\n  hunter2xyzAB\n":      {pw},
		"Back-off restarting password: |   hunter2xyzAB":               {pw},
		"Back-off pass: |-   Qz12049xK9w":                              {"Qz12049xK9w"},
		"Back-off restarting\r\nmsg: secret: >-\r\n\tTr0ub4dor9\r\n":   {pass},
		"Back-off restarting\nmsg: apiKey: |2- # rotated\n  Zq81xx\n":  {"Zq81xx"},
		"Back-off token: |   - hunter2xyzAB   - Tr0ub4dor9":            {pw, pass},
		`Back-off {"msg":"bad config: password: |   hunter2xyzAB"}`:    {pw},
		"Back-off [config password:| hunter2xyzAB":                     {pw},
		"Back-off password\u00a0: |   hunter2xyzAB":                    {pw},
		"Back-off\n    password: |\n  hunter2xyzAB\n":                  {pw},
		"Back-off\npassword: |\n---\n  hunter2xyzAB\n":                 {pw},
		"Back-off\nauth:\n  mode: basic secret: |\n    hunter2xyzAB\n": {pw},
	} {
		checkRedacted(t, in, secrets, []string{"Back-off"})
	}
	for in, secrets := range map[string][]string{
		"Back-off\ncredentials:\n  user: " + userValue + "\n  pass: s3cr3tPW\n": {userValue, "s3cr3tPW"},
		"Back-off\ntoken:\n  - hunter2xyzAB\n  - Tr0ub4dor9\n":                  {pw, pass},
		"Back-off\npassword: hunter2xyzAB\n  Tr0ub4dor9\n":                      {pw, pass},
		"Back-off\n'token': \"hunter two\n  three\"":                            {"hunter two", "three"},
	} {
		out := blockPipeline(in)
		for _, sec := range secrets {
			if strings.Contains(out, sec) {
				t.Errorf("redacted, flattened and redacted again, %q leaks %q: %q", in, sec, out)
			}
		}
		if !strings.Contains(out, "Back-off") {
			t.Errorf("%q lost Back-off: %q", in, out)
		}
	}
}

// Neighbouring text the rule for keys anywhere on a line leaves alone: keys inside JSON and flow
// collections (the flow rules read them), prose after a bare short key and an innocent block; and a kubelet
// event naming a Secret, whose key has a blank before its colon, stays whole.
func TestBlockValueRuleKeepsItsNeighbours(t *testing.T) {
	for _, in := range []string{
		`{"password": "x", "u": "KEEPME"}`,
		"{password: [!!str 'x', y], u: KEEPME}",
		"pass: 12 fail: 0   KEEPME",
		"auth: failed for user KEEPME",
		"msg: description: |\n  KEEPME\n",
	} {
		if out := String(in); !strings.Contains(out, "KEEPME") {
			t.Errorf("String(%q) = %q, lost KEEPME", in, out)
		}
	}
	const mount = "MountVolume.SetUp failed for volume \"secret-volume\" : secret \"db-password\" not found"
	if out := String(mount); out != mount {
		t.Errorf("String(%q) = %q", mount, out)
	}
}
