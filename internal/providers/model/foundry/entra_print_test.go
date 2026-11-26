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
package foundry

import (
	"strings"
	"testing"

	"github.com/shadewright-labs/shadewright/internal/testutil/printguard"
)

// A WorkloadIdentity, alone or held by another value, shows no part of its cached access token in any
// printed, logged or encoded form.
func TestWorkloadIdentityNeverPrintsTheToken(t *testing.T) {
	canary := strings.Join([]string{"eyJ0eXAi", "CANARY", "b7c2"}, ".")
	w := &WorkloadIdentity{}
	w.setToken(canary)
	printguard.Check(t, w, canary)
	if got := w.cachedToken(); got != canary {
		t.Errorf("cachedToken = %q, want the value", got)
	}
}
