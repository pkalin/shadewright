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

package revert

import (
	"context"
	"errors"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	fx "github.com/pkalin/shadewright/internal/testfixtures"
	"github.com/pkalin/shadewright/internal/testutil/importguard"
)

const (
	failedName  = "remediation-failed"
	ns          = "shadewright-system"
	failedUID   = "21212121-2121-4121-8121-212121212121"
	incidentUID = "22222222-2222-4222-8222-222222222222"
	targetUID   = "33333333-3333-4333-8333-333333333333"
)

var errBoom = errors.New("boom")

// world is the objects of one case: a request that ended Failed with Verified False, its Incident and its
// target, each edited by the case before the client is built.
type world struct {
	failed *remediationv1alpha1.RemediationRequest
	inc    *remediationv1alpha1.Incident
	target *appsv1.Deployment
	extra  []client.Object
	funcs  interceptor.Funcs
}

func newWorld() *world {
	failed := fx.MemoryLimitRequest(ns, failedName)
	failed.UID = failedUID
	failed.Spec.IncidentRef = remediationv1alpha1.IncidentRef{Name: "incident-a", UID: incidentUID}
	failed.Status.Phase = remediationv1alpha1.PhaseFailed
	meta.SetStatusCondition(&failed.Status.Conditions, metav1.Condition{Type: remediationv1alpha1.ConditionVerified,
		Status: metav1.ConditionFalse, Reason: "OOMKilled"})
	inc := fx.Incident(ns, "incident-a")
	inc.UID = incidentUID
	target := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: fx.TargetNamespace, Name: fx.WorkloadName,
		UID: targetUID}}
	return &world{failed: failed, inc: inc, target: target}
}

func (w *world) build(t *testing.T) (*Proposer, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := remediationv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	objs := append([]client.Object{}, w.extra...)
	for _, o := range []client.Object{w.failed, w.inc, w.target} {
		if o != nil && !isNil(o) {
			objs = append(objs, o)
		}
	}
	k := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(&remediationv1alpha1.Incident{}, &remediationv1alpha1.RemediationRequest{}).
		WithInterceptorFuncs(w.funcs).Build()
	return &Proposer{Client: k, Reader: k, Namespace: ns}, k
}

func isNil(o client.Object) bool {
	switch v := o.(type) {
	case *remediationv1alpha1.RemediationRequest:
		return v == nil
	case *remediationv1alpha1.Incident:
		return v == nil
	case *appsv1.Deployment:
		return v == nil
	}
	return false
}

func reconcileFailed(p *Proposer) error {
	_, err := p.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: ns, Name: failedName}})
	return err
}

var revertKey = types.NamespacedName{Namespace: ns, Name: string(remediationv1alpha1.RevertName(failedUID))}

// revertOf returns the revert in k, or nil when there is none.
func revertOf(t *testing.T, k client.Client) *remediationv1alpha1.RemediationRequest {
	t.Helper()
	rr := &remediationv1alpha1.RemediationRequest{}
	err := k.Get(context.Background(), revertKey, rr)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return rr
}

func recorded(t *testing.T, k client.Client) []remediationv1alpha1.RequestRef {
	t.Helper()
	inc := &remediationv1alpha1.Incident{}
	if err := k.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "incident-a"}, inc); err != nil {
		t.Fatal(err)
	}
	return inc.Status.RevertsProposed
}

var link0 = remediationv1alpha1.RequestRef{Name: failedName, UID: failedUID}

// TestProposesTheRevert: a request that ended Failed with Verified False gets its revert, with approval
// required (it is never merged without a person), and the Incident records it.
func TestProposesTheRevert(t *testing.T) {
	w := newWorld()
	timeout := metav1.Duration{Duration: 600_000_000_000}
	w.failed.Spec.Verification = remediationv1alpha1.Verification{Timeout: &timeout,
		Checks: []remediationv1alpha1.VerificationCheck{remediationv1alpha1.CheckWorkloadHealthy}}
	p, k := w.build(t)
	if err := reconcileFailed(p); err != nil {
		t.Fatal(err)
	}
	rr := revertOf(t, k)
	if rr == nil {
		t.Fatal("no revert was created")
	}
	s := rr.Spec
	if s.ApprovalRequired == nil || !*s.ApprovalRequired {
		t.Error("the revert does not require approval")
	}
	if s.Reverts == nil || *s.Reverts != link0 {
		t.Errorf("spec.reverts = %+v, want %+v", s.Reverts, link0)
	}
	if s.IncidentRef != w.failed.Spec.IncidentRef || s.TargetRef != w.failed.Spec.TargetRef ||
		s.PolicyRef != w.failed.Spec.PolicyRef || s.Mode != remediationv1alpha1.RemediationModeGitOps {
		t.Errorf("the revert names another Incident, target, policy or mode: %+v", s)
	}
	if !remediationv1alpha1.IsInverseAction(w.failed.Spec.Action, s.Action) {
		t.Errorf("the revert's action %+v is not the inverse of %+v", s.Action, w.failed.Spec.Action)
	}
	m := s.Action.SetContainerMemoryLimit
	if m.Previous.Cmp(resource.MustParse("384Mi")) != 0 || m.Desired.Cmp(resource.MustParse("256Mi")) != 0 {
		t.Errorf("the revert goes from %s to %s, want 384Mi to 256Mi", &m.Previous, &m.Desired)
	}
	if s.Verification.Timeout == nil || s.Verification.Timeout.Duration != timeout.Duration || len(s.Verification.Checks) != 0 {
		t.Errorf("verification = %+v, want the failed request's timeout and no extra checks", s.Verification)
	}
	if s.Replaces != nil || s.ChainRoot != "" {
		t.Error("a revert never replaces a request")
	}
	if got := recorded(t, k); !equality.Semantic.DeepEqual(got, []remediationv1alpha1.RequestRef{link0}) {
		t.Errorf("status.revertsProposed = %+v, want the failed request", got)
	}
}

// TestOneRevertForGood: once recorded, a failed request gets no second revert, also after its revert was
// deleted, when the derived name is free again.
func TestOneRevertForGood(t *testing.T) {
	w := newWorld()
	p, k := w.build(t)
	if err := reconcileFailed(p); err != nil {
		t.Fatal(err)
	}
	rr := revertOf(t, k)
	if rr == nil {
		t.Fatal("no revert was created")
	}
	if err := k.Delete(context.Background(), rr); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := reconcileFailed(p); err != nil {
			t.Fatal(err)
		}
	}
	if revertOf(t, k) != nil {
		t.Error("a second revert was created after the first was deleted")
	}
	if got := recorded(t, k); len(got) != 1 {
		t.Errorf("status.revertsProposed = %+v, want one entry", got)
	}
}

// TestNeverProposes: the cases that never give a revert, and leave nothing recorded.
func TestNeverProposes(t *testing.T) {
	for name, edit := range map[string]func(w *world){
		"a revert of a revert": func(w *world) {
			w.failed.Spec.Reverts = &remediationv1alpha1.RequestRef{Name: "remediation-older",
				UID: "23232323-2323-4323-8323-232323232323"}
			w.failed.Spec.ApprovalRequired = new(true)
		},
		"Inconclusive": func(w *world) {
			w.failed.Status.Phase = remediationv1alpha1.PhaseInconclusive
			w.failed.Status.Conditions[0].Status = metav1.ConditionUnknown
		},
		"Inconclusive with Verified False": func(w *world) {
			w.failed.Status.Phase = remediationv1alpha1.PhaseInconclusive
		},
		"Failed with Verified Unknown": func(w *world) { w.failed.Status.Conditions[0].Status = metav1.ConditionUnknown },
		"Failed with Verified True":    func(w *world) { w.failed.Status.Conditions[0].Status = metav1.ConditionTrue },
		"Failed without Verified":      func(w *world) { w.failed.Status.Conditions = nil },
		"Denied":                       func(w *world) { w.failed.Status.Phase = remediationv1alpha1.PhaseDenied },
		"Succeeded":                    func(w *world) { w.failed.Status.Phase = remediationv1alpha1.PhaseSucceeded },
		"Verifying":                    func(w *world) { w.failed.Status.Phase = remediationv1alpha1.PhaseVerifying },
		"the target gone":              func(w *world) { w.target = nil },
		"the target made again": func(w *world) {
			w.target.UID = "24242424-2424-4424-8424-242424242424"
		},
		"the target being deleted": func(w *world) {
			w.target.DeletionTimestamp = &metav1.Time{Time: metav1.Now().Time}
			w.target.Finalizers = []string{"example.com/hold"}
		},
		"values that cannot be inverted": func(w *world) {
			w.failed.Spec.Action = remediationv1alpha1.Action{Type: remediationv1alpha1.ActionAdjustKedaMaxReplicas}
		},
		"an action type this version does not know": func(w *world) {
			w.failed.Spec.Action = remediationv1alpha1.Action{Type: "NoSuchAction"}
		},
		"the Incident gone": func(w *world) { w.inc = nil },
		"an Incident of another UID": func(w *world) {
			w.inc.UID = "25252525-2525-4525-8525-252525252525"
		},
		"a full revertsProposed": func(w *world) {
			for i := range remediationv1alpha1.MaxRevertsProposed {
				w.inc.Status.RevertsProposed = append(w.inc.Status.RevertsProposed, remediationv1alpha1.RequestRef{
					Name: "remediation-other", UID: remediationv1alpha1.UID(strings.Repeat(string(rune('a'+i%6)), 8) +
						"-0000-4000-8000-" + strings.Repeat("0", 10) + string(rune('a'+i/6)) + string(rune('a'+i%6)))})
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld()
			edit(w)
			p, k := w.build(t)
			if err := reconcileFailed(p); err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			if revertOf(t, k) != nil {
				t.Error("a revert was created")
			}
			if w.inc != nil && w.inc.UID == incidentUID {
				for _, r := range recorded(t, k) {
					if r == link0 {
						t.Error("the failed request was recorded")
					}
				}
			}
		})
	}
}

// TestRevertible covers the predicate the controller watches with, which Reconcile also applies.
func TestRevertible(t *testing.T) {
	w := newWorld()
	if !Revertible(w.failed) {
		t.Error("a request Failed with Verified False is revertible")
	}
	w.failed.DeletionTimestamp = &metav1.Time{}
	if Revertible(w.failed) {
		t.Error("a request being deleted is not revertible")
	}
	if Revertible(nil) {
		t.Error("nil is not revertible")
	}
}

// TestFailedReadIsRetried: a failed read of the request, the Incident or the target returns an error for a
// retry, creates nothing and records nothing; it is never taken for "gone".
func TestFailedReadIsRetried(t *testing.T) {
	for _, kind := range []string{"RemediationRequest", "Incident", "Deployment"} {
		t.Run(kind, func(t *testing.T) {
			w := newWorld()
			down := true
			w.funcs.Get = func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object,
				opts ...client.GetOption) error {
				if down && objKind(c, obj) == kind {
					return apierrors.NewServiceUnavailable("down")
				}
				return c.Get(ctx, key, obj, opts...)
			}
			p, k := w.build(t)
			if err := reconcileFailed(p); err == nil {
				t.Error("a failed read was not returned as an error")
			}
			down = false
			if revertOf(t, k) != nil {
				t.Error("a revert was created after a failed read")
			}
		})
	}
}

func objKind(c client.WithWatch, obj client.Object) string {
	if k := obj.GetObjectKind().GroupVersionKind().Kind; k != "" {
		return k
	}
	gvk, err := c.GroupVersionKindFor(obj)
	if err != nil {
		return ""
	}
	return gvk.Kind
}

// TestAlreadyExistsWithTheSameLink: a revert made by an earlier pass whose record was lost is taken as made,
// and recorded.
func TestAlreadyExistsWithTheSameLink(t *testing.T) {
	w := newWorld()
	spec, _ := Spec(w.failed)
	w.extra = append(w.extra, &remediationv1alpha1.RemediationRequest{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: revertKey.Name}, Spec: spec})
	p, k := w.build(t)
	if err := reconcileFailed(p); err != nil {
		t.Fatal(err)
	}
	if got := recorded(t, k); len(got) != 1 || got[0] != link0 {
		t.Errorf("status.revertsProposed = %+v, want the failed request", got)
	}
}

// TestAlreadyExistsWithAnotherLink: a request at the derived name that reverts something else, or nothing, is
// an error, and is left as it is.
func TestAlreadyExistsWithAnotherLink(t *testing.T) {
	for name, reverts := range map[string]*remediationv1alpha1.RequestRef{
		"another UID":  {Name: failedName, UID: "26262626-2626-4626-8626-262626262626"},
		"another name": {Name: "remediation-other", UID: failedUID},
		"no link":      nil,
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld()
			spec, _ := Spec(w.failed)
			spec.Reverts = reverts
			squat := &remediationv1alpha1.RemediationRequest{
				ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: revertKey.Name}, Spec: spec}
			w.extra = append(w.extra, squat)
			p, k := w.build(t)
			if err := reconcileFailed(p); err == nil {
				t.Error("an existing request with another link was taken as the revert")
			}
			got := revertOf(t, k)
			if got == nil || !equality.Semantic.DeepEqual(got.Spec.Reverts, reverts) {
				t.Errorf("the existing request was changed: %+v", got)
			}
			if len(recorded(t, k)) != 0 {
				t.Error("the failed request was recorded")
			}
		})
	}
}

// TestLostCreateAnswer: a create the server took whose answer was lost returns an error; the retry finds the
// revert (AlreadyExists with the same link) and records it, with one revert in all.
func TestLostCreateAnswer(t *testing.T) {
	w := newWorld()
	lose := true
	w.funcs.Create = func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		if err := c.Create(ctx, obj, opts...); err != nil {
			return err
		}
		if lose {
			lose = false
			return apierrors.NewTimeoutError("the answer was lost", 1)
		}
		return nil
	}
	p, k := w.build(t)
	if err := reconcileFailed(p); err == nil {
		t.Fatal("a lost create answer was not returned as an error")
	}
	if len(recorded(t, k)) != 0 {
		t.Error("the revert was recorded although its create did not answer")
	}
	if err := reconcileFailed(p); err != nil {
		t.Fatalf("the retry: %v", err)
	}
	if revertOf(t, k) == nil {
		t.Fatal("no revert")
	}
	if got := recorded(t, k); len(got) != 1 || got[0] != link0 {
		t.Errorf("status.revertsProposed = %+v, want the failed request", got)
	}
}

// TestLostRecordWrite: a record write that fails returns an error; the retry finds the revert and records it.
func TestLostRecordWrite(t *testing.T) {
	w := newWorld()
	fail := true
	w.funcs.SubResourceUpdate = func(ctx context.Context, c client.Client, sub string, obj client.Object,
		opts ...client.SubResourceUpdateOption) error {
		if fail {
			fail = false
			return errBoom
		}
		return c.SubResource(sub).Update(ctx, obj, opts...)
	}
	p, k := w.build(t)
	if err := reconcileFailed(p); err == nil {
		t.Fatal("a failed record write was not returned as an error")
	}
	if revertOf(t, k) == nil {
		t.Fatal("no revert after the first pass")
	}
	if err := reconcileFailed(p); err != nil {
		t.Fatalf("the retry: %v", err)
	}
	if got := recorded(t, k); len(got) != 1 || got[0] != link0 {
		t.Errorf("status.revertsProposed = %+v, want the failed request", got)
	}
}

// TestOtherNamespaceIgnored: the proposer acts on requests of the operator namespace only.
func TestOtherNamespaceIgnored(t *testing.T) {
	w := newWorld()
	w.failed.Namespace = "elsewhere"
	p, k := w.build(t)
	_, err := p.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: "elsewhere", Name: failedName}})
	if err != nil {
		t.Fatal(err)
	}
	if revertOf(t, k) != nil {
		t.Error("a revert was created for a request outside the operator namespace")
	}
}

// TestNoModelOnThisPath: the proposer reaches no model, no model provider and nothing of the analysis loop,
// and the analysis loop does not import the proposer, so a model proposal cannot reach this path.
func TestNoModelOnThisPath(t *testing.T) {
	root := importguard.ModuleRoot(t)
	self := importguard.Module + "internal/analyzer/revert"
	for _, pkg := range importguard.Deps(t, root, self) {
		for _, banned := range []string{"internal/analysis", "internal/providers", "internal/analyzer/models",
			"internal/tools", "internal/incidentcontext"} {
			if strings.HasPrefix(pkg.ImportPath, importguard.Module+banned) {
				t.Errorf("the revert proposer depends on %s", pkg.ImportPath)
			}
		}
	}
	for _, pkg := range importguard.Deps(t, root, importguard.Module+"internal/analysis") {
		if pkg.ImportPath == self {
			t.Error("the analysis loop depends on the revert proposer")
		}
	}
}
