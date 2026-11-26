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
package providers

import (
	"strings"
	"testing"

	"github.com/shadewright-labs/shadewright/internal/testutil/printguard"
)

// An Opaque held as an unexported field of a struct, a pointer to one, a slice or a map shows none of its
// bytes with any verb: a verb that does not fit a pointer used to print the target of the pointer.
func TestOpaqueHolderNeverPrintsItsBytes(t *testing.T) {
	mark := strings.Join([]string{"THINKING", "CANARY", "91d0"}, "-")
	for _, tc := range []struct {
		name string
		o    Opaque
	}{
		{"opaque", NewOpaque([]byte(mark))},
		{"zero", Opaque{}},
	} {
		t.Run(tc.name, func(t *testing.T) { printguard.Check(t, tc.o, mark) })
	}
	printguard.Check(t, Message{Role: RoleAssistant, ProviderBlocks: NewOpaque([]byte(mark))}, mark)
}
