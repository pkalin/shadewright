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

package analysis

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/pkalin/shadewright/internal/incidentcontext"
)

// logExcerpt is a log block of two lines, indented as a multi-line message would be.
const logExcerpt = "fatal error: runtime: out of memory\n    goroutine 1 [running]: main.load\n"

// statusBlock is the name of the status section, as a quote failure names it.
const statusBlock = string(incidentcontext.SectionStatus)

// A result refused for want of evidence carries, for each quote, how it differs from what the model saw:
// a class and lengths, never the text (#474). The check itself refuses every one of them.
func TestNoEvidenceClassifiesEachQuote(t *testing.T) {
	cases := map[string]struct {
		source, quote string
		want          QuoteClass
		block         string
	}{
		"too short":       {"c2", "256Mi", QuoteTooShort, statusBlock},
		"unknown source":  {"t1", "container app terminated OOMKilled", QuoteUnknownSource, ""},
		"not available":   {"c4", "no Git source is configured", QuoteUnquotableSource, "git"},
		"wrong block":     {"c1", "container app terminated OOMKilled", QuoteWrongBlock, "summary"},
		"label and value": {"c2", "pod web-1: container app", QuoteOutsideData, statusBlock},
		"unescaped":       {"c3", "main.load <oom>", QuoteUnescaped, "log excerpt"},
		"joined lines":    {"c3", "runtime: out of memory goroutine 1", QuoteJoinedLines, "log excerpt"},
		"whitespace":      {"c2", "memory  limit\t256Mi", QuoteWhitespace, statusBlock},
		"case":            {"c2", "Container App terminated oomkilled", QuoteCase, statusBlock},
		"partial":         {"c2", "container app terminated OOMKilled after a spike...", QuotePartial, statusBlock},
		"paraphrase":      {"c2", "the container ran out of memory", QuoteNotFound, statusBlock},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture()
			f.builder.c.Blocks = append(f.builder.c.Blocks,
				incidentcontext.Block{Section: incidentcontext.SectionLogs, Provenance: incidentcontext.ProvenanceLogExcerpt,
					Availability: incidentcontext.Available, Text: strings.Replace(logExcerpt, "main.load\n",
						"main.load &lt;oom&gt;\n", 1)},
				incidentcontext.Block{Section: incidentcontext.SectionGit, Provenance: incidentcontext.ProvenanceGit,
					Availability: incidentcontext.NotAvailable, Text: "not available: no Git source is configured\n"})
			res := strings.Replace(validResult, `{"source":"c2","quote":"container app terminated OOMKilled"}`,
				`{"source":"`+tc.source+`","quote":`+strconv.Quote(tc.quote)+`}`, 1)
			f.triage.steps = []step{reply(res)}
			out := f.analyze(t, testLimits())
			if out.Reason != ReasonNoEvidence || out.Plan != nil {
				t.Fatalf("reason %s: the quote %q passed the check", out.Reason, tc.quote)
			}
			if len(out.QuoteFailures) != 1 {
				t.Fatalf("quote failures %+v, want one", out.QuoteFailures)
			}
			got := out.QuoteFailures[0]
			if got.Class != tc.want || got.Source != tc.source || got.Block != tc.block ||
				got.QuoteBytes != len(strings.TrimSpace(tc.quote)) {
				t.Errorf("got %+v, want class %s, source %s, block %q", got, tc.want, tc.source, tc.block)
			}
			if tc.want == QuotePartial && got.MatchedBytes != len("container app terminated OOMKilled") {
				t.Errorf("matched %d bytes, want the verbatim prefix", got.MatchedBytes)
			}
			if tc.block != "" && got.BlockBytes == 0 {
				t.Errorf("block bytes 0 for a block the model saw")
			}
		})
	}
}

// Only a refusal for want of evidence carries quote failures: a proposal that passes has none.
func TestProposedHasNoQuoteFailures(t *testing.T) {
	f := newFixture()
	f.triage.steps = []step{reply(validResult)}
	if out := f.analyze(t, testLimits()); out.Reason != ReasonProposed || out.QuoteFailures != nil {
		t.Errorf("reason %s, quote failures %+v", out.Reason, out.QuoteFailures)
	}
}

func TestLongestPrefixKeepsRunes(t *testing.T) {
	if n := longestPrefix("abc é", "abc éx"); n != len("abc é") {
		t.Errorf("got %d, want %d", n, len("abc é"))
	}
	if n := longestPrefix("abc \xc3", "abc é"); n != len("abc ") {
		t.Errorf("a prefix ending inside a rune: got %d, want %d", n, len("abc "))
	}
}

// The prompt and the schema tell the model what the check compares: an exact copy from within one value of
// one block, never a label, a joined line or a paraphrase (#474). Before, they only said "verbatim", and a
// model quoting a whole status line ("reason: OOMKilled (Warning)") was refused.
func TestPromptAndSchemaSayWhatAQuoteIs(t *testing.T) {
	sys := systemPrompt(false, 0)
	for _, want := range []string{`the one block it comes from`, `at least 8 characters copied exactly`,
		`Copy from within one value the block shows`, `without the label`,
		`from the line "reason: OOMKilled (Warning)" quote "OOMKilled", not the whole line`,
		`Do not paraphrase, join lines, change spacing or case`} {
		if !strings.Contains(sys, want) {
			t.Errorf("the system prompt does not say %q", want)
		}
	}
	var schema struct {
		Properties struct {
			Evidence struct {
				Items struct {
					Properties map[string]struct {
						Description string `json:"description"`
					} `json:"properties"`
				} `json:"items"`
			} `json:"evidence"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(ResponseSchema(), &schema); err != nil {
		t.Fatal(err)
	}
	props := schema.Properties.Evidence.Items.Properties
	if d := props["quote"].Description; !strings.Contains(d, "copied exactly") || !strings.Contains(d, "one value") ||
		!strings.Contains(d, "without its label") {
		t.Errorf("the quote's schema description %q does not say what the check compares", d)
	}
	if d := props["source"].Description; !strings.Contains(d, "one block") {
		t.Errorf("the source's schema description %q does not name one block", d)
	}
}

// exampleQuote is the value the prompt's example quotes.
const exampleQuote = "OOMKilled"

// The example the prompt gives is what the check accepts: the value, not its line.
func TestThePromptsExampleQuoteIsEvidence(t *testing.T) {
	for quote, want := range map[string]Reason{exampleQuote: ReasonProposed, "reason " + exampleQuote: ReasonNoEvidence} {
		f := newFixture()
		res := strings.Replace(validResult, `{"source":"c2","quote":"container app terminated OOMKilled"}`,
			`{"source":"c1","quote":`+strconv.Quote(quote)+`}`, 1)
		f.triage.steps = []step{reply(res)}
		if out := f.analyze(t, testLimits()); out.Reason != want {
			t.Errorf("quote %q: reason %s, want %s", quote, out.Reason, want)
		}
	}
}
