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
	"strings"
	"testing"

	"github.com/pkalin/shadewright/api/v1alpha1"
)

func names[T interface{ Name() string }](mods []T) []string {
	out := make([]string, 0, len(mods))
	for _, m := range mods {
		out = append(out, m.Name())
	}
	return out
}

func TestNewRefuses(t *testing.T) {
	var nilPlatform Platform
	var nilIntegration Integration
	cases := []struct {
		name         string
		platforms    []Platform
		integrations []Integration
		want         string
	}{
		{"duplicate platform", []Platform{aks, eks, aks}, nil, `platform module "aks" is registered twice`},
		{"generic passed in", []Platform{stubPlatform{name: GenericPlatform}}, nil, `"generic" is registered twice`},
		{"duplicate integration", nil, []Integration{keda, flux, keda}, `integration module "keda" is registered twice`},
		{"nil platform", []Platform{nilPlatform}, nil, "platform module 0 is nil"},
		{"nil integration", nil, []Integration{keda, nilIntegration}, "integration module 1 is nil"},
		{"empty name", []Platform{stubPlatform{}}, nil, wantName},
		{"upper case", nil, []Integration{stubIntegration{name: "KEDA"}}, wantName},
		{"leading digit", nil, []Integration{stubIntegration{name: "1keda"}}, wantName},
		{"trailing dash", nil, []Integration{stubIntegration{name: "keda-"}}, wantName},
		{"space", nil, []Integration{stubIntegration{name: "ke da"}}, wantName},
		{"too long", nil, []Integration{stubIntegration{name: "k" + strings.Repeat("a", 63)}}, wantName},
		{"scheme claimed twice", []Platform{aks, stubPlatform{name: "azure-stack", detection: PlatformDetection{
			ProviderIDScheme: "azure"}}}, nil, `platform modules "aks" and "azure-stack" both claim provider ID scheme`},
		{"label claimed twice", []Platform{aks, stubPlatform{name: "other", detection: PlatformDetection{
			NodeLabels: []string{"kubernetes.azure.com/cluster"}}}}, nil, "both claim node label"},
		{"scheme with separator", []Platform{stubPlatform{name: aksName, detection: PlatformDetection{
			ProviderIDScheme: "azure://"}}}, nil, "scheme"},
		{"upper case scheme", []Platform{stubPlatform{name: aksName, detection: PlatformDetection{
			ProviderIDScheme: "Azure"}}}, nil, "scheme"},
		{"bad label", []Platform{stubPlatform{name: aksName, detection: PlatformDetection{
			NodeLabels: []string{"bad label"}}}}, nil, "node label"},
		{"empty label", []Platform{stubPlatform{name: aksName, detection: PlatformDetection{
			NodeLabels: []string{""}}}}, nil, "node label"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, err := New(c.platforms, c.integrations)
			if err == nil || r != nil {
				t.Fatalf("New = %v, %v; want a refusal", r, err)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not say %q", err, c.want)
			}
		})
	}
}

func TestNewRefusesBadGroupVersions(t *testing.T) {
	for _, gv := range []string{"", "keda.sh/", "/v1", "Keda.sh/v1", "keda.sh/v1/x", "keda.sh/v 1"} {
		in := stubIntegration{name: kedaName, detection: IntegrationDetection{GroupVersions: []string{kedaGV, gv}}}
		r, err := New(nil, []Integration{in})
		if err == nil || r != nil || !strings.Contains(err.Error(), "group version") {
			t.Errorf("New with group version %q = %v, %v; want a refusal", gv, r, err)
		}
	}
}

func TestNewAccepts(t *testing.T) {
	// A platform and an integration may share a name; a module may declare no detection at all.
	r := mustNew(t,
		[]Platform{stubPlatform{name: "capi"}, stubPlatform{name: sharedName}},
		[]Integration{stubIntegration{name: sharedName}, stubIntegration{name: "cloudflared"},
			stubIntegration{name: "core", detection: IntegrationDetection{GroupVersions: []string{"v1", appsGV}}}})
	if got := names(r.Platforms()); !slices.Equal(got, []string{"capi", GenericPlatform, sharedName}) {
		t.Errorf("Platforms = %v", got)
	}
	if got := names(r.DetectIntegrations([]string{"v1"})); !slices.Equal(got, []string{"core"}) {
		t.Errorf("DetectIntegrations = %v, want [core]", got)
	}
}

func TestListsAreSortedWhateverTheOrderGiven(t *testing.T) {
	for _, order := range [][]int{{0, 1, 2}, {2, 1, 0}, {1, 2, 0}} {
		ps := []Platform{gke, aks, eks}
		is := []Integration{keda, flux, stubIntegration{name: "kyverno"}}
		p := make([]Platform, 0, len(order))
		i := make([]Integration, 0, len(order))
		for _, n := range order {
			p = append(p, ps[n])
			i = append(i, is[n])
		}
		r := mustNew(t, p, i)
		if got := names(r.Platforms()); !slices.Equal(got, []string{aksName, eksName, GenericPlatform, gkeName}) {
			t.Errorf("order %v: Platforms = %v", order, got)
		}
		if got := names(r.Integrations()); !slices.Equal(got, []string{fluxName, kedaName, "kyverno"}) {
			t.Errorf("order %v: Integrations = %v", order, got)
		}
	}
}

func TestLookUp(t *testing.T) {
	r := mustNew(t, []Platform{aks}, []Integration{keda})
	if p, ok := r.Platform(aksName); !ok || p.Name() != aksName {
		t.Errorf("Platform(aks) = %v, %v", p, ok)
	}
	if p, ok := r.Platform(GenericPlatform); !ok || p.Name() != GenericPlatform {
		t.Errorf("Platform(generic) = %v, %v; the generic platform is always present", p, ok)
	}
	if p, ok := r.Platform(kedaName); ok || p != nil {
		t.Errorf("Platform(keda) = %v, %v; integrations are not platforms", p, ok)
	}
	in, ok := r.Integration(kedaName)
	if !ok || in.Name() != kedaName {
		t.Fatalf("Integration(keda) = %v, %v", in, ok)
	}
	if !slices.Equal(in.Hooks().Actions, []v1alpha1.ActionType{v1alpha1.ActionAdjustKedaMaxReplicas}) {
		t.Errorf("Integration(keda) hooks = %v", in.Hooks())
	}
	if in, ok := r.Integration(aksName); ok || in != nil {
		t.Errorf("Integration(aks) = %v, %v", in, ok)
	}
}

func TestGenericContributesNothing(t *testing.T) {
	g, _ := mustNew(t, nil, nil).Platform(GenericPlatform)
	h := g.Hooks()
	if g.Detection().ProviderIDScheme != "" || len(g.Detection().NodeLabels) != 0 ||
		len(h.Symptoms)+len(h.Rules)+len(h.Tools)+len(h.Actions) != 0 {
		t.Errorf("generic = %+v, %+v; want no detection and no hooks", g.Detection(), h)
	}
}
