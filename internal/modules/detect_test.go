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
		// capiLabel stands for the labels of a self-managed node; no platform here requires it.
		capiLabel = "node.cluster.x-k8s.io/machine"
	)
	node := func(id string, labels ...string) Node {
		n := Node{ProviderID: id, Labels: map[string]string{}}
		for _, l := range labels {
			n.Labels[l] = "x"
		}
		return n
	}
	aksNode := node(azureID, aksLabel, "kubernetes.io/os")
	// A node still joining has no provider ID yet and none of the managed service's labels.
	joining := node("", "kubernetes.io/hostname")
	// A virtual node (virtual-kubelet) has no provider ID and no node pool labels.
	virtual := node("", "type", "kubernetes.io/role")
	cases := []struct {
		name  string
		nodes []Node
		want  string
	}{
		{"no nodes", nil, GenericPlatform},
		{"empty list", []Node{}, GenericPlatform},
		{"AKS", []Node{aksNode, aksNode}, aksName},
		{"EKS", []Node{node(awsID, eksLabel)}, eksName},
		{"GKE", []Node{node(gceID, gkeLabel), node(gceID, gkeLabel)}, gkeName},
		{"CAPZ-like: azure scheme without the AKS label", []Node{node(azureID, capiLabel), node(azureID)}, GenericPlatform},
		{"kubeadm on EC2", []Node{node(awsID)}, GenericPlatform},
		{"self-managed GCE", []Node{node(gceID, capiLabel)}, GenericPlatform},
		{"AKS with a self-managed node", []Node{aksNode, node(azureID)}, GenericPlatform},
		{"AKS with a joining node", []Node{aksNode, joining}, aksName},
		{"joining node first", []Node{joining, aksNode}, aksName},
		{"AKS with a virtual node", []Node{aksNode, virtual, aksNode}, aksName},
		{"only joining and virtual nodes", []Node{joining, virtual}, GenericPlatform},
		{"node with nothing is left out", []Node{aksNode, {}}, aksName},
		{"only a node with nothing", []Node{{}}, GenericPlatform},
		{"unknown scheme", []Node{node(kindID)}, GenericPlatform},
		{"AKS with an unknown provider", []Node{aksNode, node(kindID)}, GenericPlatform},
		{"unknown provider first", []Node{node(kindID), aksNode}, GenericPlatform},
		{"mixed platforms", []Node{aksNode, node(awsID, eksLabel)}, GenericPlatform},
		{"scheme is case sensitive", []Node{node("AZURE:///x", aksLabel)}, GenericPlatform},
		{"no scheme separator", []Node{node("azure", aksLabel)}, GenericPlatform},
		{"AKS label without provider ID", []Node{node("", aksLabel)}, aksName},
		{"AKS label and AKS node agree", []Node{aksNode, node("", aksLabel)}, aksName},
		{"labels of two platforms", []Node{node("", aksLabel, eksLabel)}, GenericPlatform},
		{"AKS label on another cloud", []Node{node(awsID, aksLabel)}, GenericPlatform},
		{"label value is not read", []Node{node(azureID, "unrelated")}, GenericPlatform},
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

// TestDetectPlatformSharedScheme covers two platforms on one cloud, such as AKS and a Cluster API platform on
// Azure: the scheme is shared and the labels tell them apart.
func TestDetectPlatformSharedScheme(t *testing.T) {
	const (
		azureID   = "azure:///subscriptions/sub/x"
		capiLabel = "cluster.x-k8s.io/cluster-name"
	)
	capz := stubPlatform{name: capzName, detection: PlatformDetection{ProviderIDScheme: azureScheme, NodeLabels: []string{capiLabel}}}
	r := mustNew(t, []Platform{aks, capz}, nil)
	cases := []struct {
		name string
		node Node
		want string
	}{
		{"AKS node", Node{ProviderID: azureID, Labels: map[string]string{aksLabel: "c"}}, aksName},
		{"CAPZ node", Node{ProviderID: azureID, Labels: map[string]string{capiLabel: "c"}}, capzName},
		{"node with both labels", Node{ProviderID: azureID, Labels: map[string]string{aksLabel: "c", capiLabel: "c"}},
			GenericPlatform},
		{"azure node with neither", Node{ProviderID: azureID}, GenericPlatform},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := r.DetectPlatform([]Node{c.node}); got.Name() != c.want {
				t.Errorf("DetectPlatform = %s, want %s", got.Name(), c.want)
			}
		})
	}
}

// TestDetectPlatformNeedsEveryLabel covers a platform that requires more than one label.
func TestDetectPlatformNeedsEveryLabel(t *testing.T) {
	two := stubPlatform{name: twoName, detection: PlatformDetection{NodeLabels: []string{labelX, labelY}}}
	r := mustNew(t, []Platform{two}, nil)
	if got := r.DetectPlatform([]Node{{Labels: map[string]string{labelX: ""}}}); got.Name() != GenericPlatform {
		t.Errorf("one of two labels: DetectPlatform = %s, want generic", got.Name())
	}
	both := Node{ProviderID: "metal://rack/1", Labels: map[string]string{labelX: "", labelY: ""}}
	if got := r.DetectPlatform([]Node{both}); got.Name() != twoName {
		t.Errorf("both labels, any scheme: DetectPlatform = %s, want two", got.Name())
	}
}

func TestDetectPlatformWithOnlyGeneric(t *testing.T) {
	r := mustNew(t, nil, nil)
	if got := r.DetectPlatform([]Node{{ProviderID: "azure:///x", Labels: map[string]string{aksLabel: ""}}}); got.Name() != GenericPlatform {
		t.Errorf("DetectPlatform = %s, want generic", got.Name())
	}
}

func TestDetectIntegrations(t *testing.T) {
	r := mustNew(t, nil, []Integration{keda, flux})
	cases := []struct {
		name   string
		served []string
		want   []string // name=group version
	}{
		{"nothing served", nil, nil},
		{"core only", []string{"v1", appsGV}, nil},
		{"KEDA", []string{"v1", appsGV, kedaGV}, []string{kedaName + "=" + kedaGV}},
		{"another KEDA version", []string{"keda.sh/v1beta9"}, nil},
		{"group only", []string{kedaGroup}, nil},
		{"any version of Flux", []string{helmGV}, []string{fluxName + "=" + helmGV}},
		{"both, sorted by name", []string{kedaGV, kustomizeGV},
			[]string{fluxName + "=" + kustomizeGV, kedaName + "=" + kedaGV}},
		{"served twice", []string{kedaGV, kedaGV}, []string{kedaName + "=" + kedaGV}},
		// Flux declares kustomize first: the first declared version that is served is reported, whatever
		// order discovery lists them in.
		{"two versions of one add-on", []string{helmGV, kustomizeGV}, []string{fluxName + "=" + kustomizeGV}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got []string
			for _, d := range r.DetectIntegrations(c.served) {
				got = append(got, d.Integration.Name()+"="+d.GroupVersion)
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
