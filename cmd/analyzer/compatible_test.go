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

package main

import (
	"strings"
	"testing"

	"github.com/pkalin/shadewright/internal/providers/model/foundry"
)

func TestCompatibleOnlyDoesNotReadAzureIdentity(t *testing.T) {
	f := validFlags()
	f.disableFoundry = true
	f.endpointSuffixes = ""
	f.compatibleEndpoints = `[{"url":"https://api.openai.com/v1","inferenceAPI":"Responses","models":["m"]}]`
	asked := false
	c, err := buildAnalysis(f, testDeps(t), func() (foundry.TokenSource, error) { asked = true; return noTokens{}, nil })
	if err != nil || c == nil || asked {
		t.Fatalf("compatible-only start: %v, identity read %v", err, asked)
	}
	for _, raw := range []string{
		`[{"url":"https://api.openai.com/v1","inferenceAPI":"Responses","models":["m"],"keyFile":"relative"}]`,
		`[{"url":"https://api.openai.com/v1?x","inferenceAPI":"Responses","models":["m"]}]`,
		`[{"url":"https://api.anthropic.com/v1","inferenceAPI":"Responses","models":["m"]}]`,
		`[{"url":"https://model.demo.svc/v1","inferenceAPI":"Responses","models":["m"],` +
			`"service":{"namespace":"wrong","port":8443,"podSelector":{"app":"model"}}}]`,
	} {
		bad := f
		bad.compatibleEndpoints = raw
		if _, err := buildAnalysis(bad, testDeps(t), nil); err == nil {
			t.Fatal("invalid declaration survived startup")
		}
	}
}

func TestCompatibleInvalidDeclarationStopsBeforeIdentity(t *testing.T) {
	const declared = `{"url":"https://api.openai.com/v1","inferenceAPI":"Responses","models":["m"]}`
	const invalidURL = "unambiguous HTTPS"
	for _, tc := range []struct{ name, raw, suffix, reason string }{
		{"overlap", "[" + declared + "]", ".api.openai.com", "matches the Foundry suffix"},
		{"duplicate", "[" + declared + `,` +
			`{"url":"HTTPS://API.OPENAI.COM:443/v1/","inferenceAPI":"ChatCompletions","models":["m"]}]`,
			"", "duplicate base"},
		{"prefix", "[" + declared + `,` +
			`{"url":"https://api.openai.com/v1/team","inferenceAPI":"Responses","models":["m"]}]`, "", "path prefix"},
		{"missing models", `[{"url":"https://api.openai.com/v1","inferenceAPI":"Responses"}]`, "", "nonempty models"},
		{"empty models", `[{"url":"https://api.openai.com/v1","inferenceAPI":"Responses","models":[]}]`,
			"", "nonempty models"},
		{"invalid model", `[{"url":"https://api.openai.com/v1","inferenceAPI":"Responses","models":["bad/name"]}]`,
			"", "model name"},
		{"dot path", `[{"url":"https://api.openai.com/a/../v1","inferenceAPI":"Responses","models":["m"]}]`, "", invalidURL},
		{"fragment", `[{"url":"https://api.openai.com/v1#","inferenceAPI":"Responses","models":["m"]}]`, "", invalidURL},
		{"userinfo", `[{"url":"https://@api.openai.com/v1","inferenceAPI":"Responses","models":["m"]}]`, "", invalidURL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := validFlags()
			f.endpointSuffixes, f.compatibleEndpoints = tc.suffix, tc.raw
			asked := false
			_, err := buildAnalysis(f, testDeps(t), func() (foundry.TokenSource, error) { asked = true; return noTokens{}, nil })
			if err == nil || asked || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("wrong startup refusal: %v identity=%v, want %s", err, asked, tc.reason)
			}
		})
	}
}

func TestCompatibleInClusterRangesAtStart(t *testing.T) {
	const declared = `[{"url":"https://api.openai.com/v1","inferenceAPI":"Responses","models":["m"]}]`
	f := validFlags()
	f.disableFoundry, f.endpointSuffixes, f.compatibleEndpoints = true, "", declared
	f.compatibleInCluster = "198.51.100.0/24,2001:db8:1::/48"
	if c, err := buildAnalysis(f, testDeps(t), nil); err != nil || c == nil {
		t.Fatalf("valid ranges refused: %v", err)
	}
	for _, tc := range []struct{ name, endpoints, ranges, reason string }{
		{"malformed", declared, "198.51.100.0/33", "not a CIDR"},
		{"host bits", declared, "198.51.100.1/24", "host bits"},
		{"empty item", declared, "198.51.100.0/24,", "not a CIDR"},
		{"no endpoints", "", "198.51.100.0/24", "needs --openai-compatible-endpoints"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := f
			bad.compatibleEndpoints, bad.compatibleInCluster = tc.endpoints, tc.ranges
			if tc.endpoints == "" {
				bad.anthropicKeyFile = "/var/run/secrets/shadewright/anthropic/api-key"
			}
			if _, err := buildAnalysis(bad, testDeps(t), nil); err == nil || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("wrong startup refusal: %v, want %s", err, tc.reason)
			}
		})
	}
}
