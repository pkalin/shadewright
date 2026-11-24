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
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"strconv"
	"sync"
	"testing"
	"time"

	remediationv1alpha1 "github.com/shadewright-labs/shadewright/api/v1alpha1"
	"github.com/shadewright-labs/shadewright/internal/analysis"
	"github.com/shadewright-labs/shadewright/internal/providers"
	"github.com/shadewright-labs/shadewright/internal/redact"
)

// fakeModel answers from a script, one reply per call, and records the requests.
type fakeModel struct {
	mu      sync.Mutex
	replies []providers.Completion
	errs    []error
	reqs    []providers.CompletionRequest
}

func (f *fakeModel) Complete(_ context.Context, req providers.CompletionRequest) (providers.Completion, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := len(f.reqs)
	f.reqs = append(f.reqs, req)
	var err error
	if i < len(f.errs) {
		err = f.errs[i]
	}
	if i < len(f.replies) {
		return f.replies[i], err
	}
	if err != nil {
		return providers.Completion{}, err
	}
	return providers.Completion{}, errors.New("fake model: no reply scripted")
}

func (f *fakeModel) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reqs)
}

// Model IDs and the month of the tests.
const (
	claudeID  = "claude-opus-5-5"
	gptID     = "gpt-5.1"
	testMonth = "2026-11"
	testRun   = "run-1"
	foundry   = "Foundry"
)

const goodAnswer = `{"specific":4,"consistent":5,"causeSupported":3,"missingData":2,"verdict":"useful","note":"names the limit"}`

func answerReply(model string) providers.Completion {
	return providers.Completion{Content: goodAnswer, Model: model, Usage: providers.Usage{InputTokens: 900, OutputTokens: 80}}
}

func toolReply(calls ...providers.ToolCall) providers.Completion {
	return providers.Completion{ToolCalls: calls, Model: gptID, Usage: providers.Usage{InputTokens: 900, OutputTokens: 30}}
}

func askCall(id, q string) providers.ToolCall {
	args, _ := json.Marshal(map[string]string{"question": q})
	return providers.ToolCall{ID: id, Name: crossCheckTool, Arguments: args}
}

// memSource is a record in memory that counts its reads.
type memSource struct {
	name      string
	data      []byte
	size      int64 // when above zero, reported instead of len(data)
	sizeErr   error
	readErr   error
	sizeCalls int
	readCalls int
}

func (m *memSource) Name() string { return m.name }

func (m *memSource) Size(context.Context) (int64, error) {
	m.sizeCalls++
	if m.sizeErr != nil {
		return 0, m.sizeErr
	}
	if m.size > 0 {
		return m.size, nil
	}
	return int64(len(m.data)), nil
}

func (m *memSource) Read(_ context.Context, limit int64) ([]byte, error) {
	m.readCalls++
	if m.readErr != nil {
		return nil, m.readErr
	}
	if int64(len(m.data)) > limit {
		return nil, errTooLong
	}
	return m.data, nil
}

// memLedger keeps reservations in memory.
type memLedger struct {
	mu       sync.Mutex
	spent    map[Family]int64
	reserved []Reservation
	names    map[string]bool
	spentErr error
}

// List returns the reservations of the month and family, and spent[f] as one reservation of no day or record
// (spend made before the test, which counts toward no record's attempts).
func (l *memLedger) List(_ context.Context, month string, f Family) ([]Reservation, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.spentErr != nil {
		return nil, l.spentErr
	}
	var out []Reservation
	if n := l.spent[f]; n > 0 {
		out = append(out, Reservation{Month: month, Family: f, Run: "before", Call: 1, Tokens: n})
	}
	for _, r := range l.reserved {
		if r.Month == month && r.Family == f {
			out = append(out, r)
		}
	}
	return out, nil
}

func (l *memLedger) Reserve(_ context.Context, r Reservation) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	// The name is checked as DirLedger checks it; the key leaves out the tokens, so a second reservation of
	// one run and call is refused whatever its count.
	if _, err := ReservationName(r); err != nil {
		return err
	}
	key := r.Month + "/" + string(r.Family) + "/" + r.Day + "." + r.Record + "." + r.Run + "." + strconv.Itoa(r.Call)
	if l.names == nil {
		l.names = map[string]bool{}
	}
	if l.names[key] {
		return ErrReservationExists
	}
	l.names[key] = true
	l.reserved = append(l.reserved, r)
	return nil
}

// fixture holds a scorer with fake models of both families.
type fixture struct {
	anthropic *fakeModel
	openai    *fakeModel
	ledger    *memLedger
	scorer    *Scorer
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{anthropic: &fakeModel{}, openai: &fakeModel{}, ledger: &memLedger{}}
	r, err := redact.New()
	if err != nil {
		t.Fatal(err)
	}
	f.scorer = &Scorer{
		Redactor: r,
		Models: map[Family]Model{
			FamilyAnthropic: {Provider: f.anthropic, Name: claudeID},
			FamilyOpenAI:    {Provider: f.openai, Name: gptID},
		},
		Ledger: f.ledger,
		Limits: Limits{MaxCallTokens: 40_000, MaxRunTokens: 100_000, MaxMonthTokens: 4_000_000},
		Run:    testRun,
		Now:    func() time.Time { return time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC) },
	}
	return f
}

func (f *fixture) modelCalls() int { return f.anthropic.calls() + f.openai.calls() }

// rngOf returns a seeded generator, so each corpus is the same on every run.
func rngOf(a, b uint64) *rand.Rand {
	return rand.New(rand.NewPCG(a, b)) // #nosec G404 -- a seeded corpus for a reproducible test, not a secret
}

// hexOf returns n random bytes in hex.
func hexOf(rng *rand.Rand, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(rng.UintN(256) & 0xff) // #nosec G115 -- below 256
	}
	return hex.EncodeToString(b)
}

func uidOf(rng *rand.Rand) string {
	h := hexOf(rng, 16)
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// recordOf builds a record as the analyzer builds one (internal/analysis buildRecord): the names come from the
// real name functions. models are the reply's model IDs. The summary names nothing the redactor matches;
// namedSummary is one that names the Incident and the request.
func recordOf(t *testing.T, rng *rand.Rand, models ...string) analysis.Record {
	t.Helper()
	fp := "sha256:" + hexOf(rng, 32)
	uid := uidOf(rng)
	started := time.Date(2026, 11, 20, 9, int(rng.UintN(60)), int(rng.UintN(60)), 0, time.UTC)
	spec := remediationv1alpha1.RemediationRequestSpec{IncidentRef: remediationv1alpha1.IncidentRef{
		Name: remediationv1alpha1.IncidentName(fp, 1), UID: remediationv1alpha1.UID(uid)}}
	req, err := remediationv1alpha1.RequestName(spec)
	if err != nil {
		t.Fatal(err)
	}
	occurrence := int32(1 + rng.IntN(3)) // #nosec G115 -- 1 to 3
	name := remediationv1alpha1.IncidentName(fp, occurrence)
	providersOf := analysis.RecordProviders{Triage: foundry}
	return analysis.Record{
		SchemaVersion: analysis.RecordSchemaVersion,
		ID:            uid + "-" + started.Format("20060102t150405z"),
		Incident: analysis.RecordIncident{Name: name, UID: uid, Fingerprint: fp, Reason: "OOMKilled",
			SubjectKind: "Deployment", Namespace: "shop", SubjectName: "checkout"},
		StartedAt: started, FinishedAt: started.Add(40 * time.Second), Reason: "Proposed", Calls: 3, ToolCalls: 5,
		Tokens: 41234, Deployments: analysis.RecordDeployments{Triage: "triage-small"}, Providers: providersOf,
		Models: models, PromptVersion: "analysis-12",
		Summary:     plainSummary,
		MissingData: []string{"the heap profile of the checkout pod"},
		Request:     string(req),
	}
}

// sourceOf encodes rec as the file "<ID>.json".
func sourceOf(t *testing.T, rec analysis.Record) *memSource {
	t.Helper()
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	return &memSource{name: rec.ID + ".json", data: data}
}

// namedSummary returns a summary that names the record's Incident and request, as an analysis may.
func namedSummary(rec analysis.Record) string {
	return "The container of " + rec.Incident.Name + " was killed for memory at 512Mi; " + rec.Request +
		" raises the limit to 768Mi."
}
