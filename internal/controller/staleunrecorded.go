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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/policy/freshness"
)

// This file holds what a stale plan's cleanup through a new installation of the GitHub App (#290) shows while it
// fails for a request with no pull request recorded (#808): the cleanup lists the request's branch to find its pull
// request, so a failed token or listing says the pull requests cannot be read (PullRequestUnreadable), and a close
// that does not go through says the pull request found there is not closed (PullRequestNotClosed). A request with a
// recorded pull request is cleanUpStale's other cases (#797).

// unrecordedFailure returns the condition to show for err, a failure of a stale plan's cleanup through a new
// installation of its repository, when rr has no pull request recorded and the failure is not a refusal for good:
//   - the token, the listing of the branch or what the listing answered (reinstalledFailure) gives
//     PullRequestUnreadable, reason GitNotFound for a 404 and GitFailed for any other failure, naming the step;
//   - the close of the pull request found on the branch, or a step no other class names (reinstalledCloseFailure),
//     gives PullRequestNotClosed with the same reasons.
//
// ok is false for anything else: a request with a recorded pull request, a refusal (403 or 422: a refused close is
// unrecordedRefused's, a refused step before it ends the request NeedsIntervention), a failure of the Kubernetes API
// (a *clusterError, never wrapped in a *reinstalledError), a pause and the caller's cancellation.
func unrecordedFailure(rr *remediationv1alpha1.RemediationRequest, err error) (metav1.Condition, bool) {
	if rr.Status.PullRequest != nil {
		return metav1.Condition{}, false
	}
	if s, refused, ok := reinstalledFailure(err); ok {
		if refused {
			return metav1.Condition{}, false
		}
		return unrecordedUnreadable(rr, s), true
	}
	if s, ok := reinstalledCloseFailure(err); ok {
		return unrecordedNotClosed(rr, s), true
	}
	return metav1.Condition{}, false
}

// unrecordedStepText says which step s was, for a request with no pull request recorded: there is no read by
// number, so a failed read is what the listing of the branch answered.
func unrecordedStepText(rr *remediationv1alpha1.RemediationRequest, s refusedStep) string {
	if s.step == stepRead {
		s.step = stepListing
	}
	return stepText(rr, s)
}

// unrecordedHow says how step s failed through the new installation, with its reason.
func unrecordedHow(rr *remediationv1alpha1.RemediationRequest, s refusedStep) string {
	if s.reason == ReasonGitNotFound {
		return fmt.Sprintf("the Git host answered not found (%s) to %s", s.reason, unrecordedStepText(rr, s))
	}
	return fmt.Sprintf("%s failed (%s)", unrecordedStepText(rr, s), s.reason)
}

// unrecordedUnreadable is condition PullRequestUnreadable for the stale plan of rr, with no pull request recorded,
// whose cleanup through the new installation could not read the pull requests of its branch at step s (the token or
// the listing), with reason GitNotFound or GitFailed. The message names the step, never the Git host's answer.
func unrecordedUnreadable(rr *remediationv1alpha1.RemediationRequest, s refusedStep) metav1.Condition {
	return metav1.Condition{
		Type:   remediationv1alpha1.ConditionPullRequestUnreadable,
		Status: metav1.ConditionTrue,
		Reason: s.reason,
		Message: fmt.Sprintf("The plan is stale and the settings now reach its repository through another "+
			"installation of the GitHub App; %s, so the pull requests of branch %s cannot be read and no pull "+
			"request is recorded; Shadewright tries again and closes the request's open pull request once they can "+
			"be read", unrecordedHow(rr, s), freshness.BranchName(rr)),
		ObservedGeneration: rr.Generation,
	}
}

// unrecordedNotClosed is condition PullRequestNotClosed for the stale plan of rr, with no pull request recorded,
// whose cleanup through the new installation failed at step s (the close of the pull request found on the branch,
// or a step no other class names) without a refusal, with reason GitNotFound or GitFailed.
func unrecordedNotClosed(rr *remediationv1alpha1.RemediationRequest, s refusedStep) metav1.Condition {
	return metav1.Condition{
		Type:   remediationv1alpha1.ConditionPullRequestNotClosed,
		Status: metav1.ConditionTrue,
		Reason: s.reason,
		Message: fmt.Sprintf("The plan is stale and the settings now reach its repository through another "+
			"installation of the GitHub App; %s, so the request's pull request on branch %s, which no status "+
			"recorded, is not known to be closed; Shadewright tries again and records a merge or a close by a person",
			unrecordedHow(rr, s), freshness.BranchName(rr)),
		ObservedGeneration: rr.Generation,
	}
}

// errUnrecordedCloseRefused is cleanUpStale's answer when the Git host refused for good to close the pull request
// of a stale plan found on its branch with no pull request recorded (#808, decision of 2026-11-16): the request
// keeps its phase, the mark and the lock, and refuseOrWait retries it at the error backoff (or after the delay the
// host asked for) instead of ending it. It wraps the refusal, which is not a final phase here.
var errUnrecordedCloseRefused = errors.New("the Git host refused to close the stale plan's pull request found on " +
	"its branch")

// unrecordedCleanup reports whether rr is a stale plan's cleanup (the mark) with no pull request recorded.
func unrecordedCleanup(rr *remediationv1alpha1.RemediationRequest) bool {
	return rr.Status.PullRequest == nil && staleMark(rr) != nil
}

// unrecordedRefused is condition PullRequestNotClosed for the stale plan of rr, with no pull request recorded, whose
// close of pull request cf, found on its branch, the Git host refused for good with reason (GitForbidden,
// GitRejected), through the plan's installation or a new one. The message names the pull request, never the Git
// host's answer.
func unrecordedRefused(rr *remediationv1alpha1.RemediationRequest, cf *closeFailure, reason string) metav1.Condition {
	return metav1.Condition{
		Type:   remediationv1alpha1.ConditionPullRequestNotClosed,
		Status: metav1.ConditionTrue,
		Reason: reason,
		Message: fmt.Sprintf("The plan is stale and the Git host refused (%s) to close pull request %s on branch %s, "+
			"which no status recorded; the target stays locked, Shadewright tries the close again and ends the "+
			"request once the pull request is closed, by anyone, or merged", reason,
			pullRequestName(cf.number, cf.url), freshness.BranchName(rr)),
		ObservedGeneration: rr.Generation,
	}
}

// setUnrecorded sets want, PullRequestUnreadable or PullRequestNotClosed, in the status of rr about to be written
// (setRefused), and removes the other one: with no pull request recorded, the cleanup shows one of them, never both
// (#808). It reports whether that changed the status, and whether the Event of want is due.
func (r *RemediationRequestReconciler) setUnrecorded(rr *remediationv1alpha1.RemediationRequest,
	want metav1.Condition) (changed, event bool) {
	other := remediationv1alpha1.ConditionPullRequestNotClosed
	if want.Type == remediationv1alpha1.ConditionPullRequestNotClosed {
		other = remediationv1alpha1.ConditionPullRequestUnreadable
	}
	removed := meta.RemoveStatusCondition(&rr.Status.Conditions, other)
	changed, event = r.setRefused(rr, want)
	return changed || removed, event
}

// noteUnrecorded records want, the condition unrecordedFailure or unrecordedRefused gave for err, in the status of rr
// when it changes, removes the other of PullRequestUnreadable and PullRequestNotClosed in the same write
// (setUnrecorded), with one Warning Event (named after the condition's type) when it is set or its reason changes,
// logs the retry,
// and returns err, so the caller retries as before (waitOrFail: the error backoff, or the delay the host asked
// for). The write changes nothing but that condition, so it wakes no reconcile of its own (skipOwnMarks: a
// PullRequestNotClosed, or a PullRequestUnreadable with reason GitFailed or GitNotFound). A conflict reads the
// request again from the API server and writes there, as long as it is the same request with the mark and still no
// pull request recorded. A write that still fails is returned with err; the next look sets the condition again.
func (r *RemediationRequestReconciler) noteUnrecorded(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	want metav1.Condition, err error) error {
	changed, event := r.setUnrecorded(rr, want)
	if changed {
		werr := r.Status().Update(ctx, rr)
		if apierrors.IsConflict(werr) {
			werr = retry.RetryOnConflict(retry.DefaultBackoff, func() error {
				fresh := &remediationv1alpha1.RemediationRequest{}
				if err := r.reader().Get(ctx, client.ObjectKeyFromObject(rr), fresh); err != nil {
					return fmt.Errorf("read the request again: %w", err)
				}
				if fresh.UID != rr.UID || staleMark(fresh) == nil || fresh.Status.PullRequest != nil {
					return errStaleMarkGone
				}
				var again bool
				if again, event = r.setUnrecorded(fresh, want); !again {
					fresh.DeepCopyInto(rr)
					return nil
				}
				if err := r.Status().Update(ctx, fresh); err != nil {
					return err
				}
				fresh.DeepCopyInto(rr)
				return nil
			})
		}
		if werr != nil {
			return errors.Join(err, fmt.Errorf("update status of RemediationRequest %s/%s: %w",
				rr.Namespace, rr.Name, werr))
		}
		if event {
			r.refusedEvent(rr, want.Type, want)
		}
	}
	logf.FromContext(ctx).Info("Could not clean up a stale RemediationRequest with no pull request recorded through "+
		"the new installation of its repository; trying again", "remediationRequest", client.ObjectKeyFromObject(rr),
		"condition", want.Type, "reason", want.Reason, "error", err.Error())
	return err
}

// clearReinstalledUnreadable removes, from the status about to be written as a final phase, condition
// PullRequestUnreadable with reason GitFailed or GitNotFound: only a read through a new installation of the GitHub
// App sets those (reinstalledRead, unrecordedUnreadable), and no read of a request that ended goes through that
// installation, so nothing would ever remove it again (#808).
func clearReinstalledUnreadable(rr *remediationv1alpha1.RemediationRequest) {
	if c := meta.FindStatusCondition(rr.Status.Conditions, remediationv1alpha1.ConditionPullRequestUnreadable); c != nil &&
		(c.Reason == ReasonGitFailed || c.Reason == ReasonGitNotFound) {
		meta.RemoveStatusCondition(&rr.Status.Conditions, remediationv1alpha1.ConditionPullRequestUnreadable)
	}
}
