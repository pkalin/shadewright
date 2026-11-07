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
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/pkalin/shadewright/internal/modules"
	"github.com/pkalin/shadewright/internal/modules/kyverno"
)

// watching starts the harness's watch on the Policy kind and checks that it is reported as watched.
func watching(t *testing.T) *lazyHarness {
	t.Helper()
	h := newLazyHarness(t, gvkPolicy, kyvernoActive(t))
	h.serve(meta.RESTScopeNamespace)
	h.step()
	if s := h.state(); s != WatchWatched {
		t.Fatalf("state %s after the first check, want Watched", s)
	}
	return h
}

// TestWatchThatStopsOnItsOwnBacksOff (#409 review): a watch whose controller stops on its own (its cache did
// not sync) is reported as failed (state, gauge, change notice, an Error log), its context is cancelled, and it
// is started again only after the backoff, not at every check.
func TestWatchThatStopsOnItsOwnBacksOff(t *testing.T) {
	h := watching(t)
	run, changes := h.run(), h.changes.Load()
	run.finish(errors.New("cache did not sync"))
	h.step()
	if s := h.state(); s != WatchFailed {
		t.Errorf("state %s, want Failed", s)
	}
	if g := h.gauge(); g != 0 {
		t.Errorf("gauge %v, want 0", g)
	}
	if h.changes.Load() != changes+1 {
		t.Error("no change notice for the failed watch")
	}
	if run.ctx.Err() == nil {
		t.Error("the context of the watch that stopped was not cancelled")
	}
	if _, errs := h.log.messages(); len(errs) == 0 {
		t.Error("the failed watch was not logged as an error")
	}
	h.step()
	h.clock = h.clock.Add(watchBackoff(h.l.r.c.cfg.SelectionPoll, 1) / 2)
	h.step()
	if n := h.starts.Load(); n != 1 {
		t.Errorf("%d starts within the backoff, want 1", n)
	}
	h.clock = h.clock.Add(watchBackoff(h.l.r.c.cfg.SelectionPoll, 1))
	h.step()
	if n := h.starts.Load(); n != 2 {
		t.Errorf("%d starts after the backoff, want 2", n)
	}
	if s := h.state(); s != WatchWatched {
		t.Errorf("state %s after the restart, want Watched", s)
	}
}

// TestWatchRefusedWhileListAllowedFails (#409 review): the list is granted but the watch is refused, so the
// check passes while the watch cannot run. The refusal stops the watch at once (it wakes the loop), the kind
// is reported as failed, and the watch is not started again before the backoff ends.
func TestWatchRefusedWhileListAllowedFails(t *testing.T) {
	h := watching(t)
	h.run().errs.report(forbidden())
	select {
	case <-h.l.wake:
	default:
		t.Error("a refused watch did not wake the loop")
	}
	h.step()
	if h.stops.Load() != 1 || h.l.running != nil {
		t.Errorf("stops %d, running %v, want the watch stopped", h.stops.Load(), h.l.running != nil)
	}
	if got := h.notWatched(); !slices.Equal(got, []WatchState{WatchFailed}) {
		t.Errorf("SourcesNotWatched = %v, want [Failed]", got)
	}
	h.step()
	h.step()
	if n := h.starts.Load(); n != 1 {
		t.Errorf("%d starts within the backoff, want 1", n)
	}
	// The next failure waits twice as long.
	h.clock = h.clock.Add(watchBackoff(h.l.r.c.cfg.SelectionPoll, 1))
	h.step()
	h.run().errs.report(forbidden())
	h.step()
	if h.l.retryAt.Sub(h.clock) != watchBackoff(h.l.r.c.cfg.SelectionPoll, 2) {
		t.Errorf("backoff %v after the second failure, want %v", h.l.retryAt.Sub(h.clock),
			watchBackoff(h.l.r.c.cfg.SelectionPoll, 2))
	}
}

// TestWatchRefusedAfterGrantRemovedIsNotGranted: the grant is removed while the watch runs, so the list is
// refused too. The watch stops and the kind is reported as not granted, as the check finds it.
func TestWatchRefusedAfterGrantRemovedIsNotGranted(t *testing.T) {
	h := watching(t)
	h.api.fail[kyverno.PolicyGroup] = forbidden()
	h.run().errs.report(forbidden())
	h.step()
	if got := h.notWatched(); !slices.Equal(got, []WatchState{WatchNotGranted}) {
		t.Errorf("SourcesNotWatched = %v, want [NotGranted]", got)
	}
	delete(h.api.fail, kyverno.PolicyGroup)
	h.step()
	if n := h.starts.Load(); n != 2 {
		t.Errorf("%d starts once granted again, want 2", n)
	}
}

// TestUnsyncedWatchIsNotWatched: a watch whose cache has not synced is not reported as watched, and a refusal
// of its first list (the grant removed between the check and the list) stops it.
func TestUnsyncedWatchIsNotWatched(t *testing.T) {
	h := newLazyHarness(t, gvkPolicy, kyvernoActive(t))
	h.serve(meta.RESTScopeNamespace)
	h.noSync = true
	h.othersChecked()
	h.step()
	if s := h.state(); s == WatchWatched {
		t.Error("a watch that has not synced is reported as watched")
	}
	if _, complete := h.l.r.c.SourcesNotWatched(); complete {
		t.Error("the report is complete before the watch synced")
	}
	h.run().errs.report(forbidden())
	h.step()
	if s := h.state(); s != WatchFailed {
		t.Errorf("state %s, want Failed", s)
	}
}

// TestPersistentWatchErrorIsReported (#409 review): errors the reflector retries (the API server unavailable)
// are reported once failedAfterChecks checks in a row saw one; the watch keeps running and is reported as
// watched again at the first check after a request of the watch succeeded.
func TestPersistentWatchErrorIsReported(t *testing.T) {
	h := watching(t)
	for i := 1; i <= failedAfterChecks; i++ {
		h.run().errs.report(apierrors.NewServiceUnavailable("try later"))
		h.step()
		want := WatchWatched
		if i == failedAfterChecks {
			want = WatchFailed
		}
		if s := h.state(); s != want {
			t.Errorf("state %s after %d checks with an error, want %s", s, i, want)
		}
	}
	if h.stops.Load() != 0 {
		t.Error("an error the reflector retries stopped the watch")
	}
	h.run().errs.ok()
	h.step()
	if s := h.state(); s != WatchWatched {
		t.Errorf("state %s after a request succeeded, want Watched", s)
	}
}

// TestExpiredWatchIsNotAnError: a watch that expires or ends is listed again by the reflector; it is not
// counted.
func TestExpiredWatchIsNotAnError(t *testing.T) {
	w := &watchErrors{}
	w.report(apierrors.NewResourceExpired("too old"))
	w.report(context.Canceled)
	if fatal, transient := w.take(); fatal != nil || transient != nil {
		t.Errorf("take() = %v, %v, want nothing", fatal, transient)
	}
}

// TestFirstResultIsPending (#409 review): before the first check of an active module's kind, the report is
// not complete (detection reports Unknown, not every kind watched); after it, it is.
func TestFirstResultIsPending(t *testing.T) {
	h := newLazyHarness(t, gvkPolicy, kyvernoActive(t))
	h.serve(meta.RESTScopeNamespace)
	h.othersChecked()
	if kinds, complete := h.l.r.c.SourcesNotWatched(); complete || len(kinds) != 0 {
		t.Errorf("SourcesNotWatched() = %v, %v before the first check, want none, false", kinds, complete)
	}
	h.step()
	if _, complete := h.l.r.c.SourcesNotWatched(); !complete {
		t.Error("the report is not complete after the first check")
	}
}

// cancelledLister answers every list with the error of its context, as the API reader does at shutdown.
type cancelledLister struct{ *objectReader }

func (c cancelledLister) List(ctx context.Context, _ client.ObjectList, _ ...client.ListOption) error {
	return ctx.Err()
}

// TestCancelledCheckReportsNothing (#409 review): a check cut short by a stop (the process shuts down or loses
// leadership) records no state and logs no error.
func TestCancelledCheckReportsNothing(t *testing.T) {
	h := newLazyHarness(t, gvkPolicy, kyvernoActive(t))
	h.serve(meta.RESTScopeNamespace)
	h.l.api = cancelledLister{h.api}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	h.l.step(ctx)
	if s := h.state(); s != "" {
		t.Errorf("state %s after a cancelled check, want none", s)
	}
	if _, errs := h.log.messages(); len(errs) != 0 {
		t.Errorf("errors logged for a cancelled check: %v", errs)
	}
}

// TestInactiveAfterNotWatchedIsNotAStop: a kind that was never watched and whose module is no longer active is
// not logged as a watch that stopped.
func TestInactiveAfterNotWatchedIsNotAStop(t *testing.T) {
	h := newLazyHarness(t, gvkPolicy, kyvernoActive(t))
	h.serve(meta.RESTScopeNamespace)
	h.api.fail[kyverno.PolicyGroup] = forbidden()
	h.step()
	h.sel = modules.Selection{}
	h.step()
	infos, _ := h.log.messages()
	if len(infos) == 0 || infos[len(infos)-1] != "No longer checking a symptom source kind, no active module declares it" {
		t.Errorf("logged %q, want the last to say the kind is no longer checked", infos)
	}
}

// TestOmittedGrantIsNotReported (#409 review): with Kyverno's namespaced grant rendered but not its optional
// cluster-wide one (the documented least-privilege setup), the cluster-scoped kinds are not listed, not
// watched, not reported as missing and have no gauge series; the report is complete.
func TestOmittedGrantIsNotReported(t *testing.T) {
	h := newLazyHarnessWith(t, gvkClusterPolicy, kyvernoActive(t), []string{GrantKEDA, GrantKyverno})
	h.serve(meta.RESTScopeRoot)
	h.othersChecked()
	h.step()
	if s := h.state(); s != WatchOmitted {
		t.Errorf("state %s, want Omitted", s)
	}
	if kinds, complete := h.l.r.c.SourcesNotWatched(); len(kinds) != 0 || !complete {
		t.Errorf("SourcesNotWatched() = %v, %v, want none, true", kinds, complete)
	}
	if h.hasSeries() {
		t.Error("gauge has a series for a kind left out on purpose")
	}
	if h.api.reads(kyverno.PolicyGroup) != 0 || h.starts.Load() != 0 {
		t.Error("a kind left out on purpose was listed or watched")
	}
}

// TestRequiredGrantNotRenderedIsNotGranted: with the Kyverno module active but its namespaced grant not
// rendered, the Policy kind is reported as not granted without a list.
func TestRequiredGrantNotRenderedIsNotGranted(t *testing.T) {
	h := newLazyHarnessWith(t, gvkPolicy, kyvernoActive(t), []string{})
	h.serve(meta.RESTScopeNamespace)
	h.step()
	if got := h.notWatched(); !slices.Equal(got, []WatchState{WatchNotGranted}) {
		t.Errorf("SourcesNotWatched = %v, want [NotGranted]", got)
	}
	if h.api.reads(kyverno.PolicyGroup) != 0 {
		t.Error("a kind whose grant was not rendered was listed")
	}
}

// TestEveryModuleKindHasAGrant: each kind of SourceKinds that is not built in names a grant of ModuleGrants,
// and an unknown grant is refused.
func TestEveryModuleKindHasAGrant(t *testing.T) {
	for _, gvk := range SourceKinds {
		if builtinKind(gvk) {
			continue
		}
		if name, _ := grantOf(gvk); !slices.Contains(ModuleGrants, name) {
			t.Errorf("%s has grant %q, want one of %v", gvk, name, ModuleGrants)
		}
	}
	if _, err := New(Config{ClusterID: testCluster, Grants: []string{"argo"}}); err == nil {
		t.Error("an unknown grant was accepted")
	}
	if name, _ := grantOf(schema.GroupVersionKind{Group: "example.com", Kind: "Thing"}); name != "" {
		t.Errorf("grantOf of an unknown kind = %q, want none", name)
	}
}
