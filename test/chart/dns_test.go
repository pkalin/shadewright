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

package chart

import (
	"encoding/json"
	"strings"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	analyzerEgressKey = "NetworkPolicy/" + operatorNS + "/shadewright-allow-analyzer-egress"
	executorEgressKey = "NetworkPolicy/" + operatorNS + "/shadewright-allow-executor-egress"
	testEgressKey     = "NetworkPolicy/" + operatorNS + "/shadewright-test-egress"
)

// dnsRule returns the one egress rule of the NetworkPolicy that opens port 53, as JSON.
func dnsRule(t *testing.T, k string, u *unstructured.Unstructured) string {
	t.Helper()
	if u == nil {
		t.Fatalf("%s not rendered", k)
	}
	var found []string
	for _, rule := range convert[networkingv1.NetworkPolicy](t, u).Spec.Egress {
		for _, p := range rule.Ports {
			if p.Port != nil && p.Port.IntValue() == 53 {
				found = append(found, asJSON(t, rule))
				break
			}
		}
	}
	if len(found) != 1 {
		t.Fatalf("%s: %d egress rules open port 53, want 1: %v", k, len(found), found)
	}
	return found[0]
}

const (
	dnsPorts  = `"ports":[{"protocol":"UDP","port":53},{"protocol":"TCP","port":53}]`
	kubeDNSTo = `{"podSelector":{"matchLabels":{"k8s-app":"kube-dns"}},` +
		`"namespaceSelector":{"matchLabels":{"kubernetes.io/metadata.name":"kube-system"}}}`
	kubeDNSRule  = `{"to":[` + kubeDNSTo + `],` + dnsPorts + `}`
	nodeLocalDNS = `{"to":[{"ipBlock":{"cidr":"169.254.20.10/32"}},{"ipBlock":{"cidr":"fd00::10/128"}}],` + dnsPorts + `}`
	cidrsValue   = "resolverCIDRs"
	cidrsMode    = "networkPolicy.dns.mode=" + cidrsValue
	cidrsKey     = "networkPolicy.dns.resolverCIDRs"
)

// TestDNSEgressProfilesRenderAndTheHookUsesTheWorkloadRule renders both profiles. The analyzer, the executor and
// the helm test hook must carry the same DNS rule, and it must be the one the profile names.
func TestDNSEgressProfilesRenderAndTheHookUsesTheWorkloadRule(t *testing.T) {
	cases := map[string]struct {
		set  []string
		want string
	}{
		"default is the kube-dns selector": {nil, kubeDNSRule},
		"kubeDns mode set explicitly":      {[]string{"networkPolicy.dns.mode=kubeDns"}, kubeDNSRule},
		"resolver CIDRs for NodeLocal DNS": {
			[]string{
				"networkPolicy.dns.mode=resolverCIDRs",
				"networkPolicy.dns.resolverCIDRs={169.254.20.10/32,fd00::10/128}",
			},
			nodeLocalDNS,
		},
		"resolverCIDRs ignored in kubeDns mode": {
			[]string{"networkPolicy.dns.resolverCIDRs={10.1.2.3/32}"}, kubeDNSRule,
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			r := defaultRender(t, c.set...)
			analyzer := dnsRule(t, analyzerEgressKey, r.chart[analyzerEgressKey])
			executor := dnsRule(t, executorEgressKey, r.chart[executorEgressKey])
			hook := dnsRule(t, testEgressKey, r.tests[testEgressKey])
			for k, got := range map[string]string{analyzerComp: analyzer, executorComp: executor, "test hook": hook} {
				// Compare the decoded rule, not the text: key order is not part of the rule.
				var w networkingv1.NetworkPolicyEgressRule
				if err := json.Unmarshal([]byte(c.want), &w); err != nil {
					t.Fatal(err)
				}
				if asJSON(t, w) != got {
					t.Errorf("%s DNS rule\n got:  %s\n want: %s", k, got, asJSON(t, w))
				}
			}
			if analyzer != hook || executor != hook {
				t.Errorf("the hook rule must equal the workload rule:\n analyzer: %s\n executor: %s\n hook:     %s",
					analyzer, executor, hook)
			}
		})
	}
}

// TestDNSEgressDefaultEqualsKustomize keeps the default rendering of the workload rules what config/ ships.
// (TestNetworkPoliciesServicesAndCRDsEqualKustomize compares the whole policies; this one names the DNS rule.)
func TestDNSEgressDefaultEqualsKustomize(t *testing.T) {
	r := render(t, false)
	for _, k := range []string{analyzerEgressKey, executorEgressKey} {
		if got, want := dnsRule(t, k, r.chart[k]), dnsRule(t, k, r.kustomize[k]); got != want {
			t.Errorf("%s DNS rule differs from kustomize\n chart:     %s\n kustomize: %s", k, got, want)
		}
	}
}

// TestSchemaRefusesBadDNSEgressValues checks that values.schema.json (not a template) refuses an unknown mode,
// an empty or malformed resolver list and unknown keys.
func TestSchemaRefusesBadDNSEgressValues(t *testing.T) {
	cases := map[string]refusalCase{
		"unknown mode":            {"networkPolicy.dns.mode=nodelocal", "mode"},
		"empty mode":              {"networkPolicy.dns.mode=", "mode"},
		"empty resolver list":     {cidrsMode + "," + cidrsKey + "={}", cidrsValue},
		"resolver list removed":   {cidrsMode + "," + cidrsKey + "=null", cidrsValue},
		"host name, not a CIDR":   {cidrsKey + "={node-local-dns}", cidrsValue},
		"address without a mask":  {cidrsKey + "={169.254.20.10}", cidrsValue},
		"IPv4 mask out of range":  {cidrsKey + "={10.0.0.0/33}", cidrsValue},
		"IPv4 octet out of range": {cidrsKey + "={256.0.0.1/32}", cidrsValue},
		"duplicate resolver":      {cidrsKey + "={10.0.0.1/32,10.0.0.1/32}", cidrsValue},
		"unknown dns key":         {"networkPolicy.dns.selector=x", unknownField},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if out := helmFails(t, c.set); !strings.Contains(out, c.want) {
				t.Errorf("error does not mention %q:\n%s", c.want, out)
			}
		})
	}
}
