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
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/testutil/crdschema"
)

// ptrPlatform and ptrIntegration have pointer receivers that read their fields, so a nil one panics when
// called.
type ptrPlatform struct{ name string }

func (p *ptrPlatform) Name() string               { return p.name }
func (*ptrPlatform) Detection() PlatformDetection { return PlatformDetection{} }
func (*ptrPlatform) Hooks() Hooks                 { return Hooks{} }

type ptrIntegration struct{ name string }

func (i *ptrIntegration) Name() string                  { return i.name }
func (*ptrIntegration) Detection() IntegrationDetection { return IntegrationDetection{} }
func (*ptrIntegration) Hooks() Hooks                    { return Hooks{} }

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
		{"same claim twice", []Platform{aks, stubPlatform{name: "azure-stack", detection: PlatformDetection{
			ProviderIDScheme: azureScheme, NodeLabels: []string{aksLabel}}}}, nil,
			`platform modules "aks" and "azure-stack" both claim provider ID scheme "azure"`},
		{"same claim in another label order", []Platform{
			stubPlatform{name: "one", detection: PlatformDetection{NodeLabels: []string{labelX, labelY}}},
			stubPlatform{name: twoName, detection: PlatformDetection{NodeLabels: []string{labelY, labelX}}}},
			nil, `platform modules "one" and "two" both claim`},
		{"label listed twice", []Platform{stubPlatform{name: aksName, detection: PlatformDetection{
			ProviderIDScheme: azureScheme, NodeLabels: []string{aksLabel, aksLabel}}}},
			nil, `platform module "aks": node label "kubernetes.azure.com/cluster" is listed twice`},
		{"scheme without labels", []Platform{stubPlatform{name: aksName, detection: PlatformDetection{
			ProviderIDScheme: azureScheme}}}, nil, "needs at least one node label"},
		{"typed nil platform", []Platform{aks, (*ptrPlatform)(nil)}, nil, "platform module 1 is nil"},
		{"typed nil integration", nil, []Integration{(*ptrIntegration)(nil)}, "integration module 0 is nil"},
		{"scheme with separator", []Platform{stubPlatform{name: aksName, detection: PlatformDetection{
			ProviderIDScheme: "azure://", NodeLabels: []string{labelX}}}}, nil, wantInvalid},
		{"upper case scheme", []Platform{stubPlatform{name: aksName, detection: PlatformDetection{
			ProviderIDScheme: "Azure", NodeLabels: []string{labelX}}}}, nil, wantInvalid},
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
	for _, gv := range []string{"", "keda.sh/", "/v1", "Keda.sh/v1", "keda.sh/v1/x", "keda.sh/v 1", "keda.sh/foo",
		"a..b/v1"} {
		in := stubIntegration{name: kedaName, detection: IntegrationDetection{GroupVersions: []string{kedaGV, gv}}}
		r, err := New(nil, []Integration{in})
		if err == nil || r != nil || !strings.Contains(err.Error(), "group version") {
			t.Errorf("New with group version %q = %v, %v; want a refusal", gv, r, err)
		}
	}
}

func TestNewAccepts(t *testing.T) {
	// A platform and an integration may share a name; a module may declare no detection at all; two platforms
	// may share a provider ID scheme or a label as long as their claims differ.
	capz := stubPlatform{name: capzName, detection: PlatformDetection{ProviderIDScheme: azureScheme,
		NodeLabels: []string{"cluster.x-k8s.io/cluster-name"}}}
	aksOnly := stubPlatform{name: "aks-labels", detection: PlatformDetection{
		NodeLabels: []string{aksLabel}}}
	r := mustNew(t,
		[]Platform{stubPlatform{name: "capi"}, stubPlatform{name: sharedName}, aks, capz, aksOnly},
		[]Integration{stubIntegration{name: sharedName}, stubIntegration{name: "cloudflared"},
			stubIntegration{name: "core", detection: IntegrationDetection{GroupVersions: []string{"v1", appsGV}}}})
	want := []string{aksName, "aks-labels", "capi", capzName, GenericPlatform, sharedName}
	if got := names(r.Platforms()); !slices.Equal(got, want) {
		t.Errorf("Platforms = %v, want %v", got, want)
	}
	if got := r.DetectIntegrations([]string{"v1"}); len(got) != 1 || got[0].Integration.Name() != "core" ||
		got[0].GroupVersion != "v1" {
		t.Errorf("DetectIntegrations = %+v, want core by v1", got)
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

func TestGenericHasRulesOnly(t *testing.T) {
	g, _ := mustNew(t, nil, nil).Platform(GenericPlatform)
	h := g.Hooks()
	if g.Detection().ProviderIDScheme != "" || len(g.Detection().NodeLabels) != 0 ||
		len(h.Symptoms)+len(h.Tools)+len(h.Actions) != 0 {
		t.Errorf("generic = %+v, %+v; want no detection, tools, actions or symptom sources", g.Detection(), h)
	}
	names := make([]string, 0, len(h.Rules))
	for _, r := range h.Rules {
		names = append(names, r.Name)
	}
	if !slices.Equal(names, []string{RuleNodePressure, RuleInsufficientCapacity}) {
		t.Errorf("generic rules = %v", names)
	}
}

// rulePlatform is a platform with rules, for the rule checks of New.
type rulePlatform struct {
	stubPlatform
	rules []DiagnosisRule
}

func (p rulePlatform) Hooks() Hooks { return Hooks{Rules: p.rules} }

func TestNewRefusesBadRules(t *testing.T) {
	ev := func(Event) (Symptom, bool) { return Symptom{}, false }
	cases := []struct {
		name  string
		rules []DiagnosisRule
		want  string
	}{
		{"invalid name", []DiagnosisRule{{Name: "Bad_Name", Event: ev}}, `rule name "Bad_Name" is invalid`},
		{"name twice", []DiagnosisRule{{Name: "r", Event: ev}, {Name: "r", Event: ev}}, `rule "r" is listed twice`},
		{"no function", []DiagnosisRule{{Name: "r"}}, `rule "r" has neither an event nor a node function`},
	}
	for _, c := range cases {
		_, err := New([]Platform{rulePlatform{stubPlatform: aks, rules: c.rules}}, nil)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: New = %v, want an error with %q", c.name, err, c.want)
		}
	}
	if _, err := New([]Platform{rulePlatform{stubPlatform: aks, rules: []DiagnosisRule{{Name: "r", Event: ev}}}}, nil); err != nil {
		t.Errorf("a valid rule: New = %v", err)
	}
}

const settingsCRD = "remediation.shadewright.io_remediationsettings.yaml"

// TestNamePatternMatchesSchema keeps NamePattern equal to the pattern of every module name in the generated
// RemediationSettings CRD, so a name the registry accepts can be enabled and reported, and a name the schema
// accepts is one a module may have.
func TestNamePatternMatchesSchema(t *testing.T) {
	for _, path := range []string{
		"spec.modules.platforms.items",
		"spec.modules.integrations.items",
		"status.platform.detected",
		"status.platform.active",
		"status.integrations.items.name",
	} {
		if got := crdschema.Pattern(t, settingsCRD, path); got != NamePattern {
			t.Errorf("pattern of %s is %q, want NamePattern %q", path, got, NamePattern)
		}
	}
}

// TestBoundsMatchSchema keeps MaxPlatforms and MaxIntegrations equal to the caps of the settings lists, and
// status.integrations large enough for every enabled name and every compiled-in integration.
func TestBoundsMatchSchema(t *testing.T) {
	if got := crdschema.MaxItems(t, settingsCRD, "spec.modules.platforms"); got != MaxPlatforms {
		t.Errorf("spec.modules.platforms maxItems = %d, want MaxPlatforms %d", got, MaxPlatforms)
	}
	if got := crdschema.MaxItems(t, settingsCRD, "spec.modules.integrations"); got != MaxIntegrations {
		t.Errorf("spec.modules.integrations maxItems = %d, want MaxIntegrations %d", got, MaxIntegrations)
	}
	if got := crdschema.MaxItems(t, settingsCRD, "status.integrations"); got != 2*MaxIntegrations {
		t.Errorf("status.integrations maxItems = %d, want %d (every enabled and every compiled-in integration)",
			got, 2*MaxIntegrations)
	}
}

// TestNewBoundsModules checks that New refuses one module more than each bound and accepts the bound itself.
func TestNewBoundsModules(t *testing.T) {
	platforms := func(n int) []Platform {
		out := make([]Platform, n)
		for i := range out {
			out[i] = stubPlatform{name: fmt.Sprintf("p%d", i)}
		}
		return out
	}
	integrations := func(n int) []Integration {
		out := make([]Integration, n)
		for i := range out {
			out[i] = stubIntegration{name: fmt.Sprintf("i%d", i)}
		}
		return out
	}
	r := mustNew(t, platforms(MaxPlatforms), integrations(MaxIntegrations))
	if got := len(r.Platforms()); got != MaxPlatforms+1 {
		t.Errorf("Platforms has %d entries, want %d with generic", got, MaxPlatforms+1)
	}
	if r, err := New(platforms(MaxPlatforms+1), nil); err == nil || r != nil ||
		!strings.Contains(err.Error(), fmt.Sprintf("at most %d are allowed besides generic", MaxPlatforms)) {
		t.Errorf("New with %d platforms: registry %t, error %v; want a refusal", MaxPlatforms+1, r != nil, err)
	}
	if r, err := New(nil, integrations(MaxIntegrations+1)); err == nil || r != nil ||
		!strings.Contains(err.Error(), fmt.Sprintf("at most %d are allowed", MaxIntegrations)) {
		t.Errorf("New with %d integrations: registry %t, error %v; want a refusal", MaxIntegrations+1, r != nil, err)
	}
}

func TestValidName(t *testing.T) {
	for _, s := range []string{"a", "aks", "secrets-store-csi-driver", "k8s", "a" + strings.Repeat("b", 62)} {
		if !ValidName(s) {
			t.Errorf("ValidName(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"", "AKS", "1aks", "-aks", "aks-", "a_b", "a.b", "a b", "a" + strings.Repeat("b", 63)} {
		if ValidName(s) {
			t.Errorf("ValidName(%q) = true, want false", s)
		}
	}
}
