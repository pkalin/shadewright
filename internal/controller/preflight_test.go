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
	"strings"
	"sync/atomic"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/gitops/sourcemap"
	"github.com/pkalin/shadewright/internal/policy"
	"github.com/pkalin/shadewright/internal/providers"
	fx "github.com/pkalin/shadewright/internal/testfixtures"
)

// remoteCalls counts what the source mapping costs: mappings started, Git providers opened (each one signs
// a JWT and requests an installation token on its first call) and Git operations.
type remoteCalls struct {
	resolves, opens, gitOps atomic.Int32
}

// countingReconciler is w's reconciler with a source mapping that opens the repository through a counting
// opener and reads the branch head from w's fake Git, as the real resolver does, resolving to path. Every
// call is counted in calls.
func countingReconciler(w *kedaWorld, calls *remoteCalls, path string) *RemediationRequestReconciler {
	w.git.SetFail(func(string) error {
		calls.gitOps.Add(1)
		return nil
	})
	r := w.reconciler()
	r.OpenRepository = func(context.Context, remediationv1alpha1.GitRepository) (providers.GitProvider, error) {
		calls.opens.Add(1)
		return w.git, nil
	}
	r.Source = sourceFunc(func(ctx context.Context, _ sourcemap.Input) (sourcemap.Result, error) {
		calls.resolves.Add(1)
		git, err := r.OpenRepository(ctx, remediationv1alpha1.GitRepository{Name: fx.RepositoryName})
		if err != nil {
			return sourcemap.Result{}, err
		}
		head, err := git.BranchHead(ctx, mainBranch)
		if err != nil {
			return sourcemap.Result{}, err
		}
		return sourcemap.Result{Outcome: sourcemap.Resolved, Reason: sourcemap.ReasonResolved,
			Source: &remediationv1alpha1.ResolvedSource{RepositoryRef: fx.RepositoryName, TargetBranch: mainBranch,
				Path: remediationv1alpha1.GitFilePath(path), BuildPath: scenarioBuildPath, BaseCommit: head}}, nil
	})
	return r
}

// A request the policy refuses on its own fields is decided before the source mapping (#242): no mapping, no
// Git provider opened (so no signing and no token) and no Git operation, with the reason and condition Decide
// gives, and the policy it was decided under recorded as before.
func TestPreflightRefusesBeforeReadingGit(t *testing.T) {
	cases := []struct {
		name   string
		change func(t *testing.T, w *kedaWorld)
		reason policy.Reason
	}{
		{"request namespace not allowed", func(t *testing.T, w *kedaWorld) {
			pol := &remediationv1alpha1.RemediationPolicy{}
			if err := w.c.Get(context.Background(), client.ObjectKey{Name: string(w.rr.Spec.PolicyRef)}, pol); err != nil {
				t.Fatal(err)
			}
			pol.Spec.RequestNamespaces = []remediationv1alpha1.Name{"not-the-operator"}
			if err := w.c.Update(context.Background(), pol); err != nil {
				t.Fatal(err)
			}
		}, policy.ReasonRequestNamespaceNotAllowed},
		{"module disabled", func(t *testing.T, w *kedaWorld) { w.setModules(t) }, policy.ReasonModuleNotEnabled},
		{"parameters out of bounds", func(t *testing.T, w *kedaWorld) {
			rr := w.fetch(t)
			rr.Spec.Action.AdjustKedaMaxReplicas.DesiredValue = 50 // the policy's max is 10
			if err := w.c.Update(context.Background(), rr); err != nil {
				t.Fatal(err)
			}
		}, policy.ReasonAboveMaximum},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := newKedaWorld(t, readyConditions())
			c.change(t, w)
			var calls remoteCalls
			if _, err := countingReconciler(w, &calls, scaledObjectPath).Reconcile(context.Background(), w.request); err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			if n, o, g := calls.resolves.Load(), calls.opens.Load(), calls.gitOps.Load(); n+o+g != 0 {
				t.Fatalf("a refused request cost %d mappings, %d Git providers opened and %d Git operations, want none",
					n, o, g)
			}
			got := w.fetch(t)
			if got.Status.Phase != remediationv1alpha1.PhaseDenied {
				t.Fatalf("phase = %q, want Denied", got.Status.Phase)
			}
			cond := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPolicyAllowed)
			if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != string(c.reason) || cond.Message == "" {
				t.Fatalf("PolicyAllowed = %+v, want False/%s with a message", cond, c.reason)
			}
			// The fake client sets no UID, so the revision names the policy.
			if !strings.HasPrefix(got.Status.PolicyRevision, string(w.rr.Spec.PolicyRef)+"/") {
				t.Fatalf("policyRevision %q: want the policy the refusal was made under", got.Status.PolicyRevision)
			}
			if got.Status.Source != nil || got.Status.PlanDigest != "" || got.Status.Deadline != nil ||
				got.Status.EffectiveApprovalRequired != nil {
				t.Fatalf("a refused request got a source, plan, deadline or approval: %+v", got.Status)
			}
		})
	}
}

// A request Preflight lets through is still mapped and decided by Decide in full: allowed with a source that
// the policy accepts, and Denied after the mapping with one it does not (#242).
func TestPreflightLetsThroughToMappingAndDecide(t *testing.T) {
	cases := []struct {
		name   string
		path   string
		phase  remediationv1alpha1.RemediationPhase
		reason string
	}{
		{"allowed", scaledObjectPath, remediationv1alpha1.PhasePolicyChecked, string(policy.ReasonWithinApprovedBounds)},
		{"source refused", ".github/workflows/deploy.yaml", remediationv1alpha1.PhaseDenied,
			string(policy.ReasonPathNotAllowed)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := newKedaWorld(t, readyConditions())
			var calls remoteCalls
			if _, err := countingReconciler(w, &calls, c.path).Reconcile(context.Background(), w.request); err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			if calls.resolves.Load() != 1 || calls.opens.Load() == 0 || calls.gitOps.Load() == 0 {
				t.Fatalf("mappings %d, opens %d, Git operations %d: want the source mapped once",
					calls.resolves.Load(), calls.opens.Load(), calls.gitOps.Load())
			}
			got := w.fetch(t)
			cond := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPolicyAllowed)
			if got.Status.Phase != c.phase || cond == nil || cond.Reason != c.reason {
				t.Fatalf("phase %q, PolicyAllowed %+v; want %s/%s", got.Status.Phase, cond, c.phase, c.reason)
			}
			if got.Status.Source == nil || string(got.Status.Source.Path) != c.path {
				t.Fatalf("source = %+v, want the resolved %q", got.Status.Source, c.path)
			}
		})
	}
}

// Each decision maps to an outcome whose phase and condition status match it, and an Allowed answer from the
// preflight, which never allows, fails closed: no phase PolicyChecked without a plan.
func TestDecidedOutcomes(t *testing.T) {
	pol := &remediationv1alpha1.RemediationPolicy{ObjectMeta: metav1.ObjectMeta{Name: "p", Generation: 2}}
	allowed := policy.Decision{Outcome: policy.OutcomeAllowed, Reason: policy.ReasonWithinApprovedBounds}
	if o := decided(allowed, pol); o.phase != remediationv1alpha1.PhasePolicyChecked ||
		o.status != metav1.ConditionTrue || o.approval == nil {
		t.Fatalf("decided(Allowed) = %+v, want PolicyChecked/True with the approval", o)
	}
	if o := preflightRefusal(allowed, pol); o.phase != remediationv1alpha1.PhaseNeedsIntervention ||
		o.status != metav1.ConditionUnknown || o.planDigest != "" {
		t.Fatalf("preflightRefusal(Allowed) = %+v, want NeedsIntervention", o)
	}
	for out, want := range map[policy.Outcome]remediationv1alpha1.RemediationPhase{
		policy.OutcomeDenied:            remediationv1alpha1.PhaseDenied,
		policy.OutcomeNeedsIntervention: remediationv1alpha1.PhaseNeedsIntervention,
		"Unknown":                       remediationv1alpha1.PhaseNeedsIntervention,
	} {
		if o := decided(policy.Decision{Outcome: out}, pol); o.phase != want || o.policyRevision != "p/2" {
			t.Fatalf("decided(%s) = %+v, want phase %s under p/2", out, o, want)
		}
	}
}

// updateTarget changes w's ScaledObject on the API server.
func (w *kedaWorld) updateTarget(t *testing.T, change func(so *unstructured.Unstructured)) {
	t.Helper()
	so := &unstructured.Unstructured{}
	so.SetGroupVersionKind(w.so.GroupVersionKind())
	if err := w.c.Get(context.Background(), client.ObjectKeyFromObject(w.so), so); err != nil {
		t.Fatal(err)
	}
	change(so)
	if err := w.c.Update(context.Background(), so); err != nil {
		t.Fatal(err)
	}
}

// requireLabel makes w's policy cover only targets labelled team=payments.
func (w *kedaWorld) requireLabel(t *testing.T) {
	t.Helper()
	pol := &remediationv1alpha1.RemediationPolicy{}
	if err := w.c.Get(context.Background(), client.ObjectKey{Name: string(w.rr.Spec.PolicyRef)}, pol); err != nil {
		t.Fatal(err)
	}
	pol.Spec.Targets.ObjectSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"team": "payments"}}
	if err := w.c.Update(context.Background(), pol); err != nil {
		t.Fatal(err)
	}
}

// afterFirstTargetRead is a reader that runs then once, right after it has served the first read of the target
// (a ScaledObject): a change Flux applies between the policy's read of the target and anything after it.
type afterFirstTargetRead struct {
	client.Reader
	then func()
	done bool
}

func (a *afterFirstTargetRead) Get(ctx context.Context, key client.ObjectKey, obj client.Object,
	opts ...client.GetOption) error {
	err := a.Reader.Get(ctx, key, obj, opts...)
	if err == nil && !a.done && obj.GetObjectKind().GroupVersionKind().Kind == kindScaledObject {
		a.done = true
		a.then()
	}
	return err
}

// A preflight refusal that rests on the live target (its labels) ends the request for good, so it is given only
// on the object as it is: when the target changed after it was read (Flux added the label the policy wants), the
// check starts over instead, and then lets the request through to the mapping.
func TestPreflightRereadsTargetBeforeRefusingOnIt(t *testing.T) {
	w := newKedaWorld(t, readyConditions())
	w.requireLabel(t)
	var calls remoteCalls
	r := countingReconciler(w, &calls, scaledObjectPath)
	r.APIReader = &afterFirstTargetRead{Reader: r.APIReader, then: func() {
		w.updateTarget(t, func(so *unstructured.Unstructured) { so.SetLabels(map[string]string{"team": "payments"}) })
	}}
	_, err := r.Reconcile(context.Background(), w.request)
	if err == nil || !strings.Contains(err.Error(), "changed during the policy check") {
		t.Fatalf("Reconcile = %v, want the check started over", err)
	}
	if got := w.fetch(t); got.Status.Phase != "" {
		t.Fatalf("phase = %q, want none: a refusal on a target that changed is not decided", got.Status.Phase)
	}
	if calls.resolves.Load() != 0 {
		t.Fatalf("mapped %d times before the check started over, want none", calls.resolves.Load())
	}

	// The next attempt reads the labelled target, which the policy covers.
	if _, err := r.Reconcile(context.Background(), w.request); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := w.fetch(t); got.Status.Phase != remediationv1alpha1.PhasePolicyChecked {
		t.Fatalf("phase = %q, want PolicyChecked", got.Status.Phase)
	}
}

// A preflight refusal on the target as it still is ends the request at once, before any mapping.
func TestPreflightRefusalOnUnchangedTarget(t *testing.T) {
	w := newKedaWorld(t, readyConditions())
	w.requireLabel(t)
	var calls remoteCalls
	if _, err := countingReconciler(w, &calls, scaledObjectPath).Reconcile(context.Background(), w.request); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	got := w.fetch(t)
	cond := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPolicyAllowed)
	if got.Status.Phase != remediationv1alpha1.PhaseDenied || cond == nil ||
		cond.Reason != string(policy.ReasonTargetLabelsNotMatched) || calls.resolves.Load() != 0 {
		t.Fatalf("phase %q, PolicyAllowed %+v, %d mappings; want Denied/TargetLabelsNotMatched with no mapping",
			got.Status.Phase, cond, calls.resolves.Load())
	}
}

// A refusal on the request's own fields wins over whatever the source mapping would say, a stop for a person or
// an error worth a retry included (#242: the request's fields are checked first). The mapping is not asked, and
// no source is written, since no Git read happened.
func TestPreflightRefusalWinsOverMapping(t *testing.T) {
	cases := map[string]func() (sourcemap.Result, error){
		"mapping needs a person": func() (sourcemap.Result, error) {
			return sourcemap.Result{Outcome: sourcemap.NeedsIntervention, Reason: sourcemap.ReasonHelmManaged,
				Message: "installed by a HelmRelease"}, nil
		},
		"mapping fails, worth a retry": func() (sourcemap.Result, error) {
			return sourcemap.Result{}, errors.New("the Git host did not answer")
		},
	}
	for name, mapping := range cases {
		t.Run(name, func(t *testing.T) {
			w := newKedaWorld(t, readyConditions())
			w.setModules(t)
			var resolves int
			r := w.reconciler()
			r.Source = sourceFunc(func(context.Context, sourcemap.Input) (sourcemap.Result, error) {
				resolves++
				return mapping()
			})
			if _, err := r.Reconcile(context.Background(), w.request); err != nil {
				t.Fatalf("Reconcile: %v, want the policy's refusal", err)
			}
			got := w.fetch(t)
			cond := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPolicyAllowed)
			if got.Status.Phase != remediationv1alpha1.PhaseDenied || cond == nil ||
				cond.Reason != string(policy.ReasonModuleNotEnabled) || resolves != 0 {
				t.Fatalf("phase %q, PolicyAllowed %+v, %d mappings; want Denied/ModuleNotEnabled, no mapping",
					got.Status.Phase, cond, resolves)
			}
			if got.Status.Source != nil {
				t.Fatalf("source = %+v, want none: no Git read happened", got.Status.Source)
			}
		})
	}
}
