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
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/controller/lockverdict"
	"github.com/pkalin/shadewright/internal/policy/freshness"
	"github.com/pkalin/shadewright/internal/providers"
)

// The supersede handoff (ADR-0028 section 4, #539). A replacement (spec.replaces, spec.chainRoot) takes over the
// pull request and the branch shadewright/<chainRoot> of the request it replaces, its predecessor, instead of
// opening a pull request of its own; the hint never closes the pull request and never opens another.
//
//   - Admission (chainLink, check): before anything else is read about it, the full chain link is checked once:
//     the predecessor exists with the UID named, is not being deleted, names the same Incident, target and chain
//     root, waits for review (AwaitingApproval) with a recorded pull request that is open as far as its status
//     says, has no handoff of its own that stands or is done (one that aborted does not count), and holds the
//     named hint Pending. A link that fails is Denied (ChainLinkInvalid) and takes nothing over. The limits then
//     count the predecessor neither as a holder of the target lock nor as an active request (checkLimits), so the
//     replacement takes its lock and slot; the admission's status write sets holdsTarget and status.handoff.from
//     (Reserved) together (step 1). Only a replacement admitted so frees a slot for another request
//     (admittedPredecessor).
//   - Step 2 (reserve): one status write on the predecessor sets status.handoff.to (Intent, the branch head it
//     expects; it replaces an aborted one of an earlier replacement) and its hint Reserved, with the
//     resourceVersion read, so only a successor that read the hint Pending reserves it. A conflict is read again;
//     a predecessor that left AwaitingApproval, or whose hint is no longer Pending or is reserved for another,
//     ends the replacement Cancelled (HandoffRefused).
//   - Step 3 (takeOver): after the freshness check, the render check and the branch rule (#524): the branch head
//     is the expected head and carries only the predecessor's chain of changes to the target file, which holds
//     exactly the predecessor's plan on the target file now, and the new plan renders as planned. The commit's
//     content must also be handoffContent: the predecessor's value in the file at the expected head changed to the
//     new plan's. The pull request is read (it must be open), the branch moves by one commit on top of the
//     expected head (AppendCommit, a compare-and-swap), and the pull request is read again. A head that is not the
//     expected one is accepted only when it is that commit (isHandoffCommit): its one parent is the expected head,
//     the chain holds only changes to the target file and the file is handoffContent, which depends on the two
//     specs and the expected head only, so a target branch that moved since does not hide it. Anything else, a
//     merge or a close around the update with the old head, a predecessor being deleted, or a plan that would
//     change another file, ends the replacement Cancelled (HandoffAborted), and the predecessor stays as it is.
//   - Recovery (recoverHandoff): a replacement whose update happened before a restart or a lost write finds its
//     commit before the freshness check, and goes on to the read after the update, so neither the freshness
//     check nor the deadline takes its commit, or a merge of it, for a stale plan.
//   - Hints off (ADR-0028 amendment of 2026-11-10): an executor running without --reviewer-hints Denies every
//     replacement at admission (HintsTurnedOff) and ends one admitted before at its next step, the recovery
//     included, without taking anything over (a commit pushed before the restart is not recorded, so the
//     predecessor stops as for a foreign push); the predecessor declines the hint and aborts its intent
//     (declineHintsOff). A replacement that recorded the takeover before stops NeedsIntervention
//     (hintsOffTakenOver, from afterHandoff and the watch), without step 5, step 6 or AwaitingApproval.
//   - Step 4 (recordHandoff): the replacement records the pull request with the new head, the hints of the chain
//     (each consumed one, the one it answers among them, the predecessor's other Pending hints, which it
//     answers anew since the cursor it carries is past their comments, and its Declined ones, kept as they are
//     so that their authors stay out of the approvers that count) and the predecessor's comment cursor, sets
//     from Done and moves to PRCreated; a merge right after the update whose merged head is the new commit makes
//     it Merged.
//   - Step 5 (supersedePredecessor): the predecessor ends Superseded with reason HandedOff, its to Done, its hint
//     Consumed and holdsTarget false. It is the one Superseded whose pull request may be open (prMayBeOpen).
//   - Step 6 (followHandoff): while the pull request waits, its title and body are rewritten for the new plan
//     with the replacement's marker, recorded in status.handoff.from as Done or Refused; a refusal is never
//     recorded as done, and a body the Git host did not rewrite keeps the marker of a request further up the
//     chain, which the replacement keeps accepting (ownsMarker, #591).
//   - The check run and the merge gate are not the handoff's: like on every head of a Shadewright pull request,
//     the new head starts without status.pullRequest.reviewCheck, and the look at the pull request creates the
//     check run on it (reviewcheck.go). The hints carried over (all of the chain, whatever became of them) keep
//     their authors out of the approvers that count.
//
// Each step is one write, idempotent, and a restart finds where it stopped from the two statuses and the
// branch. Every write, in Git and in status, passes the kill switch first (writeAllowed, gatedGit); while
// writes are paused the handoff waits at its recorded step. The predecessor's side (settleHandoff, called with
// its look at the pull request) aborts an intent whose replacement is gone or ended without taking over, and
// declines a Pending hint whose replacements all ended without the handoff (ReplacementDenied when one was
// Denied, else HandoffAborted); a request that ends for any other reason aborts its intent in the write that
// ends it (declineOnEnd). While its intent stands, the predecessor reads no new comments, writes nothing to Git
// (no freshness check, so no close of a stale plan; recheck), never closes the pull request (closeChangeRequest,
// and its finalizer waits for the replacement: handoffHolds), and its watch takes a head that is the
// replacement's commit (expectedHandoffHead) for the expected one instead of HeadMoved.

// msgHintsTurnedOff starts the message of a replacement refused or ended because the executor runs with
// reviewer hints off (remediationv1alpha1.ReasonHintsTurnedOff).
const msgHintsTurnedOff = "Reviewer hints are turned off in the executor (flag --reviewer-hints)"

// Hint reasons of the predecessor's side of a handoff.
const (
	// ReasonHintReplacementDenied: the replacement that answered the hint was Denied by policy, so the pull
	// request keeps the old plan.
	ReasonHintReplacementDenied = "ReplacementDenied"
	// ReasonHintHandoffAborted: the replacement that answered the hint ended without taking the pull request
	// over.
	ReasonHintHandoffAborted = "HandoffAborted"
)

// handedOff reports whether the request ended Superseded because a replacement took its pull request over.
func handedOff(rr *remediationv1alpha1.RemediationRequest) bool {
	ho := rr.Status.Handoff
	return ho != nil && ho.To != nil && ho.To.Phase == remediationv1alpha1.HandoffDone
}

// tookOver reports whether the request took over its predecessor's pull request (handoff step 4).
func tookOver(rr *remediationv1alpha1.RemediationRequest) bool {
	ho := rr.Status.Handoff
	return ho != nil && ho.From != nil && ho.From.Phase == remediationv1alpha1.HandoffDone
}

// intent returns the outgoing side of a handoff in phase Intent, or nil.
func intent(rr *remediationv1alpha1.RemediationRequest) *remediationv1alpha1.OutgoingHandoff {
	if ho := rr.Status.Handoff; ho != nil && ho.To != nil && ho.To.Phase == remediationv1alpha1.HandoffIntent {
		return ho.To
	}
	return nil
}

// chainDepth is how many commits of its own rr's branch carries: one for the first request of a chain and one
// per hint the chain consumed, which a replacement carries in its status (recordHandoff).
func chainDepth(rr *remediationv1alpha1.RemediationRequest) int {
	n := 1
	for _, h := range rr.Status.Hints {
		if h.State == remediationv1alpha1.HintConsumed {
			n++
		}
	}
	return min(n, providers.MaxChainCommits)
}

// refOf is the pin of a stored hint.
func refOf(h *remediationv1alpha1.Hint) remediationv1alpha1.HintRef {
	return remediationv1alpha1.HintRef{Source: h.Source, CommentID: h.CommentID, Digest: h.Digest}
}

// hintByRef returns the stored hint ref pins, or nil.
func hintByRef(rr *remediationv1alpha1.RemediationRequest, ref remediationv1alpha1.HintRef) *remediationv1alpha1.Hint {
	for i := range rr.Status.Hints {
		if refOf(&rr.Status.Hints[i]) == ref {
			return &rr.Status.Hints[i]
		}
	}
	return nil
}

// abortIntent ends a handoff whose intent still stands without it: to Aborted, its Reserved hint Declined with
// reason. It changes nothing on a request without an intent.
func abortIntent(rr *remediationv1alpha1.RemediationRequest, reason string) {
	to := intent(rr)
	if to == nil {
		return
	}
	to.Phase = remediationv1alpha1.HandoffAborted
	if h := hintByRef(rr, to.Hint); h != nil && h.State == remediationv1alpha1.HintReserved {
		h.State, h.Reason = remediationv1alpha1.HintDeclined, reason
	}
}

// chainRootOf is the chain root of a request: spec.chainRoot, or its own UID for the first of a chain.
func chainRootOf(rr *remediationv1alpha1.RemediationRequest) remediationv1alpha1.UID {
	if rr.Spec.ChainRoot != "" {
		return rr.Spec.ChainRoot
	}
	return remediationv1alpha1.UID(rr.UID)
}

// chainLink checks the full chain link of a replacement, once, at the policy check that admits it (ADR-0028
// section 4 item 2), before anything else is read about it. It returns the refusal's reason and message, ""
// when the link holds or rr replaces nothing, and an error only for a read worth retrying. With reviewer hints
// off every replacement is refused (ReasonHintsTurnedOff, ADR-0028 amendment of 2026-11-10), whatever its link.
func (r *RemediationRequestReconciler) chainLink(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) (
	reason, message string, err error) {
	rep := rr.Spec.Replaces
	if rep == nil {
		return "", "", nil
	}
	if !r.ReviewerHints {
		return remediationv1alpha1.ReasonHintsTurnedOff, msgHintsTurnedOff, nil
	}
	invalid := func(why string) (string, string, error) {
		return remediationv1alpha1.ReasonChainLinkInvalid, why, nil
	}
	pred := &remediationv1alpha1.RemediationRequest{}
	switch err := r.getLive(ctx, types.NamespacedName{Namespace: rr.Namespace, Name: string(rep.Name)}, pred); {
	case apierrors.IsNotFound(err):
		return invalid("The request it replaces does not exist")
	case err != nil:
		return "", "", fmt.Errorf("get the RemediationRequest %s replaces: %w", rr.Name, err)
	}
	pr := pred.Status.PullRequest
	opened := meta.FindStatusCondition(pred.Status.Conditions, remediationv1alpha1.ConditionPROpened)
	switch {
	case pred.UID != types.UID(rep.UID):
		return invalid("The request it replaces has another UID")
	case !pred.DeletionTimestamp.IsZero():
		return invalid("The request it replaces is being deleted")
	case pred.Spec.IncidentRef != rr.Spec.IncidentRef:
		return invalid("The request it replaces names another Incident")
	case pred.Spec.TargetRef != rr.Spec.TargetRef:
		return invalid("The request it replaces names another target")
	case chainRootOf(pred) != rr.Spec.ChainRoot:
		return invalid("The request it replaces belongs to another chain")
	case pred.Status.Phase != remediationv1alpha1.PhaseAwaitingApproval || pr == nil || opened == nil ||
		opened.Reason != ReasonPullRequestOpened:
		return invalid("The request it replaces does not wait for review with an open pull request")
	case pred.Status.Handoff != nil && pred.Status.Handoff.To != nil &&
		pred.Status.Handoff.To.Phase != remediationv1alpha1.HandoffAborted:
		// A handoff that aborted left the request as it was: a later hint of it may still be answered.
		return invalid("The request it replaces is already handed over")
	}
	if h := hintByRef(pred, rep.Hint); h == nil || h.State != remediationv1alpha1.HintPending {
		return invalid("The request it replaces does not hold the named hint Pending")
	}
	return "", "", nil
}

// handoffStep does the next step of a replacement admitted with status.handoff.from Reserved, one write per
// call: the predecessor's intent (step 2), or the branch update and the record of the pull request (steps 3
// and 4). recheck has checked the plan's freshness; fresh is what that check read.
func (r *RemediationRequestReconciler) handoffStep(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	fresh freshness.Result) (reconcile.Result, error) {
	from := rr.Status.Handoff.From
	if !r.ReviewerHints {
		// Admitted while hints were on, and the executor now runs with them off: nothing is taken over, and the
		// predecessor declines the hint and aborts its intent on its next look (declineHintsOff).
		return reconcile.Result{}, r.endHandoff(ctx, rr, remediationv1alpha1.ReasonHintsTurnedOff, msgHintsTurnedOff+
			"; nothing was taken over")
	}
	if fresh.Head == "" || fresh.Git == nil {
		return reconcile.Result{}, errNoHead
	}
	pred := &remediationv1alpha1.RemediationRequest{}
	switch err := r.getLive(ctx, types.NamespacedName{Namespace: rr.Namespace, Name: string(from.Name)}, pred); {
	case apierrors.IsNotFound(err):
		return reconcile.Result{}, r.endHandoff(ctx, rr, remediationv1alpha1.ReasonHandoffRefused,
			"The request it replaces is gone; nothing was taken over")
	case err != nil:
		return reconcile.Result{}, fmt.Errorf("get the RemediationRequest %s replaces: %w", rr.Name, err)
	}
	if pred.UID != types.UID(from.UID) {
		return reconcile.Result{}, r.endHandoff(ctx, rr, remediationv1alpha1.ReasonHandoffRefused,
			"The request it replaces was made again under the same name; nothing was taken over")
	}
	var to *remediationv1alpha1.OutgoingHandoff
	if pred.Status.Handoff != nil {
		to = pred.Status.Handoff.To
	}
	switch {
	case to == nil, to.Phase == remediationv1alpha1.HandoffAborted && to.UID != remediationv1alpha1.UID(rr.UID):
		// No intent yet, or only the aborted one of an earlier replacement, which this one's intent replaces.
		return r.reserve(ctx, rr, pred)
	case to.UID != remediationv1alpha1.UID(rr.UID) || to.Hint != from.Hint:
		return reconcile.Result{}, r.endHandoff(ctx, rr, remediationv1alpha1.ReasonHandoffRefused,
			"The request it replaces is handed over to another request; nothing was taken over")
	case to.Phase == remediationv1alpha1.HandoffAborted:
		return reconcile.Result{}, r.endHandoff(ctx, rr, remediationv1alpha1.ReasonHandoffAborted,
			"The request it replaces ended before the handoff; nothing was taken over")
	case to.Phase != remediationv1alpha1.HandoffIntent:
		// Done is written only after this request recorded the pull request (step 4), which moved it out of
		// PolicyChecked: this is an old read, retried.
		return reconcile.Result{}, errors.New("the predecessor is handed over, but this request has not recorded it")
	case !pred.DeletionTimestamp.IsZero():
		// Its finalizer waits for this request to take over or end (handoffHolds); before the update, it ends.
		// After an update the restart finds the commit first (recoverHandoff) and records it.
		return reconcile.Result{}, r.endHandoff(ctx, rr, remediationv1alpha1.ReasonHandoffAborted,
			"The request it replaces is being deleted; nothing was taken over")
	}
	return r.takeOver(ctx, rr, pred, to, fresh)
}

// reserve is step 2: the predecessor's intent and its hint Reserved, in one status write under optimistic
// concurrency.
func (r *RemediationRequestReconciler) reserve(ctx context.Context, rr, pred *remediationv1alpha1.RemediationRequest) (
	reconcile.Result, error) {
	from := rr.Status.Handoff.From
	h := hintByRef(pred, from.Hint)
	// A predecessor whose plan is stale (its cleanup started, or its close is refused, #199) gets nothing but the
	// close written to its pull request: no handoff pushes to it.
	if pred.Status.Phase != remediationv1alpha1.PhaseAwaitingApproval || pred.Status.PullRequest == nil ||
		!pred.DeletionTimestamp.IsZero() || h == nil || h.State != remediationv1alpha1.HintPending ||
		staleMark(pred) != nil {
		return reconcile.Result{}, r.endHandoff(ctx, rr, remediationv1alpha1.ReasonHandoffRefused,
			"The request it replaces no longer waits for review, its plan is stale, or its hint is no longer "+
				"Pending; nothing was taken over")
	}
	if err := r.writeAllowed(ctx); err != nil {
		return reconcile.Result{}, err
	}
	upd := pred.DeepCopy()
	if upd.Status.Handoff == nil {
		upd.Status.Handoff = &remediationv1alpha1.Handoff{}
	}
	upd.Status.Handoff.To = &remediationv1alpha1.OutgoingHandoff{
		Name: remediationv1alpha1.Name(rr.Name), UID: remediationv1alpha1.UID(rr.UID), Hint: from.Hint,
		ExpectedHead: pred.Status.PullRequest.HeadSHA, Phase: remediationv1alpha1.HandoffIntent,
	}
	hintByRef(upd, from.Hint).State = remediationv1alpha1.HintReserved
	if err := r.Status().Update(ctx, upd); err != nil {
		if apierrors.IsConflict(err) {
			// The predecessor was written meanwhile: read again. Only a successor that reads the hint Pending
			// reserves it, so two never both do.
			return reconcile.Result{RequeueAfter: stepDelay}, nil
		}
		return reconcile.Result{}, fmt.Errorf("record the handoff intent on RemediationRequest %s/%s: %w",
			pred.Namespace, pred.Name, err)
	}
	logf.FromContext(ctx).Info("Recorded the handoff intent of RemediationRequest", "remediationRequest",
		client.ObjectKeyFromObject(pred), "replacement", rr.Name)
	return reconcile.Result{RequeueAfter: stepDelay}, nil
}

// takeOver is step 3, and step 4 once the branch holds the replacement's commit: see the top of this file.
func (r *RemediationRequestReconciler) takeOver(ctx context.Context, rr, pred *remediationv1alpha1.RemediationRequest,
	to *remediationv1alpha1.OutgoingHandoff, fresh freshness.Result) (reconcile.Result, error) {
	git, path := fresh.Git, string(rr.Status.Source.Path)
	predPR := pred.Status.PullRequest
	switch {
	case predPR == nil || predPR.HeadSHA != to.ExpectedHead:
		return reconcile.Result{}, r.endHandoff(ctx, rr, remediationv1alpha1.ReasonHandoffAborted,
			"The request it replaces no longer records the head its intent expects; nothing was taken over")
	case pred.Status.Source == nil || string(pred.Status.Source.Path) != path:
		// ADR-0028 section 4 item 7 lets the commit restore the old plan's file; this executor changes one file
		// per commit, so a plan for another file fails closed.
		return reconcile.Result{}, r.endHandoff(ctx, rr, remediationv1alpha1.ReasonHandoffAborted,
			"The new plan changes another file than the pull request; the pull request was left as it is")
	case freshness.BranchName(rr) != freshness.BranchName(pred):
		return reconcile.Result{}, r.endHandoff(ctx, rr, remediationv1alpha1.ReasonHandoffAborted,
			"The request it replaces uses another branch; nothing was taken over")
	}
	changed, state, err := planned(fresh.File, rr)
	switch {
	case err != nil:
		return reconcile.Result{}, r.endHandoff(ctx, rr, remediationv1alpha1.ReasonHandoffAborted,
			"The typed patcher refused the target file on the target branch ("+patchReason(err)+"); the pull "+
				"request was left as it is")
	case state != fileHoldsPrevious:
		return reconcile.Result{}, r.endHandoff(ctx, rr, remediationv1alpha1.ReasonHandoffAborted,
			"The target file on the target branch no longer holds the value the change was made from; the pull "+
				"request was left as it is")
	}
	branch := freshness.BranchName(rr)
	// The predecessor's pull request is read by its recorded number (ADR-0030 section 1): a newer pull request on
	// the shared branch never stands in for it.
	cr, gone, err := r.findHanded(ctx, git, pred)
	switch {
	case err != nil:
		return reconcile.Result{}, err
	case gone != "":
		return reconcile.Result{}, r.endHandoff(ctx, rr, remediationv1alpha1.ReasonHandoffAborted,
			"The pull request of the request it replaces is not its own any more ("+gone+"); nothing was taken over")
	}
	head, err := git.BranchHead(ctx, branch)
	if err != nil {
		return reconcile.Result{}, freshness.WrapProvider("read the head of the branch", err)
	}
	if head != to.ExpectedHead {
		// The update may have happened before a restart, a lost answer or a lost status write: only the
		// replacement's own commit goes on to the read after the update.
		ours, err := r.isHandoffCommit(ctx, git, rr, pred, to, head)
		switch {
		case err != nil:
			return reconcile.Result{}, err
		case !ours:
			return reconcile.Result{}, r.endHandoff(ctx, rr, remediationv1alpha1.ReasonHandoffAborted,
				"Someone else pushed to the branch during the handoff; the pull request was left as it is")
		}
		return r.afterUpdate(ctx, git, rr, pred, head)
	}
	if cr.State != providers.ChangeRequestOpen {
		return reconcile.Result{}, r.endHandoff(ctx, rr, remediationv1alpha1.ReasonHandoffAborted,
			"The pull request was "+closedOrMerged(cr)+" before the handoff; the branch was not updated")
	}
	if ref, err := r.checkHandoff(ctx, git, rr, pred, fresh, head, changed); err != nil || ref != nil {
		if err != nil {
			return reconcile.Result{}, err
		}
		return reconcile.Result{}, r.endHandoff(ctx, rr, remediationv1alpha1.ReasonHandoffAborted,
			ref.message+" ("+ref.reason+"); the pull request was left as it is")
	}
	// The commit must be what a restart recognises as this handoff's (isHandoffCommit): the predecessor's value
	// in the pull request's file changed to the new plan's, and nothing else.
	switch want, ok, err := handoffContent(ctx, git, rr, pred, head); {
	case err != nil:
		return reconcile.Result{}, err
	case !ok || !bytes.Equal(want, changed):
		return reconcile.Result{}, r.endHandoff(ctx, rr, remediationv1alpha1.ReasonHandoffAborted,
			"The new plan cannot be written as one change of the value the pull request's file holds; the pull "+
				"request was left as it is")
	}
	newHead, err := git.AppendCommit(ctx, branch, head, providers.FileChange{
		Path: path, Content: changed, Message: commitTitle(rr),
	})
	switch {
	case providers.IsConflict(err):
		// The branch moved since it was read: the next step reads it again and decides.
		return reconcile.Result{RequeueAfter: stepDelay}, nil
	case err != nil:
		return reconcile.Result{}, freshness.WrapProvider("add the commit of the handoff", err)
	}
	logf.FromContext(ctx).Info("Added the commit of the handoff to the branch of RemediationRequest",
		"remediationRequest", client.ObjectKeyFromObject(pred), "replacement", rr.Name)
	return r.afterUpdate(ctx, git, rr, pred, newHead)
}

// checkHandoff is the render check and the branch rule before the update (ADR-0028 section 4 item 6): head,
// the expected head, carries only the predecessor's chain of changes to the target file, and that file holds
// exactly the predecessor's plan applied to the target file at the head of the target branch, so the branch
// merges as the target branch with only that file replaced; the new plan, changed, renders there as planned.
func (r *RemediationRequestReconciler) checkHandoff(ctx context.Context, git providers.GitProvider,
	rr, pred *remediationv1alpha1.RemediationRequest, fresh freshness.Result, head string, changed []byte) (
	*renderRefusal, error) {
	path := string(rr.Status.Source.Path)
	switch bs, err := branchStateOf(ctx, git, path, head, chainDepth(pred)); {
	case err != nil:
		return nil, err
	case bs != providers.BranchOneChange:
		return &renderRefusal{reason: ReasonBranchDiverged,
			message: "The branch holds more than the planned change on top of the target branch"}, nil
	}
	file, err := git.ReadFile(ctx, head, path, providers.MaxFileBytes)
	if err != nil {
		return nil, freshness.WrapProvider("read the file of the pull request", err)
	}
	if want, state, err := planned(fresh.File, pred); err != nil || state != fileHoldsPrevious ||
		!bytes.Equal(want, file.Data) {
		return &renderRefusal{reason: ReasonTargetFileDiverged, message: "The target file changed on the target " +
			"branch while the pull request waited, so the new plan was not rendered as it would merge"}, nil
	}
	if err := r.checkRender(ctx, git, fresh.Head, changed, rr, nil); err != nil {
		if ref, ok := errors.AsType[*renderRefusal](err); ok {
			return ref, nil
		}
		return nil, err
	}
	return nil, nil
}

// rebasedAction is rr's action as a change from the value pred's plan sets: the commit of a handoff changes the
// pull request's file, which holds pred's desired value, to rr's. ok is false when the two plans are not the
// same kind of change, which no single commit of a handoff can make.
func rebasedAction(rr, pred *remediationv1alpha1.RemediationRequest) (remediationv1alpha1.Action, bool) {
	a := *rr.Spec.Action.DeepCopy()
	p := pred.Spec.Action
	switch {
	case a.Type != p.Type || rr.Spec.TargetRef != pred.Spec.TargetRef:
		return a, false
	case a.SetContainerMemoryLimit != nil && p.SetContainerMemoryLimit != nil &&
		a.SetContainerMemoryLimit.Container == p.SetContainerMemoryLimit.Container:
		a.SetContainerMemoryLimit.Previous = p.SetContainerMemoryLimit.Desired
	case a.AdjustKedaMaxReplicas != nil && p.AdjustKedaMaxReplicas != nil:
		a.AdjustKedaMaxReplicas.PreviousValue = p.AdjustKedaMaxReplicas.DesiredValue
	default:
		return a, false
	}
	return a, true
}

// handoffContent is the content the commit of rr's handoff gives the target file: the file at base, the
// predecessor's head, with pred's value changed to rr's by the typed patcher. It depends only on the two
// specs and base, never on the target branch, so a restart computes the same whatever the target branch did
// meanwhile. ok is false when the patcher refuses (the file at base does not hold pred's value); an error is
// a read worth retrying.
func handoffContent(ctx context.Context, git providers.GitProvider, rr, pred *remediationv1alpha1.RemediationRequest,
	base string) ([]byte, bool, error) {
	a, ok := rebasedAction(rr, pred)
	if !ok || pred.Status.Source == nil {
		return nil, false, nil
	}
	file, err := git.ReadFile(ctx, base, string(pred.Status.Source.Path), providers.MaxFileBytes)
	if err != nil {
		return nil, false, freshness.WrapProvider("read the file of the pull request", err)
	}
	out, err := applyAction(file.Data, rr.Spec.TargetRef, a, false)
	if err != nil {
		return nil, false, nil
	}
	return out, true, nil
}

// isHandoffCommit reports whether head, a branch head that is not the expected one, is the commit of rr's
// handoff of pred: its one parent is the expected head, the chain below it changes only the target file, and
// the file is handoffContent of the expected head.
func (r *RemediationRequestReconciler) isHandoffCommit(ctx context.Context, git providers.GitProvider,
	rr, pred *remediationv1alpha1.RemediationRequest, to *remediationv1alpha1.OutgoingHandoff, head string) (bool, error) {
	if pred.Status.Source == nil {
		return false, nil
	}
	parents, err := git.Parents(ctx, head)
	if err != nil {
		return false, freshness.WrapProvider("read the parents of the branch head", err)
	}
	if len(parents) != 1 || parents[0] != to.ExpectedHead {
		return false, nil
	}
	path := string(pred.Status.Source.Path)
	switch bs, err := branchStateOf(ctx, git, path, head, min(chainDepth(pred)+1, providers.MaxChainCommits)); {
	case err != nil:
		return false, err
	case bs == providers.BranchAtTarget:
		// A merge took head into the target branch (a merge commit or a fast-forward, #591), so the chain can no
		// longer be told apart from it. head is judged against its own parent instead, the head the predecessor
		// recorded (checked above), whose chain was checked when it was recorded.
		switch state, parent, err := git.CheckCommit(ctx, head, path); {
		case err != nil:
			return false, freshness.WrapProvider("check what the merged branch head holds", err)
		case state != providers.BranchOneChange || parent != to.ExpectedHead:
			return false, nil
		}
	case bs != providers.BranchOneChange:
		return false, nil
	}
	want, ok, err := handoffContent(ctx, git, rr, pred, to.ExpectedHead)
	if err != nil || !ok {
		return false, err
	}
	file, err := git.ReadFile(ctx, head, path, providers.MaxFileBytes)
	if err != nil {
		return false, freshness.WrapProvider("read the file of the pull request", err)
	}
	return bytes.Equal(file.Data, want), nil
}

// expectedHandoffHead reports whether head, the moved head of the recorded pull request of a predecessor whose
// intent stands, is the replacement's commit (ADR-0028 section 4 item 8, design 6.5.4): one commit on top of the
// head it recorded that changes only the target file, to exactly the replacement's plan (isHandoffCommit, with
// the replacement read as it is now). Any other head, a person's push of one commit to the target file
// included, is HeadMoved as before.
func (r *RemediationRequestReconciler) expectedHandoffHead(ctx context.Context, git providers.GitProvider,
	rr *remediationv1alpha1.RemediationRequest, head string) (bool, error) {
	to := intent(rr)
	if to == nil || to.ExpectedHead != rr.Status.PullRequest.HeadSHA || rr.Status.Source == nil {
		return false, nil
	}
	repl := &remediationv1alpha1.RemediationRequest{}
	// Read from the API server: what the head is decides the predecessor's next write (decision of 2026-11-19 on #786).
	switch err := r.getLive(ctx, types.NamespacedName{Namespace: rr.Namespace, Name: string(to.Name)}, repl); {
	case apierrors.IsNotFound(err):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("get the replacement of RemediationRequest %s: %w", rr.Name, err)
	case repl.UID != types.UID(to.UID):
		return false, nil
	}
	return r.isHandoffCommit(ctx, git, repl, rr, to, head)
}

// mergeOwner is the request that records a merge of a pull request a handoff names (mergedHeadOwner).
type mergeOwner int

const (
	// ownerPredecessor: the merged head is the head the predecessor recorded, its own commit.
	ownerPredecessor mergeOwner = iota
	// ownerReplacement: the merged head is the handoff commit of a replacement that exists and is not being deleted,
	// or any head but the predecessor's own while that replacement holds the target lock before its takeover
	// (recordsEveryMerge, #870); the replacement records the merge, and the predecessor leaves it alone.
	ownerReplacement
	// ownerForeign: any other head, a deleted replacement's commit included: the predecessor records it as a merge
	// with a foreign change (decision 2 of 2026-11-18 on #786), and no replacement records it.
	ownerForeign
	// ownerReplacementPending: ownerReplacement as the predecessor sees it before the replacement holds its record of
	// that merge (predecessorMergeOwner), a replacement holding only a claim on it included (holdsMergeClaim): the
	// predecessor waits and writes nothing, so that a replacement deleted before it records the merge leaves the
	// predecessor to record it as a foreign change (second round of #786; second decision of 2026-11-19 on #880).
	ownerReplacementPending
	// ownerPredecessorRecords: seen from a replacement that took the pull request over, the merged head is the head its
	// predecessor recorded (a person put it back), and the predecessor, alive and not handed off, holds its record of
	// that merge: the replacement leaves it (replacementMergeOwner).
	ownerPredecessorRecords
	// ownerPredecessorPending: ownerPredecessorRecords before the predecessor holds that record (it may hold only a
	// claim on the merge, holdsMergeClaim, also after it handed the pull request over): the replacement waits and
	// writes nothing, so that a predecessor deleted in between leaves the merge to the replacement, which
	// records it as a foreign change.
	ownerPredecessorPending
)

// mergedHeadOwner is the one rule for who records a merge, with head as its merged head, of pred's recorded pull
// request when pred handed it, or meant to hand it, to repl (pred's outgoing handoff to, in any phase; repl nil when it
// is gone). Both requests decide by it, so they never both record the merge and never both leave it: the predecessor
// before it records a merge of a head that is not its own (watchStep, settleStopException), the replacement before it
// records the merge of its commit (recordWatchedMerge). A merge the replacement recorded (replacementHoldsRecord: with
// its receipt, or without one) is its own. Short
// of that, a replacement that is being deleted records nothing, nor does one whose handoff a verdict settled
// (handoffSettled), so its commit counts as foreign. Short of those, a replacement that holds the target lock before
// its takeover (recordsEveryMerge) records a merge of any other head too, as a foreign change (decision of 2026-11-20 on
// #870), so the predecessor records none. The predecessor's own head is the predecessor's, except once it is being
// deleted without a record of that merge while such a replacement holds the lock (predecessorLeftItsMerge, third case
// of that decision). A failed read is an error.
func (r *RemediationRequestReconciler) mergedHeadOwner(ctx context.Context, git providers.GitProvider,
	pred, repl *remediationv1alpha1.RemediationRequest, to *remediationv1alpha1.OutgoingHandoff, head string) (
	mergeOwner, error) {
	owner, _, err := r.mergedHeadOwnerOf(ctx, git, pred, repl, to, head, false)
	return owner, err
}

// mergedHeadOwnerOf is mergedHeadOwner, and with judge set it also reports whether head is repl's handoff commit
// (ours) when the replacement owns the merge, for the replacement's choice of record: the takeover with its receipt
// for its commit, a foreign change for any other head (recordOtherHeadMerge). Without judge, ours is false.
func (r *RemediationRequestReconciler) mergedHeadOwnerOf(ctx context.Context, git providers.GitProvider,
	pred, repl *remediationv1alpha1.RemediationRequest, to *remediationv1alpha1.OutgoingHandoff, head string,
	judge bool) (mergeOwner, bool, error) {
	if pred.Status.PullRequest != nil && head == pred.Status.PullRequest.HeadSHA {
		if predecessorLeftItsMerge(pred, repl, to, head) {
			return ownerReplacement, false, nil
		}
		return ownerPredecessor, false, nil
	}
	if to == nil || repl == nil || repl.UID != types.UID(to.UID) {
		return ownerForeign, false, nil
	}
	held := replacementHoldsRecord(pred, repl, head) || replacementHoldsClaim(pred, repl, head)
	// A merge the replacement recorded is its own, also while it is being deleted: its finalizer ends the predecessor
	// on it (supersedePredecessor) before it goes. A claim on it stands for it too, also after a verdict settled its
	// handoff: the merge stays its own while the claim stands (predecessorMergeOwner: ownerReplacementPending), and a
	// replacement being deleted holds none (second decision of 2026-11-19 on #880).
	if held && !judge {
		return ownerReplacement, false, nil
	}
	if !held && (!repl.DeletionTimestamp.IsZero() || head == to.ExpectedHead || handoffSettled(repl)) {
		return ownerForeign, false, nil
	}
	switch ours, err := r.isHandoffCommit(ctx, git, repl, pred, to, head); {
	case err != nil:
		return ownerForeign, false, err
	case ours:
		return ownerReplacement, true, nil
	case held, recordsEveryMerge(repl):
		return ownerReplacement, false, nil
	}
	return ownerForeign, false, nil
}

// predecessorLeftItsMerge reports whether a merge of pred's own recorded head is left to repl, its replacement named by
// its outgoing handoff to (third case of the decision of 2026-11-20 on #870): pred holds no record of that merge
// (holdsMergeRecord) and no claim on it (holdsMergeClaim), and it can no longer record it: it is being deleted, so its
// finalizer records none, or it ended and released its lock on a verdict and holds no intent (mayStillRecord false: a
// stopped request that released its lock is not looked at again and records nothing of that pull request any more,
// also when a person reopens and merges it, design 6.5). A predecessor that still runs records its own merge. repl holds the target lock before its takeover (recordsEveryMerge), so it records the merge as a
// foreign change with the same reasons as any other head; it never lets go of the lock without a record
// (mayReleaseOnMerge). A predecessor that may still record keeps its own head, and repl waits for its record. Only the
// replacement's side can find it true: the predecessor's own reading (predecessorMergeOwner) passes no replacement for
// its own head, so it is false there, and the two sides agree because such a predecessor records nothing. It reads the
// status only.
func predecessorLeftItsMerge(pred, repl *remediationv1alpha1.RemediationRequest,
	to *remediationv1alpha1.OutgoingHandoff, head string) bool {
	pr := pred.Status.PullRequest
	return pr != nil && (!pred.DeletionTimestamp.IsZero() || (ended(pred.Status.Phase) && !mayStillRecord(pred))) &&
		!holdsMergeRecord(pred, pr.Number, head) && !holdsMergeClaim(pred, pr.Number, head) &&
		to != nil && repl != nil && repl.UID == types.UID(to.UID) && recordsEveryMerge(repl)
}

// recordsEveryMerge reports whether repl, a replacement that exists and is not being deleted, records every merge of
// the pull request it was to take over whatever its head, short of the predecessor's own recorded head (decision of
// 2026-11-20 on #870): it holds the target lock before its takeover (its handoff Reserved, its commit possibly pushed,
// mayCarryHandoffCommit). One writer of the merge record per pull request, the one that holds the lock: a head
// with further commits on top of its commit, or one without it, is recorded on it as a foreign change
// (recordOtherHeadMerge), and the predecessor records none while it may (predecessorMergeOwner waits for that record).
// It reads the status only.
func recordsEveryMerge(repl *remediationv1alpha1.RemediationRequest) bool {
	return repl.DeletionTimestamp.IsZero() && mayCarryHandoffCommit(repl) &&
		repl.Status.Handoff.From.Phase == remediationv1alpha1.HandoffReserved &&
		ptr.Deref(repl.Status.HoldsTarget, false)
}

// handoffSettled reports whether repl's handoff from its predecessor is Aborted: a verdict settled it (the pull request
// closed, merged with another head, open without its commit, or its repository gone) and released its lock, and it
// records nothing of that pull request any more, also when a person reopens it (design 6.5). A merge of its commit is
// then the predecessor's to record as a foreign change, as for a deleted replacement.
func handoffSettled(repl *remediationv1alpha1.RemediationRequest) bool {
	ho := repl.Status.Handoff
	return ho != nil && ho.From != nil && ho.From.Phase == remediationv1alpha1.HandoffAborted
}

// predecessorMergeOwner is mergedHeadOwner seen from pred, whose recorded pull request was merged with head: its
// replacement, named by its outgoing handoff, is read from the API server (gone counts as nil). ownerReplacement is
// returned only once the replacement holds its record of that merge (replacementHoldsRecord, a record without a
// receipt included), as supersedeAfterAbort asks; before that it is ownerReplacementPending, and the predecessor ends
// nothing on it, also while the replacement holds only a claim on the merge (holdsMergeClaim), whether or not it holds
// the lock. A failed read of the replacement is the Kubernetes API's (a *clusterError); a failed read of the Git
// host is returned as it is.
func (r *RemediationRequestReconciler) predecessorMergeOwner(ctx context.Context, git providers.GitProvider,
	pred *remediationv1alpha1.RemediationRequest, head string) (mergeOwner, error) {
	var to *remediationv1alpha1.OutgoingHandoff
	if pred.Status.Handoff != nil {
		to = pred.Status.Handoff.To
	}
	if (pred.Status.PullRequest != nil && head == pred.Status.PullRequest.HeadSHA) || to == nil {
		return r.mergedHeadOwner(ctx, git, pred, nil, to, head)
	}
	repl := &remediationv1alpha1.RemediationRequest{}
	switch err := r.getLive(ctx, types.NamespacedName{Namespace: pred.Namespace, Name: string(to.Name)}, repl); {
	case apierrors.IsNotFound(err):
		repl = nil
	case err != nil:
		return ownerForeign, &clusterError{err: fmt.Errorf("get the replacement of RemediationRequest %s: %w",
			pred.Name, err)}
	}
	owner, err := r.mergedHeadOwner(ctx, git, pred, repl, to, head)
	if err == nil && owner == ownerReplacement && !replacementHoldsRecord(pred, repl, head) {
		if !replacementHoldsClaim(pred, repl, head) && !mayStillRecord(repl) {
			// The replacement released its lock without recording the merge: it records nothing of this pull request
			// any more, so the merge is a foreign change for the predecessor (fourth round of #786).
			return ownerForeign, nil
		}
		return ownerReplacementPending, nil
	}
	return owner, err
}

// mayStillRecord reports whether other, the other request of a handoff, read from the API server, can still record a
// merge of the pull request the handoff names: it is not being deleted, and it holds the target lock or a standing
// intent. A request that released its lock on a verdict records nothing of that pull request any more (design 6.5),
// also when a person reopens and merges it, so the request that still watches does not wait for it: the merge is a
// foreign change for the watcher (fourth round of #786, the mirror of handoffSettled). The one rule for both sides
// of a handoff (predecessorMergeOwner, replacementMergeOwner).
func mayStillRecord(other *remediationv1alpha1.RemediationRequest) bool {
	return other != nil && other.DeletionTimestamp.IsZero() &&
		(ptr.Deref(other.Status.HoldsTarget, false) || intent(other) != nil)
}

// replacementMergeOwner is the rule for who records a merge with head of the pull request rr, a replacement, took over
// (tookOver), seen from rr: a head that is the one its predecessor recorded is the predecessor's once it holds its
// record of that merge (holdsMergeRecord, ownerPredecessorRecords), also while it is being deleted, and before that
// while it is alive (its UID, not being deleted), not handed off and still able to record it (mayStillRecord: it holds
// the lock or its intent; ownerPredecessorPending: it records its own merge, mergedHeadOwner), or while it holds a
// claim on that merge (holdsMergeClaim), handed off or not (ownerPredecessorPending); anything else, a
// predecessor gone, being deleted, handed off or released by a verdict without that record, is ownerForeign, rr's to
// record as before. A failed read of the predecessor is the Kubernetes API's (a *clusterError).
func (r *RemediationRequestReconciler) replacementMergeOwner(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	head string) (mergeOwner, error) {
	if !tookOver(rr) {
		return ownerForeign, nil
	}
	from := rr.Status.Handoff.From
	pred := &remediationv1alpha1.RemediationRequest{}
	switch err := r.getLive(ctx, types.NamespacedName{Namespace: rr.Namespace, Name: string(from.Name)}, pred); {
	case apierrors.IsNotFound(err):
		return ownerForeign, nil
	case err != nil:
		return ownerForeign, &clusterError{err: fmt.Errorf("get the RemediationRequest %s replaces: %w", rr.Name, err)}
	}
	if pred.UID != types.UID(from.UID) || pred.Status.PullRequest == nil || pred.Status.PullRequest.HeadSHA != head {
		return ownerForeign, nil
	}
	if mayReleaseOnMerge(pred, pred.Status.PullRequest.Number, head) {
		// A merge the predecessor recorded is its own, with its receipt or without one, also while it is being
		// deleted: its finalizer ends this request on it (supersedeReplacementAfterMerge) before it goes (third and
		// sixth rounds of #786).
		return ownerPredecessorRecords, nil
	}
	if holdsMergeClaim(pred, pred.Status.PullRequest.Number, head) {
		// The predecessor read the merge first and holds only its claim on it, also after it handed the pull request
		// over: rr waits, neither recording the merge nor ending on it, until that claim becomes a record or goes (a
		// predecessor being deleted holds none; second decision of 2026-11-19 on #880).
		return ownerPredecessorPending, nil
	}
	if !pred.DeletionTimestamp.IsZero() || handedOff(pred) || !mayStillRecord(pred) {
		return ownerForeign, nil
	}
	return ownerPredecessorPending, nil
}

// ReasonMergedForPredecessor is the reason a replacement that took its predecessor's pull request over ends
// Superseded when that pull request was merged with the head the predecessor recorded (a person put it back): the
// predecessor records the merge with its receipt, and this request's plan was not merged (second round of #786).
const ReasonMergedForPredecessor = "MergedForPredecessor"

// leavePredecessorMerge ends rr, a replacement whose taken-over pull request v found merged with its predecessor's
// recorded head, which the predecessor records (ownerPredecessorRecords), Superseded with ReasonMergedForPredecessor:
// rr records none and is no failure for the circuit breaker, and it releases its lock in the same write, as nothing
// can be merged through that pull request any more. A stopped rr keeps its phase and only releases (settleStopException).
func (r *RemediationRequestReconciler) leavePredecessorMerge(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	v changeVerdict) error {
	if s, ok := settlePredecessorMerge(v, ownerPredecessorRecords); ok && ptr.Deref(rr.Status.HoldsTarget, false) {
		if err := lockverdict.Release(&rr.Status, s); err != nil {
			return err
		}
	}
	return r.stop(ctx, rr, remediationv1alpha1.PhaseSuperseded, ReasonMergedForPredecessor,
		fmt.Sprintf("Pull request #%d was merged with the commit of RemediationRequest %s, which records the merge; "+
			"this request's plan was not merged", v.pr.Number, rr.Status.Handoff.From.Name))
}

// supersedeReplacementAfterMerge is the step a predecessor, rr, takes once it holds the record of the merge of its own
// recorded head (holdsMergeRecord: with its receipt, or without one), so that its replacement never records the same
// merge as a foreign change once rr is gone. It runs in the reconcile that stored that record and every one after it
// (endReplacementAfterRecord), and in rr's finalizer before it lets go. Two cases end the replacement:
//   - it took the pull request over (a person put rr's head back): it ends Superseded with ReasonMergedForPredecessor
//     (leavePredecessorMerge), unless it ended or recorded a merge already (second round of #786);
//   - it holds the target lock before its takeover (recordsEveryMerge), so it would record any merge of the pull
//     request once rr is gone (heldRecoveryAlone): it ends on rr's record as it does when it reads the merge itself
//     with rr present (endOnPredecessorRecord; decision of 2026-11-20 on #870 replacing the third row).
//
// A replacement gone, made again, being deleted or not on rr's pull request needs nothing. A failed read or write is
// returned for a retry; a pause is freshness.ErrPaused.
func (r *RemediationRequestReconciler) supersedeReplacementAfterMerge(ctx context.Context,
	rr *remediationv1alpha1.RemediationRequest) error {
	ho, pr := rr.Status.Handoff, rr.Status.PullRequest
	if ho == nil || ho.To == nil || pr == nil || !holdsMergeRecord(rr, pr.Number, pr.HeadSHA) {
		return nil
	}
	repl := &remediationv1alpha1.RemediationRequest{}
	switch err := r.getLive(ctx, types.NamespacedName{Namespace: rr.Namespace, Name: string(ho.To.Name)}, repl); {
	case apierrors.IsNotFound(err):
		return nil
	case err != nil:
		return &clusterError{err: fmt.Errorf("get the replacement of RemediationRequest %s: %w", rr.Name, err)}
	}
	if repl.UID != types.UID(ho.To.UID) || !repl.DeletionTimestamp.IsZero() {
		return nil
	}
	v := changeVerdict{state: changeMerged, pr: providers.ChangeRequest{Number: pr.Number,
		HeadSHA: pr.HeadSHA, State: providers.ChangeRequestMerged}}
	if recordsEveryMerge(repl) {
		if err := r.writeAllowed(ctx); err != nil {
			return err
		}
		return r.endOnPredecessorRecord(ctx, repl, rr, v)
	}
	if !tookOver(repl) || repl.Status.PullRequest == nil || repl.Status.PullRequest.Number != pr.Number ||
		mergeRecorded(repl) ||
		(ended(repl.Status.Phase) && repl.Status.Phase != remediationv1alpha1.PhaseNeedsIntervention) {
		return nil
	}
	if err := r.writeAllowed(ctx); err != nil {
		return err
	}
	return r.ignoreStopped(r.leavePredecessorMerge(ctx, repl, v))
}

// endOnPredecessorRecord ends repl, a replacement that holds the target lock before its takeover (recordsEveryMerge),
// once its predecessor, pred, holds the record of the merge of its own recorded head, v (mayReleaseOnMerge; nothing is
// done otherwise): repl records none, its handoff moves
// to Aborted (nothing was taken over, and nothing can be through a merged pull request) and its lock is released, in
// one status write. One that ended already settles as its own look settles that merge with the predecessor present
// (endOnOtherMerge: its phase kept, an end the breaker counts as a failure becoming Cancelled with
// ReasonHandoffAborted); one still running ends Superseded with ReasonMergedForPredecessor, as a replacement that took
// the pull request over does (leavePredecessorMerge), no failure for the circuit breaker. A failed write is returned
// for a retry.
func (r *RemediationRequestReconciler) endOnPredecessorRecord(ctx context.Context,
	repl, pred *remediationv1alpha1.RemediationRequest, v changeVerdict) error {
	if !mayReleaseOnMerge(pred, v.pr.Number, v.pr.HeadSHA) {
		return nil
	}
	if ended(repl.Status.Phase) {
		return r.endOnOtherMerge(ctx, repl, pred, v.pr)
	}
	repl.Status.Handoff.From.Phase = remediationv1alpha1.HandoffAborted
	return r.ignoreStopped(r.leavePredecessorMerge(ctx, repl, v))
}

// endReplacementAfterRecord runs supersedeReplacementAfterMerge at the end of a reconcile of rr that went through
// (err nil), so the reconcile that stored rr's record of the merge of its own head ends rr's replacement in the same
// pass, and any later one tries again while that replacement still stands. A request being deleted takes the step in
// its finalizer (beginFinalize). A pause of the kill switch leaves it to the next reconcile and keeps the answer of
// the reconcile: it is logged at Info, the replacement keeps its lock meanwhile, and the reconcile is requeued after
// freshRecheckDelay at the latest (a shorter wait of the reconcile, its backoff or poll, is kept, never shortened
// further). A failed read or write of the replacement (a conflict, the API server unavailable) is returned as the
// error of the reconcile, for the controller's error backoff, which logs it, as any failed Kubernetes API call
// (decision of 2026-11-21 on #870); the replacement keeps its lock until a later attempt ends it.
func (r *RemediationRequestReconciler) endReplacementAfterRecord(ctx context.Context,
	rr *remediationv1alpha1.RemediationRequest, res *reconcile.Result, err *error) {
	if *err != nil || ctx.Err() != nil || rr.UID == "" || !rr.DeletionTimestamp.IsZero() {
		return
	}
	serr := r.supersedeReplacementAfterMerge(ctx, rr)
	if serr == nil {
		return
	}
	if !errors.Is(serr, freshness.ErrPaused) {
		*res, *err = reconcile.Result{}, fmt.Errorf("end the replacement %s of RemediationRequest %s/%s, which records "+
			"the merge of its own head: %w", rr.Status.Handoff.To.Name, rr.Namespace, rr.Name, serr)
		return
	}
	logf.FromContext(ctx).Info("Left the end of the replacement of RemediationRequest, which records the merge of its "+
		"own head, to a later reconcile: Git writes are paused", "remediationRequest", client.ObjectKeyFromObject(rr),
		"replacement", rr.Status.Handoff.To.Name)
	if res.RequeueAfter <= 0 || res.RequeueAfter > freshRecheckDelay {
		res.RequeueAfter = freshRecheckDelay
	}
}

// mayReleaseOnMerge is the one rule for whether a replacement that holds the target lock for the pull request of its
// handoff may release it on a merge of that pull request, number with head, that it does not record in the same status
// write: only once pred, its predecessor as read from the API server (or as its own reconcile just stored it), holds
// the record of that merge (holdsMergeRecord). The lock is released in the write that records the merge, or after a
// read shows that the other request holds the record; never while no request holds it, so a predecessor deleted after
// the release cannot leave the merge recorded nowhere (decision of 2026-11-20 on #870, one rule over the state). Every
// release of such a lock on a merge the replacement leaves to its predecessor decides here: the end of a replacement
// before its takeover (endOnOtherMerge, from its own look, heldRecoveryRead, and from the predecessor's record,
// endOnPredecessorRecord) and the end of one that took the pull request over (ownerPredecessorRecords,
// replacementMergeOwner, which leavePredecessorMerge releases on). It reads the status only.
func mayReleaseOnMerge(pred *remediationv1alpha1.RemediationRequest, number int64, head string) bool {
	return pred != nil && holdsMergeRecord(pred, number, head)
}

// replacementHoldsRecord reports whether repl records the merge of pred's recorded pull request with head as its own
// (holdsMergeRecord): the record a predecessor waits for before it lets go of that merge.
func replacementHoldsRecord(pred, repl *remediationv1alpha1.RemediationRequest, head string) bool {
	return repl != nil && pred.Status.PullRequest != nil && holdsMergeRecord(repl, pred.Status.PullRequest.Number, head)
}

// replacementHoldsClaim reports whether repl holds a claim on the merge of pred's recorded pull request with head
// (holdsMergeClaim): the other request then neither records that merge nor ends on it, it waits.
func replacementHoldsClaim(pred, repl *remediationv1alpha1.RemediationRequest, head string) bool {
	return repl != nil && pred.Status.PullRequest != nil && holdsMergeClaim(repl, pred.Status.PullRequest.Number, head)
}

// holdsMergeClaim reports whether x holds a claim on the merge of pull request number with head, and no record of it
// (holdsMergeRecord): a wait of kind MergeCheck or MergeContent that names that pull request and head, the first read
// of a merge reported without a merge commit ID or of a merge into a retargeted branch, stored before anything else x
// did about it (decisions of 2026-11-19 on #880). A claim is not the record: while x holds it the other request of the
// handoff does not record the merge and does not end on it either, it waits (MergeRecordPending, writing nothing to
// Git). A request being deleted holds no claim: its finalizer drops the wait (#878) and records nothing, so once x is
// being deleted, or its wait ended without a record, the other request records the merge by the rule for a deleted
// replacement (mergedHeadOwner) or for a predecessor gone (replacementMergeOwner).
func holdsMergeClaim(x *remediationv1alpha1.RemediationRequest, number int64, head string) bool {
	if !x.DeletionTimestamp.IsZero() || holdsMergeRecord(x, number, head) {
		return false
	}
	for _, k := range readWaits {
		if w := gitWait(x, k); w != nil && w.PullRequest != nil && w.PullRequest.Number == number &&
			w.PullRequest.HeadSHA == head {
			return true
		}
	}
	return false
}

// holdsMergeRecord reports whether x holds the record of the merge of pull request number with head, so the other
// request of the handoff never records the same merge again and ends on it (decisions of 2026-11-19 on #880): the
// merge recorded with its receipt, or without one (status.pullRequest of that number and head with mergedAt, which the
// write that sets Merged stores with the receipt or with the NoMergeReceipt mark, recordMerge); or a merge of that pull
// request recorded as not delivering the plan as shown (undeliveredMerge: MergedWithForeignChange,
// MergeContentUnreadable, MergeNotCheckable), which stores no head: a pull request merges once, so its number names the
// merge. A pending wait that names the merge is a claim, not the record (holdsMergeClaim). Ownership does not depend on
// a merge commit ID, a receipt or the lock alone.
func holdsMergeRecord(x *remediationv1alpha1.RemediationRequest, number int64, head string) bool {
	pr := x.Status.PullRequest
	if pr == nil || pr.Number != number {
		return false
	}
	if mr := x.Status.MergeReceipt; mr != nil && mr.HeadSHA == head {
		return true
	}
	return undeliveredMerge(x) || (pr.HeadSHA == head && pr.MergedAt != nil)
}

// replacementTookOver reports whether the replacement rr's standing intent names, read from the API server, recorded
// the takeover of rr's pull request (its handoff from Done, for rr's UID): an intent whose takeover is recorded is not
// rr's to abort, even when the replacement's write that ends rr (step 5) was lost. A replacement that is gone, or made
// again, did not. A failed read is the Kubernetes API's (a *clusterError), for a retry.
func (r *RemediationRequestReconciler) replacementTookOver(ctx context.Context,
	rr *remediationv1alpha1.RemediationRequest) (bool, error) {
	to := intent(rr)
	if to == nil {
		return false, nil
	}
	repl := &remediationv1alpha1.RemediationRequest{}
	switch err := r.getLive(ctx, types.NamespacedName{Namespace: rr.Namespace, Name: string(to.Name)}, repl); {
	case apierrors.IsNotFound(err):
		return false, nil
	case err != nil:
		return false, &clusterError{err: fmt.Errorf("get the replacement of RemediationRequest %s: %w", rr.Name, err)}
	}
	from := repl.Status.Handoff
	return repl.UID == types.UID(to.UID) && from != nil && from.From != nil && from.From.UID == remediationv1alpha1.UID(rr.UID) &&
		from.From.Phase == remediationv1alpha1.HandoffDone, nil
}

// abortStandingIntent stores the abort of rr's standing intent (abortIntent) in one status write, before rr writes
// anything to its pull request: while the intent stands it writes nothing to Git (#786), and the close checks the
// intent on a fresh read (stillOwns). Callers: a predecessor whose pull request moved to a head that is not its
// replacement's commit, and the finalizer once the replacement is gone or ended without taking over (handoffHolds).
// why says which, for the log line. A failed write is returned for a retry, with nothing written to Git.
func (r *RemediationRequestReconciler) abortStandingIntent(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	why string) error {
	if intent(rr) == nil {
		return nil
	}
	abortIntent(rr, ReasonHintHandoffAborted)
	if err := r.Status().Update(ctx, rr); err != nil {
		return fmt.Errorf("abort the handoff intent of RemediationRequest %s/%s: %w", rr.Namespace, rr.Name, err)
	}
	logf.FromContext(ctx).Info("Aborted the handoff intent of RemediationRequest: "+why,
		"remediationRequest", client.ObjectKeyFromObject(rr))
	return nil
}

// ReasonMergedForReplacement is the reason a predecessor ends Superseded when its pull request was merged with its
// replacement's handoff commit after the handoff was aborted (mergedHeadOwner, #786), or with any head but its own
// while the replacement held the target lock before its takeover (recordsEveryMerge, #870): the replacement records
// the merge (with its receipt for its commit, as a foreign change otherwise), and this request's plan was not merged.
const ReasonMergedForReplacement = "MergedForReplacement"

// leaveReplacementMerge ends rr, a predecessor whose recorded pull request v found merged with its replacement's
// handoff commit (owner, ownerReplacement), Superseded with ReasonMergedForReplacement: the merge is the replacement's
// to record, so rr records none and is no failure for the circuit breaker, and it releases its lock in the same write
// (settleReplacementMerge), as nothing can be merged through that pull request any more.
func (r *RemediationRequestReconciler) leaveReplacementMerge(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	v changeVerdict, owner mergeOwner) error {
	name := ""
	if rr.Status.Handoff != nil && rr.Status.Handoff.To != nil {
		name = string(rr.Status.Handoff.To.Name)
	}
	if s, ok := settleReplacementMerge(v, owner); ok && ptr.Deref(rr.Status.HoldsTarget, false) {
		if err := lockverdict.Release(&rr.Status, s); err != nil {
			return err
		}
	}
	return r.stop(ctx, rr, remediationv1alpha1.PhaseSuperseded, ReasonMergedForReplacement,
		fmt.Sprintf("Pull request #%d was merged while RemediationRequest %s, which replaces this request, held it, "+
			"and that request records the merge; this request's plan was not merged", v.pr.Number, name))
}

// ReasonMergeRecordPending is the reason of condition Approved False on a request of a handoff whose pull request was
// merged with the commit of the other request, which still holds the target lock or a standing intent and records
// that merge (ownerReplacementPending, ownerPredecessorPending): this request waits for that record and writes nothing
// to the merged pull request (fourth round of #786).
const ReasonMergeRecordPending = "MergeRecordPending"

// errMergeRecordPending is lookMovedHead's answer while it waits for the other request of the handoff to record the
// merge of its commit: nothing else is done on the merged pull request (no check run, no close, no reply).
var errMergeRecordPending = errors.New("waiting for the other request of the handoff to record the merge")

// awaitOtherRecord shows that rr waits for other, the request of its handoff that records the merge v found
// (condition Approved False, ReasonMergeRecordPending, written only when it changes), and returns
// errMergeRecordPending. The wait lasts while other may still record it (mayStillRecord); once other records it or
// releases its lock, the next look decides again (predecessorMergeOwner, replacementMergeOwner). A failed status write
// is returned for a retry wrapped in errMergeCheck and marked as the Kubernetes API's (a *clusterError): the caller
// then runs nothing else on the merged pull request, as for errMergeRecordPending.
func (r *RemediationRequestReconciler) awaitOtherRecord(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	v changeVerdict, other string) error {
	changed := meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{
		Type:   remediationv1alpha1.ConditionApproved,
		Status: metav1.ConditionFalse,
		Reason: ReasonMergeRecordPending,
		Message: fmt.Sprintf("Pull request #%d was merged, and RemediationRequest %s, the other request of its "+
			"handoff, records the merge; this request waits for that record while RemediationRequest %s holds the target or its handoff, "+
			"and writes nothing to the pull request", v.pr.Number, other, other),
		ObservedGeneration: rr.Generation,
	})
	if changed {
		if err := r.Status().Update(ctx, rr); err != nil {
			// Under errMergeCheck, so that nothing after the look runs on the merged pull request (no full freshness
			// check, no check run, no close): a lost or conflicting write of the wait is a merge seen and not recorded,
			// and the Kubernetes API's failure goes to the error backoff (sixth round of #786).
			return &clusterError{err: fmt.Errorf("%w: update status of RemediationRequest %s/%s: %w", errMergeCheck,
				rr.Namespace, rr.Name, err)}
		}
		logf.FromContext(ctx).Info("The pull request of RemediationRequest was merged, and the other request of its "+
			"handoff records the merge; waiting for that record",
			"remediationRequest", client.ObjectKeyFromObject(rr), "pullRequest", v.pr.Number, "recordedBy", other)
	}
	return errMergeRecordPending
}

// errRecordHeld is what every Git write of a request returns, instead of calling the host, while it waits for the other
// request of its handoff to record a merge of their pull request (recordHeld).
var errRecordHeld = errors.New("waiting for the other request of the handoff to record the merge: nothing is written to Git")

type recordHoldKey struct{}

// withRecordHold returns ctx carrying the record hold of rr, the request a reconcile works on: every Git write made with
// ctx or a context derived from it passes it in gatedGit.gate, before the kill switch, whatever step makes the write
// and whichever provider it opened. rr is read at each write, so the hold follows what the reconcile stored meanwhile.
func (r *RemediationRequestReconciler) withRecordHold(ctx context.Context,
	rr *remediationv1alpha1.RemediationRequest) context.Context {
	return context.WithValue(ctx, recordHoldKey{}, func(ctx context.Context) error { return r.recordHeld(ctx, rr) })
}

// recordHold asks the record hold ctx carries (withRecordHold); nil when it carries none.
func recordHold(ctx context.Context) error {
	if hold, ok := ctx.Value(recordHoldKey{}).(func(context.Context) error); ok {
		return hold(ctx)
	}
	return nil
}

// recordHeld returns errRecordHeld while rr waits for the other request of its handoff to record a merge of their pull
// request (decision of 2026-11-19 on #880, "a pending wait is a claim"): the other request holds a claim on a merge of
// that pull request (holdsMergeClaim: a wait of kind MergeCheck or MergeContent naming it), or rr shows that it waits
// for that record (condition Approved False with ReasonMergeRecordPending) while the other may still record it
// (mayStillRecord). It decides on live copies of both requests, read from the API server at the write (decision of
// 2026-11-19 on #786): rr's copy in the reconcile may be older than what is stored, and a write that would come before
// the look at the pull request in a reconcile (the rewrite after a handoff, followHandoff) is held whatever that copy
// shows. rr's own copy can only add a hold (a wait it shows that a lost write did not store). It is decided from what
// is stored, not from a read of the Git host. A request with no handoff, or whose other request is gone or made
// again, is not held. A failed read, or rr gone or made again, holds the write too, as freshness.ErrWriteBlocked (the
// Kubernetes API's), for a retry.
func (r *RemediationRequestReconciler) recordHeld(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) error {
	live := &remediationv1alpha1.RemediationRequest{}
	switch err := r.getLive(ctx, client.ObjectKeyFromObject(rr), live); {
	case err != nil:
		return fmt.Errorf("%w: get RemediationRequest %s/%s before a Git write: %w", freshness.ErrWriteBlocked,
			rr.Namespace, rr.Name, err)
	case live.UID != rr.UID:
		return fmt.Errorf("%w: RemediationRequest %s/%s was replaced", freshness.ErrWriteBlocked, rr.Namespace, rr.Name)
	}
	// A request in the middle of a chain has a handoff at both ends: each is asked, by the ends either copy names.
	asked := map[string]bool{}
	for _, x := range []*remediationv1alpha1.RemediationRequest{live, rr} {
		ho := x.Status.Handoff
		if ho == nil {
			continue
		}
		for _, end := range []struct {
			name string
			uid  remediationv1alpha1.UID
			ok   bool
		}{
			{name: nameOfFrom(ho), uid: uidOfFrom(ho), ok: ho.From != nil},
			{name: nameOfTo(ho), uid: uidOfTo(ho), ok: ho.To != nil},
		} {
			if !end.ok || asked[end.name] {
				continue
			}
			asked[end.name] = true
			if err := r.heldBy(ctx, live, rr, end.name, end.uid); err != nil {
				return err
			}
		}
	}
	return nil
}

func nameOfFrom(ho *remediationv1alpha1.Handoff) string {
	if ho.From == nil {
		return ""
	}
	return string(ho.From.Name)
}

func uidOfFrom(ho *remediationv1alpha1.Handoff) remediationv1alpha1.UID {
	if ho.From == nil {
		return ""
	}
	return ho.From.UID
}

func nameOfTo(ho *remediationv1alpha1.Handoff) string {
	if ho.To == nil {
		return ""
	}
	return string(ho.To.Name)
}

func uidOfTo(ho *remediationv1alpha1.Handoff) remediationv1alpha1.UID {
	if ho.To == nil {
		return ""
	}
	return ho.To.UID
}

// waitsOnRecord reports whether x shows that it waits for the other request of its handoff to record a merge
// (condition Approved False with ReasonMergeRecordPending).
func waitsOnRecord(x *remediationv1alpha1.RemediationRequest) bool {
	c := meta.FindStatusCondition(x.Status.Conditions, remediationv1alpha1.ConditionApproved)
	return c != nil && c.Status == metav1.ConditionFalse && c.Reason == ReasonMergeRecordPending
}

// heldBy is recordHeld for one other request of the handoffs of live (the request as stored) and rr (the reconcile's
// copy of it), by its name and UID; the other request is read from the API server here.
func (r *RemediationRequestReconciler) heldBy(ctx context.Context, live, rr *remediationv1alpha1.RemediationRequest,
	name string, uid remediationv1alpha1.UID) error {
	other := &remediationv1alpha1.RemediationRequest{}
	switch err := r.getLive(ctx, types.NamespacedName{Namespace: live.Namespace, Name: name}, other); {
	case apierrors.IsNotFound(err):
		return nil
	case err != nil:
		return fmt.Errorf("%w: get RemediationRequest %s of the handoff of %s: %w", freshness.ErrWriteBlocked, name,
			live.Name, err)
	}
	if other.UID != types.UID(uid) {
		return nil
	}
	pr := live.Status.PullRequest
	if pr == nil {
		pr = rr.Status.PullRequest
	}
	if pr == nil {
		// A replacement before its takeover: the pull request of the handoff is the predecessor's.
		pr = other.Status.PullRequest
	}
	if pr == nil {
		return nil
	}
	for _, k := range readWaits {
		if w := gitWait(other, k); w != nil && w.PullRequest != nil && w.PullRequest.Number == pr.Number &&
			holdsMergeClaim(other, pr.Number, w.PullRequest.HeadSHA) {
			return errRecordHeld
		}
	}
	if (waitsOnRecord(live) || waitsOnRecord(rr)) && mayStillRecord(other) {
		return errRecordHeld
	}
	return nil
}

// afterUpdate reads the pull request after the branch update (ADR-0028 section 4 item 5), by the number the
// predecessor recorded (ADR-0030 section 1): open, the replacement records it (step 4); merged with the new
// commit as its head, the replacement owns the merge; merged with another head, closed, or not the predecessor's
// any more, the handoff is abandoned. A 404 is read again (findHanded): the new commit is on the branch, and a
// pull request that may still be open carries it.
func (r *RemediationRequestReconciler) afterUpdate(ctx context.Context, git providers.GitProvider,
	rr, pred *remediationv1alpha1.RemediationRequest, newHead string) (reconcile.Result, error) {
	cr, gone, err := r.findHanded(ctx, git, pred)
	switch {
	case err != nil:
		return reconcile.Result{}, err
	case gone != "":
		return reconcile.Result{}, r.endHandoff(ctx, rr, remediationv1alpha1.ReasonHandoffAborted,
			"The pull request of the request it replaces is not its own any more ("+gone+"); nothing was taken over")
	}
	switch {
	case cr.State == providers.ChangeRequestOpen,
		cr.State == providers.ChangeRequestMerged && cr.HeadSHA == newHead:
		return reconcile.Result{RequeueAfter: stepDelay}, r.recordHandoff(ctx, rr, pred, cr, newHead)
	}
	return reconcile.Result{}, r.endHandoff(ctx, rr, remediationv1alpha1.ReasonHandoffAborted,
		"The pull request was "+closedOrMerged(cr)+" around the handoff with the old plan; nothing was taken over")
}

// findHanded reads the predecessor's recorded pull request by its number (changeOf). Unknown (a failed read, a 404
// the listing of the branch does not confirm, an answer about another pull request) is returned as an error that
// keeps the provider's, for a retry within the handoff's deadline (retryRecovery after the update, the request's
// deadline before it): no single read aborts a handoff (#688). Gone (a 404 the listing of the branch confirms, or
// the pull request with that number is from another branch or to another base branch) is returned with its
// reason, and the caller ends the handoff on it.
func (r *RemediationRequestReconciler) findHanded(ctx context.Context, git providers.GitProvider,
	pred *remediationv1alpha1.RemediationRequest) (providers.ChangeRequest, string, error) {
	if pred.Status.PullRequest == nil {
		return providers.ChangeRequest{}, "", errors.New("no pull request recorded")
	}
	v := r.changeOf(ctx, git, pred, changeQuery{})
	switch v.state {
	case changeUnknown:
		return providers.ChangeRequest{}, "", v.err
	case changeGone, changeMergedElsewhere:
		return providers.ChangeRequest{}, v.gone, nil
	case changeElsewhere:
		// Open on a base a person moved it to (#892): it is not taken over, and nothing is written to it there.
		return providers.ChangeRequest{}, openElsewhereText(v.pr), nil
	case changeOpen, changeMerged, changeClosed:
	}
	return v.pr, "", nil
}

func closedOrMerged(cr providers.ChangeRequest) string {
	if cr.State == providers.ChangeRequestMerged {
		return "merged"
	}
	return "closed"
}

// recordHandoff is step 4: the replacement records the pull request with the new head, carries every hint of
// the chain (the merge gate excludes all their authors), the predecessor's refused commands (with their
// replies, so each is answered once) and its comment cursor, sets from Done
// and moves to PRCreated, or ends Merged when the merge took the new commit. Its failures are the Kubernetes API's
// (the check at the merge, which reads more, never fails it), and are marked so (clusterSideErr).
func (r *RemediationRequestReconciler) recordHandoff(ctx context.Context, rr, pred *remediationv1alpha1.RemediationRequest,
	cr providers.ChangeRequest, newHead string) error {
	if err := r.writeAllowed(ctx); err != nil {
		return clusterSideErr(err)
	}
	unreadable := takeOverInto(ctx, rr, pred, cr, newHead)
	// Merged without a full merge commit ID yet: recorded like an open one, and the look at the recorded pull
	// request waits for the commit (awaitMergeCommit) and records the merge. With one, the plan is checked at the
	// merge first (checkAtMerge, ADR-0041): a stale one is marked in the write that sets Merged. When the check
	// cannot read what it needs, the takeover is recorded like an open pull request in the same way (with the
	// start of the wait checkAtMerge recorded), and the look at the recorded pull request checks the plan again
	// before it records the merge, until the wait is over; a failed read never records the merge as fresh.
	if cr.State == providers.ChangeRequestMerged && shaPattern.MatchString(cr.MergeCommit) {
		err := r.checkAtMerge(ctx, rr, cr)
		if err == nil {
			recordMerge(ctx, rr, cr)
			err := r.stop(ctx, rr, remediationv1alpha1.PhaseMerged, ReasonChangeRequestMerged,
				"The pull request was merged with this request's change, right after the handoff")
			if errors.Is(err, errStopped) {
				r.unreadableEvent(rr, unreadable, nil)
			}
			return clusterSideErr(r.ignoreStopped(err))
		}
		logf.FromContext(ctx).Error(err, "Recorded the takeover of a merged pull request without its merge: the "+
			"plan could not be checked at the merge", "remediationRequest", client.ObjectKeyFromObject(rr),
			"pullRequest", cr.Number)
	}
	r.setPhase(rr, remediationv1alpha1.PhasePRCreated)
	// A merge seen without its merge commit ID starts the wait for it in the same write (as awaitMergeCommit would on
	// the next look), so the status records that the pull request is merged: a next look that cannot read it is a
	// merge seen and not recorded (awaitedMergeUnread), and nothing is written to the merged pull request on it (#786).
	if cr.State == providers.ChangeRequestMerged && gitWait(rr, waitMergeCheck) == nil {
		startMergeWaitIn(rr, cr, "wait for the merge commit", r.now())
	}
	if err := r.Status().Update(ctx, rr); err != nil {
		return &clusterError{err: fmt.Errorf("update status of RemediationRequest %s/%s: %w", rr.Namespace, rr.Name, err)}
	}
	r.unreadableEvent(rr, unreadable, nil)
	logf.FromContext(ctx).Info("Took over the pull request for RemediationRequest", "remediationRequest",
		client.ObjectKeyFromObject(rr), "pullRequest", cr.Number, "replaces", pred.Name)
	return nil
}

// takeOverInto makes the changes of the takeover (step 4) in rr, unwritten: from Done, every hint of the chain, the
// predecessor's refused commands and its comment cursor carried over, the pull request recorded with newHead, the
// deadline gone, condition PullRequestOpened set and PullRequestUnreadable removed. It returns the removed
// PullRequestUnreadable, for its Normal Event once the caller's write went through. pred may hold only the name of
// the predecessor once it is gone.
func takeOverInto(ctx context.Context, rr, pred *remediationv1alpha1.RemediationRequest, cr providers.ChangeRequest,
	newHead string) *metav1.Condition {
	from := rr.Status.Handoff.From
	from.Phase = remediationv1alpha1.HandoffDone
	for _, h := range pred.Status.Hints {
		consumed := h.State == remediationv1alpha1.HintConsumed
		answered := refOf(&h) == from.Hint
		if findHint(rr.Status.Hints, h.Source, h.CommentID) != nil {
			continue
		}
		switch {
		case consumed || answered:
			h.State, h.Reason = remediationv1alpha1.HintConsumed, ""
		case h.State == remediationv1alpha1.HintPending:
			// Another hint of the same pull request, accepted before the intent: the cursor carried over is past
			// its comment, and the predecessor ends without answering it, so this request answers it, anew.
			h.Reason, h.Outcome = "", ""
		case h.State == remediationv1alpha1.HintDeclined:
			// Kept as it is, answered: its author wrote a hint in this pull request all the same, so their
			// approval never counts for the merge gate, whatever became of the hint.
		default:
			continue
		}
		rr.Status.Hints = append(rr.Status.Hints, h)
	}
	// The refused commands too, with their replies: the predecessor posts nothing while its intent stands and
	// ends without replying, so this request owes the replies it has not posted.
	for _, f := range pred.Status.RefusedHints {
		if !findRefused(rr.Status.RefusedHints, f.Source, f.CommentID) && !addRefused(&rr.Status, f) {
			logf.FromContext(ctx).Info("Did not carry a refused reviewer hint over to RemediationRequest: every "+
				"recorded refusal still waits for its reply", "remediationRequest", client.ObjectKeyFromObject(rr),
				"comment", f.CommentID)
		}
	}
	if rr.Status.CommentCursor == nil && pred.Status.CommentCursor != nil {
		rr.Status.CommentCursor = pred.Status.CommentCursor.DeepCopy()
	}
	recordNumber(rr, remediationv1alpha1.PullRequestStatus{Number: cr.Number, URL: cr.URL, HeadSHA: newHead})
	rr.Status.Deadline = nil
	// The reads of the takeover worked: condition PullRequestUnreadable of a recovery that met a failure before (#688,
	// #786) goes in the same write, and its Normal Event after it.
	var unreadable *metav1.Condition
	if c := meta.FindStatusCondition(rr.Status.Conditions, remediationv1alpha1.ConditionPullRequestUnreadable); c != nil {
		unreadable = c.DeepCopy()
		meta.RemoveStatusCondition(&rr.Status.Conditions, remediationv1alpha1.ConditionPullRequestUnreadable)
	}
	meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{
		Type:               remediationv1alpha1.ConditionPROpened,
		Status:             metav1.ConditionTrue,
		Reason:             ReasonPullRequestOpened,
		Message:            fmt.Sprintf("Pull request #%d is open; this request took it over from the request it replaces", cr.Number),
		ObservedGeneration: rr.Generation,
	})
	return unreadable
}

// endHandoff ends a replacement whose handoff did not happen: Cancelled with reason. The predecessor keeps everything
// it had, and aborts its own intent when it sees this one ended (settleHandoff). A replacement that may have pushed its
// commit (mayCarryHandoffCommit: its source is recorded) keeps its handoff Reserved and its lock: the commit may sit on
// the predecessor's pull request, and the look at stopped holders reads that pull request (heldRecovery) and records a
// merge of the commit or releases the lock on a verdict (#786). One that cannot have pushed is Aborted and releases
// its lock in the same write, as it holds no pull request of its own. Its failures are the Kubernetes API's, and are
// marked so (clusterSideErr).
func (r *RemediationRequestReconciler) endHandoff(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	reason, message string) error {
	if err := r.writeAllowed(ctx); err != nil {
		return clusterSideErr(err)
	}
	if !mayCarryHandoffCommit(rr) {
		if from := rr.Status.Handoff.From; from.Phase == remediationv1alpha1.HandoffReserved {
			from.Phase = remediationv1alpha1.HandoffAborted
		}
		if s, ok := settleHandoffAborted(rr); ok {
			if err := lockverdict.Release(&rr.Status, s); err != nil {
				return err
			}
		}
	}
	return clusterSideErr(r.stop(ctx, rr, remediationv1alpha1.PhaseCancelled, reason, message))
}

// mayCarryHandoffCommit reports whether rr is a replacement with no pull request of its own whose handoff commit may
// be on its predecessor's pull request: its source and the plan's repository are recorded, which the freshness check
// before every push records, so the branch could have been written. It reads the status only.
func mayCarryHandoffCommit(rr *remediationv1alpha1.RemediationRequest) bool {
	ho := rr.Status.Handoff
	return rr.Spec.Replaces != nil && ho != nil && ho.From != nil && rr.Status.PullRequest == nil &&
		rr.Status.Source != nil && rr.Status.PlanRepository != nil
}

// followHandoff is step 6 of a replacement that took the pull request over, once and recorded: while the pull
// request waits for review, its title and body are rewritten for this request's plan, with its marker. A final
// refusal of the Git host is recorded as Refused, never as done, so it is not asked again on every look, and
// says so (condition PullRequestOpened with ReasonPullRequestRewriteRefused, a Warning Event); a pull request
// closed or merged meanwhile leaves the step for the watch to decide. The answer of the Git host is kept in
// memory until its status write went through, so a write that failed is retried without the rewrite being sent
// again (after a restart it is sent again, which is idempotent). An error is returned for a retry, wrapping
// errStepNotRecorded when only the status write failed; rr is updated in place by the status write when it
// went through. The check run shadewright/review is not this step's: it is created on every head after the
// look at the comments (reviewCheck), so the merge gate never decides before the hints of that look are
// stored. The caller has done step 5 first.
func (r *RemediationRequestReconciler) followHandoff(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) error {
	from := rr.Status.Handoff.From
	p := rr.Status.Phase
	if (p != remediationv1alpha1.PhasePRCreated && p != remediationv1alpha1.PhaseAwaitingApproval) ||
		rr.Status.PullRequest == nil || from.PullRequestUpdate != "" || recordedMergeWait(rr) != nil {
		// Nothing to rewrite on a pull request whose merge the status records as seen (#786).
		return nil
	}
	stepCtx, cancel := bound(ctx, r.WriteTimeout, DefaultWriteTimeout)
	defer cancel()
	key := client.ObjectKeyFromObject(rr)
	pr := rr.Status.PullRequest
	done := rewriteDone{uid: rr.UID, number: pr.Number}
	if v, ok := r.rewritten.Load(key); ok && v.(rewriteDone).uid == done.uid && v.(rewriteDone).number == done.number {
		done = v.(rewriteDone)
	} else {
		git, err := r.openPlanRepository(stepCtx, rr)
		if err != nil {
			return err
		}
		log := logf.FromContext(ctx).WithValues("remediationRequest", key)
		// pullRequestText reads from the API server only for a revert, and a revert never reaches this step: it
		// never replaces another request (CEL on the spec, which is immutable), and only a request with
		// spec.replaces gets status.handoff.from (reserveOnAdmission). Here it reads nothing and never fails, so no
		// failed read of a reverted request can end in the caller's log at Info.
		title, body, err := r.pullRequestText(stepCtx, rr)
		if err != nil {
			return err
		}
		// Read by its number first: a pull request a person moved to another base, or one closed or merged meanwhile,
		// is left to the watch, which writes nothing to it there (ADR-0043). Unknown is retried.
		switch v := r.changeOf(stepCtx, git, rr, changeQuery{}); v.state {
		case changeUnknown:
			return v.err
		case changeOpen:
		case changeMerged, changeClosed, changeGone, changeElsewhere, changeMergedElsewhere:
			return nil
		}
		err = git.UpdateChangeRequest(stepCtx, pr.Number, title, body)
		step, err := stepOutcome(log, err, "rewrite the pull request")
		switch {
		case err != nil:
			return freshness.WrapProvider("rewrite the pull request", err)
		case step == "":
			// Closed or merged meanwhile: the watch of the pull request decides; nothing is left to rewrite.
			return nil
		}
		done.step = step
		r.rewritten.Store(key, done)
	}
	// A failed read of the settings here is not a failed status write: it is logged and the next look records
	// the answer kept above.
	if err := r.writeAllowed(stepCtx); err != nil {
		return err
	}
	upd := rr.DeepCopy()
	upd.Status.Handoff.From.PullRequestUpdate = done.step
	if done.step == remediationv1alpha1.HandoffStepRefused {
		meta.SetStatusCondition(&upd.Status.Conditions, metav1.Condition{
			Type:               remediationv1alpha1.ConditionPROpened,
			Status:             metav1.ConditionTrue,
			Reason:             ReasonPullRequestRewriteRefused,
			Message:            rewriteRefused,
			ObservedGeneration: upd.Generation,
		})
	}
	if err := r.Status().Update(stepCtx, upd); err != nil {
		return fmt.Errorf("%w: update status of RemediationRequest %s/%s: %w", errStepNotRecorded, rr.Namespace, rr.Name, err)
	}
	upd.DeepCopyInto(rr)
	r.rewritten.Delete(key)
	if done.step == remediationv1alpha1.HandoffStepRefused && r.Recorder != nil {
		r.Recorder.Event(rr, corev1.EventTypeWarning, ReasonPullRequestRewriteRefused, rewriteRefused)
	}
	return nil
}

// rewriteDone is what rewritten holds per request: the answer of the Git host to the rewrite of pull request
// number for the request with uid, whose status write has not gone through yet.
type rewriteDone struct {
	uid    types.UID
	number int64
	step   remediationv1alpha1.HandoffStep
}

// ReasonPullRequestRewriteRefused is the reason of condition PullRequestOpened (True) and of the Warning Event
// when the Git host refused for good to rewrite the title and body of a pull request taken over: the pull
// request is the replacement's, but its text still shows the plan it replaced.
const ReasonPullRequestRewriteRefused = "PullRequestRewriteRefused"

// rewriteRefused is the fixed message of ReasonPullRequestRewriteRefused.
const rewriteRefused = "This request took the pull request over, but the Git host refused for good to rewrite its " +
	"title and body (a permission of the App, a rule, or text over the host's limit), so they still describe the " +
	"plan it replaced; the branch carries this request's plan"

// errStepNotRecorded means a step after the handoff was done on the Git host but its status write failed: the
// caller retries it with backoff, and the answer of the host kept in memory is recorded without asking again.
var errStepNotRecorded = errors.New("the step after the handoff was not recorded")

// stepOutcome maps the answer of a write after the handoff to its record: Done, Refused for a final refusal
// (logged), "" for a conflict (closed or merged meanwhile; nothing is recorded), or the error for a retry.
func stepOutcome(log logr.Logger, err error, what string) (remediationv1alpha1.HandoffStep, error) {
	switch {
	case err == nil:
		return remediationv1alpha1.HandoffStepDone, nil
	case providers.IsConflict(err):
		return "", nil
	}
	if reason, _, final := refusal(err); final {
		log.Info("The Git host refused a step after the handoff of RemediationRequest; it is not asked again",
			"step", what, "reason", reason)
		return remediationv1alpha1.HandoffStepRefused, nil
	}
	return "", err
}

// reserveOnAdmission is the replacement's part of handoff step 1, in the status write that admits it: it names
// the request it takes over, Reserved. Any other request is left as it is.
func reserveOnAdmission(rr *remediationv1alpha1.RemediationRequest) {
	rep := rr.Spec.Replaces
	if rep == nil || (rr.Status.Handoff != nil && rr.Status.Handoff.From != nil) {
		return
	}
	rr.Status.Handoff = &remediationv1alpha1.Handoff{From: &remediationv1alpha1.IncomingHandoff{
		Name: rep.Name, UID: rep.UID, Hint: rep.Hint, Phase: remediationv1alpha1.HandoffReserved}}
}

// afterHandoff runs the steps after a handoff for a request that took a pull request over (Reconcile, any
// phase): the predecessor ends HandedOff, returned as an error until it did so it is retried with backoff; the
// pull request is rewritten while it waits. A failure of the rewrite on the Git host (or of a read of the
// settings) is logged and retried with the next look, which the poll of the waiting pull request brings; a
// final refusal is recorded Refused by followHandoff and not asked again. A failed status write after the
// rewrite is returned wrapping errStepNotRecorded: the caller still runs the recheck, so a merge or a close is
// not missed, and returns it for a retry with backoff, which does not send the rewrite again. A cancelled
// reconcile returns at once, not logged as a failure. A pause leaves both steps for later. The check run is not
// done here but after the look at the comments (reviewCheck).
// With reviewer hints off it does neither step: a request that still progresses stops (hintsOffTakenOver).
func (r *RemediationRequestReconciler) afterHandoff(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) error {
	if !tookOver(rr) {
		return nil
	}
	if !r.ReviewerHints {
		// Hints off fails closed (ADR-0028 amendment of 2026-11-10): a request that took the pull request over
		// before a restart stops (hintsOffTakenOver), and neither supersedes its predecessor nor rewrites the pull
		// request, at any phase, an ended one included. A predecessor not ended yet aborts its intent
		// (declineHintsOff) and stops as for a foreign push, as when the record of the handoff was lost.
		return r.ignoreStopped(r.hintsOffTakenOver(ctx, rr))
	}
	if err := r.supersedePredecessor(ctx, rr); err != nil && !errors.Is(err, freshness.ErrPaused) {
		return err
	}
	err := r.followHandoff(ctx, rr)
	switch {
	case err == nil, errors.Is(err, freshness.ErrPaused):
		return nil
	case ctx.Err() != nil:
		return fmt.Errorf("steps after the handoff cancelled: %w", ctx.Err())
	case errors.Is(err, errStepNotRecorded):
		return err
	}
	logf.FromContext(ctx).Info("Could not finish the steps after the handoff of RemediationRequest",
		"remediationRequest", client.ObjectKeyFromObject(rr), "error", err.Error())
	return nil
}

// msgHintsOffTakenOver is the message of a request that took a pull request over and stops because reviewer
// hints are off: its head was steered by a hint and has no merge gate.
const msgHintsOffTakenOver = msgHintsTurnedOff + "; this request took the pull request over for a reviewer " +
	"hint, and the check shadewright/review is no longer kept on its head: automation stopped, close the pull " +
	"request"

// hintsOffTakenOver stops, NeedsIntervention with ReasonHintsTurnedOff, a request that took a pull request over
// (tookOver) and still progresses, for an executor running with reviewer hints off: its head was steered by a
// hint, and without hints the check run shadewright/review is neither created nor completed (reviewCheck), so
// the request must not reach or stay in AwaitingApproval. A request that ended is left as it is. The stop is
// not held back by the kill switch: it writes nothing to Git and only stops automation, like the stops of the
// watch. The error wraps errStopped once rr stopped, and is nil when there was nothing to stop.
func (r *RemediationRequestReconciler) hintsOffTakenOver(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) error {
	if r.ReviewerHints || !tookOver(rr) || !progresses(rr) {
		return nil
	}
	return r.stop(ctx, rr, remediationv1alpha1.PhaseNeedsIntervention, remediationv1alpha1.ReasonHintsTurnedOff,
		msgHintsOffTakenOver)
}

// supersedePredecessor is step 5: the predecessor whose intent names rr ends Superseded with reason HandedOff,
// its to Done, its hint Consumed and its lock released, in one status write. A predecessor that is gone, or
// already handed over, needs nothing. It reads the cache first, which costs nothing on every later reconcile;
// a cached copy that does not show the end yet is read again from the API server, so a cache that has not seen
// the intent never leaves the predecessor holding the lock. A copy older than the predecessor makes the write a
// conflict, retried.
func (r *RemediationRequestReconciler) supersedePredecessor(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) error {
	from := rr.Status.Handoff.From
	key := types.NamespacedName{Namespace: rr.Namespace, Name: string(from.Name)}
	pred := &remediationv1alpha1.RemediationRequest{}
	switch err := r.Get(ctx, key, pred); {
	case apierrors.IsNotFound(err):
		return nil
	case err != nil:
		return fmt.Errorf("get the RemediationRequest %s replaces: %w", rr.Name, err)
	}
	if pred.UID != types.UID(from.UID) || handedOff(pred) {
		return nil
	}
	if to := intent(pred); !ended(pred.Status.Phase) && (to == nil || to.UID != remediationv1alpha1.UID(rr.UID)) {
		switch err := r.getLive(ctx, key, pred); {
		case apierrors.IsNotFound(err):
			return nil
		case err != nil:
			return fmt.Errorf("get the RemediationRequest %s replaces: %w", rr.Name, err)
		}
	}
	to := intent(pred)
	if pred.UID != types.UID(from.UID) {
		return nil
	}
	if to == nil || to.UID != remediationv1alpha1.UID(rr.UID) {
		return r.supersedeAfterAbort(ctx, rr, pred)
	}
	if err := r.writeAllowed(ctx); err != nil {
		return err
	}
	to.Phase = remediationv1alpha1.HandoffDone
	if h := hintByRef(pred, to.Hint); h != nil {
		h.State = remediationv1alpha1.HintConsumed
	}
	if s, ok := settleHandedOver(to, rr); ok {
		if err := lockverdict.Release(&pred.Status, s); err != nil {
			return err
		}
	}
	err := r.stop(ctx, pred, remediationv1alpha1.PhaseSuperseded, remediationv1alpha1.ReasonHandedOff,
		fmt.Sprintf("RemediationRequest %s took over the pull request and its branch with a new plan", rr.Name))
	if !errors.Is(err, errStopped) {
		return err
	}
	return nil
}

// supersedeAfterAbort is step 5 for rr, a replacement that recorded the merge of its commit on its predecessor's pull
// request after the predecessor's intent was aborted (a refusal, or a takeover recorded only after the abort; #786):
// the intent cannot become Done any more, so the predecessor, pred, ends Superseded with ReasonMergedForReplacement
// and releases its lock (leaveReplacementMerge), unless it records that end already. The merge is the replacement's
// (mergedHeadOwner), as the replacement's own record of it shows (holdsMergeRecord: its receipt, or a merge recorded
// without one, for the pull request pred recorded), so the predecessor never records it, also once the replacement is
// deleted. Anything else needs nothing.
func (r *RemediationRequestReconciler) supersedeAfterAbort(ctx context.Context, rr, pred *remediationv1alpha1.RemediationRequest) error {
	var to *remediationv1alpha1.OutgoingHandoff
	if pred.Status.Handoff != nil {
		to = pred.Status.Handoff.To
	}
	if to == nil || to.UID != remediationv1alpha1.UID(rr.UID) || to.Phase != remediationv1alpha1.HandoffAborted ||
		rr.Status.PullRequest == nil || pred.Status.PullRequest == nil ||
		!holdsMergeRecord(rr, pred.Status.PullRequest.Number, rr.Status.PullRequest.HeadSHA) || mergeRecorded(pred) {
		return nil
	}
	if c := meta.FindStatusCondition(pred.Status.Conditions, remediationv1alpha1.ConditionApproved); c != nil &&
		pred.Status.Phase == remediationv1alpha1.PhaseSuperseded && c.Reason == ReasonMergedForReplacement {
		return nil
	}
	if err := r.writeAllowed(ctx); err != nil {
		return err
	}
	// The verdict is the replacement's record of the merge of its own commit on this pull request.
	v := changeVerdict{state: changeMerged, pr: providers.ChangeRequest{Number: rr.Status.PullRequest.Number,
		HeadSHA: rr.Status.PullRequest.HeadSHA, State: providers.ChangeRequestMerged}}
	if err := r.leaveReplacementMerge(ctx, pred, v, ownerReplacement); err != nil && !errors.Is(err, errStopped) {
		return err
	}
	return nil
}

// recoverHandoff runs before the freshness check of a replacement admitted with from Reserved whose predecessor
// records its intent: when the branch head is not the expected one but is this handoff's commit
// (isHandoffCommit, which does not depend on the target branch), the update happened before a restart or a lost
// write, and the request goes on to the read after the update (afterUpdate) instead of the freshness check, so a
// merge of the new commit, or a target branch that moved since, is the replacement's and never taken for a
// stale plan. done is false when there is nothing to recover; the caller then goes on as before, after condition
// PullRequestUnreadable of an earlier look that could not read is removed. A final refusal of a read of the recovery
// aborts the handoff (abortOnRefusal, #786), and from then on the request only watches for a merge of its commit until
// the end of its HandoffRecovery wait (observeStep, observeFailed), never taking the pull request over again; done is
// false then only for a cancel whose read worked. With spec.cancel set, any other failure of the recovery is the
// cancel's (cancelRecoveryFailed), never an end of the request (#289); without it, retryRecovery.
func (r *RemediationRequestReconciler) recoverHandoff(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) (
	res reconcile.Result, done bool, err error) {
	ho := rr.Status.Handoff
	if rr.Spec.Replaces == nil || ho == nil || ho.From == nil || ho.From.Phase != remediationv1alpha1.HandoffReserved ||
		rr.Status.Phase != remediationv1alpha1.PhasePolicyChecked || rr.Status.Source == nil ||
		rr.Status.PlanRepository == nil || staleMark(rr) != nil {
		return reconcile.Result{}, false, nil
	}
	stepCtx, cancel := bound(ctx, r.WriteTimeout, DefaultWriteTimeout)
	defer cancel()
	// The step works on a copy: a write of it that fails (the end of the handoff, the takeover) leaves rr as it was
	// read, so the failure is sorted on the recorded state, never on a phase or a condition that was not written. A
	// write of the step that did go through before it failed (the start of the wait of the check at the merge) makes
	// a later write of rr a conflict, retried after a new read.
	work := rr.DeepCopy()
	res, done, err = r.recoverStep(stepCtx, work)
	err = recoveryStepErr(ctx, stepCtx, err)
	if err == nil || errors.Is(err, errStopped) {
		work.DeepCopyInto(rr)
	}
	switch {
	case errors.Is(err, errStopped):
		return reconcile.Result{}, true, nil
	case err != nil && abortedOnRefusal(rr):
		res, err = r.observeFailed(ctx, rr, err)
		return res, true, err
	case err == nil && !done && abortedOnRefusal(rr):
		// A cancel whose read worked: the cancel goes on (observeStep removed the condition).
		return res, false, nil
	case err == nil && !done:
		// Nothing to recover: a condition of an earlier look that could not read is removed before the request goes
		// on. A takeover removes it in its own write (recordHandoff).
		if werr := r.setUnreadable(ctx, rr, nil); werr != nil {
			return reconcile.Result{}, true, werr
		}
		return res, false, nil
	case err != nil && ctx.Err() == nil && !errors.Is(err, freshness.ErrPaused) && recoveryUnreadable(rr, err) != nil:
		refused := err
		res, err = r.abortOnRefusal(ctx, rr, recoveryUnreadable(rr, refused), refused)
		if err == nil && cancelRequested(rr) && abortedOnRefusal(rr) {
			// The cancel cannot finish while nothing can be read: it is watched with the refusal's reason (#289).
			res, err = r.cancelRecoveryFailed(ctx, rr, refused)
		}
		return res, true, err
	case err != nil && cancelRequested(rr):
		res, err = r.cancelRecoveryFailed(ctx, rr, err)
		return res, true, err
	case err != nil:
		res, err = r.retryRecovery(ctx, rr, err)
		return res, true, err
	}
	return res, done, nil
}

// handoffRecoveryGrace is how long after its deadline a replacement keeps retrying a recovery that fails for a
// reason worth retrying (the Git host did not answer): its commit may be on the pull request already, so the
// deadline alone does not end it, but the target is not held for as long as the failure lasts. A failure of the
// Kubernetes API never reaches it: that is the error backoff's, whatever the deadline. A final refusal aborts the
// handoff instead (abortOnRefusal, #786).
const handoffRecoveryGrace = 15 * time.Minute

// ReasonHandoffRecoveryFailed is the reason of NeedsIntervention when the recovery of a handoff kept failing on
// the Git host until handoffRecoveryGrace after the deadline: whether the handoff commit is on the pull request is
// unknown.
const ReasonHandoffRecoveryFailed = "HandoffRecoveryFailed"

// retryRecovery is retry for an error of recoverStep that is no final refusal (those abort the handoff,
// abortOnRefusal), as before #786: before the deadline it is retry; past it a failed Git read is retried visibly
// (retryGitRead) within the grace (the HandoffGrace entry), and any other failure worth retrying after the delay the
// host asked for (Retry-After, which the deadline does not cut short) or with the error backoff, until
// handoffRecoveryGrace after the deadline; then the request ends NeedsIntervention with ReasonHandoffRecoveryFailed,
// and its handoff Aborted (declineOnEnd). A failure of the Kubernetes API (a *clusterError) is never taken for the Git
// host's: past the deadline it goes to the error backoff and ends nothing (decision of 2026-11-17).
func (r *RemediationRequestReconciler) retryRecovery(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	err error) (reconcile.Result, error) {
	paused := errors.Is(err, freshness.ErrPaused)
	if rr.Status.Deadline == nil || !r.now().After(rr.Status.Deadline.Time) || ctx.Err() != nil || paused {
		return r.retry(ctx, rr, err)
	}
	if isClusterError(err) {
		return reconcile.Result{}, err
	}
	if gitReadFailure(err) {
		// Retried visibly within the grace (waitHandoffGrace): the check at the start of a reconcile past its end
		// ends the request (endPassedWait, endRecoveryFailed), so no failed read, timeout or early return skips it.
		return r.retryGitRead(ctx, rr, err, boundBy(waitHandoffGrace))
	}
	// The end of the recovery's waits, as endPassedWait reads it.
	end, ok := waitHandoffGrace.boundEnd(rr, r.now())
	if !ok {
		end = rr.Status.Deadline.Add(handoffRecoveryGrace)
	}
	left := end.Sub(r.now())
	if left <= 0 {
		// Any other failure is no Git wait: it ends the request here, on its own retry.
		logf.FromContext(ctx).Info("Could not recover the handoff of RemediationRequest before the grace ended",
			"remediationRequest", client.ObjectKeyFromObject(rr), "error", err.Error())
		return reconcile.Result{}, r.endRecoveryFailed(ctx, rr, nil)
	}
	d := retryAfterOf(err)
	if d <= 0 {
		return reconcile.Result{}, err
	}
	d = min(d, max(left, stepDelay))
	logf.FromContext(ctx).Info("Waiting before retrying the recovery of the handoff of RemediationRequest",
		"remediationRequest", client.ObjectKeyFromObject(rr), "error", err.Error(), "retryAfter", d)
	return reconcile.Result{RequeueAfter: d}, nil
}

// endRecoveryFailed ends a request whose HandoffGrace or HandoffRecovery wait passed its own end; refused is its
// HandoffRecovery entry as it stood before the end removed it, nil without one. A replacement whose handoff was
// aborted at a refusal ends with ReasonRecoveryWaitExpired, keeps the target lock and its handoff Reserved
// (endRecoveryWait); any other could not be recovered before handoffRecoveryGrace after its deadline:
// NeedsIntervention with ReasonHandoffRecoveryFailed, its handoff aborted with the end (declineOnEnd).
func (r *RemediationRequestReconciler) endRecoveryFailed(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	refused *remediationv1alpha1.GitWait) error {
	if refused != nil {
		return r.ignoreStopped(r.endRecoveryWait(ctx, rr, *refused))
	}
	return r.ignoreStopped(r.stop(ctx, rr, remediationv1alpha1.PhaseNeedsIntervention,
		ReasonHandoffRecoveryFailed, "The deadline passed and the Git host could not be read to learn whether "+
			"this request's commit is on the pull request it replaces; check the branch and the pull request"))
}

// recoveringHandoff reports whether rr is a replacement whose handoff recoverHandoff still recovers: the
// predecessor reserved, the request in PolicyChecked, no stale plan's cleanup started.
func recoveringHandoff(rr *remediationv1alpha1.RemediationRequest) bool {
	ho := rr.Status.Handoff
	return rr.Spec.Replaces != nil && ho != nil && ho.From != nil && ho.From.Phase == remediationv1alpha1.HandoffReserved &&
		rr.Status.Phase == remediationv1alpha1.PhasePolicyChecked && staleMark(rr) == nil
}

// recoverStep is the look of recoverHandoff. A replacement whose handoff was aborted at a refusal only watches
// (observeStep).
func (r *RemediationRequestReconciler) recoverStep(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) (
	reconcile.Result, bool, error) {
	from := rr.Status.Handoff.From
	if !r.ReviewerHints {
		// Hints off: a commit an executor with hints on pushed before a restart is not recorded either. The
		// predecessor, whose intent is aborted (declineHintsOff), then stops as for a foreign push on its next look,
		// so no Shadewright request holds a head that a hint steered without the merge gate.
		return reconcile.Result{}, true, r.endHandoff(ctx, rr, remediationv1alpha1.ReasonHintsTurnedOff,
			msgHintsTurnedOff+"; nothing was recorded as taken over")
	}
	if abortedOnRefusal(rr) {
		return r.observeStep(ctx, rr)
	}
	pred := &remediationv1alpha1.RemediationRequest{}
	switch err := r.getLive(ctx, types.NamespacedName{Namespace: rr.Namespace, Name: string(from.Name)}, pred); {
	case apierrors.IsNotFound(err):
		return reconcile.Result{}, false, nil
	case err != nil:
		return reconcile.Result{}, true, &clusterError{err: fmt.Errorf("get the RemediationRequest %s replaces: %w",
			rr.Name, err)}
	}
	to := intent(pred)
	if pred.UID != types.UID(from.UID) || to == nil || to.UID != remediationv1alpha1.UID(rr.UID) ||
		pred.Status.PullRequest == nil {
		return reconcile.Result{}, false, nil
	}
	// Every failure is returned, never dropped: the deadline check comes next, and a replacement whose commit may
	// already be on the pull request must not expire because the repository could not be opened for one look.
	git, err := r.openRecovery(ctx, rr)
	if err != nil {
		return reconcile.Result{}, true, err
	}
	head, err := git.BranchHead(ctx, freshness.BranchName(rr))
	switch {
	case errors.Is(err, providers.ErrNotFound):
		// One 404 for the branch never says there is nothing to recover (#688): the predecessor's pull request
		// decides (changeOf). Merged or closed, its head is the branch's last head, and is judged as the branch
		// head would be; gone, nothing was taken over; open, or unknown, the 404 is a passing fault, retried.
		v := r.changeOf(ctx, git, pred, changeQuery{})
		switch recoverRule[v.state] {
		case recoverRetry:
			if v.err == nil {
				v.err = fmt.Errorf("the branch of the handoff answered 404 while its pull request #%d is open; it "+
					"is read again: %w", v.pr.Number, errConflictingReads)
			}
			return reconcile.Result{}, true, v.err
		case recoverNothing:
			return reconcile.Result{}, false, nil
		}
		head = v.pr.HeadSHA
	case err != nil:
		return reconcile.Result{}, true, freshness.WrapProvider("read the head of the branch", err)
	}
	if head == to.ExpectedHead {
		return reconcile.Result{}, false, nil
	}
	switch ours, err := r.isHandoffCommit(ctx, git, rr, pred, to, head); {
	case err != nil:
		return reconcile.Result{}, true, err
	case !ours:
		// A foreign push: the handoff step after the freshness check ends it.
		return reconcile.Result{}, false, nil
	}
	res, err := r.afterUpdate(ctx, git, rr, pred, head)
	return res, true, err
}

// recoverOutcome is what recoverStep does, per verdict on the predecessor's pull request, when the branch
// answered 404.
type recoverOutcome int

const (
	recoverRetry recoverOutcome = iota
	recoverNothing
	recoverFromPullRequest
)

// recoverRule is recoverStep's rule per verdict after a 404 for the branch: open and unknown are retried (the
// branch of an open pull request exists), gone has nothing to recover, merged and closed are judged by the pull
// request's head. Open on a base a person moved it to (changeElsewhere, #892) is retried like open: its branch
// exists.
var recoverRule = map[changeState]recoverOutcome{
	changeOpen: recoverRetry, changeMerged: recoverFromPullRequest, changeClosed: recoverFromPullRequest,
	changeGone: recoverNothing, changeElsewhere: recoverRetry, changeMergedElsewhere: recoverNothing,
	changeUnknown: recoverRetry,
}

// handoffHolds is the finalizer's look at a request with an outgoing handoff (ADR-0028 section 4 item 8): keep
// is true when its pull request is its replacement's (handed over, or the replacement took it over and has not
// ended this request yet), so it must not be closed; wait is true while the replacement may still take it over.
// When the replacement is gone or ended without taking over, the abort of the intent is stored (abortStandingIntent)
// and the close goes on as for any request: the close reads rr again right before it (stillOwns), and an abort kept in
// memory only would fail that read and start the bound of the cleanup on a failure of this request's own making (the
// close it held back could later be refused past the bound, with the pull request left open; second round of #786).
// A failed write is returned for a retry.
func (r *RemediationRequestReconciler) handoffHolds(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) (
	keep, wait bool, err error) {
	if handedOff(rr) {
		return true, false, nil
	}
	to := intent(rr)
	if to == nil {
		return false, false, nil
	}
	repl := &remediationv1alpha1.RemediationRequest{}
	switch err := r.getLive(ctx, types.NamespacedName{Namespace: rr.Namespace, Name: string(to.Name)}, repl); {
	case apierrors.IsNotFound(err):
	case err != nil:
		return false, false, fmt.Errorf("get the replacement of RemediationRequest %s: %w", rr.Name, err)
	case repl.UID != types.UID(to.UID):
	case tookOver(repl):
		return true, false, nil
	case !ended(repl.Status.Phase) && !abortedOnRefusal(repl):
		return false, true, nil
	}
	if err := r.abortStandingIntent(ctx, rr, "it is being deleted, and its replacement is gone or ended without "+
		"taking the pull request over"); err != nil {
		return false, false, err
	}
	return false, false, nil
}

// settleHandoff is the predecessor's side of a handoff, in upd, the status about to be written with the look at
// its pull request: an intent whose replacement is gone or ended without taking over is aborted, and a Pending
// hint the analysis answered with a replacement is declined once every replacement that names it ended without
// the handoff. A replacement Denied for its chain link (ChainLinkInvalid) is not counted: an invalid link takes
// nothing, not even the hint. A read that fails decides nothing and is logged at Info.
func (r *RemediationRequestReconciler) settleHandoff(ctx context.Context, upd *remediationv1alpha1.RemediationRequest) {
	gaveUp := func(repl *remediationv1alpha1.RemediationRequest) bool {
		return ended(repl.Status.Phase) && !tookOver(repl)
	}
	// A replacement whose handoff was aborted at a refused read (abortedOnRefusal, #786) never takes over: its intent
	// is aborted, though it still runs.
	abandoned := func(repl *remediationv1alpha1.RemediationRequest) bool { return gaveUp(repl) || abortedOnRefusal(repl) }
	reasonOf := func(repl *remediationv1alpha1.RemediationRequest) string {
		if repl != nil && repl.Status.Phase == remediationv1alpha1.PhaseDenied {
			return ReasonHintReplacementDenied
		}
		return ReasonHintHandoffAborted
	}
	if to := intent(upd); to != nil {
		repl := &remediationv1alpha1.RemediationRequest{}
		// Read from the API server: a replacement the cache has not seen yet is not gone, and the abort of the intent
		// lets the predecessor write to Git (decision of 2026-11-19 on #786).
		err := r.getLive(ctx, types.NamespacedName{Namespace: upd.Namespace, Name: string(to.Name)}, repl)
		switch {
		case apierrors.IsNotFound(err):
			abortIntent(upd, ReasonHintHandoffAborted)
		case err != nil:
			// Decides nothing: the intent stands until a later look reads the replacement. A cancelled reconcile
			// is not a failure worth a line.
			if ctx.Err() == nil {
				logf.FromContext(ctx).Info("Could not read the replacement of RemediationRequest",
					"remediationRequest", client.ObjectKeyFromObject(upd), "replacement", to.Name, "error", err.Error())
			}
		case repl.UID != types.UID(to.UID):
			abortIntent(upd, ReasonHintHandoffAborted)
		case abandoned(repl):
			abortIntent(upd, reasonOf(repl))
		}
	}
	if !slices.ContainsFunc(upd.Status.Hints, func(h remediationv1alpha1.Hint) bool {
		return h.State == remediationv1alpha1.HintPending && h.Outcome != ""
	}) {
		return
	}
	list := &remediationv1alpha1.RemediationRequestList{}
	if err := r.List(ctx, list, client.InNamespace(upd.Namespace)); err != nil {
		logf.FromContext(ctx).Info("Could not list the replacements of RemediationRequest",
			"remediationRequest", client.ObjectKeyFromObject(upd), "error", err.Error())
		return
	}
	for i := range upd.Status.Hints {
		h := &upd.Status.Hints[i]
		if h.State != remediationv1alpha1.HintPending || h.Outcome == "" {
			continue
		}
		var alive bool
		var last *remediationv1alpha1.RemediationRequest
		for j := range list.Items {
			repl := &list.Items[j]
			rep := repl.Spec.Replaces
			if rep == nil || types.UID(rep.UID) != upd.UID || rep.Hint != refOf(h) {
				continue
			}
			if c := meta.FindStatusCondition(repl.Status.Conditions, remediationv1alpha1.ConditionPolicyAllowed); c != nil &&
				c.Reason == remediationv1alpha1.ReasonChainLinkInvalid {
				continue
			}
			if !gaveUp(repl) {
				alive = true
				break
			}
			if last == nil || repl.Status.Phase == remediationv1alpha1.PhaseDenied {
				last = repl
			}
		}
		if !alive && last != nil {
			h.State, h.Reason = remediationv1alpha1.HintDeclined, reasonOf(last)
		}
	}
}
