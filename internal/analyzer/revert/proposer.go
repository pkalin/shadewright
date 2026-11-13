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
// name v1alpha1.RevertName of that UID.
//
// The decision is recorded in the Incident's status.revertsProposed before the create: an entry in state
// Pending, then the create, then the entry moves to Created. An entry in either state means no new decision is
// taken: Pending leads to the (idempotent) create and the move to Created, Created leads to nothing, also after
// the revert was deleted. So a failed request gets one revert for good. The one window left: the create
// succeeded, the move to Created was not written (a crash or a failed write), and the revert was deleted before
// the retry; the retry then creates it once more (ADR-0030, known limit).
//
// It calls no model and imports nothing of the analysis loop or the model providers (a test checks the
// imports): its only input is the failed request, its Incident and its target as the API server holds them.
// The revert is a proposal like any other (invariant 1): the executor checks the link, then the policy, the
// freshness of the plan and the target lock, and opens a pull request that a person must approve (invariant 3).
//
// It never proposes a revert of a revert, nor for any phase but Failed with Verified False (so never for
// Inconclusive), nor when the failed request's Incident or target is gone (not found, another UID, or being
// deleted), nor when its action cannot be inverted, nor for a request whose Verified condition moved more than
// [MaxAge] ago (so a start-up does not revert every old failure), nor when the Incident's list is full. It uses
// the analyzer's existing grants only: get on requests and create on them in the operator namespace, get on the
// Incident and update on its status, get on the target with the watch role (Deployments and StatefulSets) or the
// KEDA module's role (ScaledObjects), and create on core Events in the operator namespace (the leader election
// Role's grant). It watches the requests through the informer the Incident controller already has.
package revert

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
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
// updates that record it.
const FieldOwner = "shadewright-revert-proposer"

// MaxAge bounds how long after its Verified condition moved a failed request still gets a revert. An older one
// is left to a person, so a start-up (or a first install over old requests) does not revert every past failure.
const MaxAge = 24 * time.Hour

// Reasons of the proposer's Warning events.
const (
	// EventIncidentGone: the failed request's Incident is gone or has another UID, so no revert is proposed.
	EventIncidentGone = "RevertNotProposed"
	// EventNameTaken: a request at the revert's derived name does not revert the failed request.
	EventNameTaken = "RevertNameTaken"
	// EventListFull is about the Incident: its status.revertsProposed is full, no more reverts are proposed.
	EventListFull = remediationv1alpha1.ReasonRevertListFull
)

// reconcileTimeout bounds one pass: the reads, the create and the status writes. A pass that runs out returns
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
	// Recorder writes the Warning events; nil writes none (the outcome is still logged).
	Recorder record.EventRecorder
	// Now is the clock of the age bound and of the RevertsStopped condition; nil is time.Now.
	Now func() time.Time
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

// Entry returns the entry of inc's status.revertsProposed for ref (same name and UID), or nil.
func Entry(inc *remediationv1alpha1.Incident, ref remediationv1alpha1.RequestRef) *remediationv1alpha1.ProposedRevert {
	i := slices.IndexFunc(inc.Status.RevertsProposed, func(e remediationv1alpha1.ProposedRevert) bool {
		return e.RequestRef == ref
	})
	if i < 0 {
		return nil
	}
	return &inc.Status.RevertsProposed[i]
}

// Reconcile proposes the revert of one failed request, once. The steps, each read live:
//
//  1. The failed request: gone, or no longer [Revertible], ends the pass. Its action must invert, else no
//     revert.
//  2. Its Incident (by name and UID): gone or another UID gives no revert, with a Warning event on the request.
//  3. Its entry in status.revertsProposed: Created ends the pass; Pending goes on to step 6.
//  4. Without an entry, a new decision: the Verified condition moved at most [MaxAge] ago, a full list gives
//     no revert (condition RevertsStopped on the Incident and one Warning event), the target is of a kind the
//     action can target and is there (not found, another UID, being deleted or Forbidden gives no revert).
//  5. The entry, Pending, with the resourceVersion read, retried on conflict.
//  6. The create. AlreadyExists with spec.reverts naming the same request and UID is the revert made by an
//     earlier pass; with any other link it is a terminal error, with a Warning event, and never overwritten.
//  7. The entry moves to Created.
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
		p.incidentGone(ctx, failed)
		return reconcile.Result{}, nil
	}
	if e := Entry(inc, ref); e != nil {
		if e.State == remediationv1alpha1.RevertCreated {
			return reconcile.Result{}, nil
		}
	} else {
		if decided, err := p.decide(ctx, failed, inc, ref); !decided || err != nil {
			return reconcile.Result{}, err
		}
	}

	name := remediationv1alpha1.RevertName(ref.UID)
	if err := p.create(ctx, failed, string(name), spec); err != nil {
		return reconcile.Result{}, err
	}
	if err := p.markCreated(ctx, failed, ref); err != nil {
		return reconcile.Result{}, err
	}
	logger.Info("Proposed the revert of a failed request", "revert", name, "incident", inc.Name)
	return reconcile.Result{}, nil
}

// decide takes the decision for a failed request its Incident (inc) has no entry for, and writes it as a
// Pending entry. decided is false when no revert is proposed; an error is for a retry.
func (p *Proposer) decide(ctx context.Context, failed *remediationv1alpha1.RemediationRequest,
	inc *remediationv1alpha1.Incident, ref remediationv1alpha1.RequestRef) (decided bool, err error) {
	logger := log.FromContext(ctx)
	if p.tooOld(failed) {
		logger.Info("No revert: the request failed more than the age bound ago; a person decides on it",
			"maxAge", MaxAge)
		return false, nil
	}
	if len(inc.Status.RevertsProposed) >= remediationv1alpha1.MaxRevertsProposed {
		return false, p.stop(ctx, failed)
	}
	why, err := p.targetUnfit(ctx, failed.Spec)
	if err != nil {
		return false, err
	}
	if why != "" {
		t := failed.Spec.TargetRef
		logger.Info("No revert: "+why, "apiVersion", t.APIVersion, "kind", t.Kind, "namespace", t.Namespace,
			"name", t.Name)
		return false, nil
	}
	return p.writePending(ctx, failed, ref)
}

// tooOld reports whether failed's Verified condition moved more than [MaxAge] ago. Its lastTransitionTime is
// written by the executor, another writer with its own clock, compared here with the analyzer's: the bound is
// coarse on purpose (a day against clock skews of seconds) and only keeps old failures from being reverted at a
// start-up. A missing time counts as old.
func (p *Proposer) tooOld(failed *remediationv1alpha1.RemediationRequest) bool {
	verified := meta.FindStatusCondition(failed.Status.Conditions, remediationv1alpha1.ConditionVerified)
	if verified == nil || verified.LastTransitionTime.IsZero() {
		return true
	}
	return p.now().Sub(verified.LastTransitionTime.Time) > MaxAge
}

func (p *Proposer) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
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

// incidentGone logs that failed gets no revert because its Incident is gone or has another UID, and writes a
// Warning event on failed, so a person sees why nothing was proposed. A request past the age bound gets the log
// only, so a start-up does not repeat the event for every old request.
func (p *Proposer) incidentGone(ctx context.Context, failed *remediationv1alpha1.RemediationRequest) {
	log.FromContext(ctx).Info("No revert: the failed request's Incident is gone",
		"incident", failed.Spec.IncidentRef.Name)
	if !p.tooOld(failed) {
		p.event(failed, EventIncidentGone, "No revert is proposed: Incident %s with the UID this request names is gone",
			failed.Spec.IncidentRef.Name)
	}
}

func (p *Proposer) event(obj runtime.Object, reason, format string, args ...any) {
	if p.Recorder != nil {
		p.Recorder.Eventf(obj, corev1.EventTypeWarning, reason, format, args...)
	}
}

// targets are the kinds each action of the catalog can target, as the request's CEL rules allow them. A target
// of any other kind is never read.
var targets = map[remediationv1alpha1.ActionType]func(apiVersion, kind string) bool{
	remediationv1alpha1.ActionSetContainerMemoryLimit: func(apiVersion, kind string) bool {
		return apiVersion == "apps/v1" && (kind == "Deployment" || kind == "StatefulSet")
	},
	remediationv1alpha1.ActionAdjustKedaMaxReplicas: func(apiVersion, kind string) bool {
		return strings.HasPrefix(apiVersion, "keda.sh/") && kind == "ScaledObject"
	},
}

// targetUnfit reads the target live and returns why no revert is proposed for it, or "" when it is there: its
// kind is not one the action can target, it is not found, has another UID (deleted and made again) or is being
// deleted, or the analyzer may not read it (Forbidden: no grant will appear by retrying). Any other failed read
// is an error for a retry.
func (p *Proposer) targetUnfit(ctx context.Context, spec remediationv1alpha1.RemediationRequestSpec) (string, error) {
	t := spec.TargetRef
	if fits, ok := targets[spec.Action.Type]; !ok || !fits(t.APIVersion, t.Kind) {
		return "the target is of a kind the action cannot target", nil
	}
	u := &unstructured.Unstructured{}
	u.SetAPIVersion(t.APIVersion)
	u.SetKind(t.Kind)
	if err := p.Reader.Get(ctx, types.NamespacedName{Namespace: t.Namespace, Name: t.Name}, u); err != nil {
		switch {
		case apierrors.IsNotFound(err):
			return "the failed request's target is gone", nil
		case apierrors.IsForbidden(err):
			return "the analyzer may not read the failed request's target", nil
		}
		return "", fmt.Errorf("reading the target %s %s/%s: %w", t.Kind, t.Namespace, t.Name, err)
	}
	if remediationv1alpha1.UID(u.GetUID()) != t.UID || u.GetDeletionTimestamp() != nil {
		return "the failed request's target is gone", nil
	}
	return "", nil
}

// errFull stops writePending when the Incident's list is full.
var errFull = errors.New("status.revertsProposed is full")

// writePending adds ref as Pending to the status.revertsProposed of failed's Incident, read live on each
// attempt, with the resourceVersion read; a conflict is tried again. decided is true when an entry for ref is
// there afterwards (written now or by an earlier pass) and is not Created yet; false, with no error, when the
// Incident is gone, the entry is already Created, or the list is full ([Proposer.stop]).
func (p *Proposer) writePending(ctx context.Context, failed *remediationv1alpha1.RemediationRequest,
	ref remediationv1alpha1.RequestRef) (decided bool, err error) {
	gone := false
	err = retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		gone, decided = false, false
		inc, err := p.incident(ctx, failed)
		if err != nil {
			return err
		}
		if inc == nil {
			gone = true
			return nil
		}
		if e := Entry(inc, ref); e != nil {
			decided = e.State == remediationv1alpha1.RevertPending
			return nil
		}
		if len(inc.Status.RevertsProposed) >= remediationv1alpha1.MaxRevertsProposed {
			return errFull
		}
		inc.Status.RevertsProposed = append(inc.Status.RevertsProposed, remediationv1alpha1.ProposedRevert{
			RequestRef: ref, State: remediationv1alpha1.RevertPending})
		if err := p.Client.Status().Update(ctx, inc, client.FieldOwner(FieldOwner)); err != nil {
			return err
		}
		decided = true
		return nil
	})
	switch {
	case errors.Is(err, errFull):
		return false, p.stop(ctx, failed)
	case err != nil:
		return false, fmt.Errorf("recording the decision to revert %s in its Incident: %w", failed.Name, err)
	case gone:
		p.incidentGone(ctx, failed)
	}
	return decided, nil
}

// stop records that failed's Incident gets no more reverts: its status.revertsProposed is full. It sets the
// condition RevertsStopped (True, reason RevertListFull) once, and writes one Warning event about the Incident
// when it sets it. Every other failed request of that Incident is only logged. No revert is created and nothing
// is retried, except a failed read or write.
func (p *Proposer) stop(ctx context.Context, failed *remediationv1alpha1.RemediationRequest) error {
	var stopped *remediationv1alpha1.Incident
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		stopped = nil
		inc, err := p.incident(ctx, failed)
		if err != nil || inc == nil || meta.IsStatusConditionTrue(inc.Status.Conditions,
			remediationv1alpha1.ConditionRevertsStopped) {
			return err
		}
		meta.SetStatusCondition(&inc.Status.Conditions, metav1.Condition{
			Type: remediationv1alpha1.ConditionRevertsStopped, Status: metav1.ConditionTrue,
			Reason: remediationv1alpha1.ReasonRevertListFull,
			Message: fmt.Sprintf("status.revertsProposed holds %d entries, the most it may: no more reverts are "+
				"proposed for this incident, a person decides on its other failed requests",
				remediationv1alpha1.MaxRevertsProposed),
			ObservedGeneration: inc.Generation,
			LastTransitionTime: metav1.NewTime(p.now().UTC().Truncate(time.Second)),
		})
		if err := p.Client.Status().Update(ctx, inc, client.FieldOwner(FieldOwner)); err != nil {
			return err
		}
		stopped = inc
		return nil
	})
	if err != nil {
		return fmt.Errorf("setting %s on Incident %s: %w", remediationv1alpha1.ConditionRevertsStopped,
			failed.Spec.IncidentRef.Name, err)
	}
	log.FromContext(ctx).Info("No revert: the Incident's status.revertsProposed is full",
		"incident", failed.Spec.IncidentRef.Name, "max", remediationv1alpha1.MaxRevertsProposed)
	if stopped != nil {
		p.event(stopped, EventListFull, "No more reverts are proposed for this incident: status.revertsProposed "+
			"holds %d entries", remediationv1alpha1.MaxRevertsProposed)
	}
	return nil
}

// create creates the revert name with spec. An existing request of that name is success only when its
// spec.reverts names the same request and UID; otherwise it is a terminal error, with a Warning event on failed,
// and the object is left as it is: a request at a name derived from failed's UID that reverts something else does
// not go away by retrying.
func (p *Proposer) create(ctx context.Context, failed *remediationv1alpha1.RemediationRequest, name string,
	spec remediationv1alpha1.RemediationRequestSpec) error {
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
		p.event(failed, EventNameTaken, "No revert is proposed: RemediationRequest %s, the revert's name, exists "+
			"and does not revert this request", name)
		return reconcile.TerminalError(fmt.Errorf(
			"RemediationRequest %s exists and does not revert %s with UID %s; it is left as it is",
			name, spec.Reverts.Name, spec.Reverts.UID))
	}
	return nil
}

// markCreated moves the entry of ref in the status.revertsProposed of failed's Incident from Pending to Created,
// read live on each attempt, with the resourceVersion read; a conflict is tried again. An Incident gone or with
// another UID meanwhile, or an entry already Created, needs no write.
func (p *Proposer) markCreated(ctx context.Context, failed *remediationv1alpha1.RemediationRequest,
	ref remediationv1alpha1.RequestRef) error {
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		inc, err := p.incident(ctx, failed)
		if err != nil || inc == nil {
			return err
		}
		e := Entry(inc, ref)
		if e == nil {
			// Entries are never removed (CEL), so this cannot follow a Pending write; retrying would not
			// bring the entry back.
			return reconcile.TerminalError(fmt.Errorf("incident %s has no entry for %s", inc.Name, ref.Name))
		}
		if e.State == remediationv1alpha1.RevertCreated {
			return nil
		}
		e.State = remediationv1alpha1.RevertCreated
		return p.Client.Status().Update(ctx, inc, client.FieldOwner(FieldOwner))
	})
	if err != nil {
		return fmt.Errorf("recording the revert of %s in its Incident: %w", failed.Name, err)
	}
	return nil
}
