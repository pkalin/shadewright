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
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/pkalin/shadewright/internal/incidentcontext"
	"github.com/pkalin/shadewright/internal/redact"
)

// overBoundVariant is the variant whose summary is over its bound with no absorbed tally (#841).
const overBoundVariant = "unschedulable/over-bound"

// summaryOf builds s's context with the real builder and limits, and returns its summary block.
func summaryOf(t *testing.T, s *scenario, limits incidentcontext.Limits) incidentcontext.Block {
	t.Helper()
	blocks := contextOf(t, s, limits)
	if len(blocks) == 0 || blocks[0].Section != incidentcontext.SectionSummary {
		t.Fatalf("the context's first block is not the summary: %+v", blocks)
	}
	return blocks[0]
}

// contextOf builds s's context with the real builder and limits, and returns its blocks, block i shown as
// c<i+1>.
func contextOf(t *testing.T, s *scenario, limits incidentcontext.Limits) []incidentcontext.Block {
	t.Helper()
	scheme, err := newScheme()
	if err != nil {
		t.Fatal(err)
	}
	redactor, err := redact.New()
	if err != nil {
		t.Fatal(err)
	}
	cfg := s.contextConfig(newClient(scheme, s.clusterObjects()), redactor, s.metricsSource(), s.logSource())
	cfg.Limits = limits
	b, err := incidentcontext.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	c, err := b.Build(t.Context(), s.incident.DeepCopy())
	if err != nil {
		t.Fatal(err)
	}
	return c.Blocks
}

// TestOverBoundSummaryVariant checks that unschedulable/over-bound exercises the shortened summary of #796 for an
// Incident without an absorbed tally, measured with the product's default summary bound, that the cut falls on a
// character boundary, that its recorded reply passes, and that the part cut from the summary is not quotable
// from it.
func TestOverBoundSummaryVariant(t *testing.T) {
	scheme, s := mustVariantOrScenario(t, overBoundVariant)
	if len(s.incident.Status.Absorbed) != 0 {
		t.Fatalf("%s has an absorbed tally; it must have none", s.name)
	}
	if s.limits != (incidentcontext.Limits{}) {
		t.Fatalf("%s has its own bounds %+v; it must run with the product's defaults", s.name, s.limits)
	}
	def := incidentcontext.DefaultLimits()
	bound := def.Summary

	// Before shortening: the same Incident with room for every value.
	wide := def
	wide.Summary = incidentcontext.MaxLimitBytes
	full := summaryOf(t, s, wide).Text
	if len(full) <= bound || strings.Contains(full, cutNoteText) {
		t.Fatalf("the unshortened summary is %d bytes, not over the default bound %d:\n%s", len(full), bound, full)
	}

	// After shortening, at the default bound: within it, valid UTF-8, every line kept, only the symptom cut,
	// with its note.
	blk := summaryOf(t, s, def)
	shown := blk.Text
	if len(shown) > bound {
		t.Fatalf("the shortened summary is %d bytes, over the bound %d", len(shown), bound)
	}
	if !utf8.ValidString(shown) {
		t.Fatalf("the shortened summary is not valid UTF-8: a cut fell inside a character")
	}
	t.Logf("summary: %d bytes unshortened, %d shown, bound %d", len(full), len(shown), bound)
	symptom := s.incident.Spec.Symptom
	head := shortenedSymptom(t, full, shown, symptom)
	if !strings.Contains(head, "12 Insufficient cpu") {
		t.Errorf("the cause is not in the part of the symptom shown: %q", head)
	}
	cut := symptom[len(head):]

	// The recorded reply passes on the shortened summary, and the prompt shows it.
	res := run(t, scheme, s)
	if !res.Pass {
		t.Fatalf("%s failed: outcome %+v, diagnosis %+v, summary quotes %+v", s.name, res.Outcome, res.Diagnosis,
			res.SummaryQuotes)
	}
	var prompt strings.Builder
	for _, m := range res.prompt {
		prompt.WriteString(m.Content)
	}
	if !strings.Contains(prompt.String(), "symptom: "+head+" (") {
		t.Errorf("the prompt does not show the shortened symptom")
	}

	// The head of the symptom is quotable (Block.Data, as analysis.contextMessage takes it); no quotable part
	// holds the end of the cut part, a quote across the cut or the note.
	parts := blk.Data()
	if !slices.Contains(parts, head) {
		t.Errorf("the head of the symptom is not quotable: %q", parts)
	}
	for name, quote := range map[string]string{
		"cut part":   strings.TrimSuffix(cut[len(cut)-30:], "."),
		"across cut": head[len(head)-20:] + cut[:min(20, len(cut))],
		"note":       fmt.Sprintf("(%d bytes cut)", len(cut)),
	} {
		for _, p := range parts {
			if strings.Contains(p, quote) {
				t.Errorf("%s: the quote %q is quotable from the summary: %q", name, quote, p)
			}
		}
	}
}

// shortenedSymptom checks that shown is full with only its symptom line shortened, to a head of symptom followed
// by the note of the bytes cut, and returns that head.
func shortenedSymptom(t *testing.T, full, shown, symptom string) string {
	t.Helper()
	fullLines, shownLines := strings.Split(full, "\n"), strings.Split(shown, "\n")
	if len(fullLines) != len(shownLines) {
		t.Fatalf("shortening changed the lines: %d before, %d after:\n%s", len(fullLines), len(shownLines), shown)
	}
	head := ""
	for i, l := range shownLines {
		if !strings.HasPrefix(l, "symptom: ") {
			if l != fullLines[i] {
				t.Errorf("line %d changed: %q, before %q", i, l, fullLines[i])
			}
			continue
		}
		v := strings.TrimPrefix(l, "symptom: ")
		at := strings.LastIndex(v, " (")
		if at < 0 {
			t.Fatalf("the symptom line %q has no note", l)
		}
		head = v[:at]
		if !strings.HasPrefix(symptom, head) || len(head) == len(symptom) ||
			v[at:] != fmt.Sprintf(" (%d bytes cut)", len(symptom)-len(head)) {
			t.Fatalf("the symptom line %q is not a head of the symptom with its note", l)
		}
	}
	if head == "" {
		t.Fatalf("no shortened symptom line:\n%s", shown)
	}
	return head
}
