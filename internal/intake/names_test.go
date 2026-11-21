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
	"strings"
	"testing"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/analysis"
	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/redact"
)

// earlierChecksPass reports whether rec passes the checks that run before the judge's request is built: the
// shape of the kept values, the raw record and the judge's values, each keeping the names.
func earlierChecksPass(t *testing.T, r *redact.Redactor, rec analysis.Record) bool {
	t.Helper()
	keep, refused := keptValues(rec, rec.ID)
	if refused != nil {
		return false
	}
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := checkRedaction(r, data, keep); !ok {
		return false
	}
	_, ok := checkJudgeInput(r, rec, keep)
	return ok
}

// TestNamesReplacedCorpus is the generated corpus of the replacement, with answers known by construction. A
// summary is built from pieces (the record ID, the Incident's name, the request's name, the fingerprint, the
// fingerprint's bare digits, the digits inside a longer hexadecimal run, the name of a later occurrence, plain
// words) joined by separators. A name piece is replaced exactly when both its neighbours are a separator byte
// that is not part of a token (or the text's end), as the redactor's keep list takes a whole occurrence; the
// rest of the text stays byte for byte. The expected judge message is rendered from that expected summary: a
// record whose expected message the redactor would change, or that an earlier check refuses, is quarantined
// with no call; any other is scored with exactly that message.
func TestNamesReplacedCorpus(t *testing.T) {
	r, err := redact.New()
	if err != nil {
		t.Fatal(err)
	}
	whole := []string{" ", ", ", "; ", " (", ") ", "\"", "'", "\n", "\t", "! ", "? ", "[", "]", "<", ">", "|", "#", "@"}
	glued := []string{"-2", ".", "x", "_", "/", "=", "+", ":", "0"}
	rng := rngOf(2026, 1123)
	scored, quarantined, replaced := 0, 0, 0
	for i := range corpusSize {
		rec := recordOf(t, rng, gptID)
		hex := strings.TrimPrefix(rec.Incident.Fingerprint, "sha256:")
		_, occ, _ := remediationv1alpha1.ParseIncidentName(rec.Incident.Name)
		pieces := []struct{ text, word string }{
			{rec.ID, wordRecord}, {rec.Incident.Name, wordIncident}, {rec.Request, wordRequest},
			{rec.Incident.Fingerprint, wordFingerprint}, {hex, wordFingerprint},
			{"ab" + hex, ""}, {hex + "cd", ""},
			{remediationv1alpha1.IncidentName(rec.Incident.Fingerprint, occ+1), ""},
			{"the pod", ""}, {"memory", ""}, {"limit", ""},
		}
		var got, want strings.Builder
		n := 1 + rng.IntN(6)
		seps := make([]string, n+1)
		seps[0], seps[n] = "", ""
		for j := 1; j < n; j++ {
			if rng.IntN(3) == 0 {
				seps[j] = glued[rng.IntN(len(glued))]
			} else {
				seps[j] = whole[rng.IntN(len(whole))]
			}
		}
		isWhole := func(sep string, left bool) bool {
			if sep == "" {
				return true
			}
			c := sep[len(sep)-1]
			if !left {
				c = sep[0]
			}
			return !strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789+/_-=.", rune(c))
		}
		didReplace := false
		for j := range n {
			p := pieces[rng.IntN(len(pieces))]
			got.WriteString(seps[j])
			want.WriteString(seps[j])
			got.WriteString(p.text)
			if p.word != "" && isWhole(seps[j], true) && isWhole(seps[j+1], false) {
				want.WriteString(p.word)
				didReplace = true
			} else {
				want.WriteString(p.text)
			}
		}
		rec.Summary = got.String()
		expected := rec
		expected.Summary = want.String()
		msg := renderJudgeUser(t, expected)
		ok := earlierChecksPass(t, r, rec) && r.Clean(msg)

		f := newFixture(t)
		f.anthropic.replies = []providers.Completion{answerReply(claudeID)}
		s, err := f.scorer.Score(context.Background(), sourceOf(t, rec))
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case !ok && (s.Status != StatusQuarantined || f.modelCalls() != 0):
			t.Fatalf("record %d, summary %q: score %+v after %d calls, want quarantined with none", i, rec.Summary, s, f.modelCalls())
		case !ok:
			quarantined++
		case s.Status != StatusScored || len(f.anthropic.reqs) == 0:
			t.Fatalf("record %d, summary %q: score %+v, want scored with %q", i, rec.Summary, s, msg)
		case f.anthropic.reqs[0].Messages[1].Content != msg:
			t.Fatalf("record %d, summary %q: message %q, want %q", i, rec.Summary, f.anthropic.reqs[0].Messages[1].Content, msg)
		default:
			scored++
			if didReplace {
				replaced++
			}
		}
		assertNothingSentDirty(t, f)
	}
	if scored == 0 || quarantined == 0 || replaced == 0 {
		t.Fatalf("corpus reaches too little: %d scored (%d with a replacement), %d quarantined", scored, replaced, quarantined)
	}
	t.Logf("names corpus: %d scored (%d with a replacement), %d quarantined of %d", scored, replaced, quarantined, corpusSize)
}

// assertNothingSentDirty checks that every text a model got is clean with nothing kept.
func assertNothingSentDirty(t *testing.T, f *fixture) {
	t.Helper()
	for _, m := range []*fakeModel{f.anthropic, f.openai} {
		for i, req := range m.reqs {
			for _, msg := range req.Messages {
				if !f.scorer.Redactor.Clean(msg.Content) {
					t.Fatalf("request %d holds text the redactor would change: %q", i, f.scorer.Redactor.String(msg.Content))
				}
			}
		}
	}
}

// A name directly after a secret-named flag or key: the raw record passes, because the name is a kept value and
// is the whole of the flag's value. After the replacement the flag's value is the fixed word, which the redactor
// takes as a value, so the request as sent is refused and the record is quarantined with no call. Nothing of
// the name, and nothing the redactor would change, reaches a model.
func TestNameAsASecretValueIsNotSent(t *testing.T) {
	for name, summary := range map[string]func(analysis.Record) string{
		"--token and the Incident's name": func(rec analysis.Record) string {
			return "The pod runs with --token " + rec.Incident.Name + " today."
		},
		"token: and the record ID": func(rec analysis.Record) string { return "token: " + rec.ID },
		"--token and the fingerprint": func(rec analysis.Record) string {
			return "The pod runs with --token " + rec.Incident.Fingerprint
		},
	} {
		t.Run(name, func(t *testing.T) {
			r, err := redact.New()
			if err != nil {
				t.Fatal(err)
			}
			rec := recordOf(t, rngOf(95, 97), claudeID)
			rec.Summary = summary(rec)
			if !earlierChecksPass(t, r, rec) {
				t.Fatal("the raw record is refused before the replacement; the case does not reach the check of what is sent")
			}
			f := newFixture(t)
			f.openai.replies = []providers.Completion{answerReply(gptID)}
			s, err := f.scorer.Score(context.Background(), sourceOf(t, rec))
			if err != nil {
				t.Fatal(err)
			}
			if s.Status != StatusQuarantined || s.Check != CheckRedactor || s.Reason != reasonUnsent || f.modelCalls() != 0 {
				t.Fatalf("score %+v after %d calls, want quarantined by the check of what is sent with none", s, f.modelCalls())
			}
		})
	}
}

// A secret that holds a name, or a name glued to a secret: the name is not a whole occurrence, so it is
// neither kept by the earlier check nor replaced, and the earlier check quarantines the record. The same holds
// for password= directly before a name, whose match covers the key as well as the name.
func TestSecretHoldingANameIsRefusedEarlier(t *testing.T) {
	for name, summary := range map[string]func(analysis.Record) string{
		"password= directly before the request's name (the match takes the key too)": func(rec analysis.Record) string {
			return "It sets password=" + rec.Request + " in the config."
		},
		"a token that ends in the request's name": func(rec analysis.Record) string {
			return "It uses --token abc" + rec.Request + " here."
		},
		"the Incident's name then a token, glued by -": func(rec analysis.Record) string {
			return "See " + rec.Incident.Name + "-" + fakeToken + " here."
		},
	} {
		t.Run(name, func(t *testing.T) {
			r, err := redact.New()
			if err != nil {
				t.Fatal(err)
			}
			rec := recordOf(t, rngOf(95, 98), claudeID)
			rec.Summary = summary(rec)
			if earlierChecksPass(t, r, rec) {
				t.Fatal("the earlier check passes the record")
			}
			f := newFixture(t)
			s, err := f.scorer.Score(context.Background(), sourceOf(t, rec))
			if err != nil {
				t.Fatal(err)
			}
			if s.Status != StatusQuarantined || s.Check != CheckRedactor || f.modelCalls() != 0 {
				t.Fatalf("score %+v after %d calls, want quarantined with none", s, f.modelCalls())
			}
		})
	}
}

// The judge's instructions say what the fixed words stand for.
func TestJudgePromptNamesTheWords(t *testing.T) {
	for _, w := range []string{"[record-id]", "[incident-name]", "[request-name]", "[fingerprint]"} {
		if !strings.Contains(judgePrompt, w) {
			t.Fatalf("the judge's prompt does not say what %s stands for", w)
		}
	}
}
