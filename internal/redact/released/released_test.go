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
// of package redact at origin/main 0701cd3a with "package released" for "package redact", and the change of #873,
// which made the work linear without changing the output (lines.go is new with it). What the code computes is
// checked against that release's results by TestReleasedReproducesTheReleaseBefore (package redact), and against
// the code before #873 (testdata/v052) by the tests of kept_test.go and TestReleasedSpansMatchKeptCopy.
func TestReleasedIsTheReleaseBefore(t *testing.T) {
	for name, want := range map[string]string{
		"redact.go": "f1c529161cef604df2e6d6741caa0d589fff7959969aef33981cbba97e81e031",
		"extent.go": "83c7cd9d2f8b1de7af41019e7631c88eeb23962bfa3952a5f2edc6f54d0ba0aa",
		"work.go":   "02ba296592fb1f3ac8f8d5cda941abe1ee782c89e7d1f7901844d6ae7211e3f5",
		"lines.go":  "7ba72fa9847d6bce30faecc36b3385f67d85fe2e12ea512746a33452469b6a0c",
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
