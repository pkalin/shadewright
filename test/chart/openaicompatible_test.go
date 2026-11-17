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

	"github.com/pkalin/shadewright/internal/providers/model/openaicompatible"
)

const endpointPrefix = "analyzer.analysis.openAICompatible.endpoints[0]."
const compatibleKey = endpointPrefix + "apiKey.secretName=model-key"

func compatibleSet() []string {
	return []string{"analyzer.analysis.triageModel=m", "analyzer.analysis.disableFoundry=true",
		endpointPrefix + "url=https://api.openai.com/v1", endpointPrefix + "inferenceAPI=Responses"}
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
		got[0].API != openaicompatible.APIResponses ||
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
			out, err := exec.Command(tool(t, "HELM", "helm"), args...).CombinedOutput()
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
		for _, raw := range []string{"https://user@api.openai.com/v1",
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
			out, err := exec.Command(tool(t, "HELM", "helm"), args...).CombinedOutput()
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
			"analyzer.analysis.openAICompatible.endpoints[1].inferenceAPI=ChatCompletions")
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
	for i, name := range []string{"model", "secondary-model"} {
		p := fmt.Sprintf("analyzer.analysis.openAICompatible.endpoints[%d].", i)
		set = append(set, p+"url=https://"+name+".demo.svc.cluster.local/v1", p+"inferenceAPI=Responses",
			p+"service.namespace=demo", p+"service.podSelector.app="+name, fmt.Sprintf("%sservice.port=%d", p, 8443+i))
	}
	r := compatibleRender(t, set...)
	k := "NetworkPolicy/" + operatorNS + "/shadewright-allow-analyzer-egress"
	before, after := nonDNSRules(t, k, base), nonDNSRules(t, k, r)
	if len(after) != len(before)+2 || !reflect.DeepEqual(after[:len(before)], before) {
		t.Fatal("Service declarations changed other egress")
	}
	for i, name := range []string{"model", "secondary-model"} {
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
				if len(rule.To) != 1 || rule.To[0].IPBlock == nil {
					t.Fatal("public port lacks its IP peer")
				}
			}
		}
	}
	if !found {
		t.Fatal("declared public port is blocked")
	}
}
