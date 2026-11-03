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

package importguard

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// recorder is a T that records errors instead of failing.
type recorder struct {
	testing.TB
	errors []string
}

func (r *recorder) Errorf(format string, args ...any) {
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}

func TestForbidden(t *testing.T) {
	cases := map[string]string{
		"github.com/Azure/azure-sdk-for-go/sdk/azcore":        ReasonSDK,
		"github.com/AzureAD/microsoft-authentication-library": ReasonSDK,
		"github.com/aws/aws-sdk-go-v2/service/ec2":            ReasonSDK,
		Module + "internal/providers/signer/azurekv":          ReasonAdapter,
		Module + "internal/providers/github":                  ReasonAdapter,
		Module + "internal/providers/registry":                ReasonAdapter,
		Module + "internal/providers/fake":                    "",
		Module + "internal/providers/fakeish":                 ReasonAdapter,
		Module + "internal/providers":                         "",
		Module + "internal/modules":                           "",
		"k8s.io/apimachinery/pkg/apis/meta/v1":                "",
		"github.com/Azurefake/x":                              "",
		"net/http":                                            "",
	}
	for path, want := range cases {
		if got := Forbidden(path); got != want {
			t.Errorf("Forbidden(%q) = %q, want %q", path, got, want)
		}
	}
}

// TestNoForbiddenDepsFollowsTransitiveImports checks the guard on the binary, which reaches the Azure SDK only
// through the provider registry and the Key Vault signer: the SDK import is reported although the binary does
// not import it itself.
func TestNoForbiddenDepsFollowsTransitiveImports(t *testing.T) {
	rec := &recorder{TB: t}
	NoForbiddenDeps(rec, ModuleRoot(t), Module+"cmd")
	want := []string{
		`imports "` + Module + `internal/providers/registry", an adapter package`,
		`internal/providers/signer/azurekv imports "github.com/Azure/`,
	}
	all := strings.Join(rec.errors, "\n")
	for _, w := range want {
		if !strings.Contains(all, w) {
			t.Errorf("NoForbiddenDeps did not report %q; reported:\n%s", w, all)
		}
	}
}

func TestNoForbiddenDepsPassesTheCore(t *testing.T) {
	rec := &recorder{TB: t}
	NoForbiddenDeps(rec, ModuleRoot(t), Module+"internal/modules", Module+"internal/providers")
	if len(rec.errors) != 0 {
		t.Errorf("NoForbiddenDeps reported %v", rec.errors)
	}
}

func TestIsStandardOrModule(t *testing.T) {
	for path, want := range map[string]bool{
		"fmt": true, "net/http": true, Module + "api/v1alpha1": true,
		"k8s.io/api/core/v1": false, "github.com/pkalin/other": false,
	} {
		if got := IsStandardOrModule(path); got != want {
			t.Errorf("IsStandardOrModule(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestDirectImportsOnlyStandardOrModule(t *testing.T) {
	rec := &recorder{TB: t}
	DirectImportsOnlyStandardOrModule(rec, filepath.Join("testdata", "vendored"))
	if len(rec.errors) != 1 || !strings.Contains(rec.errors[0], `imports "k8s.io/api/core/v1"`) {
		t.Errorf("DirectImportsOnlyStandardOrModule reported %v, want one error for k8s.io/api/core/v1", rec.errors)
	}
}

func TestNoTestutilInProductCodeReportsAnImport(t *testing.T) {
	rec := &recorder{TB: t}
	NoTestutilInProductCode(rec, filepath.Join("testdata", "product"))
	if len(rec.errors) != 1 || !strings.Contains(rec.errors[0], `internal/testutil/importguard`) {
		t.Errorf("NoTestutilInProductCode reported %v, want one error for the importguard import", rec.errors)
	}
}

// TestNoTestutilInProductCode enforces that no non-test file in the module imports internal/testutil.
func TestNoTestutilInProductCode(t *testing.T) {
	if n := NoTestutilInProductCode(t, ModuleRoot(t)); n == 0 {
		t.Fatal("no Go files checked")
	}
}
