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

package analysis

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/pkalin/shadewright/internal/tools"
)

// readModelTextMainTable reads modelTextMainTable: by row key, origin/main's answer per control ('h' or 's').
func readModelTextMainTable(t *testing.T) map[string]string {
	t.Helper()
	f, err := os.Open(modelTextMainTable)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	rows := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "#") || line == "" {
			continue
		}
		cut := strings.LastIndexByte(line, '\t')
		rows[line[:cut]] = line[cut+1:]
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return rows
}

// mainHid reports whether origin/main hid the secret of case c, from the table.
func mainHid(t *testing.T, rows map[string]string, c modelTextCase) bool {
	t.Helper()
	row, ok := rows[modelTextKey(c)]
	ctl := slicesIndex(modelTextControls(), c.ctl)
	if !ok || len(row) != len(modelTextControls()) || ctl < 0 {
		t.Fatalf("the table has no answer for case %s control %U: regenerate it on origin/main", modelTextKey(c), c.ctl)
	}
	return row[ctl] == 'h'
}

func slicesIndex(rs []rune, r rune) int {
	for i, x := range rs {
		if x == r {
			return i
		}
	}
	return -1
}

// jsonText is s as a JSON string; every second case writes DEL and C1 raw, as encoding/json does, and the others
// escape them, so both forms a reply can carry are read.
func jsonText(s string, escapeAll bool) string {
	b, _ := json.Marshal(s)
	if !escapeAll {
		return string(b)
	}
	var out strings.Builder
	for _, r := range string(b) {
		if r >= 0x7f && r <= 0x9f {
			fmt.Fprintf(&out, `\u%04x`, r)
		} else {
			out.WriteRune(r)
		}
	}
	return out.String()
}

// storedOf returns every text an analysis stores of the model's reply: the outcome's summary and missing data
// (the log line, resultFields) and the analysis record.
func storedOf(t *testing.T, f *fixture, out Outcome) []string {
	t.Helper()
	inc := incident()
	inc.Name, inc.UID = "inc-2", "0b0e3c1c-7f4a-4c55-9c39-6a3e2d2f0a03"
	inc.Status.LastAnalysisTime = &metav1.Time{Time: time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)}
	a, err := New(f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	c := &Controller{Analyzer: a, Now: func() time.Time { return inc.Status.LastAnalysisTime.Time }}
	rec, err := c.buildRecord(inc, recordID(inc), out, testLimits(), "")
	if err != nil {
		t.Fatal(err)
	}
	return append(append([]string{out.Summary, fmt.Sprint(resultFields(out)...)}, out.MissingData...), string(rec.Data))
}

// The model text corpus (controls_main_corpus_test.go) through the loop, with origin/main as the oracle (decision
// of 2026-11-22 on #852: no input shows a secret origin/main hides): each case as the summary and as an item of
// the missing data, which are stored, is hidden wherever origin/main hid it, and what is stored holds no control;
// each case as a hypothesis, the uncertainty, an evidence quote or a tool call's argument, which are never
// stored, shows up in nothing stored.
func TestModelTextHidesWhatMainHid(t *testing.T) {
	rows := readModelTextMainTable(t)
	cases := modelTextCases()
	fail := func(c modelTextCase, field, got string) {
		t.Helper()
		t.Errorf("%s of form %d, place %d, control %U, prefix %q: stored %q shows %q", field, c.form, c.pos, c.ctl,
			modelTextPrefixes[c.prefix], got, c.secret)
	}
	hiddenOnMain, checked := storedFieldsHide(t, rows, cases, fail)
	checked += unstoredFieldsHide(t, cases, fail)
	checked += toolCallsHide(t, cases, fail)
	t.Logf("%d cases, %d checks, %d hidden on origin/main in a stored field", len(cases), checked, hiddenOnMain)
	if hiddenOnMain == 0 {
		t.Fatal("no case was hidden on origin/main: the oracle is not read")
	}
}

// fieldSummary and fieldMissing name the stored fields in the failures.
const (
	fieldSummary = "summary"
	fieldMissing = "missing data"
)

// storedFieldsHide runs the cases as the summary and ten items of missing data per analysis, and returns how many
// origin/main hid and how many were checked.
func storedFieldsHide(t *testing.T, rows map[string]string, cases []modelTextCase,
	fail func(c modelTextCase, field, got string)) (hiddenOnMain, checked int) {
	t.Helper()
	for from := 0; from < len(cases); from += 11 {
		batch := cases[from:min(from+11, len(cases))]
		missing := make([]string, 0, len(batch)-1)
		for i, c := range batch[1:] {
			missing = append(missing, jsonText(c.text, (from+i)%2 == 0))
		}
		res := `{"summary":` + jsonText(batch[0].text, from%2 == 1) + `,"hypotheses":[],"evidence":[],"missingData":[` +
			strings.Join(missing, ",") + `],"uncertainty":"","proposal":null}`
		f := newFixture()
		f.triage.steps = []step{reply(res)}
		out := f.analyze(t, testLimits())
		if want := ReasonMissingData; len(batch) == 1 && out.Reason != ReasonNoProposal || len(batch) > 1 && out.Reason != want {
			t.Fatalf("cases %d..: reason %s, want MissingData", from, out.Reason)
		}
		if len(out.MissingData) != len(batch)-1 {
			t.Fatalf("cases %d..: %d items of missing data, want %d", from, len(out.MissingData), len(batch)-1)
		}
		stored := storedOf(t, f, out)
		for i, c := range batch {
			got, field := out.Summary, fieldSummary
			if i > 0 {
				got, field = out.MissingData[i-1], fieldMissing
			}
			if heldControl(got) {
				t.Errorf("%s %q holds a control", field, got)
			}
			checked++
			if !mainHid(t, rows, c) {
				continue
			}
			hiddenOnMain++
			if modelTextShown(got, c.secret) {
				fail(c, field, got)
			}
			// The record and the log line hold the same text redacted again: what is hidden above stays hidden there.
			for _, s := range stored[1:] {
				if modelTextShown(s, c.secret) && !anyShownOnMain(t, rows, batch, c.secret) {
					fail(c, "record or log line", s)
				}
			}
		}
	}
	return hiddenOnMain, checked
}

// unstoredFieldsHide runs the cases as five hypotheses, the uncertainty and ten quotes per analysis, fields that
// are never stored, and returns how many checks it made.
func unstoredFieldsHide(t *testing.T, cases []modelTextCase, fail func(c modelTextCase, field, got string)) (checked int) {
	t.Helper()
	for from := 0; from < len(cases); from += 16 {
		batch := cases[from:min(from+16, len(cases))]
		var hyp, ev []string
		unc := `""`
		for i, c := range batch {
			s := jsonText(c.text, (from+i)%2 == 0)
			switch {
			case i < 5:
				hyp = append(hyp, `{"description":`+s+`}`)
			case i == 5:
				unc = s
			default:
				ev = append(ev, `{"source":"c1","quote":`+s+`}`)
			}
		}
		res := `{"summary":"plain","hypotheses":[` + strings.Join(hyp, ",") + `],"evidence":[` + strings.Join(ev, ",") +
			`],"missingData":["more"],"uncertainty":` + unc + `,"proposal":null}`
		f := newFixture()
		f.triage.steps = []step{reply(res)}
		out := f.analyze(t, testLimits())
		for _, s := range storedOf(t, f, out) {
			for _, c := range batch {
				checked++
				if modelTextShown(s, c.secret) {
					fail(c, "hypothesis, uncertainty or quote", s)
				}
			}
		}
	}
	return checked
}

// toolCallsHide runs one case in seven as the argument of a tool call, four calls per analysis: each is refused
// with a fixed text and shows up in nothing stored. It returns how many checks it made.
func toolCallsHide(t *testing.T, cases []modelTextCase, fail func(c modelTextCase, field, got string)) (checked int) {
	t.Helper()
	var sample []modelTextCase
	for i, c := range cases {
		if i%7 == 0 {
			sample = append(sample, c)
		}
	}
	for from := 0; from < len(sample); from += 4 {
		batch := sample[from:min(from+4, len(sample))]
		tcs := toolReply()
		for i, c := range batch {
			tcs.c.ToolCalls = append(tcs.c.ToolCalls, toolCall(fmt.Sprintf("c%d", i), tools.NameListEvents,
				`{"namespace":`+jsonText(c.text, i%2 == 0)+`}`))
		}
		f := newFixture()
		f.builder.c.Truncated = true
		f.escalation.steps = []step{tcs, reply(noProposal)}
		out := f.analyze(t, testLimits())
		if len(f.tools.calls) != 0 {
			t.Fatalf("a call that held a control ran: %s", f.tools.calls[0].Arguments)
		}
		for _, s := range append(storedOf(t, f, out), toolMessages(f.escalation)["c0"]) {
			for _, c := range batch {
				checked++
				if modelTextShown(s, c.secret) {
					fail(c, "tool call", s)
				}
			}
		}
	}
	return checked
}

// anyShownOnMain reports whether origin/main showed secret in another case of batch: the record and the log line
// hold every case of the batch, so a secret shown there may be one origin/main showed too.
func anyShownOnMain(t *testing.T, rows map[string]string, batch []modelTextCase, secret string) bool {
	t.Helper()
	for _, c := range batch {
		if c.secret == secret && !mainHid(t, rows, c) {
			return true
		}
	}
	return false
}

// A carriage return before "Bearer" is the word boundary the rule reads: origin/main stored
// "x\rBearer [credential]" (run at a9b4120f). Redacting after the controls are removed read "xBearer" and stored the
// token; the summary and the missing data are redacted as the reply held them, then cleaned (#852).
func TestSummaryRedactedBeforeItsControlsGo(t *testing.T) {
	f := newFixture()
	f.triage.steps = []step{reply(`{"summary":"x\rBearer Qz7secretValue1","hypotheses":[],"evidence":[],` +
		`"missingData":["y\u0085Bearer Qz7secretValue1"],"uncertainty":"","proposal":null}`)}
	out := f.analyze(t, testLimits())
	if want := "xBearer [credential]"; out.Summary != want {
		t.Errorf("summary %q, want %q", out.Summary, want)
	}
	if len(out.MissingData) != 1 || strings.Contains(out.MissingData[0], "Qz7secret") || heldControl(out.MissingData[0]) {
		t.Errorf("missing data %q, want the token hidden and no control", out.MissingData)
	}
}
