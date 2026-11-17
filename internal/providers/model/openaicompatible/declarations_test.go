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
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
)

const (
	alternateHTTPSPort = ":8443"
	v1Path             = "/v1"
	gatewayHost        = "gateway.example.com"
	defaultHTTPSPort   = ":443"
	tenantABasePath    = "/tenant-a/v1"
	nestedPath         = "/v1/team"
)

func TestGeneratedDeclaredCredentialBoundary(t *testing.T) {
	keyA, keyB := keyFile(t, "key-a-marker-834-000"), keyFile(t, "key-b-marker-834-000")
	declarations := []Endpoint{{URL: "https://gateway.example.com/tenant-a/v1", API: APIChatCompletions, Models: []string{testModel}, KeyFile: keyA}, {URL: "https://gateway.example.com/tenant-b/v1", API: APIResponses, Models: []string{testModel}, KeyFile: keyB}, {URL: "https://keyless.example.com/v1", API: APIChatCompletions, Models: []string{testModel}}}
	if err := ValidateEndpoints(declarations, nil); err != nil {
		t.Fatal(err)
	}
	for _, scheme := range []string{"https", "HTTPS", "http"} {
		for _, host := range []string{gatewayHost, "GATEWAY.EXAMPLE.COM", "gateway.example.com.evil.test", "keyless.example.com"} {
			for _, port := range []string{"", defaultHTTPSPort, alternateHTTPSPort} {
				for _, path := range []string{tenantABasePath, "/tenant-a/v1/", "/tenant-b/v1", "/tenant-a/v1/child", "/tenant-b/../tenant-a/v1", "/tenant-a/%76%31", "/tenant-a/%2e%2e/tenant-b/v1", v1Path, "/tenant-a/v1//", "/TENANT-A/v1", "/tenant-a/%2576%2531",
					"/tenant-a/v1%2f..%2ftenant-b/v1", "/tenant-a/v1?x", "/tenant-a/v1#x"} {
					raw := scheme + "://" + host + port + path
					for _, api := range []API{APIChatCompletions, APIResponses} {
						settings := remediationv1alpha1.ModelDeployment{Provider: remediationv1alpha1.ModelProviderOpenAICompatible, Endpoint: raw, InferenceAPI: remediationv1alpha1.InferenceAPI(api), Deployment: testModel}
						decl, ok := MatchEndpoint(declarations, settings.Endpoint, API(settings.InferenceAPI))
						wantBase := ""
						if strings.EqualFold(scheme, "https") && (port == "" || port == defaultHTTPSPort) {
							if strings.EqualFold(host, gatewayHost) && (path == tenantABasePath || path == "/tenant-a/v1/" || path == "/tenant-a/%76%31") && api == APIChatCompletions {
								wantBase = declarations[0].URL
							}
							if strings.EqualFold(host, gatewayHost) && path == "/tenant-b/v1" && api == APIResponses {
								wantBase = declarations[1].URL
							}
							if host == "keyless.example.com" && path == v1Path && api == APIChatCompletions {
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
							key := "key-a-marker-834-000"
							if decl.API == APIResponses {
								suffix = "/responses"
								body = responsesOK
								key = "key-b-marker-834-000"
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
	for _, raw := range []string{"https://user" + "@" + "gateway.example.com/v1", "https://@gateway.example.com/v1", "https://gateway.example.com/v1?x", "https://gateway.example.com/v1?", "https://gateway.example.com/v1#x", "https://gateway.example.com/v1#", "https://gateway.example.com/a/../v1", "https://gateway.example.com/a/%2E%2E/v1", "https://api.anthropic.com/v1", "https://r.openai.azure.com/v1", "https://r.cognitiveservices.azure.com/v1", "https://r.services.ai.azure.com/v1", "https://gateway.example.com/v1"} {
		if err := ValidateEndpoints([]Endpoint{{URL: raw, API: APIChatCompletions, Models: []string{testModel}}}, []string{".gateway.example.com"}); err == nil {
			t.Errorf("invalid declaration: %q", raw)
		}
	}
	for _, raw := range []string{"HTTPS://GATEWAY.EXAMPLE.COM:443/v1/", "https://gateway.example.com/%76%31"} {
		if err := ValidateEndpoints([]Endpoint{{URL: "https://gateway.example.com/v1", API: APIChatCompletions, Models: []string{testModel}}, {URL: raw, API: APIResponses, Models: []string{testModel}}}, nil); err == nil {
			t.Error("duplicate canonical base survived startup")
		}
	}
}

func TestDeclarationPathPrefixes(t *testing.T) {
	for _, paths := range [][2]string{{"", v1Path}, {v1Path, nestedPath}, {nestedPath, v1Path}, {"/%76%31/", nestedPath}} {
		var declarations []Endpoint
		raw := fmt.Sprintf(`[{"url":"https://gateway.example.com%s","inferenceAPI":"Responses","models":["m"]},{"url":"https://gateway.example.com%s","inferenceAPI":"Responses","models":["m"]}]`, paths[0], paths[1])
		if err := json.Unmarshal([]byte(raw), &declarations); err != nil {
			t.Fatal(err)
		}
		if err := ValidateEndpoints(declarations, nil); err == nil || !strings.Contains(err.Error(), "path prefix") {
			t.Fatalf("prefix %v: %v", paths, err)
		}
	}
}

func TestGeneratedDeclarationPrefixSegments(t *testing.T) {
	for _, host := range []string{gatewayHost, "other.example.com"} {
		for _, port := range []string{"", defaultHTTPSPort, alternateHTTPSPort} {
			for _, prefix := range []string{"", v1Path, tenantABasePath} {
				for _, tail := range []string{"", "/", "/child", "-child", "ish"} {
					for _, reverse := range []bool{false, true} {
						a := Endpoint{URL: "https://gateway.example.com" + prefix, API: APIResponses, Models: []string{"m"}}
						suffix := tail
						if prefix == "" && tail != "" && !strings.HasPrefix(tail, "/") {
							suffix = "/" + tail
						}
						b := Endpoint{URL: "https://" + host + port + prefix + suffix, API: APIResponses, Models: []string{"m"}}
						pair := []Endpoint{a, b}
						if reverse {
							pair = []Endpoint{b, a}
						}
						sameAuthority := host == gatewayHost && port != alternateHTTPSPort
						// Root is a segment prefix of every path; textual siblings are distinct.
						overlaps := sameAuthority && (prefix == "" || tail == "" || tail == "/" || tail == "/child")
						if err := ValidateEndpoints(pair, nil); (err != nil) != overlaps {
							t.Fatalf("pair=%+v err=%v want overlap=%v", pair, err, overlaps)
						}
					}
				}
			}
		}
	}
}
