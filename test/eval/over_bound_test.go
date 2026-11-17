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
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/pkalin/shadewright/internal/incidentcontext"
	"github.com/pkalin/shadewright/internal/redact"
)

// overBoundVariant is the variant whose summary is over its bound with no absorbed tally (#841).
const overBoundVariant = "unschedulable/over-bound"

// summaryOf builds s's context with the real builder and limits, and returns its summary block.
func summaryOf(t *testing.T, s *scenario, limits incidentcontext.Limits) incidentcontext.Block {
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
	if len(c.Blocks) == 0 || c.Blocks[0].Section != incidentcontext.SectionSummary {
		t.Fatalf("the context's first block is not the summary: %+v", c.Blocks)
	}
	return c.Blocks[0]
}

// TestOverBoundSummaryVariant checks that unschedulable/over-bound exercises the shortened summary of #796 for an
// Incident without an absorbed tally, measured with the product's own bounds, that its recorded reply passes, and
// that the part cut from the summary is not quotable from it.
func TestOverBoundSummaryVariant(t *testing.T) {
	scheme, err := newScheme()
	if err != nil {
		t.Fatal(err)
	}
	scenarios, err := loadScenarios(scheme)
	if err != nil {
		t.Fatal(err)
	}
	var s *scenario
	for _, sc := range scenarios {
		if sc.name == overBoundVariant {
			s = sc
		}
	}
	if s == nil {
		t.Fatalf("no scenario %s", overBoundVariant)
	}
	if len(s.incident.Status.Absorbed) != 0 {
		t.Fatalf("%s has an absorbed tally; it must have none", s.name)
	}
	bound := s.limits.Summary
	if bound == 0 || bound >= incidentcontext.DefaultSummaryBytes || s.limits.Validate() != nil {
		t.Fatalf("summary bound %d: want one below the default %d that the builder accepts", bound,
			incidentcontext.DefaultSummaryBytes)
	}

	// Before shortening: the same Incident with room for every value.
	wide := s.limits
	wide.Summary = incidentcontext.MaxLimitBytes
	full := summaryOf(t, s, wide).Text
	if len(full) <= bound || strings.Contains(full, " bytes cut)") {
		t.Fatalf("the unshortened summary is %d bytes, not over the bound %d:\n%s", len(full), bound, full)
	}
	// At the default bound the summary fits: shortening there needs the variant's lower bound.
	if def := incidentcontext.DefaultLimits(); len(summaryOf(t, s, def).Text) != len(full) {
		t.Errorf("at the default summary bound %d the summary is shortened; the variant would not need its own bound",
			def.Summary)
	}

	// After shortening: within the bound, every line kept, only the symptom cut, with its note.
	blk := summaryOf(t, s, s.limits)
	shown := blk.Text
	if len(shown) > bound {
		t.Fatalf("the shortened summary is %d bytes, over the bound %d", len(shown), bound)
	}
	t.Logf("summary: %d bytes unshortened, %d shown, bound %d:\n%s", len(full), len(shown), bound, shown)
	symptom := s.incident.Spec.Symptom
	head := shortenedSymptom(t, full, shown, symptom)
	if !strings.Contains(head, "12 Insufficient cpu") {
		t.Errorf("the cause is not in the part of the symptom shown: %q", head)
	}
	cut := symptom[len(head):]

	// The recorded reply passes on the shortened summary.
	res := run(t, scheme, s)
	if !res.Pass {
		t.Fatalf("%s failed: outcome %+v, diagnosis %+v, quotes %+v", s.name, res.Outcome, res.Diagnosis, res.QuoteFailures)
	}
	var prompt strings.Builder
	for _, m := range res.prompt {
		prompt.WriteString(m.Content)
	}
	if !strings.Contains(prompt.String(), "symptom: "+head+" (") {
		t.Errorf("the prompt does not show the shortened symptom")
	}

	// The evidence check matches a quote cited from the summary (c1) against the block's quotable parts
	// (Block.Data, as analysis.contextMessage takes them). The head of the symptom is one of them; no part holds
	// the end of the cut part (whole in the pod's status, c2), a quote across the cut, or the note.
	parts := blk.Data()
	if !slices.Contains(parts, head) {
		t.Errorf("the head of the symptom is not quotable: %q", parts)
	}
	for name, quote := range map[string]string{
		"cut part":   strings.TrimSuffix(cut[len(cut)-40:], "."),
		"across cut": head[len(head)-20:] + cut[:20],
		"note":       fmt.Sprintf("(%d bytes cut)", len(cut)),
	} {
		for _, p := range parts {
			if strings.Contains(p, quote) {
				t.Errorf("%s: the quote %q is quotable from the summary: %q", name, quote, p)
			}
		}
	}
}

// A variant's summaryBytes outside what the context builder accepts is refused when the variant is loaded.
func TestVariantSummaryBytesOutOfRangeIsRefused(t *testing.T) {
	scheme, base := mustScenario(t, "unschedulable")
	src := filepath.Join(scenariosDir, "unschedulable", variantsDir, "over-bound")
	for _, n := range []int{incidentcontext.MinSectionBytes - 1, incidentcontext.MaxLimitBytes + 1, -1} {
		dir := t.TempDir()
		for _, f := range []string{"replies.yaml", "incident.yaml", "objects.yaml"} {
			raw, err := os.ReadFile(filepath.Join(src, f))
			if err != nil {
				t.Fatal(err)
			}
			// A fixed file name in the test's own temporary directory.
			if err := os.WriteFile(filepath.Join(dir, f), raw, 0o600); err != nil { // #nosec G703 -- fixed name, temp dir
				t.Fatal(err)
			}
		}
		v := fmt.Sprintf("about: a bound out of range\nsummaryBytes: %d\n", n)
		if err := os.WriteFile(filepath.Join(dir, "variant.yaml"), []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadVariant(scheme, dir, base); err == nil || !strings.Contains(err.Error(), "summaryBytes") {
			t.Errorf("summaryBytes %d: error %v, want a refusal of summaryBytes", n, err)
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
