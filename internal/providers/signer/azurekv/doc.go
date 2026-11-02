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

// Package azurekv is the Azure Key Vault adapter for [providers.Signer] (ADR-0006, ADR-0013).
//
// It signs RS256 digests with a non-exportable RSA key through the Key Vault keys API, authenticated with
// workload identity. It uses only the key's sign operation: it never reads, exports or lists keys, so the
// executor's identity needs nothing beyond the "sign" permission on one key, and the private key never
// enters the pod.
//
// The key ID is validated strictly before any call ([ParseKeyID]): only
// https://<vault>.vault.azure.net/keys/<name>[/<version>] is accepted, so a misconfiguration cannot send a
// token to another host.
//
// Every call has a deadline and a bounded number of retries on throttling and transient server errors.
// Errors are mapped to the kinds in package providers and never carry tokens, key material or response
// bodies from Key Vault.
//
// This is the only package allowed to import the Azure SDK for Key Vault and Entra ID.
package azurekv
