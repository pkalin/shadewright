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

	"k8s.io/apimachinery/pkg/runtime"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/analysis"
	"github.com/pkalin/shadewright/internal/incidentcontext"
	"github.com/pkalin/shadewright/internal/tools"
)

// The kinds of a quote cited from the summary that is not within a value the summary shows (#841), in the
// order checkSummaryQuotes tries them.
const (
	// summaryQuoteCutNote holds the note of a shortened value, "(N bytes cut)", which code writes.
	summaryQuoteCutNote = "cut-note"
	// summaryQuoteTally takes in text of the absorbed tally, which the analyzer counted and nothing quotes.
	summaryQuoteTally = "tally-line"
	// summaryQuoteCutPart is in one of the Incident's own values, but not in the part of it the summary shows:
	// the summary cut it.
	summaryQuoteCutPart = "cut-part"
	// summaryQuoteNotShown is none of the above and not within a value shown: a label, a line code wrote, two
	// values at once, or a paraphrase.
	summaryQuoteNotShown = "not-shown"
)

// tallyHeading starts the absorbed tally of a summary (incidentcontext's absorbedLines); every line from it to
// the end of the block is the tally.
const tallyHeading = "consequences counted on this cause"

// cutNoteText is in every note of a shortened value (incidentcontext's cutNote).
const cutNoteText = " bytes cut)"

// summaryQuote is a quote of the result that cites the summary and is not within a value the summary showed
// the model (#841): its kind, never its text, which only the reply holds.
type summaryQuote struct {
	// Source is the cited ID (the summary's, c1).
	Source string `json:"source"`
	// Kind is one of cut-note, tally-line, cut-part and not-shown.
	Kind string `json:"kind"`
	// QuoteBytes is the quote's length, spaces trimmed.
	QuoteBytes int `json:"quoteBytes"`
}

// checkSummaryQuotes returns the quotes of ev that cite the summary block blk, shown under id, and lie within no
// value the block shows (incidentcontext.Block.Data, as analysis.contextMessage shows them), each with its kind.
// The loop checks quotes only when the result proposes, and passes a proposal on any one of them; this check
// covers every quote of every result the loop accepted, so a quote of the part the summary cut, of its note or
// of the tally fails the run whatever its outcome. inc is the analyzed Incident, whose values tell a cut part
// from text the summary never held.
func checkSummaryQuotes(blk *incidentcontext.Block, id string, inc *remediationv1alpha1.Incident,
	ev []analysis.Evidence) []summaryQuote {
	if blk == nil {
		return nil
	}
	parts := blk.Data()
	var tally []string
	if at := strings.Index(blk.Text, tallyHeading); at >= 0 {
		for l := range strings.SplitSeq(blk.Text[at:], "\n") {
			if l = strings.TrimSpace(l); l != "" {
				tally = append(tally, l)
			}
		}
	}
	s := inc.Spec
	values := []string{s.Symptom, s.ReportingController, s.Reason, s.Subject.Name}
	var out []summaryQuote
	for _, e := range ev {
		q := strings.TrimSpace(e.Quote)
		if e.Source != id || q == "" {
			continue
		}
		if slices.ContainsFunc(parts, func(p string) bool {
			return strings.Contains(p, q) || strings.Contains(tools.EscapeAngles(p), q)
		}) {
			continue
		}
		kind := summaryQuoteNotShown
		switch {
		case strings.Contains(q, strings.TrimSpace(cutNoteText)):
			kind = summaryQuoteCutNote
		case slices.ContainsFunc(tally, func(l string) bool { return strings.Contains(l, q) || strings.Contains(q, l) }):
			kind = summaryQuoteTally
		case slices.ContainsFunc(values, func(v string) bool { return v != "" && strings.Contains(v, q) }):
			kind = summaryQuoteCutPart
		}
		out = append(out, summaryQuote{Source: e.Source, Kind: kind, QuoteBytes: len(q)})
	}
	return out
}

// summaryQuotesDir holds recorded replies that quote, from the summary, text it does not show as a value
// (#841), one file per kind.
const summaryQuotesDir = "testdata/summary-quotes"

// summaryQuoteCase is one file of summaryQuotesDir: the scenario whose context the model is shown, the kind the
// check must give the reply's one quote of the summary that is not shown, and the replies.
type summaryQuoteCase struct {
	Scenario string `json:"scenario"`
	Kind     string `json:"kind"`
	recording
}

// Each recorded reply of summaryQuotesDir is one the loop accepts with its scenario's outcome, diagnosis and
// reads, and fails only on its quote of the summary: the cut part or the note of unschedulable/over-bound, a
// tally line of oom/absorbed. The run fails with that kind, and the report names it without the quote's text.
func TestSummaryQuoteCorpus(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(summaryQuotesDir, "*.yaml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no cases in %s: %v", summaryQuotesDir, err)
	}
	covered := map[string]bool{}
	for _, file := range files {
		name := strings.TrimSuffix(filepath.Base(file), ".yaml")
		t.Run(name, func(t *testing.T) {
			var c summaryQuoteCase
			if err := readStrict(file, &c); err != nil {
				t.Fatal(err)
			}
			scheme, s := mustVariantOrScenario(t, c.Scenario)
			s.recording = c.recording
			res := run(t, scheme, s)
			if !res.Outcome.Pass || !res.Diagnosis.Pass || !res.Reads.Pass {
				t.Fatalf("the reply fails on more than its quote: outcome %+v, diagnosis %+v, reads %+v", res.Outcome,
					res.Diagnosis, res.Reads)
			}
			want := summaryQuote{Source: "c1", Kind: c.Kind}
			got := slices.Clone(res.SummaryQuotes)
			for i := range got {
				if got[i].QuoteBytes == 0 {
					t.Errorf("quote %+v has no length", got[i])
				}
			}
			if res.Pass || len(got) != 1 || got[0].Source != want.Source || got[0].Kind != want.Kind {
				t.Fatalf("pass %t, summary quotes %+v: want a failure with %+v", res.Pass, res.SummaryQuotes, want)
			}
			covered[c.Kind] = true
			withoutReply := res
			withoutReply.Reply = nil
			r := &report{Scenarios: []scenarioResult{withoutReply}}
			raw, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			md := r.markdown()
			if !strings.Contains(md, "## Quotes not in the summary as shown") || !strings.Contains(md, "| "+c.Kind+" |") {
				t.Errorf("the Markdown report does not name the kind %s:\n%s", c.Kind, md)
			}
			// The report names the kind and the length, never the quote, which only the reply text holds.
			for _, reply := range c.Replies {
				result, _, err := analysis.DecodeResult(reply.Content)
				if err != nil || result == nil {
					continue
				}
				for _, e := range result.Evidence {
					if q := strings.TrimSpace(e.Quote); e.Source == "c1" && len(q) == got[0].QuoteBytes &&
						(strings.Contains(string(raw), q) || strings.Contains(md, q)) {
						t.Errorf("the report holds the quote %q", q)
					}
				}
			}
		})
	}
	for _, kind := range []string{summaryQuoteCutNote, summaryQuoteTally, summaryQuoteCutPart} {
		if !covered[kind] {
			t.Errorf("no recorded case of the kind %s", kind)
		}
	}
}

// mustVariantOrScenario loads the scenario or variant (<scenario>/<variant>) named name.
func mustVariantOrScenario(t *testing.T, name string) (*runtime.Scheme, *scenario) {
	t.Helper()
	scheme, err := newScheme()
	if err != nil {
		t.Fatal(err)
	}
	scenarios, err := loadScenarios(scheme)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range scenarios {
		if s.name == name {
			return scheme, s
		}
	}
	t.Fatalf("no scenario %s", name)
	return nil, nil
}
