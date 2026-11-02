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
		return ctrl.Result{}, nil
	}

	out, err := r.check(ctx, rr)
	if err != nil {
		return ctrl.Result{}, err
	}

	rr.Status.ObservedGeneration = rr.Generation
	rr.Status.Phase = out.phase
	rr.Status.Source = out.source
	rr.Status.PolicyUID = out.policyUID
	rr.Status.PolicyRevision = out.policyRevision
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
	switch err := r.Get(ctx, types.NamespacedName{Name: string(rr.Spec.PolicyRef)}, pol); {
	case apierrors.IsNotFound(err):
		return denied(ReasonPolicyNotFound, fmt.Sprintf("Policy %q does not exist", rr.Spec.PolicyRef)), nil
	case err != nil:
		return outcome{}, fmt.Errorf("get RemediationPolicy %s: %w", rr.Spec.PolicyRef, err)
	}
	settings := &remediationv1alpha1.RemediationSettings{}
	switch err := r.Get(ctx, types.NamespacedName{Name: settingsName}, settings); {
	case apierrors.IsNotFound(err):
		return denied(ReasonSettingsNotFound, "RemediationSettings default does not exist"), nil
	case err != nil:
		return outcome{}, fmt.Errorf("get RemediationSettings %s: %w", settingsName, err)
	}

	ref := rr.Spec.TargetRef
	// Refuse kinds the action may not change before reading anything, so the executor never reads, for
	// example, a Secret that a request names.
	if _, err := sourcemap.FieldFor(rr.Spec.Action, ref); err != nil {
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
	err = r.Get(ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, live)
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
