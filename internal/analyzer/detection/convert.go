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

package detection

import (
	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"

	"github.com/pkalin/shadewright/internal/modules"
)

// EventOf returns what a diagnosis rule reads from an event. The fields are copied as they are: rules treat
// them as untrusted, and the evaluation redacts and bounds what it keeps.
func EventOf(ev *eventsv1.Event) modules.Event {
	return modules.Event{
		Namespace:           ev.Namespace,
		Reason:              ev.Reason,
		Type:                ev.Type,
		ReportingController: ev.ReportingController,
		Note:                ev.Note,
		Regarding: modules.ObjectRef{
			APIVersion: ev.Regarding.APIVersion,
			Kind:       ev.Regarding.Kind,
			Namespace:  ev.Regarding.Namespace,
			Name:       ev.Regarding.Name,
		},
	}
}

// NodeOf returns what a diagnosis rule reads from a node: its name, labels and conditions.
func NodeOf(n *corev1.Node) modules.NodeState {
	out := modules.NodeState{Name: n.Name, Labels: n.Labels}
	for _, c := range n.Status.Conditions {
		out.Conditions = append(out.Conditions, modules.NodeCondition{
			Type: string(c.Type), Status: string(c.Status), Reason: c.Reason, Message: c.Message,
		})
	}
	return out
}
