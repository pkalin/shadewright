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
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/prometheus/client_golang/prometheus/testutil"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	ctrlpkg "sigs.k8s.io/controller-runtime/pkg/controller"
	ctrlevent "sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	remediationv1alpha1 "github.com/shadewright-labs/shadewright/api/v1alpha1"
)

// TestExpectedRetryLog (#902): the "Reconciler error" line controller-runtime writes for the error a reconcile
// returns is at V(1) for an expected retry (a target that changed under the policy check, a conflict entering
// Verifying) and stays at ERROR for any other error, a plain conflict included. The error itself is still returned,
// so the backoff and the error counters are those of any error.
func TestExpectedRetryLog(t *testing.T) {
	conflict := apierrors.NewConflict(remediationv1alpha1.GroupVersion.WithResource("remediationrequests").GroupResource(),
		"x", errors.New("the object has been modified"))
	req := &reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "rr"}}
	for _, tc := range []struct {
		name      string
		err       error
		verbosity int
		want      string
	}{
		{name: "target changed", err: fmt.Errorf("Deployment a/b changed: %w", errTargetChanged), want: ""},
		{name: "target changed at V(1)", err: fmt.Errorf("Deployment a/b changed: %w", errTargetChanged),
			verbosity: 1, want: `"level"=1 "msg"="Reconciler error"`},
		{name: "conflict entering Verifying", err: fmt.Errorf("%w: %w", errVerifyingConflict, conflict), want: ""},
		{name: "conflict entering Verifying at V(1)", err: fmt.Errorf("%w: %w", errVerifyingConflict, conflict),
			verbosity: 1, want: `"level"=1 "msg"="Reconciler error"`},
		{name: "other conflict", err: conflict, want: `"msg"="Reconciler error" "error"=`},
		{name: "other error", err: errors.New("the API server is down"), want: `"msg"="Reconciler error" "error"=`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var lines []string
			base := funcr.New(func(prefix, args string) { lines = append(lines, prefix+" "+args) },
				funcr.Options{Verbosity: tc.verbosity})
			log := expectedRetryLogConstructor(base)(req)
			log.Error(tc.err, "Reconciler error")
			if tc.want == "" {
				if len(lines) != 0 {
					t.Fatalf("lines = %q, want none at verbosity 0", lines)
				}
				return
			}
			if len(lines) != 1 || !strings.Contains(lines[0], tc.want) {
				t.Fatalf("lines = %q, want one with %s", lines, tc.want)
			}
			// The first part of the error, before any quote funcr escapes.
			cause, _, _ := strings.Cut(tc.err.Error(), `"`)
			for _, field := range []string{`"controller"="remediationrequest"`, `"namespace"="ns"`, `"name"="rr"`,
				cause} {
				if !strings.Contains(lines[0], field) {
					t.Fatalf("line %q lacks %s", lines[0], field)
				}
			}
		})
	}
}

// TestExpectedRetryLogDiscardAndSliceSafety verifies that wrapping a discard (nil-sink) logger does not panic,
// WithCallDepth is preserved, and lowering an expected retry does not overwrite spare capacity in the caller's
// key-value slice.
func TestExpectedRetryLogDiscardAndSliceSafety(t *testing.T) {
	req := &reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "rr"}}
	discard := expectedRetryLogConstructor(logr.Discard())(req)
	discard.Info("Starting Controller")
	discard.Error(errTargetChanged, "Reconciler error")

	var lines []string
	base := funcr.New(func(prefix, args string) { lines = append(lines, prefix+" "+args) },
		funcr.Options{Verbosity: 1})
	log := expectedRetryLogConstructor(base)(req)
	if _, ok := log.GetSink().(logr.CallDepthLogSink); !ok {
		t.Fatalf("sink %T does not implement logr.CallDepthLogSink", log.GetSink())
	}
	backing := []any{"k", "v", "sentinel-key", "sentinel-val"}
	kv := backing[:2]
	log.Error(errTargetChanged, "Reconciler error", kv...)
	if backing[2] != "sentinel-key" || backing[3] != "sentinel-val" {
		t.Fatalf("caller slice backing array mutated: %v", backing)
	}
	if len(lines) != 1 || !strings.Contains(lines[0], `"level"=1`) {
		t.Fatalf("lines = %q, want one V(1) line", lines)
	}
}

// recordingLimiter records every backoff duration controller-runtime requests from its rate limiter.
type recordingLimiter struct {
	inner  workqueue.TypedRateLimiter[reconcile.Request]
	mu     sync.Mutex
	delays []time.Duration
}

func (l *recordingLimiter) When(item reconcile.Request) time.Duration {
	d := l.inner.When(item)
	l.mu.Lock()
	l.delays = append(l.delays, d)
	l.mu.Unlock()
	return d
}

func (l *recordingLimiter) Forget(item reconcile.Request) { l.inner.Forget(item) }
func (l *recordingLimiter) NumRequeues(item reconcile.Request) int {
	return l.inner.NumRequeues(item)
}

func gatherControllerRuntimeErrors(t *testing.T, name string) float64 {
	t.Helper()
	fams, err := metrics.Registry.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	for _, fam := range fams {
		if fam.GetName() != "controller_runtime_reconcile_errors_total" {
			continue
		}
		for _, m := range fam.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == "controller" && lp.GetValue() == name {
					return m.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}

// TestExpectedRetryKeepsControllerBackoffAndMetrics runs a real controller-runtime controller with
// expectedRetryLogConstructor and countErrors to verify that both expected retry errors keep growing rate-limited
// backoff and both Prometheus error counters while emitting no ERROR log line.
func TestExpectedRetryKeepsControllerBackoffAndMetrics(t *testing.T) {
	conflict := apierrors.NewConflict(remediationv1alpha1.GroupVersion.WithResource("remediationrequests").GroupResource(),
		"rr", errors.New("the object has been modified"))
	cases := []struct {
		name      string
		ctrlName  string
		err       error
		errorKind string
	}{
		{
			name:      "errTargetChanged",
			ctrlName:  "test-expected-retry-target-changed",
			err:       fmt.Errorf("Deployment a/b changed during the policy check, checking again: %w", errTargetChanged),
			errorKind: errorOther,
		},
		{
			name:      "errVerifyingConflict",
			ctrlName:  "test-expected-retry-verifying-conflict",
			err:       fmt.Errorf("%w: %w", errVerifyingConflict, conflict),
			errorKind: errorConflict,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logMu sync.Mutex
			var lines []string
			base := funcr.New(func(prefix, args string) {
				logMu.Lock()
				lines = append(lines, prefix+" "+args)
				logMu.Unlock()
			}, funcr.Options{Verbosity: 0})

			limiter := &recordingLimiter{
				inner: workqueue.NewTypedItemExponentialFailureRateLimiter[reconcile.Request](
					2*time.Millisecond, 50*time.Millisecond),
			}
			var attempts int
			done := make(chan struct{})
			rec := countErrors{reconcileFunc(func(context.Context, reconcile.Request) (reconcile.Result, error) {
				attempts++
				if attempts <= 2 {
					return reconcile.Result{}, tc.err
				}
				close(done)
				return reconcile.Result{}, nil
			})}

			skipNameValidation := true
			c, err := ctrlpkg.NewUnmanaged(tc.ctrlName, ctrlpkg.Options{
				Reconciler:         rec,
				RateLimiter:        limiter,
				LogConstructor:     expectedRetryLogConstructor(base),
				SkipNameValidation: &skipNameValidation,
			})
			if err != nil {
				t.Fatalf("NewUnmanaged: %v", err)
			}
			ch := make(chan ctrlevent.GenericEvent, 1)
			if err := c.Watch(source.Channel(ch, &handler.EnqueueRequestForObject{})); err != nil {
				t.Fatalf("Watch: %v", err)
			}

			beforeOwn := testutil.ToFloat64(reconcileErrors.WithLabelValues(tc.errorKind))
			beforeCR := gatherControllerRuntimeErrors(t, tc.ctrlName)

			ctx, cancel := context.WithCancel(t.Context())
			stopped := make(chan error, 1)
			go func() { stopped <- c.Start(ctx) }()

			ch <- ctrlevent.GenericEvent{Object: &remediationv1alpha1.RemediationRequest{
				ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "rr"},
			}}

			select {
			case <-done:
			case <-time.After(5 * time.Second):
				cancel()
				t.Fatal("timed out waiting for expected retries to settle")
			}
			cancel()
			if err := <-stopped; err != nil {
				t.Fatalf("controller Start: %v", err)
			}

			limiter.mu.Lock()
			delays := append([]time.Duration(nil), limiter.delays...)
			limiter.mu.Unlock()
			if len(delays) != 2 || delays[1] <= delays[0] {
				t.Fatalf("backoff delays = %v, want 2 growing delays", delays)
			}
			if got := testutil.ToFloat64(reconcileErrors.WithLabelValues(tc.errorKind)) - beforeOwn; got != 2 {
				t.Fatalf("shadewright_remediation_reconcile_errors_total{kind=%q} delta = %v, want 2", tc.errorKind, got)
			}
			if got := gatherControllerRuntimeErrors(t, tc.ctrlName) - beforeCR; got != 2 {
				t.Fatalf("controller_runtime_reconcile_errors_total delta = %v, want 2", got)
			}
			logMu.Lock()
			gotLines := append([]string(nil), lines...)
			logMu.Unlock()
			for _, line := range gotLines {
				if strings.Contains(line, "Reconciler error") {
					t.Fatalf("expected retry logged at ERROR: %q", line)
				}
			}
		})
	}
}
