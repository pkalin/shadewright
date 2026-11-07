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

package detection

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/modules"
	"github.com/pkalin/shadewright/internal/modules/aks"
	"github.com/pkalin/shadewright/internal/modules/eks"
	"github.com/pkalin/shadewright/internal/modules/gke"
)

var request = ctrl.Request{NamespacedName: client.ObjectKey{Name: remediationv1alpha1.SettingsName}}

func fakeScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := remediationv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func aksNode(name string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{aks.ClusterLabel: "rg"}},
		Spec:       corev1.NodeSpec{ProviderID: "azure:///subscriptions/s/vm/" + name},
	}
}

func aksSettings() *remediationv1alpha1.RemediationSettings {
	return &remediationv1alpha1.RemediationSettings{
		ObjectMeta: metav1.ObjectMeta{Name: remediationv1alpha1.SettingsName},
		Spec: remediationv1alpha1.RemediationSettingsSpec{Modules: &remediationv1alpha1.Modules{
			Platforms: []remediationv1alpha1.ModuleName{aks.Name}}},
	}
}

func newDetector(t *testing.T, c client.Client, gv GroupVersionsFunc) *Detector {
	t.Helper()
	d, err := New(Config{Registry: registry(t), GroupVersions: gv})
	if err != nil {
		t.Fatal(err)
	}
	d.reader, d.writer = c, c
	return d
}

func noGroups(context.Context) ([]string, error) { return nil, nil }

func TestNewNeedsRegistryAndDiscovery(t *testing.T) {
	if _, err := New(Config{GroupVersions: noGroups}); err == nil {
		t.Error("New without a registry succeeded")
	}
	if _, err := New(Config{Registry: registry(t)}); err == nil {
		t.Error("New without a discovery function succeeded")
	}
	d, err := New(Config{Registry: registry(t), GroupVersions: noGroups})
	if err != nil {
		t.Fatal(err)
	}
	if d.Selection().ActivePlatform() != modules.GenericPlatform || len(d.Selection().Active()) != 1 {
		t.Errorf("a new detector is not generic only: %+v", d.Selection().Active())
	}
}

// TestDetectionFailureUsesTheCurrentSpec checks that when detection fails, the active modules follow the
// current spec with the last detection that succeeded, so disabling a module takes effect at once, and that
// the status says so with DetectionFailed.
func TestDetectionFailureUsesTheCurrentSpec(t *testing.T) {
	ctx := t.Context()
	s := aksSettings()
	c := clientfake.NewClientBuilder().WithScheme(fakeScheme(t)).WithObjects(s, aksNode("n0")).
		WithStatusSubresource(s).Build()
	fail := false
	d := newDetector(t, c, func(context.Context) ([]string, error) {
		if fail {
			return nil, errors.New("discovery is down")
		}
		return nil, nil
	})
	if _, err := d.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	if d.Selection().ActivePlatform() != aks.Name {
		t.Fatalf("active = %q, want aks", d.Selection().ActivePlatform())
	}

	var cur remediationv1alpha1.RemediationSettings
	if err := c.Get(ctx, request.NamespacedName, &cur); err != nil {
		t.Fatal(err)
	}
	cur.Spec.Modules = nil
	if err := c.Update(ctx, &cur); err != nil {
		t.Fatal(err)
	}
	fail = true
	if _, err := d.Reconcile(ctx, request); err == nil {
		t.Error("Reconcile with failing discovery returned no error")
	}
	sel := d.Selection()
	if sel.ActivePlatform() != modules.GenericPlatform || sel.DetectedPlatform() != aks.Name {
		t.Errorf("after aks was disabled and detection failed: detected %q, active %q, want aks and generic",
			sel.DetectedPlatform(), sel.ActivePlatform())
	}
	if err := c.Get(ctx, request.NamespacedName, &cur); err != nil {
		t.Fatal(err)
	}
	want := remediationv1alpha1.PlatformStatus{Detected: aks.Name, Active: remediationv1alpha1.GenericPlatform}
	if cur.Status.Platform == nil || *cur.Status.Platform != want {
		t.Errorf("status = %+v, want %+v from the spec and the last detection", cur.Status.Platform, want)
	}
	cond := meta.FindStatusCondition(cur.Status.Conditions, ConditionDetectionFailed)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != ReasonDetectionFailed ||
		strings.Contains(cond.Message, "discovery is down") {
		t.Errorf("DetectionFailed = %+v, want True with a fixed message", cond)
	}
	fail = false
	if _, err := d.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, request.NamespacedName, &cur); err != nil {
		t.Fatal(err)
	}
	if cond := meta.FindStatusCondition(cur.Status.Conditions, ConditionDetectionFailed); cond == nil ||
		cond.Status != metav1.ConditionFalse {
		t.Errorf("DetectionFailed after a run that succeeded = %+v, want False", cond)
	}
}

// TestFailureWithoutAGoodDetectionReplacesTheReport checks that a failed first run does not leave an earlier
// report in place: status says generic active, nothing detected and no integration, with DetectionFailed True;
// a second equal failure writes nothing.
func TestFailureWithoutAGoodDetectionReplacesTheReport(t *testing.T) {
	ctx := t.Context()
	s := aksSettings()
	s.Status.Platform = &remediationv1alpha1.PlatformStatus{Detected: aks.Name, Active: aks.Name}
	s.Status.Integrations = []remediationv1alpha1.IntegrationStatus{{Name: "keda", Detected: true, Version: "keda.sh/v1alpha1", Active: true}}
	writes := 0
	c := clientfake.NewClientBuilder().WithScheme(fakeScheme(t)).WithObjects(s, aksNode("n0")).
		WithStatusSubresource(s).WithInterceptorFuncs(interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, cl client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			writes++
			return cl.SubResource(sub).Update(ctx, obj, opts...)
		},
	}).Build()
	d := newDetector(t, c, func(context.Context) ([]string, error) { return nil, errors.New("down") })
	for range 2 {
		if _, err := d.Reconcile(ctx, request); err == nil {
			t.Fatal("Reconcile with failing discovery returned no error")
		}
	}
	var cur remediationv1alpha1.RemediationSettings
	if err := c.Get(ctx, request.NamespacedName, &cur); err != nil {
		t.Fatal(err)
	}
	if p := cur.Status.Platform; p == nil || *p != (remediationv1alpha1.PlatformStatus{Active: remediationv1alpha1.GenericPlatform}) ||
		len(cur.Status.Integrations) != 0 {
		t.Errorf("status = %+v %+v, want generic active and nothing detected", p, cur.Status.Integrations)
	}
	if cond := meta.FindStatusCondition(cur.Status.Conditions, ConditionDetectionFailed); cond == nil ||
		cond.Status != metav1.ConditionTrue {
		t.Errorf("DetectionFailed = %+v, want True", cond)
	}
	if writes != 1 {
		t.Errorf("%d status writes for two equal failed runs, want 1", writes)
	}
	if d.Selection().ActivePlatform() != modules.GenericPlatform {
		t.Errorf("active = %q, want generic", d.Selection().ActivePlatform())
	}
}

// TestDeletedSettingsStopTheLoop checks that settings that do not exist give generic only and no periodic run.
func TestDeletedSettingsStopTheLoop(t *testing.T) {
	ctx := t.Context()
	s := aksSettings()
	c := clientfake.NewClientBuilder().WithScheme(fakeScheme(t)).WithObjects(s, aksNode("n0")).
		WithStatusSubresource(s).Build()
	d := newDetector(t, c, noGroups)
	if res, err := d.Reconcile(ctx, request); err != nil || res.RequeueAfter != DefaultInterval {
		t.Fatalf("Reconcile = %+v, %v", res, err)
	}
	if err := c.Delete(ctx, s); err != nil {
		t.Fatal(err)
	}
	res, err := d.Reconcile(ctx, request)
	if err != nil || res.RequeueAfter != 0 {
		t.Errorf("Reconcile without settings = %+v, %v; want no requeue", res, err)
	}
	if d.Selection().ActivePlatform() != modules.GenericPlatform {
		t.Errorf("active = %q without settings, want generic", d.Selection().ActivePlatform())
	}
}

// TestSettingsReadFailureKeepsTheSelection checks that settings that cannot be read, for a reason other than
// not found, keep the previous selection and return the error for a retry with backoff.
func TestSettingsReadFailureKeepsTheSelection(t *testing.T) {
	ctx := t.Context()
	s := aksSettings()
	broken := false
	c := clientfake.NewClientBuilder().WithScheme(fakeScheme(t)).WithObjects(s, aksNode("n0")).
		WithStatusSubresource(s).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*remediationv1alpha1.RemediationSettings); ok && broken {
				return apierrors.NewServiceUnavailable("etcd is down")
			}
			return cl.Get(ctx, key, obj, opts...)
		},
	}).Build()
	d := newDetector(t, c, noGroups)
	if _, err := d.Reconcile(ctx, request); err != nil || d.Selection().ActivePlatform() != aks.Name {
		t.Fatalf("Reconcile = %v, active %q", err, d.Selection().ActivePlatform())
	}
	broken = true
	if _, err := d.Reconcile(ctx, request); err == nil {
		t.Error("Reconcile with unreadable settings returned no error")
	}
	if got := d.Selection().ActivePlatform(); got != aks.Name {
		t.Errorf("active = %q with unreadable settings, want aks kept", got)
	}
}

// TestStatusWriteRetriesOnConflict checks that a conflicting status update is read again and retried.
func TestStatusWriteRetriesOnConflict(t *testing.T) {
	ctx := t.Context()
	s := aksSettings()
	conflicts := 0
	c := clientfake.NewClientBuilder().WithScheme(fakeScheme(t)).WithObjects(s, aksNode("n0")).
		WithStatusSubresource(s).WithInterceptorFuncs(interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, cl client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if conflicts < 2 {
				conflicts++
				return apierrors.NewConflict(schema.GroupResource{Group: remediationv1alpha1.GroupVersion.Group,
					Resource: "remediationsettings"}, obj.GetName(), errors.New("modified"))
			}
			return cl.SubResource(sub).Update(ctx, obj, opts...)
		},
	}).Build()
	d := newDetector(t, c, noGroups)
	if _, err := d.Reconcile(ctx, request); err != nil {
		t.Fatalf("Reconcile = %v", err)
	}
	var cur remediationv1alpha1.RemediationSettings
	if err := c.Get(ctx, request.NamespacedName, &cur); err != nil {
		t.Fatal(err)
	}
	if conflicts != 2 || cur.Status.Platform == nil || cur.Status.Platform.Active != aks.Name {
		t.Errorf("after %d conflicts the status is %+v", conflicts, cur.Status.Platform)
	}

	// A write refused for another reason is returned, not retried.
	calls := 0
	c2 := clientfake.NewClientBuilder().WithScheme(fakeScheme(t)).WithObjects(aksSettings(), aksNode("n0")).
		WithStatusSubresource(&remediationv1alpha1.RemediationSettings{}).WithInterceptorFuncs(interceptor.Funcs{
		SubResourceUpdate: func(context.Context, client.Client, string, client.Object, ...client.SubResourceUpdateOption) error {
			calls++
			return apierrors.NewForbidden(schema.GroupResource{Resource: "remediationsettings"}, "default", errors.New("no"))
		},
	}).Build()
	if _, err := newDetector(t, c2, noGroups).Reconcile(ctx, request); !apierrors.IsForbidden(err) || calls != 1 {
		t.Errorf("Reconcile = %v after %d calls, want forbidden after 1", err, calls)
	}
}

// TestNoWriteWhenNothingChanged checks that a second run with the same result does not update the status.
func TestNoWriteWhenNothingChanged(t *testing.T) {
	ctx := t.Context()
	s := aksSettings()
	writes := 0
	c := clientfake.NewClientBuilder().WithScheme(fakeScheme(t)).WithObjects(s, aksNode("n0")).
		WithStatusSubresource(s).WithInterceptorFuncs(interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, cl client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			writes++
			return cl.SubResource(sub).Update(ctx, obj, opts...)
		},
	}).Build()
	d := newDetector(t, c, noGroups)
	for range 3 {
		if _, err := d.Reconcile(ctx, request); err != nil {
			t.Fatal(err)
		}
	}
	if writes != 1 {
		t.Errorf("%d status writes for three equal runs, want 1", writes)
	}
}

// TestTooManyNodesIsGeneric checks that a cluster with more nodes than detection reads is taken as generic.
func TestTooManyNodesIsGeneric(t *testing.T) {
	b := clientfake.NewClientBuilder().WithScheme(fakeScheme(t)).WithObjects(aksSettings()).
		WithStatusSubresource(&remediationv1alpha1.RemediationSettings{})
	for i := range MaxNodes + 1 {
		b = b.WithObjects(aksNode(fmt.Sprintf("n%d", i)))
	}
	d := newDetector(t, b.Build(), noGroups)
	if _, err := d.Reconcile(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	if got := d.Selection(); got.DetectedPlatform() != "" || got.ActivePlatform() != modules.GenericPlatform {
		t.Errorf("with %d AKS nodes: detected %q, active %q, want generic", MaxNodes+1, got.DetectedPlatform(), got.ActivePlatform())
	}
}

// TestCancelledContextStopsTheRun checks that a run whose context ended returns an error and writes nothing.
func TestCancelledContextStopsTheRun(t *testing.T) {
	s := aksSettings()
	c := clientfake.NewClientBuilder().WithScheme(fakeScheme(t)).WithObjects(s, aksNode("n0")).
		WithStatusSubresource(s).Build()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	d := newDetector(t, c, func(ctx context.Context) ([]string, error) { return nil, ctx.Err() })
	if _, err := d.Reconcile(ctx, request); !errors.Is(err, context.Canceled) {
		t.Errorf("Reconcile = %v, want context.Canceled", err)
	}
	var cur remediationv1alpha1.RemediationSettings
	if err := c.Get(t.Context(), request.NamespacedName, &cur); err != nil {
		t.Fatal(err)
	}
	if cur.Status.Platform != nil {
		t.Errorf("status written after cancellation: %+v", cur.Status.Platform)
	}
}

// TestDiscoveryHonoursTheContext points discovery at a server that never answers and checks that a cancelled
// context, and a deadline, end the call at once.
func TestDiscoveryHonoursTheContext(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(release); srv.Close() })
	gv := DiscoveryGroupVersions(&rest.Config{Host: srv.URL})

	ctx, cancel := context.WithCancel(t.Context())
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()
	start := time.Now()
	if _, err := gv(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled discovery = %v, want context.Canceled", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("cancelled discovery returned after %v", d)
	}

	dctx, dcancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer dcancel()
	start = time.Now()
	if _, err := gv(dctx); err == nil {
		t.Error("discovery past its deadline returned no error")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("discovery past its deadline returned after %v", d)
	}
}

// TestHangingSettingsCallsEndAtTheDeadline checks that a settings read or a status write that never answers
// ends at Timeout with an error, so the run returns and is retried, instead of holding the controller's only
// worker (#243). Without the deadline the call waits for the reconcile's context, which has none.
func TestHangingSettingsCallsEndAtTheDeadline(t *testing.T) {
	hang := func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}
	for name, funcs := range map[string]interceptor.Funcs{
		"read": {Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*remediationv1alpha1.RemediationSettings); ok {
				return hang(ctx)
			}
			return cl.Get(ctx, key, obj, opts...)
		}},
		"write": {SubResourceUpdate: func(ctx context.Context, _ client.Client, _ string, _ client.Object, _ ...client.SubResourceUpdateOption) error {
			return hang(ctx)
		}},
	} {
		t.Run(name, func(t *testing.T) {
			s := aksSettings()
			c := clientfake.NewClientBuilder().WithScheme(fakeScheme(t)).WithObjects(s, aksNode("n0")).
				WithStatusSubresource(s).WithInterceptorFuncs(funcs).Build()
			d, err := New(Config{Registry: registry(t), GroupVersions: noGroups, Timeout: 100 * time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			d.reader, d.writer = c, c
			done := make(chan error, 1)
			go func() {
				_, err := d.Reconcile(t.Context(), request)
				done <- err
			}()
			select {
			case err := <-done:
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Errorf("Reconcile = %v, want context.DeadlineExceeded", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Reconcile did not return within 5 s of a 100 ms deadline")
			}
		})
	}
}

// TestStatusReportsEKSAndGKE checks the settings status on EKS and GKE node fixtures with only eks enabled: an
// enabled platform is active, a detected one that is not enabled is reported but generic is active, and nodes
// of two platforms report no detection.
func TestStatusReportsEKSAndGKE(t *testing.T) {
	cases := []struct {
		name  string
		files []string
		want  remediationv1alpha1.PlatformStatus
	}{
		{"EKS, enabled", []string{fixtureEKS}, remediationv1alpha1.PlatformStatus{Detected: eks.Name, Active: eks.Name}},
		{"GKE, not enabled", []string{fixtureGKE},
			remediationv1alpha1.PlatformStatus{Detected: gke.Name, Active: remediationv1alpha1.GenericPlatform}},
		{"EKS and GKE nodes", []string{fixtureEKS, fixtureGKE},
			remediationv1alpha1.PlatformStatus{Active: remediationv1alpha1.GenericPlatform}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx := t.Context()
			s := &remediationv1alpha1.RemediationSettings{
				ObjectMeta: metav1.ObjectMeta{Name: remediationv1alpha1.SettingsName},
				Spec: remediationv1alpha1.RemediationSettingsSpec{Modules: &remediationv1alpha1.Modules{
					Platforms: []remediationv1alpha1.ModuleName{eks.Name}}},
			}
			b := clientfake.NewClientBuilder().WithScheme(fakeScheme(t)).WithObjects(s).WithStatusSubresource(s)
			for _, f := range c.files {
				n := &corev1.Node{}
				load(t, f, n)
				b = b.WithObjects(n)
			}
			cl := b.Build()
			d := newDetector(t, cl, noGroups)
			if _, err := d.Reconcile(ctx, request); err != nil {
				t.Fatal(err)
			}
			var cur remediationv1alpha1.RemediationSettings
			if err := cl.Get(ctx, request.NamespacedName, &cur); err != nil {
				t.Fatal(err)
			}
			if cur.Status.Platform == nil || *cur.Status.Platform != c.want {
				t.Errorf("status = %+v, want %+v", cur.Status.Platform, c.want)
			}
			if got := d.Selection().ActivePlatform(); got != string(c.want.Active) {
				t.Errorf("active = %q, want %q", got, c.want.Active)
			}
		})
	}
}

// TestStatusReportsNoWatchedNamespaces checks that an analyzer that watches no namespace says so in the
// settings status, and that one that watches some reports the condition False.
func TestStatusReportsNoWatchedNamespaces(t *testing.T) {
	for _, tc := range []struct {
		watched int
		want    metav1.ConditionStatus
		reason  string
	}{
		{0, metav1.ConditionTrue, ReasonNoNamespacesWatched},
		{2, metav1.ConditionFalse, ReasonNamespacesWatched},
	} {
		t.Run(fmt.Sprintf("watched=%d", tc.watched), func(t *testing.T) {
			ctx := t.Context()
			s := aksSettings()
			c := clientfake.NewClientBuilder().WithScheme(fakeScheme(t)).WithObjects(s, aksNode("n0")).
				WithStatusSubresource(s).Build()
			d, err := New(Config{Registry: registry(t), GroupVersions: noGroups, WatchedNamespaces: tc.watched})
			if err != nil {
				t.Fatal(err)
			}
			d.reader, d.writer = c, c
			if _, err := d.Reconcile(ctx, request); err != nil {
				t.Fatal(err)
			}
			var cur remediationv1alpha1.RemediationSettings
			if err := c.Get(ctx, request.NamespacedName, &cur); err != nil {
				t.Fatal(err)
			}
			cond := meta.FindStatusCondition(cur.Status.Conditions, ConditionNoWatchedNamespaces)
			if cond == nil || cond.Status != tc.want || cond.Reason != tc.reason {
				t.Errorf("NoWatchedNamespaces = %+v, want %s with reason %s", cond, tc.want, tc.reason)
			}
		})
	}
}

// TestSourcesNotWatchedIsReported checks the condition SymptomSourcesNotWatched (#409): Unknown before every
// kind of an active module has been checked, True with the kinds and their states while the collector reports
// kinds it does not watch (also while others are not checked yet), False once it watches them all, and absent
// when no collector runs. The message is built from the kinds only.
func TestSourcesNotWatchedIsReported(t *testing.T) {
	ctx := t.Context()
	s := aksSettings()
	c := clientfake.NewClientBuilder().WithScheme(fakeScheme(t)).WithObjects(s, aksNode("n0")).
		WithStatusSubresource(s).Build()
	var missing []NotWatched
	complete := false
	d, err := New(Config{Registry: registry(t), GroupVersions: noGroups,
		SourcesNotWatched: func() ([]NotWatched, bool) { return missing, complete }})
	if err != nil {
		t.Fatal(err)
	}
	d.reader, d.writer = c, c
	cond := func() *metav1.Condition {
		t.Helper()
		if _, err := d.Reconcile(ctx, request); err != nil {
			t.Fatal(err)
		}
		var cur remediationv1alpha1.RemediationSettings
		if err := c.Get(ctx, request.NamespacedName, &cur); err != nil {
			t.Fatal(err)
		}
		return meta.FindStatusCondition(cur.Status.Conditions, ConditionSymptomSourcesNotWatched)
	}
	if got := cond(); got == nil || got.Status != metav1.ConditionUnknown || got.Reason != ReasonSourcesNotChecked {
		t.Fatalf("condition %+v before the first check, want Unknown", got)
	}
	missing = []NotWatched{{Group: "kyverno.io", Version: "v1", Kind: "ClusterPolicy", State: "NotGranted"}}
	got := cond()
	if got == nil || got.Status != metav1.ConditionTrue || got.Reason != ReasonSourcesNotWatched ||
		!strings.Contains(got.Message, "kyverno.io/v1 ClusterPolicy (NotGranted)") {
		t.Fatalf("condition %+v, want True naming kyverno.io/v1 ClusterPolicy (NotGranted)", got)
	}
	missing, complete = nil, true
	if got := cond(); got == nil || got.Status != metav1.ConditionFalse || got.Reason != ReasonAllSourcesWatched {
		t.Fatalf("condition %+v, want False once every kind is watched", got)
	}

	plain := newDetector(t, c, noGroups)
	if _, err := plain.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	var cur remediationv1alpha1.RemediationSettings
	if err := c.Get(ctx, request.NamespacedName, &cur); err != nil {
		t.Fatal(err)
	}
	if got := meta.FindStatusCondition(cur.Status.Conditions, ConditionSymptomSourcesNotWatched); got != nil {
		t.Errorf("condition %+v from a detector without a collector, want none", got)
	}
}

// TestRecheckEnqueuesTheSettings: a notice on Recheck starts a run for the settings.
func TestRecheckEnqueuesTheSettings(t *testing.T) {
	ch := make(chan struct{}, 1)
	d, err := New(Config{Registry: registry(t), GroupVersions: noGroups, Recheck: ch})
	if err != nil {
		t.Fatal(err)
	}
	q := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]())
	defer q.ShutDown()
	if err := d.rechecks().Start(t.Context(), q); err != nil {
		t.Fatal(err)
	}
	ch <- struct{}{}
	got, _ := q.Get()
	if got != request {
		t.Errorf("enqueued %v, want %v", got, request)
	}
}
