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
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
	"unicode"

	"github.com/pkalin/shadewright/internal/analysis"
	"github.com/pkalin/shadewright/internal/providers"
)

// spliceFirst puts member (`"key":value`) as the first member of the JSON object data.
func spliceFirst(t *testing.T, data []byte, member string) []byte {
	t.Helper()
	if len(data) < 2 || data[0] != '{' {
		t.Fatal("not a JSON object")
	}
	return []byte("{" + member + "," + string(data[1:]))
}

// assertQuarantinedUnsent checks that the record was refused before any model call and that secret reached no
// request.
func assertQuarantinedUnsent(t *testing.T, f *fixture, s Score, err error, secret string) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	if s.Status != StatusQuarantined || f.modelCalls() != 0 {
		t.Fatalf("score = %+v after %d model calls, want quarantined with none", s, f.modelCalls())
	}
	for _, m := range []*fakeModel{f.anthropic, f.openai} {
		for _, req := range m.reqs {
			for _, msg := range req.Messages {
				if strings.Contains(msg.Content, secret) {
					t.Fatal("the secret reached a model")
				}
			}
		}
	}
	assertNoRecordText(t, s, secret)
}

// Finding (a) of 2026-11-21: the redactor check decoded a map (the last duplicate wins) and the judge the record
// type (a duplicate object is merged, a null leaves the earlier string), so a secret in the first of two members
// reached the judge with status scored.
func TestDuplicateKeysNeverReachTheJudge(t *testing.T) {
	secret := "token=" + fakeToken
	t.Run("incident twice, the reason only in the first", func(t *testing.T) {
		f := newFixture(t)
		rec := recordOf(t, rngOf(71, 72), claudeID)
		f.openai.replies = []providers.Completion{answerReply(gptID)}
		dirty := rec.Incident
		dirty.Reason = secret
		first, err := json.Marshal(dirty)
		if err != nil {
			t.Fatal(err)
		}
		// The record's own incident member, without its reason, comes later and is the one a map keeps.
		var second map[string]any
		clean, _ := json.Marshal(rec.Incident)
		if err := json.Unmarshal(clean, &second); err != nil {
			t.Fatal(err)
		}
		delete(second, "reason")
		secondJSON, _ := json.Marshal(second)
		src := sourceOf(t, rec)
		src.data = []byte(strings.Replace(string(src.data), `"incident":`+string(clean), `"incident":`+string(secondJSON), 1))
		src.data = spliceFirst(t, src.data, `"incident":`+string(first))
		s, err := f.scorer.Score(context.Background(), src)
		assertQuarantinedUnsent(t, f, s, err, fakeToken)
		if s.Check != CheckSchema || s.Reason != reasonDuplicateKey {
			t.Fatalf("check = %q (%s), want the duplicate key refusal", s.Check, s.Reason)
		}
	})
	t.Run("summary with a token, then summary null", func(t *testing.T) {
		f := newFixture(t)
		rec := recordOf(t, rngOf(73, 74), claudeID)
		f.openai.replies = []providers.Completion{answerReply(gptID)}
		src := sourceOf(t, rec)
		orig, _ := json.Marshal(rec.Summary)
		dirty, _ := json.Marshal(secret)
		src.data = []byte(strings.Replace(string(src.data), `"summary":`+string(orig),
			`"summary":`+string(dirty)+`,"summary":null`, 1))
		s, err := f.scorer.Score(context.Background(), src)
		assertQuarantinedUnsent(t, f, s, err, fakeToken)
		if s.Check != CheckSchema || s.Reason != reasonDuplicateKey {
			t.Fatalf("check = %q (%s), want the duplicate key refusal", s.Check, s.Reason)
		}
	})
}

// The redactor also runs on the values the judge is given, built from the decoded record, so the check and the
// judge cannot read two different records again.
func TestJudgeInputIsCheckedAsGiven(t *testing.T) {
	f := newFixture(t)
	rec := recordOf(t, rngOf(75, 76), claudeID)
	keep, refused := keptValues(rec, rec.ID)
	if refused != nil {
		t.Fatal(refused.reason)
	}
	if kinds, ok := checkJudgeInput(f.scorer.Redactor, rec, keep); !ok {
		t.Fatalf("a clean record's judge input is refused (%v)", kinds)
	}
	for name, mutate := range map[string]func(*analysis.Record){
		"in the reason":       func(r *analysis.Record) { r.Incident.Reason = "Bearer " + fakeToken },
		"in the subject kind": func(r *analysis.Record) { r.Incident.SubjectKind = "token=" + fakeToken },
		"in the summary":      func(r *analysis.Record) { r.Summary += " token=" + fakeToken },
		"in the missing data": func(r *analysis.Record) { r.MissingData = append(r.MissingData, fakeToken) },
	} {
		t.Run(name, func(t *testing.T) {
			dirty := rec
			dirty.MissingData = append([]string(nil), rec.MissingData...)
			mutate(&dirty)
			if _, ok := checkJudgeInput(f.scorer.Redactor, dirty, keep); ok {
				t.Fatal("a token in the judge's input passed")
			}
		})
	}
}

// Finding (b) of 2026-11-21: the ID's shape let a 40-hex value through as a kept value. The ID is the Incident
// UID as a UUID, a dash and the time, and its UID is incident.uid.
func TestRecordIDIsTheIncidentUIDAndTime(t *testing.T) {
	t.Run("40 hex digits and the time, repeated in the summary", func(t *testing.T) {
		f := newFixture(t)
		rng := rngOf(77, 78)
		rec := recordOf(t, rng, claudeID)
		f.openai.replies = []providers.Completion{answerReply(gptID)}
		hex40 := hexOf(rng, 20)
		rec.ID = hex40 + "-20261120t093000z"
		rec.Summary += " " + rec.ID
		s, err := f.scorer.Score(context.Background(), sourceOf(t, rec))
		assertQuarantinedUnsent(t, f, s, err, hex40)
		if s.Check != CheckName {
			t.Fatalf("check = %q, want %q", s.Check, CheckName)
		}
	})
	t.Run("a UUID that is not incident.uid", func(t *testing.T) {
		f := newFixture(t)
		rng := rngOf(79, 80)
		rec := recordOf(t, rng, claudeID)
		f.openai.replies = []providers.Completion{answerReply(gptID)}
		other := uidOf(rng)
		rec.ID = other + "-20261120t093000z"
		rec.Summary += " " + other
		s, err := f.scorer.Score(context.Background(), sourceOf(t, rec))
		assertQuarantinedUnsent(t, f, s, err, fakeToken)
		if s.Check != CheckSchema {
			t.Fatalf("check = %q, want %q", s.Check, CheckSchema)
		}
	})
	for _, name := range []string{
		"0f8e2a4c-1b3d-4e5f-8a9b-0c1d2e3f4a5b-20261320t093000z.json", // month 13
		"0f8e2a4c-1b3d-4e5f-8a9b-0c1d2e3f4a5b-20261120t253000z.json", // hour 25
		"0f8e2a4c1b3d4e5f8a9b0c1d2e3f4a5b-20261120t093000z.json",     // no dashes
		"0F8E2A4C-1B3D-4E5F-8A9B-0C1D2E3F4A5B-20261120t093000z.json", // upper case
		"0f8e2a4c-1b3d-4e5f-8a9b-0c1d2e3f4a5b0-20261120t093000z.json",
	} {
		if _, ok := recordIDFromName(name); ok {
			t.Errorf("name %q passed", name)
		}
	}
	if id, ok := recordIDFromName("0f8e2a4c-1b3d-4e5f-8a9b-0c1d2e3f4a5b-20261120t093000z.json"); !ok ||
		id != "0f8e2a4c-1b3d-4e5f-8a9b-0c1d2e3f4a5b-20261120t093000z" {
		t.Errorf("the analyzer's name was refused (%q)", id)
	}
}

// Finding (c) of 2026-11-21: property names of the answer are matched exactly, and the note is optional.
func TestJudgeAnswerExactCaseNoteOptional(t *testing.T) {
	r := newFixture(t).scorer.Redactor
	for name, c := range map[string]struct {
		content string
		ok      bool
	}{
		"upper case name":   {`{"SPECIFIC":4,"consistent":5,"causeSupported":3,"missingData":2,"verdict":"useful","note":""}`, false},
		"one letter":        {`{"specific":4,"Consistent":5,"causeSupported":3,"missingData":2,"verdict":"useful"}`, false},
		"a folded name":     {`{"\u017fpecific":4,"consistent":5,"causeSupported":3,"missingData":2,"verdict":"useful"}`, false},
		"no note":           {`{"specific":4,"consistent":5,"causeSupported":3,"missingData":2,"verdict":"useful"}`, true},
		"a duplicate":       {`{"specific":4,"consistent":5,"causeSupported":3,"missingData":2,"verdict":"great","verdict":"useful"}`, false},
		"an escaped double": {`{"specific":4,"consistent":5,"causeSupported":3,"missingData":2,"verdict":"useful","not\u0065":"a","note":"b"}`, false},
		"null note":         {`{"specific":4,"consistent":5,"causeSupported":3,"missingData":2,"verdict":"useful","note":null}`, true},
	} {
		t.Run(name, func(t *testing.T) {
			a, ok := parseAnswer(r, c.content)
			if ok != c.ok {
				t.Fatalf("ok = %v, want %v (%+v)", ok, c.ok, a)
			}
			if ok && (a.Note != "" || a.NoteDropped || a.Verdict != "useful" || a.Specific != 4) {
				t.Fatalf("answer = %+v", a)
			}
		})
	}
}

// The judge's note is stored without control characters, as every stored model text (decision of 2026-11-22 on
// #852, threat model G4.14): a JSON escape that carries one is removed, the visible text kept; a note whose key a
// removed control joins is dropped, never stored.
func TestJudgeNoteStoredWithoutControls(t *testing.T) {
	r := newFixture(t).scorer.Redactor
	answer := func(note string) string {
		return `{"specific":4,"consistent":5,"causeSupported":3,"missingData":2,"verdict":"useful","note":"` + note + `"}`
	}
	for name, c := range map[string]struct {
		note, stored string
		dropped      bool
	}{
		"ANSI colour":        {`\u001b[31mred\u001b[0m cause`, "[31mred[0m cause", false},
		"C1 and DEL":         {`one\u0085two\u007fthree\u009b`, "onetwothree", false},
		"only controls":      {`\u0007\u001b`, "", false},
		"newline kept":       {`a\nb\tc`, "a\nb\tc", false},
		"joined secret key":  {`pass\u001bword=Hunter2Secret9x`, "", true},
		"no control, secret": {`password=Hunter2Secret9x`, "", true},
	} {
		t.Run(name, func(t *testing.T) {
			a, ok := parseAnswer(r, answer(c.note))
			if !ok {
				t.Fatalf("answer refused")
			}
			if a.Note != c.stored || a.NoteDropped != c.dropped {
				t.Errorf("note %q stored as %q (dropped %v), want %q (dropped %v)", c.note, a.Note, a.NoteDropped, c.stored, c.dropped)
			}
			if strings.ContainsFunc(a.Note, func(r rune) bool { return unicode.Is(unicode.Cc, r) && r != '\n' && r != '\t' }) {
				t.Errorf("stored note %q holds a control", a.Note)
			}
		})
	}
}

// Finding (d) of 2026-11-21: each call of the intake is one billed attempt under its one reservation. The
// adapters are built without automatic retries (cmd/intake), and every request also says so.
func TestCallsAskForOneBilledAttempt(t *testing.T) {
	f := newFixture(t)
	rec := recordOf(t, rngOf(81, 82), claudeID)
	f.openai.replies = []providers.Completion{toolReply(askCall("c1", "Is 768Mi above the node's allocatable memory?")),
		answerReply(gptID)}
	f.anthropic.replies = []providers.Completion{{Content: "No.", Model: claudeID}}
	s, err := f.scorer.Score(context.Background(), sourceOf(t, rec))
	if err != nil || s.Status != StatusScored || s.CrossCheck != CrossCheckAnswered {
		t.Fatalf("score = %+v, %v", s, err)
	}
	for _, m := range []*fakeModel{f.openai, f.anthropic} {
		for i, req := range m.reqs {
			if req.MaxBilledAttempts != 1 {
				t.Errorf("request %d asks for %d billed attempts, want 1", i, req.MaxBilledAttempts)
			}
		}
	}
	if f.modelCalls() != 3 {
		t.Fatalf("%d calls, want 3", f.modelCalls())
	}
}

// keyPool are distinct lower-case keys; no two fold to the same string.
var keyPool = []string{"a", "b", "c", "ab", "id", "kind", "name", "uid", "why", "text", "tokens", "inc",
	"models", "skip", "x1", "note"}

// variant returns a key that decodes to a name equal to k, or equal under the case folding of encoding/json, but
// is spelled differently in the document: an escape, a case change, or a Unicode fold (the Kelvin sign for "k",
// the long s for "s"). The second result names the kind.
func variant(rng *rand.Rand, k string) (string, string) {
	kinds := []string{"exact", "escaped", "upper"}
	if strings.ContainsAny(k, "ks") {
		kinds = append(kinds, "fold")
	}
	switch kind := kinds[rng.IntN(len(kinds))]; kind {
	case "escaped":
		i := rng.IntN(len(k))
		return k[:i] + fmt.Sprintf(`\u%04x`, k[i]) + k[i+1:], kind
	case "upper":
		i := rng.IntN(len(k))
		if k[i] < 'a' || k[i] > 'z' {
			return strings.ToUpper(k), kind
		}
		return k[:i] + strings.ToUpper(k[i:i+1]) + k[i+1:], kind
	case "fold":
		if before, after, ok := strings.Cut(k, "k"); ok {
			return before + "\u212a" + after, kind
		}
		before, after, _ := strings.Cut(k, "s")
		return before + "\u017f" + after, kind
	default:
		return k, kind
	}
}

// genDoc builds a JSON document of the given depth (1 is a flat object) with distinct keys in every object. When
// dup is set, one object at depth dupAt gets a second member whose key is a variant of one it has.
func genDoc(rng *rand.Rand, depth, dupAt int, dup bool) string {
	var obj func(level int) string
	obj = func(level int) string {
		n := 1 + rng.IntN(4)
		perm := rng.Perm(len(keyPool))[:n]
		var b strings.Builder
		b.WriteByte('{')
		for i, p := range perm {
			if i > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, "%q:", keyPool[p])
			if i == 0 && level < depth {
				if rng.IntN(2) == 0 {
					b.WriteString("[" + obj(level+1) + "]")
				} else {
					b.WriteString(obj(level + 1))
				}
			} else {
				b.WriteString([]string{`"v"`, `1`, `null`, `true`, `[]`, `{}`}[rng.IntN(6)])
			}
		}
		if dup && level == dupAt {
			k, _ := variant(rng, keyPool[perm[rng.IntN(n)]])
			b.WriteString(`,"` + k + `":"second"`)
		}
		b.WriteByte('}')
		return b.String()
	}
	return obj(1)
}

// TestDuplicateKeyCorpus is the generated corpus of the duplicate key rule, written before the rule. Documents
// with distinct keys, nested up to the depth limit, pass; the same documents with one more member whose key is the
// same name (as written, escaped, or equal under case folding) at any depth are refused. Deeper documents are
// refused. The answers are known by construction.
func TestDuplicateKeyCorpus(t *testing.T) {
	rng := rngOf(2026, 1122)
	for i := range corpusSize {
		// An array around a nested object adds a level, so the generated depth stays at half the limit.
		depth := 1 + rng.IntN(maxJSONDepth/2)
		at := 1 + rng.IntN(depth)
		clean := genDoc(rngOf(uint64(i), 1), depth, at, false)
		if !json.Valid([]byte(clean)) {
			t.Fatalf("doc %d is not JSON: %s", i, clean)
		}
		if err := checkKeys([]byte(clean)); err != nil {
			t.Fatalf("doc %d (depth %d) with distinct keys refused: %v\n%s", i, depth, err, clean)
		}
		dirty := genDoc(rngOf(uint64(i), 1), depth, at, true)
		if !json.Valid([]byte(dirty)) {
			t.Fatalf("doc %d is not JSON: %s", i, dirty)
		}
		if err := checkKeys([]byte(dirty)); err == nil {
			t.Fatalf("doc %d with a duplicate at depth %d passed:\n%s", i, at, dirty)
		}
	}
	// Depth: at the limit passes, one more is refused.
	deep := func(n int) string { return strings.Repeat(`{"a":`, n-1) + `{}` + strings.Repeat("}", n-1) }
	if err := checkKeys([]byte(deep(maxJSONDepth))); err != nil {
		t.Fatalf("depth %d refused: %v", maxJSONDepth, err)
	}
	if err := checkKeys([]byte(deep(maxJSONDepth + 1))); err == nil {
		t.Fatalf("depth %d passed", maxJSONDepth+1)
	}
	arrays := strings.Repeat("[", maxJSONDepth+1) + strings.Repeat("]", maxJSONDepth+1)
	if err := checkKeys([]byte(arrays)); err == nil {
		t.Fatal("arrays past the depth passed")
	}
	// The same key in two different objects is not a duplicate.
	if err := checkKeys([]byte(`{"a":{"a":1},"b":[{"a":1},{"a":2}]}`)); err != nil {
		t.Fatalf("keys of sibling objects refused: %v", err)
	}
}

// TestDuplicateKeyCorpusInRecords puts a variant of one of its keys into a record the analyzer makes, in each
// object of the record, and expects the record quarantined for its schema with no model call.
func TestDuplicateKeyCorpusInRecords(t *testing.T) {
	rng := rngOf(2026, 1123)
	// Each object of the record (by the key that holds it, "" for the record) and some of its keys; a slice, so the
	// corpus is the same on every run.
	members := []struct {
		obj  string
		keys []string
	}{
		{"", []string{"schemaVersion", "id", "incident", "summary", "models", "missingData", "providers", "deployments"}},
		{"incident", []string{"name", "uid", "fingerprint", "reason", "subjectKind"}},
		{"providers", []string{"triage"}},
		{"deployments", []string{"triage"}},
	}
	n := 0
	for i := range corpusSize / 4 {
		for _, m := range members {
			obj, keys := m.obj, m.keys
			f := newFixture(t)
			rec := recordOf(t, rng, claudeID)
			f.openai.replies = []providers.Completion{answerReply(gptID)}
			base := keys[rng.IntN(len(keys))]
			k, kind := variant(rng, base)
			// The value is a token, except for schemaVersion, whose value is read before the key check.
			member := `"` + k + `":"token=` + fakeToken + `"`
			if base == "schemaVersion" {
				member = `"` + k + `":1`
			}
			src := sourceOf(t, rec)
			if obj == "" {
				src.data = spliceFirst(t, src.data, member)
			} else {
				marker := `"` + obj + `":{`
				if !strings.Contains(string(src.data), marker) {
					t.Fatalf("no %s in the record", obj)
				}
				src.data = []byte(strings.Replace(string(src.data), marker, marker+member+",", 1))
			}
			s, err := f.scorer.Score(context.Background(), src)
			if err != nil {
				t.Fatal(err)
			}
			if s.Status != StatusQuarantined || s.Reason != reasonDuplicateKey || f.modelCalls() != 0 {
				t.Fatalf("%d: %s key %q in %q: score = %+v after %d calls", i, kind, k, obj, s, f.modelCalls())
			}
			n++
		}
	}
	if n == 0 {
		t.Fatal("no record was generated")
	}
}
