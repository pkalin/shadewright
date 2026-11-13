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

// Package netpol renders config/default with the kustomize API and checks every NetworkPolicy in the build
// (design 10.6, threat model S5): the operator namespace denies everything by default, each component is
// allowed only DNS and HTTPS, and nothing but the metrics port is open for ingress.
package netpol

import (
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/kyaml/filesys"
	"sigs.k8s.io/yaml"
)

const component = "app.kubernetes.io/component"

type build struct {
	policies    []networkingv1.NetworkPolicy
	deployments []appsv1.Deployment
}

var (
	once     sync.Once
	rendered build
	buildErr error
)

// render builds config/default once, the way `kustomize build config/default` does.
func render(t *testing.T) build {
	t.Helper()
	once.Do(func() {
		dir, err := filepath.Abs(filepath.Join("..", "..", "config", "default"))
		if err != nil {
			buildErr = err
			return
		}
		k := krusty.MakeKustomizer(krusty.MakeDefaultOptions())
		m, err := k.Run(filesys.MakeFsOnDisk(), dir)
		if err != nil {
			buildErr = err
			return
		}
		out, err := m.AsYaml()
		if err != nil {
			buildErr = err
			return
		}
		for doc := range strings.SplitSeq(string(out), "\n---\n") {
			var meta metav1.TypeMeta
			if err := yaml.Unmarshal([]byte(doc), &meta); err != nil {
				buildErr = err
				return
			}
			switch meta.Kind {
			case "NetworkPolicy":
				var p networkingv1.NetworkPolicy
				if err := yaml.UnmarshalStrict([]byte(doc), &p); err != nil {
					buildErr = err
					return
				}
				rendered.policies = append(rendered.policies, p)
			case "Deployment":
				var d appsv1.Deployment
				if err := yaml.Unmarshal([]byte(doc), &d); err != nil {
					buildErr = err
					return
				}
				rendered.deployments = append(rendered.deployments, d)
			}
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return rendered
}

func selects(t *testing.T, sel metav1.LabelSelector, l map[string]string) bool {
	t.Helper()
	s, err := metav1.LabelSelectorAsSelector(&sel)
	if err != nil {
		t.Fatal(err)
	}
	return s.Matches(labels.Set(l))
}

func has(p networkingv1.NetworkPolicy, t networkingv1.PolicyType) bool {
	return slices.Contains(p.Spec.PolicyTypes, t)
}

func isDefaultDeny(p networkingv1.NetworkPolicy) bool {
	return len(p.Spec.PodSelector.MatchLabels) == 0 && len(p.Spec.PodSelector.MatchExpressions) == 0 &&
		has(p, networkingv1.PolicyTypeIngress) && has(p, networkingv1.PolicyTypeEgress) &&
		len(p.Spec.Ingress) == 0 && len(p.Spec.Egress) == 0
}

func TestBuildEmitsPoliciesAndDeployments(t *testing.T) {
	b := render(t)
	if len(b.policies) == 0 {
		t.Fatal("config/default emits no NetworkPolicy: is ../network-policy still listed?")
	}
	if len(b.deployments) != 3 {
		t.Fatalf("expected the analyzer, executor and verifier Deployments, got %d", len(b.deployments))
	}
	for _, d := range b.deployments {
		if d.Spec.Template.Labels[component] == "" {
			t.Fatalf("Deployment %s has no %s pod label", d.Name, component)
		}
	}
}

func TestDefaultDenyCoversEveryPodInBothDirections(t *testing.T) {
	b := render(t)
	var found bool
	for _, p := range b.policies {
		if isDefaultDeny(p) {
			found = true
		}
	}
	if !found {
		t.Fatal("the build has no default-deny policy (empty selector, Ingress and Egress, no rules)")
	}
}

// Every policy that opens something must be one of the documented rules: DNS to kube-dns, TCP 443 and 6443 to
// any address, and ingress on 8443 from namespaces labeled metrics=enabled. Anything else, an allow-all rule
// included, fails here.
func TestNoPolicyAllowsMoreThanTheDocumentedRules(t *testing.T) {
	for _, p := range render(t).policies {
		t.Run(p.Name, func(t *testing.T) {
			for _, r := range p.Spec.Egress {
				if !has(p, networkingv1.PolicyTypeEgress) {
					t.Fatal("egress rules without the Egress policy type")
				}
				checkEgressRule(t, r)
			}
			for _, r := range p.Spec.Ingress {
				if !has(p, networkingv1.PolicyTypeIngress) {
					t.Fatal("ingress rules without the Ingress policy type")
				}
				checkIngressRule(t, r)
			}
		})
	}
}

func checkEgressRule(t *testing.T, r networkingv1.NetworkPolicyEgressRule) {
	t.Helper()
	if len(r.Ports) == 0 || len(r.To) == 0 {
		t.Fatalf("egress rule without ports or destinations allows everything: %+v", r)
	}
	ports := map[int32]bool{}
	for _, port := range r.Ports {
		if port.Protocol == nil || port.Port == nil || port.Port.IntVal == 0 || port.EndPort != nil {
			t.Fatalf("every port needs an explicit protocol and a single number: %+v", port)
		}
		ports[port.Port.IntVal] = true
	}
	switch {
	case ports[53] && len(ports) == 1:
		for _, to := range r.To {
			if to.IPBlock != nil || to.PodSelector == nil || to.NamespaceSelector == nil ||
				to.PodSelector.MatchLabels["k8s-app"] != "kube-dns" ||
				to.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "kube-system" {
				t.Fatalf("DNS must be limited to the cluster DNS pods: %+v", to)
			}
		}
	case ports[443] && len(ports) <= 2 && (len(ports) == 1 || ports[6443]):
		for _, port := range r.Ports {
			if *port.Protocol != "TCP" {
				t.Fatalf("HTTPS must be TCP: %+v", port)
			}
		}
		for _, to := range r.To {
			if to.IPBlock == nil || to.IPBlock.CIDR != "0.0.0.0/0" || len(to.IPBlock.Except) != 0 ||
				to.PodSelector != nil || to.NamespaceSelector != nil {
				t.Fatalf("unexpected destination for HTTPS: %+v", to)
			}
		}
	default:
		t.Fatalf("egress rule outside the documented ones: %+v", r)
	}
}

func checkIngressRule(t *testing.T, r networkingv1.NetworkPolicyIngressRule) {
	t.Helper()
	if len(r.Ports) != 1 || r.Ports[0].Port == nil || r.Ports[0].Port.IntVal != 8443 || r.Ports[0].EndPort != nil {
		t.Fatalf("ingress may open 8443 only: %+v", r.Ports)
	}
	if len(r.From) != 1 {
		t.Fatalf("ingress must have exactly one source: %+v", r.From)
	}
	f := r.From[0]
	if f.IPBlock != nil || f.PodSelector != nil || f.NamespaceSelector == nil ||
		len(f.NamespaceSelector.MatchLabels) != 1 || f.NamespaceSelector.MatchLabels["metrics"] != "enabled" ||
		len(f.NamespaceSelector.MatchExpressions) != 0 {
		t.Fatalf("ingress must come only from namespaces labeled metrics=enabled: %+v", r.From)
	}
}

// Each Deployment's pods are selected by exactly one policy that grants egress (its own), and no policy that
// grants egress selects a pod it does not belong to.
func TestEachComponentHasItsOwnEgressPolicy(t *testing.T) {
	b := render(t)
	for _, d := range b.deployments {
		c := d.Spec.Template.Labels[component]
		t.Run(c, func(t *testing.T) {
			var n int
			for _, p := range b.policies {
				if len(p.Spec.Egress) == 0 || !selects(t, p.Spec.PodSelector, d.Spec.Template.Labels) {
					continue
				}
				n++
				if !slices.Equal(p.Spec.PolicyTypes, []networkingv1.PolicyType{networkingv1.PolicyTypeEgress}) ||
					len(p.Spec.Ingress) != 0 {
					t.Fatalf("%s must be egress only: %v", p.Name, p.Spec.PolicyTypes)
				}
				for _, other := range b.deployments {
					if other.Name != d.Name && selects(t, p.Spec.PodSelector, other.Spec.Template.Labels) {
						t.Fatalf("%s selects the pods of both %s and %s", p.Name, d.Name, other.Name)
					}
				}
				var dns, https bool
				for _, r := range p.Spec.Egress {
					for _, port := range r.Ports {
						dns = dns || port.Port.IntVal == 53
						https = https || port.Port.IntVal == 443
					}
				}
				if !dns || !https {
					t.Fatalf("%s needs a DNS rule and an HTTPS rule", p.Name)
				}
			}
			if n != 1 {
				t.Fatalf("expected exactly one egress allow policy selecting %s pods, got %d", c, n)
			}
		})
	}
}

func TestMetricsIngressSelectsBothComponents(t *testing.T) {
	b := render(t)
	var found bool
	for _, p := range b.policies {
		if len(p.Spec.Ingress) == 0 {
			continue
		}
		found = true
		for _, d := range b.deployments {
			if !selects(t, p.Spec.PodSelector, d.Spec.Template.Labels) {
				t.Fatalf("%s does not select the %s pods", p.Name, d.Name)
			}
		}
	}
	if !found {
		t.Fatal("no policy opens the metrics port")
	}
}
