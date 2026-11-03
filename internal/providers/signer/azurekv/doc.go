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
// Every call has a deadline and a bounded number of retries, with exponential backoff and jitter, on
// throttling, transient server errors, network failures and response bodies that break off; a certificate
// the client refuses is final. The workload identity credential reads the federated token file itself (a
// regular file of at most 64 KiB) and sends its token requests through the same HTTP transport as the Key
// Vault requests, so a token file that cannot be read and a refused certificate of Entra ID are final, a
// token request that gets no answer is transient, and a token failure this package cannot see into (from
// a credential it did not build) is final. Errors are mapped to the kinds in package
// providers and never carry tokens, key material or response bodies from Key Vault. Besides the kind, an
// error keeps the status code, a short error code and the retry delay of a Key Vault response, or the
// fixed message of a transport refusal, a token request failure or a context error. No SDK or network
// error stays in the chain, so unwrapping and printing every link shows no SDK, URL or header text either.
// Transport errors are replaced at the HTTP transport, so azcore's own logs do not see them; responses
// larger than 64 KiB and authentication challenges the SDK would parse unsafely are refused there too,
// before any SDK code reads them.
//
// This is the only package allowed to import the Azure SDK for Key Vault and Entra ID.
package azurekv
