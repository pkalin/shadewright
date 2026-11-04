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
package eks

import (
	"reflect"
	"testing"

	"github.com/pkalin/shadewright/internal/modules"
	"github.com/pkalin/shadewright/internal/testutil/importguard"
)

// TestNoVendorImports enforces ADR-0013: no AWS SDK in a platform module.
func TestNoVendorImports(t *testing.T) {
	importguard.DirectImportsOnlyStandardOrModule(t, ".")
	importguard.NoForbiddenDeps(t, ".", ".")
}

const awsID = "aws:///eu-west-1a/i-0123456789abcdef0"

func TestDetection(t *testing.T) {
	r, err := modules.New([]modules.Platform{Platform{}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name  string
		nodes []modules.Node
		want  string
	}{
		{"managed node group", []modules.Node{{ProviderID: awsID, Labels: map[string]string{
			NodeGroupLabel: "", "eks.amazonaws.com/capacityType": "", "kubernetes.io/os": ""}}}, Name},
		{"label without provider ID", []modules.Node{{Labels: map[string]string{NodeGroupLabel: ""}}}, Name},
		{"EC2 without the label (kubeadm, Cluster API)", []modules.Node{{ProviderID: awsID,
			Labels: map[string]string{"kubernetes.io/os": ""}}}, modules.GenericPlatform},
		{"managed node group with a node without the label (Fargate, Karpenter)", []modules.Node{
			{ProviderID: awsID, Labels: map[string]string{NodeGroupLabel: ""}},
			{ProviderID: "aws:///eu-west-1a/fargate-ip-10-0-1-2.eu-west-1.compute.internal",
				Labels: map[string]string{"eks.amazonaws.com/compute-type": ""}}}, modules.GenericPlatform},
		{"label on another cloud", []modules.Node{{ProviderID: "gce://project/europe-west1-b/node-1",
			Labels: map[string]string{NodeGroupLabel: ""}}}, modules.GenericPlatform},
		{"Kind node", []modules.Node{{ProviderID: "kind://docker/kind/kind-control-plane",
			Labels: map[string]string{"kubernetes.io/hostname": ""}}}, modules.GenericPlatform},
		{"no nodes", nil, modules.GenericPlatform},
	}
	for _, c := range cases {
		if got := r.DetectPlatform(c.nodes).Name(); got != c.want {
			t.Errorf("%s: DetectPlatform = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestDetectionOnly checks that the module contributes nothing but detection.
func TestDetectionOnly(t *testing.T) {
	if h := (Platform{}).Hooks(); !reflect.DeepEqual(h, modules.Hooks{}) {
		t.Errorf("Hooks = %+v, want none", h)
	}
}
