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

package redact

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

// TestMainRedactIsUnchanged pins the copy of main's redactor that the monotonic tests compare with
// (testdata/mainredact, commit 28a8212c): an edit to it would change what "main removes" means without any
// other test noticing. When the comparison is moved to a newer main on purpose, replace the files and the sums
// together.
func TestMainRedactIsUnchanged(t *testing.T) {
	for name, want := range map[string]string{
		"extent.go": "6aa84d4f0551671a4df01f88f355a55da4640bcf0d9297e42f37112753f6869f",
		"redact.go": "714d6e2cdbf64720640c4a2b155da5b3b683227cb697c67ec43a975e8cdbbf2d",
		"spans.go":  "895f4b7920eeee1783e7c89f7570297bf6138e8c3b4ec08c57cea17e9e96f8af",
	} {
		b, err := os.ReadFile(filepath.Join("testdata", "mainredact", name))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(b)
		if got := hex.EncodeToString(sum[:]); got != want {
			t.Errorf("testdata/mainredact/%s changed: sha256 %s, want %s", name, got, want)
		}
	}
}
