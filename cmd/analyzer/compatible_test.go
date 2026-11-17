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
	"testing"

	"github.com/pkalin/shadewright/internal/providers/model/foundry"
)

func TestCompatibleOnlyDoesNotReadAzureIdentity(t *testing.T) {
	f := validFlags()
	f.disableFoundry = true
	f.endpointSuffixes = ".api.openai.com"
	asked := false
	c, err := buildAnalysis(f, testDeps(t), func() (foundry.TokenSource, error) { asked = true; return noTokens{}, nil })
	if err != nil || c == nil || asked {
		t.Fatalf("compatible-only start: %v, identity read %v", err, asked)
	}
	for _, mutate := range []func(*analysisFlags){
		func(f *analysisFlags) { f.compatibleKeyFile = "/key" },
		func(f *analysisFlags) { f.compatibleKeyHost = "api.openai.com" },
		func(f *analysisFlags) { f.compatibleServiceHosts = "*.svc.cluster.local" },
		func(f *analysisFlags) { f.compatibleKeyFile, f.compatibleKeyHost = "relative", "api.openai.com" },
	} {
		bad := f
		mutate(&bad)
		if _, err := buildAnalysis(bad, testDeps(t), nil); err == nil {
			t.Fatal("invalid compatible flags accepted")
		}
	}
}
