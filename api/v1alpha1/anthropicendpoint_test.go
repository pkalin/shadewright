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

package v1alpha1

import (
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
)

// The CEL rules on ModelDeployment repeat the Anthropic API's endpoint and host as literals, which CEL cannot
// take from a Go constant: every URL and every anthropic host they quote must be AnthropicEndpoint and its
// host, so the API server and the analyzer never disagree on it.
func TestAnthropicEndpointInRules(t *testing.T) {
	src, err := os.ReadFile("remediationsettings_types.go")
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(AnthropicEndpoint)
	if err != nil {
		t.Fatal(err)
	}
	literal := regexp.MustCompile(`'([^']*anthropic[^']*)'`)
	var rules, urls, hosts int
	for line := range strings.SplitSeq(string(src), "\n") {
		if !strings.Contains(line, "+kubebuilder:validation:XValidation") || !strings.Contains(line, "anthropic") {
			continue
		}
		rules++
		for _, m := range literal.FindAllStringSubmatch(line, -1) {
			switch {
			case strings.Contains(m[1], "://"):
				urls++
				if m[1] != AnthropicEndpoint {
					t.Errorf("a rule names the endpoint %q, the constant is %q", m[1], AnthropicEndpoint)
				}
			default:
				hosts++
				if m[1] != u.Host {
					t.Errorf("a rule names the host %q, the constant's is %q", m[1], u.Host)
				}
			}
		}
	}
	if rules != 2 || urls != 1 || hosts != 1 {
		t.Errorf("%d rules quoting %d endpoints and %d hosts, want 2, 1 and 1", rules, urls, hosts)
	}
}
