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
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/gitops/sourcemap"
)

// The envtest API server has no KEDA CRDs, so the KEDA baseline is checked here on an unstructured object.
func TestCheckBaselineKeda(t *testing.T) {
	ref := remediationv1alpha1.ObjectRef{APIVersion: "keda.sh/v1alpha1", Kind: "ScaledObject", Namespace: "orders",
		Name: "worker", UID: "u"}
	live := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "keda.sh/v1alpha1", "kind": "ScaledObject",
		"spec": map[string]any{"maxReplicaCount": int64(10)},
	}}
	for _, tc := range []struct {
		name     string
		previous int32
		live     *unstructured.Unstructured
		stop     bool
		reason   string
	}{
		{"matches", 10, live, false, ""},
		{"forged baseline", 40, live, true, ReasonPreviousValueMismatch},
		{"field missing", 10, &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{}}}, true,
			string(sourcemap.ReasonFieldNotFound)},
	} {
		action := remediationv1alpha1.Action{
			Type:                  remediationv1alpha1.ActionAdjustKedaMaxReplicas,
			AdjustKedaMaxReplicas: &remediationv1alpha1.AdjustKedaMaxReplicas{PreviousValue: tc.previous, DesiredValue: 45},
		}
		field, err := sourcemap.FieldFor(action, ref)
		if err != nil {
			t.Fatal(err)
		}
		out, stop := checkBaseline(action, field, tc.live)
		if stop != tc.stop || out.reason != tc.reason {
			t.Errorf("%s: stop %v reason %q, want %v %q", tc.name, stop, out.reason, tc.stop, tc.reason)
		}
	}
}
