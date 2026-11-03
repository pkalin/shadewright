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

// Package netpol checks the NetworkPolicies in config/network-policy (design 10.6, threat model S5): the
// operator namespace denies everything by default and each component is allowed only DNS and HTTPS.
package netpol

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/yaml"
)

const (
	analyzer = "analyzer"
	executor = "executor"
)

func read(t *testing.T, path string, into any) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "config", path))
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.UnmarshalStrict(b, into); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

func policy(t *testing.T, name string) networkingv1.NetworkPolicy {
	t.Helper()
	var p networkingv1.NetworkPolicy
	read(t, "network-policy/"+name+".yaml", &p)
	return p
}

func podLabels(t *testing.T, file string) labels.Set {
	t.Helper()
	// The executor Deployment lives in manager.yaml, after the Namespace.
	files := map[string]string{analyzer: "analyzer.yaml", executor: "manager.yaml"}
	b, err := os.ReadFile(filepath.Join("..", "..", "config", "manager", files[file]))
	if err != nil {
		t.Fatal(err)
	}
	for doc := range strings.SplitSeq(string(b), "\n---\n") {
		var d appsv1.Deployment
		if err := yaml.Unmarshal([]byte(doc), &d); err == nil && d.Kind == "Deployment" {
			return d.Spec.Template.Labels
		}
	}
	t.Fatalf("no Deployment for %s", file)
	return nil
}

func selects(t *testing.T, sel metav1.LabelSelector, l labels.Set) bool {
	t.Helper()
	s, err := metav1.LabelSelectorAsSelector(&sel)
	if err != nil {
		t.Fatal(err)
	}
	return s.Matches(l)
}

func TestDefaultDenyCoversEveryPodInBothDirections(t *testing.T) {
	p := policy(t, "default-deny")
	if len(p.Spec.PodSelector.MatchLabels) != 0 || len(p.Spec.PodSelector.MatchExpressions) != 0 {
		t.Fatal("default-deny must select every pod in the namespace")
	}
	if len(p.Spec.Ingress) != 0 || len(p.Spec.Egress) != 0 {
		t.Fatal("default-deny must allow nothing")
	}
	got := map[networkingv1.PolicyType]bool{}
	for _, pt := range p.Spec.PolicyTypes {
		got[pt] = true
	}
	if !got[networkingv1.PolicyTypeIngress] || !got[networkingv1.PolicyTypeEgress] {
		t.Fatalf("default-deny must list Ingress and Egress, got %v", p.Spec.PolicyTypes)
	}
}

func TestEachComponentHasItsOwnEgressPolicy(t *testing.T) {
	for c, otherName := range map[string]string{analyzer: executor, executor: analyzer} {
		t.Run(c, func(t *testing.T) {
			p := policy(t, "allow-"+c+"-egress")
			types := p.Spec.PolicyTypes
			if len(types) != 1 || types[0] != networkingv1.PolicyTypeEgress || len(p.Spec.Ingress) != 0 {
				t.Fatalf("%s policy must be egress only: %v", c, p.Spec.PolicyTypes)
			}
			if !selects(t, p.Spec.PodSelector, podLabels(t, c)) {
				t.Fatalf("policy does not select the %s pod", c)
			}
			if selects(t, p.Spec.PodSelector, podLabels(t, otherName)) {
				t.Fatalf("policy for %s also selects the %s pod", c, otherName)
			}

			var dns, https bool
			for _, r := range p.Spec.Egress {
				ports := map[int32]bool{}
				for _, port := range r.Ports {
					if port.Protocol == nil {
						t.Fatalf("every port needs an explicit protocol: %+v", port)
					}
					ports[port.Port.IntVal] = true
				}
				switch {
				case ports[53]:
					dns = true
					if len(ports) != 1 {
						t.Fatalf("the DNS rule may only open 53: %v", ports)
					}
					for _, to := range r.To {
						if to.IPBlock != nil || to.PodSelector == nil || to.NamespaceSelector == nil {
							t.Fatalf("DNS must be limited to the cluster DNS pods: %+v", to)
						}
					}
				case ports[443]:
					https = true
					if len(ports) > 2 || (len(ports) == 2 && !ports[6443]) {
						t.Fatalf("outside the cluster only 443 (and 6443 for the API server) may open: %v", ports)
					}
					for _, port := range r.Ports {
						if *port.Protocol != corev1.ProtocolTCP {
							t.Fatalf("HTTPS must be TCP: %+v", port)
						}
					}
				default:
					t.Fatalf("unexpected egress rule: %+v", r)
				}
			}
			if !dns || !https {
				t.Fatalf("need a DNS rule and an HTTPS rule, got dns=%v https=%v", dns, https)
			}
		})
	}
}

func TestMetricsIngressIsOnlyThePortAndOnlyFromLabeledNamespaces(t *testing.T) {
	p := policy(t, "allow-metrics-traffic")
	if len(p.Spec.PolicyTypes) != 1 || p.Spec.PolicyTypes[0] != networkingv1.PolicyTypeIngress || len(p.Spec.Egress) != 0 {
		t.Fatalf("metrics policy must be ingress only: %v", p.Spec.PolicyTypes)
	}
	if len(p.Spec.Ingress) != 1 || len(p.Spec.Ingress[0].Ports) != 1 || p.Spec.Ingress[0].Ports[0].Port.IntVal != 8443 {
		t.Fatalf("metrics policy must open 8443 only: %+v", p.Spec.Ingress)
	}
	from := p.Spec.Ingress[0].From
	if len(from) != 1 || from[0].NamespaceSelector == nil ||
		from[0].NamespaceSelector.MatchLabels["metrics"] != "enabled" {
		t.Fatalf("metrics must come only from namespaces labeled metrics=enabled: %+v", from)
	}
	for _, c := range []string{analyzer, executor} {
		if !selects(t, p.Spec.PodSelector, podLabels(t, c)) {
			t.Fatalf("metrics policy does not select the %s pod", c)
		}
	}
}

func TestOnlyMetricsAllowsIngress(t *testing.T) {
	for _, n := range []string{"default-deny", "allow-analyzer-egress", "allow-executor-egress"} {
		if p := policy(t, n); len(p.Spec.Ingress) != 0 {
			t.Fatalf("%s allows ingress", n)
		}
	}
}
