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
	"fmt"
	"os/exec"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const compatibleKey = "analyzer.analysis.openAICompatible.apiKey.secretName=model-key"
const compatibleHost = "analyzer.analysis.openAICompatible.apiKey.host=api.openai.com"

func TestCompatibleKeyAndNoAzure(t *testing.T) {
	base := render(t, false)
	set := []string{compatibleKey, compatibleHost, "analyzer.analysis.disableFoundry=true",
		"analyzer.analysis.openAICompatible.endpointSuffixes={.api.openai.com}", "analyzer.analysis.triageModel=gpt-6.1-sol"}
	r := render(t, false, set...)
	if !reflect.DeepEqual(rbacOnly(base.chart, false), rbacOnly(r.chart, false)) {
		t.Fatal("key added RBAC")
	}
	for _, name := range []string{executorDeployment, verifierDeployment} {
		if !reflect.DeepEqual(deployment(t, base, name), deployment(t, r, name)) {
			t.Errorf("key changed %s", name)
		}
	}
	a := deployment(t, r, analyzerDeployment)
	if a.Spec.Template.Labels["azure.workload.identity/use"] != "" {
		t.Fatal("compatible-only install uses Azure")
	}
	c := a.Spec.Template.Spec.Containers[0]
	for _, want := range []string{"--disable-foundry=true", "--openai-compatible-api-key-host=api.openai.com",
		"--openai-compatible-api-key-file=/var/run/secrets/shadewright/openai-compatible/api-key",
		"--openai-compatible-endpoint-suffixes=.api.openai.com"} {
		if !slices.Contains(c.Args, want) {
			t.Errorf("missing arg %s", want)
		}
	}
	found := false
	for _, m := range c.VolumeMounts {
		if m.Name == "openai-compatible-api-key" {
			found = true
			if !m.ReadOnly || m.SubPath != "" || m.SubPathExpr != "" {
				t.Fatal("key mount does not rotate")
			}
		}
	}
	if !found {
		t.Fatal("no key mount")
	}
}

func TestCompatibleChartKeyRefusals(t *testing.T) {
	for name, set := range map[string][]string{
		"no host":   {compatibleKey},
		"no source": {compatibleHost},
		"both sources": {compatibleKey, compatibleHost,
			"analyzer.analysis.openAICompatible.apiKey.volume.emptyDir.medium=Memory"},
		"unsafe filename":      {compatibleKey, compatibleHost, "analyzer.analysis.openAICompatible.apiKey.secretKey=../key"},
		"reserved suffix flag": {"analyzer.extraArgs={--openai-compatible-endpoint-suffixes=.api.openai.com}"},
		"reserved flag":        {"analyzer.extraArgs={--openai-compatible-api-key-file=/key}"},
	} {
		t.Run(name, func(t *testing.T) {
			if out := helmFails(t, set...); !strings.Contains(out, "openAICompatible") &&
				!strings.Contains(out, "openai-compatible") {
				t.Fatalf("unexpected error: %s", out)
			}
		})
	}
}

func TestCompatibleWithoutKeyOrAzure(t *testing.T) {
	r := render(t, false, "analyzer.analysis.disableFoundry=true",
		"analyzer.analysis.openAICompatible.endpointSuffixes={.model.demo.svc.cluster.local}",
		"analyzer.analysis.openAICompatible.serviceHosts[0].host=model.demo.svc.cluster.local",
		"analyzer.analysis.openAICompatible.serviceHosts[0].namespace=demo",
		"analyzer.analysis.openAICompatible.serviceHosts[0].podSelector.app=model",
		"analyzer.analysis.openAICompatible.serviceHosts[0].port=8443",
		"analyzer.analysis.triageModel=local-model")
	c := container(t, r, analyzerDeployment)
	for _, arg := range c.Args {
		if strings.HasPrefix(arg, "--openai-compatible-api-key") {
			t.Fatal("keyless install has key flags")
		}
	}
	if !slices.Contains(c.Args, "--openai-compatible-service-hosts=model.demo.svc.cluster.local") {
		t.Fatal("no Service host flag")
	}
}

// Key sources are restricted even when Helm's schema validation is disabled.
func TestCompatibleVolumeSources(t *testing.T) {
	for _, skipSchema := range []bool{false, true} {
		for _, source := range []string{
			"emptyDir.medium=Memory", "hostPath.path=/tmp", "projected.sources[0].secret.name=key",
			"secret.secretName=key,csi.driver=example.test",
		} {
			args := helmArgs(t, false, compatibleHost, "analyzer.analysis.triageModel=m",
				"analyzer.analysis.openAICompatible.endpointSuffixes={.api.openai.com}")
			for field := range strings.SplitSeq(source, ",") {
				args = append(args, "--set", "analyzer.analysis.openAICompatible.apiKey.volume."+field)
			}
			if skipSchema {
				args = append(args, "--skip-schema-validation")
			}
			cmd := exec.Command(tool(t, "HELM", "helm"), args...)
			if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "openAICompatible") {
				t.Fatalf("source %s schema skipped %v: %v %s", source, skipSchema, err, out)
			}
		}
	}
	for _, source := range []string{"secret.secretName=key", "csi.driver=example.test"} {
		render(t, false, compatibleHost, "analyzer.analysis.triageModel=m",
			"analyzer.analysis.openAICompatible.endpointSuffixes={.api.openai.com}",
			"analyzer.analysis.openAICompatible.apiKey.volume."+source)
	}
}

func TestCompatibleKeyHostAllowList(t *testing.T) {
	for _, host := range []string{
		"other.example.com", "api.anthropic.com", "r.services.ai.azure.com",
		"r.openai.azure.com", "r.cognitiveservices.azure.com",
	} {
		set := []string{compatibleKey, "analyzer.analysis.triageModel=m",
			"analyzer.analysis.openAICompatible.apiKey.host=" + host,
			"analyzer.analysis.openAICompatible.endpointSuffixes={.api.openai.com}"}
		if host != "other.example.com" {
			set[len(set)-1] = "analyzer.analysis.openAICompatible.endpointSuffixes={." + host + "}"
		}
		helmFails(t, set...)
	}
}

func TestCompatibleServiceEgress(t *testing.T) {
	base := render(t, false)
	set := make([]string, 0, 10)
	set = append(set, "analyzer.analysis.triageModel=m",
		"analyzer.analysis.openAICompatible.endpointSuffixes={.demo.svc.cluster.local}")
	for i, name := range []string{"model", "secondary-model"} {
		prefix := fmt.Sprintf("analyzer.analysis.openAICompatible.serviceHosts[%d].", i)
		set = append(set, prefix+"host="+name+".demo.svc.cluster.local", prefix+"namespace=demo",
			prefix+"podSelector.app="+name, prefix+"port="+strconv.Itoa(8443+i))
	}
	r := render(t, false, set...)
	policyKey := "NetworkPolicy/" + operatorNS + "/shadewright-allow-analyzer-egress"
	before, after := nonDNSRules(t, policyKey, base), nonDNSRules(t, policyKey, r)
	if len(after) != len(before)+2 || !reflect.DeepEqual(after[:len(before)], before) {
		t.Fatal("Service hosts changed other egress")
	}
	for i, name := range []string{"model", "secondary-model"} {
		rule := after[len(before)+i]
		if len(rule.To) != 1 || rule.To[0].IPBlock != nil ||
			rule.To[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "demo" ||
			!reflect.DeepEqual(rule.To[0].PodSelector.MatchLabels, map[string]string{"app": name}) ||
			len(rule.Ports) != 1 || rule.Ports[0].Port.IntValue() != 8443+i {
			t.Fatalf("rule %d does not select its exact pods and port: %+v", i, rule)
		}
	}
	for _, name := range []string{executorDeployment, verifierDeployment} {
		if !reflect.DeepEqual(deployment(t, base, name), deployment(t, r, name)) {
			t.Fatalf("Service host changed %s", name)
		}
	}
	for _, bad := range []string{"namespace=wrong", "port=0", "host=evil.example.com", "podSelector=null"} {
		invalid := slices.Clone(set)
		invalid = append(invalid, "analyzer.analysis.openAICompatible.serviceHosts[0]."+bad)
		helmFails(t, invalid...)
	}
}
