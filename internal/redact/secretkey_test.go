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
	"slices"
	"strings"
	"testing"
)

// A key the object gives is decided from the key alone, by main's rule for a quoted key (#845, fifth decision
// of 2026-11-18): every key a quoted JSON key of that name hid the value of is a secret key, and a reference or
// a word that only holds a short name is not.
func TestSecretKeyIsMainsQuotedKey(t *testing.T) {
	folded := []string{"pass\u00adword", "pass\u200bword", " token "}
	for _, k := range append([]string{
		passwordKey, authKey, passKey, "sig", "sas", "DB_PASS", "db_auth", "proxy-auth", "storageSas", "X-Api-Key",
		clientSecretKey, "Secret", "pre-auth", "Pass", "AUTH",
	}, folded...) {
		if !SecretKey(k) {
			t.Errorf("SecretKey(%q) = false; main hid the value of a quoted key of that name", k)
		}
		if slices.Contains(folded, k) {
			continue
		}
		if got := String(`{"` + k + `": "mint"}`); strings.Contains(got, "mint") {
			t.Errorf("%q: main's rule for a quoted key no longer hides the value: %q", k, got)
		}
	}
	for _, k := range []string{
		"secretName", "secretRef", "author", "bypass", "signal", "kansas", "KANSAS", "name", "value", "password hint",
		"path", "tokenFile",
	} {
		if SecretKey(k) {
			t.Errorf("SecretKey(%q) = true; it is no secret key or a reference", k)
		}
	}
}
