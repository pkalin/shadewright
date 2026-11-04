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
package gke

import (
	"reflect"
	"testing"

	"github.com/pkalin/shadewright/internal/modules"
	"github.com/pkalin/shadewright/internal/testutil/importguard"
)

// TestNoVendorImports enforces ADR-0013: no Google Cloud SDK in a platform module.
func TestNoVendorImports(t *testing.T) {
	importguard.DirectImportsOnlyStandardOrModule(t, ".")
	importguard.NoForbiddenDeps(t, ".", ".")
}

const gceID = "gce://project/europe-west1-b/gke-lab-default-pool-1a2b3c4d-x7k9"

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
		{"node pool", []modules.Node{{ProviderID: gceID, Labels: map[string]string{
			NodePoolLabel: "", "cloud.google.com/machine-family": "", "kubernetes.io/os": ""}}}, Name},
		{"label without provider ID", []modules.Node{{Labels: map[string]string{NodePoolLabel: ""}}}, Name},
		{"Compute Engine without the label (kubeadm, Cluster API)", []modules.Node{{ProviderID: gceID,
			Labels: map[string]string{"kubernetes.io/os": ""}}}, modules.GenericPlatform},
		{"GKE with a self-managed node", []modules.Node{
			{ProviderID: gceID, Labels: map[string]string{NodePoolLabel: ""}},
			{ProviderID: gceID, Labels: map[string]string{"kubernetes.io/os": ""}}}, modules.GenericPlatform},
		{"label on another cloud", []modules.Node{{ProviderID: "aws:///eu-west-1a/i-0123456789abcdef0",
			Labels: map[string]string{NodePoolLabel: ""}}}, modules.GenericPlatform},
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
