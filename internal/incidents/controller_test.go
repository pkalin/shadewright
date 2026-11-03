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
	"k8s.io/client-go/tools/events"
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
// update (before) or replace its result (after).
type countingClient struct {
	client.Client
	creates, updates atomic.Int32
	before           func(ctx context.Context, inc *remediationv1alpha1.Incident)
	after            func(err error) error
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
	s.c.updates.Add(1)
	if s.c.before != nil {
		s.c.before(ctx, obj.(*remediationv1alpha1.Incident))
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
	cfg.Namespace = ns
	if cfg.Window == 0 {
		cfg.Window = time.Second
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r := &running{Controller: c, writes: &countingClient{Client: env.admin}, obs: make(chan collector.Observation)}
	c.client, c.reader, c.cache, c.obs = r.writes, env.admin, env.admin, r.obs
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

func get(t *testing.T, ns string, f int) *remediationv1alpha1.Incident {
	t.Helper()
	inc := &remediationv1alpha1.Incident{}
	if err := env.admin.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: Name(fp(f))}, inc); err != nil {
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
			inc := get(t, ns, 1)
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
	if inc := get(t, ns, 1); inc.Status.Count != 4 {
		t.Fatalf("count %d after a lost answer, want 4", inc.Status.Count)
	}
	if !lost.Load() || r.writes.updates.Load() != 1 {
		t.Fatalf("lost %v, %d status writes: want the retry to find the batch applied without writing", lost.Load(), r.writes.updates.Load())
	}
	r.stop()
	if r.pending[fp(1)] != nil {
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
	inc := eventually(t, ns, 1, countIs(13))
	time.Sleep(500 * time.Millisecond)
	if inc = get(t, ns, 1); inc.Status.Count != 13 {
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
	rec := events.NewFakeRecorder(10)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "analyzer-0"}}
	r := start(t, ns, Config{MaxIncidents: 2, Recorder: rec, Regarding: pod})
	before := testutil.ToFloat64(refusedObservations)
	now := time.Now().Truncate(time.Second)
	r.obs <- observation(1, 1, 1, 1, now)
	r.obs <- observation(2, 2, 1, 1, now)
	r.obs <- observation(3, 3, 1, 1, now)
	r.obs <- observation(3, 4, 1, 1, now)
	r.obs <- observation(1, 5, 1, 1, now)
	eventually(t, ns, 1, countIs(2))
	eventually(t, ns, 2, countIs(1))
	err := env.admin.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: Name(fp(3))}, &remediationv1alpha1.Incident{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("the third fingerprint was stored (%v)", err)
	}
	if got := testutil.ToFloat64(refusedObservations) - before; got != 2 {
		t.Fatalf("refused metric grew by %v, want 2", got)
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
		t.Fatalf("a second event for the same fingerprint: %q", e)
	default:
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
	allowed := []string{"phase", "firstSeen", "lastSeen", "count", "occurrences"}
	for k := range fields {
		if !slices.Contains(allowed, k) {
			t.Errorf("unexpected status field %q", k)
		}
	}
	if want := "incident-" + strings.Repeat("0", 63) + "1"; inc.Name != want {
		t.Fatalf("name %s, want %s", inc.Name, want)
	}
}
