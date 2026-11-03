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

package groupversion

import (
	"strings"
	"testing"

	"github.com/pkalin/shadewright/internal/testutil/crdschema"
)

func TestValid(t *testing.T) {
	for _, s := range []string{"v1", "apps/v1", "keda.sh/v1alpha1", "autoscaling/v2beta2",
		"helm.toolkit.fluxcd.io/v2", "a-b.c/v10"} {
		if !Valid(s) {
			t.Errorf("Valid(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"", "keda.sh/foo", "a..b/v1", ".a/v1", "a./v1", "-a/v1", "keda.sh/", "/v1",
		"Keda.sh/v1", "keda.sh/v1/x", "keda.sh/v 1", "keda.sh/v1gamma1", "keda.sh/V1", "keda.sh",
		strings.Repeat("a", MaxLength-2) + "/v1"} {
		if Valid(s) {
			t.Errorf("Valid(%q) = true, want false", s)
		}
	}
	if s := strings.Repeat("a", MaxLength-3) + "/v1"; !Valid(s) {
		t.Errorf("a group version of %d characters is refused", len(s))
	}
}

// TestPatternMatchesSchema keeps Pattern equal to the group version patterns of the generated CRDs (the
// apiVersion of an Incident's subject and of a request's target, and the version an integration was detected
// by), so a value this package accepts is accepted by the API server too.
func TestPatternMatchesSchema(t *testing.T) {
	for _, c := range []struct {
		file string
		path string
	}{
		{"remediation.shadewright.io_incidents.yaml", "spec.subject.apiVersion"},
		{"remediation.shadewright.io_remediationrequests.yaml", "spec.targetRef.apiVersion"},
		{"remediation.shadewright.io_remediationsettings.yaml", "status.integrations.items.version"},
	} {
		if got := crdschema.Pattern(t, c.file, c.path); got != Pattern {
			t.Errorf("%s: pattern of %s is %q, want Pattern %q", c.file, c.path, got, Pattern)
		}
		if got, ok := crdschema.Field(t, c.file, c.path)["maxLength"].(float64); !ok || int(got) != MaxLength {
			t.Errorf("%s: maxLength of %s is %v, want MaxLength %d", c.file, c.path, got, MaxLength)
		}
	}
}
