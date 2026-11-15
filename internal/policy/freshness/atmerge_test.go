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

package freshness_test

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/policy/freshness"
	"github.com/pkalin/shadewright/internal/providers"
)

// mergedWorld is newWorld after the merge: the target file on the target branch and the live target hold the
// desired value, and no Git provider or resolver is available, so a check at the merge that reads Git fails.
func mergedWorld(t *testing.T) *world {
	t.Helper()
	w := newWorld(t)
	if _, err := w.git.Push("main", sourcePath, manifest("384Mi")); err != nil {
		t.Fatal(err)
	}
	w.target.Spec.Template.Spec.Containers[0].Resources.Limits[corev1.ResourceMemory] = resource.MustParse("384Mi")
	w.open, w.resolve = nil, nil
	return w
}

// The check at the merge reads no Git and no live field: a merged plan whose inputs are unchanged is fresh,
// where the full check calls it stale (#737).
func TestCheckAtMergeFreshAfterTheMerge(t *testing.T) {
	w := mergedWorld(t)
	if err := w.atMerge(t); err != nil {
		t.Fatalf("want fresh, got %v", err)
	}
	w.open = func(context.Context, v1alpha1.GitRepository) (providers.GitProvider, error) { return w.git, nil }
	if err := w.fresh(t); err == nil {
		t.Fatal("fixture: the full check passed on a merged plan")
	}
}

// Every input the check at the merge compares, changed after the plan was fixed, makes the plan stale.
func TestCheckAtMergeStalePerInput(t *testing.T) {
	cases := []struct {
		name   string
		change func(w *world)
		reason string
	}{
		{"policy generation", func(w *world) { w.pol.Generation = 4 }, freshness.ReasonPolicyChanged},
		{"policy UID (recreated)", func(w *world) { w.pol.UID = otherPolicyUID },
			freshness.ReasonPolicyChanged},
		{"policy deleted", func(w *world) { w.pol = nil }, freshness.ReasonPolicyChanged},
		{"settings deleted", func(w *world) { w.settings = nil }, freshness.ReasonSettingsChanged},
		{"repository removed from settings", func(w *world) { w.settings.Spec.Repositories = nil },
			freshness.ReasonSettingsChanged},
		{"target branch in settings", func(w *world) { w.settings.Spec.Repositories[0].TargetBranch = otherBranch },
			freshness.ReasonSettingsChanged},
		{"installation in settings", func(w *world) { w.settings.Spec.Repositories[0].GitHub.InstallationID = 2 },
			freshness.ReasonPlanChanged},
		{"recorded digest", func(w *world) {
			w.rr.Status.PlanDigest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
		}, freshness.ReasonPlanChanged},
		{"target deleted", func(w *world) { w.target = nil }, freshness.ReasonTargetChanged},
		{"target recreated with another UID", func(w *world) { w.target.UID = otherTargetUID },
			freshness.ReasonTargetChanged},
		{"target relabelled out of the policy selector", func(w *world) { w.target.Labels = map[string]string{tierLabel: "db"} },
			freshness.ReasonTargetChanged},
		// An input that can never be checked is a definite answer, so the merge is recorded and capped instead of
		// retried for good.
		{"no digest recorded", func(w *world) { w.rr.Status.PlanDigest = "" }, freshness.ReasonPlanChanged},
		{"no source recorded", func(w *world) { w.rr.Status.Source = nil }, freshness.ReasonPlanChanged},
		{"malformed target apiVersion", func(w *world) { w.rr.Spec.TargetRef.APIVersion = "apps/v1/extra" },
			freshness.ReasonTargetChanged},
		// A pause does not hide a stale plan at the merge: the check writes nothing.
		{"policy generation while paused", func(w *world) {
			w.pol.Generation = 4
			w.settings.Spec.Paused = new(true)
		}, freshness.ReasonPolicyChanged},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := mergedWorld(t)
			tc.change(w)
			wantStale(t, w.atMerge(t), tc.reason)
		})
	}
}

// A pause is no verdict at the merge: an unchanged plan is fresh while writes are paused.
func TestCheckAtMergeIgnoresAPause(t *testing.T) {
	w := mergedWorld(t)
	w.settings.Spec.Paused = new(true)
	if err := w.atMerge(t); err != nil {
		t.Fatalf("want fresh while paused, got %v", err)
	}
}

// A read that fails, or a context that ended, is neither fresh nor stale: the caller retries.
func TestCheckAtMergeFailClosed(t *testing.T) {
	failKind := func(match func(client.Object) bool) func(w *world) {
		return func(w *world) {
			w.getErr = func(_ client.ObjectKey, o client.Object) error {
				if match(o) {
					return apierrors.NewServiceUnavailable("down")
				}
				return nil
			}
		}
	}
	isTarget := func(o client.Object) bool { return o.GetObjectKind().GroupVersionKind().Kind == targetKind }
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	cases := map[string]func(w *world){
		"policy read fails": failKind(func(o client.Object) bool { _, ok := o.(*v1alpha1.RemediationPolicy); return ok }),
		"settings read fails": failKind(func(o client.Object) bool {
			_, ok := o.(*v1alpha1.RemediationSettings)
			return ok
		}),
		"target read fails": failKind(isTarget),
		// Discovery or the REST mapper did not answer for the kind: no verdict, only a clean NotFound is one.
		"target kind not mapped": func(w *world) {
			w.getErr = func(_ client.ObjectKey, o client.Object) error {
				if gvk := o.GetObjectKind().GroupVersionKind(); isTarget(o) {
					return &meta.NoKindMatchError{GroupKind: gvk.GroupKind(), SearchedVersions: []string{gvk.Version}}
				}
				return nil
			}
		},
		"context ended": func(w *world) { w.ctx = cancelled },
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			w := mergedWorld(t)
			setup(w)
			err := w.atMerge(t)
			if err == nil {
				t.Fatal("want an error, the plan must not count as fresh")
			}
			if _, ok := errors.AsType[*freshness.StaleError](err); ok {
				t.Fatalf("an unreadable input is not proof of a stale plan, got %v", err)
			}
		})
	}
}

// A target kind the API server does not serve (its CRD deleted) is retried at the merge, bounded by the wait for
// the merge, and stays a stale target in the periodic check, which runs as long as the pull request is open and
// would otherwise retry with no end.
func TestTargetKindNotServed(t *testing.T) {
	noMatch := func(w *world) {
		w.getErr = func(_ client.ObjectKey, o client.Object) error {
			if gvk := o.GetObjectKind().GroupVersionKind(); gvk.Kind == targetKind {
				return &meta.NoKindMatchError{GroupKind: gvk.GroupKind(), SearchedVersions: []string{gvk.Version}}
			}
			return nil
		}
	}
	t.Run("periodic check", func(t *testing.T) {
		w := newWorld(t)
		noMatch(w)
		wantStale(t, w.fresh(t), freshness.ReasonTargetChanged)
	})
	t.Run("check at the merge", func(t *testing.T) {
		w := mergedWorld(t)
		noMatch(w)
		wantRetry(t, w.atMerge(t))
	})
}
