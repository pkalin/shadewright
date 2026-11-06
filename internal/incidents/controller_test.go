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

// observation returns an observation of fingerprint fp(f), occurrence eventID(i). Like an event in
// production it is not Immediate: a fingerprint without an Incident opens one when total is 2 or more, or
// when it is seen again 2 to 15 min after its first sighting (sightings_test.go); immediate returns one that
// opens at once.
func observation(f, i int, total, count int64, began time.Time) collector.Observation {
	return collector.Observation{
		Fingerprint: fp(f),
		Subject: remediationv1alpha1.ObjectRef{
			APIVersion: "apps/v1", Kind: "Deployment", Namespace: "demo", Name: "orders",
			UID: "33333333-3333-4333-8333-333333333333",
		},
		Source: "demo",
		Reason: "BackOff", Type: corev1.EventTypeWarning, ReportingController: "kubelet",
		Symptom:    "Back-off restarting failed container worker",
		Occurrence: eventID(i), Total: total, Count: count, Began: began, ObservedAt: began,
	}
}

// immediate returns o marked Immediate, as the collector marks an OOMKilled termination, so that it opens
// its Incident at its first sighting.
func immediate(o collector.Observation) collector.Observation {
	o.Immediate = true
	return o
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
		r.obs <- immediate(observation(1, i, 1, 1, now.Add(time.Duration(i)*time.Second)))
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
	r.obs <- immediate(observation(1, 1, 2, 2, now))
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

// storedWrites makes the first status update that the API server stores answer as lost (a deadline), and
// counts the updates it stores. It counts those, not every update sent (#438): the analysis trigger writes
// the AnalysisDue condition on its own schedule, and when that lands between the read and the update of an
// observation write, the update meets a conflict, stores nothing and is sent again on a fresh read.
func storedWrites(r *running, lost *atomic.Bool) *atomic.Int32 {
	var stored atomic.Int32
	r.writes.after = func(err error) error {
		if err != nil {
			return err
		}
		stored.Add(1)
		if lost.CompareAndSwap(false, true) {
			return context.DeadlineExceeded
		}
		return nil
	}
	return &stored
}

func TestLostAnswerIsNotCountedTwice(t *testing.T) {
	for name, conflict := range map[string]bool{"lost answer": false, "lost answer after a conflict": true} {
		t.Run(name, func(t *testing.T) {
			ns := namespace(t)
			r := start(t, ns, Config{Window: 200 * time.Millisecond, RetryAfter: 100 * time.Millisecond})
			var lost atomic.Bool
			stored := storedWrites(r, &lost)
			// otherErr is what the other writer met; it runs on the controller's goroutine, so the test
			// goroutine checks it.
			otherErr := make(chan error, 1)
			if conflict {
				// Another write (the analysis trigger's, in #438) lands between the read and the first update:
				// that update conflicts and is sent again on a fresh read.
				var once sync.Once
				r.writes.before = func(ctx context.Context, cur *remediationv1alpha1.Incident) {
					once.Do(func() {
						inc := &remediationv1alpha1.Incident{}
						err := env.admin.Get(ctx, client.ObjectKeyFromObject(cur), inc)
						if err == nil {
							meta.SetStatusCondition(&inc.Status.Conditions, metav1.Condition{
								Type: "Reviewed", Status: metav1.ConditionTrue, Reason: "Test", Message: "set by another writer",
							})
							err = env.admin.Status().Update(ctx, inc)
						}
						otherErr <- err
					})
				}
			} else {
				otherErr <- nil
			}
			r.obs <- immediate(observation(1, 1, 4, 4, time.Now().Truncate(time.Second)))
			eventually(t, ns, 1, countIs(4))
			select {
			case err := <-otherErr:
				if err != nil {
					t.Fatalf("writing as another writer: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the other writer never ran")
			}
			// Let the batch that went back be tried again: it is recognised as written.
			time.Sleep(time.Second)
			if inc := get(t, ns); inc.Status.Count != 4 {
				t.Fatalf("count %d after a lost answer, want 4", inc.Status.Count)
			}
			if !lost.Load() || stored.Load() != 1 {
				t.Fatalf("lost %v, %d status writes stored: want the retry to find the batch applied without writing",
					lost.Load(), stored.Load())
			}
			if sent := r.writes.updates.Load(); conflict && sent < 2 {
				t.Fatalf("%d status updates sent: the conflict did not happen", sent)
			}
			r.stop()
			if r.pending[fp(1)] != nil || r.failed[fp(1)] != nil {
				t.Fatal("the batch is still pending after the retry")
			}
		})
	}
}

func TestRestartNeitherResetsNorDoubles(t *testing.T) {
	ns := namespace(t)
	began := time.Now().Truncate(time.Second)
	a := start(t, ns, Config{Window: time.Hour})
	a.obs <- immediate(observation(1, 1, 5, 5, began))
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

// The analysis bounds its claim with the trigger's analysis timeout, configured or the default.
func TestAnalysisTimeoutIsTheTriggers(t *testing.T) {
	for want, cfg := range map[time.Duration]Config{
		DefaultAnalysisTimeout: {Namespace: "x"},
		12 * time.Minute:       {Namespace: "x", AnalysisTimeout: 12 * time.Minute},
	} {
		c, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if got := c.AnalysisTimeout(); got != want {
			t.Errorf("AnalysisTimeout() = %s, want %s", got, want)
		}
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
	r.obs <- immediate(observation(1, 1, 1, 1, now))
	r.obs <- immediate(observation(2, 2, 1, 1, now))
	r.obs <- immediate(observation(3, 3, 1, 1, now))
	r.obs <- immediate(observation(3, 4, 1, 1, now))
	r.obs <- immediate(observation(1, 5, 1, 1, now))
	r.obs <- immediate(observation(4, 6, 1, 1, now)) // another new fingerprint, within the event interval
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
	r.obs <- immediate(observation(1, 1, 1, 1, now))
	r.obs <- immediate(observation(2, 2, 1, 1, now))
	time.Sleep(300 * time.Millisecond)
	r.obs <- immediate(observation(3, 3, 1, 1, now))
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
	a.obs <- immediate(observation(1, 1, 1, 1, time.Now().Truncate(time.Second)))
	eventually(t, ns, 1, countIs(1))
	a.stop()
	b := start(t, ns, Config{MaxIncidents: 1})
	b.obs <- immediate(observation(2, 2, 1, 1, time.Now().Truncate(time.Second)))
	b.obs <- immediate(observation(1, 3, 1, 1, time.Now().Truncate(time.Second)))
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
		r.obs <- immediate(observation(1, 1, int64(i+1)*collector.MaxCount, collector.MaxCount, now))
	}
	eventually(t, ns, 1, countIs(remediationv1alpha1.MaxIncidentCount))
}

// The Incident holds the subject, reason and normalized symptom, and counters: no other text.
func TestIncidentHoldsOnlyTheSymptom(t *testing.T) {
	ns := namespace(t)
	r := start(t, ns, Config{})
	o := immediate(observation(1, 1, 1, 1, time.Now().Truncate(time.Second)))
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

// TestModuleSymptomMakesOneIncident checks the end of #380's first criterion: an observation of a module
// symptom source (a KEDA HPA at its ceiling, about its ScaledObject) creates one Incident about the
// ScaledObject, and the schema keeps its "condition:" occurrence.
func TestModuleSymptomMakesOneIncident(t *testing.T) {
	ns := namespace(t)
	r := start(t, ns, Config{})
	began := time.Now().Add(-time.Minute).Truncate(time.Second)
	o := immediate(observation(1, 0, 1, 1, began))
	o.Subject = remediationv1alpha1.ObjectRef{
		APIVersion: "keda.sh/v1alpha1", Kind: "ScaledObject", Namespace: o.Subject.Namespace, Name: "orders",
		UID: "8d2e4f60-1a3b-4c5d-9e7f-a0b1c2d3e4f5",
	}
	o.Reason, o.ReportingController = "ReplicaCeiling", "horizontal-pod-autoscaler"
	o.Symptom = "The HPA that KEDA manages for this ScaledObject runs at its maximum replica count while its metrics " +
		"ask for more replicas. Evidence: spec.maxReplicas=20; status.conditions.ScalingLimited.reason=TooManyReplicas"
	o.Occurrence = "condition:" + strings.Repeat("ab", 16)
	r.obs <- o
	inc := eventually(t, ns, 1, countIs(1))
	if inc.Spec != specOf(o) {
		t.Fatalf("spec %+v, want %+v", inc.Spec, specOf(o))
	}
	if len(inc.Status.Occurrences) != 1 || inc.Status.Occurrences[0].ID != o.Occurrence {
		t.Fatalf("occurrences %+v, want the condition occurrence recorded", inc.Status.Occurrences)
	}
	var list remediationv1alpha1.IncidentList
	if err := env.admin.List(t.Context(), &list, client.InNamespace(ns)); err != nil || len(list.Items) != 1 {
		t.Fatalf("%d Incidents (%v), want one", len(list.Items), err)
	}
}

// An Incident the API server refuses as invalid is not tried again and does not hold a place under the cap.
func TestRefusedIncidentIsDroppedAndFreesItsPlace(t *testing.T) {
	ns := namespace(t)
	r := start(t, ns, Config{MaxIncidents: 1, Window: 100 * time.Millisecond, RetryAfter: 50 * time.Millisecond})
	now := time.Now().Truncate(time.Second)
	bad := immediate(observation(1, 1, 1, 1, now))
	bad.Symptom = strings.Repeat("x", 1025) // the schema allows 1024 characters
	r.obs <- bad
	time.Sleep(time.Second)
	if n := r.writes.creates.Load(); n != 1 {
		t.Fatalf("%d creates of a refused Incident, want 1", n)
	}
	if total, found, err := r.Handled(t.Context(), bad.Occurrence, now); err != nil || found {
		t.Fatalf("Handled = %d, %v, %v for an occurrence never stored: want not found", total, found, err)
	}
	r.obs <- immediate(observation(2, 2, 1, 1, now))
	eventually(t, ns, 2, countIs(1))
}

// loseAnswer makes the second successful status update look failed, as a timeout after the API server
// stored it does, and closes lost then. The first one creates the Incident's status, so the lost write is
// one that adds to it.
func loseAnswer(r *running) (lost chan struct{}) {
	lost = make(chan struct{})
	var seen atomic.Int32
	r.writes.after = func(err error) error {
		if err == nil && seen.Add(1) == 2 {
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
	lost := loseAnswer(r)
	now := time.Now().Truncate(time.Second)
	r.obs <- immediate(observation(1, 0, 1, 1, now.Add(-time.Hour)))
	eventually(t, ns, 1, countIs(1))
	n := remediationv1alpha1.MaxOccurrenceRecords + 1
	for i := 1; i <= n; i++ {
		r.obs <- immediate(observation(1, i, 1, 1, now.Add(time.Duration(i)*time.Second)))
	}
	waitFor(t, lost)
	inc := eventually(t, ns, 1, countIs(int64(n+1)))
	settled(t, ns, int64(n+1), time.Second)
	onlyStored(t, r, inc)
}

// onlyStored fails if the controller remembers a record of inc's Incident that its status does not hold:
// the records the write whose answer was lost dropped are forgotten once the retry finds it stored.
func onlyStored(t *testing.T, r *running, inc *remediationv1alpha1.Incident) {
	t.Helper()
	remembered(t, r, inc)
	stored := map[string]bool{}
	for _, o := range inc.Status.Occurrences {
		stored[o.ID] = true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, rec := range r.records {
		if rec.incident == inc.Name && !stored[id] {
			t.Errorf("the controller remembers %s, which the Incident no longer records", id)
		}
	}
	indexed(t, r.Controller)
}

// indexed fails unless recordsOf indexes exactly the records, each under its Incident. The caller holds mu.
func indexed(t *testing.T, c *Controller) {
	t.Helper()
	n := 0
	for name, ids := range c.recordsOf {
		for id := range ids {
			n++
			if r, ok := c.records[id]; !ok || r.incident != name {
				t.Errorf("recordsOf holds %s under %s, records %+v", id, name, r)
			}
		}
	}
	if n != len(c.records) {
		t.Errorf("recordsOf indexes %d records, records holds %d", n, len(c.records))
	}
}

// A write whose answer was lost drops records with the 32-record cut, and the retry, which finds that write
// stored, is refused as invalid: the dropped records are forgotten all the same.
func TestRefusedRetryAfterALostAnswerForgetsDroppedRecords(t *testing.T) {
	ns := namespace(t)
	r := start(t, ns, Config{Window: 300 * time.Millisecond, RetryAfter: 500 * time.Millisecond})
	lost := loseAnswer(r)
	refused := make(chan struct{})
	var updates atomic.Int32
	r.writes.instead = func(context.Context) error {
		if updates.Add(1) == 3 {
			close(refused)
			return apierrors.NewInvalid(remediationv1alpha1.GroupVersion.WithKind("Incident").GroupKind(), "x", nil)
		}
		return nil
	}
	now := time.Now().Truncate(time.Second)
	r.obs <- immediate(observation(1, 0, 1, 1, now.Add(-time.Hour)))
	eventually(t, ns, 1, countIs(1))
	n := remediationv1alpha1.MaxOccurrenceRecords + 1
	for i := 1; i <= n; i++ {
		r.obs <- immediate(observation(1, i, 1, 1, now.Add(time.Duration(i)*time.Second)))
	}
	waitFor(t, lost)
	// A new occurrence, so the retry has something to send besides the batch it finds stored.
	r.obs <- immediate(observation(1, n+1, 1, 1, now.Add(time.Duration(n+1)*time.Second)))
	waitFor(t, refused)
	deadline := time.Now().Add(20 * time.Second)
	for {
		r.mu.Lock()
		done := len(r.held) == 0
		r.mu.Unlock()
		if done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the refused write never returned")
		}
		time.Sleep(10 * time.Millisecond)
	}
	inc := get(t, ns)
	if inc.Status.Count != int64(n+1) {
		t.Fatalf("count %d, want %d", inc.Status.Count, n+1)
	}
	stored := map[string]bool{}
	for _, o := range inc.Status.Occurrences {
		stored[o.ID] = true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, rec := range r.records {
		if rec.incident == inc.Name && !stored[id] {
			t.Errorf("the controller remembers %s, which the Incident no longer records", id)
		}
	}
	indexed(t, r.Controller)
}

// A lost answer for a batch holding an ID the schema refuses: that occurrence is counted but not recorded,
// so it neither proves nor blocks.
func TestLostAnswerWithAnInvalidIDIsNotCountedTwice(t *testing.T) {
	ns := namespace(t)
	r := start(t, ns, Config{Window: 300 * time.Millisecond, RetryAfter: 100 * time.Millisecond})
	lost := loseAnswer(r)
	now := time.Now().Truncate(time.Second)
	r.obs <- immediate(observation(1, 1, 1, 1, now))
	eventually(t, ns, 1, countIs(1))
	bad := immediate(observation(1, 2, 2, 2, now))
	bad.Occurrence = invalidID
	r.obs <- bad
	r.obs <- immediate(observation(1, 3, 3, 3, now))
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
	lost := loseAnswer(r)
	now := time.Now().Truncate(time.Second)
	r.obs <- immediate(observation(1, 1, 1, 1, now))
	eventually(t, ns, 1, countIs(1))
	r.obs <- immediate(observation(1, 2, 4, 4, now))
	waitFor(t, lost)
	r.obs <- immediate(observation(1, 2, 6, 2, now)) // two more of the occurrence whose write was lost
	r.obs <- immediate(observation(1, 3, 1, 1, now))
	settled(t, ns, 8, time.Second)
}

// A status update the API server refuses as invalid drops its batch, and the memory goes back to what the
// Incident stores: the dropped increase reads as not counted.
func TestRefusedStatusUpdateRollsTheMemoryBack(t *testing.T) {
	ns := namespace(t)
	r := start(t, ns, Config{Window: 100 * time.Millisecond, RetryAfter: 50 * time.Millisecond})
	now := time.Now().Truncate(time.Second)
	r.obs <- immediate(observation(1, 1, 1, 1, now))
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
	r.obs <- immediate(observation(1, 1, 3, 2, now))
	waitFor(t, refused)
	time.Sleep(200 * time.Millisecond)
	if got, found, err := r.Handled(t.Context(), eventID(1), now); err != nil || !found || got != 1 {
		t.Fatalf("Handled = %d, %v, %v after the refused update; want the stored 1", got, found, err)
	}
	again := immediate(observation(1, 1, 3, 0, now))
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
	r.obs <- immediate(observation(1, 1, 1, 1, now))
	for i := 2; i < 6; i++ {
		select {
		case r.obs <- immediate(observation(2, i, 1, 1, now)):
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
	r.obs <- immediate(observation(1, 1, 1, 1, now))
	eventually(t, ns, 1, countIs(1))
	var once sync.Once
	r.writes.instead = func(context.Context) error {
		var err error
		once.Do(func() {
			err = apierrors.NewServerTimeout(remediationv1alpha1.GroupVersion.WithResource("incidents").GroupResource(), "update", 1)
		})
		return err
	}
	r.obs <- immediate(observation(1, 2, 2, 2, now))
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
	r.obs <- immediate(observation(1, 1, 1, 1, now))
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
	r.obs <- immediate(observation(1, 2, 2, 2, now))
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
		r.obs <- immediate(observation(f, f, 1, 1, now))
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

// The Incident of another fingerprint is read fresh when the write is refused, but its status is not ours:
// the records load remembered that it still holds stay, and nothing it gained since (a record, a later drop
// mark) is taken into memory.
func TestForeignStatusIsNotRemembered(t *testing.T) {
	ns := namespace(t)
	now := time.Now().Truncate(time.Second)
	at := func(d time.Duration) *metav1.Time { return &metav1.Time{Time: now.Add(-d)} }
	squat := &remediationv1alpha1.Incident{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: Name(fp(1))},
		Spec:       specOf(observation(2, 1, 1, 1, now)),
	}
	if err := env.admin.Create(t.Context(), squat); err != nil {
		t.Fatal(err)
	}
	squat.Status = remediationv1alpha1.IncidentStatus{
		Phase: remediationv1alpha1.IncidentOpen, FirstSeen: at(4 * time.Hour), LastSeen: at(4 * time.Hour), Count: 5,
		Occurrences:               []remediationv1alpha1.OccurrenceRecord{{ID: eventID(50), Total: 5, Began: at(2 * time.Hour)}},
		OccurrencesDroppedThrough: at(3 * time.Hour),
	}
	if err := env.admin.Status().Update(t.Context(), squat); err != nil {
		t.Fatal(err)
	}
	r := start(t, ns, Config{Window: 100 * time.Millisecond})
	if _, _, err := r.Handled(t.Context(), eventID(50), now); err != nil {
		t.Fatal(err)
	}
	// After load, the other Incident gains a record and a later drop mark.
	squat.Status.Count = 6
	squat.Status.Occurrences = append(squat.Status.Occurrences,
		remediationv1alpha1.OccurrenceRecord{ID: eventID(51), Total: 1, Began: at(time.Minute)})
	squat.Status.OccurrencesDroppedThrough = at(time.Hour)
	if err := env.admin.Status().Update(t.Context(), squat); err != nil {
		t.Fatal(err)
	}
	before := testutil.ToFloat64(droppedBatches.WithLabelValues(dropFingerprintConflict))
	r.obs <- observation(1, 1, 1, 1, now)
	deadline := time.Now().Add(5 * time.Second)
	for testutil.ToFloat64(droppedBatches.WithLabelValues(dropFingerprintConflict))-before < 1 {
		if time.Now().After(deadline) {
			t.Fatal("the batch was not dropped")
		}
		time.Sleep(50 * time.Millisecond)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if want := now.Add(-3 * time.Hour); !r.droppedThrough.Equal(want) {
		t.Errorf("droppedThrough %v, want %v as load remembered it", r.droppedThrough, want)
	}
	if got, found := r.handled(eventID(50), now.Add(-2*time.Hour)); !found || got != 5 {
		t.Errorf("handled = %d, %v for the record load remembered, want 5", got, found)
	}
	if got, found := r.handled(eventID(51), now.Add(-time.Minute)); found {
		t.Errorf("handled = %d for a record the other Incident gained after load, want not found", got)
	}
	if got, found := r.handled(eventID(52), now.Add(-2*time.Hour)); found {
		t.Errorf("handled = %d for an occurrence before the later drop mark, want not found", got)
	}
	indexed(t, r.Controller)
}

// A batch being written still counts as held: the new occurrences arriving while it is in flight cannot take
// the Incident over its cap.
func TestInFlightOccurrencesCountAsHeld(t *testing.T) {
	ns := namespace(t)
	const perIncident = 10
	r := start(t, ns, Config{Window: time.Hour, MaxPendingOccurrencesPerIncident: perIncident})
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	r.writes.before = func(context.Context, *remediationv1alpha1.Incident) {
		once.Do(func() {
			close(entered)
			<-release
		})
	}
	defer close(release)
	now := time.Now().Truncate(time.Second)
	// The first observation is written at once, alone: the write in flight holds one occurrence.
	r.obs <- immediate(observation(1, 1, 1, 1, now))
	waitFor(t, entered)
	for i := 2; i <= perIncident+5; i++ {
		r.obs <- immediate(observation(1, i, 1, 1, now))
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	held := 1
	if b := r.pending[fp(1)]; b != nil {
		held += len(b.occurrences)
	}
	if b := r.failed[fp(1)]; b != nil {
		held += len(b.occurrences)
	}
	if held > perIncident {
		t.Fatalf("%d occurrences held unwritten with one write in flight, over the cap of %d", held, perIncident)
	}
	if r.heldBy(fp(1)) > perIncident || r.heldTotal > perIncident {
		t.Fatalf("held %d (total %d), over the cap of %d", r.heldBy(fp(1)), r.heldTotal, perIncident)
	}
}

// Many new occurrences of one fingerprint while every status write fails: the occurrences held unwritten
// stay within the caps, per Incident and in total, and so does the memory of records. What goes over is not
// counted (an undercount, in the metric), but each Incident still gets its last seen once writes succeed.
func TestHeldOccurrencesStayWithinTheCaps(t *testing.T) {
	ns := namespace(t)
	const perIncident, total, each = 20, 30, 100
	r := start(t, ns, Config{Window: 100 * time.Millisecond, RetryAfter: 50 * time.Millisecond,
		MaxPendingOccurrences: total, MaxPendingOccurrencesPerIncident: perIncident})
	var failing atomic.Bool
	failing.Store(true)
	r.writes.instead = func(context.Context) error {
		if failing.Load() {
			return apierrors.NewServerTimeout(remediationv1alpha1.GroupVersion.WithResource("incidents").GroupResource(), "update", 1)
		}
		return nil
	}
	before := testutil.ToFloat64(overflowedOccurrences)
	from := time.Now().Truncate(time.Second).Add(-time.Hour)
	newest := from.Add(each * time.Second)
	check := func() {
		t.Helper()
		r.mu.Lock()
		defer r.mu.Unlock()
		sum := 0
		for f := 1; f <= 2; f++ {
			n := 0
			for _, b := range []*batch{r.pending[fp(f)], r.failed[fp(f)]} {
				if b != nil {
					n += len(b.occurrences)
				}
			}
			if n > perIncident {
				t.Fatalf("fingerprint %d holds %d occurrences unwritten, over the cap of %d", f, n, perIncident)
			}
			sum += n
		}
		if sum > total+2 {
			t.Fatalf("%d occurrences held unwritten, over the cap of %d and one per Incident", sum, total)
		}
		if len(r.records) > total+2 {
			t.Fatalf("%d records in memory with nothing stored, over the occurrences held", len(r.records))
		}
	}
	for i := 1; i <= each; i++ {
		for f := 1; f <= 2; f++ {
			r.obs <- immediate(observation(f, f*1000+i, 1, 1, from.Add(time.Duration(i)*time.Second)))
		}
		check()
	}
	time.Sleep(300 * time.Millisecond) // several failed retries
	check()
	r.mu.Lock()
	if r.heldTotal > total+2 {
		t.Errorf("heldTotal %d, over the cap", r.heldTotal)
	}
	r.mu.Unlock()
	uncounted := testutil.ToFloat64(overflowedOccurrences) - before
	if uncounted < 2*each-total-2 {
		t.Fatalf("overflow metric grew by %v, want at least %d", uncounted, 2*each-total-2)
	}
	failing.Store(false)
	counted := int64(2*each) - int64(uncounted)
	var got int64
	for f := 1; f <= 2; f++ {
		inc := eventually(t, ns, f, func(inc *remediationv1alpha1.Incident) bool {
			return inc.Status.LastSeen != nil && inc.Status.LastSeen.Time.Equal(newest)
		})
		got += inc.Status.Count
	}
	if got != counted {
		t.Fatalf("counted %d, want the %d not left over the caps", got, counted)
	}
	// The status write is visible before the writer recounts what it held, so wait for the recount.
	pollUntil(t, "the held occurrences to drain", func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.heldTotal == 0 && len(r.held) == 0
	})
	r.mu.Lock()
	defer r.mu.Unlock()
	indexed(t, r.Controller)
}

// observationIn is observation with one count of a new occurrence from namespace source, about a subject
// in that namespace. It is immediate, so the tests of the caps on held occurrences count every one at once;
// TestOpeningSightingsAreCountedUnderTheCaps counts first sightings under the caps.
func observationIn(source string, f, i int, began time.Time) collector.Observation {
	o := immediate(observation(f, i, 1, 1, began))
	o.Subject.Namespace, o.Source = source, source
	return o
}

// nodeObservation is observationIn about a Node, a cluster-scoped subject with no namespace, reported by an
// event in namespace source.
func nodeObservation(source string, f, i int, began time.Time) collector.Observation {
	o := immediate(observation(f, i, 1, 1, began))
	o.Subject = remediationv1alpha1.ObjectRef{APIVersion: "v1", Kind: "Node", Name: fmt.Sprintf("node-%d", f),
		UID: "44444444-4444-4444-8444-444444444444"}
	o.Reason, o.Symptom, o.Source = "NodeNotReady", "Node status is now: NodeNotReady", source
	return o
}

// heldConsistent reports, with mu held, whether held and heldIn add up to heldTotal, and each holding's in
// to its n: the empty string, or what does not. It does not fail the test itself, so the caller can unlock
// first.
func heldConsistent(c *Controller) string {
	perFingerprint, perNamespace := 0, 0
	in := map[string]int{}
	for f, h := range c.held {
		if h.n <= 0 {
			return fmt.Sprintf("fingerprint %s kept with %d held", f, h.n)
		}
		perFingerprint += h.n
		sum := 0
		for ns, k := range h.in {
			if k <= 0 {
				return fmt.Sprintf("fingerprint %s keeps namespace %q with %d held", f, ns, k)
			}
			sum += k
			in[ns] += k
		}
		if sum != h.n {
			return fmt.Sprintf("fingerprint %s holds %d, its namespaces %v", f, h.n, h.in)
		}
	}
	for ns, n := range c.heldIn {
		if n <= 0 || in[ns] != n {
			return fmt.Sprintf("namespace %q kept with %d held, its fingerprints hold %d", ns, n, in[ns])
		}
		perNamespace += n
	}
	if len(in) != len(c.heldIn) || perFingerprint != c.heldTotal || perNamespace != c.heldTotal {
		return fmt.Sprintf("held %v and heldIn %v do not add up to heldTotal %d", heldString(c), c.heldIn, c.heldTotal)
	}
	return ""
}

// heldString prints held with the counts, not the pointers. The caller holds mu.
func heldString(c *Controller) string {
	out := map[string]holding{}
	for f, h := range c.held {
		out[f] = *h
	}
	return fmt.Sprint(out)
}

// waitHeld waits until cond holds for the controller's held counts, checked with mu held, and fails at once
// when the counts do not add up.
func waitHeld(t *testing.T, r *running, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		r.mu.Lock()
		ok, bad := cond(), heldConsistent(r.Controller)
		held, in, total := heldString(r.Controller), fmt.Sprint(r.heldIn), r.heldTotal
		r.mu.Unlock()
		if bad != "" {
			t.Fatal(bad)
		}
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: held %s, heldIn %s, heldTotal %d", what, held, in, total)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// One namespace that holds its full share of the occurrences held unwritten, while status writes fail,
// leaves room under the total: the new occurrences of an Incident in another namespace are still counted.
// Once writes succeed, every held count goes back to zero.
func TestNamespaceShareLeavesRoomForOtherNamespaces(t *testing.T) {
	ns := namespace(t)
	const perIncident, perNamespace, total, noisy, quiet = 20, 10, 30, 3, 5
	r := start(t, ns, Config{Window: 100 * time.Millisecond, RetryAfter: 50 * time.Millisecond,
		MaxPendingOccurrences: total, MaxPendingOccurrencesPerIncident: perIncident,
		MaxPendingOccurrencesPerNamespace: perNamespace})
	var failing atomic.Bool
	failing.Store(true)
	r.writes.instead = func(context.Context) error {
		if failing.Load() {
			return apierrors.NewServerTimeout(remediationv1alpha1.GroupVersion.WithResource("incidents").GroupResource(), "update", 1)
		}
		return nil
	}
	from := time.Now().Truncate(time.Second).Add(-time.Hour)
	// Fingerprints 1 to 3 in "noisy" send more than the total cap between them.
	for f := 1; f <= noisy; f++ {
		for i := 1; i <= perIncident; i++ {
			r.obs <- observationIn("noisy", f, f*1000+i, from.Add(time.Duration(i)*time.Second))
		}
	}
	for i := 1; i <= quiet; i++ {
		r.obs <- observationIn("quiet", 9, 9000+i, from.Add(time.Duration(i)*time.Second))
	}
	waitHeld(t, r, "the quiet namespace's occurrences are held", func() bool { return r.heldBy(fp(9)) == quiet })
	r.mu.Lock()
	inNoisy, heldTotal := r.heldIn["noisy"], r.heldTotal
	r.mu.Unlock()
	// The noisy namespace stops at its share, plus the first occurrence of each other Incident that held none.
	if inNoisy > perNamespace+noisy-1 {
		t.Fatalf("noisy namespace holds %d, over its share of %d", inNoisy, perNamespace)
	}
	if heldTotal > total {
		t.Fatalf("heldTotal %d, over the cap of %d", heldTotal, total)
	}
	failing.Store(false)
	eventually(t, ns, 9, countIs(quiet))
	waitHeld(t, r, "held counts go back to zero once every batch is written", func() bool {
		return r.heldTotal == 0 && len(r.held) == 0 && len(r.heldIn) == 0
	})
}

// Occurrences are charged to the namespace they came from, not to their subject's: a Node has none, so
// every cluster-scoped Incident would otherwise share one key. A tenant that holds its full share with
// Node-regarding events neither blocks the same fingerprint's later occurrences from another namespace
// (default, where node events land) nor other cluster-scoped Incidents, and its share is not charged for
// them. Every held count goes back to zero after writes that succeed and after writes refused as invalid.
func TestClusterScopedSubjectsAreChargedToTheirSource(t *testing.T) {
	ns := namespace(t)
	const perIncident, perNamespace, total = 20, 10, 100
	r := start(t, ns, Config{Window: 100 * time.Millisecond, RetryAfter: 50 * time.Millisecond,
		MaxPendingOccurrences: total, MaxPendingOccurrencesPerIncident: perIncident,
		MaxPendingOccurrencesPerNamespace: perNamespace})
	const (
		failing = iota
		succeeding
		refusing
	)
	var mode atomic.Int32
	r.writes.instead = func(context.Context) error {
		switch mode.Load() {
		case failing:
			return apierrors.NewServerTimeout(remediationv1alpha1.GroupVersion.WithResource("incidents").GroupResource(), "update", 1)
		case refusing:
			return apierrors.NewInvalid(remediationv1alpha1.GroupVersion.WithKind("Incident").GroupKind(), "x", nil)
		}
		return nil
	}
	from := time.Now().Truncate(time.Second).Add(-time.Hour)
	at := func(i int) time.Time { return from.Add(time.Duration(i) * time.Second) }
	// The tenant fills its share with node 1, and node 2 takes its first occurrence past it.
	for i := 1; i <= perNamespace+5; i++ {
		r.obs <- nodeObservation("tenant", 1, i, at(i))
	}
	for i := 1; i <= 5; i++ {
		r.obs <- nodeObservation("tenant", 2, 200+i, at(i))
	}
	// The kubelet's reports in default: node 1 again, and node 3.
	const again, other = 5, 4
	for i := 1; i <= again; i++ {
		r.obs <- nodeObservation("default", 1, 100+i, at(i))
	}
	for i := 1; i <= other; i++ {
		r.obs <- nodeObservation("default", 3, 300+i, at(i))
	}
	waitHeld(t, r, "default's occurrences are counted", func() bool {
		return r.heldBy(fp(1)) == perNamespace+again && r.heldBy(fp(2)) == 1 && r.heldBy(fp(3)) == other
	})
	r.mu.Lock()
	tenant, dflt, empty := r.heldIn["tenant"], r.heldIn["default"], r.heldIn[""]
	r.mu.Unlock()
	if tenant != perNamespace+1 || dflt != again+other || empty != 0 {
		t.Fatalf("held in tenant %d, default %d, the empty namespace %d; want %d, %d and 0",
			tenant, dflt, empty, perNamespace+1, again+other)
	}
	mode.Store(succeeding)
	eventually(t, ns, 1, countIs(perNamespace+again))
	eventually(t, ns, 3, countIs(other))
	zero := func() bool { return r.heldTotal == 0 && len(r.held) == 0 && len(r.heldIn) == 0 }
	waitHeld(t, r, "held counts go back to zero once every batch is written", zero)

	// Writes refused as invalid drop their batches: the counts of both namespaces go back to zero too.
	mode.Store(refusing)
	for i := 1; i <= 3; i++ {
		r.obs <- nodeObservation("tenant", 1, 400+i, at(i))
		r.obs <- nodeObservation("default", 1, 500+i, at(i))
	}
	waitHeld(t, r, "held counts go back to zero once the batches are refused", zero)
}

// The share of one namespace is within the caps: by default a quarter of the total but never under the
// per-Incident cap, never over the total and never zero.
func TestNamespaceShareIsWithinTheCaps(t *testing.T) {
	for _, tt := range []struct {
		total, perIncident, share, want int
	}{
		{0, 0, 0, DefaultMaxPendingOccurrences / DefaultNamespaceShareDivisor},
		{30, 5, 0, 7},
		// A quarter of the total would lower the per-Incident cap: the default share takes that cap.
		{30, 20, 0, 20},
		{10000, 4000, 0, 4000},
		// ... but never more than the total.
		{30, 100, 0, 30},
		{30, 5, 10, 10},
		{30, 5, 100, 30},
		{2, 1, 0, 1},
	} {
		c, err := New(Config{Namespace: "operator", MaxPendingOccurrences: tt.total,
			MaxPendingOccurrencesPerIncident: tt.perIncident, MaxPendingOccurrencesPerNamespace: tt.share})
		if err != nil {
			t.Fatal(err)
		}
		if got := c.cfg.MaxPendingOccurrencesPerNamespace; got != tt.want {
			t.Errorf("total %d, per Incident %d, share %d: got share %d, want %d", tt.total, tt.perIncident,
				tt.share, got, tt.want)
		}
	}
}

// A status write in flight when the controller is stopped returns with the cancellation; the batches it
// held are written on stop, and every held count, per Incident, per namespace and in total, goes back to
// zero.
func TestHeldCountsReturnToZeroAfterCancellation(t *testing.T) {
	ns := namespace(t)
	r := start(t, ns, Config{Window: time.Hour, RetryAfter: 50 * time.Millisecond, StopTimeout: 5 * time.Second})
	var hang atomic.Bool
	hang.Store(true)
	entered := make(chan struct{})
	var once sync.Once
	r.writes.instead = func(ctx context.Context) error {
		if !hang.Load() {
			return nil
		}
		once.Do(func() { close(entered) })
		<-ctx.Done()
		return ctx.Err()
	}
	now := time.Now().Truncate(time.Second)
	r.obs <- observationIn("a", 1, 1, now)
	waitFor(t, entered)
	// One Incident's occurrences from two namespaces, each charged to its own.
	for i := 2; i <= 3; i++ {
		r.obs <- observationIn("a", 1, i, now)
	}
	r.obs <- observationIn("b", 1, 4, now)
	r.obs <- observationIn("b", 2, 100, now)
	waitHeld(t, r, "the occurrences are held", func() bool { return r.heldIn["a"] == 3 && r.heldIn["b"] == 2 })
	hang.Store(false)
	r.stop()
	if inc := get(t, ns); inc.Status.Count != 4 {
		t.Fatalf("count %d after stop, want 4", inc.Status.Count)
	}
	eventually(t, ns, 2, countIs(1))
	// Start has returned: nothing else touches the held counts any more.
	if bad := heldConsistent(r.Controller); bad != "" {
		t.Fatal(bad)
	}
	if r.heldTotal != 0 || len(r.held) != 0 || len(r.heldIn) != 0 {
		t.Fatalf("held %s, heldIn %v, heldTotal %d after stop wrote every batch, want nothing",
			heldString(r.Controller), r.heldIn, r.heldTotal)
	}
}
