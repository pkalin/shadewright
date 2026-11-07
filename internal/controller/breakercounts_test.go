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
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

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

// clear does what clearStale does on the API server, for the items of m: status.breakerCounts false and
// status.breakerClearedBy on at most breakerClearsPerCheck of c.stale, oldest first.
func (m *memReader) clear(c clearing) {
	for i, e := range c.stale {
		if i == breakerClearsPerCheck {
			return
		}
		for _, o := range m.items {
			if o.Name == e.name {
				o.Status.BreakerCounts = new(false)
				o.Status.BreakerClearedBy = c.by
			}
		}
	}
}

// remove deletes the request name from m as its finalizer does: the requests it cleared are read again first
// (restoreCleared).
func (m *memReader) remove(name string) {
	kept := m.items[:0]
	for _, o := range m.items {
		if o.Status.BreakerClearedBy == name {
			o.Status.BreakerCounts = new(true)
			o.Status.BreakerClearedBy = ""
		}
		if o.Name != name {
			kept = append(kept, o)
		}
	}
	m.items = kept
}

// fail moves the request name to Failed at at, as setPhase writes it.
func (m *memReader) fail(name string, at time.Time) {
	for _, o := range m.items {
		if o.Name == name {
			r := &RemediationRequestReconciler{Now: func() time.Time { return at }}
			r.setPhase(o, remediationv1alpha1.PhaseFailed)
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
		x.Status.BreakerClearedBy = ""
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

func decide(t *testing.T, r *RemediationRequestReconciler, m client.Reader, now time.Time) (decision, limitsState) {
	t.Helper()
	r.APIReader = m
	r.Now = func() time.Time { return now }
	s, err := r.checkLimits(context.Background(), pendingRequest())
	if err != nil {
		t.Fatal(err)
	}
	return decision{reason: s.reason, circuit: s.circuit, probing: s.probing}, s
}

// TestBreakerReadsBounded checks the bound of #390: after 10,000 requests ended in a namespace (successes final
// and not, failures, refusals, expiries and probes), each classified by the status write that ended it and the
// outcomes before the latest Succeeded cleared after the checks of the limits that followed, a check of the
// limits reads only the latest Succeeded and what ended after it, and decides as a read of every request does.
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
	for i := range n {
		m.endRequest(i, pattern[i%len(pattern)], start.Add(time.Duration(i)*time.Minute))
		if i%50 == 49 {
			// A check of the limits runs now and then, and clears what it found no longer needed.
			_, s := decide(t, r, m, start.Add(time.Duration(i)*time.Minute))
			m.clear(s.clear)
		}
	}
	m.returned = 0
	got, s := decide(t, r, m, limitsNow)
	if m.returned > len(pattern) {
		t.Fatalf("a check of the limits read %d of %d ended requests, want at most %d (the latest Succeeded and "+
			"what ended after it)", m.returned, n, len(pattern))
	}
	if len(s.clear.stale) != 0 {
		t.Fatalf("%d requests left to clear after the history, want 0", len(s.clear.stale))
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

// TestBreakerDecidesAsBefore checks on representative histories, with the default threshold and a raised one,
// that the watermark never changes a decision: after each request ends, after the clearing that follows, and
// after each final success is deleted again (its finalizer restores what it cleared), the breaker decides at
// several times exactly as it does when it reads every request (all without status.breakerCounts, as before
// #390).
func TestBreakerDecidesAsBefore(t *testing.T) {
	S := event{phase: remediationv1alpha1.PhaseSucceeded}
	M := event{phase: remediationv1alpha1.PhaseMerged}
	V := event{phase: remediationv1alpha1.PhaseVerifying}
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
	histories := map[string][]event{
		"trips after three failures":           {S, F, N, F},
		"a success closes it":                  {F, F, F, F, S},
		"a merge closes it but clears nothing": {S, F, F, F, M, F, V},
		"a success before the failures":        {M, S, F, N, F, F, F, F, F},
		"probes that fail keep it open":        {S, F, F, F, PF, PF, PF, PF, PF},
		"probes that end neither way":          {S, F, F, F, PE, PC, PE},
		"a probe that ends neither way last":   {S, F, F, F, F, F, PF, PC},
		"a probe's success closes it":          {F, F, F, PE, PF, PS, F, F},
		"a running probe":                      {S, F, F, F, F, PE, PRun},
		"a probe without a transition time":    {S, F, F, F, F, F, F, PNoTime},
		"refusals and expiries are skipped":    {S, F, D, E, F, E, D, F, S, F, E, F, F, PE, F, F, F},
		"successes between rows":               {F, F, F, S, F, F, S, F, F, F, F, F, S, F},
	}
	for name, h := range histories {
		for _, threshold := range []int{0, 5} {
			t.Run(fmt.Sprintf("%s, threshold %d", name, threshold), func(t *testing.T) {
				m := &memReader{}
				r := &RemediationRequestReconciler{BreakerCooldown: 30 * time.Minute, BreakerThreshold: threshold}
				last := limitsNow
				check := func(step string) {
					t.Helper()
					for _, after := range []time.Duration{time.Second, 10 * time.Minute, 20 * time.Minute,
						29 * time.Minute, 31 * time.Minute, 40 * time.Minute, 50 * time.Minute, 61 * time.Minute,
						5 * time.Hour} {
						now := last.Add(after)
						got, _ := decide(t, r, m, now)
						want, _ := decide(t, r, m.unclassified(), now)
						if got != want {
							t.Fatalf("%s, %v after the last end: decided %+v, want %+v as on every request", step,
								after, got, want)
						}
					}
				}
				for i, ev := range h {
					last = limitsNow.Add(time.Duration(i) * 7 * time.Minute)
					m.endRequest(i, ev, last)
					check(fmt.Sprintf("after request %d ended", i))
					_, s := decide(t, r, m, last)
					m.clear(s.clear)
					check(fmt.Sprintf("after the clearing that followed request %d", i))
				}
				// A merged request that is not final yet turns into Failed later (#92): the status write that
				// moves it sets status.breakerCounts again, and nothing it could have cleared is lost.
				for i, ev := range h {
					if ev.phase == remediationv1alpha1.PhaseSucceeded || !slices.Contains(successPhases, ev.phase) {
						continue
					}
					last = last.Add(time.Minute)
					m.fail(fmt.Sprintf("h-%05d", i), last)
					check(fmt.Sprintf("after request %d (%s) failed later", i, ev.phase))
					_, s := decide(t, r, m, last)
					m.clear(s.clear)
					check(fmt.Sprintf("after the clearing that followed the failure of request %d", i))
				}
				for i, ev := range slices.Backward(h) {
					if ev.phase != remediationv1alpha1.PhaseSucceeded {
						continue
					}
					m.remove(fmt.Sprintf("h-%05d", i))
					check(fmt.Sprintf("after request %d (Succeeded) was deleted", i))
					_, s := decide(t, r, m, last)
					m.clear(s.clear)
					check(fmt.Sprintf("after the clearing that followed the deletion of request %d", i))
				}
			})
		}
	}
}

// TestStaleOutcomes checks what one read lets the executor clear: only outcomes that ended strictly before the
// latest request in Succeeded, oldest first, named by it; nothing without a request in Succeeded (a long row of
// failures, or one closed by Merged, which may still turn into Failed), nothing ordered by a success that is
// being deleted, and nothing that is being deleted or cannot be ordered.
func TestStaleOutcomes(t *testing.T) {
	at := func(m int) time.Time { return limitsNow.Add(time.Duration(m) * time.Minute) }
	e := func(name string, outcome outcomeClass, final bool, m int) limitsEntry {
		return limitsEntry{name: name, uid: types.UID("uid-" + name), outcome: outcome, final: final, at: at(m),
			counts: true}
	}
	seen := func(es ...limitsEntry) map[types.UID]limitsEntry {
		out := map[types.UID]limitsEntry{}
		for _, x := range es {
			out[x.uid] = x
		}
		return out
	}
	names := func(c clearing) []string {
		out := make([]string, 0, len(c.stale))
		for _, x := range c.stale {
			out = append(out, x.name)
		}
		return out
	}
	deleting := e("s2", outcomeSuccess, true, 9)
	deleting.deleting = true
	noTime := e("f0", outcomeFailure, false, 0)
	noTime.at = time.Time{}
	gone := e("f9", outcomeFailure, false, 1)
	gone.deleting = true
	for name, c := range map[string]struct {
		seen map[types.UID]limitsEntry
		by   string
		want []string
	}{
		"oldest first, by the latest Succeeded": {seen(e("f3", outcomeFailure, false, 3), e("s1", outcomeSuccess, true, 1),
			e("f2", outcomeFailure, false, 2), e("s4", outcomeSuccess, true, 4), e("f5", outcomeFailure, false, 5)),
			"s4", []string{"s1", "f2", "f3"}},
		"a long row without a success": {seen(e("f1", outcomeFailure, false, 1), e("f2", outcomeFailure, false, 2),
			e("f3", outcomeFailure, false, 3), e("f4", outcomeFailure, false, 4), e("f5", outcomeFailure, false, 5)),
			"", nil},
		"a merge is not final":                   {seen(e("f1", outcomeFailure, false, 1), e("m2", outcomeSuccess, false, 2)), "", nil},
		"a success being deleted clears nothing": {seen(e("f1", outcomeFailure, false, 1), deleting), "", nil},
		"neither unordered nor deleting ones":    {seen(noTime, gone, e("s2", outcomeSuccess, true, 2)), "s2", nil},
	} {
		t.Run(name, func(t *testing.T) {
			got := staleOutcomes(c.seen)
			if got.by != c.by || !slices.Equal(names(got), c.want) {
				t.Fatalf("staleOutcomes = by %q %v, want by %q %v", got.by, names(got), c.by, c.want)
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

// successHistory is S1 (Succeeded, 50 minutes before limitsNow), three failures (30, 20 and 10 minutes before)
// and S2 (Succeeded, 5 minutes before), each classified as the status write that ended it does; S2 has the
// finalizer, as every request that opened a pull request does.
func successHistory() []*remediationv1alpha1.RemediationRequest {
	end := func(name string, phase remediationv1alpha1.RemediationPhase, ago time.Duration) *remediationv1alpha1.RemediationRequest {
		o := otherRequest(name, "uid-t-"+name, phase, ago)
		o.Status.HoldsTarget = new(false)
		classifyBreaker(o)
		return o
	}
	s2 := end("s2", remediationv1alpha1.PhaseSucceeded, 5*time.Minute)
	s2.Finalizers = []string{FinalizerName}
	return []*remediationv1alpha1.RemediationRequest{
		end("s1", remediationv1alpha1.PhaseSucceeded, 50*time.Minute),
		end("f1", remediationv1alpha1.PhaseFailed, 30*time.Minute),
		end("f2", remediationv1alpha1.PhaseNeedsIntervention, 20*time.Minute),
		end("f3", remediationv1alpha1.PhaseFailed, 10*time.Minute), s2,
	}
}

// limitsReason is the reason the limits give a new request on its own target now (empty: let through).
func limitsReason(t *testing.T, r *RemediationRequestReconciler) string {
	t.Helper()
	s, err := r.checkLimits(context.Background(), pendingRequest())
	if err != nil {
		t.Fatal(err)
	}
	return s.reason
}

// TestBreakerRestoredWhenTheSuccessIsDeleted checks that deleting the request in Succeeded whose success cleared
// older outcomes brings them back: S1, three failures, S2 in Succeeded; the check after S2 clears S1 and the
// failures (naming S2); deleting S2 runs its finalizer, which reads them again before it goes, so the breaker is
// open again, as it was before #390 when every outcome was read.
func TestBreakerRestoredWhenTheSuccessIsDeleted(t *testing.T) {
	r, base, rr, _ := limitsWorld(t, 20, successHistory()...)
	r.BreakerCooldown = 30 * time.Minute
	_, got, err := reconcileLimits(t, r, rr)
	expectAdmitted(t, err, got)
	for _, name := range []string{"s1", "f1", "f2", "f3"} {
		if c := breakerCountsOf(t, base, name); c != fieldFalse {
			t.Fatalf("%s: breakerCounts = %s after the check, want false", name, c)
		}
	}
	if c := breakerCountsOf(t, base, "s2"); c != fieldTrue {
		t.Fatalf("s2: breakerCounts = %s, want true", c)
	}
	if reason := limitsReason(t, r); reason != "" {
		t.Fatalf("limits = %q with S2 recorded, want the breaker closed", reason)
	}
	s2 := &remediationv1alpha1.RemediationRequest{}
	key := types.NamespacedName{Namespace: requestNamespace, Name: "s2"}
	if err := base.Get(context.Background(), key, s2); err != nil {
		t.Fatal(err)
	}
	if err := base.Delete(context.Background(), s2); err != nil {
		t.Fatal(err)
	}
	if _, err := reconcileLimitsGone(t, r, s2); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"s1", "f1", "f2", "f3"} {
		if c := breakerCountsOf(t, base, name); c != fieldTrue {
			t.Fatalf("%s: breakerCounts = %s after S2 was deleted, want true", name, c)
		}
	}
	if reason := limitsReason(t, r); reason != ReasonCircuitOpen {
		t.Fatalf("limits = %q after S2 was deleted, want %s", reason, ReasonCircuitOpen)
	}
}

// reconcileLimitsGone reconciles rr, which may be gone afterwards (its finalizer removed).
func reconcileLimitsGone(t *testing.T, r *RemediationRequestReconciler, rr *remediationv1alpha1.RemediationRequest) (
	bool, error) {
	t.Helper()
	ctx := context.Background()
	_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(rr)})
	gerr := r.Get(ctx, client.ObjectKeyFromObject(rr), &remediationv1alpha1.RemediationRequest{})
	if gerr != nil && !apierrors.IsNotFound(gerr) {
		t.Fatal(gerr)
	}
	return gerr == nil, err
}

// TestFinalizerKeptUntilRestored checks that a finalizer that cannot restore the outcomes its request cleared
// (the list fails) stays, so the deletion is retried and nothing the breaker needs is lost, and that the retry
// restores them and lets the request go.
func TestFinalizerKeptUntilRestored(t *testing.T) {
	r, base, rr, _ := limitsWorld(t, 20, successHistory()...)
	_, got, err := reconcileLimits(t, r, rr)
	expectAdmitted(t, err, got)
	s2 := &remediationv1alpha1.RemediationRequest{}
	if err := base.Get(context.Background(), types.NamespacedName{Namespace: requestNamespace, Name: "s2"}, s2); err != nil {
		t.Fatal(err)
	}
	if err := base.Delete(context.Background(), s2); err != nil {
		t.Fatal(err)
	}
	working := r.APIReader
	r.APIReader = &failingList{Reader: working, field: clearedField}
	if present, err := reconcileLimitsGone(t, r, s2); err == nil || !present {
		t.Fatalf("err = %v, present = %v; want an error and the finalizer kept", err, present)
	}
	if c := breakerCountsOf(t, base, "f3"); c != fieldFalse {
		t.Fatalf("f3: breakerCounts = %s, want still false", c)
	}
	r.APIReader = working
	if present, err := reconcileLimitsGone(t, r, s2); err != nil || present {
		t.Fatalf("err = %v, present = %v; want the retry to restore and remove the finalizer", err, present)
	}
	if c := breakerCountsOf(t, base, "f3"); c != fieldTrue {
		t.Fatalf("f3: breakerCounts = %s after the retry, want true", c)
	}
}

// TestDeletingExpiredCountsAgainstTheCap checks the guarantee of the cap for a request being deleted whose
// finalizer may close a pull request although it holds no lock (Expired, which the breaker does not need, so
// status.breakerCounts is false): the finalizer marks it before anything else, so while the finalizer is not
// done (here: the restore fails) the limits read it and count it against a cap of one.
func TestDeletingExpiredCountsAgainstTheCap(t *testing.T) {
	expired := otherRequest("expired", "uid-elsewhere", remediationv1alpha1.PhaseExpired, time.Hour)
	expired.Finalizers = []string{FinalizerName}
	expired.Status.HoldsTarget = new(false)
	classifyBreaker(expired)
	r, base, rr, _ := limitsWorld(t, 1, expired)
	if reason := limitsReason(t, r); reason != "" {
		t.Fatalf("limits = %q before the deletion, want none", reason)
	}
	if err := base.Delete(context.Background(), expired); err != nil {
		t.Fatal(err)
	}
	working := r.APIReader
	r.APIReader = &failingList{Reader: working, field: clearedField}
	if present, err := reconcileLimitsGone(t, r, expired); err == nil || !present {
		t.Fatalf("err = %v, present = %v; want the finalizer kept", err, present)
	}
	r.APIReader = working
	res, got, err := reconcileLimits(t, r, rr)
	expectHeld(t, res, err, got, ReasonConcurrencyLimit)
}

// TestClearingAfterTheDecision checks that the clearing runs after the decision is written and outside the
// admission lock, so no admission waits for it, and that a clearing already running elsewhere is not repeated.
func TestClearingAfterTheDecision(t *testing.T) {
	t.Run("after the write, outside the lock", func(t *testing.T) {
		r, base, rr, _ := limitsWorld(t, 20, successHistory()...)
		var patches, locked, undecided int
		r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch,
				opts ...client.SubResourcePatchOption) error {
				patches++
				if r.admitMu.TryLock() {
					r.admitMu.Unlock()
				} else {
					locked++
				}
				d := &remediationv1alpha1.RemediationRequest{}
				if err := c.Get(ctx, client.ObjectKeyFromObject(rr), d); err != nil || d.Status.Phase != remediationv1alpha1.PhasePolicyChecked {
					undecided++
				}
				return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
			},
		})
		_, got, err := reconcileLimits(t, r, rr)
		expectAdmitted(t, err, got)
		if patches != 4 || locked != 0 || undecided != 0 {
			t.Fatalf("clearing patches = %d (want 4), under the admission lock = %d, before the decision = %d (want 0)",
				patches, locked, undecided)
		}
	})
	t.Run("one clearer at a time", func(t *testing.T) {
		r, base, rr, _ := limitsWorld(t, 20, successHistory()...)
		r.clearMu.Lock() // another clearing runs
		_, got, err := reconcileLimits(t, r, rr)
		r.clearMu.Unlock()
		expectAdmitted(t, err, got)
		if c := breakerCountsOf(t, base, "f1"); c != fieldTrue {
			t.Fatalf("f1: breakerCounts = %s, want true: a second clearer must not repeat the writes", c)
		}
	})
}

// TestClearingErrors checks that a clearing write refused for good (Forbidden) is counted in
// shadewright_circuit_breaker_clear_errors_total and a transient one is not, that neither changes the decision,
// and that the outcome stays read.
func TestClearingErrors(t *testing.T) {
	for name, c := range map[string]struct {
		err     error
		counted float64
	}{
		"forbidden": {apierrors.NewForbidden(remediationv1alpha1.GroupVersion.WithResource("remediationrequests").GroupResource(),
			"f1", errors.New("no patch")), 1},
		"timeout": {apierrors.NewServerTimeout(remediationv1alpha1.GroupVersion.WithResource("remediationrequests").GroupResource(),
			"patch", 1), 0},
	} {
		t.Run(name, func(t *testing.T) {
			r, base, rr, _ := limitsWorld(t, 20, successHistory()...)
			writes := 0
			r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
				SubResourcePatch: func(context.Context, client.Client, string, client.Object, client.Patch,
					...client.SubResourcePatchOption) error {
					writes++
					return c.err
				},
			})
			before := testutil.ToFloat64(breakerClearErrors)
			_, got, err := reconcileLimits(t, r, rr)
			expectAdmitted(t, err, got)
			if d := testutil.ToFloat64(breakerClearErrors) - before; d != c.counted {
				t.Fatalf("clear errors rose by %v, want %v", d, c.counted)
			}
			if writes != 1 {
				t.Fatalf("clearing writes = %d, want 1 (the first error stops the clearing)", writes)
			}
			if c := breakerCountsOf(t, base, "s1"); c != fieldTrue {
				t.Fatalf("s1: breakerCounts = %s after a failed write, want true", c)
			}
		})
	}
}

// TestClearingIsIdempotent checks that the clearing writes with the resourceVersion it read: running it again
// with the same read fails nothing and changes nothing, a request that changed after the read is left as it is,
// and a success that was deleted after the read clears nothing.
func TestClearingIsIdempotent(t *testing.T) {
	read := func(t *testing.T, r *RemediationRequestReconciler) clearing {
		t.Helper()
		seen, err := r.readLimits(context.Background(), requestNamespace)
		if err != nil {
			t.Fatal(err)
		}
		return staleOutcomes(seen)
	}
	t.Run("again with the same read", func(t *testing.T) {
		r, base, _, _ := limitsWorld(t, 20, successHistory()...)
		c := read(t, r)
		r.clearStale(context.Background(), requestNamespace, c)
		r.clearStale(context.Background(), requestNamespace, c)
		if f, s := breakerCountsOf(t, base, "f1"), breakerCountsOf(t, base, "s2"); f != fieldFalse || s != fieldTrue {
			t.Fatalf("breakerCounts = %s (f1), %s (s2); want false, true", f, s)
		}
		if again := read(t, r); len(again.stale) != 0 {
			t.Fatalf("left to clear = %d, want 0", len(again.stale))
		}
	})
	t.Run("a request that changed after the read", func(t *testing.T) {
		r, base, _, _ := limitsWorld(t, 20, successHistory()...)
		c := read(t, r)
		f1 := &remediationv1alpha1.RemediationRequest{}
		if err := base.Get(context.Background(), types.NamespacedName{Namespace: requestNamespace, Name: "f1"}, f1); err != nil {
			t.Fatal(err)
		}
		f1.Status.BreakerProbe = true
		if err := base.Status().Update(context.Background(), f1); err != nil {
			t.Fatal(err)
		}
		r.clearStale(context.Background(), requestNamespace, c)
		if f := breakerCountsOf(t, base, "f1"); f != fieldTrue {
			t.Fatalf("f1: breakerCounts = %s, want true: a write must not overwrite a newer state", f)
		}
	})
	t.Run("a success deleted after the read", func(t *testing.T) {
		r, base, _, _ := limitsWorld(t, 20, successHistory()...)
		c := read(t, r)
		s2 := &remediationv1alpha1.RemediationRequest{}
		if err := base.Get(context.Background(), types.NamespacedName{Namespace: requestNamespace, Name: "s2"}, s2); err != nil {
			t.Fatal(err)
		}
		if err := base.Delete(context.Background(), s2); err != nil {
			t.Fatal(err)
		}
		r.clearStale(context.Background(), requestNamespace, c)
		if f := breakerCountsOf(t, base, "f1"); f != fieldTrue {
			t.Fatalf("f1: breakerCounts = %s, want true: a success being deleted clears nothing", f)
		}
	})
}

// TestBackfillOneWrite checks the upgrade: a request that ended before status.holdsTarget and
// status.breakerCounts existed gets both in one status write when it is reconciled, one that has not ended gets
// no breakerCounts, and a conflict on that write is retried shortly without a reconcile error.
func TestBackfillOneWrite(t *testing.T) {
	old := func(name string, phase remediationv1alpha1.RemediationPhase) *remediationv1alpha1.RemediationRequest {
		return otherRequest(name, "uid-"+name, phase, time.Hour)
	}
	failed, expired := old("failed", remediationv1alpha1.PhaseFailed), old("expired", remediationv1alpha1.PhaseExpired)
	t.Run("one write", func(t *testing.T) {
		r, base, _, _ := limitsWorld(t, 20, failed, expired)
		updates := 0
		r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object,
				opts ...client.SubResourceUpdateOption) error {
				updates++
				return c.SubResource(sub).Update(ctx, obj, opts...)
			},
		})
		for _, o := range []*remediationv1alpha1.RemediationRequest{failed, expired} {
			if _, _, err := reconcileLimits(t, r, o); err != nil {
				t.Fatal(err)
			}
		}
		if updates != 2 {
			t.Fatalf("status writes = %d, want 2 (one per request)", updates)
		}
		for name, want := range map[string]string{"failed": fieldTrue, "expired": fieldFalse} {
			if got := breakerCountsOf(t, base, name); got != want {
				t.Fatalf("%s: breakerCounts = %s, want %s", name, got, want)
			}
		}
	})
	t.Run("not ended", func(t *testing.T) {
		running := old("running", remediationv1alpha1.PhasePRCreated)
		r, base, _, _ := limitsWorld(t, 20, running)
		o := &remediationv1alpha1.RemediationRequest{}
		if err := base.Get(context.Background(), client.ObjectKeyFromObject(running), o); err != nil {
			t.Fatal(err)
		}
		if err := r.backfill(context.Background(), o); err != nil {
			t.Fatal(err)
		}
		if got := breakerCountsOf(t, base, "running"); got != fieldUnset {
			t.Fatalf("breakerCounts = %s, want unset", got)
		}
	})
	t.Run("conflict", func(t *testing.T) {
		r, base, _, _ := limitsWorld(t, 20, old("failed", remediationv1alpha1.PhaseFailed))
		r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
			SubResourceUpdate: func(context.Context, client.Client, string, client.Object, ...client.SubResourceUpdateOption) error {
				return apierrors.NewConflict(remediationv1alpha1.GroupVersion.WithResource("remediationrequests").GroupResource(),
					"failed", errors.New("changed"))
			},
		})
		res, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{
			Namespace: requestNamespace, Name: "failed"}})
		if err != nil || res.RequeueAfter != backfillRetryDelay {
			t.Fatalf("Reconcile = %+v, %v; want a retry after %v and no error", res, err, backfillRetryDelay)
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
