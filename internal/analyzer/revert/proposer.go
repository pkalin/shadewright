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
// Pending, then the create, then the entry moves to Created. An entry means no new decision is taken: the
// failed request gets no second revert, also after the revert was deleted. Pending means a create may still
// come. For a Pending entry the revert's name is read first: a revert there that names the failed request is a
// create that happened, and the entry moves to Created whatever the checks say now. Otherwise every create, the
// first or a retried one, runs the same checks first (target, age bound, Incident not being deleted), and when
// one says no, or the revert's name is taken by another request, or the create is refused as invalid, or the
// failed request is deleted or its name taken by another UID, the entry moves to Abandoned, which is final, with
// a reason and one Warning event. Right before the create the Incident is read again, and the create is skipped
// when the entry is no longer Pending or the Incident is being deleted. Pending entries are also settled from the
// Incident's side, when this analyzer starts leading and whenever an Incident with one changes, so an entry
// whose request was deleted while no analyzer was leading, or whose request no longer gets a revert, does not
// stay Pending. One lock per Incident, shared by both controllers, keeps a settlement out of the span from the
// last Incident read before a create to the move to Created. The windows left (ADR-0030,
// known limits): the create succeeded, the move to Created was not written (a crash or a failed write), and the
// revert was deleted before the retry, which then creates it once more; and two analyzers running without
// leader election, where one may create between the other's last read and its own create (the lock is per
// process); and a read or write that fails for ever keeps an entry Pending, retried with backoff.
//
// It calls no model and imports nothing of the analysis loop or the model providers (a test checks the
// imports): its only input is the failed request, its Incident and its target as the API server holds them.
// The revert is a proposal like any other (invariant 1): the executor checks the link, then the policy, the
// freshness of the plan and the target lock, and opens a pull request that a person must approve (invariant 3).
//
// It never proposes a revert of a revert, nor for any phase but Failed with Verified False (so never for
// Inconclusive), nor when the failed request's Incident is gone or being deleted, nor when its target is gone
// (not found, another UID, being deleted, or its kind no longer served; a target it may not read is retried
// until the age bound), nor when its action cannot be inverted, nor for a request whose Verified condition moved
// more than [MaxAge] ago or is more than [MaxSkew] ahead (so a start-up does not revert every old failure), nor
// when the Incident's list is full. It uses the analyzer's existing grants only: get on requests and create on
// them in the operator namespace, get, list and watch on Incidents and update on their status, get on the target
// with the watch role (Deployments and StatefulSets) or the KEDA module's role (ScaledObjects), and create on
// core Events in the operator namespace (the leader election Role's grant). It watches the requests and the
// Incidents through the manager's shared cache, with the list and watch grants the analyzer already holds for
// its Incident and analysis controllers.
package revert

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
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

// MaxSkew is how far ahead of the analyzer's clock the Verified condition's transition time may be and still
// count as now. A time further ahead gets no revert: it is not a time the bound can be checked against.
const MaxSkew = 5 * time.Minute

// Reasons of the proposer's Warning events.
const (
	// EventIncidentGone: the failed request's Incident is gone or has another UID, so no revert is proposed.
	EventIncidentGone = "RevertNotProposed"
	// EventAbandoned: an entry moved from Pending to Abandoned, because a check before the create said no. The
	// message says which. It is on the failed request, or on the Incident when the request is gone.
	EventAbandoned = "RevertAbandoned"
	// EventListFull is about the Incident: its status.revertsProposed is full, no more reverts are proposed.
	EventListFull = remediationv1alpha1.ReasonRevertListFull
	// EventAbandonedButMade is about the Incident: a revert exists while its entry is Abandoned, a record that
	// no longer matches what exists. The lock of [incidentLocks] keeps the proposer's own passes from leading
	// there; it is reported, not repaired, since Abandoned is final.
	EventAbandonedButMade = "RevertAbandonedButMade"
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

	// locks serializes the two controllers on one Incident ([incidentLocks]).
	locks incidentLocks
}

// incidentLocks holds one lock per Incident (namespaced name), shared by the request controller and the Incident
// controller. The create path holds it from its last Incident read through the create and the move to Created;
// the settlement holds it for its whole pass over that Incident. So a settlement never sees "no request, no
// revert" between the last read before a create and the create, and never abandons an entry whose revert is being
// made. No lock is held across a requeue or the controller's backoff, and an entry is dropped once no pass holds
// or waits for it, so the map holds only the Incidents being worked on.
type incidentLocks struct {
	mu sync.Mutex
	m  map[types.NamespacedName]*incidentLock
}

// incidentLock is one Incident's lock: a channel of one slot, so a wait can end with the context, and the
// number of passes holding or waiting for it.
type incidentLock struct {
	slot chan struct{}
	refs int
}

// lock takes the lock of the Incident key, waiting until it is free or ctx ends (an error for a retry). The
// returned function releases it.
func (l *incidentLocks) lock(ctx context.Context, key types.NamespacedName) (unlock func(), err error) {
	l.mu.Lock()
	if l.m == nil {
		l.m = map[types.NamespacedName]*incidentLock{}
	}
	e := l.m[key]
	if e == nil {
		e = &incidentLock{slot: make(chan struct{}, 1)}
		l.m[key] = e
	}
	e.refs++
	l.mu.Unlock()
	select {
	case e.slot <- struct{}{}:
		return func() {
			<-e.slot
			l.release(key, e)
		}, nil
	case <-ctx.Done():
		l.release(key, e)
		return nil, fmt.Errorf("waiting for the lock of Incident %s: %w", key.Name, ctx.Err())
	}
}

// release drops one holder or waiter of e, and e itself when it was the last.
func (l *incidentLocks) release(key types.NamespacedName, e *incidentLock) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e.refs--; e.refs == 0 {
		delete(l.m, key)
	}
}

// users returns how many passes hold or wait for the lock of key (for tests).
func (l *incidentLocks) users(key types.NamespacedName) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e := l.m[key]; e != nil {
		return e.refs
	}
	return 0
}

// SetupWithManager registers the proposer for the requests of the operator namespace that ended Failed with
// Verified False and are not reverts themselves, also while and after they are deleted, so a Pending entry of a
// deleted request moves to Abandoned. A second controller watches the Incidents of the operator namespace that
// have a Pending entry and settles the entries whose request is gone, has another UID or no longer gets a revert
// ([Proposer.settlePending]), for the requests no pass of their own will finish; it watches Incidents through the manager's shared cache (list and
// watch on Incidents are existing grants of the analyzer) and reads the requests with get.
func (p *Proposer) SetupWithManager(mgr manager.Manager) error {
	if p.Reader == nil {
		p.Reader = mgr.GetAPIReader()
	}
	if p.Client == nil || p.Namespace == "" {
		return errors.New("revert proposer: a client and the operator namespace are required")
	}
	failed := predicate.NewPredicateFuncs(func(o client.Object) bool {
		rr, ok := o.(*remediationv1alpha1.RemediationRequest)
		return ok && rr.Namespace == p.Namespace && failedUnverified(rr)
	})
	if err := builder.ControllerManagedBy(mgr).
		Named("revert-proposer").
		For(&remediationv1alpha1.RemediationRequest{}, builder.WithPredicates(failed)).
		Complete(p); err != nil {
		return err
	}
	withPending := predicate.NewPredicateFuncs(func(o client.Object) bool {
		inc, ok := o.(*remediationv1alpha1.Incident)
		return ok && inc.Namespace == p.Namespace && hasPending(inc)
	})
	return builder.ControllerManagedBy(mgr).
		Named("revert-proposer-pending").
		For(&remediationv1alpha1.Incident{}, builder.WithPredicates(withPending)).
		Complete(reconcile.Func(p.reconcileIncident))
}

// Revertible reports whether rr is a request the proposer makes a revert for: it ended Failed with condition
// Verified False, it is not a revert itself (a revert is never reverted) and it is not being deleted. Every
// other phase, Inconclusive included, gives no revert.
func Revertible(rr *remediationv1alpha1.RemediationRequest) bool {
	return failedUnverified(rr) && rr.DeletionTimestamp == nil
}

// failedUnverified reports whether rr ended Failed with condition Verified False and is not a revert, whether
// or not it is being deleted.
func failedUnverified(rr *remediationv1alpha1.RemediationRequest) bool {
	if rr == nil || remediationv1alpha1.IsRevert(rr) || rr.Status.Phase != remediationv1alpha1.PhaseFailed {
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

// pending reports whether inc records ref as Pending.
func pending(inc *remediationv1alpha1.Incident, ref remediationv1alpha1.RequestRef) bool {
	e := Entry(inc, ref)
	return e != nil && e.State == remediationv1alpha1.RevertPending
}

// refusal is why no revert is made: the reason an Abandoned entry records, and the sentence the log and the
// Warning event show. A zero refusal is no refusal.
type refusal struct {
	reason remediationv1alpha1.RevertReason
	why    string
}

// Reconcile proposes the revert of one failed request, once. The steps, each read live:
//
//  1. The failed request. Gone, or being deleted, settles its Pending entries ([Proposer.settle]) and ends the
//     pass; not [Revertible] otherwise ends the pass. Its action must invert, else no revert.
//  2. Its Incident (by name and UID): gone or another UID gives no revert, with a Warning event on the request.
//  3. Its entry in status.revertsProposed: Created or Abandoned ends the pass. For a Pending entry the revert's
//     name is read first: a revert there whose spec.reverts names this request and UID was created by an earlier
//     pass, and the entry moves to Created whatever the checks say now; a request there with any other link
//     moves the entry to Abandoned (NameTaken). The checks guard only a create that has not happened.
//  4. The checks, the same for a new decision and for a Pending entry: the Verified condition moved at most
//     [MaxAge] ago and at most [MaxSkew] ahead, the Incident is not being deleted, and the target is of a kind
//     the action can target and is there (not found, another UID, being deleted or a kind not served is no; a
//     Forbidden read is an error for a retry, until the age bound says no). Without an entry, a no records
//     nothing and a full list gives no revert (condition RevertsStopped on the Incident and one Warning event);
//     otherwise the entry Pending is written. For a Pending entry a no moves it to Abandoned with its reason and
//     one Warning event.
//  5. The Incident again, right before the create: gone or another UID gives no revert; being deleted moves the
//     entry to Abandoned; an entry no longer Pending (another pass finished it) ends the pass.
//  6. The create. AlreadyExists with spec.reverts naming the same request and UID is the revert made by an
//     earlier pass. With any other link, or a create refused as invalid, the entry moves to Abandoned and the
//     existing object is left as it is.
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
			return reconcile.Result{}, p.settleDeleted(ctx, req.Name)
		}
		return reconcile.Result{}, fmt.Errorf("reading RemediationRequest %s: %w", req.Name, err)
	}
	if !Revertible(failed) {
		if failed.DeletionTimestamp != nil && failedUnverified(failed) {
			return reconcile.Result{}, p.settle(ctx, failed.Spec.IncidentRef, link(failed),
				refusal{remediationv1alpha1.RevertReasonRequestGone, "the failed request is being deleted"}, failed)
		}
		return reconcile.Result{}, nil
	}
	spec, ok := Spec(failed)
	if !ok {
		logger.Info("No revert: the failed request's action cannot be inverted", "action", failed.Spec.Action.Type)
		return reconcile.Result{}, nil
	}
	ref := link(failed)
	incRef := failed.Spec.IncidentRef

	inc, err := p.incident(ctx, incRef)
	if err != nil {
		return reconcile.Result{}, err
	}
	if inc == nil {
		p.incidentGone(ctx, failed)
		return reconcile.Result{}, nil
	}
	e := Entry(inc, ref)
	if e != nil && e.State != remediationv1alpha1.RevertPending {
		return reconcile.Result{}, nil
	}
	if e != nil {
		if done, err := p.finishExisting(ctx, failed, ref); done || err != nil {
			return reconcile.Result{}, err
		}
	}
	no, full, err := p.unfit(ctx, failed, inc, e == nil)
	if err != nil {
		return reconcile.Result{}, err
	}
	switch {
	case full:
		return reconcile.Result{}, nil
	case no.reason != "" && e != nil:
		return reconcile.Result{}, p.abandon(ctx, incRef, ref, no, failed)
	case no.reason != "":
		t := failed.Spec.TargetRef
		logger.Info("No revert: "+no.why, "apiVersion", t.APIVersion, "kind", t.Kind, "namespace", t.Namespace,
			"name", t.Name)
		return reconcile.Result{}, nil
	case e == nil:
		if decided, err := p.writePending(ctx, failed, ref); !decided || err != nil {
			return reconcile.Result{}, err
		}
	}

	return reconcile.Result{}, p.createLocked(ctx, failed, spec)
}

// createLocked is steps 5 to 7 of [Proposer.Reconcile], under the lock of the Incident ([incidentLocks]): the
// last Incident read, the create and the move to Created. A settlement of that Incident runs before or after
// all of them, never between the read and the create.
func (p *Proposer) createLocked(ctx context.Context, failed *remediationv1alpha1.RemediationRequest,
	spec remediationv1alpha1.RemediationRequestSpec) error {
	ref, incRef := link(failed), failed.Spec.IncidentRef
	unlock, err := p.locks.lock(ctx, types.NamespacedName{Namespace: p.Namespace, Name: incRef.Name})
	if err != nil {
		return err
	}
	defer unlock()

	// Another pass (a second analyzer without leader election) may have created the revert and moved the entry
	// meanwhile, and the revert may be deleted since; a settlement may have moved it to Abandoned; the Incident
	// may be in deletion since the checks: create only while this snapshot holds the entry as Pending and the
	// Incident is not being deleted.
	inc, err := p.incident(ctx, incRef)
	if err != nil {
		return err
	}
	switch {
	case inc == nil:
		p.incidentGone(ctx, failed)
		return nil
	case !pending(inc, ref):
		return nil
	case inc.DeletionTimestamp != nil:
		return p.abandon(ctx, incRef, ref, whyIncidentDeleting, failed)
	}

	refused, err := p.create(ctx, ref, spec)
	if err != nil {
		return err
	}
	if refused.reason != "" {
		return p.abandon(ctx, incRef, ref, refused, failed)
	}
	if err := p.markCreated(ctx, incRef, ref); err != nil {
		return err
	}
	log.FromContext(ctx).Info("Proposed the revert of a failed request", "revert", revertName(ref),
		"incident", inc.Name)
	return nil
}

// finishExisting ends a Pending entry of ref by what exists at the revert's name: a create of an earlier pass may
// have succeeded while its move to Created was not written, so what exists decides, not the checks. A revert
// there that names ref moves the entry to Created; a request there with any other link moves it to Abandoned
// (NameTaken). done is false when the name is free and the pass goes on to the checks.
func (p *Proposer) finishExisting(ctx context.Context, failed *remediationv1alpha1.RemediationRequest,
	ref remediationv1alpha1.RequestRef) (done bool, err error) {
	made, taken, err := p.existing(ctx, ref)
	switch {
	case err != nil:
		return true, err
	case made:
		if err := p.markCreated(ctx, failed.Spec.IncidentRef, ref); err != nil {
			return true, err
		}
		log.FromContext(ctx).Info("Recorded the existing revert of a failed request", "request", ref.Name,
			"revert", revertName(ref), "incident", failed.Spec.IncidentRef.Name)
		return true, nil
	case taken.reason != "":
		return true, p.abandon(ctx, failed.Spec.IncidentRef, ref, taken, failed)
	}
	return false, nil
}

// revertName is the name of the revert of the request ref names.
func revertName(ref remediationv1alpha1.RequestRef) string {
	return string(remediationv1alpha1.RevertName(ref.UID))
}

// whyIncidentDeleting is the refusal for an Incident in deletion.
var whyIncidentDeleting = refusal{remediationv1alpha1.RevertReasonIncidentDeleting, "the Incident is being deleted"}

// unfit runs the checks every create runs first, the first or a retried one, and returns why no revert is
// made, or a zero refusal when it may be. decision is true for a new decision, which also stops at a full list
// (full true, [Proposer.stop] has logged it). An error is for a retry.
func (p *Proposer) unfit(ctx context.Context, failed *remediationv1alpha1.RemediationRequest,
	inc *remediationv1alpha1.Incident, decision bool) (no refusal, full bool, err error) {
	if why := p.ageUnfit(failed); why != "" {
		return refusal{remediationv1alpha1.RevertReasonOutsideAgeBound, why}, false, nil
	}
	if inc.DeletionTimestamp != nil {
		return whyIncidentDeleting, false, nil
	}
	if decision && len(inc.Status.RevertsProposed) >= remediationv1alpha1.MaxRevertsProposed {
		return refusal{}, true, p.stop(ctx, failed)
	}
	no, err = p.targetUnfit(ctx, failed.Spec)
	return no, false, err
}

// ageUnfit returns why failed is outside the age bound, or "": its Verified condition moved more than [MaxAge]
// ago, more than [MaxSkew] ahead of the analyzer's clock, or has no time. That time is written by the executor,
// another writer with its own clock, and compared here with the analyzer's: the bound is coarse on purpose (a
// day against clock skews of seconds) and only keeps old failures from being reverted at a start-up.
func (p *Proposer) ageUnfit(failed *remediationv1alpha1.RemediationRequest) string {
	verified := meta.FindStatusCondition(failed.Status.Conditions, remediationv1alpha1.ConditionVerified)
	if verified == nil || verified.LastTransitionTime.IsZero() {
		return "the request's Verified condition has no transition time"
	}
	age := p.now().Sub(verified.LastTransitionTime.Time)
	switch {
	case age > MaxAge:
		return "the request failed more than 24 hours ago; a person decides on it"
	case age < -MaxSkew:
		return "the request's Verified transition time is more than five minutes ahead of the analyzer's clock"
	}
	return ""
}

func (p *Proposer) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// incident reads the Incident ref names, live. It returns nil, with no error, when it is gone or has another
// UID: the revert would have nowhere to be recorded, so none is made.
func (p *Proposer) incident(ctx context.Context, ref remediationv1alpha1.IncidentRef) (
	*remediationv1alpha1.Incident, error) {
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
// Warning event on failed, so a person sees why nothing was proposed. A request outside the age bound gets the
// log only, so a start-up does not repeat the event for every old request.
func (p *Proposer) incidentGone(ctx context.Context, failed *remediationv1alpha1.RemediationRequest) {
	log.FromContext(ctx).Info("No revert: the failed request's Incident is gone",
		"incident", failed.Spec.IncidentRef.Name)
	if p.ageUnfit(failed) == "" {
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

// targetUnfit reads the target live and returns why no revert is proposed for it, or a zero refusal when it is
// there: its kind is not one the action can target, the API server does not serve the kind (KEDA uninstalled,
// for example), it is not found, has another UID (deleted and made again) or is being deleted. Any other failed
// read is an error for a retry, Forbidden included: an authorization webhook that fails also answers 403, so a
// refusal is retried with the controller's backoff, and the age bound ends it ([Proposer.unfit]).
func (p *Proposer) targetUnfit(ctx context.Context, spec remediationv1alpha1.RemediationRequestSpec) (refusal, error) {
	t := spec.TargetRef
	if fits, ok := targets[spec.Action.Type]; !ok || !fits(t.APIVersion, t.Kind) {
		return refusal{remediationv1alpha1.RevertReasonTargetKindUnsupported,
			"the target is of a kind the action cannot target"}, nil
	}
	u := &unstructured.Unstructured{}
	u.SetAPIVersion(t.APIVersion)
	u.SetKind(t.Kind)
	if err := p.Reader.Get(ctx, types.NamespacedName{Namespace: t.Namespace, Name: t.Name}, u); err != nil {
		switch {
		case apierrors.IsNotFound(err):
			return refusal{remediationv1alpha1.RevertReasonTargetGone, "the failed request's target is gone"}, nil
		case meta.IsNoMatchError(err):
			return refusal{remediationv1alpha1.RevertReasonTargetKindUnsupported,
				"the API server does not serve the failed request's target kind"}, nil
		}
		return refusal{}, fmt.Errorf("reading the target %s %s/%s: %w", t.Kind, t.Namespace, t.Name, err)
	}
	if remediationv1alpha1.UID(u.GetUID()) != t.UID {
		return refusal{remediationv1alpha1.RevertReasonTargetGone,
			"the failed request's target was replaced by another object of that name"}, nil
	}
	if u.GetDeletionTimestamp() != nil {
		return refusal{remediationv1alpha1.RevertReasonTargetGone, "the failed request's target is being deleted"}, nil
	}
	return refusal{}, nil
}

// errFull stops writePending when the Incident's list is full.
var errFull = errors.New("status.revertsProposed is full")

// writePending adds ref as Pending to the status.revertsProposed of failed's Incident, read live on each
// attempt, with the resourceVersion read; a conflict is tried again. decided is true when an entry for ref is
// there afterwards (written now or by an earlier pass) and is still Pending; false, with no error, when the
// Incident is gone, the entry is already Created or Abandoned, or the list is full ([Proposer.stop]).
func (p *Proposer) writePending(ctx context.Context, failed *remediationv1alpha1.RemediationRequest,
	ref remediationv1alpha1.RequestRef) (decided bool, err error) {
	gone := false
	err = retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		gone, decided = false, false
		inc, err := p.incident(ctx, failed.Spec.IncidentRef)
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
		inc, err := p.incident(ctx, failed.Spec.IncidentRef)
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

// create creates the revert of the request ref names, with spec. refused says why the revert can never be made,
// for the entry to move to Abandoned: an existing request of that name whose spec.reverts does not name the same
// request and UID (left as it is), or a create the API server refuses as invalid (the API error goes to the log,
// the event has a fixed sentence). An existing request with the same link is the revert made by an earlier pass.
// Any other failure is an error for a retry.
func (p *Proposer) create(ctx context.Context, ref remediationv1alpha1.RequestRef,
	spec remediationv1alpha1.RemediationRequestSpec) (refused refusal, err error) {
	name := revertName(ref)
	rr := &remediationv1alpha1.RemediationRequest{
		ObjectMeta: metav1.ObjectMeta{Namespace: p.Namespace, Name: name},
		Spec:       *spec.DeepCopy(),
	}
	err = p.Client.Create(ctx, rr, client.FieldOwner(FieldOwner))
	switch {
	case err == nil:
		return refusal{}, nil
	case apierrors.IsInvalid(err):
		log.FromContext(ctx).Error(err, "The API server refused the revert as invalid", "revert", name)
		return refusal{remediationv1alpha1.RevertReasonCreateInvalid,
			"the API server refused the revert as invalid; the analyzer's log has the error"}, nil
	case !apierrors.IsAlreadyExists(err):
		return refusal{}, fmt.Errorf("creating the revert %s: %w", name, err)
	}
	made, taken, err := p.existing(ctx, ref)
	switch {
	case err != nil:
		return refusal{}, err
	case !made && taken.reason == "":
		// Gone again since the create: the next pass creates it.
		return refusal{}, fmt.Errorf("the revert %s was deleted right after the create said it exists", name)
	}
	return taken, nil
}

// existing reads, live, the request at the revert name of ref. made is true when it is there and its
// spec.reverts names ref (name and UID): the revert exists. taken is the refusal NameTaken when a request is
// there with any other link, or none. Neither, with no error, is a name that is free. A failed read is an error
// for a retry.
func (p *Proposer) existing(ctx context.Context, ref remediationv1alpha1.RequestRef) (made bool, taken refusal,
	err error) {
	name := revertName(ref)
	rr := &remediationv1alpha1.RemediationRequest{}
	if err := p.Reader.Get(ctx, types.NamespacedName{Namespace: p.Namespace, Name: name}, rr); err != nil {
		if apierrors.IsNotFound(err) {
			return false, refusal{}, nil
		}
		return false, refusal{}, fmt.Errorf("reading the revert %s: %w", name, err)
	}
	if rr.Spec.Reverts == nil || *rr.Spec.Reverts != ref {
		return false, refusal{remediationv1alpha1.RevertReasonNameTaken, fmt.Sprintf("RemediationRequest %s, "+
			"the revert's name, exists and does not revert this request; it is left as it is", name)}, nil
	}
	return true, refusal{}, nil
}

// abandon moves the entry of ref in the status.revertsProposed of the Incident incRef names from Pending to
// Abandoned with the reason of no, read live on each attempt, with the resourceVersion read; a conflict is tried
// again. When this call made the move it logs why and writes one Warning event on eventOn, or on the Incident
// when eventOn is nil. An Incident gone or with another UID, or an entry missing or no longer Pending, needs no
// write.
func (p *Proposer) abandon(ctx context.Context, incRef remediationv1alpha1.IncidentRef,
	ref remediationv1alpha1.RequestRef, no refusal, eventOn runtime.Object) error {
	var moved *remediationv1alpha1.Incident
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		moved = nil
		inc, err := p.incident(ctx, incRef)
		if err != nil || inc == nil || !pending(inc, ref) {
			return err
		}
		e := Entry(inc, ref)
		e.State, e.Reason = remediationv1alpha1.RevertAbandoned, no.reason
		if err := p.Client.Status().Update(ctx, inc, client.FieldOwner(FieldOwner)); err != nil {
			return err
		}
		moved = inc
		return nil
	})
	if err != nil {
		return fmt.Errorf("recording in Incident %s that %s gets no revert: %w", incRef.Name, ref.Name, err)
	}
	if moved == nil {
		return nil
	}
	log.FromContext(ctx).Info("Abandoned the revert of a failed request: "+no.why, "request", ref.Name,
		"incident", incRef.Name, "reason", no.reason)
	if eventOn == nil {
		eventOn = moved
	}
	p.event(eventOn, EventAbandoned, "The revert of %s is abandoned and will not be made (%s): %s", ref.Name,
		no.reason, no.why)
	return nil
}

// settle ends the Pending entry of ref, whose failed request is gone: the revert's name is read first, and a
// revert there that names ref moves the entry to Created (the create happened, only its record was lost);
// otherwise the entry moves to Abandoned with no. A failed read or write is an error for a retry.
func (p *Proposer) settle(ctx context.Context, incRef remediationv1alpha1.IncidentRef,
	ref remediationv1alpha1.RequestRef, no refusal, eventOn runtime.Object) error {
	made, _, err := p.existing(ctx, ref)
	switch {
	case err != nil:
		return err
	case made:
		return p.markCreated(ctx, incRef, ref)
	}
	return p.abandon(ctx, incRef, ref, no, eventOn)
}

// settleDeleted settles the Pending entries of every Incident of the operator namespace, listed live, after the
// request name was deleted ([Proposer.settleIncident]): the revert of a request that no longer exists would fail
// the executor's link check. A failed list, read or write is an error for a retry; an Incident that fails does
// not stop the others, and the first error is returned at the end.
func (p *Proposer) settleDeleted(ctx context.Context, name string) error {
	list := &remediationv1alpha1.IncidentList{}
	if err := p.Reader.List(ctx, list, client.InNamespace(p.Namespace)); err != nil {
		return fmt.Errorf("listing Incidents for the deleted RemediationRequest %s: %w", name, err)
	}
	var first error
	for i := range list.Items {
		if !hasPending(&list.Items[i]) {
			continue
		}
		if err := p.settleIncident(ctx, list.Items[i].Name); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// settleIncident settles the Pending entries of the Incident name ([Proposer.settlePending]) under its lock
// ([incidentLocks]), read live once the lock is held. An Incident gone needs nothing.
func (p *Proposer) settleIncident(ctx context.Context, name string) error {
	key := types.NamespacedName{Namespace: p.Namespace, Name: name}
	unlock, err := p.locks.lock(ctx, key)
	if err != nil {
		return err
	}
	defer unlock()
	inc := &remediationv1alpha1.Incident{}
	if err := p.Reader.Get(ctx, key, inc); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("reading Incident %s: %w", name, err)
	}
	return p.settlePending(ctx, inc)
}

// hasPending reports whether inc has a Pending entry.
func hasPending(inc *remediationv1alpha1.Incident) bool {
	return slices.ContainsFunc(inc.Status.RevertsProposed, func(e remediationv1alpha1.ProposedRevert) bool {
		return e.State == remediationv1alpha1.RevertPending
	})
}

// settlePending settles each Pending entry of inc for which no pass of the request will ever come
// ([Proposer.settle], the Warning event on the Incident): the failed request is gone or being deleted, or its
// name now belongs to a request with another UID (reason RequestGone: it was deleted while the analyzer was down
// or not leading, or a new request took its name); or the request is there with its UID but is no longer
// [Revertible] or its action cannot be inverted (reason NotRevertible: the request's pass returns without a
// revert and settles nothing). An entry whose request is there, Revertible, with an action that inverts, is left
// to the request's own pass. Each request is read live; a failed read or write is an error for a retry, which
// does not stop the entries after it: the first error is returned at the end. The caller holds the Incident's
// lock ([Proposer.settleIncident]).
func (p *Proposer) settlePending(ctx context.Context, inc *remediationv1alpha1.Incident) error {
	incRef := remediationv1alpha1.IncidentRef{Name: inc.Name, UID: remediationv1alpha1.UID(inc.UID)}
	var first error
	for _, e := range slices.Clone(inc.Status.RevertsProposed) {
		if e.State != remediationv1alpha1.RevertPending {
			continue
		}
		no, err := p.orphaned(ctx, e.RequestRef)
		if err == nil && no.reason != "" {
			err = p.settle(ctx, incRef, e.RequestRef, no, nil)
		}
		if err != nil && first == nil {
			first = err
		}
	}
	return first
}

// orphaned reads, live, the request of a Pending entry ref and returns why no pass of that request will finish
// the entry, or a zero refusal when its own pass will. A failed read is an error for a retry.
func (p *Proposer) orphaned(ctx context.Context, ref remediationv1alpha1.RequestRef) (refusal, error) {
	rr := &remediationv1alpha1.RemediationRequest{}
	err := p.Reader.Get(ctx, types.NamespacedName{Namespace: p.Namespace, Name: ref.Name}, rr)
	switch {
	case apierrors.IsNotFound(err):
		return refusal{remediationv1alpha1.RevertReasonRequestGone, "the failed request was deleted"}, nil
	case err != nil:
		return refusal{}, fmt.Errorf("reading RemediationRequest %s of a Pending revert: %w", ref.Name, err)
	case remediationv1alpha1.UID(rr.UID) != ref.UID:
		return refusal{remediationv1alpha1.RevertReasonRequestGone,
			"the failed request's name now belongs to another request"}, nil
	case rr.DeletionTimestamp != nil:
		return refusal{remediationv1alpha1.RevertReasonRequestGone, "the failed request is being deleted"}, nil
	case !Revertible(rr):
		return refusal{remediationv1alpha1.RevertReasonNotRevertible,
			"the failed request no longer ended Failed with Verified False"}, nil
	}
	if _, ok := Spec(rr); !ok {
		return refusal{remediationv1alpha1.RevertReasonNotRevertible,
			"the failed request's action cannot be inverted"}, nil
	}
	return refusal{}, nil
}

// reconcileIncident settles the Pending entries of one Incident ([Proposer.settleIncident]). The controller that
// calls it watches the Incidents of the operator namespace that have a Pending entry, so it runs for each of them
// when this analyzer starts leading (the informer's first list) and whenever one changes.
func (p *Proposer) reconcileIncident(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	if req.Namespace != p.Namespace {
		return reconcile.Result{}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, reconcileTimeout)
	defer cancel()
	return reconcile.Result{}, p.settleIncident(ctx, req.Name)
}

// markCreated moves the entry of ref in the status.revertsProposed of the Incident incRef names from Pending to
// Created, read live on each attempt, with the resourceVersion read; a conflict is tried again. Every caller has
// seen or made the revert. An Incident gone or with another UID meanwhile, or an entry already Created, needs no
// write. An entry already Abandoned is a final record that a revert which exists was not made: that is not
// retried (Abandoned is final), but logged as an error with a Warning event on the Incident, so a person sees it.
func (p *Proposer) markCreated(ctx context.Context, incRef remediationv1alpha1.IncidentRef,
	ref remediationv1alpha1.RequestRef) error {
	var wrong *remediationv1alpha1.Incident
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		wrong = nil
		inc, err := p.incident(ctx, incRef)
		if err != nil || inc == nil {
			return err
		}
		e := Entry(inc, ref)
		if e == nil {
			// Entries are never removed (CEL), so this cannot follow a Pending write; retrying would not
			// bring the entry back.
			return reconcile.TerminalError(fmt.Errorf("incident %s has no entry for %s", inc.Name, ref.Name))
		}
		switch e.State {
		case remediationv1alpha1.RevertPending:
		case remediationv1alpha1.RevertAbandoned:
			wrong = inc
			return nil
		default:
			return nil
		}
		e.State = remediationv1alpha1.RevertCreated
		return p.Client.Status().Update(ctx, inc, client.FieldOwner(FieldOwner))
	})
	if err != nil {
		return fmt.Errorf("recording the revert of %s in its Incident: %w", ref.Name, err)
	}
	if wrong != nil {
		log.FromContext(ctx).Error(errAbandonedButMade, "The revert of a failed request exists but its entry is "+
			"Abandoned", "request", ref.Name, "revert", revertName(ref), "incident", incRef.Name)
		p.event(wrong, EventAbandonedButMade, "The revert %s of %s exists, but status.revertsProposed records it as "+
			"Abandoned; a person decides on the revert", revertName(ref), ref.Name)
	}
	return nil
}

// errAbandonedButMade is the error logged when a revert exists while its entry is Abandoned.
var errAbandonedButMade = errors.New("the revert exists and its entry is Abandoned")
