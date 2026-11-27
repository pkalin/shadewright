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

	api "github.com/shadewright-labs/shadewright/api/v1alpha1"
	"github.com/shadewright-labs/shadewright/internal/providers"
	fakegit "github.com/shadewright-labs/shadewright/internal/providers/fake"
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

// annotationRecoveryTouched is the annotation a test sets between a read and a write of the request.
const annotationRecoveryTouched = "example.com/recovery-touched"

// findChangeRead is the text that names the watch's read of the pull request after the abort.
const findChangeRead = "find the change request"

// refuseRecoveryReads makes the Git host refuse (403) every read of the recovery: the head of a branch, which it reads
// first, and the pull request, which a replacement whose handoff was aborted reads.
func refuseRecoveryReads(g *fakegit.Git) {
	g.SetFail(func(op string) error {
		if op == opBranchHead || op == opGet || op == opList {
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

// A refused read of the branch during the recovery aborts the handoff and keeps the replacement in its phase, past
// the deadline and its grace too, with condition PullRequestUnreadable naming the read refused now and one Warning
// Event, looked at again every poll interval; once the host answers again, a merge of its commit ends it Merged with
// its receipt and the condition is gone (#786, decision of 2026-11-18).
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
	refuseRecoveryReads(g)
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
		if c == nil || c.Status != metav1.ConditionTrue || c.Reason != ReasonGitForbidden {
			t.Fatalf("PullRequestUnreadable = %+v", c)
		}
		if !abortedOnRefusal(rr) {
			t.Fatalf("waits = %+v, want the handoff aborted at the refusal", rr.Status.GitWaits)
		}
	}
	// One Warning for the refused read of the branch, one when the watch's read of the pull request is refused; none
	// per retry.
	if ev := drain(rec); len(ev) != 2 || !strings.HasPrefix(ev[0], "Warning "+EventPullRequestUnreadable+" ") ||
		!strings.Contains(ev[1], "to find the change request") {
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

// After the abort, a read of the pull request that works removes the condition, with one Normal Event, while the
// replacement keeps watching: the HandoffRecovery entry stands, so it never takes the pull request over (#786).
func TestRecoveryClearsTheConditionOnAReadThatWorks(t *testing.T) {
	r, g, _, rr, _, _ := handoffAppendedWorld(t, nil)
	ctx := context.Background()
	rec := record.NewFakeRecorder(10)
	r.Recorder = rec
	refuseRecoveryReads(g)
	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(rr)}); err != nil {
		t.Fatal(err)
	}
	g.SetFail(nil)
	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(rr)}); err != nil {
		t.Fatal(err)
	}
	stored := &api.RemediationRequest{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(rr), stored); err != nil {
		t.Fatal(err)
	}
	if c := meta.FindStatusCondition(stored.Status.Conditions, api.ConditionPullRequestUnreadable); c != nil {
		t.Fatalf("PullRequestUnreadable kept after a read that worked: %+v", c)
	}
	if !abortedOnRefusal(stored) || tookOver(stored) || stored.Status.Phase != api.PhasePolicyChecked {
		t.Fatalf("phase %q, waits %+v: want the replacement still watching", stored.Status.Phase,
			stored.Status.GitWaits)
	}
	if ev := drain(rec); len(ev) != 2 || !strings.HasPrefix(ev[1], "Normal "+EventPullRequestReadable+" ") {
		t.Fatalf("Events = %q", ev)
	}
}

// A request changed between the read and the write of the abort (an annotation) is read again and written in the
// same reconcile: the condition and the HandoffRecovery entry are stored, the annotation kept, and the Event is
// recorded once.
func TestRecoveryRetriesTheConditionLostToAConflict(t *testing.T) {
	r, g, _, rr, _, _ := handoffAppendedWorld(t, nil)
	ctx := context.Background()
	rec := record.NewFakeRecorder(10)
	r.Recorder = rec
	refuseRecoveryReads(g)
	stale := rr.DeepCopy()
	touched := rr.DeepCopy()
	touched.Annotations = map[string]string{annotationRecoveryTouched: fieldTrue}
	if err := r.Update(ctx, touched); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.recoverHandoff(ctx, stale); err != nil {
		t.Fatalf("recoverHandoff = %v, want the write retried on a fresh read", err)
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(rr), rr); err != nil {
		t.Fatal(err)
	}
	if c := meta.FindStatusCondition(rr.Status.Conditions, api.ConditionPullRequestUnreadable); c == nil ||
		c.Reason != ReasonGitForbidden || !abortedOnRefusal(rr) {
		t.Fatalf("PullRequestUnreadable = %+v, waits %+v", c, rr.Status.GitWaits)
	}
	if rr.Annotations[annotationRecoveryTouched] != fieldTrue {
		t.Fatalf("annotations = %v, want the change kept", rr.Annotations)
	}
	if ev := drain(rec); len(ev) != 1 {
		t.Fatalf("Events = %q, want one", ev)
	}
}

// failRecoveryReadsRetryable makes every read of the recovery (the head of a branch, the pull request) fail for a
// reason worth retrying (a 503), with the Retry-After given (zero for none).
func failRecoveryReadsRetryable(g *fakegit.Git, retryAfter time.Duration) {
	g.SetFail(func(op string) error {
		if op == opBranchHead || op == opGet || op == opList {
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

// A failure worth retrying (a 503) of a replacement that watches after the abort names the read failing now once
// (RecoveryReadFailed, the time of the condition kept), then keeps the condition as it is and the pacing (the poll
// interval, or Retry-After when longer), and never ends the request HandoffRecoveryFailed, however long past the
// grace (#786, decisions of 2026-11-16 and 2026-11-18). The status decides this, not a time.
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
			refuseRecoveryReads(g)
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

			failRecoveryReadsRetryable(g, tc.retryAfter)
			for i := range 2 {
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
				if i == 0 {
					if c == nil || c.Reason != ReasonRecoveryReadFailed || !strings.Contains(c.Message,
						"find the change request") || !c.LastTransitionTime.Equal(&was.LastTransitionTime) {
						t.Fatalf("PullRequestUnreadable = %+v, want the read failing now named", c)
					}
					was = c.DeepCopy()
					_ = drain(rec)
					continue
				}
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

// Unchanged (#688): a replacement that never met a refusal ends NeedsIntervention with HandoffRecoveryFailed once a
// failure worth retrying outlasts the grace. One whose recovery met a refusal had its handoff aborted then: a read that
// worked since, and a failure past the grace, leave it watching until the end of its HandoffRecovery entry, from the
// recorded entry alone (#786, decision of 2026-11-18).
func TestRecoveryWithoutARefusalKeepsTheGrace(t *testing.T) {
	for _, refusedBefore := range []bool{false, true} {
		t.Run(fmt.Sprintf("refusedBefore=%v", refusedBefore), func(t *testing.T) {
			r, g, _, rr, _, _ := handoffAppendedWorld(t, nil)
			ctx := context.Background()
			if refusedBefore {
				refuseRecoveryReads(g)
				if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(rr)}); err != nil {
					t.Fatal(err)
				}
				if err := r.Get(ctx, client.ObjectKeyFromObject(rr), rr); err != nil {
					t.Fatal(err)
				}
				if meta.FindStatusCondition(rr.Status.Conditions, api.ConditionPullRequestUnreadable) == nil {
					t.Fatal("no condition was written")
				}
				// A read that worked removes the condition, and the reconcile that read, retrying nothing, ends its
				// retries (clearGitReadRetry); the HandoffRecovery entry stays.
				if err := r.setUnreadable(ctx, rr, nil); err != nil {
					t.Fatal(err)
				}
				if _, err := r.clearGitReadRetry(ctx, client.ObjectKeyFromObject(rr), true); err != nil {
					t.Fatal(err)
				}
				if err := r.Get(ctx, client.ObjectKeyFromObject(rr), rr); err != nil {
					t.Fatal(err)
				}
			}
			failRecoveryReadsRetryable(g, 0)
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
			if refusedBefore {
				if rr.Status.Phase != api.PhasePolicyChecked || !abortedOnRefusal(rr) {
					t.Fatalf("phase %q, waits %+v: want the aborted handoff watched past the grace", rr.Status.Phase,
						rr.Status.GitWaits)
				}
				return
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
	refuseRecoveryReads(g)
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

// An unrecognised failure of the watch's read of the pull request (an answer that cannot be parsed) is classified as
// changeOf classifies it for any request, a read retried visibly within the HandoffRecovery entry: the reconcile
// waits the poll interval with condition GitReadRetry, never ends the request past the grace, and keeps the
// condition of the refusal (#786, decision of 2026-11-18).
func TestRecoveryRetriesAnUnrecognisedFailureWhileWatching(t *testing.T) {
	r, g, _, rr, _, _ := handoffAppendedWorld(t, nil)
	ctx := context.Background()
	refuseRecoveryReads(g)
	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(rr)}); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(rr), rr); err != nil {
		t.Fatal(err)
	}
	if !abortedOnRefusal(rr) {
		t.Fatal("the handoff was not aborted at the refusal")
	}
	g.SetFail(func(op string) error {
		if op == opGet || op == opList {
			return errors.New("the answer cannot be parsed")
		}
		return nil
	})
	pastDeadline(t, r, rr, handoffRecoveryGrace+time.Minute)
	res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(rr)})
	if err != nil || res.RequeueAfter != DefaultPullRequestPollInterval {
		t.Fatalf("Reconcile = %+v, %v; want the poll interval and no error", res, err)
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(rr), rr); err != nil {
		t.Fatal(err)
	}
	if rr.Status.Phase != api.PhasePolicyChecked {
		t.Fatalf("phase = %q, want PolicyChecked: an unrecognised failure ended a watch", rr.Status.Phase)
	}
	if c := meta.FindStatusCondition(rr.Status.Conditions, api.ConditionGitReadRetry); c == nil ||
		c.Reason != waitHandoffRecovery.reason() {
		t.Fatalf("GitReadRetry = %+v, want the retry shown", c)
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
			failNow: func(g *fakegit.Git) { failRecoveryReadsRetryable(g, 0) }, names: findChangeRead},
		{name: caseLeftSettings, reason: reasonRepositoryUnavailable,
			cause: func(r *RemediationRequestReconciler, _ *fakegit.Git) func() {
				return changeSettings(t, r, func(s *api.RemediationSettings) {
					s.Spec.Repositories[0].URL = otherRepositoryURL
				})
			},
			failNow: func(g *fakegit.Git) { failRecoveryReadsRetryable(g, 0) }, names: findChangeRead},
		{name: "target branch", reason: reasonTargetBranchChanged,
			cause: func(r *RemediationRequestReconciler, _ *fakegit.Git) func() {
				return changeSettings(t, r, func(s *api.RemediationSettings) {
					s.Spec.Repositories[0].TargetBranch = otherTargetBranch
				})
			},
			failNow: func(g *fakegit.Git) { failRecoveryReadsRetryable(g, 0) }, names: findChangeRead},
		{name: "refused branch read answered", reason: ReasonGitForbidden,
			cause: func(_ *RemediationRequestReconciler, g *fakegit.Git) func() {
				refuseRecoveryReads(g)
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
	failRecoveryReadsRetryable(g, 0)
	stale := rr.DeepCopy()
	touched := rr.DeepCopy()
	touched.Annotations = map[string]string{annotationRecoveryTouched: fieldTrue}
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
				refuseRecoveryReads(g)
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

// After the abort a read that works never takes the open pull request over: the replacement records no pull request,
// writes nothing to Git and keeps watching, with one Normal Event for the read that worked (#786, decision of
// 2026-11-18: an aborted intent is not restarted by the same replacement).
func TestRecoveryAfterTheAbortNeverTakesOver(t *testing.T) {
	r, g, _, rr, _, _ := handoffAppendedWorld(t, nil)
	ctx := context.Background()
	rec := record.NewFakeRecorder(10)
	r.Recorder = rec
	refuseRecoveryReads(g)
	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(rr)}); err != nil {
		t.Fatal(err)
	}
	_ = drain(rec)
	var writes []string
	g.SetFail(func(op string) error {
		switch op {
		case opClose, opComment, opCommit, opCreateBranch, "EnableAutoMerge", opOpen:
			writes = append(writes, op)
		}
		return nil
	})
	for range 3 {
		if err := r.Get(ctx, client.ObjectKeyFromObject(rr), rr); err != nil {
			t.Fatal(err)
		}
		if _, done, err := r.recoverHandoff(ctx, rr); err != nil || !done {
			t.Fatalf("recoverHandoff = %v, %v; want the watch to go on", done, err)
		}
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(rr), rr); err != nil {
		t.Fatal(err)
	}
	if rr.Status.Phase != api.PhasePolicyChecked || rr.Status.PullRequest != nil || tookOver(rr) ||
		watchedRecovery(rr) != nil {
		t.Fatalf("phase %q, pull request %+v, conditions %+v", rr.Status.Phase, rr.Status.PullRequest,
			rr.Status.Conditions)
	}
	if len(writes) != 0 {
		t.Fatalf("writes to Git after the abort: %q, want none", writes)
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

// hotfixBranch is the base a person moves the predecessor's pull request to in the tests of a merge on another base.
const hotfixBranch = "hotfix"

// abortedThenMoved is a replacement whose handoff a refused read aborted, with the predecessor's pull request moved by
// a person to hotfixBranch afterwards. It returns the world of handoffAppendedWorld and the writes to Git recorded
// from the move on.
func abortedThenMoved(t *testing.T) (*RemediationRequestReconciler, *fakegit.Git, *api.RemediationRequest,
	*api.RemediationRequest, providers.ChangeRequest, *[]string) {
	t.Helper()
	r, g, p, rr, cr, _ := handoffAppendedWorld(t, nil)
	ctx := context.Background()
	refuseRecoveryReads(g)
	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(rr)}); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(rr), rr); err != nil {
		t.Fatal(err)
	}
	if !abortedOnRefusal(rr) {
		t.Fatalf("waits = %+v: the case is not the one under test", rr.Status.GitWaits)
	}
	writes := &[]string{}
	g.SetFail(func(op string) error {
		if hmWrites[op] || op == opCreateBranch || op == opCloseAnyBase {
			*writes = append(*writes, op)
		}
		return nil
	})
	base, err := g.BranchHead(ctx, mainBranch)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.CreateBranch(ctx, hotfixBranch, base); err != nil {
		t.Fatal(err)
	}
	*writes = nil
	if err := g.SetBase(cr.Number, hotfixBranch); err != nil {
		t.Fatal(err)
	}
	return r, g, p, rr, cr, writes
}

// A recovery wait whose pull request a person moved to another base and merged there ends at once (decision of
// 2026-11-19 on #786, as a cancel with its pull request merged on another base ends): the replacement stops in
// NeedsIntervention with ChangeRequestOnOtherBase, naming the base, its handoff moves to Aborted and its lock is
// released in that write, nothing is written to the pull request, and neither request holds the target, so a later
// request for it can be admitted. Before the change the replacement kept the lock and watched for the rest of its
// HandoffRecovery wait (24 hours).
func TestRecoveryWaitEndsOnAMergeOnAnotherBase(t *testing.T) {
	r, g, p, rr, cr, writes := abortedThenMoved(t)
	ctx := context.Background()
	if _, err := g.MergeWithCommit(cr.Number); err != nil {
		t.Fatal(err)
	}
	res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(rr)})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(rr), rr); err != nil {
		t.Fatal(err)
	}
	c := meta.FindStatusCondition(rr.Status.Conditions, api.ConditionPROpened)
	if rr.Status.Phase != api.PhaseNeedsIntervention || c == nil || c.Reason != ReasonChangeRequestOnOtherBase ||
		!strings.Contains(c.Message, mergedOnHotfix) || !strings.Contains(c.Message, fmt.Sprintf("#%d", cr.Number)) {
		t.Fatalf("phase %q, PullRequestOpened %+v, requeue %v: want the wait ended at once, naming the merge on %s",
			rr.Status.Phase, c, res.RequeueAfter, hotfixBranch)
	}
	if holdsLock(rr) || rr.Status.Handoff.From.Phase != api.HandoffAborted || gitWait(rr, waitHandoffRecovery) != nil {
		t.Fatalf("holds %v, handoff %s, waits %+v: want the lock released and the handoff aborted", holdsLock(rr),
			rr.Status.Handoff.From.Phase, rr.Status.GitWaits)
	}
	if c := meta.FindStatusCondition(rr.Status.Conditions, api.ConditionPullRequestUnreadable); c != nil {
		t.Fatalf("PullRequestUnreadable = %+v after a read that worked", c)
	}
	for range 3 {
		_, _ = r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(p)})
		_, _ = r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(rr)})
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(p), p); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(rr), rr); err != nil {
		t.Fatal(err)
	}
	if holdsLock(p) || holdsLock(rr) || rr.Status.Phase != api.PhaseNeedsIntervention {
		t.Fatalf("predecessor %q holds %v, replacement %q holds %v: want the target free for a later request",
			p.Status.Phase, holdsLock(p), rr.Status.Phase, holdsLock(rr))
	}
	if len(*writes) != 0 {
		t.Fatalf("writes to Git: %q, want none to the pull request merged on another base", *writes)
	}
}

// The neighbours of a merge on another base during a recovery wait keep the wait and the lock (decision of 2026-11-19
// on #786 ends the wait only for a merge there): a pull request closed on another base may be reopened, one open there
// may be moved back and merged, and a read that fails or a 404 the listing does not confirm decides nothing. Each keeps
// the replacement watching in PolicyChecked with the lock, writes nothing to Git, returns no error and a wait no longer
// than the poll interval (an unconfirmed 404 waits for its confirmation); a failed read shows condition PullRequestUnreadable. Once the wait has passed, the end
// (RecoveryWaitExpired) and the looks at stopped holders settle the target: free for the one closed there, held for
// the one open there (by the predecessor, whose own pull request it is: stopOpenElsewhere).
func TestRecoveryWaitKeepsTheLockForTheNeighboursOfAMergeOnAnotherBase(t *testing.T) {
	notFound := fmt.Errorf("status 404: %w", providers.ErrNotFound)
	unavailable := &providers.RetryableError{Err: errors.New("status 503")}
	for _, tc := range []struct {
		name string
		// change is what a person or the host does after the move.
		change func(*testing.T, *fakegit.Git, providers.ChangeRequest)
		// fail makes the reads fail (nil for none).
		fail func(op string) error
		// releasedAfterEnd is whether the target is free once the wait has passed (only read without fail).
		releasedAfterEnd bool
	}{
		{name: "closed there", change: func(t *testing.T, g *fakegit.Git, cr providers.ChangeRequest) {
			if err := g.Close(cr.Number); err != nil {
				t.Fatal(err)
			}
		}, releasedAfterEnd: true},
		{name: "open there", change: func(*testing.T, *fakegit.Git, providers.ChangeRequest) {}},
		{name: "merged there, the read fails", change: mergeThere, fail: func(op string) error {
			if op == opGet {
				return unavailable
			}
			return nil
		}},
		{name: "merged there, a 404 the listing fails to confirm", change: mergeThere, fail: func(op string) error {
			switch op {
			case opGet:
				return notFound
			case opList:
				return unavailable
			}
			return nil
		}},
		{name: "merged there, a 404 the listing contradicts", change: mergeThere, fail: func(op string) error {
			if op == opGet {
				return notFound
			}
			return nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, g, p, rr, cr, writes := abortedThenMoved(t)
			ctx := context.Background()
			tc.change(t, g, cr)
			if tc.fail != nil {
				g.SetFail(func(op string) error {
					if hmWrites[op] || op == opCloseAnyBase {
						*writes = append(*writes, op)
					}
					return tc.fail(op)
				})
			}
			for range 3 {
				res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(rr)})
				if err != nil {
					t.Fatalf("Reconcile: %v, want a wait without an error", err)
				}
				if res.RequeueAfter <= 0 {
					t.Fatalf("RequeueAfter = %v, want a wait", res.RequeueAfter)
				}
			}
			if err := r.Get(ctx, client.ObjectKeyFromObject(rr), rr); err != nil {
				t.Fatal(err)
			}
			if rr.Status.Phase != api.PhasePolicyChecked || !holdsLock(rr) || !abortedOnRefusal(rr) ||
				rr.Status.Handoff.From.Phase != api.HandoffReserved {
				t.Fatalf("phase %q, holds %v, waits %+v, handoff %s: want the replacement still watching with the lock",
					rr.Status.Phase, holdsLock(rr), rr.Status.GitWaits, rr.Status.Handoff.From.Phase)
			}
			unreadable := meta.FindStatusCondition(rr.Status.Conditions, api.ConditionPullRequestUnreadable)
			if (tc.fail != nil) != (unreadable != nil) {
				t.Fatalf("PullRequestUnreadable = %+v, want one exactly while a read fails", unreadable)
			}
			if len(*writes) != 0 {
				t.Fatalf("writes to Git: %q, want none", *writes)
			}
			if tc.fail != nil {
				return
			}
			later := reconcilerClock.Add(recoveryWaitBound + time.Hour)
			r.Now = func() time.Time { return later }
			for range 4 {
				_, _ = r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(p)})
				_, _ = r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(rr)})
			}
			if err := r.Get(ctx, client.ObjectKeyFromObject(rr), rr); err != nil {
				t.Fatal(err)
			}
			if err := r.Get(ctx, client.ObjectKeyFromObject(p), p); err != nil {
				t.Fatal(err)
			}
			if held := holdsLock(rr) || holdsLock(p); rr.Status.Phase != api.PhaseNeedsIntervention ||
				held == tc.releasedAfterEnd {
				t.Fatalf("after the wait: replacement %q holds %v, predecessor %q holds %v; want the target free: %v",
					rr.Status.Phase, holdsLock(rr), p.Status.Phase, holdsLock(p), tc.releasedAfterEnd)
			}
			// The one write allowed after the wait is the predecessor's failed review check on its own head, when its
			// stop reads its pull request open on another base (stopOpenElsewhere, ADR-0045, #925).
			onlyFailedCheck(t, g, *writes, p.Status.PullRequest.HeadSHA)
		})
	}
}

// onlyFailedCheck fails t unless writes holds no write to Git but the failed review check, and that check, when
// written, is the last one on head.
func onlyFailedCheck(t *testing.T, g *fakegit.Git, writes []string, head string) {
	t.Helper()
	if other := slices.DeleteFunc(slices.Clone(writes), func(w string) bool { return w == opCreateCheckRun }); len(other) != 0 {
		t.Fatalf("writes to Git after the wait: %q, want none but a failed review check", writes)
	}
	if slices.Contains(writes, opCreateCheckRun) {
		if last := lastReviewCheck(g.CheckRuns(head)); last == nil || last.Status != providers.CheckRunFailure {
			t.Fatalf("check runs %+v on the predecessor's head: want the failed one last", g.CheckRuns(head))
		}
	}
}

// mergeThere merges cr where it is now, with a merge commit.
func mergeThere(t *testing.T, g *fakegit.Git, cr providers.ChangeRequest) {
	t.Helper()
	if _, err := g.MergeWithCommit(cr.Number); err != nil {
		t.Fatal(err)
	}
}

// The end of a recovery wait on a merge on another base is one status write on the copy read at the start of the
// reconcile: a request changed between that read and the write (an annotation) loses the write to a conflict, which
// is returned for the error backoff as the Kubernetes API's with nothing recorded, and the next reconcile reads the pull
// request again and ends the wait, keeping the annotation.
func TestRecoveryWaitEndOnAnotherBaseSurvivesAConflict(t *testing.T) {
	r, g, _, rr, cr, writes := abortedThenMoved(t)
	ctx := context.Background()
	mergeThere(t, g, cr)
	stale := rr.DeepCopy()
	touched := rr.DeepCopy()
	touched.Annotations = map[string]string{annotationRecoveryTouched: fieldTrue}
	if err := r.Update(ctx, touched); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.observeStep(ctx, stale); !isClusterError(err) || !apierrors.IsConflict(err) {
		t.Fatalf("observeStep = %v, want the conflict of the end returned as the Kubernetes API's", err)
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(rr), rr); err != nil {
		t.Fatal(err)
	}
	if rr.Status.Phase != api.PhasePolicyChecked || !holdsLock(rr) || !abortedOnRefusal(rr) {
		t.Fatalf("phase %q, holds %v: the lost write recorded something", rr.Status.Phase, holdsLock(rr))
	}
	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(rr)}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(rr), rr); err != nil {
		t.Fatal(err)
	}
	c := meta.FindStatusCondition(rr.Status.Conditions, api.ConditionPROpened)
	if rr.Status.Phase != api.PhaseNeedsIntervention || c == nil || c.Reason != ReasonChangeRequestOnOtherBase ||
		holdsLock(rr) || rr.Annotations[annotationRecoveryTouched] != fieldTrue {
		t.Fatalf("phase %q, PullRequestOpened %+v, holds %v, annotations %v", rr.Status.Phase, c, holdsLock(rr),
			rr.Annotations)
	}
	if len(*writes) != 0 {
		t.Fatalf("writes to Git: %q, want none", *writes)
	}
}

// TestMergedStopDoesNotKeepSayingThePullRequestIsOpen (#902): a request that ends Merged on a path that did not
// mark its pull request merged does not keep "Pull request #N is open" in PullRequestOpened.
func TestMergedStopDoesNotKeepSayingThePullRequestIsOpen(t *testing.T) {
	r, _, _, rr, _, _ := handoffAppendedWorld(t, nil)
	ctx := context.Background()
	number := int64(42)
	rr.Status.PullRequest = &api.PullRequestStatus{Number: number}
	meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{Type: api.ConditionPROpened,
		Status: metav1.ConditionTrue, Reason: ReasonPullRequestOpened, Message: fmt.Sprintf("Pull request #%d is open", number)})
	if err := r.stop(ctx, rr, api.PhaseMerged, ReasonChangeRequestMerged, "The pull request was merged"); !errors.Is(err, errStopped) {
		t.Fatal(err)
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(rr), rr); err != nil {
		t.Fatal(err)
	}
	c := meta.FindStatusCondition(rr.Status.Conditions, api.ConditionPROpened)
	want := fmt.Sprintf("Pull request #%d was merged", number)
	if c == nil || c.Message != want || c.Reason != ReasonChangeRequestMerged {
		t.Fatalf("PullRequestOpened = %+v, want reason %s and message %q", c, ReasonChangeRequestMerged, want)
	}
}
