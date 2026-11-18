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
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/analysis"
	"github.com/pkalin/shadewright/internal/incidentcontext"
	"github.com/pkalin/shadewright/internal/tools"
)

// Generated tests of checkSummaryQuotes (#841): against the check as it was first built, so nothing it failed is
// lost, and with quotes whose kind is known by construction.

// editsToSubstring is the fewest characters to change, add or remove in q to make it a substring of t (Sellers'
// algorithm): the oracle within1 is checked against.
func editsToSubstring(t, q string) int {
	tr, qr := []rune(t), []rune(q)
	prev := make([]int, len(tr)+1) // a match may start anywhere in t: row 0 is all 0
	cur := make([]int, len(tr)+1)
	for i := 1; i <= len(qr); i++ {
		cur[0] = i
		for j := 1; j <= len(tr); j++ {
			sub := prev[j-1]
			if qr[i-1] != tr[j-1] {
				sub++
			}
			cur[j] = min(sub, prev[j]+1, cur[j-1]+1)
		}
		prev, cur = cur, prev
	}
	return slices.Min(prev)
}

// within1 agrees with the edit distance on random text over a small alphabet, multi-byte characters included,
// where near matches are common.
func TestWithin1MatchesEditDistance(t *testing.T) {
	r := rand.New(rand.NewPCG(841, 1)) // #nosec G404 -- a seeded test corpus, repeatable on purpose
	alphabet := []rune("ab é𠀋")
	word := func(n int) string {
		out := make([]rune, n)
		for i := range out {
			out[i] = alphabet[r.IntN(len(alphabet))]
		}
		return string(out)
	}
	for range 20000 {
		text, q := word(r.IntN(14)), word(2+r.IntN(6))
		if r.IntN(2) == 0 && len(text) > 0 {
			// A quote taken from the text and changed once, so matches within one change are common.
			tr := []rune(text)
			from := r.IntN(len(tr))
			qr := slices.Clone(tr[from:min(len(tr), from+2+r.IntN(6))])
			at := r.IntN(len(qr))
			switch r.IntN(3) {
			case 0:
				qr[at] = alphabet[r.IntN(len(alphabet))]
			case 1:
				qr = slices.Insert(qr, at, alphabet[r.IntN(len(alphabet))])
			default:
				qr = slices.Delete(qr, at, at+1)
			}
			q = string(qr)
		}
		if got, want := within1(text, q), editsToSubstring(text, q) <= 1; got != want {
			t.Fatalf("within1(%q, %q) = %t, edit distance %d", text, q, got, editsToSubstring(text, q))
		}
	}
}

// checkSummaryQuotesFirst is checkSummaryQuotes as first built on this branch: quotes cited from the summary
// only, matched byte for byte, every kind failing. Kept to check that the check fails at least what it failed.
func checkSummaryQuotesFirst(blk *incidentcontext.Block, id string, inc *remediationv1alpha1.Incident,
	ev []analysis.Evidence) []string {
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
	var out []string
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
		out = append(out, kind)
	}
	return out
}

// windows returns every substring of s of the given lengths in bytes that starts and ends on a character.
func windows(s string, lengths ...int) []string {
	var out []string
	for i := range len(s) {
		if !utf8RuneStart(s, i) {
			continue
		}
		for _, n := range lengths {
			if i+n <= len(s) && (i+n == len(s) || utf8RuneStart(s, i+n)) {
				out = append(out, s[i:i+n])
			}
		}
	}
	return out
}

func utf8RuneStart(s string, i int) bool { return s[i]&0xC0 != 0x80 }

// Every quote of the summary's text and of the Incident's values, cited from the summary, of every scenario: a
// quote the first check failed (cut note, tally line, cut part) still fails, with the same kind, unless it is
// shorter than the loop's shortest quote or in a value shown but for case and trailing punctuation (a
// near-miss); a quote it reported as not shown is still reported; a quote it passed
// is still not reported.
func TestSummaryQuoteCheckFailsWhatItFailedFirst(t *testing.T) {
	scheme, err := newScheme()
	if err != nil {
		t.Fatal(err)
	}
	scenarios, err := loadScenarios(scheme)
	if err != nil {
		t.Fatal(err)
	}
	failed := 0
	for _, s := range scenarios {
		blocks := contextOf(t, s, s.limits)
		if len(blocks) == 0 || blocks[0].Section != incidentcontext.SectionSummary {
			continue
		}
		spec := s.incident.Spec
		texts := []string{blocks[0].Text, spec.Symptom, spec.ReportingController, spec.Reason, spec.Subject.Name}
		seen := map[string]bool{}
		for _, text := range texts {
			for _, q := range windows(text, 8, 13, 21, 40, 90) {
				if seen[q] {
					continue
				}
				seen[q] = true
				ev := []analysis.Evidence{{Source: "c1", Quote: q}}
				before := checkSummaryQuotesFirst(&blocks[0], "c1", s.incident, ev)
				now := checkSummaryQuotes(blocks, nil, s.incident, ev)
				switch {
				case len(strings.TrimSpace(q)) < minQuoteBytes:
					if len(now) != 0 {
						t.Errorf("%s: %q is shorter than the shortest quote and reported: %+v", s.name, q, now)
					}
				case len(before) == 0:
					if len(now) != 0 {
						t.Errorf("%s: %q passed the first check and is reported now: %+v", s.name, q, now)
					}
				case summaryQuoteFails(before[0]) && slices.ContainsFunc(shownParts(blocks[0].Data()),
					func(p string) bool { return strings.Contains(normQuote(p), normQuote(q)) }):
					// The same text, but for case or trailing punctuation, is in a value shown: a near-miss
					// now, which the loop judges (#841, decision 2).
					if len(now) != 1 || now[0].Kind != summaryQuoteNearMiss {
						t.Errorf("%s: %q failed the first check as %s, is shown but for case, now %+v", s.name, q,
							before[0], now)
					}
				case summaryQuoteFails(before[0]):
					failed++
					if len(now) != 1 || now[0].Kind != before[0] || !now[0].Fails {
						t.Errorf("%s: %q failed the first check as %s, now %+v", s.name, q, before[0], now)
					}
				default:
					if len(now) != 1 {
						t.Errorf("%s: %q was reported as %s by the first check, now %+v", s.name, q, before[0], now)
					}
				}
			}
		}
	}
	if failed == 0 {
		t.Fatal("no generated quote failed the first check: the corpus checks nothing")
	}
	t.Logf("%d generated quotes failed the first check", failed)
}

// changes are the small changes a model makes to a quote, which neither hide a failing kind nor fail a quote of
// a value shown: none, case, a run of white space, a trailing full stop, one character changed, and one added.
var changes = map[string]func(string) string{
	"as is":        func(q string) string { return q },
	"upper case":   strings.ToUpper,
	"white space":  func(q string) string { return strings.Replace(q, " ", " \t  ", 1) },
	"full stop":    func(q string) string { return q + "." },
	"one changed":  func(q string) string { return swapAt(q, len([]rune(q))/2) },
	"one added":    func(q string) string { r := []rune(q); return string(slices.Insert(r, len(r)/2, 'q')) },
	"case and dot": func(q string) string { return strings.ToLower(q) + "." },
}

// swapAt changes the character at i to another one.
func swapAt(q string, i int) string {
	r := []rune(q)
	if r[i] == 'x' {
		r[i] = 'y'
	} else {
		r[i] = 'x'
	}
	return string(r)
}

// checkOne runs checkSummaryQuotes on one quote cited from id.
func checkOne(blocks []incidentcontext.Block, s *scenario, id, q string) []summaryQuote {
	return checkSummaryQuotes(blocks, nil, s.incident, []analysis.Evidence{{Source: id, Quote: q}})
}

// expectKind fails t unless got is one quote of kind, failing as the kind does; kind "" expects none.
func expectKind(t *testing.T, what string, got []summaryQuote, kind string) {
	t.Helper()
	switch {
	case kind == "" && len(got) != 0:
		t.Errorf("%s: reported %+v, want nothing", what, got)
	case kind != "" && (len(got) != 1 || got[0].Kind != kind || got[0].Fails != summaryQuoteFails(kind)):
		t.Errorf("%s: reported %+v, want one %s", what, got, kind)
	}
}

// Quotes of unschedulable/over-bound whose kind is known by construction: a part of the symptom the summary cut,
// a quote across the cut and the cut's note fail the run with any small change and whatever block they cite but
// the pod's status (c2), which shows the whole message; a part the summary shows passes cited from it, and is
// recorded as a near-miss with its case or white space changed. A quote shorter than the loop's shortest is
// never reported.
func TestSummaryQuoteCheckCutByConstruction(t *testing.T) {
	_, s := mustVariantOrScenario(t, overBoundVariant)
	blocks := contextOf(t, s, s.limits)
	symptom := s.incident.Spec.Symptom
	wide := incidentcontext.DefaultLimits()
	wide.Summary = incidentcontext.MaxLimitBytes
	head := shortenedSymptom(t, summaryOf(t, s, wide).Text, blocks[0].Text, symptom)
	cut := symptom[len(head):]
	lowHead := strings.ToLower(head)
	if !slices.ContainsFunc(blocks[1].Data(), func(p string) bool { return strings.Contains(p, symptom) }) {
		t.Fatalf("c2 does not show the whole symptom: %q", blocks[1].Data())
	}

	// Parts of the cut, far enough from anything shown that no change of one character makes them shown.
	n := 0
	for _, q := range windows(cut, 12, 25, 44) {
		if q != strings.TrimSpace(q) || editsToSubstring(lowHead, strings.ToLower(q)) <= 3 {
			continue
		}
		n++
		for name, change := range changes {
			cq := change(q)
			for _, id := range []string{"c1", "c4", "c6", "t1", "c99"} {
				expectKind(t, fmt.Sprintf("cut part %q %s from %s", q, name, id), checkOne(blocks, s, id, cq),
					summaryQuoteCutPart)
			}
			expectKind(t, fmt.Sprintf("cut part %q %s from c2", q, name), checkOne(blocks, s, "c2", cq), "")
		}
	}
	if n < 20 {
		t.Fatalf("only %d parts of the cut to quote", n)
	}

	// Across the cut: the end of the head and 1 to 12 bytes of the cut.
	for k := 1; k <= 12; k++ {
		q := head[len(head)-20:] + cut[:k]
		if q != strings.TrimSpace(q) {
			continue
		}
		for _, name := range []string{"as is", "upper case", "full stop"} {
			for _, id := range []string{"c1", "c4"} {
				expectKind(t, fmt.Sprintf("across the cut %q %s from %s", q, name, id),
					checkOne(blocks, s, id, changes[name](q)), summaryQuoteCutPart)
			}
		}
	}

	// The note, alone or with the end of the head, in its forms.
	note := fmt.Sprintf("(%d bytes cut)", len(cut))
	for _, q := range []string{note, head[len(head)-12:] + " " + note, strings.Trim(note, "()"), strings.ToUpper(note)} {
		for _, id := range []string{"c1", "c2", "c4"} {
			expectKind(t, fmt.Sprintf("note %q from %s", q, id), checkOne(blocks, s, id, q), summaryQuoteCutNote)
		}
	}

	// Parts of the head, which the summary shows: none cited as they are; near-misses with a change of case or
	// white space, which pass the run.
	for _, q := range windows(head, 9, 30) {
		if q != strings.TrimSpace(q) || strings.Contains(strings.ToLower(cut), strings.ToLower(q)) {
			continue
		}
		expectKind(t, fmt.Sprintf("shown %q", q), checkOne(blocks, s, "c1", q), "")
		expectKind(t, fmt.Sprintf("shown %q upper case", q), checkOne(blocks, s, "c1", strings.ToUpper(q)),
			summaryQuoteNearMiss)
		if strings.Contains(q, " ") {
			expectKind(t, fmt.Sprintf("shown %q white space", q), checkOne(blocks, s, "c1", changes["white space"](q)),
				summaryQuoteNearMiss)
		}
		// Cited from another block that does not show it: the loop's to judge.
		expectKind(t, fmt.Sprintf("shown %q from c4", q), checkOne(blocks, s, "c4", q), "")
	}

	// Shorter than the loop's shortest quote: skipped, as the loop skips it.
	for _, q := range windows(cut, 1, 4, minQuoteBytes-1) {
		expectKind(t, fmt.Sprintf("short %q", q), checkOne(blocks, s, "c1", q), "")
	}
}

// Quotes of oom/absorbed whose kind is known by construction: each line of the absorbed tally, and a part of
// one, fail the run with any small change and from any block; the summary's own values pass.
func TestSummaryQuoteCheckTallyByConstruction(t *testing.T) {
	_, s := mustVariantOrScenario(t, "oom/absorbed")
	blocks := contextOf(t, s, s.limits)
	text := blocks[0].Text
	at := strings.Index(text, tallyHeading)
	if at < 0 {
		t.Fatalf("no tally in the summary:\n%s", text)
	}
	var lines []string
	for l := range strings.SplitSeq(text[at:], "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) < 3 {
		t.Fatalf("the tally has %d lines: %q", len(lines), lines)
	}
	for _, line := range lines {
		for _, q := range []string{line, line[:len(line)/2], line[len(line)/3:]} {
			q = strings.TrimSpace(q)
			for name, change := range changes {
				for _, id := range []string{"c1", "c2", "c4", "t1"} {
					expectKind(t, fmt.Sprintf("tally %q %s from %s", q, name, id), checkOne(blocks, s, id, change(q)),
						summaryQuoteTally)
				}
			}
		}
	}
	for _, v := range blocks[0].Data() {
		if len(v) >= minQuoteBytes {
			expectKind(t, fmt.Sprintf("value %q", v), checkOne(blocks, s, "c1", v), "")
		}
	}
}
