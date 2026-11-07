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

package collector

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus/testutil"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/pkalin/shadewright/internal/modules"
	"github.com/pkalin/shadewright/internal/modules/kyverno"
)

// otherNS is a second watched namespace.
const otherNS = "other"

var (
	gvkPolicy        = schema.GroupVersionKind{Group: kyverno.PolicyGroup, Version: kyverno.PolicyVersion, Kind: kyverno.KindPolicy}
	gvkClusterPolicy = schema.GroupVersionKind{Group: kyverno.PolicyGroup, Version: kyverno.PolicyVersion, Kind: kyverno.KindClusterPolicy}
)

// lazyHarness drives one lazySource step by step, with a fake API reader and RESTMapper, a clock of its own
// and a starter whose runs the test controls (fakeRun).
type lazyHarness struct {
	t       *testing.T
	l       *lazySource
	api     *objectReader
	mapper  *meta.DefaultRESTMapper
	sel     modules.Selection
	clock   time.Time
	log     *recordingSink
	runs    []*fakeRun
	noSync  bool  // runs do not sync until the test says so
	buildEr error // runs stop at once with this error, as a controller that cannot be built
	starts  atomic.Int32
	changes atomic.Int32
	stops   atomic.Int32
}

// fakeRun is one watch the harness started.
type fakeRun struct {
	ctx    context.Context
	done   chan error
	synced chan struct{}
	errs   *watchErrors
	once   sync.Once
}

// finish ends the run with err (once): the controller stopped.
func (r *fakeRun) finish(err error) {
	r.once.Do(func() { r.done <- err; close(r.done) })
}

func newLazyHarness(t *testing.T, gvk schema.GroupVersionKind, sel modules.Selection) *lazyHarness {
	t.Helper()
	return newLazyHarnessWith(t, gvk, sel, nil)
}

// newLazyHarnessWith is newLazyHarness with the module grants the chart rendered (nil: not known).
func newLazyHarnessWith(t *testing.T, gvk schema.GroupVersionKind, sel modules.Selection, grants []string) *lazyHarness {
	t.Helper()
	h := &lazyHarness{t: t, api: newObjectReader(), mapper: meta.NewDefaultRESTMapper(nil), sel: sel,
		clock: time.Unix(1_800_000_000, 0), log: &recordingSink{}}
	c, err := New(Config{
		ClusterID: testCluster, Namespaces: []string{testNS, otherNS}, Grants: grants,
		Modules:      func() modules.Selection { return h.sel },
		WatchChanged: func() { h.changes.Add(1) },
	})
	if err != nil {
		t.Fatal(err)
	}
	c.now = func() time.Time { return h.clock }
	r := &sourceReconciler{c: c, reader: h.api, api: h.api, gvk: gvk}
	h.l = &lazySource{r: r, api: h.api, mapper: h.mapper, log: logr.New(h.log), wake: make(chan struct{}, 1),
		start: func(ctx context.Context, _ *sourceReconciler, errs *watchErrors) (<-chan error, <-chan struct{}) {
			h.starts.Add(1)
			run := &fakeRun{ctx: ctx, done: make(chan error, 1), synced: make(chan struct{}), errs: errs}
			h.runs = append(h.runs, run)
			if h.buildEr != nil {
				run.finish(h.buildEr)
				return run.done, run.synced
			}
			if !h.noSync {
				close(run.synced)
			}
			go func() {
				<-ctx.Done()
				h.stops.Add(1)
				run.finish(nil)
			}()
			return run.done, run.synced
		}}
	t.Cleanup(func() { h.l.stop(); setWatchedGauge(gvk, WatchInactive) })
	return h
}

// run returns the last run started.
func (h *lazyHarness) run() *fakeRun {
	h.t.Helper()
	if len(h.runs) == 0 {
		h.t.Fatal("no watch was started")
	}
	return h.runs[len(h.runs)-1]
}

// othersChecked records every other kind of SourceKinds as watched, so SourcesNotWatched's completeness is
// that of the harness's kind alone.
func (h *lazyHarness) othersChecked() {
	c := h.l.r.c
	c.watchMu.Lock()
	defer c.watchMu.Unlock()
	for _, gvk := range SourceKinds {
		if gvk != h.l.r.gvk {
			c.watches[gvk] = WatchWatched
		}
	}
}

// state returns the kind's recorded watch state.
func (h *lazyHarness) state() WatchState {
	c := h.l.r.c
	c.watchMu.Lock()
	defer c.watchMu.Unlock()
	return c.watches[h.l.r.gvk]
}

// recordingSink records the messages logged.
type recordingSink struct {
	mu     sync.Mutex
	infos  []string
	errors []string
}

func (s *recordingSink) Init(logr.RuntimeInfo) {}
func (s *recordingSink) Enabled(int) bool      { return true }
func (s *recordingSink) Info(_ int, msg string, _ ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.infos = append(s.infos, msg)
}
func (s *recordingSink) Error(_ error, msg string, _ ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.errors = append(s.errors, msg)
}
func (s *recordingSink) WithValues(...any) logr.LogSink { return s }
func (s *recordingSink) WithName(string) logr.LogSink   { return s }

func (s *recordingSink) messages() (infos, errs []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.infos), slices.Clone(s.errors)
}

func (h *lazyHarness) serve(scope meta.RESTScope) { h.mapper.Add(h.l.r.gvk, scope) }

func (h *lazyHarness) step() { h.l.step(h.t.Context()) }

func (h *lazyHarness) gauge() float64 {
	return testutil.ToFloat64(symptomSourceWatched.WithLabelValues(h.l.r.gvk.Group, h.l.r.gvk.Kind))
}

// hasSeries reports whether the gauge had a series for the harness's kind, and removes it (other tests' kinds
// may have one, so the whole gauge is not counted).
func (h *lazyHarness) hasSeries() bool {
	return symptomSourceWatched.DeleteLabelValues(h.l.r.gvk.Group, h.l.r.gvk.Kind)
}

func (h *lazyHarness) notWatched() []WatchState {
	var out []WatchState
	kinds, _ := h.l.r.c.SourcesNotWatched()
	for _, w := range kinds {
		if w.Kind == h.l.r.gvk {
			out = append(out, w.State)
		}
	}
	return out
}

func kyvernoActive(t *testing.T) modules.Selection {
	t.Helper()
	r, err := modules.New(nil, []modules.Integration{kyverno.Module()})
	if err != nil {
		t.Fatal(err)
	}
	return r.Select(modules.Enabled{Integrations: []string{kyverno.Name}},
		r.Detect(nil, []string{kyverno.PolicyGroup + "/" + kyverno.PolicyVersion}))
}

// forbidden is the refusal of Kyverno Policies.
func forbidden() error {
	return apierrors.NewForbidden(schema.GroupResource{Group: kyverno.PolicyGroup, Resource: "policies"}, "",
		errors.New("no grant"))
}

// TestModuleEnabledWithoutGrantIsReported is the acceptance test of the conditional grants (#409): Kyverno is
// enabled and detected, the Policy kind is served, but the chart's value is off, so listing it is forbidden.
// The kind is reported as not granted (SourcesNotWatched, the gauge at 0, one change notice for detection),
// no watch starts and nothing is observed. Once the grant is there, the next check starts the watch.
func TestModuleEnabledWithoutGrantIsReported(t *testing.T) {
	h := newLazyHarness(t, gvkPolicy, kyvernoActive(t))
	h.serve(meta.RESTScopeNamespace)
	h.api.fail[kyverno.PolicyGroup] = forbidden()
	h.step()
	h.step()
	if got := h.notWatched(); !slices.Equal(got, []WatchState{WatchNotGranted}) {
		t.Errorf("SourcesNotWatched = %v, want [NotGranted]", got)
	}
	if g := h.gauge(); g != 0 {
		t.Errorf("gauge = %v, want 0", g)
	}
	if n := h.starts.Load(); n != 0 {
		t.Errorf("%d watches started without the grant, want none", n)
	}
	if n := h.changes.Load(); n != 1 {
		t.Errorf("%d change notices, want 1 (once per change, not per check)", n)
	}
	select {
	case o := <-h.l.r.c.Observations():
		t.Fatalf("observation without the grant: %+v", o)
	default:
	}

	delete(h.api.fail, kyverno.PolicyGroup)
	h.step()
	if got := h.notWatched(); len(got) != 0 {
		t.Errorf("SourcesNotWatched = %v after the grant, want none", got)
	}
	if g := h.gauge(); g != 1 {
		t.Errorf("gauge = %v after the grant, want 1", g)
	}
	h.step()
	if n := h.starts.Load(); n != 1 {
		t.Errorf("%d watches started, want 1", n)
	}

	// The grant is taken away again: the watch's list and watch are refused, so it stops and the kind is
	// reported.
	h.api.fail[kyverno.PolicyGroup] = forbidden()
	h.run().errs.report(forbidden())
	h.step()
	if n := h.stops.Load(); n != 1 {
		t.Errorf("%d watches stopped after the grant was removed, want 1", n)
	}
	if got := h.notWatched(); !slices.Equal(got, []WatchState{WatchNotGranted}) {
		t.Errorf("SourcesNotWatched = %v, want [NotGranted]", got)
	}
}

// TestKindNotServedIsReported: Kyverno is active but its Policy CRD is not installed. The kind is reported as
// not served and no list is made.
func TestKindNotServedIsReported(t *testing.T) {
	h := newLazyHarness(t, gvkPolicy, kyvernoActive(t))
	h.step()
	if got := h.notWatched(); !slices.Equal(got, []WatchState{WatchNotServed}) {
		t.Errorf("SourcesNotWatched = %v, want [NotServed]", got)
	}
	if h.starts.Load() != 0 || h.api.reads(kyverno.PolicyGroup) != 0 {
		t.Error("a kind that is not served was read or watched")
	}
}

// TestDisabledModuleProbesNothing: with Kyverno not enabled, nothing is read, nothing is reported, and the
// gauge has no series for the kind.
func TestDisabledModuleProbesNothing(t *testing.T) {
	h := newLazyHarness(t, gvkPolicy, modules.Selection{})
	h.serve(meta.RESTScopeNamespace)
	h.api.fail[kyverno.PolicyGroup] = forbidden()
	h.step()
	if got := h.notWatched(); len(got) != 0 {
		t.Errorf("SourcesNotWatched = %v with the module disabled, want none", got)
	}
	if h.starts.Load() != 0 || h.changes.Load() != 0 {
		t.Error("a disabled module started a watch or reported a change")
	}
	if h.hasSeries() {
		t.Error("gauge has a series for the kind with the module disabled, want none")
	}
}

// TestTransientFailureKeepsTheWatch: a check that fails for another reason than a missing grant or kind
// (the API server is unavailable) is reported as failed before a watch runs. Once a watch runs nothing is
// listed any more (#409 review): the watch's own errors decide, so a failing check cannot stop it.
func TestTransientFailureKeepsTheWatch(t *testing.T) {
	h := newLazyHarness(t, gvkPolicy, kyvernoActive(t))
	h.serve(meta.RESTScopeNamespace)
	h.api.fail[kyverno.PolicyGroup] = apierrors.NewServiceUnavailable("try later")
	h.step()
	if got := h.notWatched(); !slices.Equal(got, []WatchState{WatchFailed}) {
		t.Errorf("SourcesNotWatched = %v, want [Failed]", got)
	}
	delete(h.api.fail, kyverno.PolicyGroup)
	h.step()
	reads := h.api.reads(kyverno.PolicyGroup)
	h.api.fail[kyverno.PolicyGroup] = apierrors.NewServiceUnavailable("try later")
	h.step()
	h.step()
	if h.starts.Load() != 1 || h.stops.Load() != 0 {
		t.Errorf("starts %d, stops %d, want the watch kept", h.starts.Load(), h.stops.Load())
	}
	if n := h.api.reads(kyverno.PolicyGroup) - reads; n != 0 {
		t.Errorf("%d lists while the watch runs, want none", n)
	}
	if s := h.state(); s != WatchWatched {
		t.Errorf("state %s, want Watched", s)
	}
}

// TestModuleDisabledStopsTheWatch: a watch that runs stops when the module is no longer active, and the kind
// is no longer reported.
func TestModuleDisabledStopsTheWatch(t *testing.T) {
	h := newLazyHarness(t, gvkPolicy, kyvernoActive(t))
	h.serve(meta.RESTScopeNamespace)
	h.step()
	h.sel = modules.Selection{}
	h.step()
	if h.stops.Load() != 1 {
		t.Errorf("%d stops, want 1", h.stops.Load())
	}
	if h.hasSeries() {
		t.Error("gauge has a series for the kind after the module was disabled, want none")
	}
}

// recordingLister records the namespace of each list, and refuses a list that is not of metadata only with a
// limit of one.
type recordingLister struct {
	*objectReader
	namespaces []string
	t          *testing.T
}

func (r *recordingLister) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	o := (&client.ListOptions{}).ApplyOptions(opts)
	r.namespaces = append(r.namespaces, o.Namespace)
	if _, ok := list.(*metav1.PartialObjectMetadataList); !ok || o.Limit != 1 {
		r.t.Errorf("listed %T with limit %d, want metadata only with limit 1", list, o.Limit)
	}
	return r.objectReader.List(ctx, list, opts...)
}

// TestClusterScopedKindIsCheckedClusterWide: the ClusterPolicy is listed once, cluster-wide (the Kyverno
// cluster-wide value), and a namespaced kind once per watched namespace.
func TestClusterScopedKindIsCheckedClusterWide(t *testing.T) {
	for _, tc := range []struct {
		gvk   schema.GroupVersionKind
		scope meta.RESTScope
		want  []string
	}{
		{gvkClusterPolicy, meta.RESTScopeRoot, []string{""}},
		{gvkPolicy, meta.RESTScopeNamespace, []string{testNS, otherNS}},
	} {
		t.Run(tc.gvk.Kind, func(t *testing.T) {
			h := newLazyHarness(t, tc.gvk, kyvernoActive(t))
			h.serve(tc.scope)
			rec := &recordingLister{objectReader: h.api, t: t}
			h.l.api = rec
			h.step()
			if !slices.Equal(rec.namespaces, tc.want) {
				t.Errorf("listed in %q, want %q", rec.namespaces, tc.want)
			}
			if h.starts.Load() != 1 {
				t.Errorf("%d starts, want 1", h.starts.Load())
			}
		})
	}
}

// TestWatchThatCannotStartIsReported: a controller that cannot be built is reported as failed, not watched,
// and is tried again only after the backoff.
func TestWatchThatCannotStartIsReported(t *testing.T) {
	h := newLazyHarness(t, gvkPolicy, kyvernoActive(t))
	h.serve(meta.RESTScopeNamespace)
	h.buildEr = errors.New("cannot build")
	h.step()
	if got := h.notWatched(); !slices.Equal(got, []WatchState{WatchFailed}) {
		t.Errorf("SourcesNotWatched = %v, want [Failed]", got)
	}
	if h.l.running != nil {
		t.Error("a watch that did not start is kept as running")
	}
	h.step()
	if n := h.starts.Load(); n != 1 {
		t.Errorf("%d starts before the backoff ended, want 1", n)
	}
	h.clock = h.clock.Add(watchBackoff(h.l.r.c.cfg.SelectionPoll, 1))
	h.step()
	if n := h.starts.Load(); n != 2 {
		t.Errorf("%d starts, want 2 (tried again after the backoff)", n)
	}
}

// TestNoNamespaceIsNeverWatchedUnchecked: with no watched namespace a namespaced kind is not watched, since
// its grant could not be checked.
func TestNoNamespaceIsNeverWatchedUnchecked(t *testing.T) {
	h := newLazyHarness(t, gvkPolicy, kyvernoActive(t))
	h.serve(meta.RESTScopeNamespace)
	h.l.r.c.cfg.Namespaces = nil
	h.step()
	if h.starts.Load() != 0 {
		t.Error("a watch started without a grant check")
	}
}
