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

package sourcemap_test

import (
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/gitops/sourcemap"
)

// WithPrevious reads a decoded value as the previous value of an action in the bounded forms only, and leaves the
// action it was given unchanged.
func TestWithPrevious(t *testing.T) {
	mem := v1alpha1.Action{Type: v1alpha1.ActionSetContainerMemoryLimit, SetContainerMemoryLimit: &v1alpha1.SetContainerMemoryLimit{
		Container: "app", Previous: resource.MustParse("128Mi"), Desired: resource.MustParse("512Mi")}}
	keda := v1alpha1.Action{Type: v1alpha1.ActionAdjustKedaMaxReplicas, AdjustKedaMaxReplicas: &v1alpha1.AdjustKedaMaxReplicas{
		PreviousValue: 3, DesiredValue: 6}}
	for _, c := range []struct {
		name   string
		action v1alpha1.Action
		v      any
		ok     bool
		want   any
	}{
		{"quantity string", mem, mi256, true, mi256},
		{"quantity integer", mem, float64(1 << 20), true, "1048576"},
		{"quantity out of bounds", mem, hugeExponent, false, nil},
		{"quantity of another type", mem, true, false, nil},
		{"replicas", keda, float64(4), true, int64(4)},
		{"replicas not whole", keda, 4.5, false, nil},
		{"replicas negative", keda, float64(-1), false, nil},
		{"replicas past int32", keda, float64(1 << 40), false, nil},
		{"replicas as a string", keda, "4", false, nil},
		{"no known type", v1alpha1.Action{}, mi256, false, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, ok := sourcemap.WithPrevious(c.action, c.v)
			if ok != c.ok {
				t.Fatalf("ok = %v, want %v", ok, c.ok)
			}
			if !ok {
				return
			}
			prev, _ := sourcemap.PreviousValue(got)
			if prev != c.want {
				t.Fatalf("previous = %v, want %v", prev, c.want)
			}
			if orig, _ := sourcemap.PreviousValue(c.action); orig == prev {
				t.Fatal("the action given was changed")
			}
		})
	}
}
