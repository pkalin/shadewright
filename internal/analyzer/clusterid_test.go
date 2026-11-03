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

package analyzer

import (
	"strings"
	"testing"
)

func TestParseClusterID(t *testing.T) {
	for _, ok := range []string{"a", "lab", "aks-lab-01", "prod.westeurope", "lab_2", strings.Repeat("a", 63)} {
		if got, err := ParseClusterID(ok); err != nil || got != ok {
			t.Errorf("ParseClusterID(%q) = %q, %v: want it accepted", ok, got, err)
		}
	}
}

func TestParseClusterIDRefuses(t *testing.T) {
	for _, bad := range []string{
		"",                      // required
		strings.Repeat("a", 64), // too long
		"-lab", "lab-", ".lab",  // must start and end with a letter or digit
		"my lab", "lab/1", "lab:1", "lab\n", "läb", // outside the allowed characters
	} {
		if _, err := ParseClusterID(bad); err == nil {
			t.Errorf("ParseClusterID(%q) accepted it", bad)
		}
	}
}
