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

package redact_test

import (
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/shadewright-labs/shadewright/internal/incidentcontext"
	"github.com/shadewright-labs/shadewright/internal/providers"
	"github.com/shadewright-labs/shadewright/internal/redact"
	"github.com/shadewright-labs/shadewright/internal/redact/redacttest"
	"github.com/shadewright-labs/shadewright/internal/tools"
)

// The escapes corpus (#1223): the sample secret of every built-in rule (redacttest.Samples) and a set of key forms,
// with a terminal escape sequence placed in the key, at the separator, next to and inside the value, around the key
// and the value as a colouring logger writes them, and cut at the edges of the text as a read can cut them. The
// answer is known by construction: the plain text is the text without the sequences placed (for a sequence cut at
// an edge, the bytes of it a reader sees stay), and the secret, or each side of a sequence placed inside it, is what
// a reader of the output must not see wherever the plain text is decided to hide it. Where even that decision shows
// it the input is counted and not checked: no reading of the escapes can hide it.

// Names and texts the tests of the package repeat.
const (
	entryPieces       = "redact.Pieces"
	entryPiecesWithin = "redact.PiecesWithin"
	authorizationKey  = "Authorization"
	// escapeGreen is the colour a logger writes a value in.
	escapeGreen = "\x1b[32m"
	// escapeValue is the short, plain value of the key forms: no token rule reads it alone.
	escapeValue = "hunter2xyzzy"
)

// escapeForms are the sequences the corpus places, each complete: colours (SGR) and other CSIs in the ESC and the
// C1 forms, a lone ESC, OSC strings ended by BEL and by ST in the ESC and the C1 forms, and two short escapes.
var escapeForms = []struct{ name, bytes string }{
	{"SGR colour", "\x1b[36m"},
	{"SGR reset", "\x1b[0m"},
	{"SGR empty", "\x1b[m"},
	{"SGR two parameters", "\x1b[1;31m"},
	{"SGR 256 colours", "\x1b[38;5;208m"},
	{"erase line", "\x1b[2K"},
	{"private mode", "\x1b[?25l"},
	{"CSI with intermediate", "\x1b[1 q"},
	{"C1 CSI", "\u009b32m"},
	{"lone ESC", "\x1b"},
	{"OSC with BEL", "\x1b]0;title\x07"},
	{"OSC hyperlink with ST", "\x1b]8;;https://example.com/x\x1b\\"},
	{"C1 OSC with C1 ST", "\u009d0;t\u009c"},
	{"charset", "\x1b(B"},
	{"save cursor", "\x1b7"},
}

// escapeEdges are the parts of a sequence a read cut can leave at the start of a text (what is left after the cut)
// and at its end (what came before it). seen is what a reader sees of it, the plain text's part.
var escapeEdges = struct {
	starts, ends []struct{ bytes, seen string }
}{
	starts: []struct{ bytes, seen string }{{"[36m", "[36m"}, {"36m", "36m"}, {"6m", "6m"}, {";1m", ";1m"}, {"m", "m"},
		{"0;title\x07", "0;title"}, {"\\", "\\"}},
	ends: []struct{ bytes, seen string }{{"\x1b", ""}, {"\x1b[", "["}, {"\x1b[3", "[3"}, {"\x1b[38;5", "[38;5"},
		{"\x1b]0;tit", "]0;tit"}, {"\u009b", ""}},
}

// escapeCase is one input of the corpus.
type escapeCase struct {
	name, text string
	// plain is the text as a reader sees it: without the sequences placed, with what a reader sees of a cut one.
	plain string
	// at is where the first sequence placed starts in text, n its length, and plainAt where it was placed in plain.
	at, n, plainAt int
	// secrets are the parts of the secret a reader must not see: the whole secret, or each side of a sequence
	// placed inside it (at least invalidCorpusMinPart bytes).
	secrets []string
	// shownPlain is true when the decision on plain shows a part of secrets: the input is not checked.
	shownPlain bool
}

// newEscapeCase is the case of plain with each insertion placed (at increasing offsets of plain, the sequence's
// bytes in text and the part of it a reader sees in plain), whose secret is plain[vs:ve] before any is placed.
func newEscapeCase(name, plain string, vs, ve int, ins ...escapeInsert) escapeCase {
	value := plain[vs:ve]
	cuts := []int{0}
	for _, in := range ins {
		if in.at > vs && in.at < ve {
			cuts = append(cuts, in.at-vs)
		}
	}
	cuts = append(cuts, len(value))
	secrets := []string{value}
	if len(cuts) > 2 {
		secrets = secrets[:0]
		for i := 1; i < len(cuts); i++ {
			if p := value[cuts[i-1]:cuts[i]]; len(p) >= invalidCorpusMinPart {
				secrets = append(secrets, p)
			}
		}
	}
	var text, shown strings.Builder
	prev := 0
	c := escapeCase{name: name, at: -1}
	for _, in := range ins {
		text.WriteString(plain[prev:in.at])
		shown.WriteString(plain[prev:in.at])
		if c.at < 0 {
			c.at, c.n, c.plainAt = text.Len(), len(in.bytes), shown.Len()
		}
		text.WriteString(in.bytes)
		shown.WriteString(in.seen)
		prev = in.at
	}
	text.WriteString(plain[prev:])
	shown.WriteString(plain[prev:])
	c.text, c.plain, c.secrets = text.String(), shown.String(), secrets
	c.shownPlain = leakedSecret(defaultRedactor().String(c.plain), secrets) != ""
	return c
}

// escapeInsert is one sequence placed at byte at of the plain text: bytes in the text, seen in the plain text.
type escapeInsert struct {
	at          int
	bytes, seen string
}

// escapeKeyForms are the key forms of the corpus: a secret key, a separator and a value after a prefix, the key
// quoted as JSON writes it too.
var escapeKeyForms = struct{ prefixes, keys, seps, values []string }{
	prefixes: []string{"level=info msg=connecting "},
	keys:     []string{signalAPIKey, fieldsKey, "DB_PASSWORD", `"db_password"`, redact.KindToken, authorizationKey},
	seps:     []string{"=", ": ", " :"},
	values:   []string{escapeValue, `"Zq7Kp2Vx9Lm4Wt8R"`},
}

// escapePlaces returns the places of a key form or a sample (key at ks, value from vs to ve, separator before vs
// when the key is not empty), sorted and each once.
func escapePlaces(ks, ss, vs, ve int) []int {
	places := []int{vs, vs + 1, vs + (ve-vs)/2, ve - 1, ve}
	if ss > ks {
		places = append(places, ks, ks+(ss-ks)/2, ss, ss+(vs-ss)/2, vs-1)
	}
	slices.Sort(places)
	return slices.Compact(places)
}

// escapeWraps returns the cases of plain with the key ks:ss and the value vs:ve coloured as a logger colours them:
// the key, the value, both, and both with a sequence cut at each edge of the text.
func escapeWraps(name, plain string, ks, ss, vs, ve int) []escapeCase {
	const on, off = "\x1b[36m", "\x1b[0m"
	key := []escapeInsert{{ks, on, ""}, {ss, off, ""}}
	value := []escapeInsert{{vs, escapeGreen, ""}, {ve, off, ""}}
	out := make([]escapeCase, 0, 3+len(escapeEdges.starts)+len(escapeEdges.ends))
	out = append(out,
		newEscapeCase(name+" key coloured", plain, vs, ve, key...),
		newEscapeCase(name+" value coloured", plain, vs, ve, value...),
		newEscapeCase(name+" key and value coloured", plain, vs, ve, append(slices.Clone(key), value...)...))
	for _, s := range escapeEdges.starts {
		ins := append([]escapeInsert{{0, s.bytes, s.seen}}, key[1:]...)
		if ks > 0 {
			ins = append([]escapeInsert{{0, s.bytes, s.seen}}, key...)
		}
		out = append(out, newEscapeCase(name+" cut start "+strconv.Quote(s.bytes), plain, vs, ve, ins...))
	}
	for _, e := range escapeEdges.ends {
		ins := append(slices.Clone(key), escapeInsert{vs, escapeGreen, ""})
		if ve < len(plain) {
			ins = append(ins, escapeInsert{ve, off, ""})
		}
		ins = append(ins, escapeInsert{len(plain), e.bytes, e.seen})
		out = append(out, newEscapeCase(name+" cut end "+strconv.Quote(e.bytes), plain, vs, ve, ins...))
	}
	return out
}

// escapeCases returns the corpus, the same on every run.
func escapeCases() []escapeCase {
	var out []escapeCase
	place := func(name, plain string, ks, ss, vs, ve int) {
		for _, at := range escapePlaces(ks, ss, vs, ve) {
			for at > 0 && at < len(plain) && !utf8.RuneStart(plain[at]) {
				at-- // a rune boundary of the plain text
			}
			for _, f := range escapeForms {
				out = append(out, newEscapeCase(name+" "+f.name+" at "+strconv.Itoa(at), plain, vs, ve,
					escapeInsert{at, f.bytes, ""}))
			}
		}
		if ss > ks {
			out = append(out, escapeWraps(name, plain, ks, ss, vs, ve)...)
		}
	}
	kf := escapeKeyForms
	for _, prefix := range kf.prefixes {
		for _, key := range kf.keys {
			for _, sep := range kf.seps {
				for _, value := range kf.values {
					plain := prefix + key + sep + value + " ok"
					ks, ss := len(prefix), len(prefix)+len(key)
					vs := ss + len(sep)
					place("key form "+strconv.Quote(plain), plain, ks, ss, vs, vs+len(value))
				}
			}
		}
	}
	for _, s := range redacttest.Samples() {
		k := strings.Index(s.Text, s.Key+s.Secret)
		if k < 0 {
			continue
		}
		ks, vs := k, k+len(s.Key)
		ss := vs
		if s.Key != "" {
			// The separator: the trailing run of the key that is not a letter or a digit.
			for ss > ks && !isWordByte(s.Text[ss-1]) {
				ss--
			}
		}
		place("rule "+strconv.Itoa(s.Rule), s.Text, ks, ss, vs, vs+len(s.Secret))
	}
	// Neighbours: a second secret key right after the first value, with each glue, and a sequence at each place
	// around the boundary. Each secret is a case of its own.
	for _, glue := range []string{" ", ",", "&", ";", `", "`, "\t", "\n", ""} {
		first, second := "password=hunter2xyzzy", "api_key=Zq7Kp2Vx9Lm4Wt8R"
		plain := first + glue + second + " ok"
		v1s, v1e := len("password="), len(first)
		v2s := len(first) + len(glue) + len("api_key=")
		v2e := v2s + len("Zq7Kp2Vx9Lm4Wt8R")
		for _, at := range []int{v1e - 1, v1e, v1e + len(glue), v1e + len(glue) + 3, v2s - 1, v2s, v2s + 1} {
			for _, f := range escapeForms {
				name := "neighbours " + strconv.Quote(glue) + " " + f.name + " at " + strconv.Itoa(at)
				out = append(out,
					newEscapeCase(name+" first", plain, v1s, v1e, escapeInsert{at, f.bytes, ""}),
					newEscapeCase(name+" second", plain, v2s, v2e, escapeInsert{at, f.bytes, ""}))
			}
		}
	}
	// The lines of the issue and of the scan that found it (F1 of 2026-11-28): a logrus line in colours, a JSON key
	// and value in colours, an env key in colours, a key split by a reset, and tokens split by one.
	const on, off = "\x1b[36m", "\x1b[0m"
	akia := "AKIA" + "IOSFODNN7EXAMPLE"
	ghp := "ghp_" + "aB3dE5fG7hI9jK1lM3nO5pQ7rS9tU1vW3xY5"
	directed := []struct {
		name, plain, secret string
		ins                 []escapeInsert
	}{
		{"logrus line", "INFO[0000] connecting password=hunter2xyzzy user=app", escapeValue,
			[]escapeInsert{{0, on, ""}, {4, off, ""}, {22, on, ""}, {30, off, ""}, {44, on, ""}, {48, off, ""}}},
		{"JSON key and value", `"db_password": "hunter2xyzzy"`, escapeValue,
			[]escapeInsert{{0, "\x1b[34m", ""}, {13, off, ""}, {15, escapeGreen, ""}, {29, off, ""}}},
		{"env key", "DB_PASSWORD: hunter2xyzzy", escapeValue, []escapeInsert{{0, on, ""}, {11, off, ""}}},
		{"key split by a reset", "password=hunter2xyzzy", escapeValue, []escapeInsert{{4, off, ""}}},
		{"AWS key split by a reset", "key " + akia, akia, []escapeInsert{{8, off, ""}}},
		{"GitHub token split by a reset", "token " + ghp, ghp, []escapeInsert{{20, off, ""}}},
	}
	for _, d := range directed {
		vs := strings.Index(d.plain, d.secret)
		out = append(out, newEscapeCase(d.name, d.plain, vs, vs+len(d.secret), d.ins...))
	}
	return out
}

// isWordByte reports whether c is an ASCII letter, digit or underscore.
func isWordByte(c byte) bool {
	return c == '_' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// escapePieces returns the text of c as one piece and cut at the first sequence placed and after it, as values as
// they are and as values of lines, each with the same pieces of the plain text (cut where the sequence was placed).
func escapePieces(c escapeCase) (texts, plains [][]redact.Piece) {
	split := func(s string, cut []int, form redact.Form) []redact.Piece {
		var ps []redact.Piece
		prev := 0
		for _, k := range append(slices.Clone(cut), len(s)) {
			if k > prev {
				ps = append(ps, redact.Piece{Text: s[prev:k], Data: true, Form: form})
			}
			prev = k
		}
		return ps
	}
	for _, form := range []redact.Form{redact.AsIs, redact.Line} {
		for _, cut := range [][2][]int{{{}, {}}, {{c.at}, {c.plainAt}}, {{c.at + c.n}, {c.plainAt}}} {
			texts = append(texts, split(c.text, cut[0], form))
			plains = append(plains, split(c.plain, cut[1], form))
		}
	}
	return texts, plains
}

// escapePiecesEvery is the share of the corpus the decisions on pieces check: one input in it. It is prime to the
// number of escapeForms, so the inputs checked take every form at every place in turn.
const escapePiecesEvery = 4

// escapeEntries are the entry points of the corpus: the log source, the context builder's value and log excerpt,
// the read tools, the decisions on pieces the read tools take, and the model text the analysis loop stores.
func escapeEntries() map[string]func(c escapeCase) []string {
	r := defaultRedactor()
	pieces := func(decide func(ps []redact.Piece, n int) []string) func(c escapeCase) []string {
		return func(c escapeCase) []string {
			var out []string
			texts, plains := escapePieces(c)
			for i, ps := range texts {
				if leakedSecret(strings.Join(decide(plains[i], len(c.plain)), ""), c.secrets) != "" {
					continue // the pieces of the plain text show it too: a limit of the reading of pieces
				}
				out = append(out, strings.Join(decide(ps, len(c.text)), ""))
			}
			return out
		}
	}
	m := map[string]func(c escapeCase) []string{
		entryRenderLog: func(c escapeCase) []string {
			ex := providers.LogExcerpt{Text: c.text, CollectedAt: time.Date(2026, 11, 28, 0, 0, 0, 0, time.UTC)}
			return []string{incidentcontext.RenderLog(r, false, "log", ex, nil, 4096, 8192).Text}
		},
		entryPieces: pieces(func(ps []redact.Piece, _ int) []string { return r.Pieces(ps) }),
		entryPiecesWithin: pieces(func(ps []redact.Piece, n int) []string {
			got, _ := r.PiecesWithin(ps, n)
			return got
		}),
		// The model text as the analysis loop stores it: redacted as read, then without its controls.
		entryModelText: func(c escapeCase) []string {
			return []string{providers.StripControls(redact.WithoutControls(r.StringKeeping(c.text)))}
		},
	}
	for _, name := range []string{canonEntryExcerpt, canonEntryValue, tools.NameGetPodLogs, tools.NameListEvents,
		tools.NameGetResource, tools.NameQueryMetrics, tools.NameGetDeliveryStatus, tools.NameCheckMissingReference} {
		f := entryPoints[name]
		m[name] = func(c escapeCase) []string { return f(c.text) }
	}
	return m
}

// Every built-in rule's secret and every key form with a terminal escape sequence in its key, at its separator,
// next to and inside its value, around its key and value, and cut at the edges of the text, through every entry
// point that shows cluster text or model text: no output shows a part of the secret that the decision on the
// plain text hides (#1223). Under -race each entry point checks a seeded sample (redact.RaceKeeps).
func TestEscapesCorpusHidesEverySecret(t *testing.T) {
	cases := escapeCases()
	limit := 0
	for _, c := range cases {
		if c.shownPlain {
			limit++
		}
	}
	if len(cases) < 9000 || len(cases)-limit < 8500 {
		t.Fatalf("the corpus has %d inputs, %d checked", len(cases), len(cases)-limit)
	}
	t.Logf("%d inputs, %d not checked: the decision on the plain text shows the secret too", len(cases), limit)
	for name, f := range escapeEntries() {
		// The decisions on pieces read each input six ways, and twice each: they check one input in
		// escapePiecesEvery, which still meets every form of escapeForms at every place.
		every := 1
		if strings.HasPrefix(name, entryPieces) {
			every = escapePiecesEvery
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			shown, ran := 0, 0
			for i, c := range cases {
				if c.shownPlain || i%every != 0 || !redact.RaceKeeps(name, i/every) {
					continue
				}
				ran++
				for _, out := range f(c) {
					if s := leakedSecret(out, c.secrets); s != "" {
						if shown++; shown <= 5 {
							t.Errorf("%s:\n  in: %q\n out: %q\n shows %q", c.name, c.text, out, s)
						}
						break
					}
				}
			}
			if shown > 0 {
				t.Errorf("%s: %d of %d inputs show a secret", name, shown, ran)
			}
			if ran < (len(cases)-limit)/(2*every*redact.RaceSampleEvery) {
				t.Fatalf("%s: only %d of %d inputs ran", name, ran, len(cases))
			}
			t.Logf("%s: %d inputs", name, ran)
		})
	}
}
