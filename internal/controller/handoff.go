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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
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
//     says, has no handoff of its own yet, and holds the named hint Pending. A link that fails is Denied
//     (ChainLinkInvalid) and takes nothing over. The limits then count the predecessor neither as a holder of
//     the target lock nor as an active request (checkLimits), so the replacement takes its lock and slot; the
//     admission's status write sets holdsTarget and status.handoff.from (Reserved) together (step 1).
//   - Step 2 (reserve): one status write on the predecessor sets status.handoff.to (Intent, the branch head it
//     expects) and its hint Reserved, with the resourceVersion read, so only a successor that read the hint
//     Pending reserves it. A conflict is read again; a predecessor that left AwaitingApproval, or whose hint is
//     no longer Pending or is reserved for another, ends the replacement Cancelled (HandoffRefused).
//   - Step 3 (takeOver): after the freshness check, the render check and the branch rule (#524): the branch head
//     is the expected head and carries only the predecessor's chain of changes to the target file, which holds
//     exactly the predecessor's plan on the target file now, and the new plan renders as planned. The pull
//     request is read (it must be open), the branch moves by one commit on top of the expected head
//     (AppendCommit, a compare-and-swap), and the pull request is read again. A head that is not the expected
//     one is accepted only when it is that commit: its one parent is the expected head, the chain holds only
//     changes to the target file and the file is the new plan (recomputed). Anything else, a merge or a close
//     around the update with the old head, or a plan that would change another file, ends the replacement
//     Cancelled (HandoffAborted), and the predecessor stays as it is.
//   - Step 4 (recordHandoff): the replacement records the pull request with the new head, the hints of the chain
//     (each consumed one, the one it answers among them) and the predecessor's comment cursor, sets from Done and
//     moves to PRCreated; a merge right after the update whose merged head is the new commit makes it Merged.
//   - Step 5 (followHandoff): the predecessor ends Superseded with reason HandedOff, its to Done, its hint
//     Consumed and holdsTarget false. It is the one Superseded whose pull request may be open (prMayBeOpen).
//   - Step 6 (followHandoff): while the pull request waits, its title and body are rewritten for the new plan
//     with the replacement's marker, and the check run shadewright/review is created in progress on the new
//     head, each recorded in status.handoff.from once done.
//
// Each step is one write, idempotent, and a restart finds where it stopped from the two statuses and the
// branch. Every write, in Git and in status, passes the kill switch first (writeAllowed, gatedGit); while
// writes are paused the handoff waits at its recorded step. The predecessor's side (settleHandoff, called with
// its look at the pull request) aborts an intent whose replacement is gone or ended without taking over, and
// declines a Pending hint whose replacements all ended without the handoff (ReplacementDenied when one was
// Denied, else HandoffAborted); a request that ends for any other reason aborts its intent in the write that
// ends it (declineOnEnd). While its intent stands, the predecessor reads no new comments, and its watch takes a
// head that is the replacement's commit (expectedHandoffHead) for the expected one instead of HeadMoved.

// Hint reasons of the predecessor's side of a handoff.
const (
	// ReasonHintReplacementDenied: the replacement that answered the hint was Denied by policy, so the pull
	// request keeps the old plan.
	ReasonHintReplacementDenied = "ReplacementDenied"
	// ReasonHintHandoffAborted: the replacement that answered the hint ended without taking the pull request
	// over.
	ReasonHintHandoffAborted = "HandoffAborted"
)

// The check run created on the head a handoff pushed (ADR-0028 section 5). Completing it on an approval is the
// merge gate's work; until then it says why it waits.
const (
	checkRunTitle   = "Waiting for an approval of this head"
	checkRunSummary = "Shadewright changed this pull request after a reviewer hint. The check completes once someone " +
		"who wrote no hint in this pull request approves its latest head; an approval of an earlier head does not " +
		"count."
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
// section 4 item 2), before anything else is read about it. It returns the refusal's message, "" when the link
// holds or rr replaces nothing, and an error only for a read worth retrying.
func (r *RemediationRequestReconciler) chainLink(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) (
	string, error) {
	rep := rr.Spec.Replaces
	if rep == nil {
		return "", nil
	}
	pred := &remediationv1alpha1.RemediationRequest{}
	switch err := r.getLive(ctx, types.NamespacedName{Namespace: rr.Namespace, Name: string(rep.Name)}, pred); {
	case apierrors.IsNotFound(err):
		return "The request it replaces does not exist", nil
	case err != nil:
		return "", fmt.Errorf("get the RemediationRequest %s replaces: %w", rr.Name, err)
	}
	pr := pred.Status.PullRequest
	opened := meta.FindStatusCondition(pred.Status.Conditions, remediationv1alpha1.ConditionPROpened)
	switch {
	case pred.UID != types.UID(rep.UID):
		return "The request it replaces has another UID", nil
	case !pred.DeletionTimestamp.IsZero():
		return "The request it replaces is being deleted", nil
	case pred.Spec.IncidentRef != rr.Spec.IncidentRef:
		return "The request it replaces names another Incident", nil
	case pred.Spec.TargetRef != rr.Spec.TargetRef:
		return "The request it replaces names another target", nil
	case chainRootOf(pred) != rr.Spec.ChainRoot:
		return "The request it replaces belongs to another chain", nil
	case pred.Status.Phase != remediationv1alpha1.PhaseAwaitingApproval || pr == nil || opened == nil ||
		opened.Reason != ReasonPullRequestOpened:
		return "The request it replaces does not wait for review with an open pull request", nil
	case pred.Status.Handoff != nil && pred.Status.Handoff.To != nil:
		return "The request it replaces is already handed over", nil
	}
	if h := hintByRef(pred, rep.Hint); h == nil || h.State != remediationv1alpha1.HintPending {
		return "The request it replaces does not hold the named hint Pending", nil
	}
	return "", nil
}

// handoffStep does the next step of a replacement admitted with status.handoff.from Reserved, one write per
// call: the predecessor's intent (step 2), or the branch update and the record of the pull request (steps 3
// and 4). recheck has checked the plan's freshness; fresh is what that check read.
func (r *RemediationRequestReconciler) handoffStep(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	fresh freshness.Result) (reconcile.Result, error) {
	from := rr.Status.Handoff.From
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
	case to == nil:
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
	}
	return r.takeOver(ctx, rr, pred, to, fresh)
}

// reserve is step 2: the predecessor's intent and its hint Reserved, in one status write under optimistic
// concurrency.
func (r *RemediationRequestReconciler) reserve(ctx context.Context, rr, pred *remediationv1alpha1.RemediationRequest) (
	reconcile.Result, error) {
	from := rr.Status.Handoff.From
	h := hintByRef(pred, from.Hint)
	if pred.Status.Phase != remediationv1alpha1.PhaseAwaitingApproval || pred.Status.PullRequest == nil ||
		!pred.DeletionTimestamp.IsZero() || h == nil || h.State != remediationv1alpha1.HintPending {
		return reconcile.Result{}, r.endHandoff(ctx, rr, remediationv1alpha1.ReasonHandoffRefused,
			"The request it replaces no longer waits for review, or its hint is no longer Pending; nothing was "+
				"taken over")
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
	cr, err := git.FindChangeRequest(ctx, branch)
	switch {
	case errors.Is(err, providers.ErrNotFound):
		return reconcile.Result{}, r.endHandoff(ctx, rr, remediationv1alpha1.ReasonHandoffAborted,
			"The pull request of the request it replaces cannot be found; nothing was taken over")
	case err != nil:
		return reconcile.Result{}, freshness.WrapProvider("find the change request", err)
	case cr.Number != predPR.Number:
		return reconcile.Result{}, r.endHandoff(ctx, rr, remediationv1alpha1.ReasonHandoffAborted,
			"The branch's pull request is not the one the request it replaces recorded; nothing was taken over")
	}
	head, err := git.BranchHead(ctx, branch)
	if err != nil {
		return reconcile.Result{}, freshness.WrapProvider("read the head of the branch", err)
	}
	if head != to.ExpectedHead {
		// The update may have happened before a restart, a lost answer or a lost status write: only the
		// replacement's own commit goes on to the read after the update.
		ours, err := r.isHandoffCommit(ctx, git, pred, to, head, changed)
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
	if err := r.checkRender(ctx, git, fresh.Head, changed, rr); err != nil {
		if ref, ok := errors.AsType[*renderRefusal](err); ok {
			return ref, nil
		}
		return nil, err
	}
	return nil, nil
}

// isHandoffCommit reports whether head, a branch head that is not the expected one, is the commit of this
// handoff: its one parent is the expected head, the chain below it changes only the target file, and the file
// is the new plan, changed, recomputed from the target branch now.
func (r *RemediationRequestReconciler) isHandoffCommit(ctx context.Context, git providers.GitProvider,
	pred *remediationv1alpha1.RemediationRequest, to *remediationv1alpha1.OutgoingHandoff, head string,
	changed []byte) (bool, error) {
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
	case bs != providers.BranchOneChange:
		return false, nil
	}
	file, err := git.ReadFile(ctx, head, path, providers.MaxFileBytes)
	if err != nil {
		return false, freshness.WrapProvider("read the file of the pull request", err)
	}
	return bytes.Equal(file.Data, changed), nil
}

// expectedHandoffHead reports whether head, the moved head of the recorded pull request of a predecessor whose
// intent stands, is the replacement's commit as far as the predecessor can tell (ADR-0028 section 4 item 8):
// one commit on top of the head it recorded that changes only the target file. Any other head is HeadMoved as
// before.
func (r *RemediationRequestReconciler) expectedHandoffHead(ctx context.Context, git providers.GitProvider,
	rr *remediationv1alpha1.RemediationRequest, head string) (bool, error) {
	to := intent(rr)
	if to == nil || to.ExpectedHead != rr.Status.PullRequest.HeadSHA || rr.Status.Source == nil {
		return false, nil
	}
	parents, err := git.Parents(ctx, head)
	if err != nil {
		return false, freshness.WrapProvider("read the parents of the pull request's head", err)
	}
	if len(parents) != 1 || parents[0] != to.ExpectedHead {
		return false, nil
	}
	bs, err := branchStateOf(ctx, git, string(rr.Status.Source.Path), head,
		min(chainDepth(rr)+1, providers.MaxChainCommits))
	return err == nil && bs == providers.BranchOneChange, err
}

// afterUpdate reads the pull request after the branch update (ADR-0028 section 4 item 5): open, the replacement
// records it (step 4); merged with the new commit as its head, the replacement owns the merge; merged with
// another head, or closed, the handoff is abandoned.
func (r *RemediationRequestReconciler) afterUpdate(ctx context.Context, git providers.GitProvider,
	rr, pred *remediationv1alpha1.RemediationRequest, newHead string) (reconcile.Result, error) {
	cr, err := git.FindChangeRequest(ctx, freshness.BranchName(rr))
	switch {
	case err != nil:
		return reconcile.Result{}, freshness.WrapProvider("find the change request after the update", err)
	case cr.Number != pred.Status.PullRequest.Number:
		return reconcile.Result{}, r.endHandoff(ctx, rr, remediationv1alpha1.ReasonHandoffAborted,
			"The branch's pull request is not the one the request it replaces recorded; nothing was taken over")
	}
	switch {
	case cr.State == providers.ChangeRequestOpen,
		cr.State == providers.ChangeRequestMerged && cr.HeadSHA == newHead:
		return reconcile.Result{RequeueAfter: stepDelay}, r.recordHandoff(ctx, rr, pred, cr, newHead)
	}
	return reconcile.Result{}, r.endHandoff(ctx, rr, remediationv1alpha1.ReasonHandoffAborted,
		"The pull request was "+closedOrMerged(cr)+" around the handoff with the old plan; nothing was taken over")
}

func closedOrMerged(cr providers.ChangeRequest) string {
	if cr.State == providers.ChangeRequestMerged {
		return "merged"
	}
	return "closed"
}

// recordHandoff is step 4: the replacement records the pull request with the new head, carries the consumed
// hints of the chain and the predecessor's comment cursor, sets from Done and moves to PRCreated, or ends Merged
// when the merge took the new commit.
func (r *RemediationRequestReconciler) recordHandoff(ctx context.Context, rr, pred *remediationv1alpha1.RemediationRequest,
	cr providers.ChangeRequest, newHead string) error {
	if err := r.writeAllowed(ctx); err != nil {
		return err
	}
	from := rr.Status.Handoff.From
	from.Phase = remediationv1alpha1.HandoffDone
	for _, h := range pred.Status.Hints {
		consumed := h.State == remediationv1alpha1.HintConsumed
		answered := refOf(&h) == from.Hint
		if (!consumed && !answered) || findHint(rr.Status.Hints, h.Source, h.CommentID) != nil {
			continue
		}
		h.State, h.Reason = remediationv1alpha1.HintConsumed, ""
		rr.Status.Hints = append(rr.Status.Hints, h)
	}
	if rr.Status.CommentCursor == nil && pred.Status.CommentCursor != nil {
		rr.Status.CommentCursor = pred.Status.CommentCursor.DeepCopy()
	}
	rr.Status.PullRequest = &remediationv1alpha1.PullRequestStatus{Number: cr.Number, URL: cr.URL, HeadSHA: newHead}
	rr.Status.Deadline = nil
	meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{
		Type:               remediationv1alpha1.ConditionPROpened,
		Status:             metav1.ConditionTrue,
		Reason:             ReasonPullRequestOpened,
		Message:            fmt.Sprintf("Pull request #%d is open; this request took it over from the request it replaces", cr.Number),
		ObservedGeneration: rr.Generation,
	})
	if cr.State == providers.ChangeRequestMerged {
		recordMerge(rr, cr)
		return r.ignoreStopped(r.stop(ctx, rr, remediationv1alpha1.PhaseMerged, ReasonChangeRequestMerged,
			"The pull request was merged with this request's change, right after the handoff"))
	}
	r.setPhase(rr, remediationv1alpha1.PhasePRCreated)
	if err := r.Status().Update(ctx, rr); err != nil {
		return fmt.Errorf("update status of RemediationRequest %s/%s: %w", rr.Namespace, rr.Name, err)
	}
	logf.FromContext(ctx).Info("Took over the pull request for RemediationRequest", "remediationRequest",
		client.ObjectKeyFromObject(rr), "pullRequest", cr.Number, "replaces", pred.Name)
	return nil
}

// endHandoff ends a replacement whose handoff did not happen: Cancelled with reason, from Aborted. It holds no
// pull request of its own, so it releases its lock in the same write; the predecessor keeps everything it had,
// and aborts its own intent when it sees this one ended (settleHandoff).
func (r *RemediationRequestReconciler) endHandoff(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	reason, message string) error {
	if err := r.writeAllowed(ctx); err != nil {
		return err
	}
	if from := rr.Status.Handoff.From; from.Phase == remediationv1alpha1.HandoffReserved {
		from.Phase = remediationv1alpha1.HandoffAborted
	}
	rr.Status.HoldsTarget = new(false)
	return r.stop(ctx, rr, remediationv1alpha1.PhaseCancelled, reason, message)
}

// followHandoff is step 6 of a replacement that took the pull request over, each part once and recorded: while
// the pull request waits for review, its title and body are rewritten and the check run is created on the
// recorded head. A final refusal of the Git host is logged and recorded as done, so it is not asked again on
// every look; a closed or merged pull request leaves nothing to do. An error is returned for a retry; rr is
// updated in place by every status write that went through. The caller has done step 5 first.
func (r *RemediationRequestReconciler) followHandoff(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) error {
	from := rr.Status.Handoff.From
	if p := rr.Status.Phase; (p != remediationv1alpha1.PhasePRCreated && p != remediationv1alpha1.PhaseAwaitingApproval) ||
		rr.Status.PullRequest == nil || (from.PullRequestUpdated && from.CheckRunCreated) {
		return nil
	}
	stepCtx, cancel := bound(ctx, r.WriteTimeout, DefaultWriteTimeout)
	defer cancel()
	git, err := r.openPlanRepository(stepCtx, rr)
	if err != nil {
		return err
	}
	log := logf.FromContext(ctx).WithValues("remediationRequest", client.ObjectKeyFromObject(rr))
	pr := rr.Status.PullRequest
	done := func(err error, what string) (bool, error) {
		switch {
		case err == nil:
			return true, nil
		case providers.IsConflict(err):
			// Closed or merged meanwhile: the watch of the pull request decides; nothing is left to rewrite.
			return true, nil
		}
		if reason, _, final := refusal(err); final {
			log.Info("The Git host refused a step after the handoff of RemediationRequest; it is not asked again",
				"step", what, "reason", reason)
			return true, nil
		}
		return false, err
	}
	if !from.PullRequestUpdated {
		err := git.UpdateChangeRequest(stepCtx, pr.Number, commitTitle(rr), pullRequestBody(rr, r.verifiedIncidentName(stepCtx, rr)))
		if ok, err := done(err, "rewrite the pull request"); !ok {
			return freshness.WrapProvider("rewrite the pull request", err)
		}
		if err := r.recordStep(stepCtx, rr, func(f *remediationv1alpha1.IncomingHandoff) { f.PullRequestUpdated = true }); err != nil {
			return err
		}
	}
	if !rr.Status.Handoff.From.CheckRunCreated {
		err := git.CreateCheckRun(stepCtx, pr.HeadSHA, providers.CheckRun{Name: providers.CheckRunName,
			Status: providers.CheckRunInProgress, Title: checkRunTitle, Summary: checkRunSummary})
		if ok, err := done(err, "create the check run"); !ok {
			return freshness.WrapProvider("create the check run", err)
		}
		if err := r.recordStep(stepCtx, rr, func(f *remediationv1alpha1.IncomingHandoff) { f.CheckRunCreated = true }); err != nil {
			return err
		}
	}
	return nil
}

// recordStep records a step after the handoff in rr's status, through the kill switch.
func (r *RemediationRequestReconciler) recordStep(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	set func(*remediationv1alpha1.IncomingHandoff)) error {
	if err := r.writeAllowed(ctx); err != nil {
		return err
	}
	set(rr.Status.Handoff.From)
	if err := r.Status().Update(ctx, rr); err != nil {
		return fmt.Errorf("update status of RemediationRequest %s/%s: %w", rr.Namespace, rr.Name, err)
	}
	return nil
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
// pull request is rewritten while it waits, only logged on failure and retried with the next look. A pause
// leaves both for later.
func (r *RemediationRequestReconciler) afterHandoff(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) error {
	if !tookOver(rr) {
		return nil
	}
	if err := r.supersedePredecessor(ctx, rr); err != nil && !errors.Is(err, freshness.ErrPaused) {
		return err
	}
	if err := r.followHandoff(ctx, rr); err != nil {
		logf.FromContext(ctx).Info("Could not finish the steps after the handoff of RemediationRequest",
			"remediationRequest", client.ObjectKeyFromObject(rr), "error", err.Error())
	}
	return nil
}

// supersedePredecessor is step 5: the predecessor whose intent names rr ends Superseded with reason HandedOff,
// its to Done, its hint Consumed and its lock released, in one status write. A predecessor that is gone, or
// already handed over, needs nothing. It reads the cache, which costs nothing on every later reconcile; a copy
// older than the predecessor makes the write a conflict, retried.
func (r *RemediationRequestReconciler) supersedePredecessor(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) error {
	from := rr.Status.Handoff.From
	pred := &remediationv1alpha1.RemediationRequest{}
	switch err := r.Get(ctx, types.NamespacedName{Namespace: rr.Namespace, Name: string(from.Name)}, pred); {
	case apierrors.IsNotFound(err):
		return nil
	case err != nil:
		return fmt.Errorf("get the RemediationRequest %s replaces: %w", rr.Name, err)
	}
	to := intent(pred)
	if pred.UID != types.UID(from.UID) || to == nil || to.UID != remediationv1alpha1.UID(rr.UID) {
		return nil
	}
	if err := r.writeAllowed(ctx); err != nil {
		return err
	}
	to.Phase = remediationv1alpha1.HandoffDone
	if h := hintByRef(pred, to.Hint); h != nil {
		h.State = remediationv1alpha1.HintConsumed
	}
	pred.Status.HoldsTarget = new(false)
	err := r.stop(ctx, pred, remediationv1alpha1.PhaseSuperseded, remediationv1alpha1.ReasonHandedOff,
		fmt.Sprintf("RemediationRequest %s took over the pull request and its branch with a new plan", rr.Name))
	if !errors.Is(err, errStopped) {
		return err
	}
	return nil
}

// settleHandoff is the predecessor's side of a handoff, in upd, the status about to be written with the look at
// its pull request: an intent whose replacement is gone or ended without taking over is aborted, and a Pending
// hint the analysis answered with a replacement is declined once every replacement that names it ended without
// the handoff. A replacement Denied for its chain link (ChainLinkInvalid) is not counted: an invalid link takes
// nothing, not even the hint. A read that fails decides nothing.
func (r *RemediationRequestReconciler) settleHandoff(ctx context.Context, upd *remediationv1alpha1.RemediationRequest) {
	gaveUp := func(repl *remediationv1alpha1.RemediationRequest) bool {
		return ended(repl.Status.Phase) && !tookOver(repl)
	}
	reasonOf := func(repl *remediationv1alpha1.RemediationRequest) string {
		if repl != nil && repl.Status.Phase == remediationv1alpha1.PhaseDenied {
			return ReasonHintReplacementDenied
		}
		return ReasonHintHandoffAborted
	}
	if to := intent(upd); to != nil {
		repl := &remediationv1alpha1.RemediationRequest{}
		err := r.Get(ctx, types.NamespacedName{Namespace: upd.Namespace, Name: string(to.Name)}, repl)
		switch {
		case apierrors.IsNotFound(err):
			abortIntent(upd, ReasonHintHandoffAborted)
		case err != nil:
		case repl.UID != types.UID(to.UID):
			abortIntent(upd, ReasonHintHandoffAborted)
		case gaveUp(repl):
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
