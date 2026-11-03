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

package modules

import (
	"slices"
	"testing"
)

func TestDetectPlatform(t *testing.T) {
	const (
		azureID = "azure:///subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/vmss/virtualMachines/0"
		awsID   = "aws:///eu-west-1a/i-0123456789abcdef0"
		gceID   = "gce://project/europe-west1-b/node-1"
		kindID  = "kind://docker/kind/kind-control-plane"
	)
	node := func(id string, labels ...string) Node {
		n := Node{ProviderID: id, Labels: map[string]string{}}
		for _, l := range labels {
			n.Labels[l] = "x"
		}
		return n
	}
	cases := []struct {
		name  string
		nodes []Node
		want  string
	}{
		{"no nodes", nil, GenericPlatform},
		{"empty list", []Node{}, GenericPlatform},
		{"AKS", []Node{node(azureID), node(azureID)}, aksName},
		{"EKS", []Node{node(awsID)}, eksName},
		{"GKE", []Node{node(gceID), node(gceID)}, gkeName},
		{"unknown scheme", []Node{node(kindID)}, GenericPlatform},
		{"scheme is case sensitive", []Node{node("AZURE:///x")}, GenericPlatform},
		{"no scheme separator", []Node{node("azure")}, GenericPlatform},
		{"mixed providers", []Node{node(azureID), node(awsID)}, GenericPlatform},
		{"mixed with an unknown provider", []Node{node(azureID), node(kindID)}, GenericPlatform},
		{"unknown provider first", []Node{node(kindID), node(azureID)}, GenericPlatform},
		{"no provider ID and no labels", []Node{{}}, GenericPlatform},
		{"one node without anything", []Node{node(azureID), {}}, GenericPlatform},
		{"label without provider ID", []Node{node("", "kubernetes.azure.com/cluster", "other")}, aksName},
		{"label and provider ID agree", []Node{node(azureID), node("", "kubernetes.azure.com/cluster")}, aksName},
		{"labels of two platforms", []Node{node("", "kubernetes.azure.com/cluster", "eks.amazonaws.com/nodegroup")}, GenericPlatform},
		{"provider ID wins over labels", []Node{node(awsID, "kubernetes.azure.com/cluster")}, eksName},
		{"label value is not read", []Node{node("", "unrelated")}, GenericPlatform},
	}
	r := mustNew(t, []Platform{gke, aks, eks}, nil)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := r.DetectPlatform(c.nodes)
			if got == nil || got.Name() != c.want {
				t.Errorf("DetectPlatform = %v, want %s", got, c.want)
			}
		})
	}
}

func TestDetectPlatformWithOnlyGeneric(t *testing.T) {
	r := mustNew(t, nil, nil)
	if got := r.DetectPlatform([]Node{{ProviderID: "azure:///x"}}); got.Name() != GenericPlatform {
		t.Errorf("DetectPlatform = %s, want generic", got.Name())
	}
}

func TestDetectIntegrations(t *testing.T) {
	r := mustNew(t, nil, []Integration{keda, flux})
	cases := []struct {
		name   string
		served []string
		want   []string
	}{
		{"nothing served", nil, nil},
		{"core only", []string{"v1", appsGV}, nil},
		{"KEDA", []string{"v1", appsGV, kedaGV}, []string{kedaName}},
		{"another KEDA version", []string{"keda.sh/v1beta9"}, nil},
		{"group only", []string{"keda.sh"}, nil},
		{"any version of Flux", []string{helmGV}, []string{fluxName}},
		{"both, sorted by name", []string{kedaGV, kustomizeGV}, []string{fluxName, kedaName}},
		{"served twice", []string{kedaGV, kedaGV}, []string{kedaName}},
		{"two versions of one add-on", []string{helmGV, kustomizeGV}, []string{fluxName}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got []string
			for _, in := range r.DetectIntegrations(c.served) {
				got = append(got, in.Name())
			}
			if !slices.Equal(got, c.want) {
				t.Errorf("DetectIntegrations = %v, want %v", got, c.want)
			}
		})
	}
}

func TestDetectionUsesTheValuesGivenToNew(t *testing.T) {
	gvs := []string{kedaGV}
	r := mustNew(t, nil, []Integration{stubIntegration{name: kedaName, detection: IntegrationDetection{GroupVersions: gvs}}})
	gvs[0] = "changed/v1"
	if got := r.DetectIntegrations([]string{kedaGV}); len(got) != 1 {
		t.Error("changing the module's slice after New changed integration detection")
	}
}
