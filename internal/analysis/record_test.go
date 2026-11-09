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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	kfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/providers/fake"
	"github.com/pkalin/shadewright/internal/redact"
	"github.com/pkalin/shadewright/internal/testutil/metricguard"
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
	keep := make([]string, 0, 2+len(rec.Verified))
	keep = append(keep, rec.ID, rec.Fingerprint)
	for _, v := range rec.Verified {
		if !v.Valid() {
			t.Errorf("verified value %q is not an object name", v)
		}
		keep = append(keep, string(v))
	}
	if s := string(rec.Data); !redact.Clean(s, keep...) {
		t.Errorf("the record does not pass the redactor:\n%s\n%s", s, redact.StringKeeping(s, keep...))
	}
	if !strings.HasPrefix(rec.ID, string(got.UID)+"-") || rec.ID != strings.ToLower(rec.ID) {
		t.Errorf("record ID %q, want the Incident's UID and the run's time in lower case", rec.ID)
	}
}

// A store that fails leaves the outcome as it is: the phase, the condition and the request are written, and no
// record ID is. The failure is counted and reported with a Warning event on the Incident.
func TestControllerRecordFailureKeepsTheOutcome(t *testing.T) {
	for name, tc := range map[string]struct {
		err    error
		result string
	}{
		"transient": {&providers.RetryableError{Err: errors.New("azureblob: no answer from Blob Storage")}, recordFailed},
		"refused":   {fmt.Errorf("azureblob: the record still holds data: %w", providers.ErrRejected), recordRefused},
	} {
		t.Run(name, func(t *testing.T) {
			ns := namespace(t)
			setSettings(t, 10)
			inc := dueIncident(t, ns, nil)
			f := newFixture()
			f.triage.steps = []step{reply(validResult)}
			c, _ := newController(t, ns, f)
			store := &fake.Store{}
			store.SetFail(func(string) error { return tc.err })
			c.Store = store
			events := record.NewFakeRecorder(4)
			c.Recorder = events
			before := testutil.ToFloat64(analysisRecords.WithLabelValues(tc.result))
			reconcile(t, c, inc)

			got := liveIncident(t, inc)
			if got.Status.Phase != remediationv1alpha1.IncidentProposed || len(got.Status.RemediationRequests) != 1 ||
				len(got.Status.AnalysisRecords) != 0 {
				t.Fatalf("status = %+v, want Proposed with the request and no record ID", got.Status)
			}
			if d := testutil.ToFloat64(analysisRecords.WithLabelValues(tc.result)) - before; d != 1 {
				t.Errorf("result %s counted %v times, want once", tc.result, d)
			}
			select {
			case e := <-events.Events:
				if !strings.HasPrefix(e, "Warning "+EventRecordNotWritten+" ") || strings.Contains(e, "azureblob") {
					t.Errorf("event %q, want the fixed Warning %s", e, EventRecordNotWritten)
				}
			default:
				t.Error("no event for the record that was not written")
			}
		})
	}
}

// A conflict means the record's ID exists. The ID is the run's own, so an earlier try whose answer was lost
// stored it: the ID is recorded in the status, as for a write that succeeded (#451 review).
func TestControllerRecordsTheIDOfARecordThatExists(t *testing.T) {
	ns := namespace(t)
	setSettings(t, 10)
	inc := dueIncident(t, ns, nil)
	f := newFixture()
	f.triage.steps = []step{reply(validResult)}
	c, _ := newController(t, ns, f)
	store := &fake.Store{}
	store.SetFail(func(string) error {
		return fmt.Errorf("azureblob: the analysis record already exists (409 BlobAlreadyExists): %w", providers.ErrConflict)
	})
	c.Store = store
	before := testutil.ToFloat64(analysisRecords.WithLabelValues(recordExists))
	reconcile(t, c, inc)

	got := liveIncident(t, inc)
	if want := recordID(got); !slices.Equal(got.Status.AnalysisRecords, []remediationv1alpha1.RecordID{want}) {
		t.Fatalf("status.analysisRecords = %v, want [%s]", got.Status.AnalysisRecords, want)
	}
	if d := testutil.ToFloat64(analysisRecords.WithLabelValues(recordExists)) - before; d != 1 {
		t.Errorf("exists counted %v times, want once", d)
	}
}

// A run whose claim another run took over writes no record: its outcome is dropped, so a record would describe
// an outcome that never happened (#451 review).
func TestControllerWritesNoRecordForALostClaim(t *testing.T) {
	ns := namespace(t)
	setSettings(t, 10)
	inc := dueIncident(t, ns, nil)
	f := newFixture()
	f.triage.steps = []step{reply(noProposal)}
	successor := metav1.NewTime(time.Now().Add(time.Minute).Truncate(time.Second))
	withHook(f, func(ctx context.Context) {
		live := liveIncident(t, inc)
		live.Status.LastAnalysisTime = &successor
		if err := env.admin.Status().Update(ctx, live); err != nil {
			t.Error(err)
		}
	})
	c, rc := newController(t, ns, f)
	store := &fake.Store{}
	c.Store = store
	reconcile(t, c, inc)
	if n := len(store.Records()); n != 0 {
		t.Errorf("%d records written for a run whose claim was lost, want none", n)
	}
	if len(rc.updates) != 1 {
		t.Errorf("%d status updates, want the claim only", len(rc.updates))
	}
}

// flakyStatusClient fails the first outcome update (the second status update) with a transient error of the
// API server, after or without applying it.
type flakyStatusClient struct {
	*recordingClient
	apply bool
}

func (c flakyStatusClient) Status() client.SubResourceWriter {
	return flakyStatus{SubResourceWriter: c.recordingClient.Status(), c: c}
}

type flakyStatus struct {
	client.SubResourceWriter
	c flakyStatusClient
}

func (s flakyStatus) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	s.c.mu.Lock()
	n := len(s.c.updates)
	s.c.mu.Unlock()
	if n != 1 {
		return s.SubResourceWriter.Update(ctx, obj, opts...)
	}
	if s.c.apply {
		if err := s.SubResourceWriter.Update(ctx, obj, opts...); err != nil {
			return err
		}
	} else {
		s.c.mu.Lock()
		s.c.updates = append(s.c.updates, remediationv1alpha1.IncidentStatus{})
		s.c.mu.Unlock()
	}
	return apierrors.NewServiceUnavailable("the API server is restarting")
}

// An outcome write that fails once with a transient error is tried again, so the record that was written keeps
// its ID in the status: the next run has another ID, and the reference would be lost for good (#451 review). A
// failed try that the API server applied anyway is recognised and not taken for a lost claim.
func TestControllerOutcomeWriteSurvivesATransientError(t *testing.T) {
	for name, apply := range map[string]bool{"not applied": false, "applied": true} {
		t.Run(name, func(t *testing.T) {
			ns := namespace(t)
			setSettings(t, 10)
			inc := dueIncident(t, ns, nil)
			f := newFixture()
			f.triage.steps = []step{reply(validResult)}
			c, rc := newController(t, ns, f)
			c.Client = flakyStatusClient{recordingClient: rc, apply: apply}
			c.Store = &fake.Store{}
			ctx, last := logged(t)
			if _, err := c.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(inc)}); err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			got := liveIncident(t, inc)
			if got.Status.Phase != remediationv1alpha1.IncidentProposed ||
				!slices.Equal(got.Status.AnalysisRecords, []remediationv1alpha1.RecordID{recordID(got)}) {
				t.Errorf("status = %+v, want Proposed with the record ID", got.Status)
			}
			if line := last("Dropped the outcome of an analysis whose claim was lost"); line != "" {
				t.Errorf("an applied outcome was logged as dropped: %s", line)
			}
		})
	}
}

// A request the server created although its create failed is named in the record, as the outcome write
// records it (#451 review).
func TestControllerRecordNamesARequestCreatedAnyway(t *testing.T) {
	ns := namespace(t)
	setSettings(t, 10)
	inc := dueIncident(t, ns, nil)
	f := newFixture()
	f.triage.steps = []step{reply(validResult)}
	c, rc := newController(t, ns, f)
	c.Client = laggingClient{Client: rc, createErr: context.DeadlineExceeded}
	store := &fake.Store{}
	c.Store = store
	reconcile(t, c, inc)

	created := requestsIn(t, ns)
	if len(created) != 1 {
		t.Fatalf("%d requests created, want 1", len(created))
	}
	recs := store.Records()
	if len(recs) != 1 {
		t.Fatalf("%d records, want 1", len(recs))
	}
	for _, rec := range recs {
		var r Record
		if err := json.Unmarshal(rec.Data, &r); err != nil {
			t.Fatal(err)
		}
		if r.Request != created[0].Name {
			t.Errorf("record names request %q, want %q, which the server created", r.Request, created[0].Name)
		}
	}
}

// With too little time left on the claim, the record is skipped so that the outcome write keeps its own full
// timeout; with enough, the claim is read live first and the record written.
func TestStoreRecordSkippedNearTheClaimsEnd(t *testing.T) {
	ns := namespace(t)
	inc := dueIncident(t, ns, nil)
	now := time.Now().Truncate(time.Second)
	started := metav1.NewTime(now.Add(-time.Hour))
	inc.Status.Phase, inc.Status.LastAnalysisTime = remediationv1alpha1.IncidentAnalyzing, &started
	if err := env.admin.Status().Update(t.Context(), inc); err != nil {
		t.Fatal(err)
	}
	store := &fake.Store{}
	c := &Controller{Store: store, Reader: env.admin, ClaimTimeout: time.Hour + finalWriteTimeout - time.Second,
		Now: func() time.Time { return now }, Analyzer: &Analyzer{}}
	before := testutil.ToFloat64(analysisRecords.WithLabelValues(recordSkipped))
	if id, err := c.storeRecord(t.Context(), inc, Outcome{Reason: ReasonNoProposal}, Limits{}, ""); id != "" ||
		err != nil || len(store.Records()) != 0 {
		t.Errorf("storeRecord = %q, %v with %d records, want it skipped", id, err, len(store.Records()))
	}
	if d := testutil.ToFloat64(analysisRecords.WithLabelValues(recordSkipped)) - before; d != 1 {
		t.Errorf("skipped counted %v times, want once", d)
	}
	c.ClaimTimeout = time.Hour + finalWriteTimeout + time.Minute
	if id, err := c.storeRecord(t.Context(), inc, Outcome{Reason: ReasonNoProposal}, Limits{}, ""); id == "" ||
		err != nil || len(store.Records()) != 1 {
		t.Errorf("storeRecord = %q, %v with %d records, want one written", id, err, len(store.Records()))
	}
}

// The record counter takes only its closed set of results and is documented.
func TestAnalysisRecordsMetric(t *testing.T) {
	initRecordMetrics(analysisRecords)
	reg := prometheus.NewRegistry()
	reg.MustRegister(analysisRecords)
	want := map[string]metricguard.Labels{"shadewright_analysis_records_total": {"result": recordResults}}
	metricguard.Check(t, reg, want)
	metricguard.Documented(t, filepath.Join("..", "..", "docs", "reference", "metrics.md"), want)
}

// claimedIncident returns a controller on a fake API server holding an Incident claimed by a run, with a
// store, for the steps after the analysis.
func claimedIncident(t *testing.T) (*Controller, *remediationv1alpha1.Incident) {
	t.Helper()
	inc := incident()
	inc.Name, inc.Namespace = "incident-claimed", "operator"
	inc.UID = "6f1e0b4a-3c2d-4e5f-8a9b-0c1d2e3f4a5b"
	inc.Generation = 1
	inc.Status.Phase = remediationv1alpha1.IncidentAnalyzing
	at := metav1.NewTime(time.Now().UTC().Truncate(time.Second))
	inc.Status.LastAnalysisTime = &at
	scheme := runtime.NewScheme()
	if err := remediationv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	k := kfake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&remediationv1alpha1.Incident{}).
		WithObjects(inc).Build()
	return &Controller{Client: k, Reader: k, Namespace: inc.Namespace, Analyzer: &Analyzer{}, Store: &fake.Store{}}, inc
}

// eofStatusClient fails the first status update with a connection closed before the API server answered.
type eofStatusClient struct {
	client.Client
	tries int
}

func (c *eofStatusClient) Status() client.SubResourceWriter {
	return eofStatus{SubResourceWriter: c.Client.Status(), c: c}
}

type eofStatus struct {
	client.SubResourceWriter
	c *eofStatusClient
}

func (s eofStatus) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	s.c.tries++
	if s.c.tries == 1 {
		return fmt.Errorf("updating Incident status: %w", io.EOF)
	}
	return s.SubResourceWriter.Update(ctx, obj, opts...)
}

// An outcome write whose connection closed without an answer (EOF, a reset) is tried again like a transient
// error of the API server, so the stored record keeps its ID in the status (#451 review).
func TestFinishRetriesAClosedConnection(t *testing.T) {
	c, inc := claimedIncident(t)
	out := Outcome{Reason: ReasonNoProposal}
	id, err := c.storeRecord(t.Context(), inc, out, Limits{}, "")
	if err != nil || id == "" {
		t.Fatalf("storeRecord: %q, %v", id, err)
	}
	k := &eofStatusClient{Client: c.Client}
	c.Client = k
	if err := c.finish(t.Context(), inc, out, "", "", id); err != nil {
		t.Fatalf("finish dropped stored record %s after %d write(s): %v", id, k.tries, err)
	}
}

// failingRequestReader fails the first live read of a RemediationRequest with a transient error.
type failingRequestReader struct {
	client.Reader
	reads int
}

func (r *failingRequestReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*remediationv1alpha1.RemediationRequest); ok {
		r.reads++
		if r.reads == 1 {
			return apierrors.NewServiceUnavailable("temporary read failure")
		}
	}
	return r.Reader.Get(ctx, key, obj, opts...)
}

// After a create whose answer was lost, the request the server created is looked up once, so the record and
// the outcome name the same request even when one read fails (#451 review).
func TestConcludeRecordAndOutcomeAgreeOnARecoveredRequest(t *testing.T) {
	c, inc := claimedIncident(t)
	_, action, err := DecodeResult(validResult)
	if err != nil {
		t.Fatal(err)
	}
	c.Analyzer.cfg.Policies = map[remediationv1alpha1.ActionType]remediationv1alpha1.Name{action.Type: memoryPolicy}
	spec := requestSpec(inc, Plan{Action: *action, PolicyRef: memoryPolicy})
	name, err := requestName(inc, spec)
	if err != nil {
		t.Fatal(err)
	}
	rr := &remediationv1alpha1.RemediationRequest{ObjectMeta: metav1.ObjectMeta{Name: string(name),
		Namespace: inc.Namespace, Labels: map[string]string{LabelIncidentUID: string(inc.UID)}}, Spec: spec}
	if err := c.Client.Create(t.Context(), rr); err != nil {
		t.Fatal(err)
	}
	c.Reader = &failingRequestReader{Reader: c.Reader}
	id, err := c.conclude(t.Context(), inc, Outcome{Reason: ReasonRequestNotCreated}, Limits{}, "", name)
	if err != nil || id == "" {
		t.Fatalf("conclude: %q, %v", id, err)
	}
	live := &remediationv1alpha1.Incident{}
	if err := c.Client.Get(t.Context(), client.ObjectKeyFromObject(inc), live); err != nil {
		t.Fatal(err)
	}
	var rec Record
	if err := json.Unmarshal(c.Store.(*fake.Store).Records()[string(id)].Data, &rec); err != nil {
		t.Fatal(err)
	}
	if len(live.Status.RemediationRequests) != 1 || rec.Request != string(live.Status.RemediationRequests[0]) {
		t.Fatalf("record request = %q, Incident requests = %v", rec.Request, live.Status.RemediationRequests)
	}
}

// A claim lost after the record was stored and before the outcome write leaves the record in the
// create-only store; the log line of the dropped outcome names it, so it can be found (#454).
func TestControllerNamesTheRecordOfADroppedOutcome(t *testing.T) {
	ns := namespace(t)
	setSettings(t, 10)
	inc := dueIncident(t, ns, nil)
	f := newFixture()
	f.triage.steps = []step{reply(noProposal)}
	c, _ := newController(t, ns, f)
	store := &fake.Store{}
	successor := metav1.NewTime(time.Now().Add(time.Minute).Truncate(time.Second))
	store.After = func(string) error {
		live := liveIncident(t, inc)
		live.Status.LastAnalysisTime = &successor
		return env.admin.Status().Update(context.Background(), live)
	}
	c.Store = store
	ctx, last := logged(t)
	if _, err := c.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(inc)}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	recs := store.Records()
	if len(recs) != 1 {
		t.Fatalf("%d records, want the one stored before the claim was lost", len(recs))
	}
	line := last("Dropped the outcome of an analysis whose claim was lost")
	for id := range recs {
		if !strings.Contains(line, `"record"="`+id+`"`) {
			t.Errorf("the dropped outcome's line does not name record %s: %q", id, line)
		}
	}
}

// countingReader counts the live reads of Incidents and fails those for which fail(n) holds (n from 1) with a
// transient error.
type countingReader struct {
	client.Reader
	fail  func(n int) bool
	reads int
}

func (r *countingReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*remediationv1alpha1.Incident); ok {
		r.reads++
		if r.fail(r.reads) {
			return apierrors.NewServiceUnavailable("the API server is restarting")
		}
	}
	return r.Reader.Get(ctx, key, obj, opts...)
}

// A failed live read before the outcome write sends nothing, so the next try does not first look for an
// outcome that could not have been written: one read for the failed try, one for the write (#454).
func TestFinishTakesAFailedReadForNoWrite(t *testing.T) {
	c, inc := claimedIncident(t)
	r := &countingReader{Reader: c.Reader, fail: func(n int) bool { return n == 1 }}
	c.Reader = r
	if err := c.finish(t.Context(), inc, Outcome{Reason: ReasonNoProposal}, "", "", ""); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if r.reads != 2 {
		t.Errorf("%d live reads of the Incident, want 2", r.reads)
	}
}

// Without a store the time budget's error does not name a record write (#454).
func TestCheckTimeBudgetNamesTheRecordWriteOnlyWithAStore(t *testing.T) {
	a, err := New(func() Config { f := newFixture(); f.cfg.Timeout = MaxTimeout; return f.cfg }())
	if err != nil {
		t.Fatal(err)
	}
	c := &Controller{Analyzer: a, Reader: kfake.NewClientBuilder().Build(), ClaimTimeout: MaxTimeout}
	if err := c.CheckTimeBudget(); err == nil || strings.Contains(err.Error(), "record") {
		t.Errorf("without a store: %v, want an error that names no record write", err)
	}
	c.Store = &fake.Store{}
	if err := c.CheckTimeBudget(); err == nil || !strings.Contains(err.Error(), "the record write ("+storeTimeout.String()+")") {
		t.Errorf("with a store: %v, want an error that names the record write", err)
	}
}

// initRecordMetrics starts every series at zero; SetupWithManager calls it only with a store, so "no store" (no
// series) and "nothing written" (zeros) differ (#454).
func TestRecordMetricsStartOnlyWithAStore(t *testing.T) {
	v := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "records_total"}, []string{recordResultLabel})
	initRecordMetrics(v)
	if n := testutil.CollectAndCount(v); n != len(recordResults) {
		t.Errorf("%d series with a store, want %d", n, len(recordResults))
	}
}

// appliedEOFStatus applies the first status update and then answers with a closed connection.
type appliedEOFStatus struct {
	client.SubResourceWriter
	tries *int
}

func (s appliedEOFStatus) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	*s.tries++
	if err := s.SubResourceWriter.Update(ctx, obj, opts...); err != nil || *s.tries > 1 {
		return err
	}
	return fmt.Errorf("updating Incident status: %w", io.EOF)
}

type appliedEOFClient struct {
	client.Client
	tries int
}

func (c *appliedEOFClient) Status() client.SubResourceWriter {
	return appliedEOFStatus{SubResourceWriter: c.Client.Status(), tries: &c.tries}
}

// An outcome write that was applied though its answer was lost, followed by a failed look for it, is looked
// for again, not taken for a lost claim (#454 review).
func TestFinishLooksAgainAfterAFailedLook(t *testing.T) {
	c, inc := claimedIncident(t)
	c.Client = &appliedEOFClient{Client: c.Client}
	// Read 1 is the recheck before the write; read 2, the first look for the applied outcome, fails.
	c.Reader = &countingReader{Reader: c.Reader, fail: func(n int) bool { return n == 2 }}
	if err := c.finish(t.Context(), inc, Outcome{Reason: ReasonNoProposal}, "", "", ""); err != nil {
		t.Fatalf("finish took an applied outcome for a lost claim: %v", err)
	}
}
