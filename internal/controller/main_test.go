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
package controller

import (
	"fmt"
	"os"
	"testing"

	"github.com/pkalin/shadewright/internal/gitops/semdiff"
)

// TestMain makes the render check of the pull request step render in this process
// (semdiff.RenderInProcessForTests): no spec here is about the render process, which the tests of
// internal/gitops/semdiff cover, and a race-built test binary started for each check slowed the suite past its
// margin (#717). The test binary still serves as the render process (semdiff.RunRenderProcess), so the suite
// passes without the in-process render too.
func TestMain(m *testing.M) {
	semdiff.RunRenderProcess()
	if err := semdiff.RenderInProcessForTests(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}
