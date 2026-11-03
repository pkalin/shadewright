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

//nolint:goconst // the tables spell their values out to read as recorded objects
package modules

import (
	"slices"
	"testing"
)

// reasons returns the reasons of the findings, and fails on a finding of another rule.
func reasons(t *testing.T, rule string, fs []Finding) []string {
	t.Helper()
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		if f.Rule != rule {
			t.Errorf("finding of rule %q, want only %q", f.Rule, rule)
		}
		out = append(out, f.Reason)
	}
	return out
}

func evidence(f Finding, field string) string {
	for _, e := range f.Evidence {
		if e.Field == field {
			return e.Value
		}
	}
	return ""
}

func TestNodePressure(t *testing.T) {
	rules := genericRules()
	cases := []struct {
		name string
		cond []NodeCondition
		want []string
	}{
		{"healthy", []NodeCondition{
			{Type: "MemoryPressure", Status: "False"}, {Type: "DiskPressure", Status: "False"},
			{Type: "PIDPressure", Status: "False"}, {Type: "Ready", Status: "True"},
		}, nil},
		{"memory", []NodeCondition{{Type: "MemoryPressure", Status: "True", Reason: "KubeletHasInsufficientMemory",
			Message: "kubelet has insufficient memory available"}}, []string{ReasonNodeMemoryPressure}},
		{"disk and pid", []NodeCondition{{Type: "PIDPressure", Status: "True"}, {Type: "DiskPressure", Status: "True"}},
			[]string{ReasonNodeDiskPressure, ReasonNodePIDPressure}},
		{"unknown status", []NodeCondition{{Type: "MemoryPressure", Status: "Unknown"}}, nil},
		{"other condition", []NodeCondition{{Type: "Ready", Status: "True"}}, nil},
	}
	for _, c := range cases {
		got := reasons(t, RuleNodePressure, evaluateNode(GenericPlatform, rules, NodeState{Name: "n1", Conditions: c.cond}))
		if !slices.Equal(got, c.want) {
			t.Errorf("%s: reasons = %v, want %v", c.name, got, c.want)
		}
	}
	f := evaluateNode(GenericPlatform, rules, NodeState{Name: "n1", Conditions: []NodeCondition{
		{Type: "MemoryPressure", Status: "True", Reason: "KubeletHasInsufficientMemory"}}})
	if len(f) != 1 || f[0].Subject.Kind != "Node" || f[0].Subject.Name != "n1" ||
		evidence(f[0], "node.condition.MemoryPressure.reason") != "KubeletHasInsufficientMemory" {
		t.Errorf("finding = %+v", f)
	}
}

func TestInsufficientCapacity(t *testing.T) {
	pod := ObjectRef{APIVersion: "v1", Kind: "Pod", Namespace: "demo", Name: "web-1"}
	cases := []struct {
		name      string
		ev        Event
		resources string
		nodes     string
	}{
		{"cpu and memory", Event{Reason: "FailedScheduling", Regarding: pod, Note: "0/3 nodes are available: " +
			"2 Insufficient cpu, 3 Insufficient memory. preemption: 0/3 nodes are available: 3 No preemption victims found."},
			"cpu, memory", "0/3"},
		{"too many pods", Event{Reason: "FailedScheduling", Regarding: pod, Note: "0/1 nodes are available: 1 Too many pods."},
			"pods", "0/1"},
		{"extended resource", Event{Reason: "FailedScheduling", Regarding: pod,
			Note: "0/2 nodes are available: 2 Insufficient nvidia.com/gpu."}, "nvidia.com/gpu", "0/2"},
		{"huge pages, upper case", Event{Reason: "FailedScheduling", Regarding: pod,
			Note: "0/2 nodes are available: 2 Insufficient hugepages-2Mi."}, "hugepages-2Mi", "0/2"},
		{"prefixed resource with underscore and several", Event{Reason: "FailedScheduling", Regarding: pod,
			Note: "0/4 nodes are available: 1 Insufficient example.com/foo_bar, 3 Insufficient cpu."},
			"cpu, example.com/foo_bar", "0/4"},
		{"taints only", Event{Reason: "FailedScheduling", Regarding: pod,
			Note: "0/2 nodes are available: 2 node(s) had untolerated taint {dedicated: infra}."}, "", ""},
		{"other reason", Event{Reason: "BackOff", Regarding: pod, Note: "1 Insufficient cpu"}, "", ""},
		{"not a pod", Event{Reason: "FailedScheduling", Regarding: ObjectRef{Kind: "Node"}, Note: "1 Insufficient cpu"}, "", ""},
	}
	for _, c := range cases {
		fs := evaluateEvent(GenericPlatform, genericRules(), c.ev)
		got := reasons(t, RuleInsufficientCapacity, fs)
		if c.resources == "" {
			if len(got) != 0 {
				t.Errorf("%s: reasons = %v, want none", c.name, got)
			}
			continue
		}
		if !slices.Equal(got, []string{ReasonInsufficientNodeCapacity}) {
			t.Errorf("%s: reasons = %v", c.name, got)
			continue
		}
		if r := evidence(fs[0], "scheduler.insufficient"); r != c.resources {
			t.Errorf("%s: insufficient = %q, want %q", c.name, r, c.resources)
		}
		if n := evidence(fs[0], "scheduler.nodesAvailable"); n != c.nodes {
			t.Errorf("%s: nodes = %q, want %q", c.name, n, c.nodes)
		}
		if fs[0].Subject != pod {
			t.Errorf("%s: subject = %+v", c.name, fs[0].Subject)
		}
	}
}
