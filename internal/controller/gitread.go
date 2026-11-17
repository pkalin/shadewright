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

	api "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/gitops/sourcemap"
	"github.com/pkalin/shadewright/internal/policy/freshness"
	"github.com/pkalin/shadewright/internal/providers"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

type gitReadTraceKey struct{}
type gitReadTrace struct {
	read    bool
	retried bool
	failure error
}

func readFailure(ctx context.Context, op string, err error) error {
	if trace, ok := ctx.Value(gitReadTraceKey{}).(*gitReadTrace); ok {
		trace.read = true
		if err != nil && !providers.IsNotFound(err) {
			trace.failure = &gitReadError{op: op, err: err}
		}
	}
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

// retryGitRead is the common result of a failed Git read: a visible retry and at least one poll interval.
// A failed Kubernetes read or status write is returned as such, never relabelled as a Git failure.
func (r *RemediationRequestReconciler) retryGitRead(ctx context.Context, rr *api.RemediationRequest, err error) (reconcile.Result, error) {
	if trace, ok := ctx.Value(gitReadTraceKey{}).(*gitReadTrace); ok {
		trace.retried = true
	}
	if ctx.Err() != nil || isClusterError(err) {
		return reconcile.Result{}, err
	}
	const unknownOperation = "read the Git host"
	op := unknownOperation
	if trace, ok := ctx.Value(gitReadTraceKey{}).(*gitReadTrace); ok && trace.failure != nil {
		if e, ok := errors.AsType[*gitReadError](trace.failure); ok {
			op = e.op
		}
	}
	if e, ok := errors.AsType[*gitReadError](err); ok {
		op = e.op
	} else if e, ok := errors.AsType[*freshness.ProviderError](err); ok && op == unknownOperation {
		op = e.Op
	} else if e, ok := errors.AsType[*sourcemap.TransientError](err); ok && op == unknownOperation {
		op = e.Op
	} else if e, ok := errors.AsType[*sourcemap.RefusedError](err); ok && op == unknownOperation {
		op = e.Op
	}
	want := &metav1.Condition{Type: conditionGitReadRetry, Status: metav1.ConditionTrue, Reason: "GitReadFailed",
		Message: "Retrying Git read: " + op, ObservedGeneration: rr.Generation, LastTransitionTime: metav1.NewTime(r.now())}
	if e := r.putGitReadRetry(ctx, rr, want); e != nil {
		return reconcile.Result{}, e
	}
	d := max(r.pullRequestPollInterval(), retryAfterOf(err))
	if hostDelay, ok := providers.RetryAfter(err); ok {
		d = max(d, hostDelay)
	}
	logf.FromContext(ctx).Info("Retried a Git read of RemediationRequest", "remediationRequest", client.ObjectKeyFromObject(rr), "operation", op, "retryAfter", d)
	return reconcile.Result{RequeueAfter: d}, nil
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
