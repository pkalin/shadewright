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

package rollout

import (
	"os"
	"slices"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

// The rollout check reads only the target object, so the executor role generated from the RBAC markers keeps
// exactly the grants it had before #532: get (no list, no watch) on Deployments and StatefulSets, nothing on
// pods, no write verb outside the operator's own requests (invariants 2 and 6). A new grant fails this test
// and has to be changed here on purpose.
func TestExecutorRoleUnchanged(t *testing.T) {
	raw, err := os.ReadFile("../../../config/rbac/executor/role.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for doc := range strings.SplitSeq(string(raw), "\n---\n") {
		if strings.TrimSpace(strings.TrimPrefix(doc, "---")) == "" {
			continue
		}
		var role rbacv1.ClusterRole // a Role has the same rules field
		if err := yaml.Unmarshal([]byte(doc), &role); err != nil {
			t.Fatal(err)
		}
		for _, r := range role.Rules {
			for _, g := range r.APIGroups {
				for _, res := range r.Resources {
					for _, v := range r.Verbs {
						got = append(got, role.Kind+" "+g+"/"+res+" "+v)
					}
				}
			}
			if len(r.ResourceNames) > 0 || len(r.NonResourceURLs) > 0 {
				t.Errorf("unexpected rule %+v", r)
			}
		}
	}
	slices.Sort(got)
	want := []string{
		"ClusterRole apps/deployments get",
		"ClusterRole apps/statefulsets get",
		"ClusterRole keda.sh/scaledobjects get",
		"ClusterRole kustomize.toolkit.fluxcd.io/kustomizations get",
		"ClusterRole remediation.shadewright.io/remediationpolicies get",
		"ClusterRole remediation.shadewright.io/remediationsettings get",
		"ClusterRole source.toolkit.fluxcd.io/gitrepositories get",
		"Role remediation.shadewright.io/incidents get",
		"Role remediation.shadewright.io/remediationrequests get",
		"Role remediation.shadewright.io/remediationrequests list",
		"Role remediation.shadewright.io/remediationrequests patch",
		"Role remediation.shadewright.io/remediationrequests watch",
		"Role remediation.shadewright.io/remediationrequests/status get",
		"Role remediation.shadewright.io/remediationrequests/status patch",
		"Role remediation.shadewright.io/remediationrequests/status update",
	}
	if !slices.Equal(got, want) {
		t.Errorf("executor role grants\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
