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
package semdiff

import (
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/pkalin/shadewright/api/v1alpha1"
)

// validate runs the basic schema checks on the changed target. A Deployment or StatefulSet must convert to
// its typed apps/v1 form with no unknown fields, the changed container's memory limit must be positive and
// not below its memory request. A ScaledObject (whose types the module does not import) must name its scale
// target, and maxReplicaCount must be a non-negative integer not below minReplicaCount.
func validate(obj map[string]any, target v1alpha1.ObjectRef, action v1alpha1.Action) error {
	switch target.Kind {
	case "Deployment":
		var d appsv1.Deployment
		if err := strictConvert(obj, &d); err != nil {
			return err
		}
		return checkMemory(d.Spec.Template.Spec.Containers, action)
	case "StatefulSet":
		var s appsv1.StatefulSet
		if err := strictConvert(obj, &s); err != nil {
			return err
		}
		return checkMemory(s.Spec.Template.Spec.Containers, action)
	case "ScaledObject":
		return checkScaledObject(obj)
	}
	return refuse(ReasonSchemaInvalid, "the target kind has no schema check")
}

func strictConvert(obj map[string]any, into runtime.Object) error {
	if runtime.DefaultUnstructuredConverter.FromUnstructuredWithValidation(obj, into, true) != nil {
		return refuse(ReasonSchemaInvalid, "the changed object does not match its schema")
	}
	return nil
}

func checkMemory(containers []corev1.Container, action v1alpha1.Action) error {
	if action.SetContainerMemoryLimit == nil {
		return refuse(ReasonSchemaInvalid, "the action does not change a memory limit")
	}
	name := action.SetContainerMemoryLimit.Container
	var found *corev1.Container
	for i := range containers {
		if containers[i].Name == name {
			if found != nil {
				return refuse(ReasonSchemaInvalid, "several containers are named "+name)
			}
			found = &containers[i]
		}
	}
	if found == nil {
		return refuse(ReasonSchemaInvalid, "no container named "+name)
	}
	limit, ok := found.Resources.Limits[corev1.ResourceMemory]
	if !ok || limit.Sign() <= 0 {
		return refuse(ReasonSchemaInvalid, "the memory limit must be positive")
	}
	if req, ok := found.Resources.Requests[corev1.ResourceMemory]; ok && req.Cmp(limit) > 0 {
		return refuse(ReasonSchemaInvalid, "the memory limit is below the memory request")
	}
	return nil
}

func checkScaledObject(obj map[string]any) error {
	spec, ok := obj["spec"].(map[string]any)
	if !ok {
		return refuse(ReasonSchemaInvalid, "the ScaledObject has no spec")
	}
	ref, _ := spec["scaleTargetRef"].(map[string]any)
	if name, _ := ref["name"].(string); name == "" {
		return refuse(ReasonSchemaInvalid, "the ScaledObject names no scale target")
	}
	maxReplicas, ok := spec["maxReplicaCount"].(int64)
	if !ok || maxReplicas < 0 {
		return refuse(ReasonSchemaInvalid, "maxReplicaCount must be a non-negative integer")
	}
	if raw, present := spec["minReplicaCount"]; present {
		minReplicas, ok := raw.(int64)
		if !ok || minReplicas < 0 || minReplicas > maxReplicas {
			return refuse(ReasonSchemaInvalid, "minReplicaCount must be a non-negative integer not above maxReplicaCount")
		}
	}
	return nil
}
