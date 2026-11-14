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
	"time"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
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
	incName     = "incident-a"
	// hold is a finalizer that keeps an object in deletion.
	hold = "example.com/hold"
	// The kinds the tests name.
	kindDeployment = "Deployment"
	kindRequest    = "RemediationRequest"
)

var errBoom = errors.New("boom")

// now is the proposer's clock in every case; the failed request's Verified condition moved an hour before.
var now = time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)

// Names of cases that several tests share.
const (
	caseTargetGone       = "the target gone"
	caseIncidentDeleting = "the Incident being deleted"
	casePastAgeBound     = "past the age bound"
)

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
	failed.Spec.IncidentRef = remediationv1alpha1.IncidentRef{Name: incName, UID: incidentUID}
	failed.Status.Phase = remediationv1alpha1.PhaseFailed
	meta.SetStatusCondition(&failed.Status.Conditions, metav1.Condition{Type: remediationv1alpha1.ConditionVerified,
		Status: metav1.ConditionFalse, Reason: "OOMKilled", LastTransitionTime: metav1.NewTime(now.Add(-time.Hour))})
	inc := fx.Incident(ns, incName)
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
	return &Proposer{Client: k, Reader: k, Namespace: ns, Recorder: record.NewFakeRecorder(20),
		Now: func() time.Time { return now }}, k
}

// events returns the events p wrote so far.
func events(p *Proposer) []string {
	rec := p.Recorder.(*record.FakeRecorder)
	var out []string
	for {
		select {
		case e := <-rec.Events:
			out = append(out, e)
		default:
			return out
		}
	}
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

func incidentOf(t *testing.T, k client.Client) *remediationv1alpha1.Incident {
	t.Helper()
	inc := &remediationv1alpha1.Incident{}
	if err := k.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: incName}, inc); err != nil {
		t.Fatal(err)
	}
	return inc
}

func recorded(t *testing.T, k client.Client) []remediationv1alpha1.ProposedRevert {
	t.Helper()
	return incidentOf(t, k).Status.RevertsProposed
}

var (
	link0    = remediationv1alpha1.RequestRef{Name: failedName, UID: failedUID}
	created0 = remediationv1alpha1.ProposedRevert{RequestRef: link0, State: remediationv1alpha1.RevertCreated}
	pending0 = remediationv1alpha1.ProposedRevert{RequestRef: link0, State: remediationv1alpha1.RevertPending}
)

// onlyAbandoned reports whether k's Incident records exactly the failed request, as Abandoned with reason.
func onlyAbandoned(t *testing.T, k client.Client, reason remediationv1alpha1.RevertReason) bool {
	t.Helper()
	got := recorded(t, k)
	return len(got) == 1 && got[0] == remediationv1alpha1.ProposedRevert{RequestRef: link0,
		State: remediationv1alpha1.RevertAbandoned, Reason: reason}
}

// abandonedOnce reports whether events holds exactly one Warning event EventAbandoned.
func abandonedOnce(events []string) bool {
	return len(events) == 1 && strings.Contains(events[0], "Warning "+EventAbandoned+" ")
}

// noMatch makes the target's kind unknown to the API server, as when KEDA is uninstalled.
func noMatch(w *world) {
	w.funcs.Get = func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object,
		opts ...client.GetOption) error {
		if _, ok := obj.(*unstructured.Unstructured); ok {
			return &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: "apps", Kind: kindDeployment},
				SearchedVersions: []string{"v1"}}
		}
		return c.Get(ctx, key, obj, opts...)
	}
}

// forbidden makes the target unreadable for the analyzer.
func forbidden(w *world) {
	w.funcs.Get = func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object,
		opts ...client.GetOption) error {
		if _, ok := obj.(*unstructured.Unstructured); ok {
			return apierrors.NewForbidden(appsv1.Resource("deployments"), key.Name, errBoom)
		}
		return c.Get(ctx, key, obj, opts...)
	}
}

// incidentDeleting puts the Incident in deletion, held by a finalizer.
func incidentDeleting(w *world) {
	w.inc.DeletionTimestamp = &metav1.Time{Time: now}
	w.inc.Finalizers = []string{hold}
}

// onlyCreated reports whether k's Incident records exactly the failed request, as Created.
func onlyCreated(t *testing.T, k client.Client) bool {
	t.Helper()
	got := recorded(t, k)
	return len(got) == 1 && got[0] == created0
}

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
	if !onlyCreated(t, k) {
		t.Errorf("status.revertsProposed = %+v, want the failed request as Created", recorded(t, k))
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
		caseTargetGone:                 func(w *world) { w.target = nil },
		"the target made again": func(w *world) {
			w.target.UID = "24242424-2424-4424-8424-242424242424"
		},
		"the target being deleted": func(w *world) {
			w.target.DeletionTimestamp = &metav1.Time{Time: metav1.Now().Time}
			w.target.Finalizers = []string{hold}
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
		"a full revertsProposed": fill,
		"a target of a kind the action cannot target": func(w *world) {
			w.failed.Spec.TargetRef.APIVersion, w.failed.Spec.TargetRef.Kind = "v1", "ConfigMap"
		},
		"failed more than a day ago": func(w *world) {
			w.failed.Status.Conditions[0].LastTransitionTime = metav1.NewTime(now.Add(-MaxAge - time.Second))
		},
		"failed more than five minutes ahead": func(w *world) {
			w.failed.Status.Conditions[0].LastTransitionTime = metav1.NewTime(now.Add(MaxSkew + time.Second))
		},
		caseIncidentDeleting:           incidentDeleting,
		"the target's kind not served": noMatch,
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
					if r.RequestRef == link0 {
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
	if !failedUnverified(w.failed) {
		t.Error("a request being deleted is still watched, so its Pending entry moves to Abandoned")
	}
	if Revertible(nil) {
		t.Error("nil is not revertible")
	}
}

// TestFailedReadIsRetried: a failed read of the request, the Incident or the target returns an error for a
// retry, creates nothing and records nothing; it is never taken for "gone".
func TestFailedReadIsRetried(t *testing.T) {
	for _, kind := range []string{kindRequest, "Incident", kindDeployment} {
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
			if got := recorded(t, k); len(got) != 0 {
				t.Errorf("status.revertsProposed = %+v after a failed read, want nothing", got)
			}
			if err := reconcileFailed(p); err != nil {
				t.Fatalf("the healthy retry: %v", err)
			}
			if revertOf(t, k) == nil || !onlyCreated(t, k) {
				t.Errorf("after the healthy retry: revert %v, status.revertsProposed = %+v", revertOf(t, k),
					recorded(t, k))
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
	if !onlyCreated(t, k) {
		t.Errorf("status.revertsProposed = %+v, want the failed request as Created", recorded(t, k))
	}
}

// TestAlreadyExistsWithAnotherLink: a request at the derived name that reverts something else, or nothing,
// moves the entry to Abandoned (retrying does not move that request), with one Warning event on the failed
// request, and is left as it is.
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
			for range 2 {
				if err := reconcileFailed(p); err != nil {
					t.Fatalf("Reconcile: %v", err)
				}
			}
			if ev := events(p); !abandonedOnce(ev) || !strings.Contains(ev[0], "does not revert") {
				t.Errorf("events = %q, want one Warning %s naming the taken name", ev, EventAbandoned)
			}
			got := revertOf(t, k)
			if got == nil || !equality.Semantic.DeepEqual(got.Spec.Reverts, reverts) {
				t.Errorf("the existing request was changed: %+v", got)
			}
			if !onlyAbandoned(t, k, remediationv1alpha1.RevertReasonNameTaken) {
				t.Errorf("status.revertsProposed = %+v, want the failed request as Abandoned", recorded(t, k))
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
	if got := recorded(t, k); len(got) != 1 || got[0] != pending0 {
		t.Errorf("status.revertsProposed = %+v, want the failed request as Pending", got)
	}
	if err := reconcileFailed(p); err != nil {
		t.Fatalf("the retry: %v", err)
	}
	if revertOf(t, k) == nil {
		t.Fatal("no revert")
	}
	if !onlyCreated(t, k) {
		t.Errorf("status.revertsProposed = %+v, want the failed request as Created", recorded(t, k))
	}
}

// TestLostPendingWrite: a decision whose Pending entry was not written creates nothing; the retry decides again.
func TestLostPendingWrite(t *testing.T) {
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
		t.Fatal("a failed Pending write was not returned as an error")
	}
	if revertOf(t, k) != nil {
		t.Fatal("a revert was created before its decision was recorded")
	}
	if err := reconcileFailed(p); err != nil {
		t.Fatalf("the retry: %v", err)
	}
	if revertOf(t, k) == nil || !onlyCreated(t, k) {
		t.Errorf("after the retry: revert %v, status.revertsProposed = %+v", revertOf(t, k), recorded(t, k))
	}
}

// TestDecisionRecordedBeforeCreate: when the revert is created, the Incident already holds the failed request
// as Pending, so a lost record after the create cannot lead to a new decision.
func TestDecisionRecordedBeforeCreate(t *testing.T) {
	w := newWorld()
	var atCreate []remediationv1alpha1.ProposedRevert
	w.funcs.Create = func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		inc := &remediationv1alpha1.Incident{}
		if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: incName}, inc); err != nil {
			return err
		}
		atCreate = inc.Status.RevertsProposed
		return c.Create(ctx, obj, opts...)
	}
	p, k := w.build(t)
	if err := reconcileFailed(p); err != nil {
		t.Fatal(err)
	}
	if len(atCreate) != 1 || atCreate[0] != pending0 {
		t.Errorf("status.revertsProposed at the create = %+v, want the failed request as Pending", atCreate)
	}
	if !onlyCreated(t, k) {
		t.Errorf("status.revertsProposed = %+v, want the failed request as Created", recorded(t, k))
	}
}

// TestPendingKeepsTheDecision: a Pending entry keeps the decision that no second revert is proposed, not that
// this one must be made. It leads to the create when the checks pass, also with a full list (the entry is
// already in it), and to Abandoned when a check refuses, also with a full list.
func TestPendingKeepsTheDecision(t *testing.T) {
	for name, c := range map[string]struct {
		edit func(w *world)
		want remediationv1alpha1.ProposedRevert
	}{
		"the checks pass": {func(*world) {}, created0},
		caseTargetGone: {func(w *world) { w.target = nil }, remediationv1alpha1.ProposedRevert{RequestRef: link0,
			State: remediationv1alpha1.RevertAbandoned, Reason: remediationv1alpha1.RevertReasonTargetGone}},
		casePastAgeBound: {func(w *world) {
			w.failed.Status.Conditions[0].LastTransitionTime = metav1.NewTime(now.Add(-2 * MaxAge))
		}, remediationv1alpha1.ProposedRevert{RequestRef: link0, State: remediationv1alpha1.RevertAbandoned,
			Reason: remediationv1alpha1.RevertReasonOutsideAgeBound}},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld()
			fill(w)
			w.inc.Status.RevertsProposed[len(w.inc.Status.RevertsProposed)-1] = pending0
			c.edit(w)
			p, k := w.build(t)
			if err := reconcileFailed(p); err != nil {
				t.Fatal(err)
			}
			if made := revertOf(t, k) != nil; made != (c.want == created0) {
				t.Errorf("revert created = %v, want %v", made, c.want == created0)
			}
			got := recorded(t, k)
			if got[len(got)-1] != c.want {
				t.Errorf("the entry is %+v, want %+v", got[len(got)-1], c.want)
			}
		})
	}
}

// TestPendingRunsTheChecks: a retried create runs the checks of the first one. When one says no, no revert is
// created, the entry moves to Abandoned for good, and one Warning event on the failed request says why; a
// second pass changes nothing and writes no second event.
func TestPendingRunsTheChecks(t *testing.T) {
	for name, c := range map[string]struct {
		edit   func(w *world)
		reason remediationv1alpha1.RevertReason
	}{
		caseTargetGone: {func(w *world) { w.target = nil }, remediationv1alpha1.RevertReasonTargetGone},
		"the target made again": {func(w *world) { w.target.UID = "24242424-2424-4424-8424-242424242424" },
			remediationv1alpha1.RevertReasonTargetGone},
		"the target's kind not served": {noMatch, remediationv1alpha1.RevertReasonTargetKindUnsupported},
		"the target being deleted": {func(w *world) {
			w.target.DeletionTimestamp = &metav1.Time{Time: now}
			w.target.Finalizers = []string{hold}
		}, remediationv1alpha1.RevertReasonTargetGone},
		casePastAgeBound: {func(w *world) {
			w.failed.Status.Conditions[0].LastTransitionTime = metav1.NewTime(now.Add(-2 * MaxAge))
		}, remediationv1alpha1.RevertReasonOutsideAgeBound},
		"ahead of the clock": {func(w *world) {
			w.failed.Status.Conditions[0].LastTransitionTime = metav1.NewTime(now.Add(time.Hour))
		}, remediationv1alpha1.RevertReasonOutsideAgeBound},
		caseIncidentDeleting: {incidentDeleting, remediationv1alpha1.RevertReasonIncidentDeleting},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld()
			w.inc.Status.RevertsProposed = []remediationv1alpha1.ProposedRevert{pending0}
			c.edit(w)
			p, k := w.build(t)
			for range 2 {
				if err := reconcileFailed(p); err != nil {
					t.Fatalf("Reconcile: %v", err)
				}
			}
			if revertOf(t, k) != nil {
				t.Error("a revert was created")
			}
			if !onlyAbandoned(t, k, c.reason) {
				t.Errorf("status.revertsProposed = %+v, want the failed request as Abandoned with %s",
					recorded(t, k), c.reason)
			}
			if ev := events(p); !abandonedOnce(ev) || !strings.Contains(ev[0], string(c.reason)) {
				t.Errorf("events = %q, want one Warning %s naming %s", ev, EventAbandoned, c.reason)
			}
		})
	}
}

// TestCreateAfterAnotherCreatorFinished: a pass that read the entry as Pending, while another pass created the
// revert, moved the entry to Created and the revert was deleted, reads the Incident again right before the
// create and creates nothing.
func TestCreateAfterAnotherCreatorFinished(t *testing.T) {
	w := newWorld()
	w.inc.Status.RevertsProposed = []remediationv1alpha1.ProposedRevert{pending0}
	incidentReads := 0
	w.funcs.Get = func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object,
		opts ...client.GetOption) error {
		if err := c.Get(ctx, key, obj, opts...); err != nil {
			return err
		}
		if _, ok := obj.(*remediationv1alpha1.Incident); ok {
			if incidentReads++; incidentReads == 1 {
				// The other pass finishes after this read: Created, and its revert deleted again.
				done := &remediationv1alpha1.Incident{}
				if err := c.Get(ctx, key, done); err != nil {
					return err
				}
				done.Status.RevertsProposed = []remediationv1alpha1.ProposedRevert{created0}
				return c.Status().Update(ctx, done)
			}
		}
		return nil
	}
	p, k := w.build(t)
	if err := reconcileFailed(p); err != nil {
		t.Fatal(err)
	}
	if revertOf(t, k) != nil {
		t.Error("a second revert was created after another pass had finished")
	}
	if !onlyCreated(t, k) {
		t.Errorf("status.revertsProposed = %+v, want the failed request as Created", recorded(t, k))
	}
}

// TestDeletedRequestAbandons: a failed request deleted after its Pending write, or being deleted, moves the
// entry to Abandoned with one Warning event (on the Incident once the request is gone), and nothing is created.
func TestDeletedRequestAbandons(t *testing.T) {
	for name, c := range map[string]struct {
		edit func(w *world)
		on   string
	}{
		"gone": {func(w *world) { w.failed = nil }, "Incident"},
		"being deleted": {func(w *world) {
			w.failed.DeletionTimestamp = &metav1.Time{Time: now}
			w.failed.Finalizers = []string{hold}
		}, kindRequest},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld()
			w.inc.Status.RevertsProposed = []remediationv1alpha1.ProposedRevert{pending0}
			c.edit(w)
			p, k := w.build(t)
			fake := p.Recorder.(*record.FakeRecorder)
			on := &kindRecorder{EventRecorder: fake}
			p.Recorder = on
			for range 2 {
				if err := reconcileFailed(p); err != nil {
					t.Fatalf("Reconcile: %v", err)
				}
			}
			p.Recorder = fake
			if revertOf(t, k) != nil {
				t.Error("a revert was created")
			}
			if !onlyAbandoned(t, k, remediationv1alpha1.RevertReasonRequestGone) {
				t.Errorf("status.revertsProposed = %+v, want the failed request as Abandoned", recorded(t, k))
			}
			if ev := events(p); !abandonedOnce(ev) || len(on.kinds) != 1 || on.kinds[0] != c.on {
				t.Errorf("events = %q on %v, want one Warning %s on the %s", ev, on.kinds, EventAbandoned, c.on)
			}
		})
	}
}

// kindRecorder notes the kind of the object of each event.
type kindRecorder struct {
	record.EventRecorder
	kinds []string
}

func (r *kindRecorder) Eventf(obj runtime.Object, eventType, reason, format string, args ...any) {
	switch obj.(type) {
	case *remediationv1alpha1.Incident:
		r.kinds = append(r.kinds, "Incident")
	case *remediationv1alpha1.RemediationRequest:
		r.kinds = append(r.kinds, kindRequest)
	default:
		r.kinds = append(r.kinds, "other")
	}
	r.EventRecorder.Eventf(obj, eventType, reason, format, args...)
}

// TestInvalidCreateAbandons: a create the API server refuses as invalid never succeeds by retrying: the entry
// moves to Abandoned (CreateInvalid) with one Warning event, and the pass returns no error. The event has a
// fixed sentence: the API server's error text goes to the log only.
func TestInvalidCreateAbandons(t *testing.T) {
	w := newWorld()
	const detail = "field-detail-from-the-api-server"
	w.funcs.Create = func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		return apierrors.NewInvalid(schema.GroupKind{Group: remediationv1alpha1.GroupVersion.Group,
			Kind: kindRequest}, obj.GetName(), field.ErrorList{field.Invalid(field.NewPath("spec"), detail, detail)})
	}
	p, k := w.build(t)
	if err := reconcileFailed(p); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if revertOf(t, k) != nil {
		t.Error("a revert was created")
	}
	if !onlyAbandoned(t, k, remediationv1alpha1.RevertReasonCreateInvalid) {
		t.Errorf("status.revertsProposed = %+v, want the failed request as Abandoned", recorded(t, k))
	}
	if ev := events(p); !abandonedOnce(ev) || strings.Contains(ev[0], detail) {
		t.Errorf("events = %q, want one Warning %s without the API error text", ev, EventAbandoned)
	}
}

// TestLostMarkWrite: a create that succeeded whose move to Created failed is finished by the retry, with one
// revert. If the revert is deleted before that retry, the retry creates it once more: the known limit of
// ADR-0030 (#96), which a Pending entry cannot tell apart from a create that never happened.
func TestLostMarkWrite(t *testing.T) {
	for _, deleted := range []bool{false, true} {
		w := newWorld()
		writes := 0
		w.funcs.SubResourceUpdate = func(ctx context.Context, c client.Client, sub string, obj client.Object,
			opts ...client.SubResourceUpdateOption) error {
			if writes++; writes == 2 {
				return errBoom
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		}
		p, k := w.build(t)
		if err := reconcileFailed(p); err == nil {
			t.Fatal("a failed move to Created was not returned as an error")
		}
		rr := revertOf(t, k)
		if rr == nil {
			t.Fatal("no revert after the first pass")
		}
		if deleted {
			if err := k.Delete(context.Background(), rr); err != nil {
				t.Fatal(err)
			}
		}
		if err := reconcileFailed(p); err != nil {
			t.Fatalf("the retry: %v", err)
		}
		if revertOf(t, k) == nil || !onlyCreated(t, k) {
			t.Errorf("deleted %v: revert %v, status.revertsProposed = %+v", deleted, revertOf(t, k), recorded(t, k))
		}
	}
}

// fill fills the Incident's status.revertsProposed with other requests, each Created.
func fill(w *world) {
	for i := range remediationv1alpha1.MaxRevertsProposed {
		w.inc.Status.RevertsProposed = append(w.inc.Status.RevertsProposed, remediationv1alpha1.ProposedRevert{
			RequestRef: remediationv1alpha1.RequestRef{
				Name: "remediation-other", UID: remediationv1alpha1.UID(strings.Repeat(string(rune('a'+i%6)), 8) +
					"-0000-4000-8000-" + strings.Repeat("0", 10) + string(rune('a'+i/6)) + string(rune('a'+i%6)))},
			State: remediationv1alpha1.RevertCreated})
	}
}

// TestFullList: a full status.revertsProposed gives no revert and no retry: the Incident gets the condition
// RevertsStopped and one Warning event, and a second pass adds no second event.
func TestFullList(t *testing.T) {
	w := newWorld()
	fill(w)
	p, k := w.build(t)
	for range 2 {
		if err := reconcileFailed(p); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
	}
	if revertOf(t, k) != nil {
		t.Error("a revert was created")
	}
	c := meta.FindStatusCondition(incidentOf(t, k).Status.Conditions, remediationv1alpha1.ConditionRevertsStopped)
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != remediationv1alpha1.ReasonRevertListFull {
		t.Errorf("condition %s = %+v, want True with %s", remediationv1alpha1.ConditionRevertsStopped, c,
			remediationv1alpha1.ReasonRevertListFull)
	}
	if ev := events(p); len(ev) != 1 || !strings.Contains(ev[0], "Warning "+EventListFull) {
		t.Errorf("events = %q, want one Warning %s", ev, EventListFull)
	}
}

// TestFullListAfterTheTargetCheck: a list that filled up between the first read and the Pending write gives no
// revert either, and no error for a retry loop.
func TestFullListAfterTheTargetCheck(t *testing.T) {
	w := newWorld()
	filled := false
	w.funcs.Get = func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object,
		opts ...client.GetOption) error {
		if _, ok := obj.(*unstructured.Unstructured); ok && !filled {
			filled = true
			full := newWorld()
			fill(full)
			inc := &remediationv1alpha1.Incident{}
			if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: incName}, inc); err != nil {
				return err
			}
			inc.Status.RevertsProposed = full.inc.Status.RevertsProposed
			if err := c.Status().Update(ctx, inc); err != nil {
				return err
			}
		}
		return c.Get(ctx, key, obj, opts...)
	}
	p, k := w.build(t)
	if err := reconcileFailed(p); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if revertOf(t, k) != nil {
		t.Error("a revert was created")
	}
	if !meta.IsStatusConditionTrue(incidentOf(t, k).Status.Conditions, remediationv1alpha1.ConditionRevertsStopped) {
		t.Error("no RevertsStopped condition")
	}
}

// TestIncidentGoneEvent: a failed request whose Incident is gone or has another UID gets a Warning event, so a
// person sees why no revert was proposed; one past the age bound gets none.
func TestIncidentGoneEvent(t *testing.T) {
	for name, edit := range map[string]func(w *world){
		"gone":        func(w *world) { w.inc = nil },
		"another UID": func(w *world) { w.inc.UID = "25252525-2525-4525-8525-252525252525" },
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld()
			edit(w)
			p, _ := w.build(t)
			if err := reconcileFailed(p); err != nil {
				t.Fatal(err)
			}
			if ev := events(p); len(ev) != 1 || !strings.Contains(ev[0], "Warning "+EventIncidentGone) {
				t.Errorf("events = %q, want one Warning %s", ev, EventIncidentGone)
			}
		})
	}
	w := newWorld()
	w.inc = nil
	w.failed.Status.Conditions[0].LastTransitionTime = metav1.NewTime(now.Add(-2 * MaxAge))
	p, _ := w.build(t)
	if err := reconcileFailed(p); err != nil {
		t.Fatal(err)
	}
	if ev := events(p); len(ev) != 0 {
		t.Errorf("events = %q for a request past the age bound, want none", ev)
	}
}

// TestAgeBound: a request whose Verified condition moved at most MaxAge ago gets its revert; one older, or
// without a transition time, gets none and no event.
func TestAgeBound(t *testing.T) {
	for name, c := range map[string]struct {
		at   time.Time
		want bool
	}{
		"just within":           {now.Add(-MaxAge), true},
		"older":                 {now.Add(-MaxAge - time.Second), false},
		"no time":               {time.Time{}, false},
		"ahead within the skew": {now.Add(MaxSkew), true},
		"ahead beyond the skew": {now.Add(MaxSkew + time.Second), false},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld()
			w.failed.Status.Conditions[0].LastTransitionTime = metav1.NewTime(c.at)
			p, k := w.build(t)
			if err := reconcileFailed(p); err != nil {
				t.Fatal(err)
			}
			if got := revertOf(t, k) != nil; got != c.want {
				t.Errorf("revert created = %v, want %v", got, c.want)
			}
			if ev := events(p); len(ev) != 0 {
				t.Errorf("events = %q, want none", ev)
			}
		})
	}
}

// TestTargetForbidden: a target read refused as Forbidden (an authorization webhook that fails answers 403 too)
// is an error for a retry, for a new decision and for a Pending entry, which stays Pending. Once the age bound
// passes, the new decision records nothing and the Pending entry moves to Abandoned (OutsideAgeBound).
func TestTargetForbidden(t *testing.T) {
	for _, entry := range []bool{false, true} {
		w := newWorld()
		if entry {
			w.inc.Status.RevertsProposed = []remediationv1alpha1.ProposedRevert{pending0}
		}
		forbidden(w)
		p, k := w.build(t)
		if err := reconcileFailed(p); err == nil || !apierrors.IsForbidden(err) {
			t.Errorf("entry %v: Reconcile = %v, want the Forbidden error for a retry", entry, err)
		}
		if revertOf(t, k) != nil {
			t.Errorf("entry %v: a revert was created", entry)
		}
		want := []remediationv1alpha1.ProposedRevert(nil)
		if entry {
			want = []remediationv1alpha1.ProposedRevert{pending0}
		}
		if got := recorded(t, k); !equality.Semantic.DeepEqual(got, want) {
			t.Errorf("entry %v: status.revertsProposed = %+v, want %+v", entry, got, want)
		}
		p.Now = func() time.Time { return now.Add(MaxAge) }
		if err := reconcileFailed(p); err != nil {
			t.Fatalf("entry %v: Reconcile past the age bound: %v", entry, err)
		}
		if revertOf(t, k) != nil {
			t.Errorf("entry %v: a revert was created past the age bound", entry)
		}
		if entry && !onlyAbandoned(t, k, remediationv1alpha1.RevertReasonOutsideAgeBound) {
			t.Errorf("status.revertsProposed = %+v, want the failed request as Abandoned", recorded(t, k))
		}
		if !entry && len(recorded(t, k)) != 0 {
			t.Errorf("status.revertsProposed = %+v, want nothing", recorded(t, k))
		}
	}
}

// TestExistingRevertWins: a Pending entry whose revert exists, linked to the failed request (the create
// succeeded, the move to Created was not written), moves to Created whatever the checks say now, with no
// Warning event: the checks guard only a create that has not happened.
func TestExistingRevertWins(t *testing.T) {
	for name, edit := range map[string]func(w *world){
		"the checks pass":      func(*world) {},
		caseTargetGone:         func(w *world) { w.target = nil },
		"the target forbidden": forbidden,
		caseIncidentDeleting:   incidentDeleting,
		casePastAgeBound: func(w *world) {
			w.failed.Status.Conditions[0].LastTransitionTime = metav1.NewTime(now.Add(-2 * MaxAge))
		},
		"the request being deleted": func(w *world) {
			w.failed.DeletionTimestamp = &metav1.Time{Time: now}
			w.failed.Finalizers = []string{hold}
		},
		"the request gone": func(w *world) { w.failed = nil },
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld()
			w.inc.Status.RevertsProposed = []remediationv1alpha1.ProposedRevert{pending0}
			spec, _ := Spec(w.failed)
			w.extra = append(w.extra, &remediationv1alpha1.RemediationRequest{
				ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: revertKey.Name}, Spec: spec})
			edit(w)
			p, k := w.build(t)
			if err := reconcileFailed(p); err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			if !onlyCreated(t, k) {
				t.Errorf("status.revertsProposed = %+v, want the failed request as Created", recorded(t, k))
			}
			if ev := events(p); len(ev) != 0 {
				t.Errorf("events = %q, want none", ev)
			}
		})
	}
}

// TestDeletedRightBeforeCreate: the Incident put in deletion after the Pending write is seen on the last read
// before the create: no revert, the entry moves to Abandoned (IncidentDeleting).
func TestDeletedRightBeforeCreate(t *testing.T) {
	w := newWorld()
	w.inc.Finalizers = []string{hold}
	w.funcs.SubResourceUpdate = func(ctx context.Context, c client.Client, sub string, obj client.Object,
		opts ...client.SubResourceUpdateOption) error {
		if err := c.SubResource(sub).Update(ctx, obj, opts...); err != nil {
			return err
		}
		if inc, ok := obj.(*remediationv1alpha1.Incident); ok && pending(inc, link0) {
			return c.Delete(ctx, &remediationv1alpha1.Incident{ObjectMeta: metav1.ObjectMeta{Namespace: ns,
				Name: incName}})
		}
		return nil
	}
	p, k := w.build(t)
	if err := reconcileFailed(p); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if revertOf(t, k) != nil {
		t.Error("a revert was created for an Incident being deleted")
	}
	if !onlyAbandoned(t, k, remediationv1alpha1.RevertReasonIncidentDeleting) {
		t.Errorf("status.revertsProposed = %+v, want the failed request as Abandoned", recorded(t, k))
	}
}

// TestUnknownTargetKindNotRead: a target of a kind the action cannot target is never read.
func TestUnknownTargetKindNotRead(t *testing.T) {
	w := newWorld()
	w.failed.Spec.TargetRef.APIVersion, w.failed.Spec.TargetRef.Kind = "v1", "Secret"
	w.funcs.Get = func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object,
		opts ...client.GetOption) error {
		if _, ok := obj.(*unstructured.Unstructured); ok {
			t.Errorf("the target %s was read", obj.GetObjectKind().GroupVersionKind().Kind)
		}
		return c.Get(ctx, key, obj, opts...)
	}
	p, _ := w.build(t)
	if err := reconcileFailed(p); err != nil {
		t.Fatal(err)
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
