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

package incidents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	corerecord "k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/analyzer/collector"
)

// The API server is started once, on first use. Each test works in its own namespace, so the cap on
// Incidents and the loaded records of one test never reach another.
var env struct {
	once    sync.Once
	err     error
	testEnv *envtest.Environment
	admin   client.Client
	nsSeq   atomic.Int32
}

func TestMain(m *testing.M) {
	code := m.Run()
	if env.testEnv != nil {
		_ = env.testEnv.Stop()
	}
	os.Exit(code)
}

func envtestBinaryDir() string {
	base := filepath.Join("..", "..", "bin", "k8s")
	entries, err := os.ReadDir(base)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if e.IsDir() {
			return filepath.Join(base, e.Name())
		}
	}
	return ""
}

// namespace starts the API server if needed and returns a new namespace.
func namespace(t *testing.T) string {
	t.Helper()
	env.once.Do(func() {
		scheme := runtime.NewScheme()
		if env.err = clientgoscheme.AddToScheme(scheme); env.err != nil {
			return
		}
		if env.err = remediationv1alpha1.AddToScheme(scheme); env.err != nil {
			return
		}
		env.testEnv = &envtest.Environment{
			CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
			ErrorIfCRDPathMissing: true,
			BinaryAssetsDirectory: envtestBinaryDir(),
		}
		cfg, err := env.testEnv.Start()
		if err != nil {
			env.err = err
			return
		}
		env.admin, env.err = client.New(cfg, client.Options{Scheme: scheme})
	})
	if env.err != nil {
		t.Fatalf("starting envtest: %v", env.err)
	}
	ns := fmt.Sprintf("operator-%d", env.nsSeq.Add(1))
	if err := env.admin.Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
		t.Fatal(err)
	}
	return ns
}

// countingClient counts the writes that reach the API server and lets a test act just before a status
// update (before), replace its result (after), or answer it without sending it (instead). Those count and
// see only the observation writes: the analysis trigger's writes (field owner triggerFieldOwner) are counted
// in marks and go through mark instead.
type countingClient struct {
	client.Client
	creates, updates, marks atomic.Int32
	before                  func(ctx context.Context, inc *remediationv1alpha1.Incident)
	after                   func(err error) error
	// instead, when it returns an error, answers a status update with it without sending the update.
	instead func(ctx context.Context) error
	// mark, when it returns an error, answers a write of the analysis trigger with it without sending it.
	mark func(obj client.Object) error
}

func (c *countingClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	c.creates.Add(1)
	return c.Client.Create(ctx, obj, opts...)
}

func (c *countingClient) Status() client.SubResourceWriter {
	return &countingStatus{SubResourceWriter: c.Client.Status(), c: c}
}

type countingStatus struct {
	client.SubResourceWriter
	c *countingClient
}

func (s *countingStatus) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	if (&client.SubResourceUpdateOptions{}).ApplyOptions(opts).FieldManager == triggerFieldOwner {
		s.c.marks.Add(1)
		if s.c.mark != nil {
			if err := s.c.mark(obj); err != nil {
				return err
			}
		}
		return s.SubResourceWriter.Update(ctx, obj, opts...)
	}
	s.c.updates.Add(1)
	if s.c.before != nil {
		s.c.before(ctx, obj.(*remediationv1alpha1.Incident))
	}
	if s.c.instead != nil {
		if err := s.c.instead(ctx); err != nil {
			return err
		}
	}
	err := s.SubResourceWriter.Update(ctx, obj, opts...)
	if s.c.after != nil {
		err = s.c.after(err)
	}
	return err
}

// running is a controller started on its own channel, stopped by stop.
type running struct {
	*Controller
	writes *countingClient
	obs    chan collector.Observation
	stop   func()
}

func start(t *testing.T, ns string, cfg Config) *running {
	t.Helper()
	return startWith(t, ns, cfg, nil)
}

// startWith is start with setup called on the controller and its counting client before it starts.
func startWith(t *testing.T, ns string, cfg Config, setup func(*Controller, *countingClient)) *running {
	t.Helper()
	cfg.Namespace = ns
	if cfg.Window == 0 {
		cfg.Window = time.Second
	}
	if cfg.SettleLimit == 0 {
		// The short test window would otherwise make the settle limit 1 s too.
		cfg.SettleLimit = DefaultWindow
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r := &running{Controller: c, writes: &countingClient{Client: env.admin}, obs: make(chan collector.Observation)}
	c.client, c.reader, c.cache, c.obs = r.writes, env.admin, env.admin, r.obs
	if setup != nil {
		setup(c, r.writes)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Start(ctx) }()
	var once sync.Once
	r.stop = func() {
		once.Do(func() {
			cancel()
			if err := <-done; err != nil {
				t.Errorf("controller stopped with %v", err)
			}
		})
	}
	t.Cleanup(r.stop)
	return r
}

func fp(n int) string { return fmt.Sprintf("sha256:%064x", n) }

// observation returns an observation of fingerprint fp(f), occurrence eventID(i).
func observation(f, i int, total, count int64, began time.Time) collector.Observation {
	return collector.Observation{
		Fingerprint: fp(f),
		Subject: remediationv1alpha1.ObjectRef{
			APIVersion: "apps/v1", Kind: "Deployment", Namespace: "demo", Name: "orders",
			UID: "33333333-3333-4333-8333-333333333333",
		},
		Reason: "BackOff", Type: corev1.EventTypeWarning, ReportingController: "kubelet",
		Symptom:    "Back-off restarting failed container worker",
		Occurrence: eventID(i), Total: total, Count: count, Began: began, ObservedAt: began,
	}
}

// get reads the Incident of fp(1).
func get(t *testing.T, ns string) *remediationv1alpha1.Incident {
	t.Helper()
	inc := &remediationv1alpha1.Incident{}
	if err := env.admin.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: Name(fp(1))}, inc); err != nil {
		t.Fatalf("reading the Incident: %v", err)
	}
	return inc
}

// eventually waits until cond holds for the Incident of fingerprint fp(f).
func eventually(t *testing.T, ns string, f int, cond func(*remediationv1alpha1.Incident) bool) *remediationv1alpha1.Incident {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		inc := &remediationv1alpha1.Incident{}
		err := env.admin.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: Name(fp(f))}, inc)
		if err == nil && cond(inc) {
			return inc
		}
		if err != nil && !apierrors.IsNotFound(err) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("condition not met; last %+v (%v)", inc.Status, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// remembered waits until the controller's memory holds the drop mark of inc, the Incident as read from the
// API server. The controller updates its memory only after the status write returns, so a test that saw the
// write on the API server may otherwise ask Handled before then. The drop mark is set under mu together with
// the removal of the records the write dropped, so once it is there, so is the rest of that write.
func remembered(t *testing.T, r *running, inc *remediationv1alpha1.Incident) {
	t.Helper()
	want := timeFrom(inc.Status.OccurrencesDroppedThrough)
	deadline := time.Now().Add(20 * time.Second)
	for {
		r.mu.Lock()
		got := r.droppedThrough
		r.mu.Unlock()
		if !got.Before(want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the controller remembers records dropped through %v, want %v", got, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func countIs(n int64) func(*remediationv1alpha1.Incident) bool {
	return func(inc *remediationv1alpha1.Incident) bool { return inc.Status.Count == n }
}

func TestBurstMakesOneIncidentWithFewWrites(t *testing.T) {
	ns := namespace(t)
	r := start(t, ns, Config{Window: 2 * time.Second})
	now := time.Now().Truncate(time.Second)
	for i := range 100 {
		r.obs <- observation(1, i, 1, 1, now.Add(time.Duration(i)*time.Second))
	}
	inc := eventually(t, ns, 1, countIs(100))
	var list remediationv1alpha1.IncidentList
	if err := env.admin.List(t.Context(), &list, client.InNamespace(ns)); err != nil || len(list.Items) != 1 {
		t.Fatalf("%d Incidents (%v), want one", len(list.Items), err)
	}
	// The first observation is written at once, the other 99 in one batch after the window.
	if c, u := r.writes.creates.Load(), r.writes.updates.Load(); c != 1 || u > 3 {
		t.Fatalf("%d creates and %d status writes for a burst of 100, want 1 and at most 3", c, u)
	}
	if len(inc.Status.Occurrences) != remediationv1alpha1.MaxOccurrenceRecords || inc.Status.Occurrences[0].ID != eventID(99) {
		t.Fatalf("records %d, first %s: want the newest 32", len(inc.Status.Occurrences), inc.Status.Occurrences[0].ID)
	}
	if inc.Status.Phase != remediationv1alpha1.IncidentOpen || inc.Status.FirstSeen == nil || inc.Status.LastSeen == nil {
		t.Fatalf("status %+v", inc.Status)
	}

	// The records the bound dropped are answered as counted in full; so is an unknown occurrence that began
	// before them, while a newer unknown one was never counted.
	if inc.Status.OccurrencesDroppedThrough == nil {
		t.Fatal("no drop mark after 100 occurrences")
	}
	remembered(t, r, inc)
	for _, q := range []struct {
		id    string
		began time.Time
		want  int64
		found bool
	}{
		{eventID(99), now.Add(99 * time.Second), 1, true},
		{eventID(3), now.Add(3 * time.Second), collector.AllHandled, true},
		{eventID(1000), now.Add(-time.Hour), collector.AllHandled, true},
		{eventID(1001), now.Add(time.Hour), 0, false},
	} {
		got, found, err := r.Handled(t.Context(), q.id, q.began)
		if err != nil || got != q.want || found != q.found {
			t.Errorf("Handled(%s) = %d, %v, %v; want %d, %v", q.id, got, found, err, q.want, q.found)
		}
	}
}

func TestConflictRetriesWithoutLosingCounts(t *testing.T) {
	ns := namespace(t)
	// A failed write would wait an hour: the count can only arrive in time through the retry on conflict.
	r := start(t, ns, Config{RetryAfter: time.Hour})
	now := time.Now().Truncate(time.Second)
	r.obs <- observation(1, 1, 2, 2, now)
	eventually(t, ns, 1, countIs(2))

	// Another writer changes the status just before the next write, so it conflicts once.
	var once sync.Once
	r.writes.before = func(ctx context.Context, _ *remediationv1alpha1.Incident) {
		once.Do(func() {
			inc := get(t, ns)
			meta.SetStatusCondition(&inc.Status.Conditions, metav1.Condition{
				Type: "Reviewed", Status: metav1.ConditionTrue, Reason: "Test", Message: "set by another writer",
			})
			if err := env.admin.Status().Update(ctx, inc); err != nil {
				t.Errorf("updating as another writer: %v", err)
			}
		})
	}
	r.obs <- observation(1, 2, 5, 5, now)
	inc := eventually(t, ns, 1, countIs(7))
	if meta.FindStatusCondition(inc.Status.Conditions, "Reviewed") == nil {
		t.Fatal("the retry overwrote the other writer's change")
	}
	if r.writes.updates.Load() < 3 {
		t.Fatalf("%d status writes: the conflict did not happen", r.writes.updates.Load())
	}
}

func TestLostAnswerIsNotCountedTwice(t *testing.T) {
	ns := namespace(t)
	r := start(t, ns, Config{Window: 200 * time.Millisecond, RetryAfter: 100 * time.Millisecond})
	var lost atomic.Bool
	r.writes.after = func(err error) error {
		if err == nil && lost.CompareAndSwap(false, true) {
			return context.DeadlineExceeded
		}
		return err
	}
	r.obs <- observation(1, 1, 4, 4, time.Now().Truncate(time.Second))
	eventually(t, ns, 1, countIs(4))
	// Let the batch that went back be tried again: it is recognised as written.
	time.Sleep(time.Second)
	if inc := get(t, ns); inc.Status.Count != 4 {
		t.Fatalf("count %d after a lost answer, want 4", inc.Status.Count)
	}
	if !lost.Load() || r.writes.updates.Load() != 1 {
		t.Fatalf("lost %v, %d status writes: want the retry to find the batch applied without writing", lost.Load(), r.writes.updates.Load())
	}
	r.stop()
	if r.pending[fp(1)] != nil || r.failed[fp(1)] != nil {
		t.Fatal("the batch is still pending after the retry")
	}
}

func TestRestartNeitherResetsNorDoubles(t *testing.T) {
	ns := namespace(t)
	began := time.Now().Truncate(time.Second)
	a := start(t, ns, Config{Window: time.Hour})
	a.obs <- observation(1, 1, 5, 5, began)
	eventually(t, ns, 1, countIs(5))
	a.obs <- observation(1, 2, 3, 3, began)
	// The second batch is pending (the window is an hour); a clean stop writes it.
	a.stop()
	eventually(t, ns, 1, countIs(8))

	b := start(t, ns, Config{Window: 200 * time.Millisecond})
	if got, found, err := b.Handled(t.Context(), eventID(1), began); err != nil || !found || got != 5 {
		t.Fatalf("Handled after restart = %d, %v, %v; want 5, true", got, found, err)
	}
	if _, found, _ := b.Handled(t.Context(), eventID(9), began.Add(time.Hour)); found {
		t.Fatal("an occurrence never counted must not be found")
	}
	repeat := observation(1, 1, 5, 0, began)
	repeat.MayRepeat = true
	b.obs <- repeat // nothing new
	grown := observation(1, 1, 9, 0, began)
	grown.MayRepeat = true
	b.obs <- grown // four more, written at once
	eventually(t, ns, 1, countIs(12))
	// In the next batch, with one new occurrence: a collector that lost its state reports occurrence 2 as
	// new, but it is recorded with that total, so it adds nothing.
	b.obs <- observation(1, 2, 3, 3, began)
	b.obs <- observation(1, 3, 1, 1, began)
	eventually(t, ns, 1, countIs(13))
	time.Sleep(500 * time.Millisecond)
	if inc := get(t, ns); inc.Status.Count != 13 {
		t.Fatalf("count %d, want 13", inc.Status.Count)
	}
}

func TestHandledWaitsForTheIncidents(t *testing.T) {
	c, err := New(Config{Namespace: "x"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if _, _, err := c.Handled(ctx, eventID(1), t0); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Handled before the load = %v, want a deadline error so the collector asks again", err)
	}
}

func TestCapRefusesNewFingerprints(t *testing.T) {
	ns := namespace(t)
	rec := corerecord.NewFakeRecorder(10)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "analyzer-0"}}
	r := start(t, ns, Config{MaxIncidents: 2, Recorder: rec, Regarding: pod})
	before := testutil.ToFloat64(refusedObservations)
	now := time.Now().Truncate(time.Second)
	r.obs <- observation(1, 1, 1, 1, now)
	r.obs <- observation(2, 2, 1, 1, now)
	r.obs <- observation(3, 3, 1, 1, now)
	r.obs <- observation(3, 4, 1, 1, now)
	r.obs <- observation(1, 5, 1, 1, now)
	r.obs <- observation(4, 6, 1, 1, now) // another new fingerprint, within the event interval
	eventually(t, ns, 1, countIs(2))
	eventually(t, ns, 2, countIs(1))
	err := env.admin.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: Name(fp(3))}, &remediationv1alpha1.Incident{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("the third fingerprint was stored (%v)", err)
	}
	if got := testutil.ToFloat64(refusedObservations) - before; got != 3 {
		t.Fatalf("refused metric grew by %v, want 3", got)
	}
	select {
	case e := <-rec.Events:
		if !strings.Contains(e, "Warning IncidentCapReached") || !strings.Contains(e, fp(3)) {
			t.Fatalf("event %q", e)
		}
	default:
		t.Fatal("no Warning event")
	}
	select {
	case e := <-rec.Events:
		t.Fatalf("a second event within the interval: %q", e)
	default:
	}
}

func TestCapEventRepeatsAfterTheInterval(t *testing.T) {
	ns := namespace(t)
	rec := corerecord.NewFakeRecorder(10)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "analyzer-0"}}
	r := start(t, ns, Config{MaxIncidents: 1, Recorder: rec, Regarding: pod, EventInterval: 200 * time.Millisecond})
	now := time.Now().Truncate(time.Second)
	r.obs <- observation(1, 1, 1, 1, now)
	r.obs <- observation(2, 2, 1, 1, now)
	time.Sleep(300 * time.Millisecond)
	r.obs <- observation(3, 3, 1, 1, now)
	eventually(t, ns, 1, countIs(1))
	for _, f := range []int{2, 3} {
		select {
		case e := <-rec.Events:
			if !strings.Contains(e, fp(f)) {
				t.Fatalf("event %q, want one for %s", e, fp(f))
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("no Warning event for %s", fp(f))
		}
	}
}

func TestCapCountsExistingIncidentsAfterRestart(t *testing.T) {
	ns := namespace(t)
	a := start(t, ns, Config{MaxIncidents: 1})
	a.obs <- observation(1, 1, 1, 1, time.Now().Truncate(time.Second))
	eventually(t, ns, 1, countIs(1))
	a.stop()
	b := start(t, ns, Config{MaxIncidents: 1})
	b.obs <- observation(2, 2, 1, 1, time.Now().Truncate(time.Second))
	b.obs <- observation(1, 3, 1, 1, time.Now().Truncate(time.Second))
	eventually(t, ns, 1, countIs(2))
	err := env.admin.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: Name(fp(2))}, &remediationv1alpha1.Incident{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("a new fingerprint was stored over the cap after a restart (%v)", err)
	}
}

func TestCountSaturates(t *testing.T) {
	ns := namespace(t)
	r := start(t, ns, Config{Window: 100 * time.Millisecond})
	now := time.Now().Truncate(time.Second)
	// Forged series updates: each adds at most collector.MaxCount.
	for i := range 1002 {
		r.obs <- observation(1, 1, int64(i+1)*collector.MaxCount, collector.MaxCount, now)
	}
	eventually(t, ns, 1, countIs(remediationv1alpha1.MaxIncidentCount))
}

// The Incident holds the subject, reason and normalized symptom, and counters: no other text.
func TestIncidentHoldsOnlyTheSymptom(t *testing.T) {
	ns := namespace(t)
	r := start(t, ns, Config{})
	o := observation(1, 1, 1, 1, time.Now().Truncate(time.Second))
	r.obs <- o
	inc := eventually(t, ns, 1, countIs(1))
	if inc.Spec != specOf(o) {
		t.Fatalf("spec %+v, want %+v", inc.Spec, specOf(o))
	}
	raw, err := json.Marshal(inc.Status)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	// conditions hold AnalysisDue, whose reason and message are fixed strings.
	allowed := []string{"phase", "firstSeen", "lastSeen", "count", "occurrences", "conditions"}
	for k := range fields {
		if !slices.Contains(allowed, k) {
			t.Errorf("unexpected status field %q", k)
		}
	}
	if want := "incident-" + strings.Repeat("0", 63) + "1"; inc.Name != want {
		t.Fatalf("name %s, want %s", inc.Name, want)
	}
}

// An Incident the API server refuses as invalid is not tried again and does not hold a place under the cap.
func TestRefusedIncidentIsDroppedAndFreesItsPlace(t *testing.T) {
	ns := namespace(t)
	r := start(t, ns, Config{MaxIncidents: 1, Window: 100 * time.Millisecond, RetryAfter: 50 * time.Millisecond})
	now := time.Now().Truncate(time.Second)
	bad := observation(1, 1, 1, 1, now)
	bad.Symptom = strings.Repeat("x", 1025) // the schema allows 1024 characters
	r.obs <- bad
	time.Sleep(time.Second)
	if n := r.writes.creates.Load(); n != 1 {
		t.Fatalf("%d creates of a refused Incident, want 1", n)
	}
	if total, found, err := r.Handled(t.Context(), bad.Occurrence, now); err != nil || found {
		t.Fatalf("Handled = %d, %v, %v for an occurrence never stored: want not found", total, found, err)
	}
	r.obs <- observation(2, 2, 1, 1, now)
	eventually(t, ns, 2, countIs(1))
}

// loseAnswer makes the n-th successful status update (counted from 1) look failed, as a timeout after the API
// server stored it does, and closes lost then.
func loseAnswer(r *running, n int32) (lost chan struct{}) {
	lost = make(chan struct{})
	var seen atomic.Int32
	r.writes.after = func(err error) error {
		if err == nil && seen.Add(1) == n {
			close(lost)
			return context.DeadlineExceeded
		}
		return err
	}
	return lost
}

// settled waits until the Incident of fp(1) has count n, then a while longer, and fails if it changed.
func settled(t *testing.T, ns string, n int64, wait time.Duration) {
	t.Helper()
	eventually(t, ns, 1, countIs(n))
	time.Sleep(wait)
	if inc := get(t, ns); inc.Status.Count != n {
		t.Fatalf("count %d, want %d", inc.Status.Count, n)
	}
}

func waitFor(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(20 * time.Second):
		t.Fatal("timed out")
	}
}

// A lost answer for a batch of more occurrences than an Incident records: the ones the cut dropped began no
// later than the drop mark, so they do not hide that the batch was stored.
func TestLostAnswerWithMoreOccurrencesThanRecordsIsNotCountedTwice(t *testing.T) {
	ns := namespace(t)
	r := start(t, ns, Config{Window: 300 * time.Millisecond, RetryAfter: 100 * time.Millisecond})
	lost := loseAnswer(r, 2)
	now := time.Now().Truncate(time.Second)
	r.obs <- observation(1, 0, 1, 1, now.Add(-time.Hour))
	eventually(t, ns, 1, countIs(1))
	n := remediationv1alpha1.MaxOccurrenceRecords + 1
	for i := 1; i <= n; i++ {
		r.obs <- observation(1, i, 1, 1, now.Add(time.Duration(i)*time.Second))
	}
	waitFor(t, lost)
	settled(t, ns, int64(n+1), time.Second)
}

// A lost answer for a batch holding an ID the schema refuses: that occurrence is counted but not recorded,
// so it neither proves nor blocks.
func TestLostAnswerWithAnInvalidIDIsNotCountedTwice(t *testing.T) {
	ns := namespace(t)
	r := start(t, ns, Config{Window: 300 * time.Millisecond, RetryAfter: 100 * time.Millisecond})
	lost := loseAnswer(r, 2)
	now := time.Now().Truncate(time.Second)
	r.obs <- observation(1, 1, 1, 1, now)
	eventually(t, ns, 1, countIs(1))
	bad := observation(1, 2, 2, 2, now)
	bad.Occurrence = invalidID
	r.obs <- bad
	r.obs <- observation(1, 3, 3, 3, now)
	waitFor(t, lost)
	settled(t, ns, 6, time.Second)
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.records[bad.Occurrence]; ok {
		t.Fatal("an ID the schema refuses is remembered, though no Incident can ever drop it")
	}
}

// Observations arriving while a batch whose answer was lost waits for its retry stay apart from it, so the
// retry still sees that the batch was stored.
func TestLostAnswerWithNewObservationsDuringTheRetryWait(t *testing.T) {
	ns := namespace(t)
	r := start(t, ns, Config{Window: 200 * time.Millisecond, RetryAfter: 500 * time.Millisecond})
	lost := loseAnswer(r, 2)
	now := time.Now().Truncate(time.Second)
	r.obs <- observation(1, 1, 1, 1, now)
	eventually(t, ns, 1, countIs(1))
	r.obs <- observation(1, 2, 4, 4, now)
	waitFor(t, lost)
	r.obs <- observation(1, 2, 6, 2, now) // two more of the occurrence whose write was lost
	r.obs <- observation(1, 3, 1, 1, now)
	settled(t, ns, 8, time.Second)
}

// A status update the API server refuses as invalid drops its batch, and the memory goes back to what the
// Incident stores: the dropped increase reads as not counted.
func TestRefusedStatusUpdateRollsTheMemoryBack(t *testing.T) {
	ns := namespace(t)
	r := start(t, ns, Config{Window: 100 * time.Millisecond, RetryAfter: 50 * time.Millisecond})
	now := time.Now().Truncate(time.Second)
	r.obs <- observation(1, 1, 1, 1, now)
	eventually(t, ns, 1, countIs(1))
	refused := make(chan struct{})
	var once sync.Once
	r.writes.instead = func(context.Context) error {
		var err error
		once.Do(func() {
			err = apierrors.NewInvalid(remediationv1alpha1.GroupVersion.WithKind("Incident").GroupKind(), "x", nil)
			close(refused)
		})
		return err
	}
	r.obs <- observation(1, 1, 3, 2, now)
	waitFor(t, refused)
	time.Sleep(200 * time.Millisecond)
	if got, found, err := r.Handled(t.Context(), eventID(1), now); err != nil || !found || got != 1 {
		t.Fatalf("Handled = %d, %v, %v after the refused update; want the stored 1", got, found, err)
	}
	again := observation(1, 1, 3, 0, now)
	again.MayRepeat = true
	r.obs <- again
	settled(t, ns, 3, 300*time.Millisecond)
}

// An occurrence without a begin time cannot be placed against the drop mark: it is never answered as
// counted in full only because some record was dropped.
func TestUnknownBeginIsNotAnsweredByTheDropMark(t *testing.T) {
	c, err := New(Config{Namespace: "x"})
	if err != nil {
		t.Fatal(err)
	}
	c.droppedThrough = t0
	if got, found := c.handled(eventID(1), time.Time{}); found || got != 0 {
		t.Fatalf("handled = %d, %v for an unknown begin time, want not found", got, found)
	}
	if got, found := c.handled(eventID(1), t0.Add(-time.Second)); !found || got != collector.AllHandled {
		t.Fatalf("handled = %d, %v for an occurrence before the mark, want AllHandled", got, found)
	}
}

// A slow status write does not stop the consumption of observations.
func TestSlowWriteDoesNotBlockObservations(t *testing.T) {
	ns := namespace(t)
	r := start(t, ns, Config{})
	release := make(chan struct{})
	var once sync.Once
	r.writes.before = func(context.Context, *remediationv1alpha1.Incident) {
		once.Do(func() { <-release })
	}
	now := time.Now().Truncate(time.Second)
	r.obs <- observation(1, 1, 1, 1, now)
	for i := 2; i < 6; i++ {
		select {
		case r.obs <- observation(2, i, 1, 1, now):
		case <-time.After(2 * time.Second):
			close(release)
			t.Fatal("an observation waited for a status write")
		}
	}
	close(release)
	eventually(t, ns, 1, countIs(1))
	eventually(t, ns, 2, countIs(4))
}

// The last writes on stop try a failed write again, within StopTimeout.
func TestStopRetriesAFailedWrite(t *testing.T) {
	ns := namespace(t)
	r := start(t, ns, Config{Window: time.Hour, RetryAfter: 50 * time.Millisecond, StopTimeout: 3 * time.Second})
	now := time.Now().Truncate(time.Second)
	r.obs <- observation(1, 1, 1, 1, now)
	eventually(t, ns, 1, countIs(1))
	var once sync.Once
	r.writes.instead = func(context.Context) error {
		var err error
		once.Do(func() {
			err = apierrors.NewServerTimeout(remediationv1alpha1.GroupVersion.WithResource("incidents").GroupResource(), "update", 1)
		})
		return err
	}
	r.obs <- observation(1, 2, 2, 2, now)
	r.stop()
	if inc := get(t, ns); inc.Status.Count != 3 {
		t.Fatalf("count %d after stop, want 3", inc.Status.Count)
	}
}

// The last writes on stop end at StopTimeout, and what they could not write is counted as lost.
func TestStopIsBounded(t *testing.T) {
	ns := namespace(t)
	r := start(t, ns, Config{Window: time.Hour, RetryAfter: 50 * time.Millisecond, StopTimeout: 300 * time.Millisecond})
	now := time.Now().Truncate(time.Second)
	r.obs <- observation(1, 1, 1, 1, now)
	eventually(t, ns, 1, countIs(1))
	// The write hangs until its deadline, or until the test gives up on a stop that has none.
	giveUp := make(chan struct{})
	r.writes.instead = func(ctx context.Context) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-giveUp:
			return errors.New("given up")
		}
	}
	r.obs <- observation(1, 2, 2, 2, now)
	before := testutil.ToFloat64(droppedBatches.WithLabelValues(dropStopped))
	started := time.Now()
	watchdog := time.AfterFunc(3*time.Second, func() { close(giveUp) })
	defer watchdog.Stop()
	r.stop()
	if d := time.Since(started); d > 2*time.Second {
		t.Fatalf("stop took %v with a StopTimeout of 300ms", d)
	}
	if got := testutil.ToFloat64(droppedBatches.WithLabelValues(dropStopped)) - before; got != 1 {
		t.Fatalf("stopped metric grew by %v, want 1", got)
	}
}

// Due times of Incidents with nothing to write are pruned, so they do not pile up.
func TestNextWriteIsPruned(t *testing.T) {
	ns := namespace(t)
	r := start(t, ns, Config{Window: 100 * time.Millisecond})
	now := time.Now().Truncate(time.Second)
	for f := 1; f <= 3; f++ {
		r.obs <- observation(f, f, 1, 1, now)
		eventually(t, ns, f, countIs(1))
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		r.mu.Lock()
		n := len(r.nextWrite)
		r.mu.Unlock()
		if n == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d due times kept with nothing to write", n)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// An Incident of the expected name that holds another fingerprint is not written, and the occurrences of the
// refused batch are forgotten, so they do not read as counted.
func TestIncidentWithAnotherFingerprintIsRefused(t *testing.T) {
	ns := namespace(t)
	squat := &remediationv1alpha1.Incident{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: Name(fp(1))},
		Spec:       specOf(observation(2, 1, 1, 1, time.Now())),
	}
	if err := env.admin.Create(t.Context(), squat); err != nil {
		t.Fatal(err)
	}
	r := start(t, ns, Config{Window: 100 * time.Millisecond})
	before := testutil.ToFloat64(droppedBatches.WithLabelValues(dropFingerprintConflict))
	now := time.Now().Truncate(time.Second)
	r.obs <- observation(1, 1, 1, 1, now)
	deadline := time.Now().Add(5 * time.Second)
	for testutil.ToFloat64(droppedBatches.WithLabelValues(dropFingerprintConflict))-before < 1 {
		if time.Now().After(deadline) {
			t.Fatal("the batch was not dropped")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got, found, err := r.Handled(t.Context(), eventID(1), now); err != nil || found {
		t.Fatalf("Handled = %d, %v, %v for a refused occurrence, want not found", got, found, err)
	}
	if r.writes.updates.Load() != 0 {
		t.Fatal("the Incident of another fingerprint was written")
	}
}
