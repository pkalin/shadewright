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
	"slices"
	"strings"
	"sync"
	"time"

	api "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/gitops/sourcemap"
	"github.com/pkalin/shadewright/internal/policy/freshness"
	"github.com/pkalin/shadewright/internal/providers"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// gitReadError identifies an error at the provider boundary, before any Kubernetes operation can wrap it.
// It carries a fixed method name only; the host's answer never enters a condition or a log.
type gitReadError struct {
	op  string
	err error
}

func (e *gitReadError) Error() string { return e.op + " failed" }
func (e *gitReadError) Unwrap() error { return e.err }

// waitBound is what bounds the retry of a failed Git read, given by the path that made the read with every call of
// retryGitRead, so a call site cannot leave it out. It names, never computes, the end of the wait:
//   - a kind (boundBy): a wait whose end is recorded state of the request (waitKind). The kind is recorded with the
//     wait, as the reason of condition GitReadRetry, and the check at the start of every reconcile (endPassedWait)
//     ends a wait past its end before any read or step runs. The site does not look at the end itself.
//   - a time to look again (paceAt, paceWithin), which ends nothing: the step that runs then reads again and
//     decides (the end of a delivery window, the full recheck's period, the bound of a cancel or a cleanup that its
//     own condition records).
//   - no end at all (unboundedRead), for the reason why.
//
// The retry is never later than the end of the kind nor the time to look again.
type waitBound struct {
	kind   waitKind
	paceAt time.Time
	paceIn time.Duration
	why    noBoundReason
}

// noBoundReason says why a failed Git read has no end to wait for. Each value names its path.
type noBoundReason string

const (
	// noBoundPolicyCheck: a request not decided yet has no deadline; it is checked again after the wait.
	noBoundPolicyCheck noBoundReason = "the policy check has no deadline before its decision"
	// noBoundHeldLock: a stopped request keeps its target lock until a read settles it, however long that takes.
	noBoundHeldLock noBoundReason = "the target lock is kept until a read settles it"
	// noBoundStaleCleanup: the cleanup of a stale plan never expires (staleMark).
	noBoundStaleCleanup noBoundReason = "the cleanup of a stale plan has no deadline"
	// noBoundAfterStop: a request that no longer progresses is read again until the read works.
	noBoundAfterStop noBoundReason = "a stopped request is read again until the read works"
	// noBoundCancelWatch: a cancel whose close cannot be made after a change of the target branch reads its pull
	// requests on both branches until a read settles them (cancelLook).
	noBoundCancelWatch noBoundReason = "a cancel after a change of the target branch is watched until a read settles it"
	// noBoundRefusedWatch: a refusal while a pull request waits is watched until the next full recheck.
	noBoundRefusedWatch noBoundReason = "a refused freshness check is watched without an end"
)

// boundPassedWait is the wait of a retry whose end passed while the reconcile's reads ran: the check at the start
// of the next reconcile (endPassedWait) ends the wait at once.
const boundPassedWait = time.Nanosecond

// boundBy is the bound of kind k, whose end the request's recorded state holds (waitKind.end).
func boundBy(k waitKind) waitBound { return waitBound{kind: k} }

// paceAt is a time to look again at t, which ends nothing.
func paceAt(t time.Time) waitBound { return waitBound{paceAt: t} }

// paceWithin is a time to look again d from when the wait is computed (a period such as the full recheck's, or
// what is left of a bound a condition of the path records), which ends nothing.
func paceWithin(d time.Duration) waitBound { return waitBound{paceIn: max(d, time.Nanosecond)} }

// unboundedRead is no bound, for the reason why.
func unboundedRead(why noBoundReason) waitBound { return waitBound{why: why} }

// at adds a time to look again at t to b.
func (b waitBound) at(t time.Time) waitBound {
	b.paceAt = t
	return b
}

// within adds a time to look again d from when the wait is computed to b.
func (b waitBound) within(d time.Duration) waitBound {
	b.paceIn = max(d, time.Nanosecond)
	return b
}

// lookBy is the earliest of the end of b's kind for rr and its times to look again, from now; ok is false when
// nothing bounds b.
func (b waitBound) lookBy(rr *api.RemediationRequest, now time.Time) (time.Time, bool) {
	by, ok := b.kind.boundEnd(rr, now)
	if !b.paceAt.IsZero() && (!ok || b.paceAt.Before(by)) {
		by, ok = b.paceAt, true
	}
	if b.paceIn > 0 && (!ok || now.Add(b.paceIn).Before(by)) {
		by, ok = now.Add(b.paceIn), true
	}
	return by, ok
}

// requestBound is the bound of a request's own deadline: a request in PolicyChecked expires at status.deadline
// (waitRequestDeadline), unless the cleanup of its stale plan has started (staleMark), which never expires.
func requestBound(rr *api.RemediationRequest) waitBound {
	if rr.Status.Phase == api.PhasePolicyChecked && rr.Status.Deadline != nil {
		switch {
		case staleMark(rr) != nil:
			return unboundedRead(noBoundStaleCleanup)
		case recoveringHandoff(rr):
			// Past the deadline the recovery has its grace (retryRecovery); before it, the deadline is looked at.
			return boundBy(waitHandoffGrace).at(rr.Status.Deadline.Time)
		case intent(rr) != nil:
			// A handoff in flight is not expired by the deadline (waitHandedOver): it is looked at then.
			return paceAt(rr.Status.Deadline.Time)
		}
		return boundBy(waitRequestDeadline)
	}
	return unboundedRead(noBoundAfterStop)
}

// kindedReadError is a failed Git read that belongs to a wait of its own, for a read made below the path that
// calls retryGitRead (what a retargeted merge brought, the check at the merge): retryGitRead records that wait
// besides the caller's. It carries the whole entry (readOfMergeCheck, readOfContent), so the wait is recorded with
// everything its end needs.
type kindedReadError struct {
	wait api.GitWait
	err  error
}

func (e *kindedReadError) Error() string { return e.err.Error() }
func (e *kindedReadError) Unwrap() error { return e.err }

// readOfMergeCheck marks err, a failed Git read of the check at the merge of cr, as one of the merge wait about cr.
func readOfMergeCheck(cr providers.ChangeRequest, err error) error {
	if err == nil {
		return nil
	}
	return &kindedReadError{wait: api.GitWait{Kind: api.GitWaitMergeCheck, PullRequest: waitPullRequest(cr)}, err: err}
}

// readOfContent marks err, a failed read of what the merge of cr brought, as one of the content wait about cr: the
// pull request the read is about, recorded or found by the request's marker and checked. It is the only way a
// content wait is built, so none is recorded without its pull request.
func readOfContent(cr providers.ChangeRequest, err error) error {
	if err == nil {
		return nil
	}
	return &kindedReadError{err: err, wait: api.GitWait{Kind: api.GitWaitMergeContent, PullRequest: waitPullRequest(cr)}}
}

// waitPullRequest is what an entry of status.gitWaits records of cr, merged: its number, URL and head, the branch
// it went into and its merge commit when the host reported a full one.
func waitPullRequest(cr providers.ChangeRequest) *api.GitWaitPullRequest {
	pr := &api.GitWaitPullRequest{Number: cr.Number, URL: cr.URL, HeadSHA: cr.HeadSHA,
		BaseBranch: api.GitBranch(cr.BaseBranch)}
	if shaPattern.MatchString(cr.MergeCommit) {
		pr.MergeCommitSHA = cr.MergeCommit
	}
	return pr
}

// keepGitRead is boundErr for a step that ended on a failed Git read: a read that ran out of the step's time is
// still that read (with the wait it belongs to, readOfMergeCheck or readOfContent), retried as one, so its wait is started, paced and ended
// like any other failure of it. Only a step whose own error is a failed Git read keeps it; a step that failed
// otherwise (a Kubernetes read, a write), and the caller's cancellation, get boundErr's answer as it is.
func keepGitRead(parent, phase context.Context, name string, err error) error {
	bounded := boundErr(parent, phase, name, err)
	if bounded == err || parent.Err() != nil || !gitReadFailure(err) {
		return bounded
	}
	read := readFailure(readOp(err), bounded)
	if e, ok := errors.AsType[*kindedReadError](err); ok {
		return &kindedReadError{wait: e.wait, err: read}
	}
	return read
}

func readFailure(op string, err error) error {
	if err == nil {
		return nil
	}
	return &gitReadError{op: op, err: err}
}

func gitReadFailure(err error) bool {
	if err == nil || isClusterError(err) {
		return false
	}
	if _, ok := errors.AsType[*gitReadError](err); ok {
		return true
	}
	if _, ok := errors.AsType[*sourcemap.RefusedError](err); ok {
		return true
	}
	if e, ok := errors.AsType[*sourcemap.TransientError](err); ok {
		// The source mapper sanitizes away the underlying error. Its Git operations use these fixed
		// prefixes; Kubernetes failures use "get <kind>" or "<kind> ... changed" and stay errors.
		return strings.HasPrefix(e.Op, "read ") || strings.HasPrefix(e.Op, "list ") || strings.HasPrefix(e.Op, "open repository ")
	}
	if pe, ok := errors.AsType[*freshness.ProviderError](err); ok {
		// Direct calls in the content and source helpers already use fixed operation names.
		if pe.Op == "open the repository" {
			return true
		}
		for _, prefix := range []string{"read ", "find ", "list ", "check ", "count "} {
			if strings.HasPrefix(pe.Op, prefix) {
				return true
			}
		}
	}
	return false
}

// readOp is the fixed name of the read err failed, for the condition and the log.
func readOp(err error) string {
	if e, ok := errors.AsType[*gitReadError](err); ok {
		return e.op
	} else if e, ok := errors.AsType[*freshness.ProviderError](err); ok {
		return e.Op
	} else if e, ok := errors.AsType[*sourcemap.TransientError](err); ok {
		return e.Op
	} else if e, ok := errors.AsType[*sourcemap.RefusedError](err); ok {
		return e.Op
	}
	return "read the Git host"
}

// retryGitRead is the common result of a failed Git read, called by the path that made the read. It starts or
// continues waits and never decides whether one is over (endPassedWait does, at the start of the next reconcile, and
// endAfterLastRead after it). In a reconcile that makes the last read of a read wait past its end (markLastRead) a
// failed read is no retry: it is noted (failedRead) and nothing is recorded, and the wait ends after the reconcile.
// Otherwise:
//   - the caller's kind (bound) gets its entry in status.gitWaits (a recovery that waits on a refusal also the
//     HandoffGrace entry of its deadline, waitKind.recorded), and so does the wait err belongs to
//     (readOfMergeCheck, readOfContent), each its own: an entry that stands keeps its start, and no other entry is
//     touched. In the path that ends a wait (expiring) only a Paced entry is recorded;
//   - every entry records its end, from the request's status or the merge it is about (newWait, wholeWait), and an
//     operation cut to the schema's bound (cutOperation), so the API server accepts it whatever branch or path it
//     names;
//   - condition GitReadRetry summarizes the entries (gitWaitSummary);
//   - the retry comes after the poll interval or the delay the host asked for, whichever is longer, and never later
//     than the end of any entry nor the time to look again; an end that passed while the reads ran leaves
//     boundPassedWait, so the next reconcile ends the wait first. In the path that ends a wait (expiring) only an
//     end still ahead shortens it: the bound being ended and ends already past are not waited for again.
//
// A failed status write is returned for a retry with the error backoff: a wait is never left unrecorded. Any error
// that is not a failed Git read (a Kubernetes read or write, the cancellation of the reconcile) is returned
// unchanged, for the error backoff.
func (r *RemediationRequestReconciler) retryGitRead(ctx context.Context, rr *api.RemediationRequest, err error,
	bound waitBound) (reconcile.Result, error) {
	if ctx.Err() != nil || !gitReadFailure(err) {
		return reconcile.Result{}, err
	}
	now := r.now()
	op := readOp(err)
	key := client.ObjectKeyFromObject(rr)
	if kinds := r.gitRetries().lastReads(key); slices.ContainsFunc(kinds, func(k waitKind) bool {
		end, ok := k.end(rr, now)
		return ok && !now.Before(end)
	}) {
		// The last read past the end of a read wait failed: nothing is retried, the wait ends after this reconcile
		// (endAfterLastRead), which names this read.
		r.gitRetries().failedRead(key, op)
		logf.FromContext(ctx).Info("A last Git read of RemediationRequest failed after its wait passed its end",
			"remediationRequest", key, "operation", op)
		return reconcile.Result{}, nil
	}
	var adds []api.GitWait
	if expiring(ctx) {
		// The path that ends a wait: nothing it reads starts a bounded wait, and its caller's bound has passed.
		w, _ := newWait(rr, waitNone, op, now)
		adds = append(adds, w)
		bound.kind = waitNone
	} else {
		if e, ok := errors.AsType[*kindedReadError](err); ok {
			if w, ok := wholeWait(e.wait, op, now); ok {
				adds = append(adds, w)
			}
		}
		for i, k := range bound.kind.recorded() {
			if w, ok := newWait(rr, k, op, now); ok {
				adds = append(adds, w)
			} else if i == 0 && k != waitNone {
				// The bound cannot be recorded (no deadline): the read is retried at the pace, never without a record.
				w, _ := newWait(rr, waitNone, op, now)
				adds = append(adds, w)
			}
		}
	}
	if e := r.putGitWaits(ctx, rr, adds); e != nil {
		return reconcile.Result{}, e
	}
	r.gitRetries().mark(key)
	d := max(r.pullRequestPollInterval(), retryAfterOf(err))
	if hostDelay, ok := providers.RetryAfter(err); ok {
		d = max(d, hostDelay)
	}
	// In the path that ends a wait (expiring) the caller's bound has passed and is being ended: the retry is not
	// pulled to it, nor to another end already past, which would only come back at once (boundPassedWait) to read
	// again. Only an end still ahead shortens it there.
	var by time.Time
	ok := false
	if !expiring(ctx) {
		by, ok = bound.lookBy(rr, now)
	}
	for _, w := range rr.Status.GitWaits {
		end, has := kindOfEntry(w).end(rr, now)
		if has && (!expiring(ctx) || end.After(now)) && (!ok || end.Before(by)) {
			by, ok = end, true
		}
	}
	if ok {
		d = min(d, max(by.Sub(now), boundPassedWait))
	}
	logf.FromContext(ctx).Info("Retried a Git read of RemediationRequest", "remediationRequest",
		client.ObjectKeyFromObject(rr), "operation", op, "waits", len(rr.Status.GitWaits), "retryAfter", d)
	return reconcile.Result{RequeueAfter: d}, nil
}

// gitRetryMarks holds, per request, what its reconcile in progress knows about condition GitReadRetry: whether
// it scheduled a Git read retry (retryGitRead), and whether the request it read at its start showed the
// condition. Reconcile empties a request's entry before it runs and, when the reconcile ends without an error and
// without a retry, removes a condition it saw: the reads that ran worked. A reconcile that saw no condition makes
// no extra read for it. It holds no failure and changes no result.
type gitRetryMarks struct {
	mu   sync.Mutex
	keys map[types.NamespacedName]gitRetryMark
}

// gitRetryMark is one request's entry in gitRetryMarks.
type gitRetryMark struct {
	// retried: this reconcile scheduled a Git read retry.
	retried bool
	// shown: the request read at the start of this reconcile carried condition GitReadRetry.
	shown bool
	// lastRead: the read waits past their end at the start of this reconcile (endPassedWait), whose reads this
	// reconcile makes once more. The rest is the evidence about those reads that endAfterLastRead decides on (tenth
	// decision of 2026-11-18 on #771): failed names the first Git read of this reconcile that failed (retryGitRead);
	// unstartable says why no read could start (the repository of the plan left the settings or cannot be used,
	// noteUnstartable from setUnreadable); answered, that a read a wait waits for answered (noteAnswered); paused, that
	// the reconcile met a pause of the kill switch (waitWhilePaused), so it may have read nothing; disagreed, that
	// answers about the pull request disagreed (changeOf), which outweighs any answer.
	lastRead    []waitKind
	failed      string
	unstartable string
	answered    bool
	disagreed   bool
	paused      bool
}

func (m *gitRetryMarks) update(key types.NamespacedName, f func(*gitRetryMark)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.keys == nil {
		m.keys = map[types.NamespacedName]gitRetryMark{}
	}
	e := m.keys[key]
	f(&e)
	m.keys[key] = e
}

// mark records that key's reconcile scheduled a Git read retry.
func (m *gitRetryMarks) mark(key types.NamespacedName) {
	m.update(key, func(e *gitRetryMark) { e.retried = true })
}

// markLastRead records that key's reconcile makes the reads of the read waits kinds once more, past their end.
func (m *gitRetryMarks) markLastRead(key types.NamespacedName, kinds []waitKind) {
	m.update(key, func(e *gitRetryMark) { e.lastRead = kinds })
}

// lastReads are the read waits past their end whose reads key's reconcile makes once more (markLastRead).
func (m *gitRetryMarks) lastReads(key types.NamespacedName) []waitKind {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.keys[key].lastRead
}

// failedRead records op, a Git read of key's reconcile that failed, when it is the first.
func (m *gitRetryMarks) failedRead(key types.NamespacedName, op string) {
	m.update(key, func(e *gitRetryMark) {
		if e.failed == "" {
			e.failed = op
		}
	})
}

// noteUnstartable records why no read of the Git host could start for key's reconcile, when it is the first such note.
func (m *gitRetryMarks) noteUnstartable(key types.NamespacedName, why string) {
	m.update(key, func(e *gitRetryMark) {
		if e.unstartable == "" {
			e.unstartable = why
		}
	})
}

// noteAnswered records that a read a wait for a read waits for (the pull request's merge, the check at the merge, what
// the merge brought) answered in key's reconcile.
func (m *gitRetryMarks) noteAnswered(key types.NamespacedName) {
	m.update(key, func(e *gitRetryMark) { e.answered = true })
}

// noteDisagreed records that answers about the pull request disagreed in key's reconcile (answeredUnknown).
func (m *gitRetryMarks) noteDisagreed(key types.NamespacedName) {
	m.update(key, func(e *gitRetryMark) { e.disagreed = true })
}

// notePaused records that key's reconcile met a pause of the kill switch.
func (m *gitRetryMarks) notePaused(key types.NamespacedName) {
	m.update(key, func(e *gitRetryMark) { e.paused = true })
}

// shown records that the request key's reconcile read carries condition GitReadRetry.
func (m *gitRetryMarks) shown(key types.NamespacedName) {
	m.update(key, func(e *gitRetryMark) { e.shown = true })
}

// take returns key's entry and forgets it.
func (m *gitRetryMarks) take(key types.NamespacedName) gitRetryMark {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.keys[key]
	delete(m.keys, key)
	return e
}

func (r *RemediationRequestReconciler) gitRetries() *gitRetryMarks {
	r.gitRetryOnce.Do(func() {
		if r.gitRetryMarks == nil {
			r.gitRetryMarks = &gitRetryMarks{}
		}
	})
	return r.gitRetryMarks
}

// clearGitReadRetry runs after a reconcile that ended without an error and saw or recorded a Git wait. With clear
// (the reconcile scheduled no Git read retry) it removes every wait but the merge waits: the content wait, which only
// a successful read of what the merge brought removes (clearContentWait), and the merge wait, which ends at its end
// or with the merge it waits for recorded: a reconcile that did not make those reads says nothing about them. left is how long until the earliest end of the waits that stand, zero when none has an end: the
// reconcile is looked at again by then, whatever pace it chose, so a wait that stands always reaches its end.
func (r *RemediationRequestReconciler) clearGitReadRetry(ctx context.Context, key types.NamespacedName, clear bool) (
	time.Duration, error) {
	// The cache tells whether there is anything to remove; dropGitWaits reads the request again before it writes,
	// and a wait this reconcile recorded has its retry bounded already (retryGitRead).
	rr := &api.RemediationRequest{}
	if err := r.Get(ctx, key, rr); err != nil {
		return 0, client.IgnoreNotFound(err)
	}
	if !rr.DeletionTimestamp.IsZero() {
		return 0, nil
	}
	shown := meta.FindStatusCondition(rr.Status.Conditions, api.ConditionGitReadRetry) != nil
	mergeWait := func(w api.GitWait) bool {
		return w.Kind == api.GitWaitMergeContent || w.Kind == api.GitWaitMergeCheck
	}
	onlyMerge := len(rr.Status.GitWaits) > 0 && !slices.ContainsFunc(rr.Status.GitWaits, func(w api.GitWait) bool {
		return !mergeWait(w)
	})
	if clear && (len(rr.Status.GitWaits) > 0 || shown) && (!onlyMerge || !shown) {
		// A request gone from the API server (its finalizer removed after the read) has nothing left to clear.
		err := r.dropGitWaits(ctx, rr, func(w api.GitWait) bool { return !mergeWait(w) })
		if apierrors.IsNotFound(err) {
			return 0, nil
		}
		if err != nil {
			return 0, err
		}
	}
	if meta.IsStatusConditionTrue(rr.Status.Conditions, api.ConditionWritesPaused) {
		// The kill switch holds the request (waitWhilePaused): no wait ends while it lasts (endPassedWait), so an end
		// that passed is not looked at sooner than the pause's own recheck.
		return 0, nil
	}
	const none = time.Duration(1<<63 - 1)
	if left := r.withinWaits(rr, none); left < none {
		return left, nil
	}
	return 0, nil
}
