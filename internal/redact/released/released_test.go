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

package released

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"testing"
)

// The files of the release before are kept byte for byte: the sums are those of redact.go, extent.go and work.go
// of package redact at origin/main 0701cd3a with "package redact" replaced by "package released". What the code
// computes is checked against that release's results by TestReleasedReproducesTheReleaseBefore (package redact).
func TestReleasedIsTheReleaseBefore(t *testing.T) {
	for name, want := range map[string]string{
		"redact.go": "d560eb22aad31d315132ba899d54e20a6786c886cc5f30d35638fbb70877de9b",
		"extent.go": "852b5f906660bb70bac104431b3de27b9ef25c2f79b0ddfdc4b13f7b42beed9a",
		"work.go":   "02ba296592fb1f3ac8f8d5cda941abe1ee782c89e7d1f7901844d6ae7211e3f5",
	} {
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(b)
		if got := hex.EncodeToString(sum[:]); got != want {
			t.Errorf("%s changed: sha256 %s, want %s; the kept reading must not be edited", name, got, want)
		}
	}
}
