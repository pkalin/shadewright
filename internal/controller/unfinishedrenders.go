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
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
)

// ReasonRenderNeverFinished: the render check of the plan started remediationv1alpha1.MaxUnfinishedRenders times
// in a row and never finished, because the executor ended with it each time (#714, ADR-0035). The request ends in
// NeedsIntervention with it and no further render is started.
const ReasonRenderNeverFinished = "RenderNeverFinished"

// renderMarkClearTimeout bounds the status write that removes the mark once the render check returned. It is
// not bound by the step's context: a step whose deadline or cancellation ended the render still learned that
// the render did not take the executor down, so the mark goes (ADR-0035).
const renderMarkClearTimeout = 15 * time.Second

// messageRenderNeverFinished is the fixed text of the RenderNeverFinished end and its Event.
var messageRenderNeverFinished = fmt.Sprintf("The render check of this plan started %d times in a row and never "+
	"finished: the executor ended while it ran each time (ADR-0035)", remediationv1alpha1.MaxUnfinishedRenders)

// unfinishedRenders returns how many render checks of rr's current plan started in a row and did not finish, as
// status.unfinishedRenders records them. A mark for another plan digest counts as zero.
func unfinishedRenders(rr *remediationv1alpha1.RemediationRequest) int32 {
	m := rr.Status.UnfinishedRenders
	if m == nil || m.PlanDigest != rr.Status.PlanDigest {
		return 0
	}
	return m.Count
}

// markRender records in rr's status that a render check of its plan starts, before the render process starts
// (#714, ADR-0035): the count of status.unfinishedRenders for the plan digest goes up by one, in its own status
// write. At MaxUnfinishedRenders it starts nothing and returns a [*renderRefusal] with reason
// RenderNeverFinished, and records a Warning Event. A failed write is returned for a retry with the status as
// it was, and the render does not start: an unrecorded render could take the executor down without a count.
func (r *RemediationRequestReconciler) markRender(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) error {
	n := unfinishedRenders(rr)
	if n >= remediationv1alpha1.MaxUnfinishedRenders {
		if r.Recorder != nil {
			r.Recorder.Event(rr, corev1.EventTypeWarning, ReasonRenderNeverFinished, messageRenderNeverFinished)
		}
		return &renderRefusal{reason: ReasonRenderNeverFinished, message: messageRenderNeverFinished}
	}
	before := rr.Status.UnfinishedRenders
	rr.Status.UnfinishedRenders = &remediationv1alpha1.UnfinishedRenders{PlanDigest: rr.Status.PlanDigest, Count: n + 1}
	if err := r.Status().Update(ctx, rr); err != nil {
		rr.Status.UnfinishedRenders = before
		return fmt.Errorf("record the start of the render check of RemediationRequest %s/%s: %w",
			rr.Namespace, rr.Name, err)
	}
	return nil
}

// unmarkRender removes status.unfinishedRenders once the render check returned, whatever it returned, after
// applying passed (when not nil) to rr, so a caller that records the pass in status needs no write of its own.
// It writes with a deadline of its own that the step's context does not end. A failed write is returned for a
// retry; the outcome of the render is then not used, and the next attempt renders again.
func (r *RemediationRequestReconciler) unmarkRender(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	passed func(*remediationv1alpha1.RemediationRequest)) error {
	if passed != nil {
		passed(rr)
	}
	rr.Status.UnfinishedRenders = nil
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), renderMarkClearTimeout)
	defer cancel()
	if err := r.Status().Update(wctx, rr); err != nil {
		return fmt.Errorf("record the end of the render check of RemediationRequest %s/%s: %w",
			rr.Namespace, rr.Name, err)
	}
	return nil
}
