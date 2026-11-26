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

package openaicompatible

import (
	"strings"
	"testing"

	"github.com/shadewright-labs/shadewright/internal/testutil/printguard"
)

// A key scan, alone or held by another value, shows no part of its key in any printed, logged or encoded
// form, and still finds the key in a reply that repeats it.
func TestKeyScanNeverPrintsTheKey(t *testing.T) {
	key := strings.Join([]string{"sk", "SCANCANARY", "d6e7"}, "-")
	printguard.Check(t, newKeyScan(key), key)
	if found, _ := keyIn([]byte(`{"choices":[{"message":{"content":"`+key+`"}}]}`), key); !found {
		t.Error("the scan did not find the key")
	}
}
