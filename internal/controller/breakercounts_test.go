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

package controller

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	fx "github.com/pkalin/shadewright/internal/testfixtures"
)

// The values breakerCountsOf returns.
const (
	fieldUnset = "unset"
	fieldTrue  = "true"
	fieldFalse = "false"
)

// memReader serves the limits' reads from items held in memory, the way the API server does (field selectors
// on the selectable fields, pages with a continue token), without the cost of the fake client, so a history of
// thousands of requests can be read many times. It counts the requests it returns. RemediationSettings default
// is served with a cap of 20.
type memReader struct {
	client.Reader
	items []*remediationv1alpha1.RemediationRequest
	// returned is how many requests the lists returned.
	returned int
}

func (m *memReader) Get(_ context.Context, key client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	s, ok := obj.(*remediationv1alpha1.RemediationSettings)
	if !ok || key.Name != remediationv1alpha1.SettingsName {
		return errors.New("memReader serves only RemediationSettings default")
	}
	s.Name = remediationv1alpha1.SettingsName
	s.Spec.MaxConcurrentRemediations = 20
	return nil
}

func (m *memReader) List(_ context.Context, list client.ObjectList, opts ...client.ListOption) error {
	o := &client.ListOptions{}
	o.ApplyOptions(opts)
	rl, ok := list.(*remediationv1alpha1.RemediationRequestList)
	if !ok {
		return errors.New("memReader lists only RemediationRequests")
	}
	sel := fields.Everything()
	if o.FieldSelector != nil {
		sel = o.FieldSelector
	}
	var match []*remediationv1alpha1.RemediationRequest
	for _, i := range m.items {
		if i.Namespace == o.Namespace && sel.Matches(selectableFields(i)) {
			match = append(match, i)
		}
	}
	start := 0
	if o.Continue != "" {
		var err error
		if start, err = strconv.Atoi(o.Continue); err != nil {
			return apierrors.NewBadRequest("bad continue token")
		}
	}
	end := len(match)
	rl.Continue = ""
	if o.Limit > 0 && int64(end-start) > o.Limit {
		end = start + int(o.Limit)
		rl.Continue = strconv.Itoa(end)
	}
	rl.Items = make([]remediationv1alpha1.RemediationRequest, 0, end-start)
	for _, i := range match[start:end] {
		rl.Items = append(rl.Items, *i.DeepCopy())
	}
	m.returned += len(rl.Items)
	return nil
}

// clear sets status.breakerCounts to false on the stale entries, as clearBreakerCounts does on the API server.
func (m *memReader) clear(stale []limitsEntry) {
	byName := map[string]bool{}
	for _, e := range stale {
		byName[e.name] = true
	}
	for _, i := range m.items {
		if byName[i.Name] {
			i.Status.BreakerCounts = new(false)
		}
	}
}

// unclassified is a copy of the items without status.breakerCounts, so the breaker's list reads every one of
// them: the decision of the breaker before #390, which read every success and failure.
func (m *memReader) unclassified() *memReader {
	c := &memReader{}
	for _, i := range m.items {
		x := i.DeepCopy()
		x.Status.BreakerCounts = nil
		c.items = append(c.items, x)
	}
	return c
}

// event is one request of a history: the phase it ended in, whether it was a probe, and whether it has
// lastPhaseTransitionTime.
type event struct {
	phase  remediationv1alpha1.RemediationPhase
	probe  bool
	noTime bool
}

// endRequest adds the i-th request of a history to m, ended at at as the executor writes it: the lock released,
// status.breakerCounts set by the status write that ends it (classifyBreaker). A Denied request has no plan
// digest (refused at the policy check).
func (m *memReader) endRequest(i int, ev event, at time.Time) {
	o := otherRequest(fmt.Sprintf("h-%05d", i), "uid-history", ev.phase, 0)
	o.ResourceVersion = "1"
	if ev.phase == remediationv1alpha1.PhaseDenied && !ev.probe {
		o.Status.PlanDigest = ""
	}
	mt := metav1.NewMicroTime(at)
	o.Status.LastPhaseTransitionTime = &mt
	o.Status.BreakerProbe = ev.probe
	if ev.noTime {
		o.Status.LastPhaseTransitionTime = nil
	}
	o.Status.HoldsTarget = new(false)
	classifyBreaker(o)
	m.items = append(m.items, o)
}

// pendingRequest is the request whose admission the limits decide: Pending, on a target no history request has.
func pendingRequest() *remediationv1alpha1.RemediationRequest {
	rr := fx.MemoryLimitRequest(requestNamespace, "decided")
	rr.UID = "uid-decided"
	rr.Spec.TargetRef.UID = "uid-decided-target"
	return rr
}

// decision is what the breaker decided in one check of the limits.
type decision struct {
	reason  string
	circuit circuitState
	probing bool
}

func decide(t *testing.T, r *RemediationRequestReconciler, m *memReader, now time.Time) (decision, limitsState) {
	t.Helper()
	r.APIReader = m
	r.Now = func() time.Time { return now }
	s, err := r.checkLimits(context.Background(), pendingRequest())
	if err != nil {
		t.Fatal(err)
	}
	return decision{reason: s.reason, circuit: s.circuit, probing: s.probing}, s
}

// TestBreakerReadsBounded checks the bound of #390: after 10,000 requests ended in a namespace (successes,
// failures, refusals, expiries and probes), each classified by the status write that ended it and the outcomes
// no longer needed cleared after the checks of the limits that followed, a check of the limits reads at most
// 2*threshold+3 requests, and decides as a read of every request does.
func TestBreakerReadsBounded(t *testing.T) {
	const n = 10000
	pattern := []event{
		{phase: remediationv1alpha1.PhaseSucceeded}, {phase: remediationv1alpha1.PhaseFailed},
		{phase: remediationv1alpha1.PhaseDenied}, {phase: remediationv1alpha1.PhaseNeedsIntervention},
		{phase: remediationv1alpha1.PhaseExpired}, {phase: remediationv1alpha1.PhaseFailed},
		{phase: remediationv1alpha1.PhaseExpired, probe: true}, {phase: remediationv1alpha1.PhaseCancelled},
		{phase: remediationv1alpha1.PhaseFailed}, {phase: remediationv1alpha1.PhaseFailed, probe: true},
		{phase: remediationv1alpha1.PhaseSuperseded}, {phase: remediationv1alpha1.PhaseMerged},
		{phase: remediationv1alpha1.PhaseFailed}, {phase: remediationv1alpha1.PhaseFailed},
		{phase: remediationv1alpha1.PhaseFailed}, {phase: remediationv1alpha1.PhaseFailed},
		{phase: remediationv1alpha1.PhaseCancelled, probe: true},
	}
	start := limitsNow.Add(-n * time.Minute)
	m := &memReader{}
	r := &RemediationRequestReconciler{}
	threshold := r.breakerThreshold()
	for i := range n {
		m.endRequest(i, pattern[i%len(pattern)], start.Add(time.Duration(i)*time.Minute))
		if i%50 == 49 {
			// A check of the limits runs now and then, and clears what it found no longer needed.
			_, s := decide(t, r, m, start.Add(time.Duration(i)*time.Minute))
			m.clear(s.stale)
		}
	}
	m.returned = 0
	got, s := decide(t, r, m, limitsNow)
	if limit := 2*threshold + 3; m.returned > limit {
		t.Fatalf("a check of the limits read %d of %d ended requests, want at most %d", m.returned, n, limit)
	}
	if len(s.stale) != 0 {
		t.Fatalf("%d requests left to clear after the history, want 0", len(s.stale))
	}
	full := m.unclassified()
	want, _ := decide(t, r, full, limitsNow)
	if full.returned < n {
		t.Fatalf("the unclassified copy read %d requests, want all %d", full.returned, n)
	}
	if got != want {
		t.Fatalf("breaker decided %+v on the bounded read, %+v on every request", got, want)
	}
}

// TestBreakerDecidesAsBefore checks on representative histories that the watermark never changes a decision:
// after each request ends, and again after the clearing that follows, the breaker decides at several times
// exactly as it does when it reads every request (all without status.breakerCounts, as before #390).
func TestBreakerDecidesAsBefore(t *testing.T) {
	S := event{phase: remediationv1alpha1.PhaseSucceeded}
	M := event{phase: remediationv1alpha1.PhaseMerged}
	F := event{phase: remediationv1alpha1.PhaseFailed}
	N := event{phase: remediationv1alpha1.PhaseNeedsIntervention}
	E := event{phase: remediationv1alpha1.PhaseExpired}
	D := event{phase: remediationv1alpha1.PhaseDenied}
	PF := event{phase: remediationv1alpha1.PhaseFailed, probe: true}
	PE := event{phase: remediationv1alpha1.PhaseExpired, probe: true}
	PC := event{phase: remediationv1alpha1.PhaseCancelled, probe: true}
	PS := event{phase: remediationv1alpha1.PhaseSucceeded, probe: true}
	PRun := event{phase: remediationv1alpha1.PhasePRCreated, probe: true}
	PNoTime := event{phase: remediationv1alpha1.PhaseExpired, probe: true, noTime: true}
	for name, h := range map[string][]event{
		"trips after three failures":         {S, F, N, F},
		"a success closes it":                {F, F, F, F, M},
		"a success before the failures":      {M, S, F, N, F, F, F},
		"probes that fail keep it open":      {S, F, F, F, PF, PF, PF, PF, PF},
		"probes that end neither way":        {S, F, F, F, PE, PC, PE},
		"a probe that ends neither way last": {S, F, F, F, F, F, PF, PC},
		"a probe's success closes it":        {F, F, F, PE, PF, PS},
		"a running probe":                    {S, F, F, F, F, PE, PRun},
		"a probe without a transition time":  {S, F, F, F, F, F, F, PNoTime},
		"refusals and expiries are skipped":  {S, F, D, E, F, E, D, F, S, F, E, F, F, PE, F},
	} {
		t.Run(name, func(t *testing.T) {
			m := &memReader{}
			r := &RemediationRequestReconciler{BreakerCooldown: 30 * time.Minute}
			start := limitsNow
			last := start
			check := func(step string) {
				t.Helper()
				for _, after := range []time.Duration{time.Second, 10 * time.Minute, 20 * time.Minute, 29 * time.Minute,
					31 * time.Minute, 40 * time.Minute, 50 * time.Minute, 61 * time.Minute,
					5 * time.Hour} {
					now := last.Add(after)
					got, _ := decide(t, r, m, now)
					want, _ := decide(t, r, m.unclassified(), now)
					if got != want {
						t.Fatalf("%s, %v after the last end: decided %+v, want %+v as on every request", step, after,
							got, want)
					}
				}
			}
			for i, ev := range h {
				last = start.Add(time.Duration(i) * 7 * time.Minute)
				m.endRequest(i, ev, last)
				check(fmt.Sprintf("after request %d ended", i))
				_, s := decide(t, r, m, last)
				m.clear(s.stale)
				check(fmt.Sprintf("after the clearing that followed request %d", i))
			}
		})
	}
}

// TestBreakerReadsRequestsWithoutTheField checks that a request without status.breakerCounts (one that ended
// before the field existed and was not backfilled yet, or was edited by hand) is read (fail closed): three such
// failures open the breaker. With the field false they are not read, so the field is what bounds the read.
func TestBreakerReadsRequestsWithoutTheField(t *testing.T) {
	failures := func(counts *bool) []*remediationv1alpha1.RemediationRequest {
		out := make([]*remediationv1alpha1.RemediationRequest, 0, 3)
		for i, ago := range []time.Duration{3 * time.Minute, 2 * time.Minute, time.Minute} {
			o := otherRequest(fmt.Sprintf("old-%d", i), "uid-old", remediationv1alpha1.PhaseFailed, ago)
			o.Status.HoldsTarget = new(false)
			o.Status.BreakerCounts = counts
			out = append(out, o)
		}
		return out
	}
	t.Run("missing", func(t *testing.T) {
		r, _, rr, stub := limitsWorld(t, 20, failures(nil)...)
		res, got, err := reconcileLimits(t, r, rr)
		expectQueued(t, r, rr, res, err, got, stub, ReasonCircuitOpen)
	})
	t.Run(fieldFalse, func(t *testing.T) {
		r, _, rr, _ := limitsWorld(t, 20, failures(new(false))...)
		_, got, err := reconcileLimits(t, r, rr)
		expectAdmitted(t, err, got)
	})
}

// failingList wraps a reader so that a list whose field selector names field fails.
type failingList struct {
	client.Reader
	field string
}

func (f *failingList) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	o := &client.ListOptions{}
	o.ApplyOptions(opts)
	if o.FieldSelector != nil && strings.Contains(o.FieldSelector.String(), f.field) {
		return apierrors.NewTimeoutError("the list of the breaker's outcomes timed out", 1)
	}
	return f.Reader.List(ctx, list, opts...)
}

// TestBreakerListErrorLetsNothingThrough checks that when the breaker's list (status.breakerCounts!=false)
// cannot be read, although the other lists can and show free slots, nothing is let through and nothing is
// cleared.
func TestBreakerListErrorLetsNothingThrough(t *testing.T) {
	r, base, rr, _ := limitsWorld(t, 20, history("ok", 2, "uid-elsewhere", remediationv1alpha1.PhaseSucceeded)...)
	r.APIReader = &failingList{Reader: &apiServerReader{Reader: base}, field: countsField}
	_, got, err := reconcileLimits(t, r, rr)
	if !apierrors.IsTimeout(err) {
		t.Fatalf("err = %v, want the list's timeout", err)
	}
	if got.Status.Phase != "" {
		t.Fatalf("phase = %q, want none", got.Status.Phase)
	}
	older := &remediationv1alpha1.RemediationRequest{}
	if err := base.Get(context.Background(), types.NamespacedName{Namespace: requestNamespace, Name: "ok-0000"},
		older); err != nil {
		t.Fatal(err)
	}
	if !ptr.Deref(older.Status.BreakerCounts, false) {
		t.Fatal("a check that could not read the breaker's list cleared an outcome")
	}
}

// breakerCountsOf returns status.breakerCounts of the request name, as fieldTrue, fieldFalse or fieldUnset.
func breakerCountsOf(t *testing.T, c client.Client, name string) string {
	t.Helper()
	o := &remediationv1alpha1.RemediationRequest{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: requestNamespace, Name: name}, o); err != nil {
		t.Fatal(err)
	}
	if o.Status.BreakerCounts == nil {
		return fieldUnset
	}
	return strconv.FormatBool(*o.Status.BreakerCounts)
}

// TestBreakerClearing checks the clearing after a check of the limits on the API server: an older success is
// set to false and the newer kept; running the clearing again with the same stale read changes nothing and fails
// nothing (idempotent, invariant 5); a request that changed after the read is left as it is; and a write that
// fails leaves the decision as it was and the request to the next check.
func TestBreakerClearing(t *testing.T) {
	successes := func() []*remediationv1alpha1.RemediationRequest {
		return history("s", 2, "uid-elsewhere", remediationv1alpha1.PhaseSucceeded)
	}
	t.Run("cleared once, then nothing", func(t *testing.T) {
		r, base, rr, _ := limitsWorld(t, 20, successes()...)
		seen, err := r.readLimits(context.Background(), requestNamespace)
		if err != nil {
			t.Fatal(err)
		}
		stale := staleOutcomes(seen, r.breakerThreshold())
		if len(stale) != 1 || stale[0].name != "s-0000" {
			t.Fatalf("stale = %+v, want only the older success s-0000", stale)
		}
		_, got, err := reconcileLimits(t, r, rr)
		expectAdmitted(t, err, got)
		if a, b := breakerCountsOf(t, base, "s-0000"), breakerCountsOf(t, base, "s-0001"); a != fieldFalse || b != fieldTrue {
			t.Fatalf("breakerCounts = %s (older), %s (newer); want false, true", a, b)
		}
		// The same stale read once more: its resourceVersion is old, so the write conflicts and is skipped.
		r.clearBreakerCounts(context.Background(), requestNamespace, stale)
		if a := breakerCountsOf(t, base, "s-0000"); a != fieldFalse {
			t.Fatalf("breakerCounts of the older success = %s after a second clearing, want false", a)
		}
		seen, err = r.readLimits(context.Background(), requestNamespace)
		if err != nil {
			t.Fatal(err)
		}
		if stale := staleOutcomes(seen, r.breakerThreshold()); len(stale) != 0 {
			t.Fatalf("stale after the clearing = %+v, want none", stale)
		}
	})
	t.Run("a request that changed after the read is left as it is", func(t *testing.T) {
		r, base, _, _ := limitsWorld(t, 20, successes()...)
		seen, err := r.readLimits(context.Background(), requestNamespace)
		if err != nil {
			t.Fatal(err)
		}
		stale := staleOutcomes(seen, r.breakerThreshold())
		older := &remediationv1alpha1.RemediationRequest{}
		if err := base.Get(context.Background(), types.NamespacedName{Namespace: requestNamespace, Name: "s-0000"},
			older); err != nil {
			t.Fatal(err)
		}
		// It ended again after the read (a failure found later), so its outcome is newer and counts.
		r.Now = func() time.Time { return limitsNow }
		r.setPhase(older, remediationv1alpha1.PhaseFailed)
		if err := base.Status().Update(context.Background(), older); err != nil {
			t.Fatal(err)
		}
		r.clearBreakerCounts(context.Background(), requestNamespace, stale)
		if a := breakerCountsOf(t, base, "s-0000"); a != fieldTrue {
			t.Fatalf("breakerCounts = %s, want true: the write must not overwrite a newer state", a)
		}
	})
	t.Run("a failed write changes no decision", func(t *testing.T) {
		r, base, rr, _ := limitsWorld(t, 20, successes()...)
		writes := 0
		r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch,
				opts ...client.SubResourcePatchOption) error {
				writes++
				return errors.New("patch refused")
			},
		})
		_, got, err := reconcileLimits(t, r, rr)
		expectAdmitted(t, err, got)
		if writes != 1 {
			t.Fatalf("clearing writes = %d, want 1", writes)
		}
		if a := breakerCountsOf(t, base, "s-0000"); a != fieldTrue {
			t.Fatalf("breakerCounts = %s after a failed write, want true (read again at the next check)", a)
		}
	})
}

// TestClassifyBreaker checks the value the status write that ends a request gives status.breakerCounts: true for
// an outcome the breaker counts and for a probe that ended any other way, false for any other end, and nothing
// while the request has not ended.
func TestClassifyBreaker(t *testing.T) {
	for _, c := range []struct {
		phase  remediationv1alpha1.RemediationPhase
		probe  bool
		digest bool
		want   string
	}{
		{remediationv1alpha1.PhaseMerged, false, true, fieldTrue},
		{remediationv1alpha1.PhaseSucceeded, false, true, fieldTrue},
		{remediationv1alpha1.PhaseFailed, false, true, fieldTrue},
		{remediationv1alpha1.PhaseNeedsIntervention, false, true, fieldTrue},
		{remediationv1alpha1.PhaseNeedsIntervention, false, false, fieldFalse},
		{remediationv1alpha1.PhaseDenied, false, false, fieldFalse},
		{remediationv1alpha1.PhaseExpired, false, true, fieldFalse},
		{remediationv1alpha1.PhaseCancelled, false, true, fieldFalse},
		{remediationv1alpha1.PhaseSuperseded, false, true, fieldFalse},
		{remediationv1alpha1.PhaseExpired, true, true, fieldTrue},
		{remediationv1alpha1.PhaseDenied, true, true, fieldTrue},
		{remediationv1alpha1.PhaseSuperseded, true, true, fieldTrue},
		{remediationv1alpha1.PhasePRCreated, true, true, fieldUnset},
		{remediationv1alpha1.PhasePending, false, false, fieldUnset},
	} {
		t.Run(fmt.Sprintf("%s probe %v digest %v", c.phase, c.probe, c.digest), func(t *testing.T) {
			o := otherRequest("o", "uid-o", c.phase, time.Minute)
			o.Status.BreakerProbe = c.probe
			if !c.digest {
				o.Status.PlanDigest = ""
			}
			r := &RemediationRequestReconciler{Now: func() time.Time { return limitsNow }}
			o.Status.Phase = remediationv1alpha1.PhasePolicyChecked
			r.setPhase(o, c.phase)
			got := fieldUnset
			if o.Status.BreakerCounts != nil {
				got = strconv.FormatBool(*o.Status.BreakerCounts)
			}
			if got != c.want {
				t.Fatalf("breakerCounts = %s, want %s", got, c.want)
			}
		})
	}
}

// TestBackfillBreaker checks the upgrade: a request that ended before status.breakerCounts existed gets it
// once from its status when it is reconciled, so the breaker's list stops reading one it never needs; one that
// has not ended gets none.
func TestBackfillBreaker(t *testing.T) {
	old := func(name string, phase remediationv1alpha1.RemediationPhase) *remediationv1alpha1.RemediationRequest {
		o := otherRequest(name, "uid-"+name, phase, time.Hour)
		o.Status.HoldsTarget = new(false)
		return o
	}
	failed, expired, running := old("failed", remediationv1alpha1.PhaseFailed),
		old("expired", remediationv1alpha1.PhaseExpired), old("running", remediationv1alpha1.PhasePRCreated)
	running.Status.HoldsTarget = new(true)
	r, base, _, _ := limitsWorld(t, 20, failed, expired, running)
	for _, o := range []*remediationv1alpha1.RemediationRequest{failed, expired} {
		if _, _, err := reconcileLimits(t, r, o); err != nil {
			t.Fatal(err)
		}
	}
	o := &remediationv1alpha1.RemediationRequest{}
	if err := base.Get(context.Background(), client.ObjectKeyFromObject(running), o); err != nil {
		t.Fatal(err)
	}
	if err := r.backfillBreaker(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"failed": fieldTrue, "expired": fieldFalse, "running": fieldUnset} {
		if got := breakerCountsOf(t, base, name); got != want {
			t.Fatalf("%s: breakerCounts = %s, want %s", name, got, want)
		}
	}
}
