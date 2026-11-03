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
	"os"
	"path/filepath"
	"strings"
	"testing"
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

// TestPatternMatchesSchema keeps Pattern equal to the group version patterns of the API (ObjectRef's apiVersion
// and the version an integration was detected by), so a value this package accepts is accepted by the API
// server too.
func TestPatternMatchesSchema(t *testing.T) {
	for _, file := range []string{"common_types.go", "remediationsettings_types.go"} {
		src, err := os.ReadFile(filepath.Join("..", "..", "api", "v1alpha1", file))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(src), "// +kubebuilder:validation:Pattern=`"+Pattern+"`") {
			t.Errorf("Pattern differs from the group version pattern in api/v1alpha1/%s", file)
		}
	}
}
