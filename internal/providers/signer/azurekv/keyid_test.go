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
	"strings"
	"testing"
)

const (
	testVersion = "0123456789abcdef0123456789abcdef"
	testVault   = "example"
	testKeyName = "github-app"
)

func TestParseKeyIDAccepts(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want KeyID
	}{
		{"https://example.vault.azure.net/keys/github-app", KeyID{Vault: testVault, Name: testKeyName}},
		{"https://kv-example-01.vault.azure.net/keys/App1/" + testVersion,
			KeyID{Vault: "kv-example-01", Name: "App1", Version: testVersion}},
		{"https://abc.vault.azure.net/keys/k", KeyID{Vault: "abc", Name: "k"}},
	} {
		got, err := ParseKeyID(tc.in)
		if err != nil {
			t.Errorf("ParseKeyID(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseKeyID(%q) = %+v, want %+v", tc.in, got, tc.want)
		}
		if got.String() != tc.in {
			t.Errorf("String() = %q, want %q", got.String(), tc.in)
		}
	}
}

func TestParseKeyIDRejects(t *testing.T) {
	for _, in := range []string{
		"",
		// Other schemes.
		"http://example.vault.azure.net/keys/github-app",
		"HTTPS://example.vault.azure.net/keys/github-app",
		"ftp://example.vault.azure.net/keys/github-app",
		"//example.vault.azure.net/keys/github-app",
		"example.vault.azure.net/keys/github-app",
		// Other hosts.
		"https://example.vault.azure.net.attacker.example/keys/github-app",
		"https://vault.azure.net/keys/github-app",
		"https://example.vault.azure.cn/keys/github-app",
		"https://example.managedhsm.azure.net/keys/github-app",
		"https://example.attacker.example/keys/github-app",
		"https://a.b.vault.azure.net/keys/github-app",
		"https://EXAMPLE.vault.azure.net/keys/github-app",
		"https://127.0.0.1/keys/github-app",
		// Vault names outside the Key Vault rules.
		"https://ab.vault.azure.net/keys/github-app",
		"https://1example.vault.azure.net/keys/github-app",
		"https://example-.vault.azure.net/keys/github-app",
		"https://ex--ample.vault.azure.net/keys/github-app",
		"https://" + strings.Repeat("a", 25) + ".vault.azure.net/keys/github-app",
		// Ports, user info, query, fragment.
		"https://example.vault.azure.net:443/keys/github-app",
		"https://user@example.vault.azure.net/keys/github-app",
		"https://user:pass@example.vault.azure.net/keys/github-app",
		"https://example.vault.azure.net/keys/github-app?api-version=1",
		"https://example.vault.azure.net/keys/github-app#x",
		// Paths.
		"https://example.vault.azure.net/keys/",
		"https://example.vault.azure.net/keys/github-app/",
		"https://example.vault.azure.net/secrets/github-app",
		"https://example.vault.azure.net/certificates/github-app",
		"https://example.vault.azure.net/keys/github-app/" + testVersion + "/sign",
		"https://example.vault.azure.net/keys/github-app/latest",
		"https://example.vault.azure.net/keys/github-app/" + strings.ToUpper(testVersion),
		"https://example.vault.azure.net/keys/../secrets/github-app",
		"https://example.vault.azure.net/keys/github_app",
		"https://example.vault.azure.net/keys/github%2Dapp",
		"https://example.vault.azure.net/keys/" + strings.Repeat("k", 128),
		"https://example.vault.azure.net/keys/github-app\n",
		" https://example.vault.azure.net/keys/github-app",
	} {
		if got, err := ParseKeyID(in); err == nil {
			t.Errorf("ParseKeyID(%q) = %+v, want an error", in, got)
		}
	}
}

func TestKeyIDMatches(t *testing.T) {
	unpinned := KeyID{Vault: testVault, Name: testKeyName}
	pinned := KeyID{Vault: testVault, Name: testKeyName, Version: testVersion}
	other := "fedcba9876543210fedcba9876543210"
	base := "https://example.vault.azure.net/keys/github-app/"

	if !unpinned.matches(base + other) {
		t.Error("unpinned key should match any version of itself")
	}
	if !pinned.matches(base + testVersion) {
		t.Error("pinned key should match its version")
	}
	if pinned.matches(base + other) {
		t.Error("pinned key matched another version")
	}
	if unpinned.matches("https://other.vault.azure.net/keys/github-app/" + other) {
		t.Error("matched a key in another vault")
	}
	if unpinned.matches("https://example.vault.azure.net/keys/another/" + other) {
		t.Error("matched another key")
	}
	if unpinned.matches("not a url") {
		t.Error("matched garbage")
	}
}
