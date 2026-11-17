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
	"strings"
	"sync"
	"time"

	api "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/gitops/sourcemap"
	"github.com/pkalin/shadewright/internal/policy/freshness"
	"github.com/pkalin/shadewright/internal/providers"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
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
	by, ok := b.kind.end(rr, now)
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

// kindedReadError is a failed Git read that names the kind of wait it belongs to, for a read made below the path
// that calls retryGitRead (what a retargeted merge brought, the check at the merge): retryGitRead records that kind,
// or the caller's when its end comes first.
type kindedReadError struct {
	kind waitKind
	err  error
}

func (e *kindedReadError) Error() string { return e.err.Error() }
func (e *kindedReadError) Unwrap() error { return e.err }

// readOfKind marks err, a failed Git read, as one of a wait of kind k.
func readOfKind(k waitKind, err error) error {
	if err == nil {
		return nil
	}
	return &kindedReadError{kind: k, err: err}
}

// keepGitRead is boundErr for a step that ended on a failed Git read: a read that ran out of the step's time is
// still that read (with the kind of its wait, readOfKind), retried as one, so its wait is started, paced and ended
// like any other failure of it. Only a step whose own error is a failed Git read keeps it; a step that failed
// otherwise (a Kubernetes read, a write), and the caller's cancellation, get boundErr's answer as it is.
func keepGitRead(parent, phase context.Context, name string, err error) error {
	bounded := boundErr(parent, phase, name, err)
	if bounded == err || parent.Err() != nil || !gitReadFailure(err) {
		return bounded
	}
	read := readFailure(readOp(err), bounded)
	if e, ok := errors.AsType[*kindedReadError](err); ok {
		return &kindedReadError{kind: e.kind, err: read}
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
// continues a wait and never decides whether one is over (endPassedWait does, at the start of the next reconcile):
//   - the kind of the wait is the caller's (bound), or the one err names (readOfKind) when its end comes first; in
//     the path that ends a wait (expiring) there is none;
//   - a kind with a clock of its own (waitKind.startClock) gets its start recorded when it has none;
//   - condition GitReadRetry says what is retried, with the kind as its reason and its lastTransitionTime the start
//     on the executor's clock (kept while the kind stays);
//   - the retry comes after the poll interval or the delay the host asked for, whichever is longer, and never later
//     than the end of the kind or the time to look again; an end that passed while the reads ran leaves
//     boundPassedWait, so the next reconcile ends the wait first.
//
// A failed status write (the clock, the condition) is returned for a retry with the error backoff: a wait is never
// left unrecorded. Any error that is not a failed Git read (a Kubernetes read or write, the cancellation of the
// reconcile) is returned unchanged, for the error backoff.
func (r *RemediationRequestReconciler) retryGitRead(ctx context.Context, rr *api.RemediationRequest, err error,
	bound waitBound) (reconcile.Result, error) {
	if ctx.Err() != nil || !gitReadFailure(err) {
		return reconcile.Result{}, err
	}
	now := r.now()
	kind := bound.kind
	if e, ok := errors.AsType[*kindedReadError](err); ok {
		kind = earlierKind(rr, now, kind, e.kind)
	}
	if expiring(ctx) {
		kind = waitNone
	}
	if werr := r.startClock(ctx, rr, kind); werr != nil {
		return reconcile.Result{}, werr
	}
	op := readOp(err)
	want := &metav1.Condition{Type: api.ConditionGitReadRetry, Status: metav1.ConditionTrue, Reason: kind.reason(),
		Message: "Retrying Git read: " + op, ObservedGeneration: rr.Generation, LastTransitionTime: metav1.NewTime(now)}
	if e := r.putGitReadRetry(ctx, rr, want); e != nil {
		return reconcile.Result{}, e
	}
	r.gitRetries().mark(client.ObjectKeyFromObject(rr))
	d := max(r.pullRequestPollInterval(), retryAfterOf(err))
	if hostDelay, ok := providers.RetryAfter(err); ok {
		d = max(d, hostDelay)
	}
	bound.kind = kind
	if by, ok := bound.lookBy(rr, now); ok {
		d = min(d, max(by.Sub(now), boundPassedWait))
	}
	logf.FromContext(ctx).Info("Retried a Git read of RemediationRequest", "remediationRequest",
		client.ObjectKeyFromObject(rr), "operation", op, "wait", kind.reason(), "retryAfter", d)
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

// clearGitReadRetry removes condition GitReadRetry after a reconcile that ended without an error and scheduled no
// Git read retry.
func (r *RemediationRequestReconciler) clearGitReadRetry(ctx context.Context, key types.NamespacedName) error {
	// The cache tells whether there is anything to remove; putGitReadRetry reads the request again before it writes.
	rr := &api.RemediationRequest{}
	if err := r.Get(ctx, key, rr); err != nil {
		return client.IgnoreNotFound(err)
	}
	if meta.FindStatusCondition(rr.Status.Conditions, api.ConditionGitReadRetry) == nil {
		return nil
	}
	// A request gone from the API server (its finalizer removed after the cache read) has nothing left to clear.
	if err := r.putGitReadRetry(ctx, rr, nil); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// putGitReadRetry writes only the retry condition and retries conflicts from a fresh API read.
func (r *RemediationRequestReconciler) putGitReadRetry(ctx context.Context, rr *api.RemediationRequest, want *metav1.Condition) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		live := &api.RemediationRequest{}
		if err := r.reader().Get(ctx, client.ObjectKeyFromObject(rr), live); err != nil {
			return &clusterError{err: err}
		}
		if live.UID != rr.UID {
			return &clusterError{err: errors.New("RemediationRequest was replaced")}
		}
		base := live.DeepCopy()
		changed := false
		if want == nil {
			changed = meta.RemoveStatusCondition(&live.Status.Conditions, api.ConditionGitReadRetry)
		} else {
			c := want.DeepCopy()
			if old := meta.FindStatusCondition(live.Status.Conditions, api.ConditionGitReadRetry); old != nil && old.Status == c.Status && old.Reason == c.Reason {
				// The start of the wait is kept while its kind stays, whatever read fails now.
				c.LastTransitionTime = old.LastTransitionTime
			} else {
				meta.RemoveStatusCondition(&live.Status.Conditions, api.ConditionGitReadRetry)
			}
			changed = meta.SetStatusCondition(&live.Status.Conditions, *c)
		}
		if changed {
			if err := r.Status().Patch(ctx, live, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
				return fmt.Errorf("record Git read retry: %w", err)
			}
		}
		rr.ResourceVersion = live.ResourceVersion
		if c := meta.FindStatusCondition(live.Status.Conditions, api.ConditionGitReadRetry); c != nil {
			meta.SetStatusCondition(&rr.Status.Conditions, *c)
		} else {
			meta.RemoveStatusCondition(&rr.Status.Conditions, api.ConditionGitReadRetry)
		}
		return nil
	})
}
