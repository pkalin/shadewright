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
package sts

import (
	"strings"
	"testing"

	"github.com/shadewright-labs/shadewright/internal/testutil/printguard"
)

// A Source held by another value shows no part of its cached access token in any printed, logged or encoded
// form: fmt walks the holder without calling Source's methods, and for a verb that does not fit a pointer
// it prints the Source's fields.
func TestSourceHolderNeverPrintsTheToken(t *testing.T) {
	canary := strings.Join([]string{"ya29", "CANARY", "4e1b"}, ".")
	src := &Source{}
	src.setToken(canary)
	printguard.Check(t, src, canary)
	if got := src.cachedToken(); got != canary {
		t.Errorf("cachedToken = %q, want the value", got)
	}
}
