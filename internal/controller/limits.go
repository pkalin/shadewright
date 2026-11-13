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
	"slices"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
)

// The executor's limits (design sections 6 and 10.7, #88): a lock per target, a cap on active requests and a
// circuit breaker. They hold back the step that lets a request through the policy check (Pending to
// PolicyChecked), the step after which a request writes to Git. A request that is already past it is never
// stopped by them; the kill switch (killswitch.go) is what stops writes at once.
//
// A request is active while the executor can still move it on before the merge (progresses: a phase before the
// merge with a plan digest), and while it is being deleted and still holds the finalizer, since its pull
// request may still be open until the finalizer is gone (one that ended Denied, Cancelled or Superseded, holds
// no lock and is not a probe is not read, so it is not counted). The lock is held longer than that (targetlock.go):
// from the allowed decision until the request's pull request is known closed or merged, so a request that
// stopped with its pull request still open keeps a second pull request off the target. Before the policy check
// writes an allowed decision it counts the active requests and reads the outcomes of the others, straight from
// the API server (never the cache, which may not show a request let through a moment ago), and lets the
// request through only when
//
//   - no other request that holds the lock names the same target UID (the lock),
//   - fewer than maxConcurrentRemediations of RemediationSettings are active (the cap), and
//   - the breaker lets it through: it is closed, or it is half-open and no probe is running (the request
//     becomes the probe then).
//
// The breaker trips when BreakerThreshold failures in a row (after the latest success) are among the
// requests that went through the policy check. Once tripped it stays so until a success, and it is
//
//   - open (CircuitOpen) for BreakerCooldown after the latest failure, or after the latest probe that ended
//     without a success or a failure (Expired, Cancelled, Superseded, Denied): a probe ends the half-open
//     state only with a success, anything else opens the breaker again for a full cool-down (fail closed);
//   - half-open after that: one request at a time goes through as the probe, and the others wait
//     (CircuitHalfOpen) while it runs. Its success (Merged or Succeeded) closes the breaker, since the failures
//     in a row are counted after the latest success; its failure is the latest failure, so the breaker is open
//     again for a full cool-down. WaitingForDelivery and Verifying are undecided (undecidedPhases): neither a
//     success nor a failure, and a probe in one of them stays the probe until it ends. With VerifyOutcomes on,
//     so is Merged with a merge receipt, on its way to WaitingForDelivery (verificationPending).
//
// The probe is marked: the policy check sets status.breakerProbe on a request it lets through while the
// breaker is tripped, in the same status write as the allowed decision, under the mutex below, so a conflict
// on that write lets nothing through and marks nothing. The breaker finds probes by that field only, never by
// comparing times written by different clocks, so leaders with skewed clocks agree on the probe. A request
// without the field (one let through before the trip, or before the field existed) is not a probe. A probe
// runs (holds the others back) until it reaches a final phase or Merged, whether or not it is being deleted;
// once its finalizer is gone it is no longer listed, so a probe that ended without a success and was then
// deleted adds no further cool-down (the next request is the new single probe, so nothing more goes through). One that ended without lastPhaseTransitionTime (written
// by hand) is taken to have ended one cool-down after the trip, the earliest it could have been let through,
// so it opens the breaker again for one more cool-down at most (bounded, fail closed). The field is never
// cleared, so a probe of an earlier trip that still runs after the breaker closed holds back the probes of
// the next trip too, and one that ended later than the failures of the next trip opens it again from its
// end: both only hold more requests back.
//
// The count and the status write that lets a request through run under one mutex, so two workers can never
// both take the last slot or both become the probe. All state is read from the requests themselves (phase,
// lastPhaseTransitionTime, breakerProbe), so a restart or a new leader neither forgets an open breaker or a
// running probe nor frees a slot (invariant 5). It fails closed: when the settings
// or the requests cannot be read, or there is no API server reader, no request is let through.
//
// The requests are listed with field selectors on status.phase, status.holdsTarget and status.breakerCounts
// (#293, #390, see limitsSelectors), so a request that ended, released or never took the lock and whose outcome
// the breaker no longer needs (breakercounts.go) is never read, and each list is read in pages of
// limitsPageSize. All lists are read within the check's deadline (CheckTimeout); one that runs out lets nothing
// through.
//
// Only a request the policy check allows is held back: refusals (Denied, NeedsIntervention) are written at
// once. A request held back keeps no phase (or Pending), gets the condition Queued with the limit as the
// reason, and is checked again after admissionRecheckDelay (heldRecheckInterval behind a lock held by a request
// that stopped, which wakes it when it changes). While it is Queued only the limits are checked, so
// it does not read Git; once they let it through, Queued is removed and the whole policy check runs again.
//
// The limits are counted per namespace: requests live in the operator namespace. The lock is per target
// object, not per file in Git; two targets declared in one file are kept apart by the plan digest and the
// freshness check, which supersede the second plan when the first one changes the file.

// Reasons of the Queued condition. The metrics (limits_metrics.go) map them to the values of a label.
const (
	// ReasonTargetLocked: another request that holds the lock (holdsLock) names the same target UID.
	ReasonTargetLocked = "TargetLocked"
	// ReasonConcurrencyLimit: maxConcurrentRemediations requests are active.
	ReasonConcurrencyLimit = "ConcurrencyLimitReached"
	// ReasonCircuitOpen: the circuit breaker is open after consecutive failures.
	ReasonCircuitOpen = "CircuitOpen"
	// ReasonCircuitHalfOpen: the circuit breaker is half-open and another request runs as its probe.
	ReasonCircuitHalfOpen = "CircuitHalfOpen"
	// ReasonLimitsUnknown: the limits could not be read, so the request is held back (fail closed). It is
	// counted in the metrics only; the request is retried with the error backoff and gets no condition.
	ReasonLimitsUnknown = "LimitsUnknown"
)

// Fixed messages of the Queued condition.
var queuedMessages = map[string]string{
	ReasonTargetLocked: "Another request changes the same target, or its pull request may still be open; this one " +
		"waits until that one ends and its pull request is closed or merged",
	ReasonConcurrencyLimit: "RemediationSettings default allows no more active requests (maxConcurrentRemediations); this one waits for a slot",
	ReasonCircuitOpen: "The circuit breaker is open after consecutive failed requests; this one waits for the " +
		"cool-down to end",
	ReasonCircuitHalfOpen: "The circuit breaker is half-open after its cool-down and another request runs as its " +
		"probe; this one waits until the probe ends",
}

// Defaults of the circuit breaker.
const (
	// DefaultBreakerThreshold is how many consecutive failures open the breaker.
	DefaultBreakerThreshold = 3
	// DefaultBreakerCooldown is how long the breaker stays open after the latest of those failures.
	DefaultBreakerCooldown = 30 * time.Minute
)

// admissionRecheckDelay is how long a request held back by a limit waits before it is checked again.
const admissionRecheckDelay = 30 * time.Second

// errSettingsMissing means RemediationSettings default does not exist. The policy check denies such a request,
// so the check before it lets the request go on to be denied; the check after it holds the request back.
var errSettingsMissing = errors.New("RemediationSettings default does not exist")

// circuitState is the state of the circuit breaker.
type circuitState int32

const (
	circuitClosed circuitState = iota
	circuitOpen
	circuitHalfOpen
)

// limitsState is what one check of the limits found.
type limitsState struct {
	// reason is empty when the request may go through, else the reason of the Queued condition.
	reason   string
	active   int
	limit    int
	circuit  circuitState
	failures int
	// probing is whether a probe runs while the breaker is half-open.
	probing bool
	// stoppedLock is whether every request that holds the lock on the target has stopped (it is not moved on
	// or deleted): the lock then lasts until a person or a look-up of checkHeld frees it, so the request
	// waits longer between checks and is woken by lockWaiters when the holder changes.
	stoppedLock bool
	// holders are the requests that hold the lock on the target, those for rr's field first, then by name, at
	// most MaxQueuedBehind, whatever
	// reason holds the request back: what waitForLimits records in status.queuedBehind (duplicates.go, #643).
	holders []remediationv1alpha1.RequestRef
	// clear is what the check found the breaker no longer needs (staleOutcomes), cleared after the decision.
	clear clearing
	// endedDeleting are the active requests, other than the one checked, that ended and are being deleted
	// (waitForCapOnDelete).
	endedDeleting []types.UID
}

// recheckDelay is how long a request held back by s waits before it checks the limits again.
func (s limitsState) recheckDelay() time.Duration {
	if s.reason == ReasonTargetLocked && s.stoppedLock {
		return heldRecheckInterval
	}
	return admissionRecheckDelay
}

// probe reports whether a request the limits let through becomes the breaker's probe: it does when the breaker
// is half-open (an open breaker lets nothing through, a closed one lets requests through as no probe).
func (s limitsState) probe() bool {
	return s.reason == "" && s.circuit == circuitHalfOpen
}

// progresses reports whether the executor can still move a request on before the merge (recheck): a phase
// before the merge, with the plan digest the policy check fixes. Phases after the merge are not built, and a
// request in one stops there today.
func progresses(rr *remediationv1alpha1.RemediationRequest) bool {
	return beforeMerge(rr.Status.Phase) && rr.Status.PlanDigest != ""
}

// active reports whether a request counts against the cap: it progresses, or it is being deleted and still
// holds the finalizer, and that finalizer may still write to Git (deleteMayWrite). A request that ended in
// another phase and released the lock has a pull request known merged or closed (or none of its own), and its
// finalizer writes nothing to Git, so it is not counted, whichever list read it.
func active(rr *remediationv1alpha1.RemediationRequest) bool {
	if !rr.DeletionTimestamp.IsZero() {
		return controllerutil.ContainsFinalizer(rr, FinalizerName) && (!ended(rr.Status.Phase) || deleteMayWrite(rr))
	}
	return progresses(rr)
}

// deleteMayWrite reports whether the finalizer of a request that ended may write to Git: it closes the pull
// request of a phase that may have left one open (closeOnDelete: NeedsIntervention, Expired) and of one that
// holds the lock (finalize). Such a request is read while it is being deleted: by the lock's list when it holds
// the lock, else by the breaker's list once its finalizer marked it (markDeleting), which it does before the
// close.
func deleteMayWrite(rr *remediationv1alpha1.RemediationRequest) bool {
	return closeOnDelete(rr.Status.Phase) || holdsLock(rr)
}

// Outcomes of a request for the circuit breaker.
type outcomeClass int

const (
	outcomeNone outcomeClass = iota
	outcomeSuccess
	outcomeFailure
)

// breakerOutcome classifies a request for the breaker. Merged and Succeeded are successes: the change merged, and
// a failure found later moves the request to Failed with a later transition time. WaitingForDelivery and
// Verifying are undecided, neither a success nor a failure: the move to them refreshes
// lastPhaseTransitionTime, so a merge older than later failures would otherwise read as newer. Failed and
// NeedsIntervention (a pull request closed without a merge included) are failures. Expired is neither: it
// comes from Git host outages and deadlines, not from a bad change. Denied, Cancelled and Superseded say
// nothing about whether the changes work, and a request without a plan digest never got through the policy
// check, so neither counts. Nor does a request without lastPhaseTransitionTime (one that ended before the
// field existed): it cannot be ordered. A request in Merged that carries the NoMergeReceipt mark (merge.go) is
// read like the Inconclusive it ends in, never as a success. A revert (spec.reverts) is never a success, in any
// phase and whatever VerifyOutcomes says; its failures count. The limits read a request that is still undecided
// (verificationPending, which depends on VerifyOutcomes) as no outcome over this.
func breakerOutcome(rr *remediationv1alpha1.RemediationRequest) outcomeClass {
	if rr.Status.PlanDigest == "" || rr.Status.LastPhaseTransitionTime == nil {
		return outcomeNone
	}
	switch {
	case noReceiptMerge(rr):
		// Merged without a receipt: it ends Inconclusive at the next reconcile, and is read as that already.
		return outcomeNone
	case remediationv1alpha1.IsRevert(rr) && slices.Contains(successPhases, rr.Status.Phase):
		// A revert is never a success (ADR-0030 section 8): it clears nothing, so the failure it reverts stays
		// counted. With --verify-outcomes off it stays Merged, which would otherwise read as one.
		return outcomeNone
	case slices.Contains(successPhases, rr.Status.Phase):
		return outcomeSuccess
	case slices.Contains(failurePhases, rr.Status.Phase):
		return outcomeFailure
	}
	return outcomeNone
}

func (r *RemediationRequestReconciler) breakerThreshold() int {
	if r.BreakerThreshold > 0 {
		return r.BreakerThreshold
	}
	return DefaultBreakerThreshold
}

func (r *RemediationRequestReconciler) breakerCooldown() time.Duration {
	if r.BreakerCooldown > 0 {
		return r.BreakerCooldown
	}
	return DefaultBreakerCooldown
}

// checkLimits reads the settings and the requests in rr's namespace from the API server and decides whether rr
// may go through the policy check now. It returns errSettingsMissing when the settings do not exist, and any
// other read error as it is; the caller must then not let the request through. Without an APIReader it
// refuses too: like the kill switch, it never falls back to the cache.
func (r *RemediationRequestReconciler) checkLimits(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) (
	limitsState, error) {
	if r.APIReader == nil {
		return limitsState{}, errors.New("cannot check the limits: no API server reader")
	}
	settings := &remediationv1alpha1.RemediationSettings{}
	switch err := r.APIReader.Get(ctx, types.NamespacedName{Name: remediationv1alpha1.SettingsName}, settings); {
	case apierrors.IsNotFound(err):
		return limitsState{}, errSettingsMissing
	case err != nil:
		return limitsState{}, fmt.Errorf("get RemediationSettings %s: %w", remediationv1alpha1.SettingsName, err)
	}
	// An object written before the field had a default, or by hand, may hold 0: the smallest limit the schema
	// allows is used then, never no limit.
	s := limitsState{limit: max(int(settings.Spec.MaxConcurrentRemediations), 1)}
	seen, err := r.readLimits(ctx, rr.Namespace)
	if err != nil {
		return limitsState{}, err
	}
	locked, lockMoving := false, false
	var holders []lockHolder
	var lastSuccess, lastFailure time.Time
	var failures []time.Time
	var probes []remediationv1alpha1.RemediationRequest
	// A replacement takes over the target lock and the slot of the request it replaces (ADR-0028 section 4):
	// that one counts for neither, so the chain counts once, not twice. Its link was checked before (chainLink);
	// a link that does not hold is denied, which takes nothing.
	predecessor := predecessorOf(rr)
	for uid, e := range seen {
		if uid == rr.UID {
			continue
		}
		if e.probe != nil {
			probes = append(probes, *e.probe)
		}
		if e.active && uid != predecessor {
			s.active++
			if e.deleting && e.ended {
				s.endedDeleting = append(s.endedDeleting, uid)
			}
		}
		if e.target == rr.Spec.TargetRef.UID && e.lock && uid != predecessor {
			locked = true
			lockMoving = lockMoving || e.active
			holders = append(holders, lockHolder{ref: remediationv1alpha1.RequestRef{Name: e.name, UID: remediationv1alpha1.UID(uid)},
				same: e.sameField(ctx, rr)})
		}
		switch e.outcome {
		case outcomeSuccess:
			if e.at.After(lastSuccess) {
				lastSuccess = e.at
			}
		case outcomeFailure:
			failures = append(failures, e.at)
			if e.at.After(lastFailure) {
				lastFailure = e.at
			}
		}
	}
	s.active -= chainPairs(seen, rr.UID, predecessor)
	s.holders = recordedHolders(holders)
	// The failures in a row are those after the latest success; one at the very same time counts as a failure.
	inRow := make([]time.Time, 0, len(failures))
	for _, t := range failures {
		if !t.Before(lastSuccess) {
			inRow = append(inRow, t)
		}
	}
	s.failures = len(inRow)
	s.circuit, s.probing = r.breaker(inRow, lastFailure, probes)
	s.clear = staleOutcomes(seen)
	switch {
	case s.circuit == circuitOpen:
		s.reason = ReasonCircuitOpen
	case s.circuit == circuitHalfOpen && s.probing:
		s.reason = ReasonCircuitHalfOpen
	case locked:
		s.reason = ReasonTargetLocked
		s.stoppedLock = !lockMoving
	case s.active >= s.limit:
		s.reason = ReasonConcurrencyLimit
	}
	return s, nil
}

// limitsPageSize is the most RemediationRequests one list call of the limits check asks the API server for.
const limitsPageSize = 100

// Field selectors of RemediationRequest: the selectable fields of the CRD (see the markers on the type).
const (
	phaseField  = "status.phase"
	probeField  = "status.breakerProbe"
	lockField   = "status.holdsTarget"
	countsField = "status.breakerCounts"
)

// limitsEntry is what the limits need of one request, so a check keeps no whole requests in memory except the
// breaker's probes.
type limitsEntry struct {
	active bool
	// lock is holdsLock.
	lock    bool
	target  remediationv1alpha1.UID
	outcome outcomeClass
	// at is lastPhaseTransitionTime, zero when the request has none.
	at time.Time
	// probe is the request when status.breakerProbe is set.
	probe *remediationv1alpha1.RemediationRequest
	// counts is status.breakerCounts true; name and resourceVersion are what clearStale writes with.
	counts          bool
	name            string
	resourceVersion string
	uid             types.UID
	// final is a request in Succeeded, the only success that clears older outcomes (staleOutcomes).
	final bool
	// deleting is a request being deleted; ended is a request in one of endedPhases.
	deleting bool
	ended    bool
	// pending is a request whose outcome the breaker does not know yet (verificationPending).
	pending bool
	// replaces is the UID of the request a replacement takes over (spec.replaces), set only once the executor
	// admitted the replacement with its checked chain link (admittedPredecessor); empty for any other, so a
	// spec.replaces nobody checked never frees a slot.
	replaces types.UID
	// fix is the request's spec.action and spec.targetRef, kept only for a request that holds a target lock:
	// checkLimits records first the holders that change the waiter's field (sameField, #643).
	fix *remediationv1alpha1.RemediationRequest
}

// sameField reports whether e holds a target lock for the field rr changes (sameField in duplicates.go).
func (e limitsEntry) sameField(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) bool {
	return e.fix != nil && sameField(ctx, e.fix, rr)
}

// lockHolder is a request that holds the lock on the target of the request checked, and whether it changes
// the same field.
type lockHolder struct {
	ref  remediationv1alpha1.RequestRef
	same bool
}

// recordedHolders returns the holders status.queuedBehind records, at most MaxQueuedBehind: those for the
// checked request's field first, since only their pull request closed without a merge ends it, then by name,
// so the cut is the same on every check (#643).
func recordedHolders(holders []lockHolder) []remediationv1alpha1.RequestRef {
	slices.SortFunc(holders, func(a, b lockHolder) int {
		if a.same != b.same {
			if a.same {
				return -1
			}
			return 1
		}
		return strings.Compare(a.ref.Name, b.ref.Name)
	})
	if len(holders) == 0 {
		return nil
	}
	holders = holders[:min(len(holders), remediationv1alpha1.MaxQueuedBehind)]
	refs := make([]remediationv1alpha1.RequestRef, 0, len(holders))
	for _, h := range holders {
		refs = append(refs, h.ref)
	}
	return refs
}

// admittedPredecessor returns the UID of the request o takes over when the executor admitted o as its
// replacement: spec.replaces, matched by the status.handoff.from that only the admission writes, while it is
// Reserved or Done. Anything else, an unchecked spec.replaces or an aborted handoff, returns empty.
func admittedPredecessor(o *remediationv1alpha1.RemediationRequest) types.UID {
	rep, ho := o.Spec.Replaces, o.Status.Handoff
	if rep == nil || ho == nil || ho.From == nil || ho.From.UID != rep.UID ||
		(ho.From.Phase != remediationv1alpha1.HandoffReserved && ho.From.Phase != remediationv1alpha1.HandoffDone) {
		return ""
	}
	return types.UID(rep.UID)
}

// predecessorOf returns the UID of the request rr replaces, empty when it replaces none.
func predecessorOf(rr *remediationv1alpha1.RemediationRequest) types.UID {
	if rr.Spec.Replaces == nil {
		return ""
	}
	return types.UID(rr.Spec.Replaces.UID)
}

// chainPairs counts the active requests, other than self and predecessor, that an active replacement takes over:
// a request and its replacement are one chain, counted once against the cap.
func chainPairs(seen map[types.UID]limitsEntry, self, predecessor types.UID) int {
	replaced := map[types.UID]bool{}
	for _, e := range seen {
		if e.active && e.replaces != "" {
			replaced[e.replaces] = true
		}
	}
	n := 0
	for uid := range replaced {
		if e, ok := seen[uid]; ok && e.active && uid != self && uid != predecessor {
			n++
		}
	}
	return n
}

// neutralProbe reports whether the entry is a probe that ended without a success or a failure.
func (e limitsEntry) neutralProbe() bool {
	return e.probe != nil && e.outcome == outcomeNone && ended(e.probe.Status.Phase) && !e.pending
}

// limitsSelectors are the three lists the limits read (#293, #390), in this order:
//
//   - every request whose phase is not one of endedPhases: no phase and Pending, every phase the executor
//     still moves on, and any phase added later or written by hand (an exclusion list, so a new phase is read
//     until it is classified). This is one paged list, one consistent snapshot: the cap and the requests the
//     executor moves on (which hold the lock whatever status.holdsTarget says) come from the same moment;
//   - every request whose status.holdsTarget is not false, in any phase: the holders of the lock (holdsLock),
//     a stopped one included, also one whose phase was set to Denied by hand, and those that do not have the
//     field yet (a missing field is the empty value, which is not false). The executor writes the field on
//     every request it decides (true with the allowed decision, false with a refusal) and once on those
//     written before it existed (backfill), so after that pass only undecided requests lack it;
//   - every request whose status.breakerCounts is not false (breakercounts.go): the outcomes the breaker still
//     needs (the latest request in Succeeded and every outcome after it), every probe still running, every
//     request being deleted whose finalizer may close a pull request (markDeleting), and every request not
//     classified yet (one that has not ended, or ended before the field existed: a missing field is not false,
//     so it is read).
//
// A request that ended, holds no lock (status.holdsTarget false) and whose outcome the breaker does not need
// (status.breakerCounts false) is never read: it is not active (not counted against the cap while it is being
// deleted, see active), not an outcome the breaker needs and not a holder. So the reads grow with the requests
// that still run, hold the lock or are not classified yet, not with the history of ended ones.
//
// The lock and breaker lists are read after the first; a later read of a request replaces an earlier one, so a
// request seen twice counts in its newer state, and one that left the first list in between is still read
// when it holds the lock or the breaker needs it.
func limitsSelectors() []fields.Selector {
	read := make([]fields.Selector, 0, len(endedPhases))
	for _, p := range endedPhases {
		read = append(read, fields.OneTermNotEqualSelector(phaseField, string(p)))
	}
	return []fields.Selector{fields.AndSelectors(read...), fields.OneTermNotEqualSelector(lockField, "false"),
		fields.OneTermNotEqualSelector(countsField, "false")}
}

// readLimits reads the requests of namespace that the limits need (limitsSelectors), each list from the API
// server in pages, and returns what the limits need of each by UID. Any error is returned (fail closed).
func (r *RemediationRequestReconciler) readLimits(ctx context.Context, namespace string) (
	map[types.UID]limitsEntry, error) {
	seen := map[types.UID]limitsEntry{}
	for _, sel := range limitsSelectors() {
		err := r.listPages(ctx, namespace, sel, func(o *remediationv1alpha1.RemediationRequest) {
			e := limitsEntry{active: active(o), lock: holdsLock(o), target: o.Spec.TargetRef.UID,
				outcome: breakerOutcome(o), counts: ptr.Deref(o.Status.BreakerCounts, false), name: o.Name,
				resourceVersion: o.ResourceVersion, uid: o.UID, deleting: !o.DeletionTimestamp.IsZero(),
				ended: ended(o.Status.Phase), pending: r.verificationPending(o)}
			if e.pending {
				e.outcome = outcomeNone
			}
			e.replaces = admittedPredecessor(o)
			if e.lock {
				e.fix = &remediationv1alpha1.RemediationRequest{Spec: remediationv1alpha1.RemediationRequestSpec{
					Action: *o.Spec.Action.DeepCopy(), TargetRef: o.Spec.TargetRef}}
			}
			e.final = e.outcome == outcomeSuccess && o.Status.Phase == remediationv1alpha1.PhaseSucceeded
			if o.Status.LastPhaseTransitionTime != nil {
				e.at = o.Status.LastPhaseTransitionTime.Time
			}
			if o.Status.BreakerProbe {
				// Every probe the breaker needs is in the last list, in its newest state, which replaces a copy
				// from an earlier list.
				e.probe = o.DeepCopy()
			}
			seen[o.UID] = e
		})
		if err != nil {
			return nil, err
		}
	}
	return seen, nil
}

// listPages lists the RemediationRequests in namespace that sel selects from the API server, one page of
// limitsPageSize at a time, and calls visit with each, so no response grows with the number of requests. The
// pages after the first continue from the first one's resourceVersion, so together they are one consistent
// snapshot. Any error, an expired continue token (the snapshot was compacted away) included, is returned and
// nothing more is visited; the caller then lets nothing through (fail closed).
func (r *RemediationRequestReconciler) listPages(ctx context.Context, namespace string, sel fields.Selector,
	visit func(*remediationv1alpha1.RemediationRequest)) error {
	cont := ""
	for {
		list := &remediationv1alpha1.RemediationRequestList{}
		if err := r.APIReader.List(ctx, list, client.InNamespace(namespace), client.MatchingFieldsSelector{Selector: sel},
			client.Limit(limitsPageSize), client.Continue(cont)); err != nil {
			return fmt.Errorf("list RemediationRequests in %s (%s): %w", namespace, sel, err)
		}
		for i := range list.Items {
			visit(&list.Items[i])
		}
		if cont = list.Continue; cont == "" {
			return nil
		}
	}
}

// breaker works out the state of the circuit breaker (see the top of this file) from inRow, the times of the
// failures in a row, lastFailure, the latest of them, and probes, the other requests of the namespace marked as
// probes. The probes are looked at only when the breaker is tripped. probing is whether a probe runs; it is set
// only when the breaker is half-open.
func (r *RemediationRequestReconciler) breaker(inRow []time.Time, lastFailure time.Time,
	probes []remediationv1alpha1.RemediationRequest) (state circuitState, probing bool) {
	threshold := r.breakerThreshold()
	if len(inRow) < threshold {
		return circuitClosed, false
	}
	slices.SortFunc(inRow, time.Time.Compare)
	tripped := inRow[threshold-1]
	reopened := lastFailure
	for i := range probes {
		p := &probes[i]
		if !ended(p.Status.Phase) || r.verificationPending(p) {
			// Still running, also while it is being deleted: until its finalizer is gone its pull request may
			// still be open and merge. A probe whose outcome is still undecided after the merge
			// (verificationPending) runs too.
			probing = true
			continue
		}
		if breakerOutcome(p) != outcomeNone {
			// A success closed the breaker unless failures came after it; a failure is counted in lastFailure.
			continue
		}
		// Ended without a success: the breaker opens again from then. Without a transition time (written by
		// hand) the end is taken as one cool-down after the trip, the earliest a probe could have been let
		// through, so the breaker is open for one more cool-down at most, not for ever.
		end := tripped.Add(r.breakerCooldown())
		if p.Status.LastPhaseTransitionTime != nil {
			end = p.Status.LastPhaseTransitionTime.Time
		}
		if end.After(reopened) {
			reopened = end
		}
	}
	if r.now().Before(reopened.Add(r.breakerCooldown())) {
		return circuitOpen, false
	}
	return circuitHalfOpen, probing
}

// ended reports whether phase is a final phase, or Merged or a later one, after which the executor no longer
// writes to Git for the request. Any other phase, an unknown one included, runs (fail closed). A probe in an
// undecided phase (undecided) has ended its Git writes but not its probe.
func ended(phase remediationv1alpha1.RemediationPhase) bool {
	return slices.Contains(endedPhases, phase)
}

// undecided reports whether phase is one of undecidedPhases: after the merge, with the outcome not known yet.
func undecided(phase remediationv1alpha1.RemediationPhase) bool {
	return slices.Contains(undecidedPhases, phase)
}

// verificationPending reports whether the breaker reads rr as undecided, neither a success nor a failure: in
// one of undecidedPhases, or, with VerifyOutcomes on, Merged with a merge receipt, which the next reconcile
// moves to WaitingForDelivery (afterReceipt). Read as a success in that gap, a probe that has just merged would
// close the breaker and let another request through as an ordinary one before its outcome is known. With
// VerifyOutcomes off such a request stays Merged, a success as before #681 (never a revert, breakerOutcome). A
// Merged request without a receipt is classified by breakerOutcome alone (noReceiptMerge).
// status.breakerCounts does not depend on the flag (countsForBreaker reads Merged as a success), so a request
// merged while the flag was on is still read when it is turned off.
func (r *RemediationRequestReconciler) verificationPending(rr *remediationv1alpha1.RemediationRequest) bool {
	return undecided(rr.Status.Phase) || (r.VerifyOutcomes && rr.Status.Phase == remediationv1alpha1.PhaseMerged &&
		rr.Status.MergeReceipt != nil)
}

// The classes of the phases after the policy check. Every phase of the API is Pending, a phase before the
// merge (beforeMerge) or in exactly one of these (TestEveryPhaseIsClassified).
var (
	// successPhases are the breaker's successes: the change merged.
	successPhases = []remediationv1alpha1.RemediationPhase{remediationv1alpha1.PhaseMerged,
		remediationv1alpha1.PhaseSucceeded}
	// undecidedPhases follow the merge while the executor verifies the outcome (ADR-0030 section 6): no success
	// and no failure for the breaker, and a probe in one of them still runs. They make no Git write.
	undecidedPhases = []remediationv1alpha1.RemediationPhase{remediationv1alpha1.PhaseWaitingForDelivery,
		remediationv1alpha1.PhaseVerifying}
	// failurePhases are the breaker's failures.
	failurePhases = []remediationv1alpha1.RemediationPhase{remediationv1alpha1.PhaseFailed,
		remediationv1alpha1.PhaseNeedsIntervention}
	// neutralPhases end a request without saying whether its change works.
	// Inconclusive (ADR-0030 section 5) is one: merged, and neither success nor failure could be shown, so it is
	// read like Expired (a probe that ends in it opens the breaker again for a full cool-down).
	neutralPhases = []remediationv1alpha1.RemediationPhase{remediationv1alpha1.PhaseExpired,
		remediationv1alpha1.PhaseDenied, remediationv1alpha1.PhaseCancelled, remediationv1alpha1.PhaseSuperseded,
		remediationv1alpha1.PhaseInconclusive}
	// endedPhases are the phases that end a probe (ended), and those the first list of the limits leaves out
	// (limitsSelectors).
	endedPhases = slices.Concat(successPhases, undecidedPhases, failurePhases, neutralPhases)
)

// waitForLimits holds back a request that a limit does not let through: it sets Queued True with the limit as
// the reason and, when the target is locked, records its holders in status.queuedBehind (#643; holdBack checked
// the recorded ones first), writes the status when that changed, and checks again after s.recheckDelay. A
// record is kept while the target is not locked. A wait for the target's state that the request was in before
// ends here (endWait): the policy check let it through, so a later wait is a new one with its own clock, not
// one that already lasted the whole time the request was Queued (#556).
func (r *RemediationRequestReconciler) waitForLimits(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	s limitsState) (reconcile.Result, error) {
	reason := s.reason
	queued := meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{
		Type:               remediationv1alpha1.ConditionQueued,
		Status:             metav1.ConditionTrue,
		Reason:             reason,
		Message:            queuedMessages[reason],
		ObservedGeneration: rr.Generation,
	})
	recorded := false
	if len(s.holders) > 0 && !slices.Equal(rr.Status.QueuedBehind, s.holders) {
		rr.Status.QueuedBehind = s.holders
		recorded = true
	}
	if endWait(&rr.Status.Conditions) || queued || recorded {
		if err := r.Status().Update(ctx, rr); err != nil {
			return reconcile.Result{}, fmt.Errorf("update status of RemediationRequest %s/%s: %w", rr.Namespace, rr.Name, err)
		}
		logf.FromContext(ctx).Info("Queued RemediationRequest behind a limit", "remediationRequest",
			client.ObjectKeyFromObject(rr), "reason", reason)
	}
	return reconcile.Result{RequeueAfter: s.recheckDelay()}, nil
}

// setPhase sets the request's phase and, when it changes, lastPhaseTransitionTime to now (and, for Merged with a
// recorded pull request, status.pullRequest.mergedAt once) and, when the request ended, status.breakerCounts
// (classifyBreaker). That reads the plan digest and status.breakerProbe, so a caller sets those first. A phase
// that ends the request also declines its Pending reviewer hints (declineOnEnd): every path that ends a request
// comes through here.
func (r *RemediationRequestReconciler) setPhase(rr *remediationv1alpha1.RemediationRequest,
	phase remediationv1alpha1.RemediationPhase) {
	declineOnEnd(rr, phase)
	if rr.Status.Phase == phase && rr.Status.LastPhaseTransitionTime != nil {
		return
	}
	rr.Status.Phase = phase
	t := metav1.NewMicroTime(r.now())
	rr.Status.LastPhaseTransitionTime = &t
	if phase == remediationv1alpha1.PhaseMerged && rr.Status.PullRequest != nil && rr.Status.PullRequest.MergedAt == nil {
		// The merge's own time, which no later phase moves: duplicates created before it end Superseded (#556).
		merged := t
		rr.Status.PullRequest.MergedAt = &merged
	}
	if mr := rr.Status.MergeReceipt; phase == remediationv1alpha1.PhaseMerged && mr != nil && mr.MergedAt.IsZero() &&
		rr.Status.PullRequest != nil && rr.Status.PullRequest.MergedAt != nil {
		// The receipt recordMerge built is completed with the time of this write (ADR-0030 section 1).
		mr.MergedAt = *rr.Status.PullRequest.MergedAt
	}
	if phase == remediationv1alpha1.PhaseMerged && rr.Status.MergeReceipt == nil && !noReceiptMerge(rr) {
		// The write that sets Merged carries the receipt or the NoMergeReceipt mark, never neither, also on a path
		// that did not go through recordMerge: settleReceipt reads a request with neither as one merged before
		// receipts existed.
		markNoMergeReceipt(rr)
	}
	// The breaker's watermark is set in the same status write that ends the request (breakercounts.go).
	classifyBreaker(rr)
}

// waitForCapOnDelete holds back the Git write of the finalizer of a request that ended (the close of its pull
// request) while the cap is reached without it. Such a request was not active until it was deleted, so a check
// of the limits made before its finalizer marked it (markDeleting) may have let another request through in its
// slot. Under the admission lock it checks the limits, which now read this request: the others that count are
// the active ones, less those that ended and are being deleted but are not closing now (they wait here and make
// no Git write, so two of them never wait on each other). When fewer than maxConcurrentRemediations count, the
// request is recorded as closing until release is called and the close goes on; else wait is true and the
// deletion is checked again after admissionRecheckDelay without writing. A failure to read the limits is
// returned with wait true (fail closed: no close, retried with backoff); missing settings let the close go on,
// since it then finds no repository. The record of closing requests is the leader's own: a new leader starts
// with none, after the old one stopped.
func (r *RemediationRequestReconciler) waitForCapOnDelete(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) (
	release func(), res reconcile.Result, wait bool, err error) {
	r.admitMu.Lock()
	defer r.admitMu.Unlock()
	limCtx, cancel := bound(ctx, r.CheckTimeout, DefaultCheckTimeout)
	s, err := r.checkLimits(limCtx, rr)
	err = boundErr(ctx, limCtx, "check of the limits", err)
	cancel()
	switch {
	case errors.Is(err, errSettingsMissing):
	case err != nil:
		return nil, reconcile.Result{}, true, err
	default:
		n := s.active
		for _, uid := range s.endedDeleting {
			if _, closing := r.closingOnDelete[uid]; !closing {
				n--
			}
		}
		if n >= s.limit {
			logf.FromContext(ctx).Info("Waiting for a slot under maxConcurrentRemediations before closing the pull "+
				"request of a deleted RemediationRequest", "remediationRequest", client.ObjectKeyFromObject(rr),
				"active", n, "limit", s.limit)
			return nil, reconcile.Result{RequeueAfter: admissionRecheckDelay}, true, nil
		}
	}
	if r.closingOnDelete == nil {
		r.closingOnDelete = map[types.UID]struct{}{}
	}
	r.closingOnDelete[rr.UID] = struct{}{}
	return func() {
		r.admitMu.Lock()
		delete(r.closingOnDelete, rr.UID)
		r.admitMu.Unlock()
	}, reconcile.Result{}, false, nil
}

// holdBack checks the limits for a request about to go through the policy check, and holds it back (Queued)
// when one does not let it through. stop means the caller must return res and err without deciding; else probe
// is whether the request goes through as the breaker's probe (the caller marks it so in the status write that
// lets it through). A failure
// to read the limits stops too, with the error, so the request is retried with backoff (fail closed). clr is
// what a check that read every list found the breaker no longer needs; the caller clears it (clearStale) after
// its decision is written and outside the admission lock, so an admission never waits for that clearing.
//
// queued is the check of a request that is already Queued, made in place of the policy check so that a
// waiting request does not read Git. Once the limits let it through, Queued is removed (written at once, so it
// is not left behind when the policy check then fails for a while) and the policy check runs. Missing
// settings let it go on as well: the policy check denies the request then.
func (r *RemediationRequestReconciler) holdBack(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	queued bool) (res reconcile.Result, probe, stop bool, clr clearing, err error) {
	limCtx, cancel := bound(ctx, r.CheckTimeout, DefaultCheckTimeout)
	s, err := r.checkLimits(limCtx, rr)
	err = boundErr(ctx, limCtx, "check of the limits", err)
	cancel()
	if err == nil {
		clr = s.clear
	}
	pass := queued && errors.Is(err, errSettingsMissing)
	if !pass {
		recordLimits(s, err)
		if err != nil {
			return reconcile.Result{}, false, true, clearing{}, err
		}
		if s.reason != "" {
			// Other holders than the recorded ones: the recorded ones are checked before the record is replaced,
			// so a holder whose pull request was closed while another request took the lock still ends this one
			// (duplicates.go, #643).
			if len(s.holders) > 0 && len(rr.Status.QueuedBehind) > 0 &&
				!slices.Equal(rr.Status.QueuedBehind, s.holders) {
				if ended, err := r.endIfDuplicate(ctx, rr); ended || err != nil {
					if ended {
						r.forget(client.ObjectKeyFromObject(rr))
					}
					return reconcile.Result{}, false, true, clr, err
				}
			}
			res, err = r.waitForLimits(ctx, rr, s)
			return res, false, true, clr, err
		}
	}
	// The limits let the request through. A duplicate of a request of the same Incident and target merged
	// meanwhile ends Superseded instead (duplicates.go, #556): a Queued one before it reads Git, and every one
	// under the admission lock right before it would take the target lock.
	if ended, err := r.endIfDuplicate(ctx, rr); ended || err != nil {
		if ended {
			r.forget(client.ObjectKeyFromObject(rr))
		}
		return reconcile.Result{}, false, true, clr, err
	}
	if queued && meta.FindStatusCondition(rr.Status.Conditions, remediationv1alpha1.ConditionQueued) != nil {
		meta.RemoveStatusCondition(&rr.Status.Conditions, remediationv1alpha1.ConditionQueued)
		if err := r.Status().Update(ctx, rr); err != nil {
			return reconcile.Result{}, false, true, clr, fmt.Errorf("update status of RemediationRequest %s/%s: %w",
				rr.Namespace, rr.Name, err)
		}
	}
	return reconcile.Result{}, s.probe(), false, clr, nil
}
