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

	"github.com/pkalin/shadewright/internal/incidentcontext"
	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/redact"
	"github.com/pkalin/shadewright/internal/redact/redacttest"
	"github.com/pkalin/shadewright/internal/tools"
)

// The invalid bytes corpus: the sample secret of every built-in rule (redacttest.Samples) with an invalid UTF-8
// form placed in its key, at its separator, next to and inside its value, read through the entry points that show
// cluster text after making it valid UTF-8 (each run of invalid bytes becomes "?") or that show model text. The
// answer is known by construction: the secret, or the bytes of it on each side of the invalid form, is what a
// reader of the output must not see wherever the same text, made valid UTF-8 first, is decided to hide it. Where
// even that decision shows it (an invalid byte inside a key word or a token's shape leaves no form a rule reads),
// the input is counted and not checked: no reading of the text as shown can hide it. The collector's short entry
// points (fingerprint, symptom, value, module values) are not in it: they read the text as before.

// invalidForms are the invalid UTF-8 forms the corpus places: a lone continuation byte, a byte that starts no
// sequence, a lead byte of a two-byte sequence alone, a three-byte sequence cut after its second byte, and the
// encoding of a lone surrogate (three invalid bytes).
var invalidForms = []struct{ name, bytes string }{
	{"0x80", "\x80"},
	{"0xC0", "\xc0"},
	{"0xFF", "\xff"},
	{"cut sequence", "\xe2\x82"},
	{"lone surrogate", "\xed\xa0\x80"},
}

// Names of entry points of the corpus and of the unchanged table.
const (
	entryRenderLog = "incidentcontext.RenderLog"
	entryModelText = "model text"
)

// invalidCase is one input of the corpus: a sample's text with an invalid form at one place.
type invalidCase struct {
	name string
	text string
	// at is where the invalid form starts in text, n its length.
	at, n int
	// secrets are the parts of the sample's secret a reader must not see: the whole secret, or the two sides of
	// the invalid form when it is inside the secret (each at least 4 bytes; a shorter side shows too little).
	secrets []string
	// shownRepaired is true when the decision on the text made valid UTF-8 shows a part of secrets: the input is
	// not checked.
	shownRepaired bool
}

// invalidCorpusMinPart is the shortest side of a split secret the corpus checks.
const invalidCorpusMinPart = 4

// newInvalidCase is the case of text with the invalid form f at at, whose secret is value[vs:ve] of text before
// the form is placed.
func newInvalidCase(name, text string, at int, f string, vs, ve int) invalidCase {
	value := text[vs:ve]
	secrets := []string{value}
	if at > vs && at < ve {
		secrets = secrets[:0]
		for _, p := range []string{value[:at-vs], value[at-vs:]} {
			if len(p) >= invalidCorpusMinPart {
				secrets = append(secrets, p)
			}
		}
	}
	text = text[:at] + f + text[at:]
	return invalidCase{
		name:          name,
		text:          text,
		at:            at,
		n:             len(f),
		secrets:       secrets,
		shownRepaired: leakedSecret(defaultRedactor().String(strings.ToValidUTF8(text, "?")), secrets) != "",
	}
}

// invalidKeyForms are the key forms of the corpus: a secret key, a separator with blanks and line ends around it,
// and a value, after a prefix. The invalid form goes at each boundary of the parts and inside each.
var invalidKeyForms = struct{ prefixes, keys, seps, values []string }{
	prefixes: []string{"", "\x80\xee \t"},
	keys:     []string{signalAPIKey, fieldsKey, redact.KindToken, "Authorization"},
	seps:     []string{"=", ": ", ":\t", "\n:", " :", " \t:"},
	values:   []string{"sigQz22589xK9wAbc", "Bearer Zq7Kp2Vx9Lm4Wt8R"},
}

// invalidCases returns the corpus, the same on every run.
func invalidCases() []invalidCase {
	var out []invalidCase
	kf := invalidKeyForms
	for _, prefix := range kf.prefixes {
		for _, key := range kf.keys {
			for _, sep := range kf.seps {
				for _, value := range kf.values {
					text := prefix + key + sep + value + " ok"
					ks, ss, vs := len(prefix), len(prefix)+len(key), len(prefix)+len(key)+len(sep)
					ve := vs + len(value)
					places := []int{ks, ks + len(key)/2, ss, ss + len(sep)/2, vs, vs + 1, vs + len(value)/2, ve}
					seen := map[int]bool{}
					for _, at := range places {
						if seen[at] {
							continue
						}
						seen[at] = true
						for _, f := range invalidForms {
							out = append(out, newInvalidCase("key form "+strconv.Quote(text)+" "+f.name+" at "+strconv.Itoa(at),
								text, at, f.bytes, vs, ve))
						}
					}
				}
			}
		}
	}
	for _, s := range redacttest.Samples() {
		k := strings.Index(s.Text, s.Key+s.Secret)
		if k < 0 {
			continue
		}
		ks, vs, ve := k, k+len(s.Key), k+len(s.Key)+len(s.Secret)
		places := map[string]int{
			"before the value": vs,
			"value + 1":        vs + 1,
			"inside the value": vs + len(s.Secret)/2,
			"value - 1":        ve - 1,
			"after the value":  ve,
		}
		if s.Key != "" {
			places["key start"] = ks
			places["inside the key"] = ks + len(s.Key)/2
			places["before the separator"] = vs - 1
			places["key - 2"] = vs - 2
		}
		names := make([]string, 0, len(places))
		for n := range places {
			names = append(names, n)
		}
		slices.Sort(names)
		seen := map[int]bool{}
		for _, place := range names {
			at := places[place]
			for at > 0 && at < len(s.Text) && !utf8.RuneStart(s.Text[at]) {
				at-- // a rune boundary of the sample: the invalid form is the only invalid text
			}
			if at < 0 || at > len(s.Text) || seen[at] {
				continue
			}
			seen[at] = true
			for _, f := range invalidForms {
				out = append(out, newInvalidCase("rule "+strconv.Itoa(s.Rule)+" "+f.name+" "+place, s.Text, at, f.bytes, vs, ve))
			}
		}
	}
	return out
}

// invalidPieces returns the text of c in 1, 2 and 3 pieces (cut at the invalid form, and around it), as values
// as they are and as values of lines.
func invalidPieces(c invalidCase) [][]redact.Piece {
	cuts := [][]int{{}, {c.at}, {c.at, c.at + c.n}}
	out := make([][]redact.Piece, 0, 2*len(cuts))
	for _, form := range []redact.Form{redact.AsIs, redact.Line} {
		for _, cut := range cuts {
			var ps []redact.Piece
			prev := 0
			for _, k := range append(slices.Clone(cut), len(c.text)) {
				if k > prev {
					ps = append(ps, redact.Piece{Text: c.text[prev:k], Data: true, Form: form})
				}
				prev = k
			}
			out = append(out, ps)
		}
	}
	return out
}

// invalidEntries are the entry points of the corpus: the log source, the context builder's value and log
// excerpt, the read tools, the decisions on pieces the read tools take (each answer made valid UTF-8 as the tools
// make it), and the model text the analysis loop stores.
func invalidEntries() map[string]func(c invalidCase) []string {
	r := defaultRedactor()
	text := func(name string) func(c invalidCase) []string {
		f := entryPoints[name]
		return func(c invalidCase) []string { return f(c.text) }
	}
	valid := func(out []string) []string {
		for i := range out {
			out[i] = strings.ToValidUTF8(out[i], "?")
		}
		return []string{strings.Join(out, "")}
	}
	// pieces runs decide on each answer of pieces of a case whose pieces, each made valid UTF-8 first, are decided
	// to hide the secret: an answer of pieces that shows it even then (a private key cut between two values) is a
	// limit of the reading of pieces, not of invalid bytes.
	pieces := func(decide func(ps []redact.Piece, n int) []string) func(c invalidCase) []string {
		return func(c invalidCase) []string {
			var out []string
			for _, ps := range invalidPieces(c) {
				got := valid(decide(ps, len(c.text)))
				if leakedSecret(got[0], c.secrets) != "" {
					// Only an answer that shows the secret is decided again, made valid UTF-8 first.
					repaired := slices.Clone(ps)
					for i := range repaired {
						repaired[i].Text = strings.ToValidUTF8(repaired[i].Text, "?")
					}
					if leakedSecret(strings.Join(decide(repaired, len(c.text)), ""), c.secrets) != "" {
						continue
					}
				}
				out = append(out, got...)
			}
			return out
		}
	}
	m := map[string]func(c invalidCase) []string{
		entryRenderLog: func(c invalidCase) []string {
			ex := providers.LogExcerpt{Text: c.text, CollectedAt: time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC)}
			return []string{incidentcontext.RenderLog(r, false, "log", ex, nil, 4096, 8192).Text}
		},
		"redact.Pieces": pieces(func(ps []redact.Piece, _ int) []string { return r.Pieces(ps) }),
		"redact.PiecesWithin": pieces(func(ps []redact.Piece, n int) []string {
			got, _ := r.PiecesWithin(ps, n)
			return got
		}),
		"redact.PiecesFitting": pieces(func(ps []redact.Piece, n int) []string {
			got, _ := r.PiecesFitting(ps, func(o []string) bool { return len(strings.Join(o, "")) <= n })
			return got
		}),
		// The model text as the analysis loop stores it (shownModelText): redacted as read, then without its
		// controls, then written by encoding/json, which writes each invalid byte as U+FFFD.
		entryModelText: func(c invalidCase) []string {
			return []string{providers.StripControls(redact.WithoutControls(r.StringKeeping(c.text, "hunter")))}
		},
	}
	for _, name := range []string{canonEntryExcerpt, canonEntryValue, tools.NameGetPodLogs, tools.NameListEvents,
		tools.NameGetResource, tools.NameQueryMetrics, tools.NameGetDeliveryStatus, tools.NameCheckMissingReference} {
		m[name] = text(name)
	}
	return m
}

// Every built-in rule's secret with an invalid UTF-8 form in its key, at its separator, next to and inside its
// value, through every entry point that shows cluster text or model text: no output shows a part of the secret
// that the decision on the text made valid UTF-8 hides. Under -race each entry point checks a seeded sample
// (redact.RaceKeeps).
func TestInvalidBytesCorpusHidesEverySecret(t *testing.T) {
	cases := invalidCases()
	limit := 0
	for _, c := range cases {
		if c.shownRepaired {
			limit++
		}
	}
	if len(cases) < 4000 || len(cases)-limit < 3000 {
		t.Fatalf("the corpus has %d inputs, %d checked", len(cases), len(cases)-limit)
	}
	t.Logf("%d inputs, %d not checked: the decision on the text made valid UTF-8 shows the secret too", len(cases), limit)
	for name, f := range invalidEntries() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			shown, ran := 0, 0
			for i, c := range cases {
				if c.shownRepaired || !redact.RaceKeeps(name, i) {
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
			if ran < (len(cases)-limit)/(2*redact.RaceSampleEvery) {
				t.Fatalf("%s: only %d of %d inputs ran", name, ran, len(cases))
			}
			t.Logf("%s: %d inputs", name, ran)
		})
	}
}
