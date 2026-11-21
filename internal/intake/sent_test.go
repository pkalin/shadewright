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
	"go/ast"
	"go/parser"
	"go/token"
	"math/rand/v2"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/pkalin/shadewright/internal/analysis"
	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/redact"
)

// plainSummary is a summary that names no Incident, request or fingerprint.
const plainSummary = "The container was killed for memory at 512Mi; the proposal raises the limit to 768Mi."

// plainRecordOf is recordOf, whose summary names nothing the redactor matches.
func plainRecordOf(t *testing.T, rng *rand.Rand, models ...string) analysis.Record {
	t.Helper()
	return recordOf(t, rng, models...)
}

// assertNothingDirtySent checks that every request a model got is clean as sent, keeping only the record ID.
func assertNothingDirtySent(t *testing.T, f *fixture, id string) {
	t.Helper()
	for _, m := range []*fakeModel{f.anthropic, f.openai} {
		for i, req := range m.reqs {
			for _, msg := range req.Messages {
				if !f.scorer.Redactor.Clean(msg.Content, id) {
					t.Fatalf("request %d holds text the redactor would change: %q", i, f.scorer.Redactor.String(msg.Content))
				}
			}
		}
	}
}

// Finding 1 of 2026-11-21 (head fdfc63a6): the check ran value by value, so a secret-named flag at the end of
// one value and its secret at the start of the next passed, while the redactor changes the judge's message as
// sent.
func TestValuesSplitAcrossFieldsNeverReachAModel(t *testing.T) {
	for name, mutate := range map[string]func(*analysis.Record){
		"a flag and its value in two missingData items": func(r *analysis.Record) {
			r.MissingData = []string{"--token", "abcdefgh12345678"}
		},
		"password= in the summary, the value in missingData": func(r *analysis.Record) {
			r.Summary = "password="
			r.MissingData = []string{"hunter2"}
		},
		"a flag at the end of the summary, a value in missingData": func(r *analysis.Record) {
			r.Summary = "see --client-secret"
			r.MissingData = []string{"abcdefgh12345678"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			rec := plainRecordOf(t, rngOf(91, 92), claudeID)
			mutate(&rec)
			f.openai.replies = []providers.Completion{answerReply(gptID)}
			s, err := f.scorer.Score(context.Background(), sourceOf(t, rec))
			if err != nil {
				t.Fatal(err)
			}
			if s.Status != StatusQuarantined || s.Check != CheckRedactor || f.modelCalls() != 0 || len(f.ledger.reserved) != 0 {
				t.Fatalf("score %+v after %d calls and %d reservations, want quarantined by the redactor with none",
					s, f.modelCalls(), len(f.ledger.reserved))
			}
		})
	}
}

// Finding 2 of 2026-11-21: the fingerprint was a kept value of the check on what the judge gets, so 64 hex
// digits the record chooses passed after a secret-named flag. The judge is never given the fingerprint, and the
// check of what is sent keeps only the record ID.
func TestFingerprintIsNotKeptInWhatIsSent(t *testing.T) {
	f := newFixture(t)
	rec := plainRecordOf(t, rngOf(93, 94), claudeID)
	rec.Summary = "The pod runs with --token " + rec.Incident.Fingerprint
	f.openai.replies = []providers.Completion{answerReply(gptID)}
	s, err := f.scorer.Score(context.Background(), sourceOf(t, rec))
	if err != nil {
		t.Fatal(err)
	}
	if s.Status != StatusQuarantined || s.Check != CheckRedactor || f.modelCalls() != 0 {
		t.Fatalf("score %+v after %d calls, want quarantined by the redactor with none", s, f.modelCalls())
	}
}

// The question to the cross-check model and the answer handed back to the judge are checked as sent, with
// their delimiters: a flag at the end of either takes the closing tag as its value.
func TestCrossCheckIsCheckedAsSent(t *testing.T) {
	t.Run("a question that ends in a flag is not sent", func(t *testing.T) {
		f := newFixture(t)
		rec := plainRecordOf(t, rngOf(95, 96), gptID)
		f.anthropic.replies = []providers.Completion{{ToolCalls: []providers.ToolCall{askCall("a", "Is --token")}},
			answerReply(claudeID)}
		s, err := f.scorer.Score(context.Background(), sourceOf(t, rec))
		if err != nil {
			t.Fatal(err)
		}
		if f.openai.calls() != 0 {
			t.Fatalf("score %+v: the cross-check model was called", s)
		}
		assertNothingDirtySent(t, f, rec.ID)
	})
	t.Run("an answer that ends in a flag does not reach the judge", func(t *testing.T) {
		f := newFixture(t)
		rec := plainRecordOf(t, rngOf(97, 98), gptID)
		f.anthropic.replies = []providers.Completion{{ToolCalls: []providers.ToolCall{askCall("a", "Why 512Mi?")}},
			answerReply(claudeID)}
		f.openai.replies = []providers.Completion{{Content: "set --client-secret"}}
		s, err := f.scorer.Score(context.Background(), sourceOf(t, rec))
		if err != nil {
			t.Fatal(err)
		}
		if f.anthropic.calls() != 1 || s.Status != StatusQuarantined || s.Check != CheckRedactor {
			t.Fatalf("score %+v after %d judge calls, want quarantined after the first", s, f.anthropic.calls())
		}
		assertNothingDirtySent(t, f, rec.ID)
	})
}

// secretKeyWords returns the key names of the redactor's own list (secretWordExpr and shortKeyNamesExpr in
// internal/redact), read from its source and expanded: "api[_-]?key" gives "apikey", "api_key" and "api-key".
func secretKeyWords(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filepath.Join("..", "redact", "redact.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	consts := map[string]string{}
	ast.Inspect(file, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok || len(vs.Names) != 1 || len(vs.Values) != 1 {
			return true
		}
		if lit, ok := vs.Values[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
			if v, err := strconv.Unquote(lit.Value); err == nil {
				consts[vs.Names[0].Name] = v
			}
		}
		return true
	})
	words := strings.TrimSuffix(strings.TrimPrefix(consts["secretWordExpr"], "(?:"), ")")
	short := consts["shortKeyNamesExpr"]
	if words == "" || short == "" {
		t.Fatal("the redactor's key lists were not found")
	}
	var out []string
	var expand func(string)
	expand = func(w string) {
		if i := strings.Index(w, "[_-]?"); i >= 0 {
			for _, sep := range []string{"", "_", "-"} {
				expand(w[:i] + sep + w[i+len("[_-]?"):])
			}
			return
		}
		if i := strings.Index(w, "?"); i > 0 {
			expand(w[:i-1] + w[i+1:])
			expand(w[:i] + w[i+1:])
			return
		}
		if strings.ContainsAny(w, `[]()?*+\|{}.^$`) {
			t.Fatalf("a key word %q holds a regular expression this test cannot expand", w)
		}
		out = append(out, w)
	}
	for _, w := range strings.Split(words+"|"+short, "|") {
		expand(w)
	}
	return out
}

// judgeField is one string field of the judge's input, in the order the judge reads it.
type judgeField struct {
	name string
	set  func(rec *analysis.Record, end, start string)
}

// adjacentFieldPairs are the pairs of adjacent string fields of the judge's input (judgeInput): the first gets
// a text ending in a flag-like key, the second a value at its start.
var adjacentFieldPairs = []judgeField{
	{"incidentReason|subjectKind", func(rec *analysis.Record, end, start string) {
		rec.Incident.Reason, rec.Incident.SubjectKind = "OOMKilled "+end, start
	}},
	{"subjectKind|summary", func(rec *analysis.Record, end, start string) {
		rec.Incident.SubjectKind, rec.Summary = "Deployment "+end, start+" was used."
	}},
	{"summary|missingData[0]", func(rec *analysis.Record, end, start string) {
		rec.Summary, rec.MissingData = plainSummary+" See "+end, []string{start}
	}},
	{"missingData[0]|missingData[1]", func(rec *analysis.Record, end, start string) {
		rec.MissingData = []string{"the heap profile, then " + end, start}
	}},
}

// TestFlagAndValueAcrossFieldsCorpus is the generated corpus of finding 1: for each key of the redactor's own
// list, written as a flag or a key, at the end of one field, and a random value at the start of the next, across
// every pair of adjacent fields of the judge's input. By construction each value passes the earlier checks on
// its own (checked here), and the answer is what the redactor says of the judge's message as rendered by this
// test: a record whose message the redactor would change is quarantined with no call; any other is scored with
// the message sent byte for byte as rendered.
func TestFlagAndValueAcrossFieldsCorpus(t *testing.T) {
	r, err := redact.New()
	if err != nil {
		t.Fatal(err)
	}
	words := secretKeyWords(t)
	forms := []func(string) string{
		func(w string) string { return "--" + w },
		func(w string) string { return "-" + w },
		func(w string) string { return "--" + w + "=" },
		func(w string) string { return w + "=" },
		func(w string) string { return w + ":" },
	}
	rng := rngOf(2026, 1122)
	sample := 1
	if raceDetector {
		sample = 16
	}
	for _, pair := range adjacentFieldPairs {
		t.Run(pair.name, func(t *testing.T) {
			total, quarantined := 0, 0
			for wi, w := range words {
				for fi, form := range forms {
					if (wi*len(forms)+fi)%sample != 0 {
						continue
					}
					total++
					secret := randomValue(rng)
					rec := plainRecordOf(t, rng, claudeID)
					pair.set(&rec, form(w), secret)
					// By construction the earlier checks see nothing: each value is clean on its own.
					keep, refused := keptValues(rec, rec.ID)
					if refused != nil {
						t.Fatal(refused.reason)
					}
					if kinds, ok := checkJudgeInput(r, rec, keep); !ok {
						t.Fatalf("%s %q: the per-value check refused (%v); the case is not across fields", pair.name, form(w), kinds)
					}
					want := renderJudgeUser(t, rec)
					dirty := !r.Clean(want, rec.ID)
					f := newFixture(t)
					f.openai.replies = []providers.Completion{answerReply(gptID)}
					s, err := f.scorer.Score(context.Background(), sourceOf(t, rec))
					if err != nil {
						t.Fatal(err)
					}
					switch {
					case dirty && (s.Status != StatusQuarantined || s.Check != CheckRedactor || f.modelCalls() != 0):
						t.Fatalf("%s %q then a value: score %+v after %d calls, want quarantined with none",
							pair.name, form(w), s, f.modelCalls())
					case dirty:
						quarantined++
					case s.Status != StatusScored || f.openai.reqs[0].Messages[1].Content != want:
						t.Fatalf("%s %q then a value: a clean record was not scored as rendered (%+v)", pair.name, form(w), s)
					}
				}
			}
			if quarantined == 0 {
				t.Fatalf("no record of %d trips the redactor across %s; the corpus does not reach the case", total, pair.name)
			}
			t.Logf("%s: %d of %d quarantined", pair.name, quarantined, total)
		})
	}
}

// randomValue returns 16 lower-case letters and digits, starting with letters, which the redactor leaves alone
// on their own.
func randomValue(rng *rand.Rand) string {
	const letters = "abcdefghijklmnopqrstuvwxyz"
	const alnum = letters + "0123456789"
	b := make([]byte, 16)
	for i := range b {
		set := alnum
		if i < 4 {
			set = letters
		}
		b[i] = set[rng.IntN(len(set))]
	}
	return string(b)
}

// renderJudgeUser renders the judge's user message as the prompt contract says, apart from judgeMessages.
func renderJudgeUser(t *testing.T, rec analysis.Record) string {
	t.Helper()
	missing := rec.MissingData
	if missing == nil {
		missing = []string{}
	}
	data, err := json.Marshal(map[string]any{"incidentReason": rec.Incident.Reason,
		"subjectKind": rec.Incident.SubjectKind, "summary": rec.Summary, "missingData": missing,
		"escalated": rec.Escalated, "calls": rec.Calls, "toolCalls": rec.ToolCalls})
	if err != nil {
		t.Fatal(err)
	}
	// encoding/json writes a map's keys sorted; the judge input keeps its field order. Re-order by decoding
	// into the field list.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	b.WriteString("<record>\n{")
	for i, k := range []string{"incidentReason", "subjectKind", "summary", "missingData", "escalated", "calls", "toolCalls"} {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%q:%s", k, fields[k])
	}
	b.WriteString("}\n</record>")
	return b.String()
}

// TestOrdinaryCorpusUnderTheSentCheck runs the existing clean corpus through the whole check. Its summaries name
// the Incident and the request, as an analysis may; those names are not kept in the check of what is sent, so
// such a record is quarantined. The count is logged; the same records with a summary that names neither are
// all scored.
func TestOrdinaryCorpusUnderTheSentCheck(t *testing.T) {
	rng := rngOf(7, 7)
	n := corpusSize / 4
	quarantined := 0
	for i := range n {
		rec := recordOf(t, rng, gptID)
		for _, summary := range []string{namedSummary(rec), plainSummary} {
			f := newFixture(t)
			rec.Summary = summary
			f.anthropic.replies = []providers.Completion{answerReply(claudeID)}
			s, err := f.scorer.Score(context.Background(), sourceOf(t, rec))
			if err != nil {
				t.Fatal(err)
			}
			switch {
			case summary == plainSummary && s.Status != StatusScored:
				t.Fatalf("record %d with a plain summary: score %+v", i, s)
			case summary != plainSummary && s.Status == StatusQuarantined:
				quarantined++
			}
			assertNothingDirtySent(t, f, rec.ID)
		}
	}
	t.Logf("ordinary corpus: %d of %d quarantined by the check of what is sent", quarantined, n)
}

// The request a model gets is checked in one place: only send calls a model, and it takes a checkedRequest,
// which only checkRequest makes.
func TestOnlySendCallsAModel(t *testing.T) {
	fset := token.NewFileSet()
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var callers, makers []string
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		{
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					switch x := n.(type) {
					case *ast.SelectorExpr:
						if x.Sel.Name == "Complete" {
							callers = append(callers, fn.Name.Name)
						}
					case *ast.CompositeLit:
						if id, ok := x.Type.(*ast.Ident); ok && id.Name == "checkedRequest" {
							makers = append(makers, fn.Name.Name)
						}
					}
					return true
				})
			}
		}
	}
	if len(callers) != 1 || callers[0] != "send" {
		t.Fatalf("functions that call a model: %v, want only send", callers)
	}
	if len(makers) == 0 || slices.ContainsFunc(makers, func(m string) bool { return m != "checkRequest" }) {
		t.Fatalf("functions that make a checkedRequest: %v, want only checkRequest", makers)
	}
}
