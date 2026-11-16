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
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crevent "sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	api "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/providers"
	fakegit "github.com/pkalin/shadewright/internal/providers/fake"
)

// A refusal during the recovery of a handoff waits instead of ending the request (#786).

// refuseBranchHead makes the Git host refuse (403) every read of a branch head.
func refuseBranchHead(g *fakegit.Git) {
	g.SetFail(func(op string) error {
		if op == opBranchHead {
			return fmt.Errorf("status 403: %w", providers.ErrForbidden)
		}
		return nil
	})
}

// reconcileUntilMoved reconciles rr up to three times, until it leaves PolicyChecked, and returns it as stored.
func reconcileUntilMoved(t *testing.T, r *RemediationRequestReconciler, rr *api.RemediationRequest) *api.RemediationRequest {
	t.Helper()
	ctx := context.Background()
	got := &api.RemediationRequest{}
	for range 3 {
		_, _ = r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(rr)})
		if err := r.Get(ctx, client.ObjectKeyFromObject(rr), got); err != nil {
			t.Fatal(err)
		}
		if got.Status.Phase != api.PhasePolicyChecked {
			break
		}
	}
	return got
}

// drain returns the Events recorded so far.
func drain(rec *record.FakeRecorder) []string {
	var out []string
	for {
		select {
		case e := <-rec.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

// A refused read of the branch during the recovery keeps the replacement in its phase, past the deadline and its
// grace too, with condition PullRequestUnreadable and one Warning Event, looked at again every poll interval; once
// the host answers again, a merge of its commit ends it Merged with its receipt and the condition is gone.
// With outcome verification on, the merged request keeps the target lock, so only the takeover's own write removes
// the condition.
func TestRecoveryWatchesARefusalAndRecordsALaterMerge(t *testing.T) {
	for _, verify := range []bool{false, true} {
		t.Run(fmt.Sprintf("verify=%v", verify), func(t *testing.T) { recoveryWatchesARefusal(t, verify) })
	}
}

func recoveryWatchesARefusal(t *testing.T, verify bool) {
	r, g, _, rr, cr, _ := handoffAppendedWorld(t, nil)
	r.VerifyOutcomes = verify
	ctx := context.Background()
	rec := record.NewFakeRecorder(10)
	r.Recorder = rec
	refuseBranchHead(g)
	for _, ago := range []time.Duration{-time.Hour, handoffRecoveryGrace + time.Hour} {
		pastDeadline(t, r, rr, ago)
		res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(rr)})
		if err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		if res.RequeueAfter != DefaultPullRequestPollInterval {
			t.Errorf("RequeueAfter = %v, want the poll interval", res.RequeueAfter)
		}
		if err := r.Get(ctx, client.ObjectKeyFromObject(rr), rr); err != nil {
			t.Fatal(err)
		}
		if rr.Status.Phase != api.PhasePolicyChecked {
			t.Fatalf("phase = %q, want PolicyChecked: the replacement's commit may still be merged", rr.Status.Phase)
		}
		c := meta.FindStatusCondition(rr.Status.Conditions, api.ConditionPullRequestUnreadable)
		if c == nil || c.Status != metav1.ConditionTrue || c.Reason != ReasonGitForbidden ||
			!strings.Contains(c.Message, "read the head of the branch") {
			t.Fatalf("PullRequestUnreadable = %+v", c)
		}
	}
	if ev := drain(rec); len(ev) != 1 || !strings.HasPrefix(ev[0], "Warning "+EventPullRequestUnreadable+" ") {
		t.Fatalf("Events = %q, want one Warning %s", ev, EventPullRequestUnreadable)
	}

	g.SetFail(nil)
	if _, err := g.MergeWithCommit(cr.Number); err != nil {
		t.Fatal(err)
	}
	got := reconcileUntilMoved(t, r, rr)
	if got.Status.Phase != api.PhaseMerged || got.Status.MergeReceipt == nil {
		t.Fatalf("phase = %q, receipt %v: the merge of the replacement's own commit was not recorded",
			got.Status.Phase, got.Status.MergeReceipt)
	}
	if c := meta.FindStatusCondition(got.Status.Conditions, api.ConditionPullRequestUnreadable); c != nil {
		t.Fatalf("PullRequestUnreadable outlived the read that worked: %+v", c)
	}
	if ev := drain(rec); !containsPrefix(ev, "Normal "+EventPullRequestReadable+" ") {
		t.Fatalf("Events = %q, want a Normal %s", ev, EventPullRequestReadable)
	}
}

func containsPrefix(ev []string, prefix string) bool {
	for _, e := range ev {
		if strings.HasPrefix(e, prefix) {
			return true
		}
	}
	return false
}

// The settings that cannot reach the plan's repository any more hold the recovery as a refusal does, with the
// reason of condition PullRequestUnreadable that says why, as for a cancelled request (cancelRecoveryFailed).
func TestRecoveryWatchesSettingsThatCannotReachTheRepository(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		settings     func(*api.RemediationSettings)
		open         error
	}{
		{name: "left the settings", reason: reasonRepositoryUnavailable, settings: func(s *api.RemediationSettings) {
			s.Spec.Repositories[0].URL = "https://github.com/example/other"
		}},
		{name: "target branch", reason: reasonTargetBranchChanged, settings: func(s *api.RemediationSettings) {
			s.Spec.Repositories[0].TargetBranch = "release"
		}},
		{name: "unusable", reason: reasonRepositoryUnusable, open: fmt.Errorf("the key is not a PEM key: %w",
			providers.ErrInvalidConfig)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _, _, rr, _, _ := handoffAppendedWorld(t, nil)
			ctx := context.Background()
			rec := record.NewFakeRecorder(10)
			r.Recorder = rec
			if tc.settings != nil {
				s := &api.RemediationSettings{}
				if err := r.Get(ctx, client.ObjectKey{Name: api.SettingsName}, s); err != nil {
					t.Fatal(err)
				}
				tc.settings(s)
				if err := r.Update(ctx, s); err != nil {
					t.Fatal(err)
				}
			}
			if tc.open != nil {
				r.OpenRepository = func(context.Context, api.GitRepository) (providers.GitProvider, error) {
					return nil, tc.open
				}
			}
			pastDeadline(t, r, rr, handoffRecoveryGrace+time.Hour)
			res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(rr)})
			if err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			if res.RequeueAfter != DefaultPullRequestPollInterval {
				t.Errorf("RequeueAfter = %v, want the poll interval", res.RequeueAfter)
			}
			if err := r.Get(ctx, client.ObjectKeyFromObject(rr), rr); err != nil {
				t.Fatal(err)
			}
			if rr.Status.Phase != api.PhasePolicyChecked || rr.Status.Handoff.From.Phase != api.HandoffReserved {
				t.Fatalf("phase = %q, handoff %s: want a watched replacement", rr.Status.Phase, rr.Status.Handoff.From.Phase)
			}
			c := meta.FindStatusCondition(rr.Status.Conditions, api.ConditionPullRequestUnreadable)
			if c == nil || c.Reason != tc.reason {
				t.Fatalf("PullRequestUnreadable = %+v, want reason %s", c, tc.reason)
			}
			if ev := drain(rec); len(ev) != 1 || !strings.HasPrefix(ev[0], "Warning "+EventPullRequestUnreadable+" ") {
				t.Fatalf("Events = %q", ev)
			}
		})
	}
}

// A read that works and finds nothing to recover removes the condition before the request goes on, with one
// Normal Event.
func TestRecoveryClearsTheConditionOnAReadThatWorks(t *testing.T) {
	r, g, _, rr, _, _ := handoffAppendedWorld(t, nil)
	ctx := context.Background()
	rec := record.NewFakeRecorder(10)
	r.Recorder = rec
	refuseBranchHead(g)
	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(rr)}); err != nil {
		t.Fatal(err)
	}
	g.SetFail(nil)
	if err := r.Get(ctx, client.ObjectKeyFromObject(rr), rr); err != nil {
		t.Fatal(err)
	}
	// The predecessor's intent is gone: nothing to recover.
	pred := &api.RemediationRequest{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: rr.Namespace, Name: string(rr.Spec.Replaces.Name)}, pred); err != nil {
		t.Fatal(err)
	}
	pred.Status.Handoff.To = nil
	if err := r.Status().Update(ctx, pred); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.recoverHandoff(ctx, rr); err != nil {
		t.Fatal(err)
	}
	if c := meta.FindStatusCondition(rr.Status.Conditions, api.ConditionPullRequestUnreadable); c != nil {
		t.Fatalf("PullRequestUnreadable kept after a read that worked: %+v", c)
	}
	stored := &api.RemediationRequest{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(rr), stored); err != nil {
		t.Fatal(err)
	}
	if c := meta.FindStatusCondition(stored.Status.Conditions, api.ConditionPullRequestUnreadable); c != nil {
		t.Fatalf("PullRequestUnreadable kept in the stored request: %+v", c)
	}
	if ev := drain(rec); len(ev) != 2 || !strings.HasPrefix(ev[1], "Normal "+EventPullRequestReadable+" ") {
		t.Fatalf("Events = %q", ev)
	}
}

// A request changed between the read and the write of the condition (an annotation) is a conflict handed back for a
// retry, with no Event; the next look writes the condition and records the Event once.
func TestRecoveryRetriesTheConditionLostToAConflict(t *testing.T) {
	r, g, _, rr, _, _ := handoffAppendedWorld(t, nil)
	ctx := context.Background()
	rec := record.NewFakeRecorder(10)
	r.Recorder = rec
	refuseBranchHead(g)
	stale := rr.DeepCopy()
	touched := rr.DeepCopy()
	touched.Annotations = map[string]string{"example.com/recovery-touched": "true"}
	if err := r.Update(ctx, touched); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.recoverHandoff(ctx, stale); err == nil {
		t.Fatal("a condition write lost to a conflict was not handed back for a retry")
	}
	if ev := drain(rec); len(ev) != 0 {
		t.Fatalf("Events after a lost write = %q", ev)
	}
	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(rr)}); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(rr), rr); err != nil {
		t.Fatal(err)
	}
	if c := meta.FindStatusCondition(rr.Status.Conditions, api.ConditionPullRequestUnreadable); c == nil ||
		c.Reason != ReasonGitForbidden {
		t.Fatalf("PullRequestUnreadable = %+v", c)
	}
	if ev := drain(rec); len(ev) != 1 {
		t.Fatalf("Events = %q, want one", ev)
	}
}

// The write of the condition while the recovery waits wakes no reconcile of its own (skipOwnMarks), so the poll
// interval it returned stands; the same change on a request that is not recovering a handoff still does.
func TestRecoveryConditionWriteWakesNoReconcile(t *testing.T) {
	r, g, _, rr, _, _ := handoffAppendedWorld(t, nil)
	ctx := context.Background()
	refuseBranchHead(g)
	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(rr)}); err != nil {
		t.Fatal(err)
	}
	after := &api.RemediationRequest{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(rr), after); err != nil {
		t.Fatal(err)
	}
	if meta.FindStatusCondition(after.Status.Conditions, api.ConditionPullRequestUnreadable) == nil {
		t.Fatal("no condition was written")
	}
	before := after.DeepCopy()
	meta.RemoveStatusCondition(&before.Status.Conditions, api.ConditionPullRequestUnreadable)
	before.ResourceVersion = "1"
	if skipOwnMarks.Update(crevent.UpdateEvent{ObjectOld: before, ObjectNew: after}) {
		t.Fatal("the recovery's condition write woke a reconcile before the poll interval")
	}
	if skipOwnMarks.Update(crevent.UpdateEvent{ObjectOld: after, ObjectNew: before}) {
		t.Fatal("the removal of the recovery's condition woke a reconcile")
	}
	other, otherAfter := before.DeepCopy(), after.DeepCopy()
	other.Status.Phase, otherAfter.Status.Phase = api.PhasePRCreated, api.PhasePRCreated
	if !skipOwnMarks.Update(crevent.UpdateEvent{ObjectOld: other, ObjectNew: otherAfter}) {
		t.Fatal("a condition change outside the recovery was dropped")
	}
}

// failBranchHeadRetryable makes every read of a branch head fail for a reason worth retrying (a 503), with the
// Retry-After given (zero for none).
func failBranchHeadRetryable(g *fakegit.Git, retryAfter time.Duration) {
	g.SetFail(func(op string) error {
		if op == opBranchHead {
			return &providers.RetryableError{Err: errors.New("status 503"), RetryAfter: retryAfter}
		}
		return nil
	})
}

// A read of the predecessor's pull request that the Git host refuses for good (403) waits as a refused read of the
// branch does (#786, decision of 2026-11-16): past the deadline and its grace too, with condition
// PullRequestUnreadable and one Warning Event, every poll interval; the grace of #688 no longer ends it. Once the
// host answers again, a merge of the replacement's commit is recorded.
func TestRecoveryWatchesARefusedReadOfThePullRequest(t *testing.T) {
	r, g, _, rr, cr, _ := handoffAppendedWorld(t, nil)
	ctx := context.Background()
	rec := record.NewFakeRecorder(10)
	r.Recorder = rec
	g.SetFail(func(op string) error {
		switch op {
		case opBranchHead:
			return fmt.Errorf("branch: %w", providers.ErrNotFound)
		case opGet, opList:
			return fmt.Errorf("status 403: %w", providers.ErrForbidden)
		}
		return nil
	})
	for _, ago := range []time.Duration{-time.Hour, handoffRecoveryGrace + time.Hour} {
		pastDeadline(t, r, rr, ago)
		res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(rr)})
		if err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		if res.RequeueAfter != DefaultPullRequestPollInterval {
			t.Errorf("RequeueAfter = %v, want the poll interval", res.RequeueAfter)
		}
		if err := r.Get(ctx, client.ObjectKeyFromObject(rr), rr); err != nil {
			t.Fatal(err)
		}
		if rr.Status.Phase != api.PhasePolicyChecked {
			t.Fatalf("phase = %q, want PolicyChecked: a refused read of the pull request ended the replacement",
				rr.Status.Phase)
		}
		c := meta.FindStatusCondition(rr.Status.Conditions, api.ConditionPullRequestUnreadable)
		if c == nil || c.Status != metav1.ConditionTrue || c.Reason != ReasonGitForbidden {
			t.Fatalf("PullRequestUnreadable = %+v", c)
		}
	}
	// One Warning for the replacement, one for the predecessor, whose own read of its pull request (changeOf) sets
	// its condition (#688); none per retry.
	if ev := drain(rec); len(ev) != 2 || !strings.HasPrefix(ev[0], "Warning "+EventPullRequestUnreadable+" ") ||
		!strings.HasPrefix(ev[1], "Warning "+EventPullRequestUnreadable+" ") {
		t.Fatalf("Events = %q, want two Warning %s", ev, EventPullRequestUnreadable)
	}

	g.SetFail(nil)
	if _, err := g.MergeWithCommit(cr.Number); err != nil {
		t.Fatal(err)
	}
	got := reconcileUntilMoved(t, r, rr)
	if got.Status.Phase != api.PhaseMerged || got.Status.MergeReceipt == nil {
		t.Fatalf("phase = %q, receipt %v: the merge of the replacement's own commit was not recorded",
			got.Status.Phase, got.Status.MergeReceipt)
	}
	if c := meta.FindStatusCondition(got.Status.Conditions, api.ConditionPullRequestUnreadable); c != nil {
		t.Fatalf("PullRequestUnreadable outlived the read that worked: %+v", c)
	}
}

// A failure worth retrying (a 503) of a recovery that already waits on a refusal keeps the condition as it is and
// the pacing (the poll interval, or Retry-After when longer), and never ends the request HandoffRecoveryFailed,
// however long past the grace (#786, decision of 2026-11-16). The status decides this, not a time.
func TestRecoveryKeepsWaitingOnARefusalThroughARetryableFailure(t *testing.T) {
	for _, tc := range []struct {
		name       string
		retryAfter time.Duration
		want       time.Duration
	}{
		{name: "no Retry-After", want: DefaultPullRequestPollInterval},
		{name: "Retry-After longer than the poll interval", retryAfter: 2 * time.Hour, want: 2 * time.Hour},
		{name: "Retry-After shorter than the poll interval", retryAfter: time.Second,
			want: DefaultPullRequestPollInterval},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, g, _, rr, cr, _ := handoffAppendedWorld(t, nil)
			ctx := context.Background()
			rec := record.NewFakeRecorder(10)
			r.Recorder = rec
			refuseBranchHead(g)
			pastDeadline(t, r, rr, handoffRecoveryGrace+2*time.Hour)
			if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(rr)}); err != nil {
				t.Fatal(err)
			}
			if err := r.Get(ctx, client.ObjectKeyFromObject(rr), rr); err != nil {
				t.Fatal(err)
			}
			was := meta.FindStatusCondition(rr.Status.Conditions, api.ConditionPullRequestUnreadable)
			if was == nil || was.Reason != ReasonGitForbidden {
				t.Fatalf("PullRequestUnreadable = %+v", was)
			}
			was = was.DeepCopy()
			_ = drain(rec)

			failBranchHeadRetryable(g, tc.retryAfter)
			for range 2 {
				res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(rr)})
				if err != nil {
					t.Fatalf("Reconcile: %v", err)
				}
				if res.RequeueAfter != tc.want {
					t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, tc.want)
				}
				if err := r.Get(ctx, client.ObjectKeyFromObject(rr), rr); err != nil {
					t.Fatal(err)
				}
				if rr.Status.Phase != api.PhasePolicyChecked {
					t.Fatalf("phase = %q: a failure worth retrying ended a replacement that waits on a refusal",
						rr.Status.Phase)
				}
				c := meta.FindStatusCondition(rr.Status.Conditions, api.ConditionPullRequestUnreadable)
				if c == nil || c.Reason != was.Reason || c.Message != was.Message ||
					!c.LastTransitionTime.Equal(&was.LastTransitionTime) {
					t.Fatalf("PullRequestUnreadable = %+v, want it kept as %+v", c, was)
				}
			}
			if ev := drain(rec); len(ev) != 0 {
				t.Fatalf("Events = %q, want none for a failure that keeps the wait", ev)
			}

			g.SetFail(nil)
			if _, err := g.MergeWithCommit(cr.Number); err != nil {
				t.Fatal(err)
			}
			if got := reconcileUntilMoved(t, r, rr); got.Status.Phase != api.PhaseMerged {
				t.Fatalf("phase = %q: the merge after the wait was not recorded", got.Status.Phase)
			}
		})
	}
}

// Unchanged (#688): a replacement that never met a refusal, or whose refusal a read that worked has cleared, ends
// NeedsIntervention with HandoffRecoveryFailed once a failure worth retrying outlasts the grace. Only the condition
// in the status keeps the wait, never a memory of an earlier refusal.
func TestRecoveryWithoutARefusalKeepsTheGrace(t *testing.T) {
	for _, refusedBefore := range []bool{false, true} {
		t.Run(fmt.Sprintf("refusedBefore=%v", refusedBefore), func(t *testing.T) {
			r, g, _, rr, _, _ := handoffAppendedWorld(t, nil)
			ctx := context.Background()
			if refusedBefore {
				refuseBranchHead(g)
				if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(rr)}); err != nil {
					t.Fatal(err)
				}
				if err := r.Get(ctx, client.ObjectKeyFromObject(rr), rr); err != nil {
					t.Fatal(err)
				}
				if meta.FindStatusCondition(rr.Status.Conditions, api.ConditionPullRequestUnreadable) == nil {
					t.Fatal("no condition was written")
				}
				// A read that worked removed it (setUnreadable, as recoverHandoff does with nothing to recover).
				if err := r.setUnreadable(ctx, rr, nil); err != nil {
					t.Fatal(err)
				}
			}
			failBranchHeadRetryable(g, 0)
			pastDeadline(t, r, rr, handoffRecoveryGrace+time.Minute)
			if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(rr)}); err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			if err := r.Get(ctx, client.ObjectKeyFromObject(rr), rr); err != nil {
				t.Fatal(err)
			}
			if rr.Status.Phase != api.PhaseNeedsIntervention {
				t.Fatalf("phase = %q, want NeedsIntervention once the grace is over", rr.Status.Phase)
			}
			if c := meta.FindStatusCondition(rr.Status.Conditions, api.ConditionPROpened); c == nil ||
				c.Reason != ReasonHandoffRecoveryFailed {
				t.Fatalf("PullRequestOpened: %+v", c)
			}
		})
	}
}
