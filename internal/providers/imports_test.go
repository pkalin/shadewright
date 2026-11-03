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

package providers_test

import (
	"testing"

	"github.com/pkalin/shadewright/internal/testutil/importguard"
)

// TestNoVendorImports enforces ADR-0013: the interfaces and the fakes import only the standard library and
// this module, and nothing they depend on, however indirectly, is a product SDK or an adapter, so no vendor
// SDK can leak into the core through them.
func TestNoVendorImports(t *testing.T) {
	importguard.DirectImportsOnlyStandardOrModule(t, ".", "fake")
	importguard.NoForbiddenDeps(t, ".", ".", "./fake")
}
