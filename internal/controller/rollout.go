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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/gitops/sourcemap"
	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/verification/rollout"
)

// The check after the merge (#532, design 6 and 7.3): once a request is Merged, the executor asks the delivery
// verifier whether the Flux Kustomization that applies the target reports the merge commit (or a later commit of
// the target branch that holds it) applied, records the time it first sees that (status.pullRequest
// .appliedObservedAt, set once), and then reads the target's rollout status and sets the RolledOut condition:
// True when every pod runs the applied template, False with reason RolloutStuck when the rollout did not
// complete within the deadline measured from that time, with one core Event.
//
// It only reports (invariant 2): it never deletes, restarts or otherwise acts on a pod. It reads the target and
// the Kustomization with the get the executor already holds on them and asks the Git host, a read, whether a
// later applied commit holds the merge commit; it writes only the request's status and one Event (invariant 6).
// No Git write is made, so the kill switch has nothing to gate; the Git provider is opened through
// openPlanRepository, which refuses while writes are paused, so a paused executor also asks the Git host nothing
// and the check waits.
//
// A read that fails, a delivery not yet seen applied and a cancelled reconcile are never a verdict: the request
// keeps its status and is looked at again. The check is bounded in time: a delivery not seen within the window
// is reported (DeliveryNotObserved) and a rollout that never completes stops being read. A target that is not a
// Deployment or StatefulSet gets RolledOut Unknown (KindNotChecked) once, and nothing is read.

// Timing of the check after the merge.
const (
	// deliveryWindow is how long after the merge (the request's move to Merged) the executor waits for the
	// delivery to be seen applied. Past it RolledOut is set False with reason DeliveryNotObserved, one Event is
	// recorded and nothing more is read.
	deliveryWindow = time.Hour
	// stuckWatch is how long after the deadline the executor keeps reading a rollout it reported stuck, so a
	// rollout a person unblocks turns RolledOut True.
	stuckWatch = time.Hour
	// afterMergePollInterval is how often the delivery and a rollout in progress are read.
	afterMergePollInterval = 30 * time.Second
	// stuckPollInterval is how often a rollout reported stuck is read again.
	stuckPollInterval = 2 * time.Minute
)

// DeliveryVerifier says whether a delivery unit applied a merge commit merged into branch.
// The Flux adapter (internal/providers/delivery/flux) implements it.
type DeliveryVerifier interface {
	Verify(ctx context.Context, unit providers.DeliveryUnit, mergeCommit, branch string) (
		providers.DeliveryOutcome, providers.DeliveryStatus, error)
}

// errTargetGone means the target the request names is gone or was replaced (another UID).
var errTargetGone = errors.New("the target is gone or was replaced")

// errNoAncestry means the Git provider of the plan's repository cannot say whether one commit is in the history
// of another.
var errNoAncestry = errors.New("the Git provider cannot check commit ancestry")

// planAncestry is the AncestryChecker of the plan's repository, opened only when the delivery check needs it:
// when the unit applied a later commit than the merge commit.
type planAncestry struct {
	r  *RemediationRequestReconciler
	rr *remediationv1alpha1.RemediationRequest
}

func (a planAncestry) IsAncestor(ctx context.Context, ancestor, commit string) (bool, error) {
	git, err := a.r.openPlanRepository(ctx, a.rr)
	if err != nil {
		return false, err
	}
	checker, ok := git.(providers.AncestryChecker)
	if !ok {
		return false, errNoAncestry
	}
	return checker.IsAncestor(ctx, ancestor, commit)
}

// rolloutTarget returns an empty Deployment or StatefulSet for the request's target, or nil when the target is
// another kind, which has no rollout to check.
func rolloutTarget(ref remediationv1alpha1.ObjectRef) client.Object {
	if ref.APIVersion != appsv1.SchemeGroupVersion.String() {
		return nil
	}
	switch ref.Kind {
	case "Deployment":
		return &appsv1.Deployment{}
	case "StatefulSet":
		return &appsv1.StatefulSet{}
	}
	return nil
}

// afterMerge runs the check after the merge on a Merged request; see the comment at the top of this file. It
// returns when to look again (zero when the check is over or does not apply) and an error only for a failed read
// or status write, which is retried with backoff.
func (r *RemediationRequestReconciler) afterMerge(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) (
	reconcile.Result, error) {
	if r.Delivery == nil || rr.Status.Phase != remediationv1alpha1.PhaseMerged || rr.Status.Source == nil ||
		rr.Status.PullRequest == nil || rr.Status.PullRequest.MergeCommitSHA == "" || rolloutReported(rr) {
		return reconcile.Result{}, nil
	}
	now := r.now()
	if rolloutTarget(rr.Spec.TargetRef) == nil {
		// Said once, so a person does not wait for a report that never comes.
		meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{
			Type: rollout.ConditionRolledOut, Status: metav1.ConditionUnknown, Reason: rollout.ReasonKindNotChecked,
			ObservedGeneration: rr.Generation, LastTransitionTime: metav1.NewTime(now),
			Message: fmt.Sprintf("The rollout check covers Deployments and StatefulSets only; a %s is not checked",
				rr.Spec.TargetRef.Kind)})
		return reconcile.Result{}, r.writeIf(ctx, rr, true)
	}
	deadline := rollout.ClampDeadline(r.RolloutDeadline)

	stepCtx, cancel := bound(ctx, r.CheckTimeout, DefaultCheckTimeout)
	defer cancel()
	recorded := false
	if rr.Status.PullRequest.AppliedObservedAt == nil {
		var (
			wait time.Duration
			err  error
		)
		if recorded, wait, err = r.observeDelivery(ctx, stepCtx, rr, now); err != nil || !recorded {
			return reconcile.Result{RequeueAfter: wait}, err
		}
	}
	appliedAt := rr.Status.PullRequest.AppliedObservedAt.Time
	if !recorded && now.After(appliedAt.Add(deadline+stuckWatch)) {
		return reconcile.Result{}, nil
	}

	// The target is read after the delivery was seen applied, never before: an earlier read could show the old
	// template fully rolled out.
	target := rolloutTarget(rr.Spec.TargetRef)
	readErr := boundErr(ctx, stepCtx, "rollout check", r.readTarget(stepCtx, rr.Spec.TargetRef, target))
	if ctx.Err() != nil {
		// Cancelled: no verdict and no write, not even the applied time; the next reconcile sees the same.
		return reconcile.Result{}, readErr
	}
	if readErr != nil {
		// The applied time, when just seen, is a fact: it is kept, and the rollout is read again.
		return reconcile.Result{}, errors.Join(readErr, r.writeIf(ctx, rr, recorded))
	}
	result, err := rollout.Evaluate(target, appliedAt, now, deadline)
	if err != nil {
		// Not a verdict (fail closed): nothing is reported, the applied time alone is kept.
		logf.FromContext(ctx).Info("The rollout of RemediationRequest cannot be evaluated",
			"remediationRequest", client.ObjectKeyFromObject(rr), "error", err.Error())
		return reconcile.Result{}, r.writeIf(ctx, rr, recorded)
	}
	if err := r.report(ctx, rr, result, now, recorded); err != nil {
		return reconcile.Result{}, err
	}
	switch result.State {
	case rollout.Complete:
		return reconcile.Result{}, nil
	case rollout.Stuck:
		return reconcile.Result{RequeueAfter: stuckPollInterval}, nil
	}
	return reconcile.Result{RequeueAfter: pollAfter(appliedAt.Add(deadline).Sub(now))}, nil
}

// observeDelivery asks whether the merge commit is applied, within the delivery window after the merge. When it
// is, it records now as the applied time in rr (not written yet) and returns true. Otherwise it returns when to
// look again (zero past the window) or the error of the check, bounded by stepCtx.
func (r *RemediationRequestReconciler) observeDelivery(ctx, stepCtx context.Context,
	rr *remediationv1alpha1.RemediationRequest, now time.Time) (bool, time.Duration, error) {
	merged := rr.Status.LastPhaseTransitionTime
	if merged == nil {
		// No time of the merge (a request written before the field existed): no window to measure.
		return false, 0, nil
	}
	if now.After(merged.Add(deliveryWindow)) {
		return false, 0, r.deliveryNotObserved(ctx, stepCtx, rr, now)
	}
	applied, err := r.delivered(stepCtx, rr)
	if err = boundErr(ctx, stepCtx, "delivery check", err); err != nil {
		return false, 0, err
	}
	if !applied {
		return false, pollAfter(merged.Add(deliveryWindow).Sub(now)), nil
	}
	if !recordApplied(rr, now) {
		return false, 0, nil
	}
	logf.FromContext(ctx).Info("Observed the merge commit of RemediationRequest applied",
		"remediationRequest", client.ObjectKeyFromObject(rr), "appliedObservedAt", now.Truncate(time.Second))
	return true, 0, nil
}

// rolloutReported reports whether the check after the merge has ended: RolledOut is True, or says the
// delivery was not observed or the kind is not checked.
func rolloutReported(rr *remediationv1alpha1.RemediationRequest) bool {
	c := meta.FindStatusCondition(rr.Status.Conditions, rollout.ConditionRolledOut)
	return c != nil && (c.Status == metav1.ConditionTrue || c.Reason == rollout.ReasonDeliveryNotObserved ||
		c.Reason == rollout.ReasonKindNotChecked)
}

// deliveryNotObserved ends the check of a request whose delivery was not seen applied within the window: it sets
// RolledOut False with reason DeliveryNotObserved, naming the Kustomization (from the target's labels, read
// again now) and the merge commit, and records one Warning Event. A failed read of the target is returned and
// retried; nothing is reported then.
func (r *RemediationRequestReconciler) deliveryNotObserved(ctx, stepCtx context.Context,
	rr *remediationv1alpha1.RemediationRequest, now time.Time) error {
	target := rolloutTarget(rr.Spec.TargetRef)
	err := boundErr(ctx, stepCtx, "delivery check", r.readTarget(stepCtx, rr.Spec.TargetRef, target))
	unit := "no Flux Kustomization (the target has no valid Kustomization labels)"
	switch {
	case errors.Is(err, errTargetGone) && ctx.Err() == nil:
		// Permanent: reported without the Kustomization, never retried without end.
		unit = "no Flux Kustomization (the target is gone or was replaced)"
	case err != nil:
		return err
	}
	if name, namespace, ok := kustomizationOf(target); ok && err == nil {
		unit = "the Flux Kustomization " + namespace + "/" + name
	}
	message := fmt.Sprintf("The rollout was not checked: %s was not seen applying merge commit %s, or a later "+
		"commit of branch %s that holds it, within %s of the merge. Check the GitOps controller",
		unit, rr.Status.PullRequest.MergeCommitSHA, rr.Status.Source.TargetBranch, deliveryWindow)
	meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{
		Type: rollout.ConditionRolledOut, Status: metav1.ConditionFalse, Reason: rollout.ReasonDeliveryNotObserved,
		ObservedGeneration: rr.Generation, LastTransitionTime: metav1.NewTime(now), Message: message})
	if err := r.writeIf(ctx, rr, true); err != nil {
		return err
	}
	if r.Recorder != nil {
		r.Recorder.Event(rr, corev1.EventTypeWarning, rollout.ReasonDeliveryNotObserved, message)
	}
	logf.FromContext(ctx).Info("Reported the delivery of RemediationRequest not observed",
		"remediationRequest", client.ObjectKeyFromObject(rr))
	return nil
}

// kustomizationOf returns the Kustomization the target's Flux labels name, when both are valid object names.
func kustomizationOf(target client.Object) (name, namespace string, ok bool) {
	labels := target.GetLabels()
	name, namespace = labels[sourcemap.LabelKustomizationName], labels[sourcemap.LabelKustomizationNamespace]
	if len(validation.IsDNS1123Subdomain(name)) > 0 || len(validation.IsDNS1123Label(namespace)) > 0 {
		return "", "", false
	}
	return name, namespace, true
}

// report writes the RolledOut condition when its status or reason changes, or the status when the applied time
// was just recorded, and records one Event on the write that first reports the rollout stuck.
func (r *RemediationRequestReconciler) report(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	result rollout.Result, now time.Time, recorded bool) error {
	previous := meta.FindStatusCondition(rr.Status.Conditions, rollout.ConditionRolledOut)
	cond := result.Condition(rr.Generation, now)
	changed := previous == nil || previous.Status != cond.Status || previous.Reason != cond.Reason
	if changed {
		meta.SetStatusCondition(&rr.Status.Conditions, cond)
	}
	if err := r.writeIf(ctx, rr, changed || recorded); err != nil {
		return err
	}
	if !changed || result.State == rollout.Progressing {
		return nil
	}
	log := logf.FromContext(ctx).WithValues("remediationRequest", client.ObjectKeyFromObject(rr))
	if result.State == rollout.Complete {
		log.Info("Observed the rollout of RemediationRequest complete")
		return nil
	}
	if r.Recorder != nil {
		r.Recorder.Event(rr, corev1.EventTypeWarning, rollout.ReasonRolloutStuck, result.Message)
	}
	log.Info("Reported the rollout of RemediationRequest stuck", "oldPods", result.OldPods)
	return nil
}

// writeIf writes the request's status when write is true.
func (r *RemediationRequestReconciler) writeIf(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	write bool) error {
	if !write {
		return nil
	}
	if err := r.Status().Update(ctx, rr); err != nil {
		return fmt.Errorf("update status of RemediationRequest %s/%s: %w", rr.Namespace, rr.Name, err)
	}
	return nil
}

// delivered reports whether the Flux Kustomization named by the target's labels applied the merge commit, or a
// later commit of the target branch that holds it. Every outcome but Applied is false: Pending, Failed (Flux
// may still retry; the phases of #94 report it), a target without Kustomization labels. A read that fails is an
// error.
func (r *RemediationRequestReconciler) delivered(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) (
	bool, error) {
	ref := rr.Spec.TargetRef
	target := rolloutTarget(ref)
	if err := r.readTarget(ctx, ref, target); err != nil {
		return false, err
	}
	name, namespace, ok := kustomizationOf(target)
	if !ok {
		return false, nil
	}
	verifier, err := r.Delivery(planAncestry{r: r, rr: rr})
	if err != nil {
		return false, fmt.Errorf("build the delivery verifier: %w", err)
	}
	unit := providers.DeliveryUnit{Kind: "Kustomization", Namespace: namespace, Name: name}
	outcome, _, err := verifier.Verify(ctx, unit, rr.Status.PullRequest.MergeCommitSHA, string(rr.Status.Source.TargetBranch))
	if err != nil {
		return false, fmt.Errorf("verify the delivery of the merge commit: %w", err)
	}
	return outcome == providers.DeliveryApplied, nil
}

// readTarget reads the target into obj from the API server and checks that it is the object the request names
// (its UID, when the request names one). A missing or replaced target is an error, retried, never a verdict.
func (r *RemediationRequestReconciler) readTarget(ctx context.Context, ref remediationv1alpha1.ObjectRef,
	obj client.Object) error {
	err := r.reader().Get(ctx, types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}, obj)
	switch {
	case apierrors.IsNotFound(err):
		return fmt.Errorf("get %s %s/%s: %w", ref.Kind, ref.Namespace, ref.Name, errTargetGone)
	case err != nil:
		return fmt.Errorf("get %s %s/%s: %w", ref.Kind, ref.Namespace, ref.Name, err)
	case ref.UID != "" && string(obj.GetUID()) != string(ref.UID):
		return fmt.Errorf("get %s %s/%s: %w (another UID)", ref.Kind, ref.Namespace, ref.Name, errTargetGone)
	}
	return nil
}

// pollAfter returns the poll interval, or left when that is shorter, and never less than a second.
func pollAfter(left time.Duration) time.Duration {
	return max(min(afterMergePollInterval, left), time.Second)
}

// earlier returns the result that looks again sooner: a zero RequeueAfter means not again.
func earlier(a, b reconcile.Result) reconcile.Result {
	switch {
	case a.RequeueAfter <= 0:
		return b
	case b.RequeueAfter <= 0:
		return a
	case b.RequeueAfter < a.RequeueAfter:
		return b
	}
	return a
}
