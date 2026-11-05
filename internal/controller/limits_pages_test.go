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
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/fields"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
)

// apiServerReader serves RemediationRequest lists from base the way the API server does: it applies a field
// selector on the CRD's selectable fields (status.phase, status.breakerProbe, status.holdsTarget; a missing
// value is the empty one, as limits_selector_envtest_test.go shows), and with a limit it returns at most Limit items, sorted by
// name, and a continue token for the rest. The fake client supports neither != selectors nor pages. It
// records each call, and with expire set it answers every continued call with the error the API server gives
// for a compacted snapshot.
type apiServerReader struct {
	client.Reader
	expire bool

	mu    sync.Mutex
	calls []listCall
}

// listCall is one list the reader served.
type listCall struct {
	selector string
	limit    int64
	names    []string
}

func (p *apiServerReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	o := &client.ListOptions{}
	o.ApplyOptions(opts)
	rl, ok := list.(*remediationv1alpha1.RemediationRequestList)
	if !ok {
		return p.Reader.List(ctx, list, opts...)
	}
	if o.Continue != "" && p.expire {
		return apierrors.NewResourceExpired("the provided continue parameter is too old")
	}
	if err := p.Reader.List(ctx, rl, client.InNamespace(o.Namespace)); err != nil {
		return err
	}
	sel := fields.Everything()
	if o.FieldSelector != nil {
		sel = o.FieldSelector
	}
	rl.Items = slices.DeleteFunc(rl.Items, func(i remediationv1alpha1.RemediationRequest) bool {
		lock := ""
		if i.Status.HoldsTarget != nil {
			lock = strconv.FormatBool(*i.Status.HoldsTarget)
		}
		return !sel.Matches(fields.Set{phaseField: string(i.Status.Phase),
			probeField: strconv.FormatBool(i.Status.BreakerProbe), lockField: lock})
	})
	slices.SortFunc(rl.Items, func(a, b remediationv1alpha1.RemediationRequest) int {
		return strings.Compare(a.Name, b.Name)
	})
	start := 0
	if o.Continue != "" {
		var err error
		if start, err = strconv.Atoi(o.Continue); err != nil {
			return apierrors.NewBadRequest("bad continue token")
		}
	}
	end := len(rl.Items)
	rl.Continue = ""
	if o.Limit > 0 && int64(end-start) > o.Limit {
		end = start + int(o.Limit)
		rl.Continue = strconv.Itoa(end)
	}
	rl.Items = rl.Items[start:end]
	call := listCall{selector: sel.String(), limit: o.Limit}
	for _, i := range rl.Items {
		call.names = append(call.names, i.Name)
	}
	p.mu.Lock()
	p.calls = append(p.calls, call)
	p.mu.Unlock()
	return nil
}

// read returns the names of all requests the reader returned, and whether a list needed more than one page.
func (p *apiServerReader) read() (names []string, paged bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	pages := map[string]int{}
	for _, c := range p.calls {
		names = append(names, c.names...)
		pages[c.selector]++
	}
	for _, n := range pages {
		paged = paged || n > 1
	}
	return names, paged
}

// expectBoundedPages checks that every list call asked for at most limitsPageSize requests and got no more,
// with a field selector, and, when paged, that some list was followed past its first page.
func (p *apiServerReader) expectBoundedPages(t *testing.T, paged bool) {
	t.Helper()
	p.mu.Lock()
	for i, c := range p.calls {
		if c.limit <= 0 || c.limit > limitsPageSize {
			t.Fatalf("list call %d asked for limit %d, want 1..%d", i, c.limit, limitsPageSize)
		}
		if len(c.names) > limitsPageSize {
			t.Fatalf("list call %d returned %d requests, more than %d", i, len(c.names), limitsPageSize)
		}
		if c.selector == "" {
			t.Fatalf("list call %d had no field selector: it read every request", i)
		}
	}
	p.mu.Unlock()
	if _, got := p.read(); got != paged {
		t.Fatalf("a list followed over pages = %v, want %v", got, paged)
	}
}

// history is n requests on target named prefix-NNNN, in the phases given in turn, whose phase changed n-i
// hours before limitsNow, so later names are newer. Requests in a phase past Pending have a plan digest; those in
// an ended phase have released the target lock (status.holdsTarget false), as the executor writes once their
// pull request is known closed or merged.
func history(prefix string, n int, target string,
	phases ...remediationv1alpha1.RemediationPhase) []*remediationv1alpha1.RemediationRequest {
	out := make([]*remediationv1alpha1.RemediationRequest, 0, n)
	for i := range n {
		o := otherRequest(fmt.Sprintf("%s-%04d", prefix, i), target, phases[i%len(phases)],
			time.Duration(n-i)*time.Hour)
		if ended(o.Status.Phase) {
			o.Status.HoldsTarget = new(false)
		}
		out = append(out, o)
	}
	return out
}

// refusals are the ended phases the limits never read unless the request is a probe.
var refusals = []remediationv1alpha1.RemediationPhase{remediationv1alpha1.PhaseDenied,
	remediationv1alpha1.PhaseCancelled, remediationv1alpha1.PhaseSuperseded}

// TestLimitsDoNotReadRefusals checks the bound of #293: however many requests ended Denied, Cancelled or
// Superseded without holding the lock (status.holdsTarget false), on the same target included, a check of the
// limits reads none of them, for the lock or anything else; a probe among them is still read.
func TestLimitsDoNotReadRefusals(t *testing.T) {
	const n = 3 * limitsPageSize
	others := history("refused", n, limitsTarget, refusals...)
	probe := otherRequest("refused-probe", "uid-probe", remediationv1alpha1.PhaseDenied, time.Minute)
	probe.Status.BreakerProbe = true
	probe.Status.HoldsTarget = new(false)
	others = append(others, probe)
	r, base, rr, _ := limitsWorld(t, 1, others...)
	p := &apiServerReader{Reader: base}
	r.APIReader = p
	_, got, err := reconcileLimits(t, r, rr)
	expectAdmitted(t, err, got)
	p.expectBoundedPages(t, false)
	names, _ := p.read()
	slices.Sort(names)
	// One check of the limits runs (under the lock, after the policy check). rr itself is listed twice (no
	// phase yet, and no status.holdsTarget, which the lock's list reads), then skipped.
	if want := []string{"refused-probe", rr.Name, rr.Name}; !slices.Equal(names, want) {
		t.Fatalf("read %d requests %v, want only %v", len(names), names, want)
	}
}

// TestLimitsReadInBoundedPages checks that the lists the limits read are followed over pages of at most
// limitsPageSize, and that what is on a later page still counts.
func TestLimitsReadInBoundedPages(t *testing.T) {
	const n = 2*limitsPageSize + 50
	t.Run("ended requests on the same target are not counted", func(t *testing.T) {
		others := history("ended", n, limitsTarget, remediationv1alpha1.PhaseSucceeded,
			remediationv1alpha1.PhaseMerged, remediationv1alpha1.PhaseExpired)
		r, base, rr, _ := limitsWorld(t, 1, others...)
		p := &apiServerReader{Reader: base}
		r.APIReader = p
		_, got, err := reconcileLimits(t, r, rr)
		expectAdmitted(t, err, got)
		p.expectBoundedPages(t, true)
	})
	t.Run("an active request on a later page locks the target", func(t *testing.T) {
		// Requests without a plan digest are not active, but are listed with the ones that can be.
		others := append(history("new", n, "uid-elsewhere", remediationv1alpha1.PhasePending),
			otherRequest("zz-active", limitsTarget, remediationv1alpha1.PhasePRCreated, time.Minute))
		r, base, rr, _ := limitsWorld(t, 20, others...)
		p := &apiServerReader{Reader: base}
		r.APIReader = p
		res, got, err := reconcileLimits(t, r, rr)
		expectHeld(t, res, err, got, ReasonTargetLocked)
		p.expectBoundedPages(t, true)
	})
	t.Run("failures on later pages trip the breaker", func(t *testing.T) {
		// Old failures, then a success, then three failures on the second and third page of the failures.
		others := history("failed", n, "uid-elsewhere", remediationv1alpha1.PhaseFailed)
		for i, at := range []int{limitsPageSize + 10, 2*limitsPageSize + 10, n - 1} {
			others[at].Status.LastPhaseTransitionTime.Time = limitsNow.Add(-time.Duration(3-i) * time.Minute)
		}
		others = append(others, otherRequest("success", "uid-s", remediationv1alpha1.PhaseSucceeded,
			10*time.Minute))
		r, base, rr, _ := limitsWorld(t, 20, others...)
		p := &apiServerReader{Reader: base}
		r.APIReader = p
		res, got, err := reconcileLimits(t, r, rr)
		expectHeld(t, res, err, got, ReasonCircuitOpen)
		p.expectBoundedPages(t, true)
	})
}

// TestLimitsCountDeletingRequests checks which requests being deleted with the finalizer still count against
// the cap: those whose deletion may close a pull request (a phase before the merge, NeedsIntervention, Expired)
// and every success and failure, which the breaker reads anyway; a Denied, Cancelled or Superseded one, whose
// deletion writes nothing to Git, is not read (#293).
func TestLimitsCountDeletingRequests(t *testing.T) {
	for _, tc := range []struct {
		phase   remediationv1alpha1.RemediationPhase
		counted bool
	}{
		{remediationv1alpha1.PhasePRCreated, true},
		{remediationv1alpha1.PhaseExpired, true},
		{remediationv1alpha1.PhaseNeedsIntervention, true},
		{remediationv1alpha1.PhaseSucceeded, true},
		{remediationv1alpha1.PhaseDenied, false},
		{remediationv1alpha1.PhaseCancelled, false},
		{remediationv1alpha1.PhaseSuperseded, false},
	} {
		t.Run(string(tc.phase), func(t *testing.T) {
			deleting := otherRequest("deleting", "uid-elsewhere", tc.phase, time.Hour)
			deleting.Finalizers = []string{FinalizerName}
			deleting.Status.HoldsTarget = new(!ended(tc.phase))
			r, base, rr, _ := limitsWorld(t, 1, deleting)
			if err := base.Delete(context.Background(), deleting); err != nil {
				t.Fatal(err)
			}
			r.APIReader = &apiServerReader{Reader: base}
			res, got, err := reconcileLimits(t, r, rr)
			if tc.counted {
				expectHeld(t, res, err, got, ReasonConcurrencyLimit)
			} else {
				expectAdmitted(t, err, got)
			}
		})
	}
}

// TestLimitsStoppedHolderLocks checks that a request that stopped while it holds the lock (status.holdsTarget
// true in a phase no other list reads, Cancelled) is found by the lock's list and keeps its target locked, while
// requests that released it are not read; and that one without the field (written before it existed) is read
// too, its lock derived from its status.
func TestLimitsStoppedHolderLocks(t *testing.T) {
	t.Run("holdsTarget true", func(t *testing.T) {
		holder := otherRequest("holder", limitsTarget, remediationv1alpha1.PhaseCancelled, time.Hour)
		holder.Status.HoldsTarget = new(true)
		others := append(history("refused", limitsPageSize, limitsTarget, refusals...), holder)
		r, base, rr, _ := limitsWorld(t, 20, others...)
		p := &apiServerReader{Reader: base}
		r.APIReader = p
		res, got, err := reconcileLimits(t, r, rr)
		expectHeldBehindStopped(t, res, err, got)
		names, _ := p.read()
		if slices.ContainsFunc(names, func(n string) bool { return strings.HasPrefix(n, "refused-") }) {
			t.Fatalf("read a request that released the lock: %v", names)
		}
	})
	t.Run("holder whose phase was set to Denied", func(t *testing.T) {
		// Denied is written only before the lock is taken; a holder edited to it still locks (fail closed).
		holder := otherRequest("holder", limitsTarget, remediationv1alpha1.PhaseDenied, time.Hour)
		holder.Status.HoldsTarget = new(true)
		r, base, rr, _ := limitsWorld(t, 20, holder)
		r.APIReader = &apiServerReader{Reader: base}
		res, got, err := reconcileLimits(t, r, rr)
		expectHeldBehindStopped(t, res, err, got)
	})
	t.Run("no holdsTarget, pull request may be open", func(t *testing.T) {
		old := otherRequest("old", limitsTarget, remediationv1alpha1.PhaseCancelled, time.Hour)
		old.Status.PullRequest = &remediationv1alpha1.PullRequestStatus{Number: 7}
		r, base, rr, _ := limitsWorld(t, 20, old)
		r.APIReader = &apiServerReader{Reader: base}
		res, got, err := reconcileLimits(t, r, rr)
		expectHeldBehindStopped(t, res, err, got)
	})
}

// expectHeldBehindStopped checks that the request waits behind a lock held by a request that stopped: Queued
// TargetLocked, checked again after heldRecheckInterval.
func expectHeldBehindStopped(t *testing.T, res reconcile.Result, err error, got *remediationv1alpha1.RemediationRequest) {
	t.Helper()
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	c := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionQueued)
	if got.Status.Phase == remediationv1alpha1.PhasePolicyChecked || c == nil || c.Reason != ReasonTargetLocked {
		t.Fatalf("phase %q, Queued %+v; want the request held back with TargetLocked", got.Status.Phase, c)
	}
	if res.RequeueAfter != heldRecheckInterval {
		t.Fatalf("RequeueAfter = %v, want %v", res.RequeueAfter, heldRecheckInterval)
	}
}

// TestLimitsReadThreeLists checks that a check of the limits reads three lists (#293): one of every phase but
// the unread neutral ends, so the cap, the moved-on requests and the breaker's outcomes come from one consistent
// snapshot and a request that changes phase between two reads cannot be missed or counted in two states; the
// holders of the lock; and the probes.
func TestLimitsReadThreeLists(t *testing.T) {
	r, base, rr, _ := limitsWorld(t, 20)
	p := &apiServerReader{Reader: base}
	r.APIReader = p
	_, got, err := reconcileLimits(t, r, rr)
	expectAdmitted(t, err, got)
	sels := make([]string, 0, len(p.calls))
	for _, c := range p.calls {
		sels = append(sels, c.selector)
	}
	want := []string{"status.phase!=Denied,status.phase!=Cancelled,status.phase!=Superseded",
		"status.holdsTarget!=false", "status.breakerProbe=true"}
	if !slices.Equal(sels, want) {
		t.Fatalf("lists read = %q, want %q", sels, want)
	}
}

// TestEveryPhaseIsClassified checks that every phase the API allows (the enum marker of RemediationPhase) is
// Pending, a phase before the merge, or in exactly one of successPhases, failurePhases and neutralPhases, so a
// phase added to the API is not silently left out of the breaker or the lock.
func TestEveryPhaseIsClassified(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "api", "v1alpha1", "remediationrequest_types.go"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^// \+kubebuilder:validation:Enum=(.*)\ntype RemediationPhase string`).FindSubmatch(src)
	if m == nil {
		t.Fatal("no enum marker on RemediationPhase")
	}
	for v := range strings.SplitSeq(string(m[1]), ";") {
		p := remediationv1alpha1.RemediationPhase(v)
		n := 0
		for _, in := range []bool{p == remediationv1alpha1.PhasePending, beforeMerge(p),
			slices.Contains(successPhases, p), slices.Contains(failurePhases, p), slices.Contains(neutralPhases, p)} {
			if in {
				n++
			}
		}
		if n != 1 {
			t.Errorf("phase %s is in %d classes, want exactly 1", p, n)
		}
	}
}

// TestRefusalsWriteHoldsTargetFalse checks that a request refused before the field existed (Denied, no plan
// digest, no status.holdsTarget) gets status.holdsTarget false once, so the lock's list
// (status.holdsTarget!=false) stops reading it.
func TestRefusalsWriteHoldsTargetFalse(t *testing.T) {
	old := otherRequest("old-denied", limitsTarget, remediationv1alpha1.PhaseDenied, time.Hour)
	old.Status.PlanDigest = "" // refused at the policy check: no plan
	r, _, _, _ := limitsWorld(t, 20, old)
	_, got, err := reconcileLimits(t, r, old)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status.HoldsTarget == nil || *got.Status.HoldsTarget {
		t.Fatalf("holdsTarget = %v, want false", got.Status.HoldsTarget)
	}
}

// TestUnreadPhasesMakeNoGitWriteOnDelete checks that the finalizer closes no pull request for a phase the limits
// do not read (unreadPhases) unless the request holds the lock, so not counting such a request while it is being
// deleted lets no Git write past the cap.
func TestUnreadPhasesMakeNoGitWriteOnDelete(t *testing.T) {
	for _, p := range unreadPhases {
		if closeOnDelete(p) {
			t.Errorf("closeOnDelete(%s) = true, but the limits do not read a %s request that holds no lock", p, p)
		}
	}
}

// TestLimitsPagesFailClosed checks that a list that cannot be finished (the continue token expired because the
// snapshot was compacted) lets nothing through, although the first page alone shows free slots.
func TestLimitsPagesFailClosed(t *testing.T) {
	r, base, rr, stub := limitsWorld(t, 20, history("new", limitsPageSize+1, "uid-elsewhere",
		remediationv1alpha1.PhasePending)...)
	r.APIReader = &apiServerReader{Reader: base, expire: true}
	_, got, err := reconcileLimits(t, r, rr)
	if !apierrors.IsResourceExpired(err) {
		t.Fatalf("err = %v, want the expired continue token", err)
	}
	if got.Status.Phase != "" {
		t.Fatalf("phase = %q, want none", got.Status.Phase)
	}
	if stub.calls.Load() != 1 {
		t.Fatalf("resolver calls = %d, want 1 (the policy check ran, the limits held it)", stub.calls.Load())
	}
}

// TestLimitsPagedAfterRestart checks that a new executor (a fresh reconciler, as after a restart or a new
// leader) reading the selected lists in pages finds the same limits as the one before it: nothing is kept in
// memory between checks (invariant 5).
func TestLimitsPagedAfterRestart(t *testing.T) {
	const n = limitsPageSize + 20
	others := append(history("new", n, "uid-elsewhere", remediationv1alpha1.PhasePending),
		otherRequest("zz-active", limitsTarget, remediationv1alpha1.PhasePolicyChecked, time.Minute))
	others = append(others, history("failed", 3, "uid-f", remediationv1alpha1.PhaseFailed)...)
	for _, f := range others[len(others)-3:] {
		f.Status.LastPhaseTransitionTime.Time = limitsNow.Add(-time.Minute)
	}
	r, base, rr, _ := limitsWorld(t, 20, others...)
	r.APIReader = &apiServerReader{Reader: base}
	res, got, err := reconcileLimits(t, r, rr)
	expectHeld(t, res, err, got, ReasonCircuitOpen)

	p := &apiServerReader{Reader: base}
	restarted := &RemediationRequestReconciler{Client: base, APIReader: p, Scheme: base.Scheme(), Source: r.Source,
		Now: r.Now}
	res, got, err = reconcileLimits(t, restarted, got)
	expectHeld(t, res, err, got, ReasonCircuitOpen)
	p.expectBoundedPages(t, true)
}
