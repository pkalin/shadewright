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

package eval

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/pkalin/shadewright/internal/analysis"
)

// quotesDir holds the recorded replies whose quotes the evidence check refuses, one file per way a quote can
// differ from the block it cites (#474).
const quotesDir = "testdata/quotes"

// quoteCase is one file of quotesDir: a scenario whose context the model is shown, the class the report must
// give each quote, in order, and the replies.
type quoteCase struct {
	Scenario string   `json:"scenario"`
	Classes  []string `json:"classes"`
	recording
}

// Every recorded reply of the quote corpus ends NoEvidence, and the report classifies each of its quotes
// with the cited block and lengths, never the quote's text.
func TestQuoteFailureCorpus(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(quotesDir, "*.yaml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no quote cases in %s: %v", quotesDir, err)
	}
	covered := map[string]bool{}
	for _, file := range files {
		name := strings.TrimSuffix(filepath.Base(file), ".yaml")
		t.Run(name, func(t *testing.T) {
			var c quoteCase
			if err := readStrict(file, &c); err != nil {
				t.Fatal(err)
			}
			scheme, s := mustScenario(t, c.Scenario)
			s.recording = c.recording
			res := run(t, scheme, s)
			if res.Reason != string(analysis.ReasonNoEvidence) {
				t.Fatalf("reason %s, want NoEvidence", res.Reason)
			}
			got := make([]string, 0, len(res.QuoteFailures))
			for _, q := range res.QuoteFailures {
				got = append(got, q.Class)
				covered[q.Class] = true
				if q.QuoteBytes == 0 || (q.Class != string(analysis.QuoteUnknownSource) && (q.Block == "" || q.BlockBytes == 0)) {
					t.Errorf("quote failure %+v lacks its block or lengths", q)
				}
			}
			if !slices.Equal(got, c.Classes) {
				t.Errorf("classes %v, want %v", got, c.Classes)
			}
			// The report holds no quote: neither its JSON nor its Markdown.
			r := &report{Scenarios: []scenarioResult{res}}
			raw, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			md := r.markdown()
			if !strings.Contains(md, "## Quotes refused as evidence") {
				t.Errorf("the Markdown report does not list the refused quotes")
			}
			for _, reply := range c.Replies {
				var body struct {
					Evidence []struct {
						Quote string `json:"quote"`
					} `json:"evidence"`
				}
				// A tool call or an invalid reply holds no quote.
				if json.Unmarshal([]byte(reply.Content), &body) != nil {
					continue
				}
				for _, e := range body.Evidence {
					if len(e.Quote) > 4 && (strings.Contains(string(raw), e.Quote) || strings.Contains(md, e.Quote)) {
						t.Errorf("the report holds the quote %q", e.Quote)
					}
				}
			}
		})
	}
	// Every class but one has a recorded case: no scenario context holds an angle bracket to unescape, which
	// the analysis package's tests cover.
	for _, class := range []analysis.QuoteClass{analysis.QuoteTooShort, analysis.QuoteUnknownSource,
		analysis.QuoteUnquotableSource, analysis.QuoteWrongBlock, analysis.QuoteOutsideData, analysis.QuoteJoinedLines,
		analysis.QuoteWhitespace, analysis.QuoteCase, analysis.QuotePartial, analysis.QuoteNotFound} {
		if !covered[string(class)] {
			t.Errorf("no recorded case of the class %s", class)
		}
	}
}

// A run that did not end NoEvidence adds no section to the Markdown report.
func TestReportWithoutQuoteFailures(t *testing.T) {
	scheme, s := mustScenario(t, scnOOM)
	res := run(t, scheme, s)
	if res.QuoteFailures != nil {
		t.Fatalf("quote failures %+v on a passing run", res.QuoteFailures)
	}
	if md := (&report{Scenarios: []scenarioResult{res}}).markdown(); strings.Contains(md, "Quotes refused") {
		t.Errorf("the Markdown report lists refused quotes for a run without any")
	}
}
