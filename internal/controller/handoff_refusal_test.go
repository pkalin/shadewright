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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	api "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/providers"
	fakegit "github.com/pkalin/shadewright/internal/providers/fake"
)

// A refusal during the recovery of a handoff waits instead of ending the request (#786).

const (
	// otherRepositoryURL is a repository URL the plan was not made for.
	otherRepositoryURL = "https://github.com/example/other"
	// otherTargetBranch is a target branch the plan was not made for.
	otherTargetBranch = "release"
	// branchHeadRead is the operation text of the read of the branch head, as a condition names it.
	branchHeadRead = "read the head of the branch"
	// caseLeftSettings and caseUnusable name the cases of a repository that left the settings or cannot be used.
	caseLeftSettings = "left the settings"
	caseUnusable     = "unusable"
)

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
			!strings.Contains(c.Message, branchHeadRead) {
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
		{name: caseLeftSettings, reason: reasonRepositoryUnavailable, settings: func(s *api.RemediationSettings) {
			s.Spec.Repositories[0].URL = otherRepositoryURL
		}},
		{name: "target branch", reason: reasonTargetBranchChanged, settings: func(s *api.RemediationSettings) {
			s.Spec.Repositories[0].TargetBranch = otherTargetBranch
		}},
		{name: caseUnusable, reason: reasonRepositoryUnusable, open: fmt.Errorf("the key is not a PEM key: %w",
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
	touched.Annotations = map[string]string{"example.com/recovery-touched": fieldTrue}
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
			// The failed read records the HandoffGrace wait, past its end; the check at the start of the next
			// reconcile ends it (endPassedWait).
			for range 2 {
				if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(rr)}); err != nil {
					t.Fatalf("Reconcile: %v", err)
				}
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

// conflictOnPredecessorStatus makes every status patch of the predecessor fail with an API conflict, as when it
// changed between the read and the write.
func conflictOnPredecessorStatus(r *RemediationRequestReconciler) {
	base := r.Client
	r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch,
			opts ...client.SubResourcePatchOption) error {
			if obj.GetName() == oldName {
				return apierrors.NewConflict(api.GroupVersion.WithResource("remediationrequests").GroupResource(),
					oldName, errors.New("it changed since it was read"))
			}
			return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
		},
	})
}

// A failure of the Kubernetes API while a recovery waits on a refusal (#786, decision of 2026-11-16 narrowing point
// 2) is returned to the error backoff, never taken for a failure of the Git host that keeps the wait: here the
// Git reads work, and the write that removes the predecessor's condition is refused with a conflict.
func TestRecoveryReturnsAnAPIFailureWhileWaiting(t *testing.T) {
	r, g, p, rr, _, _ := handoffAppendedWorld(t, nil)
	ctx := context.Background()
	refuseBranchHead(g)
	pastDeadline(t, r, rr, handoffRecoveryGrace+time.Hour)
	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(rr)}); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(rr), rr); err != nil {
		t.Fatal(err)
	}
	if watchedRecovery(rr) == nil {
		t.Fatal("the recovery does not wait")
	}
	// The predecessor's own read of its pull request was refused earlier (#688).
	if err := r.Get(ctx, client.ObjectKeyFromObject(p), p); err != nil {
		t.Fatal(err)
	}
	meta.SetStatusCondition(&p.Status.Conditions, *refusedRead(p, ReasonGitForbidden,
		fmt.Errorf("status 403: %w", providers.ErrForbidden), p.Status.PullRequest.Number))
	if err := r.Status().Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	g.SetFail(nil)
	conflictOnPredecessorStatus(r)
	res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(rr)})
	if err == nil {
		t.Fatalf("Reconcile = %+v, nil: an API conflict was kept as a wait instead of the error backoff", res)
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(rr), rr); err != nil {
		t.Fatal(err)
	}
	if rr.Status.Phase != api.PhasePolicyChecked {
		t.Fatalf("phase = %q: an API failure ended a watched replacement", rr.Status.Phase)
	}
}

// A failure that is neither a refusal nor worth retrying (an answer that cannot be parsed) while a recovery waits on
// a refusal never ends the request (#786, decision of 2026-11-16: let nothing unknown end a waiting recovery): it is
// returned to the error backoff, past the grace too, and the condition still names the refused read.
func TestRecoveryReturnsAnUnrecognisedFailureWhileWaiting(t *testing.T) {
	r, g, _, rr, _, _ := handoffAppendedWorld(t, nil)
	ctx := context.Background()
	refuseBranchHead(g)
	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(rr)}); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(rr), rr); err != nil {
		t.Fatal(err)
	}
	if watchedRecovery(rr) == nil {
		t.Fatal("the recovery does not wait")
	}
	g.SetFail(func(op string) error {
		if op == opBranchHead {
			return errors.New("the answer cannot be parsed")
		}
		return nil
	})
	pastDeadline(t, r, rr, handoffRecoveryGrace+time.Minute)
	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(rr)}); err == nil {
		t.Fatal("Reconcile = nil: an unrecognised failure was not returned to the error backoff")
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(rr), rr); err != nil {
		t.Fatal(err)
	}
	if rr.Status.Phase != api.PhasePolicyChecked {
		t.Fatalf("phase = %q, want PolicyChecked: an unrecognised failure ended a waiting recovery", rr.Status.Phase)
	}
	if c := watchedRecovery(rr); c == nil || c.Reason != ReasonGitForbidden || !strings.Contains(c.Message, branchHeadRead) {
		t.Fatalf("PullRequestUnreadable = %+v, want the refused read of the branch", c)
	}
}

// When the cause a waiting recovery's condition names is gone (the repository opens, or the refused read now gets
// an answer) and another read fails for a reason worth retrying, the condition is replaced by one that names the
// read failing now, with one Warning Event, and the request still waits past the grace (#786, decision of
// 2026-11-16 narrowing point 2). Further failures of the same read change nothing.
func TestRecoveryNamesTheReadThatFailsNow(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		cause        func(*RemediationRequestReconciler, *fakegit.Git) (fix func())
		failNow      func(*fakegit.Git)
		names        string
	}{
		{name: caseUnusable, reason: reasonRepositoryUnusable,
			cause: func(r *RemediationRequestReconciler, _ *fakegit.Git) func() {
				open := r.OpenRepository
				r.OpenRepository = func(context.Context, api.GitRepository) (providers.GitProvider, error) {
					return nil, fmt.Errorf("the key is not a PEM key: %w", providers.ErrInvalidConfig)
				}
				return func() { r.OpenRepository = open }
			},
			failNow: func(g *fakegit.Git) { failBranchHeadRetryable(g, 0) }, names: branchHeadRead},
		{name: caseLeftSettings, reason: reasonRepositoryUnavailable,
			cause: func(r *RemediationRequestReconciler, _ *fakegit.Git) func() {
				return changeSettings(t, r, func(s *api.RemediationSettings) {
					s.Spec.Repositories[0].URL = otherRepositoryURL
				})
			},
			failNow: func(g *fakegit.Git) { failBranchHeadRetryable(g, 0) }, names: branchHeadRead},
		{name: "target branch", reason: reasonTargetBranchChanged,
			cause: func(r *RemediationRequestReconciler, _ *fakegit.Git) func() {
				return changeSettings(t, r, func(s *api.RemediationSettings) {
					s.Spec.Repositories[0].TargetBranch = otherTargetBranch
				})
			},
			failNow: func(g *fakegit.Git) { failBranchHeadRetryable(g, 0) }, names: branchHeadRead},
		{name: "refused branch read answered", reason: ReasonGitForbidden,
			cause: func(_ *RemediationRequestReconciler, g *fakegit.Git) func() {
				refuseBranchHead(g)
				return func() { g.SetFail(nil) }
			},
			failNow: func(g *fakegit.Git) {
				g.SetFail(func(op string) error {
					if op == opGet || op == opList {
						return &providers.RetryableError{Err: errors.New("status 503")}
					}
					return nil
				})
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, g, _, rr, _, _ := handoffAppendedWorld(t, nil)
			ctx := context.Background()
			rec := record.NewFakeRecorder(20)
			r.Recorder = rec
			fix := tc.cause(r, g)
			if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(rr)}); err != nil {
				t.Fatal(err)
			}
			if err := r.Get(ctx, client.ObjectKeyFromObject(rr), rr); err != nil {
				t.Fatal(err)
			}
			was := meta.FindStatusCondition(rr.Status.Conditions, api.ConditionPullRequestUnreadable)
			if was == nil || was.Reason != tc.reason {
				t.Fatalf("PullRequestUnreadable = %+v, want reason %s", was, tc.reason)
			}
			was = was.DeepCopy()
			_ = drain(rec)

			fix()
			tc.failNow(g)
			pastDeadline(t, r, rr, handoffRecoveryGrace+time.Hour)
			for range 3 {
				res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(rr)})
				if err != nil {
					t.Fatalf("Reconcile: %v", err)
				}
				if res.RequeueAfter != DefaultPullRequestPollInterval {
					t.Errorf("RequeueAfter = %v, want the poll interval", res.RequeueAfter)
				}
			}
			if err := r.Get(ctx, client.ObjectKeyFromObject(rr), rr); err != nil {
				t.Fatal(err)
			}
			if rr.Status.Phase != api.PhasePolicyChecked {
				t.Fatalf("phase = %q: the replacement left the wait", rr.Status.Phase)
			}
			c := meta.FindStatusCondition(rr.Status.Conditions, api.ConditionPullRequestUnreadable)
			if c == nil || c.Status != metav1.ConditionTrue || c.Reason != ReasonRecoveryReadFailed ||
				c.Message == was.Message || (tc.names != "" && !strings.Contains(c.Message, tc.names)) {
				t.Fatalf("PullRequestUnreadable = %+v, want reason %s naming the read failing now (was %+v)",
					c, ReasonRecoveryReadFailed, was)
			}
			if watchedRecovery(rr) == nil {
				t.Fatal("the replaced condition does not keep the request in the wait")
			}
			if ev := drain(rec); len(ev) != 1 || !strings.HasPrefix(ev[0], "Warning "+EventPullRequestUnreadable+" ") {
				t.Fatalf("Events = %q, want one Warning %s for the change", ev, EventPullRequestUnreadable)
			}
		})
	}
}

// changeSettings changes RemediationSettings and returns the function that puts them back.
func changeSettings(t *testing.T, r *RemediationRequestReconciler, change func(*api.RemediationSettings)) func() {
	t.Helper()
	ctx := context.Background()
	s := &api.RemediationSettings{}
	if err := r.Get(ctx, client.ObjectKey{Name: api.SettingsName}, s); err != nil {
		t.Fatal(err)
	}
	orig := s.Spec.DeepCopy()
	change(s)
	if err := r.Update(ctx, s); err != nil {
		t.Fatal(err)
	}
	return func() {
		if err := r.Get(ctx, client.ObjectKey{Name: api.SettingsName}, s); err != nil {
			t.Fatal(err)
		}
		s.Spec = *orig
		if err := r.Update(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
}

// The replacement of a condition whose cause is gone, lost to a request changed since it was read (an annotation),
// is handed back for a retry with no Event; the next look writes it and records the Event once.
func TestRecoveryRetriesTheReplacedConditionLostToAConflict(t *testing.T) {
	r, g, _, rr, _, _ := handoffAppendedWorld(t, nil)
	ctx := context.Background()
	rec := record.NewFakeRecorder(10)
	r.Recorder = rec
	open := r.OpenRepository
	r.OpenRepository = func(context.Context, api.GitRepository) (providers.GitProvider, error) {
		return nil, fmt.Errorf("the key is not a PEM key: %w", providers.ErrInvalidConfig)
	}
	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(rr)}); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(rr), rr); err != nil {
		t.Fatal(err)
	}
	_ = drain(rec)
	r.OpenRepository = open
	failBranchHeadRetryable(g, 0)
	stale := rr.DeepCopy()
	touched := rr.DeepCopy()
	touched.Annotations = map[string]string{"example.com/recovery-touched": fieldTrue}
	if err := r.Update(ctx, touched); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.recoverHandoff(ctx, stale); err == nil {
		t.Fatal("a replaced condition lost to a conflict was not handed back for a retry")
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
		c.Reason != ReasonRecoveryReadFailed {
		t.Fatalf("PullRequestUnreadable = %+v", c)
	}
	if ev := drain(rec); len(ev) != 1 {
		t.Fatalf("Events = %q, want one", ev)
	}
}

// A recovery that waits on a reason about the repository (#786, decision of 2026-11-16: sort by where the error comes
// from): once the settings entry is fine again and the open of the provider is what fails, the condition names that
// open; while the read of the settings itself fails, it is the Kubernetes API's, returned to the error backoff, and
// the reason about the repository stays.
func TestRecoveryOpenErrorIsSortedBySource(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		cause        func(*RemediationRequestReconciler) (fix func())
	}{
		{name: caseLeftSettings, reason: reasonRepositoryUnavailable,
			cause: func(r *RemediationRequestReconciler) func() {
				return changeSettings(t, r, func(s *api.RemediationSettings) {
					s.Spec.Repositories[0].URL = otherRepositoryURL
				})
			}},
		{name: caseUnusable, reason: reasonRepositoryUnusable,
			cause: func(r *RemediationRequestReconciler) func() {
				open := r.OpenRepository
				r.OpenRepository = func(context.Context, api.GitRepository) (providers.GitProvider, error) {
					return nil, fmt.Errorf("the key is not a PEM key: %w", providers.ErrInvalidConfig)
				}
				return func() { r.OpenRepository = open }
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _, _, rr, _, _ := handoffAppendedWorld(t, nil)
			ctx := context.Background()
			fix := tc.cause(r)
			if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(rr)}); err != nil {
				t.Fatal(err)
			}
			if err := r.Get(ctx, client.ObjectKeyFromObject(rr), rr); err != nil {
				t.Fatal(err)
			}
			if c := watchedRecovery(rr); c == nil || c.Reason != tc.reason {
				t.Fatalf("PullRequestUnreadable = %+v, want %s", c, tc.reason)
			}
			fix()
			pastDeadline(t, r, rr, handoffRecoveryGrace+time.Hour)

			// The settings read fails with a transport error: returned, the reason stays.
			base := r.Client
			r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object,
					opts ...client.GetOption) error {
					if _, ok := obj.(*api.RemediationSettings); ok {
						return errors.New("dial tcp 10.0.0.1:443: connect: connection refused")
					}
					return c.Get(ctx, key, obj, opts...)
				},
			})
			r.APIReader = r.Client
			if _, _, err := r.recoverHandoff(ctx, rr); err == nil {
				t.Fatal("a failed read of the settings was not returned to the error backoff")
			}
			if err := base.Get(ctx, client.ObjectKeyFromObject(rr), rr); err != nil {
				t.Fatal(err)
			}
			if c := watchedRecovery(rr); c == nil || c.Reason != tc.reason || rr.Status.Phase != api.PhasePolicyChecked {
				t.Fatalf("phase %q, PullRequestUnreadable = %+v, want %s kept", rr.Status.Phase, c, tc.reason)
			}

			// The settings are fine, the open of the provider fails: the condition names the open.
			r.Client, r.APIReader = base, base
			open := r.OpenRepository
			r.OpenRepository = func(context.Context, api.GitRepository) (providers.GitProvider, error) {
				return nil, &providers.RetryableError{Err: errors.New("the token endpoint did not answer")}
			}
			res, _, err := r.recoverHandoff(ctx, rr)
			if err != nil || res.RequeueAfter != DefaultPullRequestPollInterval {
				t.Fatalf("recoverHandoff = %+v, %v; want the wait", res, err)
			}
			if err := r.Get(ctx, client.ObjectKeyFromObject(rr), rr); err != nil {
				t.Fatal(err)
			}
			if c := watchedRecovery(rr); c == nil || c.Reason != ReasonRecoveryReadFailed ||
				!strings.Contains(c.Message, "open the repository") {
				t.Fatalf("PullRequestUnreadable = %+v, want %s naming the open", c, ReasonRecoveryReadFailed)
			}
			r.OpenRepository = open
		})
	}
}

// A refusal that comes with a failed write of the Kubernetes API (the predecessor's own condition, lost to a conflict)
// is watched, and the failed write is returned to the error backoff after it: on the first refusal and on a recovery
// that already waits.
func TestRecoveryReturnsTheAPIFailureThatCameWithARefusal(t *testing.T) {
	for _, waiting := range []bool{false, true} {
		t.Run(fmt.Sprintf("waiting=%v", waiting), func(t *testing.T) {
			r, g, _, rr, _, _ := handoffAppendedWorld(t, nil)
			ctx := context.Background()
			if waiting {
				refuseBranchHead(g)
				if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(rr)}); err != nil {
					t.Fatal(err)
				}
			}
			if err := r.Get(ctx, client.ObjectKeyFromObject(rr), rr); err != nil {
				t.Fatal(err)
			}
			// The branch answers 404 and the read of the pull request is refused; the predecessor's condition for
			// that refusal is lost to a conflict.
			g.SetFail(func(op string) error {
				switch op {
				case opBranchHead:
					return fmt.Errorf("status 404: %w", providers.ErrNotFound)
				case opGet:
					return fmt.Errorf("status 403: %w", providers.ErrForbidden)
				}
				return nil
			})
			conflictOnPredecessorStatus(r)
			pastDeadline(t, r, rr, handoffRecoveryGrace+time.Hour)
			if res, _, err := r.recoverHandoff(ctx, rr); err == nil {
				t.Fatalf("recoverHandoff = %+v, nil: the API conflict was not returned", res)
			}
			if err := r.Get(ctx, client.ObjectKeyFromObject(rr), rr); err != nil {
				t.Fatal(err)
			}
			if c := watchedRecovery(rr); c == nil || c.Reason != ReasonGitForbidden || strings.Contains(c.Message, branchHeadRead) {
				t.Fatalf("PullRequestUnreadable = %+v, want the refused read of the pull request", c)
			}
			if rr.Status.Phase != api.PhasePolicyChecked {
				t.Fatalf("phase = %q", rr.Status.Phase)
			}
		})
	}
}

// A takeover of the open pull request by a recovery that waited records the Normal Event once its write went through.
func TestRecoveryTakeoverRecordsTheReadableEvent(t *testing.T) {
	r, g, _, rr, _, _ := handoffAppendedWorld(t, nil)
	ctx := context.Background()
	rec := record.NewFakeRecorder(10)
	r.Recorder = rec
	refuseBranchHead(g)
	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(rr)}); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(rr), rr); err != nil {
		t.Fatal(err)
	}
	_ = drain(rec)
	g.SetFail(nil)
	if _, done, err := r.recoverHandoff(ctx, rr); err != nil || !done {
		t.Fatalf("recoverHandoff = %v, %v", done, err)
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(rr), rr); err != nil {
		t.Fatal(err)
	}
	if rr.Status.Phase != api.PhasePRCreated || watchedRecovery(rr) != nil {
		t.Fatalf("phase %q, conditions %+v", rr.Status.Phase, rr.Status.Conditions)
	}
	if ev := drain(rec); len(ev) != 1 || !strings.HasPrefix(ev[0], "Normal "+EventPullRequestReadable+" ") {
		t.Fatalf("Events = %q, want one Normal %s", ev, EventPullRequestReadable)
	}
}

// A request that ends while its recovery waits does not keep a recovery condition that says the read is made again;
// a reason about the repository, which still holds, stays.
func TestEndDropsTheRecoveryWait(t *testing.T) {
	for _, tc := range []struct {
		reason string
		kept   bool
	}{
		{reason: ReasonRecoveryReadFailed}, {reason: ReasonGitForbidden}, {reason: reasonRepositoryUnavailable, kept: true},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			r, _, _, rr, _, _ := handoffAppendedWorld(t, nil)
			ctx := context.Background()
			meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{Type: api.ConditionPullRequestUnreadable,
				Status: metav1.ConditionTrue, Reason: tc.reason, Message: "it is read again"})
			rr.Status.HoldsTarget = new(true)
			if err := r.Status().Update(ctx, rr); err != nil {
				t.Fatal(err)
			}
			if err := r.stop(ctx, rr, api.PhaseSuperseded, api.ReasonHandedOff, "ended"); !errors.Is(err, errStopped) {
				t.Fatal(err)
			}
			if err := r.Get(ctx, client.ObjectKeyFromObject(rr), rr); err != nil {
				t.Fatal(err)
			}
			if !ptr.Deref(rr.Status.HoldsTarget, false) {
				t.Fatal("the ended request let its lock go: the case is not the one under test")
			}
			if got := meta.FindStatusCondition(rr.Status.Conditions, api.ConditionPullRequestUnreadable) != nil; got != tc.kept {
				t.Fatalf("PullRequestUnreadable kept = %v, want %v", got, tc.kept)
			}
		})
	}
}
