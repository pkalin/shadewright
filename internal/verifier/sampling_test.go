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

package verifier

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/testfixtures"
	"github.com/pkalin/shadewright/internal/verification/clearing"
)

// TestRequestSampledOnceTheRolloutIsRecorded: the executor sets Verifying first and records the template
// revision in a later status write. The first reconcile, without it, takes no sample but looks again after the
// poll interval, and the later write passes the predicate.
func TestRequestSampledOnceTheRolloutIsRecorded(t *testing.T) {
	w := newWorkload(t, watchedNS)
	rr := testfixtures.MemoryLimitRequest(operatorNS, unique("rr"))
	rr.Spec.TargetRef.Namespace = w.deploy.Namespace
	rr.Spec.TargetRef.Name = w.deploy.Name
	rr.Spec.TargetRef.UID = v1alpha1.UID(w.deploy.UID)
	mustCreate(t, rr)
	// The executor's first write in Verifying: the phase, before the template revision is recorded.
	setStatusRollout(t, rr, v1alpha1.PhaseVerifying, false)
	rollout := &v1alpha1.RolloutStatus{TemplateRevision: revision}
	clk := &clock{now: time.Now()}
	r := newReconciler(admin, admin, clk)
	res, err := reconcile(t, r, rr)
	if err != nil {
		t.Fatal(err)
	}
	if res.RequeueAfter != clearing.PollInterval {
		t.Fatalf("RequeueAfter = %v without a recorded template revision, want the poll interval", res.RequeueAfter)
	}
	if _, ok := getReport(t, rr); ok {
		t.Fatal("a report was created before the template revision was recorded")
	}

	recorded := rr.DeepCopy()
	recorded.Status.Rollout = rollout
	if !inputsChanged().Update(event.UpdateEvent{ObjectOld: rr, ObjectNew: recorded}) {
		t.Error("the write that records the rollout did not pass the predicate")
	}
	if err := admin.Status().Update(t.Context(), recorded); err != nil {
		t.Fatal(err)
	}
	if _, err := reconcile(t, r, rr); err != nil {
		t.Fatal(err)
	}
	report, ok := getReport(t, rr)
	if !ok {
		t.Fatal("no report once the template revision was recorded")
	}
	t.Cleanup(func() { _ = admin.Delete(context.Background(), report) })
	if report.Status.Sample != 1 {
		t.Errorf("sample = %d, want 1", report.Status.Sample)
	}
}

// TestPredicatePassesEveryInputOfSampled: a change of the phase, the merge receipt, the presence of the rollout
// or its template revision passes; any other status write does not.
func TestPredicatePassesEveryInputOfSampled(t *testing.T) {
	base := testfixtures.MemoryLimitRequest(operatorNS, "rr")
	base.Status.Phase = v1alpha1.PhaseVerifying
	base.Status.Rollout = &v1alpha1.RolloutStatus{TemplateRevision: revision}
	base.Status.MergeReceipt = &v1alpha1.MergeReceipt{MergeCommitSHA: merge}
	p := inputsChanged()
	cases := map[string]struct {
		change func(*v1alpha1.RemediationRequest)
		pass   bool
	}{
		"phase":             {func(r *v1alpha1.RemediationRequest) { r.Status.Phase = v1alpha1.PhaseMerged }, true},
		"receipt recorded":  {func(r *v1alpha1.RemediationRequest) { r.Status.MergeReceipt = nil }, true},
		"receipt changed":   {func(r *v1alpha1.RemediationRequest) { r.Status.MergeReceipt.MergeCommitSHA = head }, true},
		"rollout recorded":  {func(r *v1alpha1.RemediationRequest) { r.Status.Rollout = nil }, true},
		"template revision": {func(r *v1alpha1.RemediationRequest) { r.Status.Rollout.TemplateRevision = "4" }, true},
		"other status": {func(r *v1alpha1.RemediationRequest) {
			r.Status.Conditions = []metav1.Condition{{Type: "Verified", Status: metav1.ConditionUnknown}}
		}, false},
	}
	for name, c := range cases {
		changed := base.DeepCopy()
		c.change(changed)
		// Both directions: old to new and new to old.
		if got := p.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: changed}); got != c.pass {
			t.Errorf("%s: predicate = %v, want %v", name, got, c.pass)
		}
		if got := p.Update(event.UpdateEvent{ObjectOld: changed, ObjectNew: base}); got != c.pass {
			t.Errorf("%s (reversed): predicate = %v, want %v", name, got, c.pass)
		}
	}
}

// deadlineReader refuses every call made without a context deadline.
type deadlineReader struct{ client.Reader }

var errNoDeadline = errors.New("called without a deadline")

func (d deadlineReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := ctx.Deadline(); !ok {
		return errNoDeadline
	}
	return d.Reader.Get(ctx, key, obj, opts...)
}

func (d deadlineReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if _, ok := ctx.Deadline(); !ok {
		return errNoDeadline
	}
	return d.Reader.List(ctx, list, opts...)
}

// deadlineClient refuses every read and write made without a context deadline.
type deadlineClient struct{ client.Client }

func (d deadlineClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	return deadlineReader{d.Client}.Get(ctx, key, obj, opts...)
}

func (d deadlineClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if _, ok := ctx.Deadline(); !ok {
		return errNoDeadline
	}
	return d.Client.Create(ctx, obj, opts...)
}

func (d deadlineClient) Status() client.SubResourceWriter { return deadlineStatus{d.Client.Status()} }

type deadlineStatus struct{ client.SubResourceWriter }

func (d deadlineStatus) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	if _, ok := ctx.Deadline(); !ok {
		return errNoDeadline
	}
	return d.SubResourceWriter.Update(ctx, obj, opts...)
}

// TestEveryAPICallHasADeadline: every read and write of a sample runs with a context deadline, so a hung call
// cannot hold the single worker.
func TestEveryAPICallHasADeadline(t *testing.T) {
	w := newWorkload(t, watchedNS)
	rr := request(t, w, v1alpha1.PhaseVerifying)
	r := newReconciler(deadlineClient{admin}, deadlineReader{admin}, &clock{now: time.Now()})
	if _, err := reconcile(t, r, rr); err != nil {
		t.Fatalf("Reconcile = %v, want every call made with a deadline", err)
	}
	report, ok := getReport(t, rr)
	if !ok {
		t.Fatal("no report")
	}
	t.Cleanup(func() { _ = admin.Delete(context.Background(), report) })
	if report.Status.Sample != 1 {
		t.Errorf("sample = %d, want 1", report.Status.Sample)
	}
}

// slowReader moves the verifier's clock forward by step at each pod list and ReplicaSet get, as reads that
// take that long would, and runs during, if set, after the pod list.
type slowReader struct {
	client.Reader
	clk    *clock
	step   time.Duration
	during func()
}

func (s slowReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	err := s.Reader.Get(ctx, key, obj, opts...)
	if _, ok := obj.(*appsv1.ReplicaSet); ok {
		s.clk.now = s.clk.now.Add(s.step)
	}
	return err
}

func (s slowReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	err := s.Reader.List(ctx, list, opts...)
	if _, ok := list.(*corev1.PodList); ok {
		s.clk.now = s.clk.now.Add(s.step)
		if s.during != nil {
			s.during()
		}
	}
	return err
}

// TestSampleTimeIsTakenAfterTheReads: sampledAt and windowStart are the verifier's clock after the pod list and
// the owner reads returned, never before them (clearing.Input.Now).
func TestSampleTimeIsTakenAfterTheReads(t *testing.T) {
	w := newWorkload(t, watchedNS)
	rr := request(t, w, v1alpha1.PhaseVerifying)
	clk := &clock{now: time.Now().Truncate(time.Second)}
	start := clk.now
	r := newReconciler(admin, slowReader{Reader: admin, clk: clk, step: 3 * time.Second}, clk)
	if _, err := reconcile(t, r, rr); err != nil {
		t.Fatal(err)
	}
	report, ok := getReport(t, rr)
	if !ok {
		t.Fatal("no report")
	}
	t.Cleanup(func() { _ = admin.Delete(context.Background(), report) })
	if want := start.Add(6 * time.Second); report.Status.SampledAt == nil || !report.Status.SampledAt.Time.Equal(want) {
		t.Errorf("sampledAt = %v, want %v: the time after the pod list and the ReplicaSet read", report.Status.SampledAt, want)
	}
	if report.Status.WindowStart == nil || report.Status.WindowStart.Time.Before(start.Add(6*time.Second)) {
		t.Errorf("windowStart = %v predates the reads", report.Status.WindowStart)
	}
}

// TestSlowObservationIsDiscarded: a pod list answered after 45 seconds, while the container restarted in the
// meantime, writes no sample: the counter does not rise, so the executor's counter rule cannot read the report
// as fresh, and the stale "no restart" never becomes evidence.
func TestSlowObservationIsDiscarded(t *testing.T) {
	w := newWorkload(t, watchedNS)
	rr := request(t, w, v1alpha1.PhaseVerifying)
	clk := &clock{now: time.Now().Truncate(time.Second)}
	fast := newReconciler(admin, admin, clk)
	for range 2 {
		if _, err := reconcile(t, fast, rr); err != nil {
			t.Fatal(err)
		}
		clk.now = clk.now.Add(clearing.PollInterval)
	}
	before, ok := getReport(t, rr)
	if !ok {
		t.Fatal("no report")
	}
	t.Cleanup(func() { _ = admin.Delete(context.Background(), before) })

	var counter clearing.Counter
	executorNow := time.Now()
	counter.Observe(before.UID, before.Status.Sample, executorNow)

	slow := newReconciler(admin, slowReader{Reader: admin, clk: clk, step: 45 * time.Second,
		during: func() { w.setRestarts(t, 1) }}, clk)
	res, err := reconcile(t, slow, rr)
	if err != nil {
		t.Fatal(err)
	}
	if res.RequeueAfter != clearing.PollInterval {
		t.Errorf("RequeueAfter = %v, want the poll interval", res.RequeueAfter)
	}
	after, _ := getReport(t, rr)
	if after.Status.Sample != before.Status.Sample || after.ResourceVersion != before.ResourceVersion {
		t.Fatalf("a slow observation was written: sample %d -> %d", before.Status.Sample, after.Status.Sample)
	}
	if f := counter.Observe(after.UID, after.Status.Sample, executorNow.Add(clearing.FastPollInterval)); f.Fresh {
		t.Error("the executor reads the report as fresh after a discarded sample")
	}
}

// TestFailingListCreatesNoReport: a pod list that keeps failing at the first sample leaves no report, never a
// report with an empty status; the executor reads the missing report as StaleReport.
func TestFailingListCreatesNoReport(t *testing.T) {
	w := newWorkload(t, watchedNS)
	rr := request(t, w, v1alpha1.PhaseVerifying)
	r := newReconciler(admin, failingReader{admin}, &clock{now: time.Now()})
	for range 2 {
		if _, err := reconcile(t, r, rr); err == nil {
			t.Fatal("Reconcile succeeded with a failing pod list")
		}
	}
	if report, ok := getReport(t, rr); ok {
		t.Cleanup(func() { _ = admin.Delete(context.Background(), report) })
		t.Errorf("a report was created without a sample: %+v", report.Status)
	}
}

// logLines collects log lines.
type logLines struct {
	mu    sync.Mutex
	lines []string
}

func (l *logLines) logger() logr.Logger {
	return funcr.New(func(prefix, args string) {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.lines = append(l.lines, args)
	}, funcr.Options{Verbosity: 1})
}

// find returns the first line with every one of the given parts.
func (l *logLines) find(parts ...string) (string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, line := range l.lines {
		ok := true
		for _, p := range parts {
			ok = ok && strings.Contains(line, p)
		}
		if ok {
			return line, true
		}
	}
	return "", false
}

// TestEverySkipIsLogged: a Verifying request the verifier takes no sample of gets a log line with the request
// key and a fixed reason: a missing target without a target UID (looked at again), a namespace outside
// --watch-namespaces, a kind it does not sample, and a report bound to another rollout, which is logged as an
// error naming the report and not retried on a timer.
func TestEverySkipIsLogged(t *testing.T) {
	w := newWorkload(t, watchedNS)
	outside := newWorkload(t, unwatched)

	missing := testfixtures.MemoryLimitRequest(operatorNS, unique("rr"))
	missing.Spec.TargetRef.Namespace = watchedNS
	missing.Spec.TargetRef.Name = unique("gone")
	mustCreate(t, missing)
	setStatus(t, missing, v1alpha1.PhaseVerifying)

	so := testfixtures.RemediationRequest(operatorNS, unique("rr"))
	mustCreate(t, so)
	setStatus(t, so, v1alpha1.PhaseVerifying)

	notBound := request(t, w, v1alpha1.PhaseVerifying)
	other := &v1alpha1.VerificationReport{
		ObjectMeta: metav1.ObjectMeta{Namespace: operatorNS, Name: clearing.ReportName(notBound.UID)},
		Spec: v1alpha1.VerificationReportSpec{RequestUID: v1alpha1.UID(notBound.UID), MergeCommitSHA: strings.Repeat("e", 40),
			TargetUID: v1alpha1.UID(w.deploy.UID), TemplateRevision: revision, Container: container},
	}
	mustCreate(t, other)

	cases := []struct {
		name    string
		rr      *v1alpha1.RemediationRequest
		reason  string
		requeue time.Duration
		extra   []string
	}{
		{"missing target", missing, "TargetUIDUnknown", clearing.PollInterval, nil},
		{"unwatched namespace", request(t, outside, v1alpha1.PhaseVerifying), "NamespaceNotWatched", 0, nil},
		{"ScaledObject", so, "TargetKindNotSampled", 0, nil},
		{"report not bound", notBound, "ReportNotBound", 0, []string{`"error"=`, clearing.ReportName(notBound.UID)}},
	}
	for _, c := range cases {
		logs := &logLines{}
		// The CRD requires targetRef.uid; a request read without one (an older object) is what this case covers.
		r := newReconciler(noTargetUID{admin}, admin, &clock{now: time.Now()})
		ctx := logf.IntoContext(t.Context(), logs.logger())
		res, err := r.Reconcile(ctx, reconcileRequest(c.rr))
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if res.RequeueAfter != c.requeue {
			t.Errorf("%s: RequeueAfter = %v, want %v", c.name, res.RequeueAfter, c.requeue)
		}
		parts := append([]string{client.ObjectKeyFromObject(c.rr).String(), `"reason"="` + c.reason + `"`}, c.extra...)
		if _, ok := logs.find(parts...); !ok {
			t.Errorf("%s: no log line with %q; got %q", c.name, parts, logs.lines)
		}
		if c.rr != notBound {
			if report, ok := getReport(t, c.rr); ok {
				t.Errorf("%s: a report was created: %+v", c.name, report.Spec)
			}
		}
	}
}

func reconcileRequest(rr *v1alpha1.RemediationRequest) ctrl.Request {
	return ctrl.Request{NamespacedName: client.ObjectKeyFromObject(rr)}
}

// noTargetUID reads requests without spec.targetRef.uid.
type noTargetUID struct{ client.Client }

func (n noTargetUID) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	err := n.Client.Get(ctx, key, obj, opts...)
	if rr, ok := obj.(*v1alpha1.RemediationRequest); ok {
		rr.Spec.TargetRef.UID = ""
	}
	return err
}
