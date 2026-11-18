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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	api "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/policy/freshness"
	"github.com/pkalin/shadewright/internal/providers"
)

// A Git wait is recorded state (decisions of 2026-11-17 on #771): an entry of status.gitWaits per kind of bound
// (waitKind), with its start on the executor's clock, the read that failed, and, for the content wait, the pull
// request it is about. Starting or ending one kind never touches another (putGitWaits, dropGitWaits), and an entry
// is built whole (newWait, wholeWait): it records its end (status.gitWaits[].ends) for every kind but Paced, and the
// merge waits record the pull request they are about. The end of an entry is read from the entry alone
// (waitKind.end), and its operation is cut to the schema's bound where it is built (cutOperation). The check at the
// start of every reconcile (endPassedWait) ends each RequestDeadline and HandoffGrace entry past its end before any
// read or step runs, reading nothing from the Git host (endWait). A wait for a read (readWaits: MergeCheck,
// MergeContent) past its end ends only after one more read (ninth decision of 2026-11-18 on #771): that reconcile
// makes its reads as any other, and when they leave the entry standing (a read failed, or none could be made) it
// ends there (endAfterLastRead). A call site only starts or continues a wait (retryGitRead); it never looks at
// whether the end has passed, so an error, a timeout or an early return in a step cannot skip the end. An entry past
// its end always ends: by its kind's end, or, when the kind no longer applies to the request, by its removal; never
// by an error that repeats. Condition GitReadRetry summarizes the entries for people (gitWaitSummary).

// waitKind is the kind of bound of a Git wait.
type waitKind int

const (
	// waitNone has no end: the retry is paced, nothing ends it (entry kind Paced, condition reason GitReadFailed).
	waitNone waitKind = iota
	// waitRequestDeadline ends at status.deadline of a request in PolicyChecked whose stale plan's cleanup has not
	// started: the request expires (expireEnded).
	waitRequestDeadline
	// waitHandoffGrace ends handoffRecoveryGrace after status.deadline of a request recovering a handoff: it ends
	// NeedsIntervention with HandoffRecoveryFailed.
	waitHandoffGrace
	// waitMergeCheck ends mergeCommitWait after the entry's start, a wait for the merge commit or an answer of the
	// check at the merge: once one more read after the end failed, the merge the entry names is recorded with the plan
	// not shown fresh (endMergeWait). It ends after a last read (readWaits).
	waitMergeCheck
	// waitMergeContent ends mergeContentWait after the entry's start, the reads of what a merge into the target
	// branch the settings name now brought (#771): the request ends NeedsIntervention with MergeContentUnreadable,
	// with the pull request the entry names, once one more read after the end failed (readWaits).
	waitMergeContent
)

// Reasons of condition GitReadRetry True, one per kind of wait; ReasonGitReadFailed is the wait without an end.
const (
	ReasonGitReadUntilDeadline     = "GitReadFailedBeforeDeadline"
	ReasonGitReadUntilHandoffGrace = "GitReadFailedBeforeHandoffGrace"
	ReasonGitReadUntilMergeWait    = "GitReadFailedBeforeMergeWait"
	ReasonGitReadUntilContentWait  = "GitReadFailedBeforeContentWait"
)

var gitWaitReasons = map[waitKind]string{
	waitNone:            api.ReasonGitReadFailed,
	waitRequestDeadline: ReasonGitReadUntilDeadline,
	waitHandoffGrace:    ReasonGitReadUntilHandoffGrace,
	waitMergeCheck:      ReasonGitReadUntilMergeWait,
	waitMergeContent:    ReasonGitReadUntilContentWait,
}

var gitWaitKinds = map[waitKind]api.GitWaitKind{
	waitNone:            api.GitWaitPaced,
	waitRequestDeadline: api.GitWaitRequestDeadline,
	waitHandoffGrace:    api.GitWaitHandoffGrace,
	waitMergeCheck:      api.GitWaitMergeCheck,
	waitMergeContent:    api.GitWaitMergeContent,
}

// reason is the reason of condition GitReadRetry for a wait of kind k.
func (k waitKind) reason() string { return gitWaitReasons[k] }

// kindOfEntry is the kind of an entry of status.gitWaits.
func kindOfEntry(w api.GitWait) waitKind {
	for k, a := range gitWaitKinds {
		if a == w.Kind {
			return k
		}
	}
	return waitNone
}

// gitWait is rr's entry of kind k, or nil.
func gitWait(rr *api.RemediationRequest, k waitKind) *api.GitWait {
	for i := range rr.Status.GitWaits {
		if rr.Status.GitWaits[i].Kind == gitWaitKinds[k] {
			return &rr.Status.GitWaits[i]
		}
	}
	return nil
}

// maxOperation is the schema's bound of status.gitWaits[].operation (MaxLength, characters).
const maxOperation = 256

// cutOperation bounds op, the fixed name of a read, to maxOperation characters: a name that carries a branch or a
// path may be longer, and an entry the API server refuses could never be recorded. A cut name ends in "...".
func cutOperation(op string) string {
	runes := []rune(op)
	if len(runes) <= maxOperation {
		return op
	}
	return string(runes[:maxOperation-3]) + "..."
}

// newWait is a whole entry of kind k for a read op that failed at now, with its end from the recorded state of rr:
// the deadline (waitRequestDeadline), the deadline and the grace (waitHandoffGrace), none (waitNone). The merge waits
// are not built here: they need their pull request (wholeWait), so ok is false for them, and for a deadline that rr
// does not record.
func newWait(rr *api.RemediationRequest, k waitKind, op string, now time.Time) (api.GitWait, bool) {
	w := api.GitWait{Kind: gitWaitKinds[k], Since: metav1.NewTime(now.Truncate(time.Second)), Operation: cutOperation(op)}
	switch k {
	case waitNone:
		return w, true
	case waitRequestDeadline, waitHandoffGrace:
		if rr.Status.Deadline == nil {
			return api.GitWait{}, false
		}
		ends := rr.Status.Deadline.Time
		if k == waitHandoffGrace {
			ends = ends.Add(handoffRecoveryGrace)
		}
		w.Ends = &metav1.Time{Time: ends}
		return w, true
	}
	return api.GitWait{}, false
}

// wholeWait completes tmpl, an entry of a merge wait with its pull request (readOfMergeCheck, readOfContent,
// mergeWaitEntry), for a read op that failed at now: its start, the operation, its end, mergeContentWait after the
// start for the content wait, mergeCommitWait after the start for the merge wait (both from the entry's own start,
// never from status.mergeCommitAwaitedSince). ok is false for an entry without its pull request.
func wholeWait(tmpl api.GitWait, op string, now time.Time) (api.GitWait, bool) {
	k := kindOfEntry(tmpl)
	if tmpl.PullRequest == nil || (k != waitMergeCheck && k != waitMergeContent) {
		return api.GitWait{}, false
	}
	w := *tmpl.DeepCopy()
	w.Since, w.Operation = metav1.NewTime(now.Truncate(time.Second)), cutOperation(op)
	wait := mergeContentWait
	if k == waitMergeCheck {
		wait = mergeCommitWait
	}
	w.Ends = &metav1.Time{Time: w.Since.Add(wait)}
	return w, true
}

// applies reports whether an entry of kind k still has something to end on rr: the request deadline only in
// PolicyChecked before a stale plan's cleanup or a handoff, the handoff grace only while the handoff is recovered,
// the merge waits only while the merge is not recorded.
func (k waitKind) applies(rr *api.RemediationRequest) bool {
	switch k {
	case waitRequestDeadline:
		return rr.Status.Phase == api.PhasePolicyChecked && staleMark(rr) == nil && intent(rr) == nil &&
			!recoveringHandoff(rr)
	case waitHandoffGrace:
		return recoveringHandoff(rr)
	case waitMergeCheck, waitMergeContent:
		return !mergeRecorded(rr)
	}
	return false
}

// boundEnd is when a wait of kind k bounds rr: its entry's end (end), or, before an entry is recorded, the end the
// entry would record from rr's status (newWait for the deadline kinds). ok is false when nothing bounds it: the read
// waits have no end before their entry, which counts from its own start.
func (k waitKind) boundEnd(rr *api.RemediationRequest, now time.Time) (time.Time, bool) {
	if gitWait(rr, k) != nil || !k.applies(rr) {
		return k.end(rr, now)
	}
	if k == waitRequestDeadline || k == waitHandoffGrace {
		if w, ok := newWait(rr, k, "bound", now); ok {
			return w.Ends.Time, true
		}
	}
	return time.Time{}, false
}

// end is when rr's entry of kind k ends, read from recorded state alone: the entry's end (status.gitWaits[].ends); ok
// is false when nothing bounds it (no entry, a Paced one, or a kind that no longer applies to rr). An entry whose start
// lies more than mergeCommitClockSlack after now (the clock went back) ends at now.
func (k waitKind) end(rr *api.RemediationRequest, now time.Time) (time.Time, bool) {
	w := gitWait(rr, k)
	if w == nil || w.Ends == nil || !k.applies(rr) {
		return time.Time{}, false
	}
	if w.Since.After(now.Add(mergeCommitClockSlack)) {
		return now, true
	}
	return w.Ends.Time, true
}

// withinWaits caps d, a wait a path chose after retryGitRead (a slower pace of its own), at the earliest end of
// rr's Git waits: a pace never carries a wait past its end.
func (r *RemediationRequestReconciler) withinWaits(rr *api.RemediationRequest, d time.Duration) time.Duration {
	now := r.now()
	for _, w := range rr.Status.GitWaits {
		if end, ok := kindOfEntry(w).end(rr, now); ok {
			d = min(d, max(end.Sub(now), boundPassedWait))
		}
	}
	return d
}

// expiringKey marks the context of the path that ends a wait (endPassedWait, expire).
type expiringKey struct{}

// expiringCtx marks ctx as the path that ends a wait: a read that fails in it starts no bounded wait of its own
// (retryGitRead records waitNone), so it is retried at the poll interval and never at once.
func expiringCtx(ctx context.Context) context.Context {
	return context.WithValue(ctx, expiringKey{}, true)
}

// expiring reports whether ctx is the path that ends a wait (expiringCtx).
func expiring(ctx context.Context) bool {
	v, _ := ctx.Value(expiringKey{}).(bool)
	return v
}

// gitWaitSummary sets condition GitReadRetry in conditions from waits: removed when there is none; otherwise True
// with the reason of the entry that ends first (a Paced entry ends never), the reads named in the message, and that
// entry's start as lastTransitionTime.
func gitWaitSummary(rr *api.RemediationRequest, now time.Time) {
	if len(rr.Status.GitWaits) == 0 {
		meta.RemoveStatusCondition(&rr.Status.Conditions, api.ConditionGitReadRetry)
		return
	}
	first := rr.Status.GitWaits[0]
	firstEnd, firstOK := kindOfEntry(first).end(rr, now)
	ops := make([]string, 0, len(rr.Status.GitWaits))
	for _, w := range rr.Status.GitWaits {
		if !slices.Contains(ops, w.Operation) {
			ops = append(ops, w.Operation)
		}
		if e, ok := kindOfEntry(w).end(rr, now); ok && (!firstOK || e.Before(firstEnd)) {
			first, firstEnd, firstOK = w, e, true
		}
	}
	want := metav1.Condition{Type: api.ConditionGitReadRetry, Status: metav1.ConditionTrue,
		Reason: kindOfEntry(first).reason(), Message: "Retrying Git read: " + strings.Join(ops, "; "),
		ObservedGeneration: rr.Generation, LastTransitionTime: first.Since}
	if c := meta.FindStatusCondition(rr.Status.Conditions, api.ConditionGitReadRetry); c != nil &&
		(c.Reason != want.Reason || !c.LastTransitionTime.Equal(&want.LastTransitionTime)) {
		meta.RemoveStatusCondition(&rr.Status.Conditions, api.ConditionGitReadRetry)
	}
	meta.SetStatusCondition(&rr.Status.Conditions, want)
}

// showsGitWait reports whether rr records a Git wait or shows condition GitReadRetry.
func showsGitWait(rr *api.RemediationRequest) bool {
	return len(rr.Status.GitWaits) > 0 || meta.FindStatusCondition(rr.Status.Conditions, api.ConditionGitReadRetry) != nil
}

// dropWait removes rr's entry of kind k in memory, with the summary.
func dropWait(rr *api.RemediationRequest, k waitKind, now time.Time) {
	rr.Status.GitWaits = slices.DeleteFunc(rr.Status.GitWaits, func(w api.GitWait) bool { return w.Kind == gitWaitKinds[k] })
	if len(rr.Status.GitWaits) == 0 {
		rr.Status.GitWaits = nil
	}
	gitWaitSummary(rr, now)
}

// readWaits are the kinds of wait for reads of the Git host (ninth decision of 2026-11-18 on #771, which replaced
// the stretch by recorded paused time of the eighth). One past its end ends only after one more read made once the
// end has passed: endPassedWait lets the reconcile make its reads (markLastRead), and endAfterLastRead ends what they
// left standing. Whatever kept the executor from reading (a pause, a restart, a leader change, an executor scaled to
// zero), the wait never ends without that read. Under a pause no read is made, so nothing ends. The request deadline
// and the handoff grace bound waiting for a person or another executor, not a read: they end with no read and count
// paused time, as on main.
var readWaits = []waitKind{waitMergeCheck, waitMergeContent}

// dropEnded removes, in memory, the entries of rr whose kind no longer applies to it (waitKind.applies: a merge
// recorded ends the merge waits, a final phase the deadline's), so the status write of a stop never leaves an entry
// that only the next reconcile would remove.
func dropEnded(rr *api.RemediationRequest, now time.Time) {
	n := len(rr.Status.GitWaits)
	rr.Status.GitWaits = slices.DeleteFunc(rr.Status.GitWaits, func(w api.GitWait) bool {
		k := kindOfEntry(w)
		return k != waitNone && !k.applies(rr)
	})
	if len(rr.Status.GitWaits) == n {
		return
	}
	if len(rr.Status.GitWaits) == 0 {
		rr.Status.GitWaits = nil
	}
	gitWaitSummary(rr, now)
}

// putGitWaits records adds in status.gitWaits: an entry of a kind that stands keeps its start and its pull request
// and takes the read that failed now; any other is added whole, as Paced when its kind no longer applies to the live
// request (waitKind.applies).
// Only the entries and their summary are written, in a patch with the resourceVersion read, retried on a conflict
// from a fresh read.
func (r *RemediationRequestReconciler) putGitWaits(ctx context.Context, rr *api.RemediationRequest, adds []api.GitWait) error {
	return r.patchGitWaits(ctx, rr, func(live *api.RemediationRequest) {
		for _, a := range adds {
			if k := kindOfEntry(a); k != waitNone && !k.applies(live) {
				// The live request has left the state this kind bounds (a step after the read ended it, or the cache
				// served an older copy): the retry is recorded as Paced, so it stays visible and has no end to pass.
				a = api.GitWait{Kind: gitWaitKinds[waitNone], Since: a.Since, Operation: a.Operation}
			}
			if w := gitWait(live, kindOfEntry(a)); w != nil {
				w.Operation = a.Operation
				continue
			}
			live.Status.GitWaits = append(live.Status.GitWaits, a)
		}
	})
}

// dropGitWaits removes the entries drop names, and the summary with the last.
func (r *RemediationRequestReconciler) dropGitWaits(ctx context.Context, rr *api.RemediationRequest,
	drop func(api.GitWait) bool) error {
	return r.patchGitWaits(ctx, rr, func(live *api.RemediationRequest) {
		live.Status.GitWaits = slices.DeleteFunc(live.Status.GitWaits, drop)
		if len(live.Status.GitWaits) == 0 {
			live.Status.GitWaits = nil
		}
	})
}

// patchGitWaits applies change to the entries of a fresh read of rr, sets the summary, and writes them alone; rr takes
// the entries and the summary. rr takes the new resourceVersion only when it was the object the server held
// (the same resourceVersion as the fresh read): a copy read earlier (the cache served an older one) keeps its
// own, so a later status write of it is refused as a conflict instead of putting back what the older read held (a
// released holdsTarget, an earlier phase). A request replaced under the same name is a cluster error.
func (r *RemediationRequestReconciler) patchGitWaits(ctx context.Context, rr *api.RemediationRequest,
	change func(live *api.RemediationRequest)) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		live := &api.RemediationRequest{}
		if err := r.reader().Get(ctx, client.ObjectKeyFromObject(rr), live); err != nil {
			return &clusterError{err: err}
		}
		if live.UID != rr.UID {
			return &clusterError{err: errors.New("RemediationRequest was replaced")}
		}
		current := live.ResourceVersion == rr.ResourceVersion
		base := live.DeepCopy()
		change(live)
		gitWaitSummary(live, r.now())
		if !equality.Semantic.DeepEqual(base.Status, live.Status) {
			if err := r.Status().Patch(ctx, live, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
				return fmt.Errorf("record the Git waits: %w", err)
			}
		}
		if current {
			rr.ResourceVersion = live.ResourceVersion
		}
		rr.Status.GitWaits = live.Status.GitWaits
		if c := meta.FindStatusCondition(live.Status.Conditions, api.ConditionGitReadRetry); c != nil {
			meta.RemoveStatusCondition(&rr.Status.Conditions, api.ConditionGitReadRetry)
			meta.SetStatusCondition(&rr.Status.Conditions, *c)
		} else {
			meta.RemoveStatusCondition(&rr.Status.Conditions, api.ConditionGitReadRetry)
		}
		return nil
	})
}

// endPassedWait is the check at the start of every reconcile of a request that is not being deleted. An entry whose
// kind no longer applies to the request (waitKind.end has no end) is removed. While writes are paused (the kill
// switch, read live) and an entry has passed its end, nothing ends: the request keeps its phase and waits for the
// pause (waitWhilePaused), as every other step does (sixth decision of 2026-11-18 on #771). Otherwise:
//   - every RequestDeadline and HandoffGrace entry past its end is ended before any read or step runs, by the kind's
//     own end, earliest first, from recorded state alone (the entries, the deadline, the executor's clock). Time
//     spent paused counts against them, as on main, so the first reconcile after a pause ends those that passed
//     meanwhile;
//   - a MergeCheck or MergeContent entry past its end (readWaits) is not ended here: the reconcile goes on and makes
//     its reads once more (markLastRead), and endAfterLastRead ends what they leave standing (ninth decision of
//     2026-11-18 on #771).
//
// done is true when the reconcile stops here; a merge wait ended by its mark lets the reconcile go on, which then
// records the merge. A failed status write, or a failed read of the settings, is returned for the error backoff.
func (r *RemediationRequestReconciler) endPassedWait(ctx context.Context, rr *api.RemediationRequest) (
	ctrl.Result, bool, error) {
	now := r.now()
	var stale []waitKind
	for _, w := range rr.Status.GitWaits {
		if k := kindOfEntry(w); k != waitNone {
			if _, ok := k.end(rr, now); !ok {
				stale = append(stale, k)
			}
		}
	}
	if len(stale) > 0 {
		// Nothing is left for these to end: removed, never ended again.
		if err := r.dropGitWaits(ctx, rr, func(w api.GitWait) bool { return slices.Contains(stale, kindOfEntry(w)) }); err != nil {
			return ctrl.Result{}, true, err
		}
	}
	if _, ok := passedWait(rr, now, false); ok {
		paused, err := r.writesPaused(ctx)
		if err != nil {
			return ctrl.Result{}, true, err
		}
		if paused {
			res, err := r.waitWhilePaused(ctx, rr)
			return res, true, err
		}
	}
	for {
		kind, ok := passedWait(rr, now, true)
		if !ok {
			break
		}
		res, done, err := r.endWait(ctx, rr, kind, now, "")
		if done || err != nil {
			return res, done, err
		}
	}
	if kinds := passedReadWaits(rr, now); len(kinds) > 0 {
		logf.FromContext(ctx).Info("Reading the Git host once more for RemediationRequest: a wait for a read passed "+
			"its end", "remediationRequest", client.ObjectKeyFromObject(rr), "waits", len(kinds))
		r.gitRetries().markLastRead(client.ObjectKeyFromObject(rr), kinds)
	}
	return ctrl.Result{}, false, nil
}

// passedWait is the kind of rr's entry past its end that ended first; with noRead, only among the kinds that end
// with no read (not readWaits).
func passedWait(rr *api.RemediationRequest, now time.Time, noRead bool) (waitKind, bool) {
	var kind waitKind
	var at time.Time
	found := false
	for _, w := range rr.Status.GitWaits {
		k := kindOfEntry(w)
		if noRead && slices.Contains(readWaits, k) {
			continue
		}
		if end, ok := k.end(rr, now); k != waitNone && ok && !now.Before(end) && (!found || end.Before(at)) {
			kind, at, found = k, end, true
		}
	}
	return kind, found
}

// passedReadWaits are the kinds of rr's read waits (readWaits) past their end at now, earliest end first.
func passedReadWaits(rr *api.RemediationRequest, now time.Time) []waitKind {
	var kinds []waitKind
	for _, k := range readWaits {
		if end, ok := k.end(rr, now); ok && !now.Before(end) {
			kinds = append(kinds, k)
		}
	}
	slices.SortStableFunc(kinds, func(a, b waitKind) int {
		ea, _ := a.end(rr, now)
		eb, _ := b.end(rr, now)
		return ea.Compare(eb)
	})
	return kinds
}

// endAfterLastRead runs after a reconcile of the request key that started with read waits past their end (kinds,
// markLastRead) and returned without an error: those reads were made once more. A read that answered went on as
// after any read, and a merge it recorded removed the entries. Each entry of kinds that still stands ends here, as a
// failure, with no retry: its last read failed (failed names it, from retryGitRead) or could not be made (the
// repository left the settings or cannot be opened). Under a pause nothing ends: no read was made. ended is false
// when nothing was left to end. A failed Kubernetes read or write is returned for the error backoff.
func (r *RemediationRequestReconciler) endAfterLastRead(ctx context.Context, key types.NamespacedName,
	kinds []waitKind, failed string) (ctrl.Result, bool, error) {
	rr := &api.RemediationRequest{}
	if err := r.reader().Get(ctx, key, rr); err != nil {
		return ctrl.Result{}, false, client.IgnoreNotFound(err)
	}
	if !rr.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, false, nil
	}
	now := r.now()
	var left []waitKind
	for _, k := range kinds {
		if end, ok := k.end(rr, now); ok && !now.Before(end) {
			left = append(left, k)
		}
	}
	if len(left) == 0 {
		return ctrl.Result{}, false, nil
	}
	switch paused, err := r.writesPaused(ctx); {
	case err != nil:
		return ctrl.Result{}, false, err
	case paused:
		// A pause set while the reconcile ran: its reads stopped at the pause, so nothing ends.
		return ctrl.Result{}, false, nil
	}
	why := "no read of the Git host could be made after the wait (it could not start, for example because the " +
		"repository of the plan cannot be opened or is not in the settings)"
	if failed != "" {
		why = "the read after the wait failed: " + failed
	}
	res, _, err := r.endWait(ctx, rr, left[0], now, why)
	return r.lookAgainWhileHeld(rr, res, err), true, err
}

// endWait ends rr's entry of kind, past its end: a RequestDeadline or HandoffGrace entry from endPassedWait, a read
// wait from endAfterLastRead, with why saying how its last read failed.
func (r *RemediationRequestReconciler) endWait(ctx context.Context, rr *api.RemediationRequest, kind waitKind,
	now time.Time, why string) (ctrl.Result, bool, error) {
	entry := *gitWait(rr, kind)
	log := logf.FromContext(ctx).WithValues("remediationRequest", client.ObjectKeyFromObject(rr), "wait", entry.Kind)
	if why != "" {
		log.Info("Ended the Git wait of RemediationRequest: its bound passed and " + why)
	} else {
		log.Info("Ended the Git wait of RemediationRequest: its bound passed")
	}
	ctx = expiringCtx(ctx)
	// The wait ends in the write of its end below; it is not left to end again.
	dropWait(rr, kind, now)
	// Each end reads nothing from the Git host and returns no retry of one: only a failed write or read of the
	// Kubernetes API is returned, for the error backoff.
	switch kind {
	case waitRequestDeadline:
		res, err := r.expire(ctx, rr)
		return res, true, err
	case waitHandoffGrace:
		return ctrl.Result{}, true, r.endRecoveryFailed(ctx, rr)
	case waitMergeCheck:
		return ctrl.Result{}, true, r.ignoreStopped(r.endMergeWait(ctx, rr, entry, why))
	case waitMergeContent:
		return ctrl.Result{}, true, r.ignoreStopped(r.endContentUnreadable(ctx, rr, entry, why))
	}
	return ctrl.Result{}, false, nil
}

// endMergeWait ends a request whose merge wait passed (waitMergeCheck, entry w) and whose last read after it failed
// (last says how): the host did not report the merge commit, or the check at the merge did not answer, within
// mergeCommitWait. It decides from the entry and the recorded status alone, with no read of the Git host:
//   - the pull request w names is the recorded one (number and head): its merge is recorded, the plan marked not
//     shown fresh (PlanChanged) unless it carries a stale mark (or the mark of a cancel for its Incident), and the
//     merge gets its receipt only when w records a merge commit (a wait for the check at the merge, which runs only
//     after what the merge brought was checked), else the NoMergeReceipt mark, which ends it Inconclusive. A merge
//     into the branch the settings name now (w's base branch, a retargeted merge) is recorded into that branch with
//     the stale mark SettingsChanged when w records its merge commit, and ends NeedsIntervention with
//     MergeContentUnreadable without one (what it brought was never read);
//   - any other pull request (none recorded, another number or head, found on the adopt path): nothing proves what
//     the merge brought, so it ends NeedsIntervention without a receipt, MergedWithForeignChange (BranchDiverged when
//     no merge commit was reported, as checkMerged answers) or MergeContentUnreadable into another branch.
func (r *RemediationRequestReconciler) endMergeWait(ctx context.Context, rr *api.RemediationRequest,
	w api.GitWait, last string) error {
	pr := w.PullRequest
	cr := providers.ChangeRequest{Number: pr.Number, URL: pr.URL, HeadSHA: pr.HeadSHA, BaseBranch: string(pr.BaseBranch),
		MergeCommit: pr.MergeCommitSHA, State: providers.ChangeRequestMerged}
	reason, cancelled := cancelledForIncident(rr)
	why := "it did not answer within the wait for the merge"
	if pr.MergeCommitSHA == "" {
		why = "the Git host reported no merge commit within the wait for the merge"
	}
	if last != "" {
		why += ", and " + last
	}
	recorded, source := rr.Status.PullRequest, rr.Status.Source
	ours := recorded != nil && recorded.Number == cr.Number && recorded.HeadSHA == cr.HeadSHA && source != nil
	branch := api.GitBranch(cr.BaseBranch)
	other := source != nil && branch != "" && branch != source.TargetBranch
	unread := &renderRefusal{reason: ReasonMergeContentUnreadable,
		message: "what the merge brought could not be checked within " + mergeCommitWait.String() + " (" + why + ")"}
	if !ours || (other && pr.MergeCommitSHA == "") {
		recordFound(rr, cr)
		if cancelled {
			keepCancelReason(rr, reason, "found merged afterwards")
		}
		if other {
			markStaleForBranch(rr, branch)
			return r.mergedForeignInto(ctx, rr, branch, unread)
		}
		ref := unread
		if pr.MergeCommitSHA == "" {
			// As checkMerged answers after the wait for the merge commit: without it nothing can be checked.
			ref = &renderRefusal{reason: ReasonBranchDiverged, message: "The Git host reported no full merge commit " +
				"ID for the merged pull request, so what the merge brought cannot be checked"}
		}
		if !markedStale(rr) && !cancelled {
			markNotShownFresh(rr, why)
		}
		return r.mergedForeign(ctx, rr, ref)
	}
	switch {
	case cancelled:
		markCancelledMerge(rr, reason)
	case other:
		markStaleForBranch(rr, branch)
	case !markedStale(rr):
		markNotShownFresh(rr, why)
		if r.Recorder != nil {
			r.Recorder.Event(rr, corev1.EventTypeWarning, ReasonStalePlanMerged, notShownFreshMessage(why))
		}
	}
	message := "The pull request was merged"
	if other {
		message = "The pull request was merged into " + string(branch) + ", the target branch the settings name now"
	} else {
		branch = source.TargetBranch
	}
	markClosed(rr, ReasonChangeRequestMerged, "was merged")
	recordMergeInto(ctx, rr, cr, branch)
	return r.stop(ctx, rr, api.PhaseMerged, ReasonChangeRequestMerged, message)
}

// endContentUnreadable ends a request whose reads of what a retargeted merge brought kept failing for
// mergeContentWait (waitMergeContent, entry w), and whose last read after it failed (last says how): NeedsIntervention with MergeContentUnreadable, the stale mark, no
// receipt. The pull request w names is recorded when the status holds none (its status write was lost and the read
// found it by the request's marker and checked its head), so the end never depends on a write that may have been
// lost. The branch is the one the settings name now; when the settings cannot say it, the message says so instead.
// Only a failed Kubernetes read is returned, for the error backoff.
func (r *RemediationRequestReconciler) endContentUnreadable(ctx context.Context, rr *api.RemediationRequest,
	w api.GitWait, last string) error {
	if rr.Status.PullRequest == nil && w.PullRequest != nil {
		recordFound(rr, providers.ChangeRequest{Number: w.PullRequest.Number, URL: w.PullRequest.URL,
			HeadSHA: w.PullRequest.HeadSHA, State: providers.ChangeRequestMerged})
	}
	// The branch the merge went into, as the read that started the wait saw it; an entry written without it (before
	// the upgrade) takes the target branch the settings name now. With neither, the messages name no branch.
	var branch api.GitBranch
	if w.PullRequest != nil {
		branch = w.PullRequest.BaseBranch
	}
	if branch == "" {
		switch repo, _, err := r.planEntry(ctx, rr); {
		case err == nil || errors.Is(err, errTargetBranchChanged):
			branch = repo.TargetBranch
		case !errors.Is(err, errRepositoryGone) && !errors.Is(err, freshness.ErrPaused):
			// The read of the settings failed (a Kubernetes read): retried with the error backoff.
			return err
		}
	}
	markStaleForBranch(rr, branch)
	return r.mergedForeignInto(ctx, rr, branch, &renderRefusal{reason: ReasonMergeContentUnreadable,
		message: contentUnreadableMessage(last)})
}

// contentUnreadableMessage is the message of MergeContentUnreadable at the end of the content wait, last saying how
// the read after the wait failed.
func contentUnreadableMessage(last string) string {
	m := "the reads of the Git host kept failing for " + mergeContentWait.String()
	if last != "" {
		m += ", and " + last
	}
	return m
}
