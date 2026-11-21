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

// Package intakerun scores one ordinary record through the scoring intake with a given judge, for the tests of
// a model adapter that check the intake's requests are ones the adapter can send. Tests only.
package intakerun

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/analysis"
	"github.com/pkalin/shadewright/internal/intake"
	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/redact"
)

// Model IDs of the record's model, by the judge's family: the judge is of the other family.
const (
	claudeID = "claude-opus-5-5"
	gptID    = "gpt-5.1"
)

// CrossModel is the cross-check model: it answers every call with Answer and counts the calls.
type CrossModel struct {
	Answer string
	Model  string
	mu     sync.Mutex
	calls  int
}

// Complete returns the answer.
func (c *CrossModel) Complete(context.Context, providers.CompletionRequest) (providers.Completion, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	return providers.Completion{Content: c.Answer, Model: c.Model,
		Usage: providers.Usage{InputTokens: 100, OutputTokens: 20}}, nil
}

// Calls returns the number of calls made.
func (c *CrossModel) Calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

type source struct {
	name string
	data []byte
}

func (s source) Name() string                                { return s.name }
func (s source) Size(context.Context) (int64, error)         { return int64(len(s.data)), nil }
func (s source) Read(context.Context, int64) ([]byte, error) { return s.data, nil }

type ledger struct{}

func (ledger) List(context.Context, string, intake.Family) ([]intake.Reservation, error) {
	return nil, nil
}
func (ledger) Reserve(context.Context, intake.Reservation) error { return nil }

// Score scores one ordinary record, as the analyzer writes one, whose model is of the other family than
// judgeFamily: judge is the judge and cross answers the cross-check.
func Score(t *testing.T, judge providers.ModelProvider, judgeFamily intake.Family, cross *CrossModel) (intake.Score, error) {
	t.Helper()
	r, err := redact.New()
	if err != nil {
		t.Fatal(err)
	}
	recordModel, judgeName := gptID, claudeID
	if judgeFamily == intake.FamilyOpenAI {
		recordModel, judgeName = claudeID, gptID
	}
	if cross.Model == "" {
		cross.Model = recordModel
	}
	models := map[intake.Family]intake.Model{
		judgeFamily:         {Provider: judge, Name: judgeName},
		judgeFamily.Other(): {Provider: cross, Name: recordModel},
	}
	s := &intake.Scorer{Redactor: r, Models: models, Ledger: ledger{}, Run: "run-1",
		Limits: intake.Limits{MaxCallTokens: 40_000, MaxRunTokens: 100_000, MaxMonthTokens: 4_000_000},
		Now:    func() time.Time { return time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC) }}
	rec, err := record(recordModel)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	return s.Score(context.Background(), source{name: rec.ID + ".json", data: data})
}

// record builds a record as the analyzer does, with the real name functions.
func record(model string) (analysis.Record, error) {
	fp := "sha256:3f7a9c1e5b2d8f4a6c0e2b4d6f8a1c3e5b7d9f0a2c4e6b8d0f1a3c5e7b9d2f4a"
	uid := "5c1d2e3f-4a5b-4c6d-8e7f-90a1b2c3d4e5"
	started := time.Date(2026, 11, 20, 9, 14, 3, 0, time.UTC)
	spec := remediationv1alpha1.RemediationRequestSpec{IncidentRef: remediationv1alpha1.IncidentRef{
		Name: remediationv1alpha1.IncidentName(fp, 1), UID: remediationv1alpha1.UID(uid)}}
	req, err := remediationv1alpha1.RequestName(spec)
	if err != nil {
		return analysis.Record{}, errors.New("the request name of the test record")
	}
	return analysis.Record{
		SchemaVersion: analysis.RecordSchemaVersion,
		ID:            uid + "-" + started.Format("20060102t150405z"),
		Incident: analysis.RecordIncident{Name: remediationv1alpha1.IncidentName(fp, 1), UID: uid, Fingerprint: fp,
			Reason: "OOMKilled", SubjectKind: "Deployment", Namespace: "shop", SubjectName: "checkout"},
		StartedAt: started, FinishedAt: started.Add(40 * time.Second), Reason: "Proposed", Calls: 3, ToolCalls: 5,
		Tokens: 41234, Deployments: analysis.RecordDeployments{Triage: "triage-small"},
		Providers: analysis.RecordProviders{Triage: "Foundry"}, Models: []string{model}, PromptVersion: "analysis-12",
		Summary:     "The container was killed for memory at 512Mi; the proposal raises the limit to 768Mi.",
		MissingData: []string{"the heap profile of the checkout pod"},
		Request:     string(req),
	}, nil
}
