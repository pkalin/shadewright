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
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	v1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/testfixtures"
	"github.com/pkalin/shadewright/internal/verification/clearing"
	"github.com/pkalin/shadewright/internal/verification/rollout"
)

const (
	operatorNS = "shadewright-system"
	watchedNS  = testfixtures.TargetNamespace
	unwatched  = "team-b"
	revision   = "3"
	container  = "worker"
	merge      = "89abcdef0123456789abcdef0123456789abcdef"
	head       = "0123456789abcdef0123456789abcdef01234567"
)

var (
	scheme = runtime.NewScheme()
	admin  client.Client
	seq    atomic.Int64
)

func TestMain(m *testing.M) {
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		panic(err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		panic(err)
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	if dir := firstEnvtestBinaryDir(); dir != "" {
		env.BinaryAssetsDirectory = dir
	}
	cfg, err := env.Start()
	if err != nil {
		panic(err)
	}
	if admin, err = client.New(cfg, client.Options{Scheme: scheme}); err != nil {
		_ = env.Stop()
		panic(err)
	}
	for _, ns := range []string{operatorNS, watchedNS, unwatched} {
		if err := admin.Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
			_ = env.Stop()
			panic(err)
		}
	}
	code := m.Run()
	_ = env.Stop()
	os.Exit(code)
}

func firstEnvtestBinaryDir() string {
	base := filepath.Join("..", "..", "bin", "k8s")
	entries, err := os.ReadDir(base)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if e.IsDir() {
			return filepath.Join(base, e.Name())
		}
	}
	return ""
}

func unique(prefix string) string { return fmt.Sprintf("%s-%d", prefix, seq.Add(1)) }

// workload is a Deployment with one ReplicaSet and one pod of the rollout, as the Deployment controller and the
// kubelet would leave them (envtest runs neither).
type workload struct {
	deploy *appsv1.Deployment
	pod    *corev1.Pod
}

func newWorkload(t *testing.T, ns string) workload {
	t.Helper()
	name := unique("web")
	labels := map[string]string{"app": name}
	limits := corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("384Mi")}
	podSpec := corev1.PodSpec{Containers: []corev1.Container{{Name: container, Image: "example.invalid/web:1",
		Resources: corev1.ResourceRequirements{Limits: limits}}}}
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name,
			Annotations: map[string]string{rollout.DeploymentRevisionAnnotation: revision}},
		Spec: appsv1.DeploymentSpec{Replicas: ptr.To[int32](1), Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: podSpec}},
	}
	mustCreate(t, d)
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name + "-rs", Labels: labels,
			Annotations:     map[string]string{rollout.DeploymentRevisionAnnotation: revision},
			OwnerReferences: []metav1.OwnerReference{controllerRef("apps/v1", "Deployment", d.Name, d.UID)}},
		Spec: appsv1.ReplicaSetSpec{Replicas: ptr.To[int32](1), Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: podSpec}},
	}
	mustCreate(t, rs)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name + "-pod", Labels: labels,
			OwnerReferences: []metav1.OwnerReference{controllerRef("apps/v1", "ReplicaSet", rs.Name, rs.UID)}},
		Spec: podSpec,
	}
	mustCreate(t, pod)
	w := workload{deploy: d, pod: pod}
	w.setRestarts(t, 0)
	return w
}

// setRestarts writes the pod's status as the kubelet would: Ready, with the container's restart count.
func (w workload) setRestarts(t *testing.T, n int32) {
	t.Helper()
	p := &corev1.Pod{}
	if err := admin.Get(t.Context(), client.ObjectKeyFromObject(w.pod), p); err != nil {
		t.Fatal(err)
	}
	p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: container, RestartCount: n, Ready: true,
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}
	if err := admin.Status().Update(t.Context(), p); err != nil {
		t.Fatal(err)
	}
}

func controllerRef(apiVersion, kind, name string, uid types.UID) metav1.OwnerReference {
	return metav1.OwnerReference{APIVersion: apiVersion, Kind: kind, Name: name, UID: uid, Controller: new(true)}
}

func mustCreate(t *testing.T, obj client.Object) {
	t.Helper()
	if err := admin.Create(t.Context(), obj); err != nil {
		t.Fatalf("creating %T: %v", obj, err)
	}
	t.Cleanup(func() { _ = admin.Delete(context.Background(), obj) })
}

// request creates a SetContainerMemoryLimit request on w's Deployment and moves its status to phase, with a
// merge receipt and a recorded template revision unless noReceipt.
func request(t *testing.T, w workload, phase v1alpha1.RemediationPhase, noReceipt bool) *v1alpha1.RemediationRequest {
	t.Helper()
	rr := testfixtures.MemoryLimitRequest(operatorNS, unique("rr"))
	rr.Spec.TargetRef.Namespace = w.deploy.Namespace
	rr.Spec.TargetRef.Name = w.deploy.Name
	rr.Spec.TargetRef.UID = v1alpha1.UID(w.deploy.UID)
	mustCreate(t, rr)
	setStatus(t, rr, phase, noReceipt)
	return rr
}

// setStatus moves a created request's status to phase, with a merge receipt and a recorded template revision
// unless noReceipt.
func setStatus(t *testing.T, rr *v1alpha1.RemediationRequest, phase v1alpha1.RemediationPhase, noReceipt bool) {
	t.Helper()
	setStatusRollout(t, rr, phase, noReceipt, true)
}

// setStatusRollout is setStatus, with status.rollout left unset unless withRollout.
func setStatusRollout(t *testing.T, rr *v1alpha1.RemediationRequest, phase v1alpha1.RemediationPhase, noReceipt, withRollout bool) {
	t.Helper()
	mergedAt := metav1.NewMicroTime(time.Now().Add(-10 * time.Minute).UTC().Truncate(time.Second))
	rr.Status.Phase = phase
	if withRollout {
		rr.Status.Rollout = &v1alpha1.RolloutStatus{TemplateRevision: revision}
	}
	if !noReceipt {
		rr.Status.PullRequest = &v1alpha1.PullRequestStatus{Number: 1, URL: "https://github.com/example/fleet/pull/1",
			HeadSHA: head, MergeCommitSHA: merge, MergedAt: &mergedAt}
		rr.Status.MergeReceipt = &v1alpha1.MergeReceipt{
			Repository:   v1alpha1.PlanRepository{URL: "https://github.com/example/fleet", InstallationID: 7},
			TargetBranch: "main", PullRequest: 1, HeadSHA: head, MergeCommitSHA: merge, MergedAt: mergedAt,
		}
	}
	if err := admin.Status().Update(t.Context(), rr); err != nil {
		t.Fatalf("writing the request status: %v", err)
	}
}

// recorder is a client that records every write and fails the test on a write to anything but a
// VerificationReport.
type recorder struct {
	client.Client
	mu     sync.Mutex
	writes []string
}

func (c *recorder) note(op string, obj client.Object) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writes = append(c.writes, fmt.Sprintf("%s %T", op, obj))
}

func (c *recorder) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	c.note("create", obj)
	return c.Client.Create(ctx, obj, opts...)
}

func (c *recorder) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	c.note("update", obj)
	return c.Client.Update(ctx, obj, opts...)
}

func (c *recorder) Patch(ctx context.Context, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
	c.note("patch", obj)
	return c.Client.Patch(ctx, obj, p, opts...)
}

func (c *recorder) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	c.note("delete", obj)
	return c.Client.Delete(ctx, obj, opts...)
}

func (c *recorder) Status() client.SubResourceWriter {
	return &statusRecorder{c: c, w: c.Client.Status()}
}

type statusRecorder struct {
	c *recorder
	w client.SubResourceWriter
}

func (s *statusRecorder) Create(ctx context.Context, obj, sub client.Object, opts ...client.SubResourceCreateOption) error {
	s.c.note("status create", obj)
	return s.w.Create(ctx, obj, sub, opts...)
}

func (s *statusRecorder) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	s.c.note("status update", obj)
	return s.w.Update(ctx, obj, opts...)
}

func (s *statusRecorder) Patch(ctx context.Context, obj client.Object, p client.Patch, opts ...client.SubResourcePatchOption) error {
	s.c.note("status patch", obj)
	return s.w.Patch(ctx, obj, p, opts...)
}

func (s *statusRecorder) Apply(ctx context.Context, obj runtime.ApplyConfiguration, opts ...client.SubResourceApplyOption) error {
	s.c.note("status apply", nil)
	return s.w.Apply(ctx, obj, opts...)
}

// clock is a settable clock for the verifier.
type clock struct{ now time.Time }

func (c *clock) Now() time.Time { return c.now }

func newReconciler(c client.Client, r client.Reader, clk *clock) *Reconciler {
	return &Reconciler{Client: c, APIReader: r, Namespace: operatorNS, Watched: []string{watchedNS}, Now: clk.Now}
}

func reconcile(t *testing.T, r *Reconciler, rr *v1alpha1.RemediationRequest) (ctrl.Result, error) {
	t.Helper()
	return r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(rr)})
}

func getReport(t *testing.T, rr *v1alpha1.RemediationRequest) (*v1alpha1.VerificationReport, bool) {
	t.Helper()
	report := &v1alpha1.VerificationReport{}
	err := admin.Get(t.Context(), client.ObjectKey{Namespace: operatorNS, Name: clearing.ReportName(rr.UID)}, report)
	if apierrors.IsNotFound(err) {
		return nil, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return report, true
}

// TestCreatesOneReportForAVerifyingRequest: the first reconcile of a Verifying request creates its report with
// the spec of ADR-0030 section 7, named from the request UID and owned by the request with blockOwnerDeletion
// false; later reconciles write samples into the same report.
func TestCreatesOneReportForAVerifyingRequest(t *testing.T) {
	w := newWorkload(t, watchedNS)
	rr := request(t, w, v1alpha1.PhaseVerifying, false)
	clk := &clock{now: time.Now()}
	r := newReconciler(admin, admin, clk)

	res, err := reconcile(t, r, rr)
	if err != nil {
		t.Fatal(err)
	}
	if res.RequeueAfter != clearing.PollInterval {
		t.Errorf("RequeueAfter = %v, want the poll interval %v", res.RequeueAfter, clearing.PollInterval)
	}
	clk.now = clk.now.Add(clearing.PollInterval)
	if _, err := reconcile(t, r, rr); err != nil {
		t.Fatal(err)
	}

	report, ok := getReport(t, rr)
	if !ok {
		t.Fatal("no VerificationReport was created")
	}
	t.Cleanup(func() { _ = admin.Delete(context.Background(), report) })
	want := v1alpha1.VerificationReportSpec{RequestUID: v1alpha1.UID(rr.UID), MergeCommitSHA: merge,
		TargetUID: v1alpha1.UID(w.deploy.UID), TemplateRevision: revision, Container: container}
	if report.Spec != want {
		t.Errorf("spec = %+v, want %+v", report.Spec, want)
	}
	if n := len(report.OwnerReferences); n != 1 {
		t.Fatalf("%d owner references, want 1", n)
	}
	o := report.OwnerReferences[0]
	if o.UID != rr.UID || o.Kind != "RemediationRequest" || o.Name != rr.Name || o.APIVersion != v1alpha1.GroupVersion.String() {
		t.Errorf("owner reference %+v does not name the request", o)
	}
	if o.Controller == nil || !*o.Controller {
		t.Errorf("controller = %v, want true", o.Controller)
	}
	if o.BlockOwnerDeletion == nil || *o.BlockOwnerDeletion {
		t.Errorf("blockOwnerDeletion = %v, want false", o.BlockOwnerDeletion)
	}
	st := report.Status
	if st.Sample != 2 || st.PodsCovered != 1 || st.DesiredReplicas != 1 || st.WindowStart == nil || st.LastReset != nil {
		t.Errorf("status after two good samples = %+v", st)
	}
	if len(st.Baselines) != 1 || st.Baselines[0].PodUID != v1alpha1.UID(w.pod.UID) {
		t.Errorf("baselines = %+v, want one for the pod", st.Baselines)
	}

	var all v1alpha1.VerificationReportList
	if err := admin.List(t.Context(), &all, client.InNamespace(operatorNS)); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, it := range all.Items {
		if it.Spec.RequestUID == v1alpha1.UID(rr.UID) {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d reports for the request, want 1", n)
	}
}

// TestNoReportForRequestsItDoesNotSample: a request that is not Verifying, one without a merge receipt, one on a
// ScaledObject and one whose target is outside the watched namespaces get no report. Only the one without a
// receipt, which the executor may still record, is looked at again.
func TestNoReportForRequestsItDoesNotSample(t *testing.T) {
	w := newWorkload(t, watchedNS)
	outside := newWorkload(t, unwatched)
	cases := map[string]*v1alpha1.RemediationRequest{
		"Merged":              request(t, w, v1alpha1.PhaseMerged, false),
		"no receipt":          request(t, w, v1alpha1.PhaseVerifying, true),
		"unwatched namespace": request(t, outside, v1alpha1.PhaseVerifying, false),
	}
	so := testfixtures.RemediationRequest(operatorNS, unique("rr"))
	mustCreate(t, so)
	setStatus(t, so, v1alpha1.PhaseVerifying, false)
	cases["ScaledObject"] = so
	r := newReconciler(admin, admin, &clock{now: time.Now()})
	for name, rr := range cases {
		res, err := reconcile(t, r, rr)
		if err != nil {
			t.Errorf("%s: %v", name, err)
		}
		want := time.Duration(0)
		if name == "no receipt" {
			want = clearing.PollInterval
		}
		if res.RequeueAfter != want {
			t.Errorf("%s: requeued after %v, want %v", name, res.RequeueAfter, want)
		}
		if _, ok := getReport(t, rr); ok {
			t.Errorf("%s: a report was created", name)
		}
	}
}

// TestNoReportForARequestBeingDeleted: a request in Verifying that a finalizer still holds is not sampled and
// not looked at again.
func TestNoReportForARequestBeingDeleted(t *testing.T) {
	w := newWorkload(t, watchedNS)
	rr := request(t, w, v1alpha1.PhaseVerifying, false)
	key := client.ObjectKeyFromObject(rr)
	const hold = "test.shadewright.io/hold"
	finalizers := func(v string) client.Patch {
		return client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"finalizers":`+v+`}}`))
	}
	if err := admin.Patch(context.Background(), rr, finalizers(`["`+hold+`"]`)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cur := &v1alpha1.RemediationRequest{}
		if err := admin.Get(context.Background(), key, cur); err == nil {
			_ = admin.Patch(context.Background(), cur, finalizers("null"))
		}
	})
	if err := admin.Delete(context.Background(), rr); err != nil {
		t.Fatal(err)
	}
	r := newReconciler(admin, admin, &clock{now: time.Now()})
	res, err := reconcile(t, r, rr)
	if err != nil {
		t.Fatal(err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("requeued after %v, want no requeue", res.RequeueAfter)
	}
	if _, ok := getReport(t, rr); ok {
		t.Error("a report was created for a request being deleted")
	}
}

// TestWritesOnlyVerificationReports: across the first sample (create and status update) and later ones, every
// write the verifier makes is on a VerificationReport, and the request is never written.
func TestWritesOnlyVerificationReports(t *testing.T) {
	w := newWorkload(t, watchedNS)
	rr := request(t, w, v1alpha1.PhaseVerifying, false)
	before := &v1alpha1.RemediationRequest{}
	if err := admin.Get(t.Context(), client.ObjectKeyFromObject(rr), before); err != nil {
		t.Fatal(err)
	}
	rec := &recorder{Client: admin}
	clk := &clock{now: time.Now()}
	r := newReconciler(rec, admin, clk)
	for range 3 {
		if _, err := reconcile(t, r, rr); err != nil {
			t.Fatal(err)
		}
		clk.now = clk.now.Add(clearing.PollInterval)
	}
	if report, ok := getReport(t, rr); ok {
		t.Cleanup(func() { _ = admin.Delete(context.Background(), report) })
	}
	if len(rec.writes) == 0 {
		t.Fatal("the verifier wrote nothing")
	}
	for _, wr := range rec.writes {
		if !strings.HasSuffix(wr, "*v1alpha1.VerificationReport") {
			t.Errorf("write %q is not on a VerificationReport", wr)
		}
	}
	after := &v1alpha1.RemediationRequest{}
	if err := admin.Get(t.Context(), client.ObjectKeyFromObject(rr), after); err != nil {
		t.Fatal(err)
	}
	if after.ResourceVersion != before.ResourceVersion {
		t.Error("the request was written")
	}
}

// TestRestartKeepsTheBaselines: a new verifier (a restart, or another replica that took the lease) starts with
// no memory, reads the baselines from the report, and so takes a restart count raised while nobody watched as
// a Restarted reset, never as a new baseline.
func TestRestartKeepsTheBaselines(t *testing.T) {
	w := newWorkload(t, watchedNS)
	rr := request(t, w, v1alpha1.PhaseVerifying, false)
	clk := &clock{now: time.Now()}
	first := newReconciler(admin, admin, clk)
	for range 2 {
		if _, err := reconcile(t, first, rr); err != nil {
			t.Fatal(err)
		}
		clk.now = clk.now.Add(clearing.PollInterval)
	}
	report, ok := getReport(t, rr)
	if !ok {
		t.Fatal("no report")
	}
	t.Cleanup(func() { _ = admin.Delete(context.Background(), report) })
	if report.Status.LastReset != nil || report.Status.WindowStart == nil {
		t.Fatalf("the window did not open: %+v", report.Status)
	}

	w.setRestarts(t, 1)
	restarted := newReconciler(admin, admin, clk)
	if _, err := reconcile(t, restarted, rr); err != nil {
		t.Fatal(err)
	}
	report, _ = getReport(t, rr)
	st := report.Status
	if st.LastReset == nil || st.LastReset.Reason != v1alpha1.ResetRestarted {
		t.Fatalf("last reset = %+v, want Restarted", st.LastReset)
	}
	if st.TroubleResets != 1 {
		t.Errorf("troubleResets = %d, want 1", st.TroubleResets)
	}
	if len(st.Baselines) != 1 || st.Baselines[0].Restarts != 1 {
		t.Errorf("baselines = %+v, want the raised count", st.Baselines)
	}
}

// failingReader fails every pod list.
type failingReader struct{ client.Reader }

func (f failingReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if _, ok := list.(*corev1.PodList); ok {
		return apierrors.NewServiceUnavailable("the API server is unavailable")
	}
	return f.Reader.List(ctx, list, opts...)
}

// TestFailedReadIsRetriedAndNotASample: a pod list that fails is returned as an error for a retry; no sample is
// written, so the counter does not rise and the window does not move.
func TestFailedReadIsRetriedAndNotASample(t *testing.T) {
	w := newWorkload(t, watchedNS)
	rr := request(t, w, v1alpha1.PhaseVerifying, false)
	clk := &clock{now: time.Now()}
	if _, err := reconcile(t, newReconciler(admin, admin, clk), rr); err != nil {
		t.Fatal(err)
	}
	report, ok := getReport(t, rr)
	if !ok {
		t.Fatal("no report")
	}
	t.Cleanup(func() { _ = admin.Delete(context.Background(), report) })
	clk.now = clk.now.Add(clearing.PollInterval)
	_, err := reconcile(t, newReconciler(admin, failingReader{admin}, clk), rr)
	if !apierrors.IsServiceUnavailable(err) {
		t.Fatalf("Reconcile = %v, want the list error for a retry", err)
	}
	after, _ := getReport(t, rr)
	if after.Status.Sample != report.Status.Sample || after.ResourceVersion != report.ResourceVersion {
		t.Errorf("a failed read wrote a sample: %d -> %d", report.Status.Sample, after.Status.Sample)
	}
}

// TestReportBoundToAnotherRolloutIsNotWritten: a report under the request's name whose spec names another merge
// commit is left as it is, and not retried on a timer: its spec is immutable.
func TestReportBoundToAnotherRolloutIsNotWritten(t *testing.T) {
	w := newWorkload(t, watchedNS)
	rr := request(t, w, v1alpha1.PhaseVerifying, false)
	other := &v1alpha1.VerificationReport{
		ObjectMeta: metav1.ObjectMeta{Namespace: operatorNS, Name: clearing.ReportName(rr.UID)},
		Spec: v1alpha1.VerificationReportSpec{RequestUID: v1alpha1.UID(rr.UID), MergeCommitSHA: strings.Repeat("e", 40),
			TargetUID: v1alpha1.UID(w.deploy.UID), TemplateRevision: revision, Container: container},
	}
	mustCreate(t, other)
	res, err := reconcile(t, newReconciler(admin, admin, &clock{now: time.Now()}), rr)
	if err != nil {
		t.Fatal(err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("RequeueAfter = %v, want no requeue", res.RequeueAfter)
	}
	got, _ := getReport(t, rr)
	if got.Status.Sample != 0 {
		t.Errorf("the verifier wrote a sample into a report bound to another merge: %+v", got.Status)
	}
}
