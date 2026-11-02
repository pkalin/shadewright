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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/gitops/sourcemap"
	"github.com/pkalin/shadewright/internal/policy"
	"github.com/pkalin/shadewright/internal/policy/freshness"
	"github.com/pkalin/shadewright/internal/providers"
)

// settingsName is the name of the one RemediationSettings object.
const settingsName = "default"

// Reasons the controller itself gives for a refusal, next to the reasons of package policy and sourcemap.
const (
	// ReasonPolicyNotFound: the policy the request names does not exist (Denied).
	ReasonPolicyNotFound = "PolicyNotFound"
	// ReasonSettingsNotFound: the RemediationSettings object does not exist (Denied).
	ReasonSettingsNotFound = "SettingsNotFound"
	// ReasonTargetMismatch: the live target has another UID than the request names (Denied).
	ReasonTargetMismatch = "TargetMismatch"
	// ReasonPreviousValueMismatch means the previous value the request claims is not the live value, so
	// the policy's increase and decrease limits would be measured from a wrong baseline.
	ReasonPreviousValueMismatch = "PreviousValueMismatch"
)

// SourceResolver maps the target to its file in Git (ADR-0012). [sourcemap.Resolver] implements it.
type SourceResolver interface {
	Resolve(ctx context.Context, in sourcemap.Input) (sourcemap.Result, error)
}

// RemediationRequestReconciler reconciles a RemediationRequest object. It runs the policy check phase: a
// Pending request becomes PolicyChecked, Denied or NeedsIntervention, and later phases are left to other steps.
type RemediationRequestReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Source resolves the target's declaration in Git.
	Source SourceResolver
	// APIReader reads straight from the API server (mgr.GetAPIReader()). The live target is read through it,
	// so the executor needs only get on target kinds and never starts an informer that would need list and
	// watch; and the policy and settings are read through it, so a decision never rests on a stale cached
	// copy. Nil means the client is used (tests).
	APIReader client.Reader
	// OpenRepository opens the Git repository a settings entry names. It is used to re-check a plan before a
	// write and to close the change request of a superseded one. Nil disables the freshness step of Reconcile
	// (no Git adapter yet); [RemediationRequestReconciler.EnsureFresh] then fails closed.
	OpenRepository freshness.RepositoryOpener
}

// ErrSuperseded is returned by EnsureFresh when the plan went stale: the request is now Superseded and its
// change request is closed. The caller must not write.
var ErrSuperseded = errors.New("the plan is superseded")

// freshRecheckDelay is how long a request waits to be re-checked when writes are paused.
const freshRecheckDelay = time.Minute

// reader returns the uncached reader, or the client when none is set.
func (r *RemediationRequestReconciler) reader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// getLive reads a cluster-scoped authorizing object (the policy or the settings) straight from the API server.
// The cache is not used: a cached copy can still hold a deleted policy, an older approval mode or a repository
// removed from the settings, and the request's resourceVersion does not change when either of them does, so a
// stale copy would authorize a request the live configuration refuses. A policy created together with the
// request is found as well.
func (r *RemediationRequestReconciler) getLive(ctx context.Context, key types.NamespacedName, obj client.Object) error {
	return r.reader().Get(ctx, key, obj)
}

// Reconcile runs the policy check on a Pending request (design section 6, step 1 to 6).
//
// It is idempotent (invariant 5): only a request that has no phase or is Pending is looked at, the outcome is
// written once through the status subresource with the resourceVersion that was read, and a request that
// already has a phase is left untouched. A stale cache read therefore ends in a conflict, never in a second
// decision. Only deterministic code decides (invariant 1): the request's own fields never lower the policy.
//
// A missing policy or settings object and a target with another UID end the request as Denied at once, so a
// permanent refusal never becomes a retry loop. Only errors worth retrying (an API or Git host failure) are
// returned.
func (r *RemediationRequestReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	rr := &remediationv1alpha1.RemediationRequest{}
	if err := r.Get(ctx, req.NamespacedName, rr); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if rr.Status.Phase != "" && rr.Status.Phase != remediationv1alpha1.PhasePending {
		return r.recheck(ctx, rr)
	}

	out, err := r.check(ctx, rr)
	if err != nil {
		// A remote system that asked for a delay (Retry-After) gets it; controller-runtime's error backoff
		// does not read it and could retry sooner.
		if te, ok := errors.AsType[*sourcemap.TransientError](err); ok && te.RetryAfter > 0 {
			log.Info("Waiting before retrying RemediationRequest", "remediationRequest", req.NamespacedName,
				"error", err.Error(), "retryAfter", te.RetryAfter)
			return ctrl.Result{RequeueAfter: te.RetryAfter}, nil
		}
		return ctrl.Result{}, err
	}

	rr.Status.ObservedGeneration = rr.Generation
	rr.Status.Phase = out.phase
	rr.Status.Source = out.source
	rr.Status.PolicyUID = out.policyUID
	rr.Status.PolicyRevision = out.policyRevision
	rr.Status.PlanDigest = out.planDigest
	rr.Status.EffectiveApprovalRequired = out.approval
	meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{
		Type:               remediationv1alpha1.ConditionPolicyAllowed,
		Status:             out.status,
		Reason:             out.reason,
		Message:            out.message,
		ObservedGeneration: rr.Generation,
	})
	if err := r.Status().Update(ctx, rr); err != nil {
		return ctrl.Result{}, fmt.Errorf("update status of RemediationRequest %s: %w", req.NamespacedName, err)
	}
	log.Info("Checked RemediationRequest against policy", "remediationRequest", req.NamespacedName,
		"phase", out.phase, "reason", out.reason)
	return ctrl.Result{}, nil
}

// outcome is what the policy check writes to the status.
type outcome struct {
	phase          remediationv1alpha1.RemediationPhase
	status         metav1.ConditionStatus
	reason         string
	message        string
	source         *remediationv1alpha1.ResolvedSource
	policyUID      remediationv1alpha1.UID
	policyRevision string
	approval       *bool
	planDigest     string
}

func denied(reason, message string) outcome {
	return outcome{phase: remediationv1alpha1.PhaseDenied, status: metav1.ConditionFalse, reason: reason, message: message}
}

func needsIntervention(reason, message string) outcome {
	return outcome{phase: remediationv1alpha1.PhaseNeedsIntervention, status: metav1.ConditionUnknown,
		reason: reason, message: message}
}

// check gathers the input and decides. The error is non-nil only for a failure worth retrying.
func (r *RemediationRequestReconciler) check(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) (
	outcome, error) {
	pol := &remediationv1alpha1.RemediationPolicy{}
	switch err := r.getLive(ctx, types.NamespacedName{Name: string(rr.Spec.PolicyRef)}, pol); {
	case apierrors.IsNotFound(err):
		return denied(ReasonPolicyNotFound, fmt.Sprintf("Policy %q does not exist", rr.Spec.PolicyRef)), nil
	case err != nil:
		return outcome{}, fmt.Errorf("get RemediationPolicy %s: %w", rr.Spec.PolicyRef, err)
	}
	settings := &remediationv1alpha1.RemediationSettings{}
	switch err := r.getLive(ctx, types.NamespacedName{Name: settingsName}, settings); {
	case apierrors.IsNotFound(err):
		return denied(ReasonSettingsNotFound, "RemediationSettings default does not exist"), nil
	case err != nil:
		return outcome{}, fmt.Errorf("get RemediationSettings %s: %w", settingsName, err)
	}

	ref := rr.Spec.TargetRef
	// Refuse kinds the action may not change before reading anything, so the executor never reads, for
	// example, a Secret that a request names.
	field, err := sourcemap.FieldFor(rr.Spec.Action, ref)
	if err != nil {
		return needsIntervention(string(sourcemap.ReasonInvalidInput), "The action does not fit the target"), nil
	}
	live, ok, err := r.target(ctx, ref)
	if err != nil {
		return outcome{}, err
	}
	if !ok {
		return needsIntervention(string(sourcemap.ReasonTargetNotFound),
			fmt.Sprintf("%s %s/%s does not exist", ref.Kind, ref.Namespace, ref.Name)), nil
	}
	if live.GetUID() != types.UID(ref.UID) {
		return denied(ReasonTargetMismatch,
			fmt.Sprintf("%s %s/%s has another UID than the request names", ref.Kind, ref.Namespace, ref.Name)), nil
	}
	if o, stop := checkBaseline(rr.Spec.Action, field, live); stop {
		return o, nil
	}

	res, err := r.Source.Resolve(ctx, sourcemap.Input{
		Target:       ref,
		Action:       rr.Spec.Action,
		Repositories: settings.Spec.Repositories,
		GitOps:       pol.Spec.GitOps,
	})
	if err != nil {
		// The error is sanitised by the resolver: no provider text, no URL.
		return outcome{}, fmt.Errorf("resolve source of %s %s/%s: %w", ref.Kind, ref.Namespace, ref.Name, err)
	}
	switch res.Outcome {
	case sourcemap.Resolved:
		if res.Source == nil {
			return needsIntervention(string(sourcemap.ReasonInvalidInput), "The source mapping returned no source"), nil
		}
	case sourcemap.Denied:
		return denied(string(res.Reason), res.Message), nil
	default:
		// NeedsIntervention, and anything unknown: a person must look. Never Denied, never retried.
		return needsIntervention(string(res.Reason), res.Message), nil
	}

	// The baseline was checked on the first read, and the resolver read the target again to compare it with
	// Git. If the target changed in between (Flux applied a new revision), the baseline and the resolved source
	// describe different objects, and a change the policy refuses could pass. Read once more and start over
	// when the object is not the one checked: an unchanged resourceVersion means nothing changed in between.
	again, ok, err := r.target(ctx, ref)
	if err != nil {
		return outcome{}, err
	}
	if !ok || again.GetUID() != live.GetUID() || again.GetResourceVersion() != live.GetResourceVersion() {
		return outcome{}, fmt.Errorf("%s %s/%s changed during the policy check, checking again",
			ref.Kind, ref.Namespace, ref.Name)
	}

	d := policy.Decide(policy.DecisionInput{
		Request:  rr,
		Policy:   pol,
		Settings: settings,
		Source:   res.Source,
		Target: &policy.Target{
			APIVersion: live.GetAPIVersion(),
			Kind:       live.GetKind(),
			Namespace:  live.GetNamespace(),
			Name:       live.GetName(),
			UID:        remediationv1alpha1.UID(live.GetUID()),
			Labels:     live.GetLabels(),
		},
	})
	o := outcome{
		reason:         string(d.Reason),
		message:        d.Message,
		source:         res.Source,
		policyUID:      remediationv1alpha1.UID(pol.UID),
		policyRevision: fmt.Sprintf("%s/%d", pol.Name, pol.Generation),
	}
	switch d.Outcome {
	case policy.OutcomeAllowed:
		o.phase, o.status = remediationv1alpha1.PhasePolicyChecked, metav1.ConditionTrue
		o.approval = &d.EffectiveApprovalRequired
		// The plan is fixed here, from the very objects the decision was made on, so no later read of the
		// policy or the settings can slip in between the decision and the digest.
		digest, err := freshness.ComputeDigest(rr, res.Source, pol, settings)
		if err != nil {
			return needsIntervention(string(sourcemap.ReasonInvalidInput), "The plan digest cannot be computed"), nil
		}
		o.planDigest = digest
	case policy.OutcomeDenied:
		o.phase, o.status = remediationv1alpha1.PhaseDenied, metav1.ConditionFalse
	default:
		o.phase, o.status = remediationv1alpha1.PhaseNeedsIntervention, metav1.ConditionUnknown
	}
	return o, nil
}

// target reads the live target. It reports false when the object, or its kind, does not exist.
func (r *RemediationRequestReconciler) target(ctx context.Context, ref remediationv1alpha1.ObjectRef) (
	*unstructured.Unstructured, bool, error) {
	gv, err := schema.ParseGroupVersion(ref.APIVersion)
	if err != nil {
		return nil, false, nil
	}
	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(gv.WithKind(ref.Kind))
	err = r.reader().Get(ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, live)
	switch {
	case apierrors.IsNotFound(err) || meta.IsNoMatchError(err):
		return nil, false, nil
	case err != nil:
		return nil, false, fmt.Errorf("get %s %s/%s: %w", ref.Kind, ref.Namespace, ref.Name, err)
	}
	return live, true, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *RemediationRequestReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Source == nil {
		return errors.New("a SourceResolver is required")
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&remediationv1alpha1.RemediationRequest{}).
		Named("remediationrequest").
		Complete(r)
}

// checkBaseline compares the previous value the request claims with the live value of the field. The policy
// measures increases and decreases from the claimed value, so a forged or stale one could pass a change that
// is far larger than the limit allows (invariant 1). A missing field needs a person; a different value is
// refused.
func checkBaseline(action remediationv1alpha1.Action, field sourcemap.Field, live *unstructured.Unstructured) (
	outcome, bool) {
	claimed, ok := sourcemap.PreviousValue(action)
	if !ok {
		return needsIntervention(string(sourcemap.ReasonInvalidInput), "The action has no previous value"), true
	}
	got, found := field.Get(live.Object)
	if !found {
		return needsIntervention(string(sourcemap.ReasonFieldNotFound),
			fmt.Sprintf("%s is not set on the live object", field)), true
	}
	if !field.Equals(got, claimed) {
		return denied(ReasonPreviousValueMismatch,
			fmt.Sprintf("The previous value in the request is not the live value of %s", field)), true
	}
	return outcome{}, false
}

// beforeMerge reports whether a phase is one in which the plan can still be superseded.
func beforeMerge(p remediationv1alpha1.RemediationPhase) bool {
	switch p {
	case remediationv1alpha1.PhasePolicyChecked, remediationv1alpha1.PhasePRCreated,
		remediationv1alpha1.PhaseAwaitingApproval, remediationv1alpha1.PhaseAutoMergeQueued:
		return true
	}
	return false
}

// recheck is the freshness step of Reconcile for a request that has left the policy check but has not merged.
// Git writes are made by a later step, which calls EnsureFresh itself before each of them; this step makes a
// stale plan visible (and closes its change request) without waiting for the next write.
func (r *RemediationRequestReconciler) recheck(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) (
	ctrl.Result, error) {
	if r.OpenRepository == nil || !beforeMerge(rr.Status.Phase) || rr.Status.PlanDigest == "" {
		return ctrl.Result{}, nil
	}
	switch err := r.EnsureFresh(ctx, rr); {
	case err == nil || errors.Is(err, ErrSuperseded):
		return ctrl.Result{}, nil
	case errors.Is(err, freshness.ErrPaused):
		return ctrl.Result{RequeueAfter: freshRecheckDelay}, nil
	default:
		return ctrl.Result{}, err
	}
}

// EnsureFresh must run right before every Git write of a request (design section 6). It returns nil only when
// the plan is fresh. When an input of the plan digest changed, it closes the request's change request through
// the provider, sets the phase to Superseded and returns ErrSuperseded; the branch is never rebuilt or
// force-pushed. Any other error (an input could not be read, writes are paused) means "not fresh": the caller
// must not write and may retry.
//
// It is idempotent (invariant 5): closing a closed change request succeeds, and a request that is already
// Superseded returns ErrSuperseded without calling the provider again. The change request is closed before
// the status is written, so a crash in between leaves a retry that closes it again as a no-op.
func (r *RemediationRequestReconciler) EnsureFresh(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) error {
	if rr.Status.Phase == remediationv1alpha1.PhaseSuperseded {
		return ErrSuperseded
	}
	err := freshness.Checker{Reader: r.reader(), OpenRepository: r.OpenRepository}.Fresh(ctx, rr)
	staleErr, ok := errors.AsType[*freshness.StaleError](err)
	if !ok {
		return err
	}
	if err := r.closeChangeRequest(ctx, rr); err != nil {
		return err
	}
	rr.Status.Phase = remediationv1alpha1.PhaseSuperseded
	meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{
		Type:               remediationv1alpha1.ConditionPlanFresh,
		Status:             metav1.ConditionFalse,
		Reason:             staleErr.Reason,
		Message:            staleErr.Message,
		ObservedGeneration: rr.Generation,
	})
	if err := r.Status().Update(ctx, rr); err != nil {
		return fmt.Errorf("update status of RemediationRequest %s/%s: %w", rr.Namespace, rr.Name, err)
	}
	logf.FromContext(ctx).Info("Superseded RemediationRequest", "remediationRequest", client.ObjectKeyFromObject(rr),
		"reason", staleErr.Reason)
	return ErrSuperseded
}

// closeChangeRequest closes the request's change request, if it has one. A change request that is already
// closed, or that was never opened, is not an error; a merged one is, because the change landed.
func (r *RemediationRequestReconciler) closeChangeRequest(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) error {
	src := rr.Status.Source
	if src == nil || r.OpenRepository == nil {
		return errors.New("cannot close the change request: no source or Git provider")
	}
	settings := &remediationv1alpha1.RemediationSettings{}
	if err := r.getLive(ctx, types.NamespacedName{Name: settingsName}, settings); err != nil {
		return fmt.Errorf("get RemediationSettings %s: %w", settingsName, err)
	}
	var repo *remediationv1alpha1.GitRepository
	for i := range settings.Spec.Repositories {
		if settings.Spec.Repositories[i].Name == src.RepositoryRef {
			repo = &settings.Spec.Repositories[i]
		}
	}
	if repo == nil {
		// The repository left the settings: nothing identifies where the change request lives.
		return fmt.Errorf("repository %q is not in the settings, cannot close its change request", src.RepositoryRef)
	}
	git, err := r.OpenRepository(ctx, *repo)
	if err != nil {
		return fmt.Errorf("open repository %q: %w", repo.Name, err)
	}
	number := int64(0)
	if rr.Status.PullRequest != nil {
		number = rr.Status.PullRequest.Number
	} else {
		// The pull request may exist although the status write that records it failed (design section 6).
		cr, err := git.FindChangeRequest(ctx, freshness.BranchName(rr))
		switch {
		case errors.Is(err, providers.ErrNotFound):
			return nil
		case err != nil:
			return fmt.Errorf("find the change request of %q: %w", repo.Name, err)
		}
		number = cr.Number
	}
	if err := git.CloseChangeRequest(ctx, number); err != nil {
		return fmt.Errorf("close change request %d of %q: %w", number, repo.Name, err)
	}
	return nil
}
