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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/policy/freshness"
	"github.com/pkalin/shadewright/internal/providers"
)

// This file holds what happens when the Git host refuses for good (403 or 422) a step of closing a pull request
// that Shadewright must close (#199, #288): the close of a stale plan's pull request, and the cleanup of a deleted
// request by the finalizer (the token for the repository, the read of the pull request, the listing of the branch,
// the close). refusedCleanup is the one place that decides whether a failed step is such a refusal, and which
// step it was. Condition PullRequestNotClosed records a refused close of a stale plan, condition CleanupRefused a
// refused step of a deletion.

const (
	// EventPullRequestNotClosed is the Warning Event recorded when condition PullRequestNotClosed is set, or its
	// reason changes.
	EventPullRequestNotClosed = "PullRequestNotClosed"
	// EventCleanupRefused is the Warning Event recorded when condition CleanupRefused is set, or its reason
	// changes.
	EventCleanupRefused = "CleanupRefused"
	// EventPullRequestLeftOpen is the Warning Event recorded when the finalizer lets a deleted request go although
	// the Git host kept refusing a step of its cleanup until closeRefusedGrace had passed.
	EventPullRequestLeftOpen = "PullRequestLeftOpen"
)

// cleanupStep names a step of closing a request's pull request that the Git host may refuse.
type cleanupStep int

const (
	// stepToken is opening the repository: the token of the Git App for it.
	stepToken cleanupStep = iota
	// stepRead is the read of the recorded pull request by its number.
	stepRead
	// stepListing is the listing of the pull requests of the request's branch.
	stepListing
	// stepClose is the close call itself.
	stepClose
)

// stepFailure is the error of a step before the close that is not the read by number: opening the repository
// (stepToken) or listing the branch (stepListing). It wraps the provider error and adds no text.
type stepFailure struct {
	step cleanupStep
	err  error
}

func (e *stepFailure) Error() string { return e.err.Error() }

func (e *stepFailure) Unwrap() error { return e.err }

// listFailed wraps err, a failed listing of the branch's pull requests (op says which), as a *stepFailure.
func listFailed(op string, err error) error {
	return &stepFailure{step: stepListing, err: freshness.WrapProvider(op, err)}
}

// closeFailure is the error of a call that closes pull request number (url as the Git host reported it), so the
// caller can name the pull request it could not close. It wraps the provider error.
type closeFailure struct {
	number int64
	url    string
	err    error
}

func (e *closeFailure) Error() string { return e.err.Error() }

func (e *closeFailure) Unwrap() error { return e.err }

// closeFailed wraps err, the error of closing cr, as a *closeFailure; nil stays nil.
func closeFailed(cr providers.ChangeRequest, err error) error {
	if err == nil {
		return nil
	}
	return &closeFailure{number: cr.Number, url: cr.URL, err: freshness.WrapProvider("close the change request", err)}
}

// refusedStep is a step that the Git host refused for good: which step, the reason (GitForbidden or GitRejected),
// and, for stepClose, the pull request whose close was refused.
type refusedStep struct {
	step   cleanupStep
	reason string
	closed *closeFailure
}

// refusedCleanup is the one place that decides whether err, the failure of closing a request's pull request, is a
// refusal for good of one of its steps (refusal: providers.ErrForbidden or ErrRejected, by class, never by text),
// and of which: the close call (a *closeFailure), opening the repository or listing the branch (a *stepFailure),
// or the read of the pull request (any other failed read, isUnknownChange). A failure worth a retry (a 5xx, a
// timeout, a rate limit), the caller's cancellation and the step's deadline (which boundErr has replaced by then)
// are never refusals, and neither is a failed read without such a class (an answer about another pull request,
// two reads that disagree).
func refusedCleanup(err error) (refusedStep, bool) {
	reason, _, ok := refusal(err)
	if !ok {
		return refusedStep{}, false
	}
	if cf, ok := errors.AsType[*closeFailure](err); ok && !isUnknownChange(err) {
		return refusedStep{step: stepClose, reason: reason, closed: cf}, true
	}
	if sf, ok := errors.AsType[*stepFailure](err); ok {
		return refusedStep{step: sf.step, reason: reason}, true
	}
	if isUnknownChange(err) {
		return refusedStep{step: stepRead, reason: reason}, true
	}
	return refusedStep{}, false
}

// refusedClose returns the failed close in err, and the reason, when the Git host refused the close call itself
// for good (refusedCleanup with stepClose). A refused read, listing or token is not a refused close.
func refusedClose(err error) (*closeFailure, string, bool) {
	s, ok := refusedCleanup(err)
	if !ok || s.step != stepClose {
		return nil, "", false
	}
	return s.closed, s.reason, true
}

// closeRefusedGrace is how long after the first refusal of a step of a deleted request's cleanup (the
// lastTransitionTime of CleanupRefused, on the executor's clock) the finalizer keeps trying, before it lets the
// request go and leaves the pull request as it is (#288).
const closeRefusedGrace = handoffRecoveryGrace

// closeRefusedRetry is how often the finalizer tries the cleanup again while the Git host refuses a step of it,
// within closeRefusedGrace.
const closeRefusedRetry = time.Minute

// notClosedCondition is condition PullRequestNotClosed for the close of a stale plan's pull request cf, refused
// with reason.
func notClosedCondition(rr *remediationv1alpha1.RemediationRequest, cf *closeFailure, reason string) metav1.Condition {
	return metav1.Condition{
		Type:   remediationv1alpha1.ConditionPullRequestNotClosed,
		Status: metav1.ConditionTrue,
		Reason: reason,
		Message: fmt.Sprintf("The plan is stale and the Git host refused (%s) to close pull request %s; Shadewright "+
			"writes nothing else to it, tries the close again on every look and records a merge or a close by a person",
			reason, pullRequestName(cf.number, cf.url)),
		ObservedGeneration: rr.Generation,
	}
}

// stepText says which step s was, for a message.
func stepText(rr *remediationv1alpha1.RemediationRequest, s refusedStep) string {
	switch s.step {
	case stepToken:
		return "the access token for the plan's repository"
	case stepListing:
		return "the listing of the pull requests of branch " + freshness.BranchName(rr)
	case stepClose:
		return "the close of pull request " + pullRequestName(s.closed.number, s.closed.url)
	}
	return "the read of the recorded pull request"
}

// recordedText names the recorded pull request of rr, for a message, or says there is none.
func recordedText(rr *remediationv1alpha1.RemediationRequest) string {
	if pr := rr.Status.PullRequest; pr != nil {
		return "the recorded pull request is " + pullRequestName(pr.Number, pr.URL)
	}
	return "no pull request is recorded"
}

// cleanupRefusedCondition is condition CleanupRefused for the refused step s of a deleted request's cleanup.
func cleanupRefusedCondition(rr *remediationv1alpha1.RemediationRequest, s refusedStep) metav1.Condition {
	return metav1.Condition{
		Type:   remediationv1alpha1.ConditionCleanupRefused,
		Status: metav1.ConditionTrue,
		Reason: s.reason,
		Message: fmt.Sprintf("The Git host refused (%s) %s while Shadewright cleans up the deleted request (%s); "+
			"Shadewright tries again and, %s after the first refusal, lets the request go", s.reason, stepText(rr, s),
			recordedText(rr), ShortDuration(closeRefusedGrace)),
		ObservedGeneration: rr.Generation,
	}
}

// pullRequestName is "#<number>", with the URL the Git host reported for it when there is one.
func pullRequestName(number int64, url string) string {
	if url == "" {
		return fmt.Sprintf("#%d", number)
	}
	return fmt.Sprintf("#%d (%s)", number, url)
}

// notClosed returns condition PullRequestNotClosed of rr, nil when it is not True.
func notClosed(rr *remediationv1alpha1.RemediationRequest) *metav1.Condition {
	c := meta.FindStatusCondition(rr.Status.Conditions, remediationv1alpha1.ConditionPullRequestNotClosed)
	if c == nil || c.Status != metav1.ConditionTrue {
		return nil
	}
	return c
}

// setRefused sets want, a True condition of a refusal (PullRequestNotClosed or CleanupRefused), in the status
// about to be written and reports whether that changed it, and whether the Event that goes with it is due after
// the write (it is new, or its reason changed). The transition time is the executor's clock when the condition is
// new; while it stays True it is kept, whatever the reason and message become (a lastTransitionTime is never
// rewritten without a change of status).
func (r *RemediationRequestReconciler) setRefused(rr *remediationv1alpha1.RemediationRequest,
	want metav1.Condition) (changed, event bool) {
	was := meta.FindStatusCondition(rr.Status.Conditions, want.Type)
	if was == nil || was.Status != metav1.ConditionTrue {
		want.LastTransitionTime = metav1.NewTime(r.now())
	}
	event = was == nil || was.Status != metav1.ConditionTrue || was.Reason != want.Reason
	if !meta.SetStatusCondition(&rr.Status.Conditions, want) {
		return false, false
	}
	return true, event
}

// clearNotClosed removes condition PullRequestNotClosed from the status about to be written.
func clearNotClosed(rr *remediationv1alpha1.RemediationRequest) {
	meta.RemoveStatusCondition(&rr.Status.Conditions, remediationv1alpha1.ConditionPullRequestNotClosed)
}

// refusedEvent records a Warning Event with reason and the condition's message. Without a Recorder it records
// none.
func (r *RemediationRequestReconciler) refusedEvent(rr *remediationv1alpha1.RemediationRequest, reason string,
	c metav1.Condition) {
	if r.Recorder != nil {
		r.Recorder.Event(rr, corev1.EventTypeWarning, reason, c.Message)
	}
}

// cleanupRefusedOnDelete handles a step of the finalizer's cleanup that the Git host refused for good
// (refusedCleanup): it records condition CleanupRefused (with one Warning Event when it is set or its reason
// changes) and keeps the finalizer, trying the cleanup again every closeRefusedRetry, until closeRefusedGrace has
// passed since the first refusal after the deletion began (the condition's lastTransitionTime, on the executor's
// clock; the condition is only ever set on a deleted request). Then release is true: the caller lets the request
// go and reports it (leftOpen). A failed status write is returned for a retry and starts nothing.
func (r *RemediationRequestReconciler) cleanupRefusedOnDelete(ctx context.Context,
	rr *remediationv1alpha1.RemediationRequest, s refusedStep) (reconcile.Result, bool, error) {
	want := cleanupRefusedCondition(rr, s)
	changed, event := r.setRefused(rr, want)
	if changed {
		if err := r.Status().Update(ctx, rr); err != nil {
			return reconcile.Result{}, false, fmt.Errorf("update status of RemediationRequest %s/%s: %w",
				rr.Namespace, rr.Name, err)
		}
		if event {
			r.refusedEvent(rr, EventCleanupRefused, want)
		}
		logf.FromContext(ctx).Info("The Git host refused a step of the cleanup of a deleted RemediationRequest",
			"remediationRequest", client.ObjectKeyFromObject(rr), "step", stepText(rr, s), "reason", s.reason)
	}
	since := meta.FindStatusCondition(rr.Status.Conditions, remediationv1alpha1.ConditionCleanupRefused).
		LastTransitionTime.Time
	left := since.Add(closeRefusedGrace).Sub(r.now())
	if left > 0 {
		return reconcile.Result{RequeueAfter: min(left, closeRefusedRetry)}, false, nil
	}
	return reconcile.Result{}, true, nil
}

// leftOpen reports a deleted request that the finalizer lets go after the Git host kept refusing step s of its
// cleanup (cleanupRefusedOnDelete): a Warning Event PullRequestLeftOpen on the request and a log line, naming the
// pull request by its number and URL, never the Git host's answer. When the close itself was refused the pull
// request is left open; when an earlier step was, its state is unknown and it may be open. The Event is a
// core/v1 Event in the request's namespace with no owner, so it stays there for the API server's Event time to
// live after the request is gone.
func (r *RemediationRequestReconciler) leftOpen(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	s refusedStep) {
	var message string
	if s.step == stepClose {
		message = fmt.Sprintf("The Git host kept refusing (%s) to close pull request %s for %s; the deleted request "+
			"was let go and the pull request is left open: close it by hand", s.reason,
			pullRequestName(s.closed.number, s.closed.url), ShortDuration(closeRefusedGrace))
	} else {
		which := "any pull request of branch " + freshness.BranchName(rr)
		if pr := rr.Status.PullRequest; pr != nil {
			which = "pull request " + pullRequestName(pr.Number, pr.URL)
		}
		message = fmt.Sprintf("The Git host kept refusing (%s) %s for %s; the deleted request was let go and "+
			"nothing was closed: the state of %s is unknown and it may be open, check it and close it by hand",
			s.reason, stepText(rr, s), ShortDuration(closeRefusedGrace), which)
	}
	if r.Recorder != nil {
		r.Recorder.Event(rr, corev1.EventTypeWarning, EventPullRequestLeftOpen, message)
	}
	logf.FromContext(ctx).Info("Gave up the cleanup of a deleted RemediationRequest after the Git host kept "+
		"refusing it", "remediationRequest", client.ObjectKeyFromObject(rr), "step", stepText(rr, s),
		"reason", s.reason)
}
