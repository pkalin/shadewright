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
	"reflect"
	"slices"
	"strings"
	"testing"
)

const compatibleKey = "analyzer.analysis.openAICompatible.apiKey.secretName=model-key"
const compatibleHost = "analyzer.analysis.openAICompatible.apiKey.host=api.openai.com"

func TestCompatibleKeyAndNoAzure(t *testing.T) {
	base := render(t, false)
	set := []string{compatibleKey, compatibleHost, "analyzer.analysis.disableFoundry=true",
		"analyzer.analysis.endpointSuffixes={.api.openai.com}", "analyzer.analysis.triageModel=gpt-6.1-sol"}
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
		"--openai-compatible-api-key-file=/var/run/secrets/shadewright/openai-compatible/api-key"} {
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
		"unsafe filename": {compatibleKey, compatibleHost, "analyzer.analysis.openAICompatible.apiKey.secretKey=../key"},
		"reserved flag":   {"analyzer.extraArgs={--openai-compatible-api-key-file=/key}"},
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
		"analyzer.analysis.endpointSuffixes={.model.demo.svc.cluster.local}",
		"analyzer.analysis.openAICompatible.serviceHosts={model.demo.svc.cluster.local}",
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
