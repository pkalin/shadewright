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
	"cmp"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode"

	"k8s.io/apimachinery/pkg/runtime"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/analysis"
	"github.com/pkalin/shadewright/internal/incidentcontext"
	"github.com/pkalin/shadewright/internal/tools"
)

// The kinds of a quote that lies within no value its cited block shows and touches the summary (#841), in the
// order checkSummaryQuotes tries them. The first three fail the run, whatever block the quote cites: the model
// quoted text code wrote, counted or cut. The last two are only recorded, for a quote cited from the summary:
// the loop's own rules decide whether its result passes, as they do for any other block.
const (
	// summaryQuoteCutNote holds the note of a shortened value, "(N bytes cut)", which code writes.
	summaryQuoteCutNote = "cut-note"
	// summaryQuoteTally takes in text of the absorbed tally, which the analyzer counted and nothing quotes.
	summaryQuoteTally = "tally-line"
	// summaryQuoteCutPart is in one of the Incident's own values, but not in the part of it the summary shows:
	// the summary cut it.
	summaryQuoteCutPart = "cut-part"
	// summaryQuoteNearMiss is cited from the summary and within a value it shows once case, runs of white space
	// and trailing punctuation are ignored: a quote the model changed a little.
	summaryQuoteNearMiss = "near-miss"
	// summaryQuoteNotShown is cited from the summary and none of the above: a label, a line code wrote, two
	// values at once, or a paraphrase.
	summaryQuoteNotShown = "not-shown"
)

// summaryQuoteFails reports whether a quote of the kind fails the run.
func summaryQuoteFails(kind string) bool {
	return kind == summaryQuoteCutNote || kind == summaryQuoteTally || kind == summaryQuoteCutPart
}

// minQuoteBytes is the loop's shortest quote, spaces trimmed, that counts as evidence (analysis.minQuoteBytes):
// the check skips a shorter quote, as the loop does.
const minQuoteBytes = 8

// tallyHeading starts the absorbed tally of a summary (incidentcontext's absorbedLines); every line from it to
// the end of the block is the tally.
const tallyHeading = "consequences counted on this cause"

// cutNoteText is in every note of a shortened value (incidentcontext's cutNote).
const cutNoteText = " bytes cut)"

// cutNotePattern finds the note of a shortened value in a quote normalised by normQuote: after its number, or
// with its closing parenthesis.
var cutNotePattern = regexp.MustCompile(`\d bytes cut\b|\bbytes cut\)`)

// summaryQuote is a quote of the result of one of the kinds above (#841): its kind, never its text, which only
// the reply holds.
type summaryQuote struct {
	// Source is the cited ID.
	Source string `json:"source"`
	// Kind is one of cut-note, tally-line, cut-part, near-miss and not-shown.
	Kind string `json:"kind"`
	// Fails is whether the quote fails the run: true for cut-note, tally-line and cut-part. Always written, so a
	// reader of an earlier build of this schema, which took every entry for a failure, refuses the report.
	Fails bool `json:"fails"`
	// QuoteBytes is the quote's length, spaces trimmed.
	QuoteBytes int `json:"quoteBytes"`
}

// checkSummaryQuotes returns the quotes of ev, from a context of blocks (block i shown as c<i+1>, as
// analysis.contextMessage numbers them) and tool results toolText, that lie within no value their cited block
// shows and touch the summary, each with its kind. The loop checks quotes only when the result proposes, and
// passes a proposal on any one of them; this check covers every quote of every result the loop accepted.
//
// A quote within a value its cited block shows (incidentcontext.Block.Data, as the loop takes it; for a tool
// result, within the text of any tool result of the run) is never reported. Otherwise it fails the run when it
// holds the cut note, a tally line or a part the summary cut, whatever block it cites, and also when it does so
// with a small change: case, runs of white space, trailing punctuation, and one character changed, added or
// removed. A quote cited from the summary that does none of these is recorded as near-miss or not-shown. inc is
// the analyzed Incident, whose values tell a cut part from text the summary never held.
func checkSummaryQuotes(blocks []incidentcontext.Block, toolText []string, inc *remediationv1alpha1.Incident,
	ev []analysis.Evidence) []summaryQuote {
	at := slices.IndexFunc(blocks, func(b incidentcontext.Block) bool {
		return b.Section == incidentcontext.SectionSummary
	})
	if at < 0 {
		return nil
	}
	summaryID := fmt.Sprintf("c%d", at+1)
	shownSummary := shownParts(blocks[at].Data())
	var tally []string
	if i := strings.Index(blocks[at].Text, tallyHeading); i >= 0 {
		for l := range strings.SplitSeq(blocks[at].Text[i:], "\n") {
			if l = normQuote(l); len(l) >= minQuoteBytes {
				tally = append(tally, l)
			}
		}
	}
	s := inc.Spec
	var values []string
	for _, v := range []string{s.Symptom, s.ReportingController, s.Reason, s.Subject.Name} {
		if v != "" {
			values = append(values, normQuote(v), normQuote(tools.EscapeAngles(v)))
		}
	}
	var out []summaryQuote
	for _, e := range ev {
		q := strings.TrimSpace(e.Quote)
		if len(q) < minQuoteBytes {
			continue
		}
		var cited []string
		if n, err := strconv.Atoi(strings.TrimPrefix(e.Source, "c")); err == nil && strings.HasPrefix(e.Source, "c") &&
			n >= 1 && n <= len(blocks) {
			cited = shownParts(blocks[n-1].Data())
		} else if strings.HasPrefix(e.Source, "t") {
			cited = shownParts(toolText)
		}
		if slices.ContainsFunc(cited, func(p string) bool { return strings.Contains(p, q) }) {
			continue
		}
		fromSummary := e.Source == summaryID
		nq := normQuote(q)
		nearCited := slices.ContainsFunc(cited, func(p string) bool { return strings.Contains(normQuote(p), nq) })
		closeCited := nearCited || slices.ContainsFunc(cited, func(p string) bool { return within1(normQuote(p), nq) })
		if closeCited && !fromSummary {
			// Within a value of the block it cites but for a small change: the loop's to judge, not a summary's.
			continue
		}
		kind := ""
		switch {
		case cutNotePattern.MatchString(nq):
			kind = summaryQuoteCutNote
		case !nearCited && slices.ContainsFunc(tally, func(l string) bool { return within1(nq, l) || within1(l, nq) }):
			kind = summaryQuoteTally
		case cutPart(values, shownSummary, nq):
			kind = summaryQuoteCutPart
		case !fromSummary:
			continue
		case nearCited:
			kind = summaryQuoteNearMiss
		default:
			kind = summaryQuoteNotShown
		}
		out = append(out, summaryQuote{Source: e.Source, Kind: kind, Fails: summaryQuoteFails(kind), QuoteBytes: len(q)})
	}
	return out
}

// cutPart reports whether the normalised quote nq takes in text of a value the summary cut: it is in one of the
// Incident's values but not in a part the summary shows, or within one change of a value and of no part shown.
// The first takes a quote that runs one character past a cut; the second a cut part with one character changed.
func cutPart(values, shown []string, nq string) bool {
	inShown := func(match func(t, q string) bool) bool {
		return slices.ContainsFunc(shown, func(p string) bool { return match(normQuote(p), nq) })
	}
	inValue := func(match func(t, q string) bool) bool {
		return slices.ContainsFunc(values, func(v string) bool { return match(v, nq) })
	}
	return (inValue(strings.Contains) && !inShown(strings.Contains)) || (inValue(within1) && !inShown(within1))
}

// shownParts returns the parts with each also as the block shows it, angle brackets escaped.
func shownParts(parts []string) []string {
	out := slices.Clone(parts)
	for _, p := range parts {
		if e := tools.EscapeAngles(p); e != p {
			out = append(out, e)
		}
	}
	return out
}

// normQuote is s in lower case, every run of white space one space, with spaces and trailing punctuation
// trimmed: the changes analysis.classifyQuote reports as whitespace and case, and a full stop or comma added
// at the end.
func normQuote(s string) string {
	s = strings.ToLower(strings.Join(strings.Fields(s), " "))
	return strings.TrimRightFunc(s, func(r rune) bool { return unicode.IsPunct(r) && r != ')' || unicode.IsSpace(r) })
}

// within1 reports whether q is in t with at most one character changed, added or removed. Such a match keeps
// one half of q whole, so each place a half is found is checked for the other half with one edit.
func within1(t, q string) bool {
	if strings.Contains(t, q) {
		return true
	}
	qr := []rune(q)
	if len(qr) < 2 {
		// One character, changed into any of t or removed.
		return true
	}
	left, right := string(qr[:len(qr)/2]), string(qr[len(qr)/2:])
	for i := 0; ; {
		j := strings.Index(t[i:], left)
		if j < 0 {
			break
		}
		if prefixWithin1([]rune(t[i+j+len(left):]), []rune(right)) {
			return true
		}
		i += j + 1
	}
	for i := 0; ; {
		j := strings.Index(t[i:], right)
		if j < 0 {
			break
		}
		if prefixWithin1(reversed([]rune(t[:i+j])), reversed([]rune(left))) {
			return true
		}
		i += j + 1
	}
	return false
}

// prefixWithin1 reports whether s starts with p with at most one character of p changed, added or removed.
func prefixWithin1(s, p []rune) bool {
	k := 0
	for k < len(s) && k < len(p) && s[k] == p[k] {
		k++
	}
	if k == len(p) {
		return true
	}
	hasPrefix := func(s, p []rune) bool { return len(s) >= len(p) && slices.Equal(s[:len(p)], p) }
	return (k+1 <= len(s) && hasPrefix(s[k+1:], p[k+1:])) || hasPrefix(s[k:], p[k+1:]) ||
		(k+1 <= len(s) && hasPrefix(s[k+1:], p[k:]))
}

// reversed returns r in reverse order.
func reversed(r []rune) []rune {
	out := slices.Clone(r)
	slices.Reverse(out)
	return out
}

// summaryQuotesDir holds recorded replies with one quote of each kind (#841), and of the changes a quote of a
// failing kind may carry.
const summaryQuotesDir = "testdata/summary-quotes"

// summaryQuoteCase is one file of summaryQuotesDir: the scenario whose context the model is shown, the kind the
// check must give the reply's one reported quote and the ID that quote cites (c1 when empty), and the replies.
type summaryQuoteCase struct {
	Scenario string `json:"scenario"`
	Kind     string `json:"kind"`
	Cites    string `json:"cites,omitempty"`
	recording
}

// Each recorded reply of summaryQuotesDir is one the loop accepts with its scenario's outcome, diagnosis and
// reads, and has one quote the check reports: of the cut part or the note of unschedulable/over-bound, a tally
// line of oom/absorbed, or a quote of the summary changed a little or not in it. A quote of a failing kind
// fails the run with that kind, whatever block it cites and with a small change; a near-miss or not-shown
// quote is recorded and the run passes. The report names the kind without the quote's text.
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
			want := summaryQuote{Source: cmp.Or(c.Cites, "c1"), Kind: c.Kind, Fails: summaryQuoteFails(c.Kind)}
			got := slices.Clone(res.SummaryQuotes)
			for i := range got {
				if got[i].QuoteBytes < minQuoteBytes {
					t.Errorf("quote %+v is shorter than the shortest quote", got[i])
				}
				got[i].QuoteBytes = 0
			}
			if res.Pass == want.Fails || len(got) != 1 || got[0] != want {
				t.Fatalf("pass %t, summary quotes %+v: want %+v, the run failing: %t", res.Pass, res.SummaryQuotes,
					want, want.Fails)
			}
			covered[c.Kind] = true
			withoutReply := res
			withoutReply.Reply = nil
			r := &report{Scenarios: []scenarioResult{withoutReply}}
			raw, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(raw), `"fails":`+strconv.FormatBool(want.Fails)) {
				t.Errorf("the JSON report does not say whether the quote fails: %s", raw)
			}
			md := r.markdown()
			if !strings.Contains(md, "## Quotes not in the summary as shown") ||
				!strings.Contains(md, fmt.Sprintf("| %s | %t |", c.Kind, want.Fails)) {
				t.Errorf("the Markdown report does not name the kind %s:\n%s", c.Kind, md)
			}
			// The report names the kind and the length, never the quote, which only the reply text holds.
			for _, reply := range c.Replies {
				result, _, err := analysis.DecodeResult(reply.Content)
				if err != nil || result == nil {
					continue
				}
				for _, e := range result.Evidence {
					if q := strings.TrimSpace(e.Quote); e.Source == want.Source &&
						len(q) == res.SummaryQuotes[0].QuoteBytes &&
						(strings.Contains(string(raw), q) || strings.Contains(md, q)) {
						t.Errorf("the report holds the quote %q", q)
					}
				}
			}
		})
	}
	for _, kind := range []string{summaryQuoteCutNote, summaryQuoteTally, summaryQuoteCutPart, summaryQuoteNearMiss,
		summaryQuoteNotShown} {
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
