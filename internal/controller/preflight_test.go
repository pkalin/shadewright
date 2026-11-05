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
	"strings"
	"sync/atomic"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
