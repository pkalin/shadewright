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
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/providers/fake"
	"github.com/pkalin/shadewright/internal/redact"
)

// With a store, a run writes one record and its ID goes into status.analysisRecords in the outcome update. The
// record names the request, passes the redactor unchanged with its verified values kept (the check of the
// Blob Storage adapter), and the ID is one the adapter accepts as a blob name.
func TestControllerWritesTheRecord(t *testing.T) {
	ns := namespace(t)
	setSettings(t, 10)
	inc := dueIncident(t, ns, nil)
	f := newFixture()
	f.triage.steps = []step{reply(validResult)}
	c, rc := newController(t, ns, f)
	store := &fake.Store{}
	c.Store = store
	reconcile(t, c, inc)

	got := &remediationv1alpha1.Incident{}
	if err := env.admin.Get(t.Context(), client.ObjectKeyFromObject(inc), got); err != nil {
		t.Fatal(err)
	}
	if len(rc.updates) != 2 || len(rc.updates[0].AnalysisRecords) != 0 {
		t.Fatalf("updates %+v, want the claim without a record ID and the outcome", rc.updates)
	}
	want := recordID(got)
	if len(got.Status.AnalysisRecords) != 1 || got.Status.AnalysisRecords[0] != want {
		t.Fatalf("status.analysisRecords = %v, want [%s]", got.Status.AnalysisRecords, want)
	}
	recs := store.Records()
	rec, ok := recs[string(want)]
	if len(recs) != 1 || !ok {
		t.Fatalf("store holds %d records, want the one with ID %s", len(recs), want)
	}
	if rec.Fingerprint != inc.Spec.Fingerprint {
		t.Errorf("record fingerprint %q", rec.Fingerprint)
	}
	var r Record
	if err := json.Unmarshal(rec.Data, &r); err != nil {
		t.Fatalf("record data: %v", err)
	}
	if r.SchemaVersion != RecordSchemaVersion || r.ID != string(want) || r.Reason != ReasonProposed ||
		r.Request != string(got.Status.RemediationRequests[0]) || r.PromptVersion != PromptVersion() ||
		r.Calls != 1 || len(r.Models) != 1 || r.Incident.UID != string(got.UID) || r.Incident.Name != got.Name ||
		!r.StartedAt.Equal(got.Status.LastAnalysisTime.Time) || r.Summary == "" ||
		r.Deployments.Triage != testLimits().Triage.Deployment || r.Deployments.Escalation != "" {
		t.Errorf("record %+v", r)
	}
	keep := append([]string{rec.ID, rec.Fingerprint}, rec.Verified...)
	if s := string(rec.Data); redact.StringKeeping(s, keep...) != s {
		t.Errorf("the record does not pass the redactor unchanged:\n%s\n%s", s, redact.StringKeeping(s, keep...))
	}
	if !strings.HasPrefix(rec.ID, string(got.UID)+"-") || rec.ID != strings.ToLower(rec.ID) {
		t.Errorf("record ID %q, want the Incident's UID and the run's time in lower case", rec.ID)
	}
}

// A store that fails leaves the outcome as it is: the phase, the condition and the request are written, and no
// record ID is.
func TestControllerRecordFailureKeepsTheOutcome(t *testing.T) {
	ns := namespace(t)
	setSettings(t, 10)
	inc := dueIncident(t, ns, nil)
	f := newFixture()
	f.triage.steps = []step{reply(validResult)}
	c, _ := newController(t, ns, f)
	store := &fake.Store{}
	store.SetFail(func(string) error {
		return errors.Join(errors.New("azureblob: the analysis record already exists"), providers.ErrConflict)
	})
	c.Store = store
	reconcile(t, c, inc)

	got := &remediationv1alpha1.Incident{}
	if err := env.admin.Get(t.Context(), client.ObjectKeyFromObject(inc), got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != remediationv1alpha1.IncidentProposed || len(got.Status.RemediationRequests) != 1 ||
		len(got.Status.AnalysisRecords) != 0 {
		t.Fatalf("status = %+v, want Proposed with the request and no record ID", got.Status)
	}
}

// With too little time left on the claim, the record is skipped so that the outcome write keeps its own full
// timeout.
func TestStoreRecordSkippedNearTheClaimsEnd(t *testing.T) {
	now := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	inc := incident()
	inc.Name, inc.UID = "incident-x", "6f1e0b4a-3c2d-4e5f-8a9b-0c1d2e3f4a5b"
	started := now.Add(-time.Hour)
	inc.Status.LastAnalysisTime = &metav1.Time{Time: started}
	store := &fake.Store{}
	c := &Controller{Store: store, ClaimTimeout: time.Hour + finalWriteTimeout - time.Second,
		Now: func() time.Time { return now }, Analyzer: &Analyzer{}}
	if id := c.storeRecord(t.Context(), inc, Outcome{Reason: ReasonNoProposal}, Limits{}, ""); id != "" || len(store.Records()) != 0 {
		t.Errorf("storeRecord = %q with %d records, want it skipped", id, len(store.Records()))
	}
	c.ClaimTimeout = time.Hour + finalWriteTimeout + time.Minute
	if id := c.storeRecord(t.Context(), inc, Outcome{Reason: ReasonNoProposal}, Limits{}, ""); id == "" || len(store.Records()) != 1 {
		t.Errorf("storeRecord = %q with %d records, want one written", id, len(store.Records()))
	}
}
