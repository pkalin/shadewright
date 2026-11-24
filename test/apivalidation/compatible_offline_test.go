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

package apivalidation

import (
	"os"
	"testing"

	extensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	extensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	validation "k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/yaml"

	"github.com/shadewright-labs/shadewright/api/v1alpha1"
)

const compatibleEndpoint = "https://api.openai.com/v1"

// TestCompatibleSchemaOffline evaluates the generated OpenAPI schema and CEL rules in-process, as well as
// the API-server entries above: no local listening port is required for this matrix.
func TestCompatibleSchemaOffline(t *testing.T) {
	data, err := os.ReadFile("../../config/crd/bases/remediation.shadewright.io_remediationsettings.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var crd extensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(data, &crd); err != nil {
		t.Fatal(err)
	}
	modelSchema := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Properties["analysis"].Properties["models"]
	v1 := modelSchema.Properties["triage"]
	var props extensions.JSONSchemaProps
	if err := extensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(&v1, &props, nil); err != nil {
		t.Fatal(err)
	}
	structural, err := schema.NewStructural(&props)
	if err != nil {
		t.Fatal(err)
	}
	rules := cel.NewValidator(structural, false, 10000000)
	shape, _, err := validation.NewSchemaValidator(&props)
	if err != nil {
		t.Fatal(err)
	}
	for _, provider := range []v1alpha1.ModelProvider{
		"", v1alpha1.ModelProviderFoundry, v1alpha1.ModelProviderAnthropic, v1alpha1.ModelProviderOpenAICompatible, "Other",
	} {
		for _, version := range []string{"", "v1", "2024-10-21"} {
			for _, api := range []v1alpha1.InferenceAPI{
				"", v1alpha1.InferenceAPIChatCompletions, v1alpha1.InferenceAPIResponses, "Other",
			} {
				name := string(provider) + "/" + version + "/" + string(api)
				t.Run(name, func(t *testing.T) {
					endpoint := compatibleEndpoint
					if provider == "" || provider == v1alpha1.ModelProviderFoundry {
						endpoint = "https://lab.services.ai.azure.com"
					}
					if provider == v1alpha1.ModelProviderAnthropic {
						endpoint = v1alpha1.AnthropicEndpoint
					}
					obj := map[string]any{"endpoint": endpoint, "deployment": "model", "maxTokensPerDay": int64(100000)}
					if provider != "" {
						obj["provider"] = string(provider)
					}
					if version != "" {
						obj["apiVersion"] = version
					}
					if api != "" {
						obj["inferenceAPI"] = string(api)
					}
					errs := validation.ValidateCustomResource(field.NewPath("model"), obj, shape)
					ce, _ := rules.Validate(t.Context(), field.NewPath("model"), structural, obj, nil, 10000000)
					accepted := len(errs)+len(ce) == 0
					want := false
					switch provider {
					case "", v1alpha1.ModelProviderFoundry:
						want = version != "" && (api == "" || api == v1alpha1.InferenceAPIChatCompletions ||
							api == v1alpha1.InferenceAPIResponses && version == "v1")
					case v1alpha1.ModelProviderAnthropic:
						want = api == ""
					case v1alpha1.ModelProviderOpenAICompatible:
						want = version == "" && (api == "" || api == v1alpha1.InferenceAPIChatCompletions ||
							api == v1alpha1.InferenceAPIResponses)
					}
					if accepted != want {
						t.Fatalf("accepted %v, want %v: OpenAPI %v, CEL %v", accepted, want, errs, ce)
					}
				})
			}
		}
	}
	for _, tc := range []struct {
		provider v1alpha1.ModelProvider
		endpoint string
		accepted bool
	}{
		{v1alpha1.ModelProviderFoundry, compatibleEndpoint, false},
		{v1alpha1.ModelProviderOpenAICompatible, v1alpha1.AnthropicEndpoint, false},
		{v1alpha1.ModelProviderOpenAICompatible, "https://r.openai.azure.com/v1", false},
		{v1alpha1.ModelProviderOpenAICompatible, "https://r.cognitiveservices.azure.com/v1", false},
		{v1alpha1.ModelProviderOpenAICompatible, "https://r.services.ai.azure.com/v1", false},
		{v1alpha1.ModelProviderOpenAICompatible, "https://openai.azure.com/v1", false},
		{v1alpha1.ModelProviderOpenAICompatible, "https://gateway.example.com/v1", true},
		{v1alpha1.ModelProviderOpenAICompatible, "https://model.demo.svc/v1", true},
		{v1alpha1.ModelProviderOpenAICompatible, "http://model.demo.svc/v1", false},
		{v1alpha1.ModelProviderOpenAICompatible, "https://127.0.0.1/v1", false},
	} {
		t.Run("host/"+string(tc.provider)+"/"+tc.endpoint, func(t *testing.T) {
			obj := map[string]any{"provider": string(tc.provider), "endpoint": tc.endpoint,
				"deployment": "model", "maxTokensPerDay": int64(100000)}
			if tc.provider == v1alpha1.ModelProviderFoundry {
				obj["apiVersion"] = "v1"
			}
			errs := validation.ValidateCustomResource(field.NewPath("model"), obj, shape)
			ce, _ := rules.Validate(t.Context(), field.NewPath("model"), structural, obj, nil, 10000000)
			if (len(errs)+len(ce) == 0) != tc.accepted {
				t.Fatalf("host verdict: OpenAPI %v, CEL %v", errs, ce)
			}
		})
	}
}
