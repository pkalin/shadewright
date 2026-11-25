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

package redacttest

import (
	"fmt"
	"slices"
	"strings"
)

// The block-value corpus of #840: a YAML block value under a secret key (a literal or folded block scalar
// with every chomping and indentation indicator, a value on the following lines as a scalar, a list, a
// nested map or a list of block scalars, a quoted scalar over several lines and a plain scalar with
// continuation lines), with spaces or tabs, LF or CRLF, the key at the start of a line, after other text on
// the same line, and with every line break flattened to a blank as the Incident context does, then the end of
// the input, a dedented innocent key or a line of prose. The generator places each secret and each kept word
// at a known offset, so the answer is known by construction.

// BlockCase is one input of the corpus with the byte ranges of the secrets the generator placed, which must
// be redacted, and of the text around them that must survive.
type BlockCase struct {
	Name    string
	In      string
	Secrets [][2]int
	Keep    [][2]int
	// Lined is the input before its line breaks were flattened (In itself when it was not).
	Lined string
	// Indicator: the value opens with a block scalar Indicator; AfterText: other text than sequence dashes
	// stands before the key on its line.
	Indicator, AfterText bool
	// BareQuoted: a quoted or plain scalar over several lines under a bare short key.
	BareQuoted bool
}

// blockWriter builds one BlockCase.
type blockWriter struct {
	b    strings.Builder
	c    BlockCase
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
		w.c.Secrets = append(w.c.Secrets, [2]int{start, w.b.Len()})
	}
}

// kept writes s and records that it must survive.
func (w *blockWriter) kept(s string) {
	start := w.b.Len()
	w.b.WriteString(s)
	w.c.Keep = append(w.c.Keep, [2]int{start, w.b.Len()})
}

// line starts a new line at the indentation ind plus n steps.
func (w *blockWriter) line(ind string, n int) {
	w.text(w.nl + ind + strings.Repeat(w.step, n))
}

// blockStyle writes the value of key, whose line has the indentation ind, after the key's colon.
type blockStyle struct {
	name  string
	write func(w *blockWriter, ind string)
	// phrase: the style may hold a multi-word value, which a bare short key (passKey) does not count.
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
	"api-key", "access_key", "private_key", credentialsKey, "credential", "connectionString", "conn_str",
	quotedPasswordKey, "'token'", passKey, authKey, sigKey, "db.pass", "basic_auth", "storageSas", "AZURE_STORAGE_SAS",
}

// blockInnocentKeys are key names that hold no secret, among them words that contain a short key name.
var blockInnocentKeys = []string{
	"description", "script", "message", "config", "data", "notes", "author", "bypass", "signal", "kansas",
	"passing", "authority",
}

var blockShortKeys = []string{authKey, passKey, sigKey, "sas"}
var blockProsePrefixes = []string{"pre", "post", "re", "two", "multi", "single"}

// isBareShortCorpusKey reports whether the corpus key counts only with a value that looks like a secret.
// TestBlockCorpusKeyClasses checks this classification against the redactor.
func isBareShortCorpusKey(key string) bool {
	if slices.Contains(blockShortKeys, strings.ToLower(key)) {
		return true
	}
	prefix, short, ok := strings.Cut(key, "-")
	return ok && slices.Contains(blockShortKeys, short) && slices.Contains(blockProsePrefixes, prefix)
}

// blockCorpus returns the corpus over keys; with secret, the values are secrets (opaque tokens, and
// passphrases where the key counts them), otherwise innocent words.
func blockCorpus(keys []string, secret bool) []BlockCase {
	out := make([]BlockCase, 0, 1<<14)
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
	value func(bool) string) BlockCase {
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
			w.c.Secrets = append(w.c.Secrets, [2]int{start, w.b.Len()})
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
	w.c.In = w.b.String()
	w.c.Lined = w.c.In
	w.c.Indicator = strings.HasPrefix(st.name, "scalar")
	w.c.AfterText = pl.lead != "" && pl.lead != "- "
	w.c.BareQuoted = (strings.HasSuffix(st.name, "quoted over lines") || st.name == "plain continuation") &&
		isBareShortCorpusKey(key)
	if pl.flat {
		w.c.In = strings.Map(func(r rune) rune {
			if r == '\n' || r == '\r' {
				return ' '
			}
			return r
		}, w.c.In)
	}
	w.c.Name = name
	return w.c
}
