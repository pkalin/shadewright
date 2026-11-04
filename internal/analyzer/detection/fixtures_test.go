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
package detection

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	"sigs.k8s.io/yaml"

	"github.com/pkalin/shadewright/internal/modules"
	"github.com/pkalin/shadewright/internal/modules/aks"
	"github.com/pkalin/shadewright/internal/modules/eks"
	"github.com/pkalin/shadewright/internal/modules/gke"
)

// The EKS and GKE node fixtures, used by the detection and status tests.
const (
	fixtureEKS = "node-eks.yaml"
	fixtureGKE = "node-gke.yaml"
)

func load(t *testing.T, name string, into any) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.UnmarshalStrict(b, into); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
}

func registry(t *testing.T) *modules.Registry {
	t.Helper()
	r, err := modules.New([]modules.Platform{aks.Platform{}, eks.Platform{}, gke.Platform{}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// onAKS is the selection of an AKS cluster with aks enabled, detected from the node fixtures.
func onAKS(t *testing.T) modules.Selection {
	t.Helper()
	r := registry(t)
	nodes := make([]modules.Node, 0, 2)
	for _, f := range []string{"node-aks-spot-preempt.yaml", "node-aks-memory-pressure.yaml"} {
		var n corev1.Node
		load(t, f, &n)
		nodes = append(nodes, nodeOfForDetection(&n))
	}
	s := r.Select(modules.Enabled{Platforms: []string{aks.Name}}, r.Detect(nodes, nil))
	if s.ActivePlatform() != aks.Name {
		t.Fatalf("active platform = %q, want aks", s.ActivePlatform())
	}
	return s
}

type found struct{ module, rule, reason string }

func keys(fs []modules.Finding) []found {
	out := make([]found, 0, len(fs))
	for _, f := range fs {
		out = append(out, found{f.Module, f.Rule, f.Reason})
	}
	return out
}

// TestFixtureEvents runs every rule on the event fixtures through EventOf.
func TestFixtureEvents(t *testing.T) {
	cases := []struct {
		file string
		want []found
	}{
		{"event-pod-ip-exhaustion.yaml", []found{{aks.Name, aks.RulePodIPExhaustion, aks.ReasonPodIPExhaustion}}},
		{"event-quota-exceeded.yaml", []found{{aks.Name, aks.RuleQuotaExceeded, aks.ReasonAzureQuotaExceeded}}},
		{"event-preempt-scheduled.yaml", []found{{aks.Name, aks.RuleSpotEviction, aks.ReasonSpotEviction}}},
		{"event-failed-scheduling.yaml", []found{{modules.GenericPlatform, modules.RuleInsufficientCapacity,
			modules.ReasonInsufficientNodeCapacity}}},
	}
	s := onAKS(t)
	for _, c := range cases {
		var ev eventsv1.Event
		load(t, c.file, &ev)
		fs := s.EvaluateEvent(EventOf(&ev))
		if got := keys(fs); !slices.Equal(got, c.want) {
			t.Errorf("%s: findings = %v, want %v", c.file, got, c.want)
		}
		for _, f := range fs {
			for _, e := range f.Evidence {
				if strings.Contains(e.Value, "9f1e3c2a-0b4d-4e8f-9a7c-1d2e3f4a5b6c") || len(e.Value) > modules.MaxEvidenceValueLength {
					t.Errorf("%s: evidence %s kept a UID or is too long: %q", c.file, e.Field, e.Value)
				}
			}
		}
	}
}

// TestFixtureNodes runs every rule on the node fixtures through NodeOf.
func TestFixtureNodes(t *testing.T) {
	cases := []struct {
		file string
		want []found
	}{
		{"node-aks-spot-preempt.yaml", []found{{aks.Name, aks.RuleSpotEviction, aks.ReasonSpotEviction}}},
		{"node-aks-memory-pressure.yaml", []found{{modules.GenericPlatform, modules.RuleNodePressure,
			modules.ReasonNodeMemoryPressure}}},
		{"node-kind.yaml", nil},
		{fixtureEKS, nil},
		{fixtureGKE, nil},
	}
	s := onAKS(t)
	for _, c := range cases {
		var n corev1.Node
		load(t, c.file, &n)
		if got := keys(s.EvaluateNode(NodeOf(&n))); !slices.Equal(got, c.want) {
			t.Errorf("%s: findings = %v, want %v", c.file, got, c.want)
		}
	}
}

// TestFixtureNodesDetection checks detection on the node fixtures: AKS, EKS and GKE nodes give their platform; a
// Kind node, no node, and nodes of two platforms give generic.
func TestFixtureNodesDetection(t *testing.T) {
	r := registry(t)
	cases := []struct {
		files []string
		want  string
	}{
		{[]string{"node-aks-spot-preempt.yaml", "node-aks-memory-pressure.yaml"}, aks.Name},
		{[]string{"node-kind.yaml"}, modules.GenericPlatform},
		{[]string{"node-kind.yaml", "node-aks-memory-pressure.yaml"}, modules.GenericPlatform},
		{[]string{fixtureEKS}, eks.Name},
		{[]string{fixtureGKE}, gke.Name},
		{[]string{fixtureEKS, fixtureGKE}, modules.GenericPlatform},
		{[]string{fixtureGKE, "node-aks-spot-preempt.yaml"}, modules.GenericPlatform},
		{[]string{fixtureEKS, "node-kind.yaml"}, modules.GenericPlatform},
		{nil, modules.GenericPlatform},
	}
	for _, c := range cases {
		var nodes []modules.Node
		for _, f := range c.files {
			var n corev1.Node
			load(t, f, &n)
			nodes = append(nodes, nodeOfForDetection(&n))
		}
		if got := r.DetectPlatform(nodes).Name(); got != c.want {
			t.Errorf("%v: platform = %q, want %q", c.files, got, c.want)
		}
	}
}

// TestEventOfRelated checks that EventOf copies the related object when the event has one, and leaves it
// empty when it has none.
func TestEventOfRelated(t *testing.T) {
	ev := eventsv1.Event{
		Regarding: corev1.ObjectReference{Kind: "ClusterPolicy", Name: "require-run-as-nonroot"},
		Related:   &corev1.ObjectReference{APIVersion: "batch/v1", Kind: "Job", Namespace: "demo", Name: "migrate"},
	}
	want := modules.ObjectRef{APIVersion: "batch/v1", Kind: "Job", Namespace: "demo", Name: "migrate"}
	if got := EventOf(&ev).Related; got != want {
		t.Errorf("related = %+v, want %+v", got, want)
	}
	ev.Related = nil
	if got := EventOf(&ev).Related; got != (modules.ObjectRef{}) {
		t.Errorf("related = %+v, want empty", got)
	}
}
