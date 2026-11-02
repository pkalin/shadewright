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

// Package registry turns an entry of RemediationSettings spec.repositories into a [providers.GitProvider]
// and keeps one provider per entry across reconciles (design sections 6, 8.2 and 9, ADR-0006).
//
// The provider holds the installation token cache, so it must outlive one reconcile; it is therefore kept
// under a key made of everything it is built from (URL, installation ID, App client ID, Key Vault key ID and
// target branch). A changed entry has another key and gets a new provider; an entry that left the settings is
// dropped on the next call.
//
// Nothing is built at startup and nothing reaches Azure or GitHub here: a provider is built on the first
// [Registry.Open] of its key, and the adapters make no network call when they are constructed.
//
// Settings that can never work (a provider other than GitHub, no GitHub block, no signer, a malformed key
// ID, URL, client ID or branch) fail with an error that matches [providers.ErrInvalidConfig], which callers
// treat as permanent. Every error has a fixed text: no URL, ID, key or adapter error text is ever repeated.
package registry
