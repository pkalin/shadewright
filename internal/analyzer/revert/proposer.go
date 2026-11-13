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

// Package revert is the analyzer's deterministic revert proposer (ADR-0030 section 8, #96). For a
// RemediationRequest that ended Failed with Verified False it creates the revert: a new request with the same
// Incident, target and policy, the inverse action (v1alpha1.InverseAction, the helper the executor's link check
// compares against), approvalRequired true, spec.reverts naming the failed request by name and UID, and the
// name v1alpha1.RevertName of that UID. Then it records the failed request in the Incident's
// status.revertsProposed, and never creates a revert for a request recorded there, so a failed request gets one
// revert for good, also after that revert was deleted.
//
// It calls no model and imports nothing of the analysis loop or the model providers (a test checks the
// imports): its only input is the failed request, its Incident and its target as the API server holds them.
// The revert is a proposal like any other (invariant 1): the executor checks the link, then the policy, the
// freshness of the plan and the target lock, and opens a pull request that a person must approve (invariant 3).
//
// It never proposes a revert of a revert, nor for any phase but Failed with Verified False (so never for
// Inconclusive), nor when the failed request's Incident or target is gone (not found, another UID, or being
// deleted), nor when its action cannot be inverted. It uses the analyzer's existing grants only: get on
// requests and create on them in the operator namespace, get on the Incident and update on its status, and get
// on the target with the watch role (Deployments and StatefulSets) or the KEDA module's role (ScaledObjects).
// It watches the requests through the informer the Incident controller already has.
package revert

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
)

// FieldOwner is the field manager of the proposer's writes: the revert it creates and the Incident status
// update that records it.
const FieldOwner = "shadewright-revert-proposer"

// reconcileTimeout bounds one pass: the reads, the create and the status write. A pass that runs out returns
// the error and is retried with the controller's backoff.
const reconcileTimeout = 60 * time.Second

// Proposer creates the revert of a request that ended Failed with Verified False. It runs on the leader only.
// A failed read or write returns an error, which the controller retries with its exponential backoff; it is
// never taken as an answer.
type Proposer struct {
	// Client creates the revert and writes the Incident status.
	Client client.Client
	// Reader reads the failed request, its Incident, its target and an existing revert live, not from the
	// cache, so a decision never rests on a stale copy.
	Reader client.Reader
	// Namespace is the operator namespace: the requests' and the Incidents'.
	Namespace string
}

// SetupWithManager registers the proposer for the requests of the operator namespace that ended Failed with
// Verified False and are not reverts themselves ([Revertible]).
func (p *Proposer) SetupWithManager(mgr manager.Manager) error {
	if p.Reader == nil {
		p.Reader = mgr.GetAPIReader()
	}
	if p.Client == nil || p.Namespace == "" {
		return errors.New("revert proposer: a client and the operator namespace are required")
	}
	failed := predicate.NewPredicateFuncs(func(o client.Object) bool {
		rr, ok := o.(*remediationv1alpha1.RemediationRequest)
		return ok && rr.Namespace == p.Namespace && Revertible(rr)
	})
	return builder.ControllerManagedBy(mgr).
		Named("revert-proposer").
		For(&remediationv1alpha1.RemediationRequest{}, builder.WithPredicates(failed)).
		Complete(p)
}

// Revertible reports whether rr is a request the proposer makes a revert for: it ended Failed with condition
// Verified False, it is not a revert itself (a revert is never reverted) and it is not being deleted. Every
// other phase, Inconclusive included, gives no revert.
func Revertible(rr *remediationv1alpha1.RemediationRequest) bool {
	if rr == nil || remediationv1alpha1.IsRevert(rr) || rr.DeletionTimestamp != nil ||
		rr.Status.Phase != remediationv1alpha1.PhaseFailed {
		return false
	}
	verified := meta.FindStatusCondition(rr.Status.Conditions, remediationv1alpha1.ConditionVerified)
	return verified != nil && verified.Status == metav1.ConditionFalse
}

// Spec returns the spec of the revert of failed: the same Incident, target, mode and policy, the inverse
// action, approvalRequired true, the failed request's verification bounds (timeout and stableFor; a revert is
// never checked for clearing, so its extra checks would not apply) and spec.reverts naming failed by name and
// UID. ok is false when the action cannot be inverted ([remediationv1alpha1.InverseAction]).
func Spec(failed *remediationv1alpha1.RemediationRequest) (spec remediationv1alpha1.RemediationRequestSpec, ok bool) {
	inverse, ok := remediationv1alpha1.InverseAction(failed.Spec.Action)
	if !ok {
		return remediationv1alpha1.RemediationRequestSpec{}, false
	}
	v := failed.Spec.Verification
	return remediationv1alpha1.RemediationRequestSpec{
		IncidentRef:      failed.Spec.IncidentRef,
		TargetRef:        failed.Spec.TargetRef,
		Mode:             remediationv1alpha1.RemediationModeGitOps,
		ApprovalRequired: new(true),
		PolicyRef:        failed.Spec.PolicyRef,
		Action:           inverse,
		Verification: remediationv1alpha1.Verification{
			Timeout: v.Timeout.DeepCopy(), StableFor: v.StableFor.DeepCopy(),
		},
		Reverts: new(link(failed)),
	}, true
}

// link is the spec.reverts of failed's revert.
func link(failed *remediationv1alpha1.RemediationRequest) remediationv1alpha1.RequestRef {
	return remediationv1alpha1.RequestRef{Name: failed.Name, UID: remediationv1alpha1.UID(failed.UID)}
}

// Reconcile proposes the revert of one failed request, once. The steps, each read live:
//
//  1. The failed request: gone, or no longer [Revertible], ends the pass.
//  2. Its action must invert, else no revert.
//  3. Its Incident (by name and UID): gone or another UID gives no revert. A request already listed in
//     status.revertsProposed gets no second one; a full list gives none either.
//  4. Its target (by kind, name and UID): gone, another UID or being deleted gives no revert.
//  5. The create. AlreadyExists with spec.reverts naming the same request and UID is the revert made by an
//     earlier pass whose record was lost; with any other link it is an error, retried and never overwritten.
//  6. The record in status.revertsProposed, with the resourceVersion read, retried on conflict.
//
// A crash between 5 and 6 leaves a revert without its record; the next pass finds it at step 5 and records it.
func (p *Proposer) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	if req.Namespace != p.Namespace {
		return reconcile.Result{}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, reconcileTimeout)
	defer cancel()
	logger := log.FromContext(ctx).WithValues("request", req.Name)

	failed := &remediationv1alpha1.RemediationRequest{}
	if err := p.Reader.Get(ctx, req.NamespacedName, failed); err != nil {
		if apierrors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, fmt.Errorf("reading RemediationRequest %s: %w", req.Name, err)
	}
	if !Revertible(failed) {
		return reconcile.Result{}, nil
	}
	spec, ok := Spec(failed)
	if !ok {
		logger.Info("No revert: the failed request's action cannot be inverted", "action", failed.Spec.Action.Type)
		return reconcile.Result{}, nil
	}
	ref := link(failed)

	inc, err := p.incident(ctx, failed)
	if err != nil {
		return reconcile.Result{}, err
	}
	if inc == nil {
		logger.Info("No revert: the failed request's Incident is gone", "incident", failed.Spec.IncidentRef.Name)
		return reconcile.Result{}, nil
	}
	if slices.Contains(inc.Status.RevertsProposed, ref) {
		return reconcile.Result{}, nil
	}
	if len(inc.Status.RevertsProposed) >= remediationv1alpha1.MaxRevertsProposed {
		logger.Info("No revert: the Incident's status.revertsProposed is full", "incident", inc.Name,
			"max", remediationv1alpha1.MaxRevertsProposed)
		return reconcile.Result{}, nil
	}

	gone, err := p.targetGone(ctx, failed.Spec.TargetRef)
	if err != nil {
		return reconcile.Result{}, err
	}
	if gone {
		logger.Info("No revert: the failed request's target is gone", "kind", failed.Spec.TargetRef.Kind,
			"namespace", failed.Spec.TargetRef.Namespace, "name", failed.Spec.TargetRef.Name)
		return reconcile.Result{}, nil
	}

	name := remediationv1alpha1.RevertName(ref.UID)
	if err := p.create(ctx, string(name), spec); err != nil {
		return reconcile.Result{}, err
	}
	if err := p.record(ctx, failed, ref); err != nil {
		return reconcile.Result{}, err
	}
	logger.Info("Proposed the revert of a failed request", "revert", name, "incident", inc.Name)
	return reconcile.Result{}, nil
}

// incident reads the Incident failed names, live. It returns nil, with no error, when it is gone or has
// another UID: the revert would have nowhere to be recorded, so none is made.
func (p *Proposer) incident(ctx context.Context, failed *remediationv1alpha1.RemediationRequest) (
	*remediationv1alpha1.Incident, error) {
	ref := failed.Spec.IncidentRef
	inc := &remediationv1alpha1.Incident{}
	if err := p.Reader.Get(ctx, types.NamespacedName{Namespace: p.Namespace, Name: ref.Name}, inc); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading Incident %s: %w", ref.Name, err)
	}
	if remediationv1alpha1.UID(inc.UID) != ref.UID {
		return nil, nil
	}
	return inc, nil
}

// targetGone reads the target live and reports whether it is gone: not found, another UID (deleted and made
// again) or being deleted. Any other failed read is an error.
func (p *Proposer) targetGone(ctx context.Context, t remediationv1alpha1.ObjectRef) (bool, error) {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion(t.APIVersion)
	u.SetKind(t.Kind)
	if err := p.Reader.Get(ctx, types.NamespacedName{Namespace: t.Namespace, Name: t.Name}, u); err != nil {
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, fmt.Errorf("reading the target %s %s/%s: %w", t.Kind, t.Namespace, t.Name, err)
	}
	return remediationv1alpha1.UID(u.GetUID()) != t.UID || u.GetDeletionTimestamp() != nil, nil
}

// create creates the revert name with spec. An existing request of that name is success only when its
// spec.reverts names the same request and UID; otherwise it is an error, and the object is left as it is.
func (p *Proposer) create(ctx context.Context, name string, spec remediationv1alpha1.RemediationRequestSpec) error {
	rr := &remediationv1alpha1.RemediationRequest{
		ObjectMeta: metav1.ObjectMeta{Namespace: p.Namespace, Name: name},
		Spec:       *spec.DeepCopy(),
	}
	err := p.Client.Create(ctx, rr, client.FieldOwner(FieldOwner))
	if !apierrors.IsAlreadyExists(err) {
		if err != nil {
			return fmt.Errorf("creating the revert %s: %w", name, err)
		}
		return nil
	}
	existing := &remediationv1alpha1.RemediationRequest{}
	if err := p.Reader.Get(ctx, client.ObjectKeyFromObject(rr), existing); err != nil {
		// Gone again since the create, or unreadable: the next pass creates or reads it.
		return fmt.Errorf("reading the existing revert %s: %w", name, err)
	}
	if existing.Spec.Reverts == nil || *existing.Spec.Reverts != *spec.Reverts {
		return fmt.Errorf("RemediationRequest %s exists and does not revert %s with UID %s; it is left as it is",
			name, spec.Reverts.Name, spec.Reverts.UID)
	}
	return nil
}

// record adds ref to the status.revertsProposed of failed's Incident, read live on each attempt, with the
// resourceVersion read; a conflict is tried again. An Incident gone or with another UID meanwhile needs no
// record: the revert names it, and no pass makes another revert for an Incident that is gone.
func (p *Proposer) record(ctx context.Context, failed *remediationv1alpha1.RemediationRequest,
	ref remediationv1alpha1.RequestRef) error {
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		inc, err := p.incident(ctx, failed)
		if err != nil || inc == nil || slices.Contains(inc.Status.RevertsProposed, ref) {
			return err
		}
		if len(inc.Status.RevertsProposed) >= remediationv1alpha1.MaxRevertsProposed {
			return fmt.Errorf("status.revertsProposed of Incident %s is full", inc.Name)
		}
		inc.Status.RevertsProposed = append(inc.Status.RevertsProposed, ref)
		return p.Client.Status().Update(ctx, inc, client.FieldOwner(FieldOwner))
	})
	if err != nil {
		return fmt.Errorf("recording the revert of %s in its Incident: %w", failed.Name, err)
	}
	return nil
}
