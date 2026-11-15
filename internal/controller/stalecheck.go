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

	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/policy/freshness"
	"github.com/pkalin/shadewright/internal/providers"
)

// The failed check run of a stale plan (#713, ADR-0028 amendment of 2026-11-15). With reviewer hints on, the check
// run shadewright/review is the merge check a ruleset may require. A check that went green before the plan went
// stale would stay green while the Git host refuses the close (#199), so a person could merge the stale plan on it.
// cleanUpStale therefore completes the check on the recorded head as failed, before each try of the close:
//
//   - once per request: state Stale on the recorded head (status.pullRequest.reviewCheck) says it is done, and a
//     request that records it never writes the check again (reviewCheck), so it never goes green again. Until Stale
//     is recorded each try writes the failed run again, without reading what the Git host shows first: a second
//     failed run after a lost status write is harmless, and no read can keep the write from happening;
//   - whether or not the close then goes through: a closed pull request with a failed check is harmless, but one
//     whose check is still green could be reopened and merged on it, so the request does not end Superseded while
//     the check is pending (staleCheckPending): it is tried again every poll interval (errStaleCheckPending), with
//     no other bound, as the refused close is;
//   - a failed or refused write, or a lost status write, changes nothing else: it is logged, the close goes on,
//     and the next try of the close tries the check again, with no backoff of its own and no Event;
//   - the provider opened for the stale plan lets any failed check run shadewright/review through, and no other
//     write but the close (gatedGit.CreateCheckRun).

// staleCheckTitle is the title of the failed check run of a stale plan.
const staleCheckTitle = "The plan is stale"

// errStaleCheckPending is the answer of a stale plan's cleanup whose pull request is closed while the failed check
// is not recorded yet (staleCheckPending): the request keeps its phase and the mark, and the check and the end are
// tried again after the poll interval (refuseOrWait, endStaleClosed). It is not a final phase.
var errStaleCheckPending = errors.New("the review check of the stale plan's closed pull request is not failed yet")

// staleCheckRun is the failed check run shadewright/review of a plan found stale with reason, one of staleReasons.
func staleCheckRun(reason string) providers.CheckRun {
	return providers.CheckRun{Name: providers.CheckRunName, Status: providers.CheckRunFailure, Title: staleCheckTitle,
		Summary: "Shadewright found the plan of this pull request stale (" + reason + "): what it was decided on " +
			"has changed since, so it must not be merged. Shadewright closes the pull request; if the Git host " +
			"refuses the close, a person closes it. This check stays failed."}
}

// staleCheckPending reports whether a stale plan's check run still has to be failed: reviewer hints on, a recorded
// pull request this request still owns (not handed over, no handoff intent), and no state Stale on its head.
func (r *RemediationRequestReconciler) staleCheckPending(rr *remediationv1alpha1.RemediationRequest) bool {
	pr := rr.Status.PullRequest
	if !r.ReviewerHints || pr == nil || handedOff(rr) || intent(rr) != nil {
		return false
	}
	rc := pr.ReviewCheck
	return rc == nil || rc.Head != pr.HeadSHA || rc.State != remediationv1alpha1.ReviewCheckStale
}

// failStaleCheck completes the check run shadewright/review on the recorded head of a stale plan's pull request as
// failed and records state Stale (see above). It does nothing and returns nil when the check is not pending
// (staleCheckPending). A failure is logged and returned, but for a cancelled reconcile and a pause, which are only
// returned. The step has a deadline of its own, so it never eats the close's.
func (r *RemediationRequestReconciler) failStaleCheck(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	reason string) error {
	if !r.staleCheckPending(rr) {
		return nil
	}
	pr := rr.Status.PullRequest
	stepCtx, cancel := bound(ctx, r.WriteTimeout, DefaultWriteTimeout)
	defer cancel()
	err := r.failStaleCheckStep(stepCtx, rr, reason)
	if err == nil || ctx.Err() != nil || errors.Is(err, freshness.ErrPaused) {
		// A pause is the close's to report (waitWhilePaused).
		return err
	}
	logf.FromContext(ctx).Info("Could not fail the review check of a stale RemediationRequest; tried again with "+
		"the close", "remediationRequest", client.ObjectKeyFromObject(rr), "pullRequest", pr.Number,
		"head", pr.HeadSHA, "error", err.Error())
	return err
}

// failStaleCheckStep is failStaleCheck's work: it writes the failed check, then records Stale.
func (r *RemediationRequestReconciler) failStaleCheckStep(ctx context.Context,
	rr *remediationv1alpha1.RemediationRequest, reason string) error {
	git, err := r.openPlanRepository(ctx, rr)
	if err != nil {
		return err
	}
	if err := git.CreateCheckRun(ctx, rr.Status.PullRequest.HeadSHA, staleCheckRun(reason)); err != nil {
		return freshness.WrapProvider("write the check run", err)
	}
	return r.recordStaleCheck(ctx, rr)
}

// recordStaleCheck records state Stale for the recorded head in rr's status, through the kill switch. A reason of
// condition Approved about the check (ReviewCheckRefused, ReviewListTooLong) no longer applies once the check is
// failed for the stale plan, so it gives way to WaitingForReview, as in recordReviewCheck; PlanFresh says why the
// request will not be merged. rr is changed only when the write went through.
func (r *RemediationRequestReconciler) recordStaleCheck(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) error {
	if err := r.writeAllowed(ctx); err != nil {
		return err
	}
	upd := rr.DeepCopy()
	pr := upd.Status.PullRequest
	pr.ReviewCheck = &remediationv1alpha1.ReviewCheck{Head: pr.HeadSHA, State: remediationv1alpha1.ReviewCheckStale}
	if staleReviewCheckWarning(upd) {
		setWaitingForReview(upd)
	}
	if err := r.Status().Update(ctx, upd); err != nil {
		return fmt.Errorf("update status of RemediationRequest %s/%s: %w", rr.Namespace, rr.Name, err)
	}
	upd.DeepCopyInto(rr)
	logf.FromContext(ctx).Info("Failed the review check of a stale RemediationRequest", "remediationRequest",
		client.ObjectKeyFromObject(rr), "pullRequest", pr.Number, "head", pr.HeadSHA)
	return nil
}
