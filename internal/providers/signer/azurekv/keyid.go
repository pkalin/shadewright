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
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// vaultSuffix is the only Key Vault DNS suffix accepted: the Azure public cloud.
const vaultSuffix = ".vault.azure.net"

// maxKeyIDLength matches the limit on RemediationSettings keyID.
const maxKeyIDLength = 2048

// keyIDPattern follows the Key Vault naming rules: a vault name of 3 to 24 lower-case letters, digits and
// hyphens that starts with a letter and ends with a letter or digit; a key name of 1 to 127 letters, digits
// and hyphens; an optional version of 32 lower-case hexadecimal digits. Nothing else may follow.
var keyIDPattern = regexp.MustCompile(
	`^https://([a-z][a-z0-9-]{1,22}[a-z0-9])\.vault\.azure\.net/keys/([A-Za-z0-9-]{1,127})(?:/([0-9a-f]{32}))?$`)

// KeyID is a validated Key Vault key identifier.
type KeyID struct {
	// Vault is the vault name, the first label of the host.
	Vault string
	// Name is the key name.
	Name string
	// Version is the key version; empty means the current version.
	Version string
}

// ParseKeyID validates a key identifier of the form https://<vault>.vault.azure.net/keys/<name>[/<version>].
// Any other scheme, host, port, user info, path, query or fragment is rejected.
func ParseKeyID(s string) (KeyID, error) {
	if s == "" {
		return KeyID{}, errors.New("key ID is empty")
	}
	if len(s) > maxKeyIDLength {
		return KeyID{}, fmt.Errorf("key ID is longer than %d characters", maxKeyIDLength)
	}
	m := keyIDPattern.FindStringSubmatch(s)
	if m == nil {
		return KeyID{}, errors.New(
			"key ID must be https://<vault>.vault.azure.net/keys/<name> with an optional 32-character version")
	}
	if strings.Contains(m[1], "--") {
		return KeyID{}, errors.New("key ID vault name must not contain consecutive hyphens")
	}
	// Belt and braces: the standard parser must agree with the pattern.
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "https" || u.Host != m[1]+vaultSuffix || u.User != nil ||
		u.RawQuery != "" || u.Fragment != "" || u.Port() != "" {
		return KeyID{}, errors.New("key ID is not a plain https Key Vault URL")
	}
	return KeyID{Vault: m[1], Name: m[2], Version: m[3]}, nil
}

// VaultURL returns the vault's base URL, https://<vault>.vault.azure.net.
func (k KeyID) VaultURL() string { return "https://" + k.Vault + vaultSuffix }

// String returns the key identifier in its canonical form.
func (k KeyID) String() string {
	s := k.VaultURL() + "/keys/" + k.Name
	if k.Version != "" {
		s += "/" + k.Version
	}
	return s
}

// matches reports whether kid, as returned by Key Vault, names this key: the same vault and name and, when
// this key ID is pinned to a version, the same version.
func (k KeyID) matches(kid string) bool {
	got, err := ParseKeyID(kid)
	if err != nil {
		return false
	}
	if got.Vault != k.Vault || !strings.EqualFold(got.Name, k.Name) {
		return false
	}
	return k.Version == "" || got.Version == k.Version
}
