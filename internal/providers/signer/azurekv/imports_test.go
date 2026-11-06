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

package azurekv

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/pkalin/shadewright/internal/testutil/importguard"
)

// azureSDK is the import path prefix of the Azure SDK for Go, including Entra ID and MSAL.
var azureSDK = []string{"github.com/Azure/", "github.com/AzureAD/"}

// TestAzureSDKOnlyHere enforces ADR-0013 for the Azure adapters: no Go file in the module outside this package
// and the Blob Storage analysis store imports the Azure SDK.
func TestAzureSDKOnlyHere(t *testing.T) {
	here, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	root := importguard.ModuleRoot(t)
	allowed := map[string]bool{here: true, filepath.Join(root, "internal", "providers", "store", "azureblob"): true}
	checked := importguard.WalkGoFiles(t, root, func(path string) {
		if allowed[filepath.Dir(path)] {
			return
		}
		for _, p := range importguard.FileImports(t, path) {
			for _, prefix := range azureSDK {
				if strings.HasPrefix(p, prefix) {
					rel, _ := filepath.Rel(root, path)
					t.Errorf("%s imports %q: the Azure SDK is allowed only in internal/providers/signer/azurekv and internal/providers/store/azureblob", rel, p)
				}
			}
		}
	})
	if checked == 0 {
		t.Fatal("no Go files checked")
	}
}
