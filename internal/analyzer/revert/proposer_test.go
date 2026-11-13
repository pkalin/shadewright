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
	"k8s.io/apimachinery/pkg/types"
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
)

var errBoom = errors.New("boom")

// now is the proposer's clock in every case; the failed request's Verified condition moved an hour before.
var now = time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)

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
		"a full revertsProposed": fill,
		"a target of a kind the action cannot target": func(w *world) {
			w.failed.Spec.TargetRef.APIVersion, w.failed.Spec.TargetRef.Kind = "v1", "ConfigMap"
		},
		"failed more than a day ago": func(w *world) {
			w.failed.Status.Conditions[0].LastTransitionTime = metav1.NewTime(now.Add(-MaxAge - time.Second))
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
	if !onlyCreated(t, k) {
		t.Errorf("status.revertsProposed = %+v, want the failed request as Created", recorded(t, k))
	}
}

// TestAlreadyExistsWithAnotherLink: a request at the derived name that reverts something else, or nothing, is
// a terminal error (no retry with backoff: retrying does not move it), with a Warning event on the failed
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
			err := reconcileFailed(p)
			if err == nil {
				t.Fatal("an existing request with another link was taken as the revert")
			}
			if !errors.Is(err, reconcile.TerminalError(nil)) {
				t.Errorf("error %v is retried with backoff, want a terminal error", err)
			}
			if ev := events(p); len(ev) != 1 || !strings.Contains(ev[0], "Warning "+EventNameTaken) {
				t.Errorf("events = %q, want one Warning %s", ev, EventNameTaken)
			}
			got := revertOf(t, k)
			if got == nil || !equality.Semantic.DeepEqual(got.Spec.Reverts, reverts) {
				t.Errorf("the existing request was changed: %+v", got)
			}
			for _, r := range recorded(t, k) {
				if r.State == remediationv1alpha1.RevertCreated {
					t.Error("the failed request was recorded as Created")
				}
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

// TestPendingKeepsTheDecision: a Pending entry leads to the create and the move to Created, also when the
// request has since passed the age bound, its target is gone or the list is full: the decision was taken.
func TestPendingKeepsTheDecision(t *testing.T) {
	w := newWorld()
	w.failed.Status.Conditions[0].LastTransitionTime = metav1.NewTime(now.Add(-2 * MaxAge))
	w.target = nil
	fill(w)
	w.inc.Status.RevertsProposed[len(w.inc.Status.RevertsProposed)-1] = pending0
	p, k := w.build(t)
	if err := reconcileFailed(p); err != nil {
		t.Fatal(err)
	}
	if revertOf(t, k) == nil {
		t.Fatal("a Pending decision did not lead to the create")
	}
	got := recorded(t, k)
	if got[len(got)-1] != created0 {
		t.Errorf("the entry is %+v, want Created", got[len(got)-1])
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
		"just within": {now.Add(-MaxAge), true},
		"older":       {now.Add(-MaxAge - time.Second), false},
		"no time":     {time.Time{}, false},
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

// TestTargetForbidden: a target the analyzer may not read gives no revert, and no error retried for ever.
func TestTargetForbidden(t *testing.T) {
	w := newWorld()
	w.funcs.Get = func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object,
		opts ...client.GetOption) error {
		if objKind(c, obj) == "Deployment" {
			return apierrors.NewForbidden(appsv1.Resource("deployments"), key.Name, errBoom)
		}
		return c.Get(ctx, key, obj, opts...)
	}
	p, k := w.build(t)
	if err := reconcileFailed(p); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if revertOf(t, k) != nil || len(recorded(t, k)) != 0 {
		t.Error("a revert was proposed for a target the analyzer may not read")
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
