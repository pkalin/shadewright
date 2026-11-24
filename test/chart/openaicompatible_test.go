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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/shadewright-labs/shadewright/internal/providers/model/openaicompatible"
)

const serviceModelName = "model"

const endpointPrefix = "analyzer.analysis.openAICompatible.endpoints[0]."
const compatibleKey = endpointPrefix + "apiKey.secretName=model-key"

func compatibleSet() []string {
	return []string{"analyzer.analysis.triageModel=m", "analyzer.analysis.disableFoundry=true",
		endpointPrefix + "url=https://api.openai.com/v1", endpointPrefix + "inferenceAPI=Responses",
		endpointPrefix + "models[0]=m"}
}

// Compatible rendering needs Helm only. A missing Helm fails loudly.
func compatibleRender(t *testing.T, set ...string) rendered {
	t.Helper()
	helm := os.Getenv("HELM")
	if helm == "" {
		local := filepath.Join(repoRoot(t), "bin", "helm")
		if _, err := os.Stat(local); err == nil {
			helm = local
		} else {
			helm, _ = exec.LookPath("helm")
		}
	}
	if helm == "" {
		t.Fatal("compatible chart tests require Helm")
	}

	r := rendered{chart: map[string]*unstructured.Unstructured{}, tests: map[string]*unstructured.Unstructured{}}
	for _, u := range objects(t, run(t, repoRoot(t), helm, helmArgs(t, false, set...)...)) {
		if isTestHook(u) {
			r.tests[key(u)] = u
		} else {
			r.chart[key(u)] = u
		}
	}
	if len(r.chart) == 0 {
		t.Fatal("empty chart")
	}
	return r
}

func declaredArgs(t *testing.T, r rendered) []openaicompatible.Endpoint {
	t.Helper()
	for _, arg := range container(t, r, analyzerDeployment).Args {
		if raw, ok := strings.CutPrefix(arg, "--openai-compatible-endpoints="); ok {
			var out []openaicompatible.Endpoint
			if err := json.Unmarshal([]byte(raw), &out); err != nil {
				t.Fatal(err)
			}
			return out
		}
	}
	t.Fatal("no endpoint declarations")
	return nil
}

func TestCompatibleDeclaredEndpointRender(t *testing.T) {
	t.Setenv("KUSTOMIZE", "/unavailable-kustomize")
	set := append(compatibleSet(), compatibleKey)
	r := compatibleRender(t, set...)
	got := declaredArgs(t, r)
	if len(got) != 1 || got[0].URL != "https://api.openai.com/v1" ||
		got[0].API != openaicompatible.APIResponses || !reflect.DeepEqual(got[0].Models, []string{"m"}) ||
		got[0].KeyFile != "/var/run/secrets/shadewright/openai-compatible/0/api-key" {
		t.Fatalf("declarations: %+v", got)
	}
	a := deployment(t, r, analyzerDeployment)
	if a.Spec.Template.Labels["azure.workload.identity/use"] != "" {
		t.Fatal("compatible-only deployment uses Azure")
	}
	mounts := container(t, r, analyzerDeployment).VolumeMounts
	found := false
	for _, m := range mounts {
		if m.Name == "openai-compatible-key-0" {
			found = true
			if !m.ReadOnly || m.SubPath != "" || m.SubPathExpr != "" {
				t.Fatal("key mount cannot rotate")
			}
		}
	}
	if !found {
		t.Fatal("missing key mount")
	}
	base := compatibleRender(t)
	if !reflect.DeepEqual(rbacOnly(base.chart, false), rbacOnly(r.chart, false)) {
		t.Fatal("key changed RBAC")
	}
	for _, name := range []string{executorDeployment, verifierDeployment} {
		if !reflect.DeepEqual(deployment(t, base, name), deployment(t, r, name)) {
			t.Fatal("key changed another identity")
		}
	}
}

func TestCompatibleWithoutKeyOrAzure(t *testing.T) {
	r := compatibleRender(t, compatibleSet()...)
	if got := declaredArgs(t, r); len(got) != 1 || got[0].KeyFile != "" {
		t.Fatal("keyless declaration gained a key")
	}
}

func TestCompatibleVolumeSources(t *testing.T) {
	for _, skip := range []bool{false, true} {
		for _, source := range []string{
			"emptyDir.medium=Memory", "hostPath.path=/tmp", "projected.sources[0].secret.name=key",
			"secret.secretName=key,csi.driver=example.test", "secret={}", "csi={}",
		} {
			set := compatibleSet()
			for field := range strings.SplitSeq(source, ",") {
				set = append(set, endpointPrefix+"apiKey.volume."+field)
			}
			args := helmArgs(t, false, set...)
			if skip {
				args = append(args, "--skip-schema-validation")
			}
			out, err := exec.CommandContext(t.Context(), tool(t, "HELM", "helm"), args...).CombinedOutput()
			if err == nil || !strings.Contains(string(out), "openAICompatible") {
				t.Fatalf("source %s skip schema %v: %v %s", source, skip, err, out[:min(len(out), 1024)])
			}
		}
	}
	for _, source := range []string{"secret.secretName=key", "csi.driver=example.test"} {
		compatibleRender(t, append(compatibleSet(), endpointPrefix+"apiKey.volume."+source)...)
	}
}

func TestCompatibleChartDeclarationRefusals(t *testing.T) {
	for _, skip := range []bool{false, true} {
		for _, raw := range []string{"https://user" + "@" + "api.openai.com/v1",
			"https://@api.openai.com/v1",
			"https://api.openai.com/v1?x",
			"https://api.openai.com/v1?",
			"https://api.openai.com/v1#x",
			"https://api.openai.com/v1#",
			"https://api.openai.com/a/../v1",
			"https://api.openai.com/a/%2e%2e/v1",
			"https://api.anthropic.com/v1",
			"https://r.openai.azure.com/v1",
			"https://r.services.ai.azure.com/v1",
			"https://r.cognitiveservices.azure.com/v1"} {
			args := helmArgs(t, false, append(compatibleSet(), endpointPrefix+"url="+raw)...)
			if skip {
				args = append(args, "--skip-schema-validation")
			}
			out, err := exec.CommandContext(t.Context(), tool(t, "HELM", "helm"), args...).CombinedOutput()
			if err == nil || !strings.Contains(string(out), "openAICompatible") {
				t.Fatalf("invalid URL %q skip %v: %v %s", raw, skip, err, out[:min(len(out), 1024)])
			}
		}
	}
	// This exact non-Azure host is in both boundaries; neither the schema nor vendor exclusion can catch it.
	for _, key := range []bool{false, true} {
		set := append(compatibleSet(), "analyzer.analysis.endpointSuffixes={.api.openai.com}")
		if key {
			set = append(set, compatibleKey)
		}
		if out := helmFails(t, set...); !strings.Contains(out, "matches Foundry endpointSuffixes") {
			t.Fatalf("wrong overlap refusal: %s", out)
		}
	}
	for _, duplicate := range []string{
		"https://api.openai.com/v1", "HTTPS://API.OPENAI.COM:443/v1/", "https://api.openai.com/%76%31",
	} {
		set := append(compatibleSet(), "analyzer.analysis.openAICompatible.endpoints[1].url="+duplicate,
			"analyzer.analysis.openAICompatible.endpoints[1].inferenceAPI=ChatCompletions",
			"analyzer.analysis.openAICompatible.endpoints[1].models[0]=m")
		if out := helmFails(t, set...); !strings.Contains(out, "duplicate") {
			t.Fatalf("wrong duplicate refusal: %s", out)
		}
	}
	for _, set := range [][]string{
		append(compatibleSet(), compatibleKey, endpointPrefix+"apiKey.secretKey=../key"),
		append(compatibleSet(), compatibleKey, endpointPrefix+"apiKey.volume.csi.driver=example.test"),
		{"analyzer.extraArgs={--openai-compatible-endpoints=[]}"},
	} {
		helmFails(t, set...)
	}
}

func TestCompatibleServiceEgress(t *testing.T) {
	base := compatibleRender(t)
	set := make([]string, 0, 16)
	set = append(set, "analyzer.analysis.triageModel=m", "analyzer.analysis.disableFoundry=true")
	for i, name := range []string{serviceModelName, "secondary-model"} {
		p := fmt.Sprintf("analyzer.analysis.openAICompatible.endpoints[%d].", i)
		set = append(set, p+"url=https://"+name+".demo.svc.cluster.local/v1", p+"inferenceAPI=Responses", p+"models[0]=m",
			p+"service.namespace=demo", p+"service.podSelector.app="+name, fmt.Sprintf("%sservice.port=%d", p, 8443+i))
	}
	r := compatibleRender(t, set...)
	k := "NetworkPolicy/" + operatorNS + "/shadewright-allow-analyzer-egress"
	before, after := nonDNSRules(t, k, base), nonDNSRules(t, k, r)
	if len(after) != len(before)+2 || !reflect.DeepEqual(after[:len(before)], before) {
		t.Fatal("Service declarations changed other egress")
	}
	for i, name := range []string{serviceModelName, "secondary-model"} {
		rule := after[len(before)+i]
		if len(rule.To) != 1 || rule.To[0].IPBlock != nil ||
			rule.To[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "demo" ||
			!reflect.DeepEqual(rule.To[0].PodSelector.MatchLabels, map[string]string{"app": name}) ||
			len(rule.Ports) != 1 || rule.Ports[0].Port.IntValue() != 8443+i {
			t.Fatalf("incorrect Service rule: %+v", rule)
		}
	}
	for _, bad := range []string{
		"service.namespace=wrong", "service.port=0", "url=https://evil.example.com/v1", "service.podSelector=null",
	} {
		helmFails(t, append(set, endpointPrefix+bad)...)
	}
}

func TestCompatibleTwoEndpointKeys(t *testing.T) {
	set := append(compatibleSet(), compatibleKey,
		"analyzer.analysis.openAICompatible.endpoints[1].url=https://api.openai.com/tenant-b/v1",
		"analyzer.analysis.openAICompatible.endpoints[1].inferenceAPI=ChatCompletions",
		"analyzer.analysis.openAICompatible.endpoints[1].models[0]=m",
		"analyzer.analysis.openAICompatible.endpoints[1].apiKey.secretName=second-key")
	r := compatibleRender(t, set...)
	got := declaredArgs(t, r)
	if len(got) != 2 || got[0].KeyFile == got[1].KeyFile {
		t.Fatal("endpoint keys share a mount")
	}
	a := deployment(t, r, analyzerDeployment)
	secrets := map[string]bool{}
	for _, v := range a.Spec.Template.Spec.Volumes {
		if v.Secret != nil {
			secrets[v.Secret.SecretName] = true
		}
	}
	if !secrets["model-key"] || !secrets["second-key"] {
		t.Fatal("missing independent keys")
	}
}

func TestCompatibleDeclaredPortEgress(t *testing.T) {
	r := compatibleRender(t, append(compatibleSet(), endpointPrefix+"url=https://api.openai.com:8443/v1")...)
	k := "NetworkPolicy/" + operatorNS + "/shadewright-allow-analyzer-egress"
	rules := nonDNSRules(t, k, r)
	found := false
	for _, rule := range rules {
		for _, port := range rule.Ports {
			if port.Port != nil && port.Port.IntValue() == 8443 {
				found = true
				except := []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10", "169.254.0.0/16"}
				if len(rule.To) != 1 || rule.To[0].IPBlock == nil ||
					!reflect.DeepEqual(rule.To[0].IPBlock.Except, except) {
					t.Fatal("public port lacks its IP peer")
				}
			}
		}
	}
	if !found {
		t.Fatal("declared public port is blocked")
	}
}

func TestCompatibleChartModelAndPrefixRefusals(t *testing.T) {
	for _, skip := range []bool{false, true} {
		for _, tc := range []struct{ name, value, reason string }{
			{"prefix", "https://api.openai.com/v1/team", "path prefix"},
			{"empty models", "", "models"},
			{"invalid model", "bad/name", "model"},
		} {
			t.Run(fmt.Sprintf("%s/schema-skipped=%v", tc.name, skip), func(t *testing.T) {
				set := append(compatibleSet(), endpointPrefix+"models[0]=m")
				switch {
				case tc.name == "prefix":
					p := "analyzer.analysis.openAICompatible.endpoints[1]."
					set = append(set, p+"url="+tc.value, p+"inferenceAPI=Responses", p+"models[0]=m")
				case tc.value == "":
					set = append(set, endpointPrefix+"models=null")
				default:
					set = append(set, endpointPrefix+"models[0]="+tc.value)
				}
				args := helmArgs(t, false, set...)
				if skip {
					args = append(args, "--skip-schema-validation")
				}
				out, err := exec.CommandContext(t.Context(), tool(t, "HELM", "helm"), args...).CombinedOutput()
				if err == nil || !strings.Contains(string(out), tc.reason) {
					t.Fatalf("wrong refusal: %v %.1200s", err, out)
				}
			})
		}
	}
}

const inClusterKey = "inClusterCIDRs"
const inClusterValue = "analyzer.analysis.openAICompatible." + inClusterKey

// Declared in-cluster ranges reach the analyzer as one flag, and the IPv4 ones are excepted from the egress
// rule of a declared port other than 443 like the private ranges (ADR-0042).
func TestCompatibleInClusterCIDRs(t *testing.T) {
	set := append(compatibleSet(), endpointPrefix+"url=https://api.openai.com:8443/v1",
		inClusterValue+"={198.51.100.0/24,2001:db8:1::/48,11.0.0.0/8}")
	r := compatibleRender(t, set...)
	flag := ""
	for _, arg := range container(t, r, analyzerDeployment).Args {
		if v, ok := strings.CutPrefix(arg, "--openai-compatible-in-cluster-cidrs="); ok {
			flag = v
		}
	}
	if flag != "198.51.100.0/24,2001:db8:1::/48,11.0.0.0/8" {
		t.Fatalf("in-cluster flag = %q", flag)
	}
	if _, err := openaicompatible.ParseInClusterRanges(flag); err != nil {
		t.Fatalf("the analyzer refuses the rendered flag: %v", err)
	}
	k := "NetworkPolicy/" + operatorNS + "/shadewright-allow-analyzer-egress"
	want := []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10", "169.254.0.0/16",
		"198.51.100.0/24", "11.0.0.0/8"}
	found := false
	for _, rule := range nonDNSRules(t, k, r) {
		for _, port := range rule.Ports {
			if port.Port != nil && port.Port.IntValue() == 8443 {
				found = true
				if len(rule.To) != 1 || rule.To[0].IPBlock == nil || rule.To[0].IPBlock.CIDR != "0.0.0.0/0" ||
					!reflect.DeepEqual(rule.To[0].IPBlock.Except, want) {
					t.Fatalf("declared port rule = %+v", rule.To)
				}
			}
		}
	}
	if !found {
		t.Fatal("declared port rule missing")
	}

	// Without the value: no flag.
	for _, arg := range container(t, compatibleRender(t, compatibleSet()...), analyzerDeployment).Args {
		if strings.HasPrefix(arg, "--openai-compatible-in-cluster-cidrs") {
			t.Fatalf("flag rendered without ranges: %s", arg)
		}
	}
}

func TestCompatibleInClusterCIDRsRefused(t *testing.T) {
	for _, skip := range []bool{false, true} {
		for _, tc := range []struct{ name, value, reason string }{
			{"not a cidr", "{pods}", inClusterKey},
			{"no prefix length", "{198.51.100.0}", inClusterKey},
			{"prefix too long", "{198.51.100.0/33}", inClusterKey},
			{"octet too large", "{198.51.300.0/24}", inClusterKey},
			{"mapped ipv6", "{::ffff:198.51.100.0/120}", inClusterKey},
			{"ipv6 prefix too long", "{2001:db8::/129}", inClusterKey},
		} {
			t.Run(fmt.Sprintf("%s/schema-skipped=%v", tc.name, skip), func(t *testing.T) {
				args := helmArgs(t, false, append(compatibleSet(), inClusterValue+"="+tc.value)...)
				if skip {
					args = append(args, "--skip-schema-validation")
				}
				out, err := exec.CommandContext(t.Context(), tool(t, "HELM", "helm"), args...).CombinedOutput()
				if err == nil || !strings.Contains(string(out), tc.reason) {
					t.Fatalf("wrong refusal: %v %.1200s", err, out)
				}
			})
		}
	}
	// Ranges without a compatible endpoint have nothing to guard.
	args := helmArgs(t, false, "analyzer.analysis.triageModel=m", "analyzer.analysis.anthropic.apiKey.secretName=k",
		inClusterValue+"={198.51.100.0/24}")
	args = append(args, "--skip-schema-validation")
	out, err := exec.CommandContext(t.Context(), tool(t, "HELM", "helm"), args...).CombinedOutput()
	if err == nil || !strings.Contains(string(out), "needs analyzer.analysis.openAICompatible.endpoints") {
		t.Fatalf("ranges without endpoints rendered: %v %.1200s", err, out)
	}
}

// One rule for the in-cluster ranges: the analyzer's flag parser and the chart, with its schema and without
// it, give the same verdict for each input, and the chart's refusal names the value (ADR-0042).
func TestCompatibleInClusterCIDRsSameVerdict(t *testing.T) {
	for _, tc := range []struct {
		value string
		ok    bool
	}{
		{"198.51.100.0/24", true},
		{"11.0.0.0/8", true},
		{"128.0.0.0/1", true},
		{"203.0.113.7/32", true},
		{"2001:db8:1::/48", true},
		{"2a0b:4000::/32", true},
		{"64:ff9b:1::/48", true},
		{"1::/16", true},
		{"2001:db8::1/128", true},
		{"1:0:1:1:1:1:1:1/128", true},
		{"1::1:0:0:1:1/128", true},
		{"1:2:3:4:5:6:7:8/128", true},
		{"0.0.0.0/0", false},
		{"11.0.0.0/0", false},
		{"::/0", false},
		{"11.0.0.1/8", false},
		{"203.0.113.7/24", false},
		{"2001:db8::1/32", false},
		{"2a0b:4000::8000/113", true},
		{"2a0b:4000::4000/113", false},
		{"::ffff:a00:0/104", false},
		{"::ffff:10.0.0.0/104", false},
		{"64:ff9b::c633:6400/120", false},
		{"64:ff9b::198.51.100.0/120", false},
		{"::c633:6400/120", false},
		{"2002:c633:6400::/40", false},
		{"2000::/3", false},
		{"::/64", false},
		{"2001:DB8::/32", false},
		{"2001:0db8::/32", false},
		{"2001:db8:0::/48", false},
		{"2001:db8:0:0:0:0:0:0/32", false},
		{"1::1:1:1:1:1:1/128", false},
		{"1:0:0:1::1:1/128", false},
		{"2001:db8::1.2.3.4/128", false},
		{"1:2:3:4:5:6:7::8/128", false},
		{"1:2:3:4:5:6:7:8:9/128", false},
		{":::/64", false},
		{"1::2::/64", false},
		{"fe80::%eth0/64", false},
		{"2001:db8::/129", false},
		{"198.51.100.0/33", false},
		{"198.51.300.0/24", false},
		{"010.0.0.0/8", false},
		{"11.0.0.0/08", false},
		{"198.51.100.0", false},
		{"pods", false},
	} {
		t.Run(tc.value, func(t *testing.T) {
			t.Parallel()
			if _, err := openaicompatible.ParseInClusterRanges(tc.value); (err == nil) != tc.ok {
				t.Errorf("flag verdict %v, want accepted=%v", err, tc.ok)
			}
			for _, skip := range []bool{false, true} {
				args := helmArgs(t, false, append(compatibleSet(), inClusterValue+"={"+tc.value+"}")...)
				if skip {
					args = append(args, "--skip-schema-validation")
				}
				out, err := exec.CommandContext(t.Context(), tool(t, "HELM", "helm"), args...).CombinedOutput()
				if (err == nil) != tc.ok {
					t.Errorf("chart verdict (schema skipped=%v) %v, want accepted=%v: %.600s", skip, err, tc.ok, out)
				}
				if err != nil && skip && !strings.Contains(string(out), fmt.Sprintf("%q", tc.value)) {
					t.Errorf("the chart's refusal does not name the value: %.600s", out)
				}
			}
		})
	}
}

// A key belongs to one declaration (decision 7 of #834): two compatible endpoints that mount one key of one
// Secret, or an endpoint that mounts the Anthropic key, are refused at render, and the error names both. One
// Secret with different keys is accepted.
func TestCompatibleKeySourceBelongsToOneDeclaration(t *testing.T) {
	second := []string{"analyzer.analysis.openAICompatible.endpoints[1].url=https://gateway.example.com/v1",
		"analyzer.analysis.openAICompatible.endpoints[1].inferenceAPI=ChatCompletions",
		"analyzer.analysis.openAICompatible.endpoints[1].models[0]=m"}
	const key1 = "analyzer.analysis.openAICompatible.endpoints[1].apiKey."
	for name, set := range map[string][]string{
		"two endpoints, one Secret key": append(append(compatibleSet(), second...), compatibleKey,
			key1+"secretName=model-key"),
		"two endpoints, Secret and secret volume": append(append(compatibleSet(), second...), compatibleKey,
			key1+"volume.secret.secretName=model-key"),
		"endpoint and Anthropic key": append(compatibleSet(), endpointPrefix+"apiKey.secretName=anthropic-key",
			"analyzer.analysis.anthropic.apiKey.secretName=anthropic-key"),
		"endpoint and Anthropic key with a named key": append(compatibleSet(), endpointPrefix+"apiKey.secretName=shared",
			endpointPrefix+"apiKey.secretKey=k", "analyzer.analysis.anthropic.apiKey.secretName=shared",
			"analyzer.analysis.anthropic.apiKey.secretKey=k"),
		// A secret volume's items map a Secret key to another file name: the ID follows the Secret key.
		"two endpoints, one key through items": append(append(compatibleSet(), second...),
			endpointPrefix+"apiKey.volume.secret.secretName=shared",
			endpointPrefix+"apiKey.volume.secret.items[0].key=anthropic",
			endpointPrefix+"apiKey.volume.secret.items[0].path=k", endpointPrefix+"apiKey.fileName=k",
			key1+"volume.secret.secretName=shared", key1+"volume.secret.items[0].key=other",
			key1+"volume.secret.items[0].path=j", key1+"volume.secret.items[1].key=anthropic",
			key1+"volume.secret.items[1].path=k2", key1+"fileName=k2"),
		"endpoint through items and Anthropic key": append(compatibleSet(),
			endpointPrefix+"apiKey.volume.secret.secretName=shared",
			endpointPrefix+"apiKey.volume.secret.items[0].key=anthropic",
			endpointPrefix+"apiKey.volume.secret.items[0].path=k", endpointPrefix+"apiKey.fileName=k",
			"analyzer.analysis.anthropic.apiKey.secretName=shared",
			"analyzer.analysis.anthropic.apiKey.secretKey=anthropic"),
		"endpoint Secret key and Anthropic through items": append(compatibleSet(),
			endpointPrefix+"apiKey.secretName=shared", endpointPrefix+"apiKey.secretKey=anthropic",
			"analyzer.analysis.anthropic.apiKey.volume.secret.secretName=shared",
			"analyzer.analysis.anthropic.apiKey.volume.secret.items[0].key=anthropic",
			"analyzer.analysis.anthropic.apiKey.volume.secret.items[0].path=api-key"),
	} {
		t.Run(name, func(t *testing.T) {
			out := helmFails(t, set...)
			if !strings.Contains(out, "belongs to one declaration") || !strings.Contains(out, "endpoints[0]") {
				t.Fatalf("wrong refusal: %s", out)
			}
		})
	}
	for name, set := range map[string][]string{
		"one Secret, two keys": append(append(compatibleSet(), second...), compatibleKey, key1+"secretName=model-key",
			key1+"secretKey=other-key"),
		"Anthropic key in another Secret": append(compatibleSet(), compatibleKey,
			"analyzer.analysis.anthropic.apiKey.secretName=anthropic-key"),
		// The file name of one declaration is the Secret key of the other, but items map it to another key.
		"items map one file name to another key": append(compatibleSet(),
			endpointPrefix+"apiKey.volume.secret.secretName=shared",
			endpointPrefix+"apiKey.volume.secret.items[0].key=compatible",
			endpointPrefix+"apiKey.volume.secret.items[0].path=anthropic", endpointPrefix+"apiKey.fileName=anthropic",
			"analyzer.analysis.anthropic.apiKey.secretName=shared",
			"analyzer.analysis.anthropic.apiKey.secretKey=anthropic"),
	} {
		t.Run(name, func(t *testing.T) { compatibleRender(t, set...) })
	}
	// items that do not name the key file leave its Secret key unknown, and the file would not exist.
	out := helmFails(t, append(compatibleSet(), endpointPrefix+"apiKey.volume.secret.secretName=shared",
		endpointPrefix+"apiKey.volume.secret.items[0].key=anthropic",
		endpointPrefix+"apiKey.volume.secret.items[0].path=other", endpointPrefix+"apiKey.fileName=k")...)
	if !strings.Contains(out, `no item with path "k"`) {
		t.Fatalf("wrong refusal: %s", out)
	}
	// An item path is compared cleaned: the kubelet writes "./api-key" as "api-key", and an Anthropic-only
	// install with such a path rendered before the key source was resolved through items.
	defaultRender(t, "analyzer.analysis.anthropic.apiKey.volume.secret.secretName=anthropic-key",
		"analyzer.analysis.anthropic.apiKey.volume.secret.items[0].key=k",
		"analyzer.analysis.anthropic.apiKey.volume.secret.items[0].path=./api-key")
}
