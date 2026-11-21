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

package intake

import (
	"context"
	"encoding/json"
	"math/rand/v2"
	"strings"
	"testing"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/analysis"
	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/redact"
)

// corpusSize is the number of generated records of each corpus: a seeded sample of them under -race.
var corpusSize = func() int {
	if raceDetector {
		return 30
	}
	return 200
}()

// TestKeptNamesCorpus is the generated corpus of ADR-0047 section 3, written before the rule. Records built as
// the analyzer builds them (names and fingerprints from the real name functions, a summary that mentions them)
// must pass the intake's redactor check, which keeps the four values only by their shape. Without the kept
// values every one of them is refused, because the names trip the redactor. Each record changed in one kept
// value, or with a secret glued to a kept value, must be refused.
func TestKeptNamesCorpus(t *testing.T) {
	r, err := redact.New()
	if err != nil {
		t.Fatal(err)
	}
	rng := rngOf(2026, 1121)
	tripped := 0
	for i := range corpusSize {
		rec := recordOf(t, rng, claudeID)
		data, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		keep, refused := keptValues(rec, rec.ID)
		if refused != nil {
			t.Fatalf("record %d: %s", i, refused.reason)
		}
		if kinds, ok := checkRedaction(r, data, keep); !ok {
			t.Fatalf("record %d: a record the analyzer makes is refused (%v)", i, kinds)
		}
		// The rule earns its place: with nothing kept, the same record is refused.
		if _, ok := checkRedaction(r, data, nil); !ok {
			tripped++
		}
	}
	if tripped != corpusSize {
		t.Fatalf("%d of %d records trip the redactor with nothing kept; the corpus no longer needs the rule", tripped, corpusSize)
	}
}

// TestKeptNamesCorpusRefusals changes one kept value of a generated record, or puts text next to one, and
// expects the record quarantined with no model call. The right answer is known by construction.
func TestKeptNamesCorpusRefusals(t *testing.T) {
	seps := []string{"=", ":", ": ", " ", "\t", "\n", ",", ";", "\"", "'", "&", "/", ""}
	mutations := map[string]func(rng *rand.Rand, rec *analysis.Record){
		"the name of another fingerprint, everywhere": func(rng *rand.Rand, rec *analysis.Record) {
			forged := remediationv1alpha1.IncidentName("sha256:"+hexOf(rng, 32), 1)
			rec.Summary = strings.ReplaceAll(rec.Summary, rec.Incident.Name, forged)
			rec.Incident.Name = forged
		},
		"an upper-case fingerprint": func(_ *rand.Rand, rec *analysis.Record) {
			rec.Incident.Fingerprint = strings.ToUpper(rec.Incident.Fingerprint)
		},
		"a fingerprint of 63 digits": func(_ *rand.Rand, rec *analysis.Record) {
			rec.Incident.Fingerprint = rec.Incident.Fingerprint[:len(rec.Incident.Fingerprint)-1]
		},
		"a name with a leading-zero occurrence": func(_ *rand.Rand, rec *analysis.Record) {
			rec.Incident.Name = remediationv1alpha1.IncidentName(rec.Incident.Fingerprint, 1) + "-02"
		},
		"a request of 26 digits": func(_ *rand.Rand, rec *analysis.Record) { rec.Request += "ab" },
		"a request of another prefix": func(_ *rand.Rand, rec *analysis.Record) {
			rec.Request = "remedy-" + strings.TrimPrefix(rec.Request, "remediation-")
		},
		"another Incident's name in the summary": func(rng *rand.Rand, rec *analysis.Record) {
			rec.Summary += " See " + remediationv1alpha1.IncidentName("sha256:"+hexOf(rng, 32), 1) + "."
		},
		"a kept name inside a longer run": func(_ *rand.Rand, rec *analysis.Record) {
			rec.Summary += " x" + rec.Incident.Name + "y"
		},
	}
	for _, sep := range seps {
		mutations["a token after the name, separator "+strings.ReplaceAll(sep, "\n", "\\n")] = func(_ *rand.Rand, rec *analysis.Record) {
			rec.Summary += " " + rec.Incident.Name + sep + fakeToken
		}
		mutations["a secret key after the request, separator "+strings.ReplaceAll(sep, "\n", "\\n")] = func(_ *rand.Rand, rec *analysis.Record) {
			rec.Summary += " " + rec.Request + sep + "password=" + hexOf(rngOf(1, 1), 12)
		}
	}
	rng := rngOf(1121, 2026)
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			for i := range corpusSize / 10 {
				f := newFixture(t)
				rec := recordOf(t, rng, claudeID)
				mutate(rng, &rec)
				f.openai.replies = []providers.Completion{answerReply(gptID)}
				s, err := f.scorer.Score(context.Background(), sourceOf(t, rec))
				if err != nil {
					t.Fatal(err)
				}
				if s.Status != StatusQuarantined || s.Check != CheckRedactor || f.modelCalls() != 0 {
					t.Fatalf("record %d: score %+v with %d calls, want quarantined by the redactor", i, s, f.modelCalls())
				}
			}
		})
	}
}

// TestCorpusRecordsAreScored runs the generated records through the whole check and a fake judge.
func TestCorpusRecordsAreScored(t *testing.T) {
	rng := rngOf(7, 7)
	for i := range corpusSize / 4 {
		f := newFixture(t)
		rec := recordOf(t, rng, gptID)
		f.anthropic.replies = []providers.Completion{answerReply(claudeID)}
		s, err := f.scorer.Score(context.Background(), sourceOf(t, rec))
		if err != nil || s.Status != StatusScored {
			t.Fatalf("record %d: score %+v, %v", i, s, err)
		}
	}
}
