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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// conditionGitReadRetry names the read being retried. Its transition time is the executor's retry start.
const conditionGitReadRetry = "GitReadRetry"

// gitReadError identifies an error at the provider boundary, before any Kubernetes operation can wrap it.
// It carries a fixed method name only; the host's answer never enters a condition or a log.
type gitReadError struct {
	op  string
	err error
}

func (e *gitReadError) Error() string { return e.op + " failed" }
func (e *gitReadError) Unwrap() error { return e.err }

// gitReadBound is the latest time the retry of a failed Git read may wait until: the end of the wait, grace or
// deadline of the path that made the read, from the request's recorded state. A path without such an end says why
// (unboundedRead). retryGitRead takes one with every call, so a call site cannot leave it out.
type gitReadBound struct {
	// until is a deadline on the executor's clock; zero for none.
	until time.Time
	// within is a duration from the moment the wait is computed; zero for none.
	within time.Duration
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

// readBoundAt is a bound at t.
func readBoundAt(t time.Time) gitReadBound { return gitReadBound{until: t} }

// readBoundIn is a bound of d from when the wait is computed: what is left of a wait the caller just computed, or
// a period such as the full recheck's.
func readBoundIn(d time.Duration) gitReadBound { return gitReadBound{within: max(d, time.Nanosecond)} }

// unboundedRead is no bound, for the reason why.
func unboundedRead(why noBoundReason) gitReadBound { return gitReadBound{why: why} }

// earlierBound is the earlier of b and o on each form; a bound beats none.
func earlierBound(b, o gitReadBound) gitReadBound {
	out := b
	if !o.until.IsZero() && (out.until.IsZero() || o.until.Before(out.until)) {
		out.until = o.until
	}
	if o.within > 0 && (out.within == 0 || o.within < out.within) {
		out.within = o.within
	}
	if out.bounded() {
		out.why = ""
	}
	return out
}

func (b gitReadBound) bounded() bool { return !b.until.IsZero() || b.within > 0 }

// cap returns d, shortened to what is left of b at now. A deadline that has passed, or none, leaves d.
func (b gitReadBound) cap(now time.Time, d time.Duration) time.Duration {
	if left := b.until.Sub(now); !b.until.IsZero() && left > 0 {
		d = min(d, left)
	}
	if b.within > 0 {
		d = min(d, b.within)
	}
	return d
}

// readBoundOf is the earlier of bound and the one err carries (withReadBound).
func readBoundOf(err error, bound gitReadBound) gitReadBound {
	if e, ok := errors.AsType[*boundedReadError](err); ok {
		return earlierBound(bound, e.bound)
	}
	return bound
}

// requestBound is the bound of a request's own deadline: a request in PolicyChecked expires at status.deadline,
// unless the cleanup of its stale plan has started (staleMark), which never expires.
func requestBound(rr *api.RemediationRequest) gitReadBound {
	if rr.Status.Phase == api.PhasePolicyChecked && rr.Status.Deadline != nil {
		if staleMark(rr) != nil {
			return unboundedRead(noBoundStaleCleanup)
		}
		return readBoundAt(rr.Status.Deadline.Time)
	}
	return unboundedRead(noBoundAfterStop)
}

// boundedReadError is a failed Git read that carries the bound of the path that made it, for a caller that returns
// an error rather than a result (checkRetargeted): retryGitRead keeps the earlier of it and its own argument.
type boundedReadError struct {
	err   error
	bound gitReadBound
}

func (e *boundedReadError) Error() string { return e.err.Error() }
func (e *boundedReadError) Unwrap() error { return e.err }

// withReadBound attaches bound to err, a failed Git read.
func withReadBound(err error, bound gitReadBound) error {
	if err == nil {
		return nil
	}
	return &boundedReadError{err: err, bound: bound}
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

// retryGitRead is the common result of a failed Git read, called by the path that made the read: a visible retry
// (condition GitReadRetry) after the poll interval or the delay the host asked for, whichever is longer, and never
// later than bound (or a bound err carries, withReadBound), so the path's own end (a wait, a grace, a deadline) runs
// on time. A bound that has passed does not shorten the wait: the caller's expiry path runs before its reads, and a
// read that failed after it is a read of that path. Any error that is not a failed Git read (a Kubernetes read or
// write, the cancellation of the reconcile) is returned unchanged, for the error backoff.
func (r *RemediationRequestReconciler) retryGitRead(ctx context.Context, rr *api.RemediationRequest, err error,
	bound gitReadBound) (reconcile.Result, error) {
	if ctx.Err() != nil || !gitReadFailure(err) {
		return reconcile.Result{}, err
	}
	op := "read the Git host"
	if e, ok := errors.AsType[*gitReadError](err); ok {
		op = e.op
	} else if e, ok := errors.AsType[*freshness.ProviderError](err); ok {
		op = e.Op
	} else if e, ok := errors.AsType[*sourcemap.TransientError](err); ok {
		op = e.Op
	} else if e, ok := errors.AsType[*sourcemap.RefusedError](err); ok {
		op = e.Op
	}
	want := &metav1.Condition{Type: conditionGitReadRetry, Status: metav1.ConditionTrue, Reason: "GitReadFailed",
		Message: "Retrying Git read: " + op, ObservedGeneration: rr.Generation, LastTransitionTime: metav1.NewTime(r.now())}
	if e := r.putGitReadRetry(ctx, rr, want); e != nil {
		return reconcile.Result{}, e
	}
	r.gitRetries().mark(client.ObjectKeyFromObject(rr))
	d := max(r.pullRequestPollInterval(), retryAfterOf(err))
	if hostDelay, ok := providers.RetryAfter(err); ok {
		d = max(d, hostDelay)
	}
	d = readBoundOf(err, bound).cap(r.now(), d)
	logf.FromContext(ctx).Info("Retried a Git read of RemediationRequest", "remediationRequest", client.ObjectKeyFromObject(rr), "operation", op, "retryAfter", d)
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
	if meta.FindStatusCondition(rr.Status.Conditions, conditionGitReadRetry) == nil {
		return nil
	}
	return r.putGitReadRetry(ctx, rr, nil)
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
			changed = meta.RemoveStatusCondition(&live.Status.Conditions, conditionGitReadRetry)
		} else {
			c := want.DeepCopy()
			if old := meta.FindStatusCondition(live.Status.Conditions, conditionGitReadRetry); old != nil && old.Message == c.Message {
				c.LastTransitionTime = old.LastTransitionTime
			} else {
				meta.RemoveStatusCondition(&live.Status.Conditions, conditionGitReadRetry)
			}
			changed = meta.SetStatusCondition(&live.Status.Conditions, *c)
		}
		if changed {
			if err := r.Status().Patch(ctx, live, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
				return fmt.Errorf("record Git read retry: %w", err)
			}
		}
		rr.ResourceVersion = live.ResourceVersion
		if c := meta.FindStatusCondition(live.Status.Conditions, conditionGitReadRetry); c != nil {
			meta.SetStatusCondition(&rr.Status.Conditions, *c)
		} else {
			meta.RemoveStatusCondition(&rr.Status.Conditions, conditionGitReadRetry)
		}
		return nil
	})
}
