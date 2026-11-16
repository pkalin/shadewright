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

package freshness_test

import (
	"testing"

	"github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/policy/freshness"
)

// TestFindReinstalled checks that only the plan's URL under another GitHub installation is the same repository
// reached through a new installation (#290); another URL, the same installation or no recorded installation is not.
func TestFindReinstalled(t *testing.T) {
	const url = "https://github.com/example/fleet"
	entry := func(u string, provider v1alpha1.GitProviderType, installation int64) v1alpha1.GitRepository {
		r := v1alpha1.GitRepository{Name: "fleet", URL: u, Provider: provider}
		if installation != 0 {
			r.GitHub = &v1alpha1.GitHubRepository{InstallationID: installation}
		}
		return r
	}
	cases := []struct {
		name  string
		entry v1alpha1.GitRepository
		want  *v1alpha1.PlanRepository
		found bool
	}{
		{"new installation of the same URL", entry(url, v1alpha1.GitProviderGitHub, 2), &v1alpha1.PlanRepository{URL: url, InstallationID: 1}, true},
		{"same installation", entry(url, v1alpha1.GitProviderGitHub, 1), &v1alpha1.PlanRepository{URL: url, InstallationID: 1}, false},
		{"another URL", entry(url+"-moved", v1alpha1.GitProviderGitHub, 2), &v1alpha1.PlanRepository{URL: url, InstallationID: 1}, false},
		{"no installation recorded", entry(url, v1alpha1.GitProviderGitHub, 2), &v1alpha1.PlanRepository{URL: url}, false},
		{"no plan repository", entry(url, v1alpha1.GitProviderGitHub, 2), nil, false},
		{"entry without GitHub settings", entry(url, v1alpha1.GitProviderGitHub, 0), &v1alpha1.PlanRepository{URL: url, InstallationID: 1}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &v1alpha1.RemediationSettings{Spec: v1alpha1.RemediationSettingsSpec{
				Repositories: []v1alpha1.GitRepository{tc.entry}}}
			got, ok := freshness.FindReinstalled(s, tc.want)
			if ok != tc.found {
				t.Fatalf("found = %v, want %v", ok, tc.found)
			}
			if ok && got.URL != url {
				t.Fatalf("got entry %q", got.URL)
			}
			if _, exact := freshness.FindRepository(s, tc.want); exact && ok {
				t.Fatal("an entry FindRepository finds is never a reinstalled one")
			}
		})
	}
}
