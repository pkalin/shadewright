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

package openaicompatible

import (
	"net/http"
	"strings"
	"testing"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
)

func TestGeneratedDeclaredCredentialBoundary(t *testing.T) {
	keyA, keyB := keyFile(t, "key-a"), keyFile(t, "key-b")
	declarations := []Endpoint{{URL: "https://gateway.example.com/tenant-a/v1", API: APIChatCompletions, KeyFile: keyA}, {URL: "https://gateway.example.com/tenant-b/v1", API: APIResponses, KeyFile: keyB}, {URL: "https://keyless.example.com/v1", API: APIChatCompletions}}
	if err := ValidateEndpoints(declarations, nil); err != nil {
		t.Fatal(err)
	}
	for _, scheme := range []string{"https", "HTTPS", "http"} {
		for _, host := range []string{"gateway.example.com", "GATEWAY.EXAMPLE.COM", "gateway.example.com.evil.test", "keyless.example.com"} {
			for _, port := range []string{"", ":443", ":8443"} {
				for _, path := range []string{"/tenant-a/v1", "/tenant-a/v1/", "/tenant-b/v1", "/tenant-a/v1/child", "/tenant-b/../tenant-a/v1", "/tenant-a/%76%31", "/tenant-a/%2e%2e/tenant-b/v1", "/v1", "/tenant-a/v1//", "/TENANT-A/v1", "/tenant-a/%2576%2531",
					"/tenant-a/v1%2f..%2ftenant-b/v1", "/tenant-a/v1?x", "/tenant-a/v1#x"} {
					raw := scheme + "://" + host + port + path
					for _, api := range []API{APIChatCompletions, APIResponses} {
						settings := remediationv1alpha1.ModelDeployment{Provider: remediationv1alpha1.ModelProviderOpenAICompatible, Endpoint: raw, InferenceAPI: remediationv1alpha1.InferenceAPI(api), Deployment: testModel}
						decl, ok := MatchEndpoint(declarations, settings.Endpoint, API(settings.InferenceAPI))
						wantBase := ""
						if strings.EqualFold(scheme, "https") && (port == "" || port == ":443") {
							if strings.EqualFold(host, "gateway.example.com") && (path == "/tenant-a/v1" || path == "/tenant-a/v1/" || path == "/tenant-a/%76%31") && api == APIChatCompletions {
								wantBase = declarations[0].URL
							}
							if strings.EqualFold(host, "gateway.example.com") && path == "/tenant-b/v1" && api == APIResponses {
								wantBase = declarations[1].URL
							}
							if host == "keyless.example.com" && path == "/v1" && api == APIChatCompletions {
								wantBase = declarations[2].URL
							}
						}
						if ok != (wantBase != "") {
							t.Fatalf("match %q %s: %v want %q", raw, api, ok, wantBase)
						}
						if !ok {
							continue
						}
						calls := 0
						p, err := newProvider(Config{Endpoint: decl.URL, API: decl.API, KeyFile: decl.KeyFile, Deployment: testModel}, testHooks{transport: roundTrip(func(r *http.Request) (*http.Response, error) {
							calls++
							suffix := "/chat/completions"
							body := chatOK
							key := "key-a"
							if decl.API == APIResponses {
								suffix = "/responses"
								body = responsesOK
								key = "key-b"
							}
							auth := "Bearer " + key
							if decl.KeyFile == "" {
								auth = ""
							}
							if r.URL.String() != wantBase+suffix || r.Header.Get("Authorization") != auth {
								t.Fatal("credential escaped its declared base")
							}
							return response(200, "application/json", body), nil
						})})
						if err != nil {
							t.Fatal(err)
						}
						if _, err = p.Complete(t.Context(), request()); err != nil || calls != 1 {
							t.Fatalf("call: %v", err)
						}
					}
				}
			}
		}
	}
}
func TestDeclarationRefusals(t *testing.T) {
	for _, raw := range []string{"https://user@gateway.example.com/v1", "https://@gateway.example.com/v1", "https://gateway.example.com/v1?x", "https://gateway.example.com/v1?", "https://gateway.example.com/v1#x", "https://gateway.example.com/v1#", "https://gateway.example.com/a/../v1", "https://gateway.example.com/a/%2E%2E/v1", "https://api.anthropic.com/v1", "https://r.openai.azure.com/v1", "https://r.cognitiveservices.azure.com/v1", "https://r.services.ai.azure.com/v1", "https://gateway.example.com/v1"} {
		if err := ValidateEndpoints([]Endpoint{{URL: raw, API: APIChatCompletions}}, []string{".gateway.example.com"}); err == nil {
			t.Errorf("invalid declaration: %q", raw)
		}
	}
	for _, raw := range []string{"HTTPS://GATEWAY.EXAMPLE.COM:443/v1/", "https://gateway.example.com/%76%31"} {
		if err := ValidateEndpoints([]Endpoint{{URL: "https://gateway.example.com/v1", API: APIChatCompletions}, {URL: raw, API: APIResponses}}, nil); err == nil {
			t.Error("duplicate canonical base survived startup")
		}
	}
}
