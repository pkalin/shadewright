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

// This file holds what happens when the Git host refuses for good (403 or 422) to close a pull request that
// Shadewright must close (#199, #288): the close of a stale plan's pull request, and the close of a deleted
// request's pull request by the finalizer. refusedClose is the one place that decides whether a failed close is
// such a refusal; condition PullRequestNotClosed records it.

const (
	// EventPullRequestNotClosed is the Warning Event recorded when condition PullRequestNotClosed is set, or its
	// reason changes.
	EventPullRequestNotClosed = "PullRequestNotClosed"
	// EventPullRequestLeftOpen is the Warning Event recorded when the finalizer lets a deleted request go although
	// the Git host kept refusing to close its pull request until closeRefusedGrace had passed.
	EventPullRequestLeftOpen = "PullRequestLeftOpen"
)

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

// refusedClose returns the failed close in err, and the reason (GitForbidden or GitRejected), when the Git host
// refused the close call itself for good (refusal). A failed read of a pull request (isUnknownChange) is never a
// refused close, whatever its class, and neither is a failure worth a retry (a 5xx, a timeout, a rate limit),
// the caller's cancellation or the step's deadline, which boundErr has replaced by then.
func refusedClose(err error) (*closeFailure, string, bool) {
	if err == nil || isUnknownChange(err) {
		return nil, "", false
	}
	cf, ok := errors.AsType[*closeFailure](err)
	if !ok {
		return nil, "", false
	}
	reason, _, ok := refusal(err)
	if !ok {
		return nil, "", false
	}
	return cf, reason, true
}

// mayHaveClosed reports whether err, a failed close of a stale plan's pull request that was not refused, may have
// closed it anyway: the close call failed in a way worth a retry (its answer may be lost), or the step reached its
// deadline or was cancelled while it may have been running. A close refused as a conflict closed nothing.
func mayHaveClosed(err error) bool {
	if err == nil || errors.Is(err, freshness.ErrPaused) || providers.IsConflict(err) {
		// The kill switch stopped the call before it reached the Git host, or the host refused the close as a
		// conflict (a merged pull request): nothing was closed.
		return false
	}
	if _, ok := errors.AsType[*closeFailure](err); ok {
		return true
	}
	return errors.Is(err, ErrTimeout) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// closeRefusedGrace is how long after the first refusal (the lastTransitionTime of PullRequestNotClosed, on the
// executor's clock) the finalizer of a deleted request keeps trying to close a pull request the Git host refuses
// to close, before it lets the request go and leaves the pull request open (#288).
const closeRefusedGrace = handoffRecoveryGrace

// closeRefusedRetry is how often the finalizer tries the close again while the Git host refuses it, within
// closeRefusedGrace.
const closeRefusedRetry = time.Minute

// notClosedCondition is condition PullRequestNotClosed for the close of cf refused with reason. deleting says
// whether it is the finalizer's close.
func notClosedCondition(rr *remediationv1alpha1.RemediationRequest, cf *closeFailure, reason string,
	deleting bool) metav1.Condition {
	var message string
	if deleting {
		message = fmt.Sprintf("The Git host refused (%s) to close pull request %s of the deleted request; Shadewright "+
			"tries again and, %s after the first refusal, lets the request go and leaves the pull request open",
			reason, pullRequestName(cf), ShortDuration(closeRefusedGrace))
	} else {
		message = fmt.Sprintf("The plan is stale and the Git host refused (%s) to close pull request %s; Shadewright "+
			"writes nothing else to it, tries the close again on every look and records a merge or a close by a person",
			reason, pullRequestName(cf))
	}
	return metav1.Condition{
		Type:               remediationv1alpha1.ConditionPullRequestNotClosed,
		Status:             metav1.ConditionTrue,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: rr.Generation,
	}
}

// pullRequestName is "#<number>", with the URL the Git host reported for it when there is one.
func pullRequestName(cf *closeFailure) string {
	if cf.url == "" {
		return fmt.Sprintf("#%d", cf.number)
	}
	return fmt.Sprintf("#%d (%s)", cf.number, cf.url)
}

// notClosed returns condition PullRequestNotClosed of rr, nil when it is not True.
func notClosed(rr *remediationv1alpha1.RemediationRequest) *metav1.Condition {
	c := meta.FindStatusCondition(rr.Status.Conditions, remediationv1alpha1.ConditionPullRequestNotClosed)
	if c == nil || c.Status != metav1.ConditionTrue {
		return nil
	}
	return c
}

// setNotClosed sets condition PullRequestNotClosed to want in the status about to be written and reports whether
// that changed it, with the Event that goes with a change after the write (notClosedEvent). The transition time
// is the executor's clock: set when the condition is new, kept while it stays True, and set again when the
// recorded one lies after now (the clock went back), so the grace is never measured from a time ahead of the
// clock.
func (r *RemediationRequestReconciler) setNotClosed(rr *remediationv1alpha1.RemediationRequest,
	want metav1.Condition) (changed, event bool) {
	now := r.now()
	was := notClosed(rr)
	if was != nil && was.LastTransitionTime.After(now) {
		meta.RemoveStatusCondition(&rr.Status.Conditions, remediationv1alpha1.ConditionPullRequestNotClosed)
		was = nil
	}
	event = was == nil || was.Reason != want.Reason
	if was == nil {
		want.LastTransitionTime = metav1.NewTime(now)
	}
	if !meta.SetStatusCondition(&rr.Status.Conditions, want) {
		return false, false
	}
	return true, event
}

// clearNotClosed removes condition PullRequestNotClosed from the status about to be written and reports whether
// it was there.
func clearNotClosed(rr *remediationv1alpha1.RemediationRequest) bool {
	return meta.RemoveStatusCondition(&rr.Status.Conditions, remediationv1alpha1.ConditionPullRequestNotClosed)
}

// notClosedEvent records the Warning Event PullRequestNotClosed with the condition's message. Without a Recorder
// it records none.
func (r *RemediationRequestReconciler) notClosedEvent(rr *remediationv1alpha1.RemediationRequest, c metav1.Condition) {
	if r.Recorder != nil {
		r.Recorder.Event(rr, corev1.EventTypeWarning, EventPullRequestNotClosed, c.Message)
	}
}

// closeRefusedOnDelete handles the finalizer's close that the Git host refused for good (refusedClose): it records
// condition PullRequestNotClosed on the first refusal (with one Warning Event) and keeps the finalizer, trying the
// close again every closeRefusedRetry, until closeRefusedGrace has passed since that first refusal on the
// executor's clock. Then release is true: the caller lets the request go and reports the pull request left open
// (leftOpen). A failed status write is returned for a retry and starts nothing.
func (r *RemediationRequestReconciler) closeRefusedOnDelete(ctx context.Context,
	rr *remediationv1alpha1.RemediationRequest, cf *closeFailure, reason string) (reconcile.Result, bool, error) {
	want := notClosedCondition(rr, cf, reason, true)
	changed, event := r.setNotClosed(rr, want)
	if changed {
		if err := r.Status().Update(ctx, rr); err != nil {
			return reconcile.Result{}, false, fmt.Errorf("update status of RemediationRequest %s/%s: %w",
				rr.Namespace, rr.Name, err)
		}
		if event {
			r.notClosedEvent(rr, want)
		}
		logf.FromContext(ctx).Info("The Git host refused to close the pull request of a deleted RemediationRequest",
			"remediationRequest", client.ObjectKeyFromObject(rr), "pullRequest", cf.number, "reason", reason)
	}
	since := notClosed(rr).LastTransitionTime.Time
	left := since.Add(closeRefusedGrace).Sub(r.now())
	if left > 0 {
		return reconcile.Result{RequeueAfter: min(left, closeRefusedRetry)}, false, nil
	}
	return reconcile.Result{}, true, nil
}

// leftOpen reports a deleted request that the finalizer let go with its pull request open (closeRefusedOnDelete):
// a Warning Event PullRequestLeftOpen on the request and a log line, both naming the pull request by its number
// and URL, never the Git host's answer. The Event is a core/v1 Event in the request's namespace with no owner, so
// it stays there for the API server's Event time to live after the request is gone.
func (r *RemediationRequestReconciler) leftOpen(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	cf *closeFailure, reason string) {
	message := fmt.Sprintf("The Git host kept refusing (%s) to close pull request %s for %s; the deleted request "+
		"was let go and the pull request is left open: close it by hand", reason, pullRequestName(cf),
		ShortDuration(closeRefusedGrace))
	if r.Recorder != nil {
		r.Recorder.Event(rr, corev1.EventTypeWarning, EventPullRequestLeftOpen, message)
	}
	logf.FromContext(ctx).Info("Released the finalizer of RemediationRequest with its pull request left open",
		"remediationRequest", client.ObjectKeyFromObject(rr), "pullRequest", cf.number, "url", cf.url,
		"reason", reason)
}
