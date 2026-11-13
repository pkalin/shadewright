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
	"strings"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
)

const promPort = 9090

var promOn = []string{
	"networkPolicy.analyzer.prometheus.enabled=true",
	"networkPolicy.analyzer.prometheus.namespace=prometheus",
}

// nonDNSRules returns the egress rules of a component's policy that do not open port 53.
func nonDNSRules(t *testing.T, key string, r rendered) []networkingv1.NetworkPolicyEgressRule {
	t.Helper()
	u := r.chart[key]
	if u == nil {
		t.Fatalf("%s not rendered", key)
	}
	var out []networkingv1.NetworkPolicyEgressRule
	for _, rule := range convert[networkingv1.NetworkPolicy](t, u).Spec.Egress {
		dns := false
		for _, p := range rule.Ports {
			dns = dns || (p.Port != nil && p.Port.IntValue() == 53)
		}
		if !dns {
			out = append(out, rule)
		}
	}
	return out
}

// TestEgressIsSeparatePerComponentAndTheAnalyzerHasNoExecutorOnlyRule: the default policies select one
// component each, and the analyzer's rules are a subset of the executor's (no rule meant for GitHub only).
func TestEgressIsSeparatePerComponentAndTheAnalyzerHasNoExecutorOnlyRule(t *testing.T) {
	r := defaultRender(t, withVerifier)
	for _, k := range []string{"shadewright-default-deny", "shadewright-allow-metrics-traffic"} {
		if r.chart["NetworkPolicy/"+operatorNS+"/"+k] == nil {
			t.Errorf("%s not rendered", k)
		}
	}
	egress := map[string]string{analyzerEgressKey: analyzerComp, executorEgressKey: executorComp,
		verifierEgressKey: verifierComp}
	for k, comp := range egress {
		np := convert[networkingv1.NetworkPolicy](t, r.chart[k])
		if got := np.Spec.PodSelector.MatchLabels["app.kubernetes.io/component"]; got != comp {
			t.Errorf("%s selects component %q, want %q", k, got, comp)
		}
	}
	exec := map[string]bool{}
	for _, rule := range nonDNSRules(t, executorEgressKey, r) {
		exec[asJSON(t, rule)] = true
	}
	for _, rule := range nonDNSRules(t, analyzerEgressKey, r) {
		if !exec[asJSON(t, rule)] {
			t.Errorf("analyzer has a rule the executor lacks: %s", asJSON(t, rule))
		}
	}
	// The verifier has the executor's rules, not one more: DNS (checked apart) and TCP 443 and 6443.
	if got, want := len(nonDNSRules(t, verifierEgressKey, r)), len(exec); got != want {
		t.Errorf("verifier has %d non-DNS egress rules, want the executor's %d", got, want)
	}
	for _, rule := range nonDNSRules(t, verifierEgressKey, r) {
		if !exec[asJSON(t, rule)] {
			t.Errorf("verifier has a rule the executor lacks: %s", asJSON(t, rule))
		}
	}
}

// TestPrometheusEgressIsForTheAnalyzerOnly: the optional rule reaches only the named Prometheus Pods on one
// port, and never appears in the executor's policy.
func TestPrometheusEgressIsForTheAnalyzerOnly(t *testing.T) {
	off := defaultRender(t)
	if a, e := len(nonDNSRules(t, analyzerEgressKey, off)), len(nonDNSRules(t, executorEgressKey, off)); a != e {
		t.Errorf("by default the analyzer has %d non-DNS rules and the executor %d, want equal", a, e)
	}
	on := defaultRender(t, promOn...)
	if a, e := len(nonDNSRules(t, analyzerEgressKey, on)), len(nonDNSRules(t, executorEgressKey, on)); a != e+1 {
		t.Fatalf("analyzer has %d non-DNS rules, executor %d, want one more", a, e)
	}
	var found []string
	for _, rule := range nonDNSRules(t, analyzerEgressKey, on) {
		if len(rule.Ports) == 1 && rule.Ports[0].Port.IntValue() == promPort {
			found = append(found, asJSON(t, rule))
		}
	}
	want := `{"ports":[{"protocol":"TCP","port":9090}],"to":[{"podSelector":{"matchLabels":` +
		`{"app.kubernetes.io/component":"server","app.kubernetes.io/name":"prometheus"}},` +
		`"namespaceSelector":{"matchLabels":{"kubernetes.io/metadata.name":"prometheus"}}}]}`
	if len(found) != 1 || found[0] != want {
		t.Errorf("Prometheus rule\n got:  %v\n want: %s", found, want)
	}
	for _, rule := range nonDNSRules(t, executorEgressKey, on) {
		if len(rule.Ports) == 1 && rule.Ports[0].Port.IntValue() == promPort {
			t.Errorf("executor got the Prometheus rule: %s", asJSON(t, rule))
		}
	}
}

func TestPrometheusEgressNeedsANamespaceAndValidValues(t *testing.T) {
	out := helmFails(t, "networkPolicy.analyzer.prometheus.enabled=true")
	if !strings.Contains(out, "prometheus.namespace") {
		t.Errorf("error does not name the namespace value:\n%s", out)
	}
	for name, set := range map[string]string{
		"port zero":     "networkPolicy.analyzer.prometheus.port=0",
		"port too high": "networkPolicy.analyzer.prometheus.port=70000",
		"bad namespace": "networkPolicy.analyzer.prometheus.namespace=Not_A_Name",
		"unknown key":   "networkPolicy.analyzer.prometheus.host=x",
	} {
		t.Run(name, func(t *testing.T) { helmFails(t, set) })
	}
}
