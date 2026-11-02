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

// Package auth gets GitHub App installation tokens for the executor (ADR-0006, ADR-0013).
//
// It builds the App JWT (RS256, iss = client ID, issued 60 seconds in the past to absorb clock skew, valid
// for 10 minutes in total), signs its SHA-256 digest through a [providers.Signer] so the private key never
// enters the pod, and exchanges it at https://api.github.com for an installation token scoped to the one
// repository and to the permissions of design section 8.4 (contents, pull requests and issues write,
// metadata read). A [Source] caches the token and refreshes it shortly before it expires; concurrent callers
// share one refresh, and a failed refresh leaves the cache as it was.
//
// The host is pinned in code, the repository URL must be exactly https://github.com/<owner>/<repo>, and the
// installation and client IDs are validated before any request. The HTTP client never follows redirects,
// requires TLS 1.2 or later, has timeouts, and reads at most 64 KiB of any response.
//
// Neither the token nor the JWT is ever part of an error, a log line or a formatted value: [Token] prints
// as a placeholder, and errors carry fixed text plus the status code, never response text.
package auth
