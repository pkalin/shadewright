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
	"sync"
	"sync/atomic"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/kedaready"
	verification "github.com/pkalin/shadewright/internal/verification/clearing"
	"github.com/pkalin/shadewright/internal/verification/rollout"
)

// Cleared (ADR-0030 sections 3, 4 and 7, #683). Each check of a Verifying request, after the executor's own reads
// (target, Configured, RolledOut and the template's freshness, phases.go), judges whether the symptom cleared:
//
//   - For a Deployment or StatefulSet it reads the verifier's VerificationReport of the request, one object by
//     its name through the API reader, and treats it as untrusted evidence. A report that records an OOM kill of
//     the action's container and is bound to this request (its spec check) ends the request Failed with
//     OOMKilled at once. Otherwise the check is good when RolledOut is True, the report passes the trust
//     contract (clearing.CheckReport: missing, never sampled or stale is StaleReport, another spec is
//     ReportMismatch, incomplete coverage is PartialRollout) and the target reports exactly the desired
//     replicas available (else NotAvailable). A good check opens the executor's own window, a bad one resets
//     it, but only when it closes an open window or names another reason than the last reset (resetWindow);
//     the window lives in status.verification. A reset whose status write failed is kept in memory
//     (unwrittenResets) and applied again by the next check, so the retry, which reads the window from before
//     it, cannot end the request across that bad check. A restart or a leader change loses that memory, so the
//     first check of a new process resets every open window that no check of its own opened (ownWindows), with
//     ExecutorRestarted (restarted), and stores that reset like any other: the window then needs a full
//     stableFor again, and status.verification shows why. Before RolledOut is True nothing opens or resets the
//     window.
//   - Success (clearing.Succeeds) needs a good check, a report whose sample counter rose since a check at most
//     one poll interval earlier by the executor's clock, taken again after the report read (the counter is
//     kept in memory, like the start of reconciling), and both spans at least the effective stableFor plus the
//     node grace, each on its own writer's clock, the executor's counted from its stored windowStart. One status
//     write then sets Succeeded, Cleared True
//     (NoRecurrence) and Verified True. A request whose spec.verification.checks names QueueBacklogDecreasing
//     ends Inconclusive with CheckNotAvailable instead, since no metrics are read.
//   - A ScaledObject has no pods of its own: its window is good while it is ready by the KedaReady rule (else
//     NotAvailable), and once it held for the effective stableFor the request ends Inconclusive with
//     ClearingNotChecked (CheckNotAvailable when QueueBacklogDecreasing is named).
//   - A revert of a Deployment or StatefulSet reads no report (clearRevertStep): a check before RolledOut is
//     True resets its window (PartialRollout), so no earlier span carries over, and so does a target with no
//     desired replica (NotAvailable).
//   - At the timeout without a success, clearing.AtTimeout decides: Failed for a stuck rollout, a ScaledObject
//     that is not ready and recorded trouble in a report bound to the request (Restarted), else Inconclusive
//     with the reason that holds the window closed.
//
// A failed read of the report is retried, never a verdict and never "missing", until the timeout and its grace;
// after that the timeout is decided without a report. No decision compares a time of the executor's clock with
// one the verifier wrote.

// Reasons of the Cleared and Verified conditions the executor writes from the report (ADR-0030 sections 3 to 5).
const (
	// ReasonNoRecurrence: Cleared True and Verified True; a quiet window held for the effective stableFor plus
	// the node grace (Succeeded).
	ReasonNoRecurrence = "NoRecurrence"
	// ReasonObserving: Cleared Unknown while the window of a Verifying Deployment or StatefulSet runs.
	ReasonObserving = "Observing"
	// ReasonOOMKilled: the report records an OOM kill of the action's container on a pod of the rollout
	// (Cleared False, Verified False, Failed at once).
	ReasonOOMKilled = "OOMKilled"
	// ReasonRestarted: the report recorded a restart above its baseline after the window first opened (Cleared
	// False, Verified False, Failed at the timeout).
	ReasonRestarted = string(remediationv1alpha1.ResetRestarted)
	// ReasonCheckNotAvailable: the request names the check QueueBacklogDecreasing, which no evidence source
	// supports yet (Inconclusive).
	ReasonCheckNotAvailable = "CheckNotAvailable"
)

// reportCounters keeps, per request, the executor's memory of its report's sample counter
// (verification.Counter). It is memory only and dropped when the request ends, is being deleted or is gone.
type reportCounters struct {
	m sync.Map
}

// counter returns the counter of the request with key, created when missing. A request is reconciled by one
// worker at a time, so its counter is never used concurrently.
func (c *reportCounters) counter(key types.NamespacedName) *verification.Counter {
	v, _ := c.m.LoadOrStore(key, &verification.Counter{})
	return v.(*verification.Counter)
}

// forget drops the counter of the request with key.
func (c *reportCounters) forget(key types.NamespacedName) {
	c.m.Delete(key)
}

// unwrittenResets keeps, per request, the last reset of the executor's window that a check recorded and no
// status write has stored yet. The next check applies it again to the window it reads (checkWindow), so a lost
// write cannot drop a bad check: without it the retry would read the window from before that check, and a good
// state then could end the request across the bad interval. It is memory only, cleared once a write of the
// window succeeds (keepVerifying) and dropped when the request ends, is being deleted or is gone.
type unwrittenResets struct {
	m sync.Map
}

// keptReset is one entry of unwrittenResets: the reset and the UID of the request it belongs to.
type keptReset struct {
	uid   types.UID
	reset remediationv1alpha1.WindowReset
}

// note remembers the reset of w when the check left it closed. An open w changes nothing: a reset still kept was
// applied to it (checkWindow) and stays until a write stores it.
func (u *unwrittenResets) note(rr *remediationv1alpha1.RemediationRequest, w verification.Window) {
	if !w.Open() && w.LastReset != nil {
		u.m.Store(client.ObjectKeyFromObject(rr), keptReset{uid: rr.UID, reset: *w.LastReset.DeepCopy()})
	}
}

// get returns the reset kept for rr. An entry of an earlier request with the same name (another UID) is dropped
// and not returned: a request never inherits the reset of the one whose name it reuses.
func (u *unwrittenResets) get(rr *remediationv1alpha1.RemediationRequest) (remediationv1alpha1.WindowReset, bool) {
	key := client.ObjectKeyFromObject(rr)
	v, ok := u.m.Load(key)
	if !ok {
		return remediationv1alpha1.WindowReset{}, false
	}
	kept := v.(keptReset)
	if kept.uid != rr.UID {
		u.m.Delete(key)
		return remediationv1alpha1.WindowReset{}, false
	}
	return kept.reset, true
}

// forget drops the reset kept for the request with key.
func (u *unwrittenResets) forget(key types.NamespacedName) {
	u.m.Delete(key)
}

// reconcilingSince is the moment, on the executor's clock, this process began reconciling: set once, by the
// first of sinceMarker (started when the manager's leader election is won, or at once without leader
// election) and the first check that asks for it. A standby replica takes it when it becomes leader, not when
// its process started, since it checked nothing before. A check can run before sinceMarker starts (the
// manager starts both together), and then its own later time is kept, which can only delay an end.
type reconcilingSince struct {
	t atomic.Pointer[time.Time]
}

// mark sets the moment to at unless it is set, and returns it.
func (s *reconcilingSince) mark(at time.Time) time.Time {
	s.t.CompareAndSwap(nil, &at)
	return *s.t.Load()
}

// sinceMarker is the runnable that sets the reconciler's reconcilingSince when the manager's leader election
// is won.
type sinceMarker struct {
	r *RemediationRequestReconciler
}

// Start sets the moment and waits for the manager to stop.
func (m sinceMarker) Start(ctx context.Context) error {
	m.r.since.mark(m.r.now())
	<-ctx.Done()
	return nil
}

// NeedLeaderElection makes the moment the start of leadership on a standby that becomes leader later.
func (sinceMarker) NeedLeaderElection() bool { return true }

// ownWindows keeps, per request in Verifying, what this process knows of the executor's window from its own
// checks: the start of every open window a check of its own left (starts, at most ownWindowsBound), and what it
// read of the report at the check that recorded its restart (restart). Memory only, so a new process has neither:
// dropped when the request ends, is being deleted or is gone, like reportCounters. A request is reconciled by one
// worker at a time.
type ownWindows struct {
	m sync.Map
}

// ownWindowsBound is how many window starts ownWindows keeps per request (decision of 2026-11-21 on #970). Past
// it the oldest is forgotten, and a check that then reads that window in status records a restart that did not
// happen: the window closes and needs a full stableFor again, a longer wait, never a shorter one.
const ownWindowsBound = 16

// ownWindow is one entry of ownWindows, for the request with uid. starts holds the start of every window this
// process opened for it, oldest first, whatever became of the status write that carried it.
type ownWindow struct {
	uid     types.UID
	starts  []metav1.Time
	restart *verification.Restart
}

// load returns the entry of rr. An entry of an earlier request with the same name (another UID) is dropped.
func (o *ownWindows) load(rr *remediationv1alpha1.RemediationRequest) ownWindow {
	key := client.ObjectKeyFromObject(rr)
	if v, ok := o.m.Load(key); ok {
		if e := v.(ownWindow); e.uid == rr.UID {
			return e
		}
		o.m.Delete(key)
	}
	return ownWindow{uid: rr.UID}
}

// owns reports whether the open window w is one a check of this process opened: its start is one of the starts
// this process remembers for rr. Only starts this process stored in memory are compared with w's, values it wrote
// itself: no time of another process is compared.
func (o *ownWindows) owns(rr *remediationv1alpha1.RemediationRequest, w verification.Window) bool {
	if !w.Open() {
		return false
	}
	for _, s := range o.load(rr).starts {
		if s.Equal(w.Start) {
			return true
		}
	}
	return false
}

// note remembers the start of w when the check left it open, whether its write is stored, lost, or stored with
// an error returned (#970). Any of these windows may be the one status holds at a later check: a lost write
// leaves an earlier window of this process there, and a write stored after a lost one may be read back after
// further lost writes. So every window this process opened stays its own, not only the last one; a window
// another process opened was never noted here and is reset by restarted. A check that closed w adds nothing:
// the window it closed is already noted, and when its write is lost the reset kept in unwrittenResets closes it
// again. At most ownWindowsBound starts are kept, the oldest forgotten first.
func (o *ownWindows) note(rr *remediationv1alpha1.RemediationRequest, w verification.Window) {
	if !w.Open() {
		return
	}
	e := o.load(rr)
	for _, s := range e.starts {
		if s.Equal(w.Start) {
			return
		}
	}
	// A new slice on every change: a stored entry is never changed in place.
	starts := make([]metav1.Time, 0, min(len(e.starts)+1, ownWindowsBound))
	if len(e.starts) >= ownWindowsBound {
		e.starts = e.starts[len(e.starts)-ownWindowsBound+1:]
	}
	e.starts = append(append(starts, e.starts...), *w.Start.DeepCopy())
	o.m.Store(client.ObjectKeyFromObject(rr), e)
}

// noteRestart remembers what the check that recorded the restart read of the report: its UID and reset count, a
// zero UID for a missing report. Only the first is kept: a later check that records the restart again (its first
// write was lost) read the report later, and a reset in between must still count as after the restart.
func (o *ownWindows) noteRestart(rr *remediationv1alpha1.RemediationRequest, report *remediationv1alpha1.VerificationReport) {
	e := o.load(rr)
	if e.restart != nil {
		return
	}
	e.restart = &verification.Restart{}
	if report != nil {
		e.restart.Report, e.restart.Resets = report.UID, report.Status.Resets
	}
	o.m.Store(client.ObjectKeyFromObject(rr), e)
}

// restart returns what this process read of the report when it recorded its restart, nil when it recorded none.
func (o *ownWindows) restart(rr *remediationv1alpha1.RemediationRequest) *verification.Restart {
	return o.load(rr).restart
}

// forget drops the entry of the request with key.
func (o *ownWindows) forget(key types.NamespacedName) {
	o.m.Delete(key)
}

// restarted records, in the executor's window w read from status, the restart of the executor (ADR-0030 section
// 4, amendments of 2026-11-20 and 2026-11-21, #895, #970): an open window that no check of this process opened
// (ownWindows, the last ownWindowsBound of them) is reset with ExecutorRestarted, and restarted reports whether it
// did. A reset an earlier process recorded but
// could not store is lost with its memory (unwrittenResets) and may lie in that window, and no check ran while no
// process reconciled, so no window carries time over from before this process. The reset is stored like any
// other, so status.verification shows it and the stored window is the one that decides; the next good check
// opens it again. A closed window stays as it is: it counts nothing until a good check of this process.
//
// Which window is this process's is decided by its own memory, never by comparing the stored windowStart, written
// on the clock of the process that opened it, with the start of reconciling on this one: a new leader whose clock
// is behind the old one's would keep a window the old leader opened. The reset's time is the second before the
// start of reconciling on this process's clock, so a good check in the same second still opens the window; it is
// never earlier than the stored windowStart (clearing.Window.Reset), so an old leader's clock ahead of this one
// keeps the window closed until this clock passes that start, which can only delay an end.
func (r *RemediationRequestReconciler) restarted(rr *remediationv1alpha1.RemediationRequest, w *verification.Window) bool {
	if !w.Open() || r.ownWindows.owns(rr, *w) {
		return false
	}
	since := r.since.mark(r.now()).Truncate(time.Second)
	w.Reset(remediationv1alpha1.ResetExecutorRestarted, since.Add(-time.Second))
	return true
}

// checkWindow returns the executor's window a check starts from: the stored one, with the restart of the
// executor recorded (restarted, whose result it returns) and a reset of an earlier check of this process whose
// write was lost applied again. resetWindow keeps the times from moving back, so a reset already stored changes
// nothing, and one older than a later windowStart closes that window too, which can only delay an end. The
// restart comes first: a kept reset is of this process, so it is never older than the restart.
func (r *RemediationRequestReconciler) checkWindow(rr *remediationv1alpha1.RemediationRequest) (verification.Window, bool) {
	w := executorWindow(rr)
	restart := r.restarted(rr, &w)
	if lost, ok := r.unwrittenResets.get(rr); ok {
		resetWindow(&w, lost.Reason, lost.Time.Time)
	}
	return w, restart
}

// noteWindow keeps in memory what a check left of the executor's window w: a reset its write may lose
// (unwrittenResets) and the open window that is this process's (ownWindows).
func (r *RemediationRequestReconciler) noteWindow(rr *remediationv1alpha1.RemediationRequest, w verification.Window) {
	r.unwrittenResets.note(rr, w)
	r.ownWindows.note(rr, w)
}

// executorWindow returns the executor's own window from the request's status.
func executorWindow(rr *remediationv1alpha1.RemediationRequest) verification.Window {
	v := rr.Status.Verification
	if v == nil {
		return verification.Window{}
	}
	w := verification.Window{Start: v.WindowStart.DeepCopy(), Resets: v.Resets}
	if v.LastReset != nil {
		w.LastReset = &remediationv1alpha1.WindowReset{Reason: v.LastReset.Reason, Time: v.LastReset.Time}
	}
	return w
}

// storeWindow writes w into the request's status (not written yet) and reports whether it changed.
func storeWindow(rr *remediationv1alpha1.RemediationRequest, w verification.Window) bool {
	next := &remediationv1alpha1.VerificationWindow{WindowStart: w.Start, Resets: w.Resets}
	if w.LastReset != nil {
		next.LastReset = &remediationv1alpha1.VerificationReset{Reason: w.LastReset.Reason, Time: w.LastReset.Time}
	}
	if rr.Status.Verification != nil {
		if equalWindow(*rr.Status.Verification, *next) {
			return false
		}
		next.IncidentCount = rr.Status.Verification.IncidentCount
	}
	rr.Status.Verification = next
	return true
}

func equalWindow(a, b remediationv1alpha1.VerificationWindow) bool {
	timeEq := func(x, y *metav1.Time) bool { return (x == nil) == (y == nil) && (x == nil || x.Equal(y)) }
	resetEq := (a.LastReset == nil) == (b.LastReset == nil) &&
		(a.LastReset == nil || a.LastReset.Reason == b.LastReset.Reason && a.LastReset.Time.Equal(&b.LastReset.Time))
	return a.Resets == b.Resets && timeEq(a.WindowStart, b.WindowStart) && resetEq
}

// readReport reads the request's VerificationReport through the API reader. A report that does not exist is
// nil without an error; any other failed read is an error, retried, never a missing report.
func (r *RemediationRequestReconciler) readReport(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) (
	*remediationv1alpha1.VerificationReport, error) {
	report := &remediationv1alpha1.VerificationReport{}
	key := types.NamespacedName{Namespace: rr.Namespace, Name: verification.ReportName(rr.UID)}
	err := r.reader().Get(ctx, key, report)
	switch {
	case apierrors.IsNotFound(err):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("get VerificationReport %s: %w", key, err)
	}
	return report, nil
}

// expectedReport is what the request's report must be bound to: the request, its receipt's merge commit, the
// target the executor read in this check (its UID and spec.replicas), the recorded template revision and the
// action's container.
func expectedReport(rr *remediationv1alpha1.RemediationRequest, target client.Object) verification.Expected {
	exp := verification.Expected{RequestUID: rr.UID, TargetUID: target.GetUID()}
	if mr := rr.Status.MergeReceipt; mr != nil {
		exp.MergeCommitSHA = mr.MergeCommitSHA
	}
	if st := rr.Status.Rollout; st != nil {
		exp.TemplateRevision = st.TemplateRevision
	}
	if a := rr.Spec.Action.SetContainerMemoryLimit; a != nil {
		exp.Container = a.Container
	}
	exp.Replicas, _ = replicasOf(target)
	return exp
}

// replicasOf returns the desired replicas (spec.replicas, 1 when unset) and the available replicas a Deployment
// or StatefulSet reports. Any other kind has neither.
func replicasOf(target client.Object) (desired, available int32) {
	switch t := target.(type) {
	case *appsv1.Deployment:
		return replicasOrOne(t.Spec.Replicas), t.Status.AvailableReplicas
	case *appsv1.StatefulSet:
		return replicasOrOne(t.Spec.Replicas), t.Status.AvailableReplicas
	}
	return 0, 0
}

func replicasOrOne(p *int32) int32 {
	if p == nil {
		return 1
	}
	return *p
}

// namesBacklogCheck reports whether the request names the check QueueBacklogDecreasing, which no evidence
// source supports yet.
func namesBacklogCheck(rr *remediationv1alpha1.RemediationRequest) bool {
	return slices.Contains(rr.Spec.Verification.Checks, remediationv1alpha1.CheckQueueBacklogDecreasing)
}

// clearStep judges the symptom of a Verifying request after the executor's own reads of this check (see the
// comment at the top of this file). changed and stuck carry what those reads put into rr: whether the status
// changed and the stuck rollout to report once. It returns when to look again, and an error only for a failed
// read or status write, retried with backoff.
func (r *RemediationRequestReconciler) clearStep(ctx, stepCtx context.Context,
	rr *remediationv1alpha1.RemediationRequest, target client.Object, now, deadline time.Time, changed bool,
	stuck *rollout.Result) (reconcile.Result, error) {
	ref := rr.Spec.TargetRef
	stableFor := verification.StableFor(rr.Spec.Verification.StableFor, rr.Spec.Action.Type)
	need := stableFor + r.nodeGrace()
	w, restart := r.checkWindow(rr)
	interval := verification.PollInterval
	if rolloutTarget(ref) == nil {
		return r.clearScaledStep(ctx, rr, target, now, deadline, stableFor, changed, stuck)
	}
	if remediationv1alpha1.IsRevert(rr) {
		// A revert has no report: the OOM rule and Cleared do not apply to it (ADR-0030 section 8).
		return r.clearRevertStep(ctx, rr, target, now, deadline, stableFor, changed, stuck)
	}

	key := client.ObjectKeyFromObject(rr)
	// The report's counter is remembered at the time taken before its read, a lower bound of when what the
	// read returns was true; its freshness is judged at the time taken after it (below).
	readAt := laterOf(now, r.now())
	report, err := r.readReport(stepCtx, rr)
	err = boundErr(ctx, stepCtx, "report read", err)
	if apierrors.IsForbidden(err) {
		// A missing grant does not clear by itself: say so at once, not only at the timeout.
		logf.FromContext(ctx).Error(err, "The executor may not read the VerificationReport of RemediationRequest; "+
			"it needs get on verificationreports in the operator namespace", "remediationRequest", key)
	}
	// Freshness, both spans and the timeout are judged at the end of this check's reads, against counters
	// remembered at the start of theirs: a slow read can make a report look older, never fresher. The clock is
	// never taken back.
	now = laterOf(readAt, r.now())
	readFailed := false
	switch {
	case ctx.Err() != nil:
		return reconcile.Result{}, err
	case err != nil && now.Before(deadline.Add(finalGrace)):
		// Retried, never a missing report. What this check read of the target is still recorded, and a rollout
		// first seen stuck is reported once that record is written.
		if werr := r.writeIf(ctx, rr, changed); werr != nil {
			return reconcile.Result{}, errors.Join(err, werr)
		}
		r.reportStuck(rr, stuck)
		return reconcile.Result{}, err
	case err != nil:
		logf.FromContext(ctx).Info("The VerificationReport of RemediationRequest could not be read",
			"remediationRequest", key, "error", err.Error())
		readFailed = true
	}
	if restart && !readFailed {
		// The report as this check read it is the line between resets before the restart and after it, counted by
		// the report's own reset counter (clearing.AtTimeout).
		r.ownWindows.noteRestart(rr, report)
	}
	exp := expectedReport(rr, target)
	if verification.OOMFails(report, exp) {
		k := report.Status.OOMKill
		return reconcile.Result{}, r.endVerification(ctx, rr, remediationv1alpha1.PhaseFailed, ReasonOOMKilled,
			fmt.Sprintf("The verifier recorded an OOM kill of container %s on pod %s of the rollout, which runs the "+
				"desired limit", k.Container, k.PodUID))
	}
	// A report that was created but never sampled has no evidence: it is read as missing.
	sampled := report
	if report != nil && report.Status.SampledAt == nil {
		sampled = nil
	}
	var fresh verification.Freshness
	if sampled != nil {
		fresh = r.reportCounters.counter(key).Observe(sampled.UID, sampled.Status.Sample, readAt, now)
	}
	contract := verification.CheckReport(sampled, exp, fresh)
	rolledOut := meta.IsStatusConditionTrue(rr.Status.Conditions, rollout.ConditionRolledOut)
	good := false
	if rolledOut {
		desired, available := replicasOf(target)
		switch {
		case contract != "":
			resetWindow(&w, contract, now)
		case available != desired:
			// ADR-0030 section 4: available replicas equal spec.replicas, so a surge above it is no quiet
			// window either.
			resetWindow(&w, remediationv1alpha1.ResetNotAvailable, now)
		default:
			w.Good(now)
			good = true
		}
	}
	changed = storeWindow(rr, w) || changed
	r.noteWindow(rr, w)
	if good && verification.Succeeds(verification.Success{Report: sampled, Contract: contract, Freshness: fresh,
		Executor: w, Now: now, StableFor: stableFor, NodeGrace: r.nodeGrace()}) {
		if namesBacklogCheck(rr) {
			return reconcile.Result{}, r.endVerification(ctx, rr, remediationv1alpha1.PhaseInconclusive,
				ReasonCheckNotAvailable, fmt.Sprintf("No restart or OOM kill for %s, but the check "+
					"QueueBacklogDecreasing needs metrics, which no evidence source reads yet", need))
		}
		return reconcile.Result{}, r.succeed(ctx, rr, stableFor, fmt.Sprintf("No OOM kill and no restart on every desired "+
			"replica for %s (stableFor %s plus the node grace), by the executor's reads and a fresh report of "+
			"the verifier", need, stableFor))
	}
	if !now.Before(deadline) {
		verdict, reason := verification.AtTimeout(verification.Timeout{RolledOut: stuckRecorded(rr) == nil,
			Report: report, Expected: exp, Executor: w, Restart: r.ownWindows.restart(rr)})
		note := ""
		if readFailed {
			note = " (the verifier's report could not be read at the end; the executor log has the error)"
		}
		return reconcile.Result{}, r.endAtTimeout(ctx, rr, verdict, reason, note)
	}
	if sampled != nil && good {
		st := &sampled.Status
		if verification.ReportWindow(st).Span(st.SampledAt.Time) >= need && w.Span(now) >= need {
			interval = verification.FastPollInterval
		}
	}
	changed = setCondition(rr, remediationv1alpha1.ConditionCleared, metav1.ConditionUnknown, ReasonObserving,
		fmt.Sprintf("Waiting for a quiet window of %s on the executor's checks and in the verifier's report", need)) ||
		changed
	return r.keepVerifying(ctx, rr, now, deadline, interval, changed, stuck)
}

// keepVerifying writes the status of a check that ended nothing, when it changed, reports a rollout first seen
// stuck, and looks again after interval or at the deadline, whichever is sooner.
func (r *RemediationRequestReconciler) keepVerifying(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	now, deadline time.Time, interval time.Duration, changed bool, stuck *rollout.Result) (reconcile.Result, error) {
	if err := r.writeIf(ctx, rr, changed); err != nil {
		// A reset of this check stays in unwrittenResets, so the retry cannot read it away.
		return reconcile.Result{}, err
	}
	// The window of this check is stored now (or was already, when nothing changed).
	r.unwrittenResets.forget(client.ObjectKeyFromObject(rr))
	r.reportStuck(rr, stuck)
	return reconcile.Result{RequeueAfter: max(min(interval, deadline.Sub(now)), time.Second)}, nil
}

// reportStuck sends the Warning Event of a rollout first seen stuck in this check, after its status was written.
func (r *RemediationRequestReconciler) reportStuck(rr *remediationv1alpha1.RemediationRequest, stuck *rollout.Result) {
	if stuck != nil && r.Recorder != nil {
		r.Recorder.Event(rr, corev1.EventTypeWarning, rollout.ReasonRolloutStuck, stuck.Message)
	}
}

// resetWindow records a bad check in the executor's window w at now. It counts as a reset only when it closes an
// open window or names another reason than the last reset: the same reason again on a closed window changes
// nothing, so it writes no status, and a status write (which queues the request again) cannot raise resets on
// every check (ADR-0030 section 4, amendment of 2026-11-13).
func resetWindow(w *verification.Window, reason remediationv1alpha1.ResetReason, now time.Time) {
	if !w.Open() && w.LastReset != nil && w.LastReset.Reason == reason {
		return
	}
	w.Reset(reason, now)
}

// laterOf returns the later of two times of the executor's clock.
func laterOf(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// endAtTimeout ends a request at its timeout with the verdict of clearing.AtTimeout. note is added to the
// message.
func (r *RemediationRequestReconciler) endAtTimeout(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	verdict verification.Verdict, reason, note string) error {
	phase := remediationv1alpha1.PhaseInconclusive
	if verdict == verification.VerdictFailed {
		phase = remediationv1alpha1.PhaseFailed
	}
	var message string
	switch {
	case reason == rollout.ReasonRolloutStuck:
		message = "The rollout did not complete by the verification timeout"
		if c := stuckRecorded(rr); c != nil {
			message += ": " + c.Message
		}
	case reason == ReasonRestarted && verdict == verification.VerdictFailed:
		// Only recorded trouble fails the request; a Restarted reset that is not recorded as trouble (before the
		// window first opened, or shown only by a termination time) is the default case below.
		message = "The verifier recorded a container restart above its baseline after the window opened; at the " +
			"verification timeout the change is not shown to work"
	default:
		message = fmt.Sprintf("The verification timeout passed without a quiet window; the last reason that held it "+
			"closed is %s", reason)
	}
	return r.endVerification(ctx, rr, phase, reason, message+note)
}

// succeed ends a Verifying request Succeeded in one status write: Cleared True and Verified True with reason
// NoRecurrence, the executor's window as it is, the phase and its breaker classification, no deadline and the
// target lock settled. Cleared carries ResolvedBeforeRollout instead when the readings of the Incident's count
// show it did not move from the merge to the rollout (clearedReason); that changes nothing else. One Normal
// Event follows. A merged stale plan ends Inconclusive with StalePlanMerged instead (capStaleSuccess).
func (r *RemediationRequestReconciler) succeed(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	stableFor time.Duration, message string) error {
	if done, err := r.capStaleSuccess(ctx, rr); done {
		return err
	}
	clearedBy, clearedMessage := clearedReason(rr, stableFor, message)
	setCondition(rr, remediationv1alpha1.ConditionCleared, metav1.ConditionTrue, clearedBy, clearedMessage)
	setCondition(rr, remediationv1alpha1.ConditionVerified, metav1.ConditionTrue, ReasonNoRecurrence, message)
	r.setPhase(rr, remediationv1alpha1.PhaseSucceeded)
	rr.Status.Deadline = nil
	r.settleRecorded(ctx, rr)
	if err := r.writeIf(ctx, rr, true); err != nil {
		return err
	}
	r.reportCounters.forget(client.ObjectKeyFromObject(rr))
	r.ownWindows.forget(client.ObjectKeyFromObject(rr))
	r.unwrittenResets.forget(client.ObjectKeyFromObject(rr))
	r.mergeReadings.forget(client.ObjectKeyFromObject(rr))
	if r.Recorder != nil {
		r.Recorder.Event(rr, corev1.EventTypeNormal, ReasonNoRecurrence, message)
	}
	logf.FromContext(ctx).Info("Verified RemediationRequest", "remediationRequest", client.ObjectKeyFromObject(rr),
		"phase", remediationv1alpha1.PhaseSucceeded)
	return nil
}

// clearedAtEnd is the Cleared condition of a request that ends from Verifying in phase with reason: False when
// it ends Failed for the trouble the report recorded (OOMKilled, Restarted), Unknown with the same reason
// otherwise. The phase decides, not the reason alone: an Inconclusive end can carry Restarted as the reason that
// held the window closed without any trouble recorded. A revert's is always Unknown with RevertApplied, whatever
// ends it: its clearing is never checked (ADR-0030 section 8).
func clearedAtEnd(rr *remediationv1alpha1.RemediationRequest, phase remediationv1alpha1.RemediationPhase,
	reason, message string) metav1.Condition {
	if remediationv1alpha1.IsRevert(rr) {
		return metav1.Condition{Type: remediationv1alpha1.ConditionCleared, Status: metav1.ConditionUnknown,
			Reason: remediationv1alpha1.ReasonRevertApplied, Message: msgRevertNotCleared, ObservedGeneration: rr.Generation}
	}
	status := metav1.ConditionUnknown
	if phase == remediationv1alpha1.PhaseFailed && (reason == ReasonOOMKilled || reason == ReasonRestarted) {
		status = metav1.ConditionFalse
	}
	return metav1.Condition{Type: remediationv1alpha1.ConditionCleared, Status: status, Reason: reason,
		Message: message, ObservedGeneration: rr.Generation}
}

// clearScaledStep is clearStep for a target without pods of its own, a ScaledObject: rolled out is Delivered
// plus KedaReady, and container status shows nothing of its symptom, so Cleared is not checked.
func (r *RemediationRequestReconciler) clearScaledStep(ctx context.Context,
	rr *remediationv1alpha1.RemediationRequest, target client.Object, now, deadline time.Time,
	stableFor time.Duration, changed bool, stuck *rollout.Result) (reconcile.Result, error) {
	ref := rr.Spec.TargetRef
	w, _ := r.checkWindow(rr)
	interval := verification.PollInterval
	state := kedaready.Ready
	if isScaledObject(ref) {
		state = kedaState(target)
	}
	kedaOK := state == kedaready.Ready
	if kedaOK {
		w.Good(now)
	} else {
		resetWindow(&w, remediationv1alpha1.ResetNotAvailable, now)
	}
	changed = storeWindow(rr, w) || changed
	r.noteWindow(rr, w)
	revert := remediationv1alpha1.IsRevert(rr)
	if w.Span(now) >= stableFor {
		reason, message := ReasonClearingNotChecked, fmt.Sprintf("The %s was rolled out and configured for %s; "+
			"its symptom is not visible in container status, so clearing is not checked", ref.Kind, stableFor)
		switch {
		case revert:
			reason, message = remediationv1alpha1.ReasonRevertApplied, revertAppliedMessage(ref.Kind, stableFor)
		case namesBacklogCheck(rr):
			reason, message = ReasonCheckNotAvailable, fmt.Sprintf("The %s was rolled out and configured for %s; "+
				"the check QueueBacklogDecreasing needs metrics, which no evidence source reads yet", ref.Kind,
				stableFor)
		}
		return reconcile.Result{}, r.endVerification(ctx, rr, remediationv1alpha1.PhaseInconclusive, reason, message)
	}
	if !now.Before(deadline) {
		verdict, reason := verification.AtTimeout(verification.Timeout{RolledOut: true,
			ScaledObject: isScaledObject(ref), KedaReady: kedaOK, Executor: w})
		if reason == ReasonKedaNotReady {
			return reconcile.Result{}, r.endVerification(ctx, rr, remediationv1alpha1.PhaseFailed,
				ReasonKedaNotReady, fmt.Sprintf("The ScaledObject is %s by the KedaReady rule at the verification "+
					"timeout, so it does not scale on its metrics", state))
		}
		return reconcile.Result{}, r.endAtTimeout(ctx, rr, verdict, reason, "")
	}
	cleared, message := ReasonClearingNotChecked, fmt.Sprintf("A %s's symptom is not visible in container status, "+
		"so clearing is not checked", ref.Kind)
	if revert {
		cleared, message = remediationv1alpha1.ReasonRevertApplied, msgRevertNotCleared
	}
	changed = setCondition(rr, remediationv1alpha1.ConditionCleared, metav1.ConditionUnknown, cleared, message) ||
		changed
	return r.keepVerifying(ctx, rr, now, deadline, interval, changed, stuck)
}

// msgRevertNotCleared is the message of a revert's Cleared condition, Unknown with RevertApplied.
const msgRevertNotCleared = "A revert is not checked for clearing: the symptom of the change it undoes is " +
	"expected to come back, so no OOM kill or restart decides it"

// revertAppliedMessage is the message of a revert's end, Inconclusive with RevertApplied.
func revertAppliedMessage(kind string, stableFor time.Duration) string {
	return fmt.Sprintf("The revert was rolled out, configured and available on the %s for %s; a revert is never "+
		"verified as a success, and the Incident stays with a person", kind, stableFor)
}

// clearRevertStep is clearStep for a revert of a Deployment or StatefulSet (ADR-0030 section 8). It reads no
// report: the verifier writes none for a revert, so the OOM rule and Cleared do not apply, and Cleared stays
// Unknown with RevertApplied. The executor's own window is good while RolledOut is True (else it is reset with
// PartialRollout) and the target reports exactly the desired replicas available, at least one (else
// NotAvailable); once it spans stableFor on the executor's clock the request ends Inconclusive with
// RevertApplied, never Succeeded. At the timeout a stuck rollout ends it Failed
// (RolloutStuck); anything else ends it Inconclusive with the reason that held the window closed.
func (r *RemediationRequestReconciler) clearRevertStep(ctx context.Context,
	rr *remediationv1alpha1.RemediationRequest, target client.Object, now, deadline time.Time,
	stableFor time.Duration, changed bool, stuck *rollout.Result) (reconcile.Result, error) {
	w, _ := r.checkWindow(rr)
	good := false
	desired, available := replicasOf(target)
	switch {
	case !meta.IsStatusConditionTrue(rr.Status.Conditions, rollout.ConditionRolledOut):
		// Not rolled out is a bad check too: a window recorded before it must not carry its span over.
		resetWindow(&w, remediationv1alpha1.ResetPartialRollout, now)
	case desired < 1 || available != desired:
		// No desired replica (scaled to zero, or a kind replicasOf does not know) shows no availability.
		resetWindow(&w, remediationv1alpha1.ResetNotAvailable, now)
	default:
		w.Good(now)
		good = true
	}
	changed = storeWindow(rr, w) || changed
	r.noteWindow(rr, w)
	if good && w.Span(now) >= stableFor {
		return reconcile.Result{}, r.endVerification(ctx, rr, remediationv1alpha1.PhaseInconclusive,
			remediationv1alpha1.ReasonRevertApplied, revertAppliedMessage(rr.Spec.TargetRef.Kind, stableFor))
	}
	if !now.Before(deadline) {
		if stuckRecorded(rr) != nil {
			return reconcile.Result{}, r.endAtTimeout(ctx, rr, verification.VerdictFailed, rollout.ReasonRolloutStuck, "")
		}
		// No report is read for a revert, so only the executor's own reset can name the cause.
		reason := string(remediationv1alpha1.ResetEvidenceMissing)
		if w.LastReset != nil {
			reason = string(w.LastReset.Reason)
		}
		return reconcile.Result{}, r.endAtTimeout(ctx, rr, verification.VerdictInconclusive, reason,
			" (a revert is checked for rollout, configuration and availability only)")
	}
	changed = setCondition(rr, remediationv1alpha1.ConditionCleared, metav1.ConditionUnknown,
		remediationv1alpha1.ReasonRevertApplied, msgRevertNotCleared) || changed
	return r.keepVerifying(ctx, rr, now, deadline, verification.PollInterval, changed, stuck)
}
