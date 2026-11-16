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
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crevent "sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
)

// ReasonRenderNeverFinished: the render check of the plan started remediationv1alpha1.MaxUnfinishedRenders times
// in a row and never finished, because the executor ended with it each time (#714, ADR-0035). The request ends in
// NeedsIntervention with it and no further render is started; a replacement in a handoff ends Cancelled with
// HandoffAborted and a message that names it.
const ReasonRenderNeverFinished = "RenderNeverFinished"

// renderMarkClearTimeout bounds the removal of the mark once the render check returned, its retries included.
// It is not bound by the step's context: a step whose deadline or cancellation ended the render still learned
// that the render did not take the executor down, so the mark goes (ADR-0035).
const renderMarkClearTimeout = 15 * time.Second

// messageRenderNeverFinished is the fixed text of the RenderNeverFinished end and its Event.
var messageRenderNeverFinished = fmt.Sprintf("The render check of this plan started %d times in a row and never "+
	"finished: the executor ended while it ran each time (ADR-0035)", remediationv1alpha1.MaxUnfinishedRenders)

// executorProcess is this process's identity in status.unfinishedRenders.process: 128 random bits drawn when
// the process starts, so a restarted container, a new Pod and a new leader are each another process.
var executorProcess = newProcessID()

// newProcessID draws a process identity of 32 lowercase hexadecimal characters (the schema's pattern).
func newProcessID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never returns an error; it ends the program instead
	return hex.EncodeToString(b[:])
}

// errMarkChanged: the mark of a render that returned is no longer the one the executor wrote when it removes
// it after a conflict. The outcome of the render is not used and the step is retried.
var errMarkChanged = errors.New("status.unfinishedRenders changed while the render check ran")

// processID returns the identity this reconciler writes into a mark.
func (r *RemediationRequestReconciler) processID() string {
	if r.process != "" {
		return r.process
	}
	return executorProcess
}

// unfinishedRenders returns how many render checks of rr's current plan started in a row and did not finish, as
// status.unfinishedRenders records them. A mark for another plan digest counts as zero, and so does a mark of
// process, the running executor process: it is alive, so its render returned and only the removal was lost.
func unfinishedRenders(rr *remediationv1alpha1.RemediationRequest, process string) int32 {
	m := rr.Status.UnfinishedRenders
	if m == nil || m.PlanDigest != rr.Status.PlanDigest || m.Process == process {
		return 0
	}
	return m.Count
}

// markRender records in rr's status that a render check of its plan starts, before the render process starts
// (#714, ADR-0035): the count of status.unfinishedRenders for the plan digest goes up by one, in its own status
// write, with this process's identity. At MaxUnfinishedRenders it starts nothing and returns a [*renderRefusal]
// with reason RenderNeverFinished, and records a Warning Event. A mark this process wrote itself is of a render
// that returned (its removal was lost): it is removed first, in a write of its own, and the count starts at 1.
// A failed write is returned for a retry with the status as it was, and the render does not start: an
// unrecorded render could take the executor down without a count.
func (r *RemediationRequestReconciler) markRender(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) error {
	self := r.processID()
	n := unfinishedRenders(rr, self)
	if n >= remediationv1alpha1.MaxUnfinishedRenders {
		if r.Recorder != nil {
			r.Recorder.Event(rr, corev1.EventTypeWarning, ReasonRenderNeverFinished, messageRenderNeverFinished)
		}
		return &renderRefusal{reason: ReasonRenderNeverFinished, message: messageRenderNeverFinished}
	}
	before := rr.Status.UnfinishedRenders
	if before != nil && before.PlanDigest == rr.Status.PlanDigest && before.Process == self {
		// The schema lets a count for the same plan only rise by one, so the finished render's mark goes first.
		rr.Status.UnfinishedRenders = nil
		if err := r.Status().Update(ctx, rr); err != nil {
			rr.Status.UnfinishedRenders = before
			return fmt.Errorf("remove the mark of a finished render check of RemediationRequest %s/%s: %w",
				rr.Namespace, rr.Name, err)
		}
		before = nil
	}
	want := remediationv1alpha1.UnfinishedRenders{PlanDigest: rr.Status.PlanDigest, Count: n + 1, Process: self}
	mark := want
	rr.Status.UnfinishedRenders = &mark
	if err := r.Status().Update(ctx, rr); err != nil {
		rr.Status.UnfinishedRenders = before
		return fmt.Errorf("record the start of the render check of RemediationRequest %s/%s: %w",
			rr.Namespace, rr.Name, err)
	}
	// The write answers with the object as stored. A CustomResourceDefinition older than this executor does
	// not know the field: the API server drops it and the write still succeeds, so success alone proves nothing.
	if got := rr.Status.UnfinishedRenders; got == nil || *got != want {
		return fmt.Errorf("record the start of the render check of RemediationRequest %s/%s: %w",
			rr.Namespace, rr.Name, errMarkNotStored)
	}
	return nil
}

// errMarkNotStored is markRender's error when the status write went through and the stored request does not
// hold the mark: the render does not start, and the reconcile is tried again.
var errMarkNotStored = errors.New("the API server did not store status.unfinishedRenders " +
	"(apply the CustomResourceDefinitions of this release)")

// unmarkRender removes status.unfinishedRenders once the render check returned, whatever it returned, after
// applying passed (when not nil) to rr, so a caller that records the pass in status needs no write of its own;
// passed returns false when the request holds no place for the pass, which is an error for a retry.
// It writes with a deadline of its own that the step's context does not end. A write that conflicts (the
// object changed while the render ran: a label, an annotation) reads the request again and removes the mark
// there, with passed applied again, as long as the mark is still the one markRender wrote; rr is then the
// request as written. A failed write, a failed read, retries used up, or a mark that changed are returned for
// a retry; the outcome of the render is then not used, and the next attempt renders again (markRender does
// not count a mark of its own process).
func (r *RemediationRequestReconciler) unmarkRender(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	passed func(*remediationv1alpha1.RemediationRequest) bool) error {
	mark := rr.Status.UnfinishedRenders.DeepCopy()
	digest := rr.Status.PlanDigest
	if passed != nil && !passed(rr) {
		return fmt.Errorf("record the end of the render check of RemediationRequest %s/%s: the pass cannot be "+
			"recorded", rr.Namespace, rr.Name)
	}
	rr.Status.UnfinishedRenders = nil
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), renderMarkClearTimeout)
	defer cancel()
	err := r.Status().Update(wctx, rr)
	if apierrors.IsConflict(err) {
		err = retry.RetryOnConflict(retry.DefaultBackoff, func() error {
			return r.unmarkFresh(wctx, rr, mark, digest, passed)
		})
	}
	if err != nil {
		return fmt.Errorf("record the end of the render check of RemediationRequest %s/%s: %w",
			rr.Namespace, rr.Name, err)
	}
	return nil
}

// unmarkFresh is one retry of unmarkRender after a conflict: it reads the request from the API server and
// removes the mark there if it is still the mark of this render, for the plan digest it checked.
func (r *RemediationRequestReconciler) unmarkFresh(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	mark *remediationv1alpha1.UnfinishedRenders, digest string, passed func(*remediationv1alpha1.RemediationRequest) bool) error {
	var reader client.Reader = r.Client
	if r.APIReader != nil {
		reader = r.APIReader
	}
	fresh := &remediationv1alpha1.RemediationRequest{}
	if err := reader.Get(ctx, client.ObjectKeyFromObject(rr), fresh); err != nil {
		return fmt.Errorf("read the request again: %w", err)
	}
	m := fresh.Status.UnfinishedRenders
	if fresh.UID != rr.UID || fresh.Status.PlanDigest != digest || m == nil || mark == nil || *m != *mark ||
		(passed != nil && !passed(fresh)) {
		return errMarkChanged
	}
	fresh.Status.UnfinishedRenders = nil
	if err := r.Status().Update(ctx, fresh); err != nil {
		return err
	}
	fresh.DeepCopyInto(rr)
	return nil
}

// skipOwnMarks drops the watch events of an update that changed nothing of a request but status.unfinishedRenders,
// condition PullRequestNotClosed and condition PullRequestUnreadable with reason GitFailed or GitNotFound, or with
// any reason on a replacement whose handoff recovery waits (recovering, #786), set or
// removed, PullRequestOpened recorded closed in the write that removes PullRequestNotClosed (closedRecorded), and
// the metadata every write changes. Those are the executor's own writes: the mark and its removal
// around a render, the condition that says why a stale plan's pull request is not closed (keepWatchingStale,
// noteNotReached) and its removal with the close (recordStaleClosed), and the one that says a read through a new installation of the GitHub App failed (noteRead,
// noteLockUnreadable), and each such reconcile returns its own delay. Without the filter
// each would queue the request again at once, before the error backoff, the step delay or a Git host's Retry-After
// that the reconcile returned, so a step that fails after a render (a commit the host refuses) would render again
// with no delay (#714), and a cleanup the host asked to wait would be tried again at once (#797).
var skipOwnMarks = predicate.Funcs{UpdateFunc: func(e crevent.UpdateEvent) bool {
	return !onlyOwnMarksChanged(e.ObjectOld, e.ObjectNew)
}}

// onlyOwnMarksChanged reports whether newObj differs from oldObj, and only in status.unfinishedRenders, condition
// PullRequestNotClosed, condition PullRequestUnreadable while neither side has another reason than GitFailed or
// GitNotFound (reinstalledUnreadable) or both sides are a replacement recovering its handoff (recovering), the
// resource version and the managed fields. Anything that is not a request
// is never such an update.
func onlyOwnMarksChanged(oldObj, newObj client.Object) bool {
	o, ok1 := oldObj.(*remediationv1alpha1.RemediationRequest)
	n, ok2 := newObj.(*remediationv1alpha1.RemediationRequest)
	if !ok1 || !ok2 || o == nil || n == nil {
		return false
	}
	o, n = o.DeepCopy(), n.DeepCopy()
	for _, rr := range []*remediationv1alpha1.RemediationRequest{o, n} {
		rr.ResourceVersion = ""
		rr.ManagedFields = nil
	}
	if equality.Semantic.DeepEqual(o, n) {
		return false
	}
	if closedRecorded(o, n) {
		for _, rr := range []*remediationv1alpha1.RemediationRequest{o, n} {
			meta.RemoveStatusCondition(&rr.Status.Conditions, remediationv1alpha1.ConditionPROpened)
		}
	}
	for _, rr := range []*remediationv1alpha1.RemediationRequest{o, n} {
		rr.Status.UnfinishedRenders = nil
		meta.RemoveStatusCondition(&rr.Status.Conditions, remediationv1alpha1.ConditionPullRequestNotClosed)
	}
	if (reinstalledUnreadable(o) && reinstalledUnreadable(n)) || (recovering(o) && recovering(n)) {
		for _, rr := range []*remediationv1alpha1.RemediationRequest{o, n} {
			meta.RemoveStatusCondition(&rr.Status.Conditions, remediationv1alpha1.ConditionPullRequestUnreadable)
		}
	}
	return equality.Semantic.DeepEqual(o, n)
}

// closedRecorded reports whether n is recordStaleClosed's write over o: PullRequestNotClosed True in o and gone in
// n, and PullRequestOpened with reason PullRequestClosed in n. Only that write removes the condition and records the
// close without ending the request (#797); an end changes the phase too, which the caller still compares.
func closedRecorded(o, n *remediationv1alpha1.RemediationRequest) bool {
	c := meta.FindStatusCondition(n.Status.Conditions, remediationv1alpha1.ConditionPROpened)
	return notClosed(o) != nil && meta.FindStatusCondition(n.Status.Conditions,
		remediationv1alpha1.ConditionPullRequestNotClosed) == nil && c != nil && c.Reason == ReasonPullRequestClosed
}

// recovering reports whether rr is a replacement whose handoff recovery runs on every look (recoverHandoff): in
// PolicyChecked, with its incoming handoff Reserved. While it cannot read, only watchRecovery, its replacement of a
// cause that is gone (nowUnreadable) and the recovery's removal write condition PullRequestUnreadable on it (#786), and the reconcile that wrote it returns its own delay.
func recovering(rr *remediationv1alpha1.RemediationRequest) bool {
	return rr.Spec.Replaces != nil && rr.Status.Phase == remediationv1alpha1.PhasePolicyChecked &&
		rr.Status.Handoff != nil && rr.Status.Handoff.From != nil &&
		rr.Status.Handoff.From.Phase == remediationv1alpha1.HandoffReserved
}

// reinstalledUnreadable reports whether rr has no condition PullRequestUnreadable, or one with reason GitFailed or
// GitNotFound: only a read through a new installation of the GitHub App sets those (reinstalledRead, #797).
func reinstalledUnreadable(rr *remediationv1alpha1.RemediationRequest) bool {
	c := meta.FindStatusCondition(rr.Status.Conditions, remediationv1alpha1.ConditionPullRequestUnreadable)
	return c == nil || c.Reason == ReasonGitFailed || c.Reason == ReasonGitNotFound
}
