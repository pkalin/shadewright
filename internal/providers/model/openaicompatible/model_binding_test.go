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

package openaicompatible_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"testing"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/analysis"
	"github.com/pkalin/shadewright/internal/analyzer/models"
	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/providers/model/openaicompatible"
)

const otherModel = "other"

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestGeneratedSettingsModelsBoundToDeclarations(t *testing.T) {
	for _, declared := range [][]string{{"m"}, {"m", "Model_2.0-x"}, {strings.Repeat("a", 64)}} {
		names, err := json.Marshal(declared)
		if err != nil {
			t.Fatal(err)
		}
		var endpoints []openaicompatible.Endpoint
		raw := fmt.Sprintf(`[{"url":"https://gateway.example.com/a","inferenceAPI":"ChatCompletions","models":%s},{"url":"https://gateway.example.com/b","inferenceAPI":"Responses","models":["other"]}]`, names)
		if err := json.Unmarshal([]byte(raw), &endpoints); err != nil {
			t.Fatal(err)
		}
		factory, err := models.NewModelFactory(models.ModelsConfig{DisableFoundry: true, CompatibleEndpoints: endpoints})
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{"a", "a/child", "b", "c"} {
			for _, api := range []remediationv1alpha1.InferenceAPI{"ChatCompletions", "Responses"} {
				for _, model := range append(append([]string{}, declared...), otherModel, "undeclared", "M", "m ", "bad/name", "") {
					want := path == "a" && api == "ChatCompletions" && slices.Contains(declared, model) || path == "b" && api == "Responses" && model == otherModel
					p, err := factory(analysis.RoleTriage, remediationv1alpha1.ModelDeployment{Provider: remediationv1alpha1.ModelProviderOpenAICompatible, Endpoint: "https://gateway.example.com/" + path, InferenceAPI: api, Deployment: model})
					if (err == nil) != want {
						t.Fatalf("path=%q api=%q model=%q: %v, want allowed=%v", path, api, model, err, want)
					}
					if !want {
						continue
					}
					calls := 0
					allowed := declared
					if path == "b" {
						allowed = []string{otherModel}
					}
					openaicompatible.SetTestTransport(p.(*openaicompatible.Provider), transportFunc(func(r *http.Request) (*http.Response, error) {
						calls++
						var body struct {
							Model string `json:"model"`
						}
						if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
							t.Fatal(err)
						}
						if body.Model != model || !slices.Contains(allowed, body.Model) {
							t.Fatalf("undeclared body model: %q", body.Model)
						}
						return &http.Response{StatusCode: 400, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("{}"))}, nil
					}))
					_, _ = p.Complete(t.Context(), providers.CompletionRequest{Messages: []providers.Message{{Role: providers.RoleUser, Content: "test"}}})
					if calls != 1 {
						t.Fatalf("allowed model sent %d requests", calls)
					}
				}
			}
		}
	}
}

// The factory hands the declared in-cluster ranges to the dial guard of every compatible provider it builds.
func TestFactoryWiresInClusterRanges(t *testing.T) {
	endpoints := []openaicompatible.Endpoint{{URL: "https://gateway.example.com/a", API: openaicompatible.APIChatCompletions, Models: []string{"m"}}}
	ranges := []netip.Prefix{netip.MustParsePrefix("11.0.0.0/8"), netip.MustParsePrefix("2a0b:4000::/32")}
	factory, err := models.NewModelFactory(models.ModelsConfig{DisableFoundry: true, CompatibleEndpoints: endpoints, CompatibleInClusterRanges: ranges})
	if err != nil {
		t.Fatal(err)
	}
	ranges[0] = netip.MustParsePrefix("12.0.0.0/8") // a later change by the caller moves nothing
	p, err := factory(analysis.RoleTriage, remediationv1alpha1.ModelDeployment{Provider: remediationv1alpha1.ModelProviderOpenAICompatible,
		Endpoint: "https://gateway.example.com/a", InferenceAPI: remediationv1alpha1.InferenceAPI(openaicompatible.APIChatCompletions), Deployment: "m"})
	if err != nil {
		t.Fatal(err)
	}
	guard := openaicompatible.DialGuard(p.(*openaicompatible.Provider))
	for _, addr := range []string{"11.2.3.4:443", "[2a0b:4000::1]:443", "[::ffff:11.2.3.4]:443"} {
		if err := guard.Control("tcp", addr, nil); err == nil {
			t.Errorf("in-cluster address %s allowed", addr)
		}
	}
	if err := guard.Control("tcp", "12.2.3.4:443", nil); err != nil {
		t.Errorf("public address refused: %v", err)
	}
	if _, err := models.NewModelFactory(models.ModelsConfig{DisableFoundry: true, CompatibleEndpoints: endpoints,
		CompatibleInClusterRanges: []netip.Prefix{netip.MustParsePrefix("11.0.0.1/8")}}); err == nil {
		t.Fatal("a range with host bits was accepted")
	}
}
