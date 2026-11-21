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
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/policy/freshness"
)

// claimPath is how a path that reaches a held claim paces it; what is decided is settleClaim's alone.
type claimPath struct {
	// pace is the time to read the claim again when a read settled nothing, never past the end of a wait the request
	// records (recoveryPace), so never past the claim's own end.
	pace time.Duration
	// bound is the wait a failed read of the claim is retried within, besides the claim's own (retryGitRead).
	bound waitBound
}

// claimDuringWatch is the path of a replacement that watches after the abort of its handoff (observeStep).
func (r *RemediationRequestReconciler) claimDuringWatch() claimPath {
	return claimPath{pace: r.pullRequestPollInterval(), bound: boundBy(waitHandoffRecovery)}
}

// claimAfterWatch is the path of a replacement that ended before its takeover and keeps the lock (heldRecovery).
func claimAfterWatch() claimPath {
	return claimPath{pace: heldRecheckInterval, bound: unboundedRead(noBoundHeldLock)}
}

// settleClaim is the one reader of a held claim (otherHeadClaim: rr holds the target lock before its takeover and a
// MergeContent wait about the merge of the pull request it was to take over, merged with another head), whichever path
// reaches it: during the watch after the abort (observeStep) and after its end (heldRecovery), whether the predecessor
// still hands over, is gone, was made again or no longer names rr (decision of 2026-11-21 on #1058). It reads the
// claimed pull request by the claim's own number on the chain's branch (changeOf, with the open pull requests of the
// branch listed, so an answer that the listing contradicts is no answer, and the request's marker asked for, the
// predecessor's counting as its own), through the plan's repository as a stopped holder's read opens it
// (openStoppedRead), and decides by the claim verdicts only:
//   - merged into the target branch with the head the claim names: recorded on rr as a foreign change
//     (recordOtherHeadMerge), after the read of what the merge brought; the lock is released after that record;
//   - the Git host answers and the answers disagree (a 404 by number while the branch lists it, a merged pull request
//     the listing has open, an answer about a pull request not asked for, another merged head than the claim's):
//     nothing is written, the claim stands, and rr is read again at the path's pace; at the claim's end the wait ends
//     MergeContentUnreadable saying the answers disagreed (endAfterLastRead);
//   - any other answer (closed, gone, the marker taken out, open, merged or closed on another base): nothing is written, the claim and the
//     lock stand until the claim's own end, which records the merge MergeContentUnreadable saying the reads answered
//     and nothing matched; never the generic look, which can release the lock without a record;
//   - a read that fails, also the read of what the merge brought and the open of the repository: retried visibly
//     (retryGitRead, condition GitReadRetry, and condition PullRequestUnreadable for a refusal or settings that cannot
//     reach the repository), never past the claim's end, which then says the reads kept failing; a pause waits.
//
// recorded is true when a record was written. Only a failure of the Kubernetes API, or the end of ctx, is returned.
func (r *RemediationRequestReconciler) settleClaim(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	via claimPath) (res reconcile.Result, recorded bool, err error) {
	claim := gitWait(rr, waitMergeContent).PullRequest
	pred, err := r.claimPredecessor(ctx, rr, claim.Number)
	if err != nil {
		return reconcile.Result{}, false, err
	}
	readCtx, cancel := bound(ctx, r.CleanupTimeout, DefaultCleanupTimeout)
	defer cancel()
	git, err := r.openStoppedRead(readCtx, rr)
	if err != nil {
		res, err := r.claimReadFailed(ctx, rr, via, recoveryStepErr(ctx, readCtx, err))
		return res, false, err
	}
	inheriting := rr.DeepCopy()
	inheriting.Status.Handoff.From.Phase = remediationv1alpha1.HandoffDone
	v := r.changeOf(readCtx, git, inheriting, changeQuery{number: claim.Number, marker: true, openOnBranch: true})
	// changeOf may record PullRequestUnreadable through the copy: rr takes that write, never the copy's handoff.
	rr.ResourceVersion, rr.Status.Conditions = inheriting.ResourceVersion, inheriting.Status.Conditions
	key := client.ObjectKeyFromObject(rr)
	switch {
	case v.state == changeUnknown:
		res, err := r.claimReadFailed(ctx, rr, via, recoveryStepErr(ctx, readCtx, v.err))
		return res, false, err
	case v.state == changeMerged && v.pr.Number == claim.Number && v.pr.HeadSHA == claim.HeadSHA:
		switch err := recoveryStepErr(ctx, readCtx, r.recordOtherHeadMerge(readCtx, git, rr, pred, v.pr)); {
		case err == nil, errors.Is(err, errStopped):
			return reconcile.Result{RequeueAfter: stepDelay}, true, nil
		default:
			res, err := r.claimReadFailed(ctx, rr, via, err)
			return res, false, err
		}
	case v.state == changeMerged:
		// Another merged head than the one the claim's first read stored: a pull request merges once, so the answers
		// about it disagree.
		r.gitRetries().noteDisagreed(key)
	}
	keptClaimLog(ctx, rr, v)
	res, err = r.recoveryPace(ctx, rr, via.pace)
	return res, false, err
}

// claimPredecessor is the request rr replaces, read from the API server, when it hands over to rr and records the
// claimed pull request (number): the takeover in the record of the merge carries its hints over. Any other (gone, made
// again, no longer naming rr, recording another number) is named only, as the record of a merge whose predecessor is
// gone names it. A failed read is the Kubernetes API's (a *clusterError).
func (r *RemediationRequestReconciler) claimPredecessor(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	number int64) (*remediationv1alpha1.RemediationRequest, error) {
	from := rr.Status.Handoff.From
	named := &remediationv1alpha1.RemediationRequest{}
	named.Name, named.Namespace = string(from.Name), rr.Namespace
	pred := &remediationv1alpha1.RemediationRequest{}
	switch err := r.getLive(ctx, types.NamespacedName{Namespace: rr.Namespace, Name: string(from.Name)}, pred); {
	case apierrors.IsNotFound(err):
		return named, nil
	case err != nil:
		return nil, &clusterError{err: fmt.Errorf("get the RemediationRequest %s replaces: %w", rr.Name, err)}
	}
	var to *remediationv1alpha1.OutgoingHandoff
	if pred.Status.Handoff != nil {
		to = pred.Status.Handoff.To
	}
	if !handsOverClaimed(rr, pred, to) || pred.Status.PullRequest.Number != number {
		return named, nil
	}
	return pred, nil
}

// claimReadFailed is settleClaim's answer to err, a read of the claim that did not answer:
//   - the end of the reconcile's own context is returned as it is, and a pause waits (waitWhilePaused);
//   - a failure of the Kubernetes API is returned, for the error backoff;
//   - answers that disagree are no failed call: the claim stands with nothing written, read again at the path's pace;
//   - anything else is a failed Git read: a final refusal or settings that cannot reach the repository set condition
//     PullRequestUnreadable that names it (recoveryUnreadable), and the read is retried visibly within the claim and
//     the path's bound (retryGitRead), never past the claim's end.
func (r *RemediationRequestReconciler) claimReadFailed(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	via claimPath, err error) (reconcile.Result, error) {
	switch {
	case ctx.Err() != nil:
		return reconcile.Result{}, err
	case errors.Is(err, freshness.ErrPaused):
		// Before the Kubernetes API's failures: the pause the record's write meets is marked as one of them.
		return r.waitWhilePaused(ctx, rr)
	case isClusterError(err):
		return reconcile.Result{}, err
	case answeredUnknown(err):
		// changeOf noted the disagreement on rr's key, unless the answer came from a read below it.
		r.gitRetries().noteDisagreed(client.ObjectKeyFromObject(rr))
		return r.recoveryPace(ctx, rr, via.pace)
	}
	if want := recoveryUnreadable(rr, err); want != nil {
		if werr := r.setUnreadable(ctx, rr, want); werr != nil {
			return reconcile.Result{}, errors.Join(err, werr)
		}
	}
	read := recoveryRead(err)
	if !gitReadFailure(read) {
		read = readFailure(readOp(err), err)
	}
	res, err := r.retryGitRead(ctx, rr, read, via.bound)
	if err != nil {
		return reconcile.Result{}, err
	}
	// The path's pace at the least, never past the end of a wait rr records.
	return r.recoveryPace(ctx, rr, max(res.RequeueAfter, via.pace))
}
