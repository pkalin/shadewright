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

package policy_test

import (
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/policy"
)

const (
	kindDeployment = "Deployment"
	appsV1         = "apps/v1"
)

func TestCheckActionAlone(t *testing.T) {
	ok := v1alpha1.Action{Type: v1alpha1.ActionSetContainerMemoryLimit, SetContainerMemoryLimit: &v1alpha1.SetContainerMemoryLimit{
		Container: "app", Previous: resource.MustParse("256Mi"), Desired: resource.MustParse("512Mi")}}
	if reason, msg := policy.CheckAction(ok); reason != "" {
		t.Errorf("a valid action is refused: %s %s", reason, msg)
	}
	for name, a := range map[string]v1alpha1.Action{
		"unknown type": {Type: "DeletePod"},
		"no member":    {Type: v1alpha1.ActionSetContainerMemoryLimit},
		"both members": {Type: v1alpha1.ActionAdjustKedaMaxReplicas,
			AdjustKedaMaxReplicas:   &v1alpha1.AdjustKedaMaxReplicas{PreviousValue: 1, DesiredValue: 2},
			SetContainerMemoryLimit: ok.SetContainerMemoryLimit},
		"out of range": {Type: v1alpha1.ActionAdjustKedaMaxReplicas,
			AdjustKedaMaxReplicas: &v1alpha1.AdjustKedaMaxReplicas{PreviousValue: 1, DesiredValue: 1001}},
	} {
		if reason, _ := policy.CheckAction(a); reason == "" {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestActionMayTarget(t *testing.T) {
	for _, tc := range []struct {
		typ              v1alpha1.ActionType
		apiVersion, kind string
		want             bool
	}{
		{v1alpha1.ActionSetContainerMemoryLimit, appsV1, kindDeployment, true},
		{v1alpha1.ActionSetContainerMemoryLimit, appsV1, "StatefulSet", true},
		{v1alpha1.ActionSetContainerMemoryLimit, appsV1, "DaemonSet", false},
		{v1alpha1.ActionSetContainerMemoryLimit, "v1", "Pod", false},
		{v1alpha1.ActionAdjustKedaMaxReplicas, "keda.sh/v1alpha1", "ScaledObject", true},
		{v1alpha1.ActionAdjustKedaMaxReplicas, appsV1, kindDeployment, false},
		{"DeletePod", "v1", "Pod", false},
	} {
		if got := policy.ActionMayTarget(tc.typ, tc.apiVersion, tc.kind); got != tc.want {
			t.Errorf("ActionMayTarget(%s, %s, %s) = %v", tc.typ, tc.apiVersion, tc.kind, got)
		}
	}
}
