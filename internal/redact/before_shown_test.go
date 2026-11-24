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

package redact_test

import (
	"strings"
	"testing"

	"github.com/shadewright-labs/shadewright/internal/incidentcontext"
	"github.com/shadewright-labs/shadewright/internal/redact"
)

// The reading of the release before leaves out a value on a line at the bound incidentcontext.Value leaves it
// out at: the two constants agree.
func TestReleasedShownMatchesValue(t *testing.T) {
	id := func(s string) string { return s }
	if v := incidentcontext.Value(id, strings.Repeat("a", redact.MaxShownBytes)); strings.Contains(v, "left out") {
		t.Errorf("a value of %d bytes is left out by incidentcontext.Value", redact.MaxShownBytes)
	}
	if v := incidentcontext.Value(id, strings.Repeat("a", redact.MaxShownBytes+1)); !strings.Contains(v, "left out") {
		t.Errorf("a value of %d bytes is shown by incidentcontext.Value", redact.MaxShownBytes+1)
	}
}
