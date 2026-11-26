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

package auth

import (
	"strings"
	"testing"

	"github.com/shadewright-labs/shadewright/internal/testutil/printguard"
)

// tokenCanary is built at run time so no token-shaped literal sits in the source.
func tokenCanary() string { return strings.Join([]string{"ghs", "CANARY", "7f3a9c"}, "_") }

// A Token shows no part of its value in any printed, logged or encoded form, alone or held in a struct, a
// pointer to one, a slice or a map, with every verb: a verb that does not fit a pointer used to print the
// target of the pointer that held the value.
func TestTokenNeverPrintsItsValue(t *testing.T) {
	canary := tokenCanary()
	for _, tc := range []struct {
		name string
		tok  Token
	}{
		{"token", newToken(canary)},
		{"zero token", Token{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			printguard.Check(t, tc.tok, canary)
		})
	}
	if got := newToken(canary).Reveal(); got != canary {
		t.Errorf("Reveal = %q, want the value", got)
	}
	if got := (Token{}).Reveal(); got != "" {
		t.Errorf("Reveal of the zero Token = %q, want empty", got)
	}
}

// A Source holds a cached Token; a struct that holds a Source shows no part of it either.
func TestSourceHolderNeverPrintsTheToken(t *testing.T) {
	canary := tokenCanary()
	src := &Source{tok: newToken(canary)}
	printguard.Check(t, src, canary)
}
