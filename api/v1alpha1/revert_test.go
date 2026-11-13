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

package v1alpha1

import (
	"testing"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"
)

// noSuchAction is an action type this version does not know.
const noSuchAction ActionType = "NoSuchAction"

func keda(prev, desired int32) Action {
	return Action{Type: ActionAdjustKedaMaxReplicas,
		AdjustKedaMaxReplicas: &AdjustKedaMaxReplicas{PreviousValue: prev, DesiredValue: desired}}
}

func memory(container, prev, desired string) Action {
	return Action{Type: ActionSetContainerMemoryLimit, SetContainerMemoryLimit: &SetContainerMemoryLimit{
		Container: container, Previous: resource.MustParse(prev), Desired: resource.MustParse(desired)}}
}

func TestInverseAction(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   Action
		want *Action
	}{
		{"keda", keda(10, 15), new(keda(15, 10))},
		{"memory", memory("app", "256Mi", "384Mi"), new(memory("app", "384Mi", "256Mi"))},
		{"unknown type", Action{Type: noSuchAction}, nil},
		{"keda member missing", Action{Type: ActionAdjustKedaMaxReplicas}, nil},
		{"memory member missing", Action{Type: ActionSetContainerMemoryLimit}, nil},
		{"keda equal values", keda(10, 10), nil},
		{"keda zero", keda(0, 10), nil},
		{"memory equal by value", memory("app", "1Gi", "1024Mi"), nil},
		{"memory zero", memory("app", "0", "1Gi"), nil},
		{"memory no container", memory("", "256Mi", "384Mi"), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := InverseAction(tc.in)
			if ok != (tc.want != nil) {
				t.Fatalf("InverseAction ok = %v, want %v", ok, tc.want != nil)
			}
			if tc.want != nil && !equality.Semantic.DeepEqual(got, *tc.want) {
				t.Errorf("InverseAction = %+v, want %+v", got, *tc.want)
			}
		})
	}
}

func TestIsInverseAction(t *testing.T) {
	if !IsInverseAction(keda(10, 15), keda(15, 10)) {
		t.Error("15 to 10 is the inverse of 10 to 15")
	}
	if IsInverseAction(keda(10, 15), keda(15, 11)) {
		t.Error("15 to 11 is not the inverse of 10 to 15")
	}
	if !IsInverseAction(memory("app", "256Mi", "384Mi"), memory("app", "402653184", "268435456")) {
		t.Error("quantities compare by value")
	}
	if IsInverseAction(memory("app", "256Mi", "384Mi"), memory("sidecar", "384Mi", "256Mi")) {
		t.Error("another container is not the inverse")
	}
	both := memory("app", "384Mi", "256Mi")
	both.AdjustKedaMaxReplicas = &AdjustKedaMaxReplicas{PreviousValue: 1, DesiredValue: 2}
	if IsInverseAction(memory("app", "256Mi", "384Mi"), both) {
		t.Error("an action with another member set is not the inverse")
	}
	if IsInverseAction(Action{Type: noSuchAction}, Action{Type: noSuchAction}) {
		t.Error("an unknown action type is never an inverse")
	}
}
