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
	"math/rand/v2"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	api "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/providers"
)

// nextRequestName is the replacement a handoff of these tests names.
const nextRequestName = "next-request"

// headNotPlanned is the message of a refused open in these tests.
const headNotPlanned = "The head is not the planned one"

// stopWroteCheck fails t unless the stop's step wrote the failed review check on head and recorded state Stale on it
// in the stored request (#1008, decision of 2026-11-21: every stop that wrote it on main writes it in the same step).
func stopWroteCheck(t *testing.T, r *RemediationRequestReconciler, runs []providers.CheckRun, rr *api.RemediationRequest,
	head string) {
	t.Helper()
	live := &api.RemediationRequest{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(rr), live); err != nil {
		t.Fatal(err)
	}
	if !failedOnly(runs) {
		t.Errorf("check runs of the head after the stop's step %+v: want the stop's failed check, as on main", runs)
	}
	if pr := live.Status.PullRequest; pr == nil || pr.ReviewCheck == nil || pr.ReviewCheck.State != api.ReviewCheckStale ||
		pr.ReviewCheck.Head != head {
		t.Errorf("stored pull request %+v: want state Stale recorded on the head in the stop's write", pr)
	}
}

// #1008: the adoption of a lost pull request found open on another base records it in memory (recordFound) and stops
// in the same step (stopOpenElsewhere). The stored request records no pull request yet; the gate must still write the
// stop's failed check in that step, as main did, since the caller records the pull request in this step.
func TestAdoptionStopOnOtherBaseWritesTheCheckInItsStep(t *testing.T) {
	r, g, rr := settlementWorld(t, api.PhaseAwaitingApproval)
	ctx := context.Background()
	r.ReviewerHints = true
	setTargetMain(t, r)
	meta.RemoveStatusCondition(&rr.Status.Conditions, api.ConditionApproved)
	if err := r.Status().Update(ctx, rr); err != nil {
		t.Fatal(err)
	}
	n, head := rr.Status.PullRequest.Number, rr.Status.PullRequest.HeadSHA
	moveTo(t, g, n, movedBranch)
	adoptMoved(t, r, g, rr)
	key := client.ObjectKeyFromObject(rr)
	clock := clockOf(r)
	live := &api.RemediationRequest{}
	for range 6 {
		if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key}); err != nil {
			t.Logf("Reconcile: %v", err)
		}
		if err := r.Get(ctx, key, live); err != nil {
			t.Fatal(err)
		}
		if live.Status.Phase == api.PhaseNeedsIntervention {
			break
		}
		clock.advance(confirmWait + time.Minute)
	}
	if live.Status.Phase != api.PhaseNeedsIntervention || !hasReason(live, ReasonChangeRequestOnOtherBase, "") {
		t.Fatalf("phase %s, conditions %s: want the adoption to stop with ChangeRequestOnOtherBase", live.Status.Phase,
			conditionsOf(live))
	}
	stopWroteCheck(t, r, g.CheckRuns(head), rr, head)
}

// #1008: refuseOpened finds the pull request it was about to close moved to another base (refuseMoved) and stops
// (stopOpenElsewhere) with the pull request recorded in memory only. The gate must write the stop's failed check in
// that step, as main did.
func TestRefuseOpenedMovedStopWritesTheCheckInItsStep(t *testing.T) {
	r, g, rr := settlementWorld(t, api.PhaseAwaitingApproval)
	ctx := context.Background()
	r.ReviewerHints = true
	setTargetMain(t, r)
	n, head := rr.Status.PullRequest.Number, rr.Status.PullRequest.HeadSHA
	moveTo(t, g, n, movedBranch)
	adoptMoved(t, r, g, rr)
	cr := g.ChangeRequests()[n-1]
	err := r.refuseOpened(ctx, g, rr, cr, &renderRefusal{reason: ReasonHeadMoved, message: headNotPlanned})
	if !errors.Is(err, errStopped) {
		t.Fatalf("refuseOpened: %v, want the request stopped", err)
	}
	stopWroteCheck(t, r, g.CheckRuns(head), rr, head)
}

// #1008: the gate decides on its own read of the request, made after every other call of the step and immediately
// before the write. A deletion that starts while the look of a stopped holder reads the review check of its head
// (ReviewCheckRun, after afterLook read the request) must stop the write: no failed check on a request being deleted.
func TestStoppedCheckGateReadsAfterTheStepsCalls(t *testing.T) {
	r, g, p, _, _, head := gateWorld(t)
	ctx := context.Background()
	plainWaiting(t, r, p, head)
	stopLive(t, r, g, p)
	if err := g.CreateCheckRun(ctx, head, checkRunFor(providers.CheckRunInProgress)); err != nil {
		t.Fatal(err)
	}
	key := client.ObjectKeyFromObject(p)
	if err := r.Get(ctx, key, p); err != nil {
		t.Fatal(err)
	}
	p.Finalizers = append(p.Finalizers, FinalizerName)
	if err := r.Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(ctx, key, p); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	var deleteErr error
	deleted := false
	g.SetFail(func(op string) error {
		if op == opReviewCheckRun {
			once.Do(func() {
				deleteErr = r.Delete(ctx, &api.RemediationRequest{ObjectMeta: p.ObjectMeta})
				deleted = true
			})
		}
		return nil
	})
	runs := len(g.CheckRuns(head))
	err := r.afterLook(ctx, p)
	if deleteErr != nil || !deleted {
		t.Fatalf("deletion during ReviewCheckRun: ran %v, err %v", deleted, deleteErr)
	}
	if err != nil {
		t.Fatalf("afterLook: %v", err)
	}
	if got := g.CheckRuns(head); len(got) != runs {
		t.Errorf("check runs %+v: the look wrote the failed check on a request whose deletion started before the "+
			"write", got[runs:])
	}
}

// mainStopWritesCheck is the rule of origin/main (before the gate of #1008) for a stop's failed review check, kept
// here as the oracle of TestStopPathsWriteTheCheckAsOnMain: the stop wrote it whenever the request it held in memory
// owned the review check (main's ownsReviewCheck: reviewer hints on, a pull request recorded, no handoff done and no
// handoff intent).
func mainStopWritesCheck(hints bool, rr *api.RemediationRequest) bool {
	ho := rr.Status.Handoff
	handed := ho != nil && ho.To != nil && (ho.To.Phase == api.HandoffDone || ho.To.Phase == api.HandoffIntent)
	return hints && rr.Status.PullRequest != nil && !handed
}

// stopPath is one stop of TestStopPathsWriteTheCheckAsOnMain: whether it records its pull request in its own step
// (the stored request records none yet), and the call that stops.
type stopPath struct {
	name       string
	recordsNow bool
	stop       func(ctx context.Context, r *RemediationRequestReconciler, rr *api.RemediationRequest,
		cr providers.ChangeRequest) error
}

// TestStopPathsWriteTheCheckAsOnMain (#1008, decision of 2026-11-21: every stop that wrote the failed check on main
// writes it in the same step here) draws stops with a seeded generator: the stop path (the adoption of a lost pull
// request and the refused open, which record it in their own step, the watch's stop on another base and on a pull
// request gone, which find it stored), reviewer hints and the handoff state. The stored request holds the lock, is
// not being deleted and records no merge, as at any of those stops, so the answer is known by construction: the
// stop's step writes the check exactly when main's rule (mainStopWritesCheck) did.
func TestStopPathsWriteTheCheckAsOnMain(t *testing.T) {
	paths := []stopPath{
		{name: "adoption on another base", recordsNow: true, stop: func(ctx context.Context, r *RemediationRequestReconciler,
			rr *api.RemediationRequest, cr providers.ChangeRequest) error {
			recordFound(rr, cr)
			return r.stopOpenElsewhere(ctx, rr, cr, true)
		}},
		{name: "refused open moved", recordsNow: true, stop: func(ctx context.Context, r *RemediationRequestReconciler,
			rr *api.RemediationRequest, cr providers.ChangeRequest) error {
			return r.refuseOpened(ctx, r.gitForTest(ctx, t, rr), rr, cr, &renderRefusal{reason: ReasonHeadMoved,
				message: headNotPlanned})
		}},
		{name: "watch on another base", stop: func(ctx context.Context, r *RemediationRequestReconciler,
			rr *api.RemediationRequest, cr providers.ChangeRequest) error {
			return r.stopOpenElsewhere(ctx, rr, cr, false)
		}},
		{name: "watch on gone", stop: func(ctx context.Context, r *RemediationRequestReconciler,
			rr *api.RemediationRequest, _ providers.ChangeRequest) error {
			return r.stopGone(ctx, rr, changeVerdict{gone: "not found"})
		}},
	}
	handoffs := []*api.Handoff{nil,
		{To: &api.OutgoingHandoff{Name: nextRequestName, Phase: api.HandoffDone}},
		{To: &api.OutgoingHandoff{Name: nextRequestName, Phase: api.HandoffIntent}},
	}
	// #nosec G404 -- a seeded generator, so a failing draw can be run again.
	rng := rand.New(rand.NewPCG(1008, 21))
	counts := map[string]int{}
	for i := range 48 {
		path := paths[i%len(paths)]
		hints := rng.IntN(4) != 0
		ho := handoffs[rng.IntN(len(handoffs))]
		t.Run(fmt.Sprintf("%02d %s hints=%v handoff=%v", i, path.name, hints, ho != nil), func(t *testing.T) {
			r, g, rr := settlementWorld(t, api.PhaseAwaitingApproval)
			ctx := context.Background()
			r.ReviewerHints = hints
			setTargetMain(t, r)
			n, head := rr.Status.PullRequest.Number, rr.Status.PullRequest.HeadSHA
			moveTo(t, g, n, movedBranch)
			if path.recordsNow {
				adoptMoved(t, r, g, rr)
			}
			rr.Status.Handoff = ho.DeepCopy()
			if err := r.Status().Update(ctx, rr); err != nil {
				t.Fatal(err)
			}
			cr := g.ChangeRequests()[n-1]
			mem := rr.DeepCopy()
			if path.recordsNow {
				recordNumber(mem, api.PullRequestStatus{Number: cr.Number, URL: cr.URL, HeadSHA: cr.HeadSHA})
			}
			want := mainStopWritesCheck(hints, mem)
			if err := path.stop(ctx, r, rr, cr); err != nil && !errors.Is(err, errStopped) {
				t.Fatalf("stop: %v", err)
			}
			runs := g.CheckRuns(head)
			if got := failedOnly(runs) && len(runs) == 1; got != want {
				t.Errorf("check runs of the head after the stop's step %+v: want the failed check %v, as on main", runs,
					want)
			}
			if want {
				counts[path.name]++
			}
		})
	}
	for _, p := range paths {
		if counts[p.name] == 0 {
			t.Errorf("no draw of %q wrote the check: the comparison proves nothing for that path", p.name)
		}
	}
}

// gitForTest opens rr's plan repository for a direct call of a step that takes the Git provider.
func (r *RemediationRequestReconciler) gitForTest(ctx context.Context, t *testing.T,
	rr *api.RemediationRequest) providers.GitProvider {
	t.Helper()
	git, _, _, err := r.openPlanRepositoryFor(ctx, rr)
	if err != nil {
		t.Fatal(err)
	}
	return git
}
