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

package sourcemap

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestRevisionOfBranch(t *testing.T) {
	const br = "main"
	sha1 := strings.Repeat("0123456789abcdef", 2) + "01234567"
	sha256 := strings.Repeat("0123456789abcdef", 4)
	for _, tc := range []struct {
		revision, branch string
		want             bool
	}{
		{br + "@sha1:" + sha1, br, true},
		{br + "@sha256:" + sha256, br, true},
		{"release/v1@sha1:" + sha1, "release/v1", true},
		{"a@b@sha1:" + sha1, "a@b", true},
		{br + "@sha1:" + sha1, "release", false},
		{br + "@sha1:" + sha1, "", false},
		{"@sha1:" + sha1, "", false},
		{br, br, false},
		{br + "@" + sha1, br, false},
		{br + "@sha1:" + sha256, br, false},
		{br + "@sha256:" + sha1, br, false},
		{br + "@sha1:" + sha1[:39], br, false},
		{br + "@sha1:" + strings.ToUpper(sha1), br, false},
		{br + "@md5:" + sha1, br, false},
		{br + "@sha1:" + sha1 + "\n", br, false},
	} {
		if got := revisionOfBranch(tc.revision, tc.branch); got != tc.want {
			t.Errorf("revisionOfBranch(%q, %q) = %v, want %v", tc.revision, tc.branch, got, tc.want)
		}
	}
}

// TestKustomizationSpecRefusal checks the exported spec rules: each refusing field gives the mapping's reason,
// suspend wins over the others as in the mapping, and a plain spec passes.
func TestKustomizationSpecRefusal(t *testing.T) {
	for name, tc := range map[string]struct {
		spec map[string]any
		want Reason
	}{
		"plain":      {spec: map[string]any{"path": "./apps"}},
		"suspended":  {spec: map[string]any{"suspend": true, "kubeConfig": map[string]any{"x": "y"}}, want: ReasonKustomizationSuspended},
		"remote":     {spec: map[string]any{"kubeConfig": map[string]any{"x": "y"}, "images": []any{"x"}}, want: ReasonRemoteCluster},
		"transform":  {spec: map[string]any{"postBuild": map[string]any{"x": "y"}}, want: ReasonKustomizationTransforms},
		"decryption": {spec: map[string]any{"decryption": map[string]any{"provider": "sops"}}, want: ReasonDecryption},
		"not set":    {spec: map[string]any{"suspend": false, "patches": []any{}}},
	} {
		t.Run(name, func(t *testing.T) {
			ks := &unstructured.Unstructured{Object: map[string]any{"spec": tc.spec}}
			ks.SetNamespace("flux-system")
			ks.SetName("apps")
			r := KustomizationSpecRefusal(ks)
			switch {
			case tc.want == "" && r != nil:
				t.Fatalf("got %s, want nil", r.Reason)
			case tc.want != "" && (r == nil || r.Reason != tc.want || r.Outcome != NeedsIntervention):
				t.Fatalf("got %+v, want %s", r, tc.want)
			}
		})
	}
}
