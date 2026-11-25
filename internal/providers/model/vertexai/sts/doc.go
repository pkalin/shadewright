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

// Package sts is the token source of the Vertex AI adapter for clusters outside Google Cloud (ADR-0050,
// decision 2). It exchanges the analyzer's separately projected Kubernetes ServiceAccount token at Google
// STS for a federated access token, with the standard library and this module only (ADR-0013). There is no
// key file and no service account impersonation.
//
// [New] checks its [Config] without a network call and without reading the token file: the STS audience
// must name a workload identity pool provider, the token file must be a clean absolute path, the subject must
// be system:serviceaccount:<namespace>:<serviceaccount> with valid Kubernetes names and at most 127 bytes
// (Google's limit), the projection audience must be printable ASCII and not the Azure Entra ID audience
// (api://AzureADTokenExchange), and the in-cluster ranges must follow the rule of the compatible adapter's
// installation flag. A failure is [providers.ErrInvalidConfig] with fixed text.
//
// Each exchange reads the token file again, because the kubelet rotates it. The file is opened without
// blocking and must be a regular file of at most 64 KiB. A file that is missing, empty, unreadable, too large
// or not a regular file is retryable, because the kubelet swaps the projected volume while it rotates the
// token, and no request is sent. The payload of the token is decoded,
// not verified (STS verifies the signature), before anything is sent: it must be one JSON object with no
// repeated claim name, claim names are matched exactly, aud must hold exactly one audience equal to the
// projection audience and sub must equal the subject. A token of another ServiceAccount, a token for the
// Azure audience, a token with more than one audience and anything that is not a JWT is
// [providers.ErrForbidden], and no request is sent.
//
// The token is posted only to [TokenURL], as the token exchange grant asking for an access token with the
// cloud-platform [Scope]. The HTTP client has no proxy, requires TLS 1.2 or later, follows no redirect and
// connects only to public unicast addresses outside the declared in-cluster ranges (openaiwire.Guard). Each
// exchange has a deadline of 30 seconds. STS answers 408, 429 and 5xx are retryable; 400, 401, 403 and any
// other status, a redirect included, are [providers.ErrForbidden]. The kubelet replaces the token file at 80
// percent of its lifetime, so an expired assertion is not a rotation race, and no 400 is retried. A 200 must
// be one JSON object with token_type Bearer, issued_token_type the access token type when present, a
// positive expires_in (counted as at most 24 hours) and an access_token of 1 to 16 KiB of printable ASCII
// (0x21 to 0x7e): STS issues opaque bearer tokens, not JWTs. Any other 200 is retryable and nothing of it is
// cached.
//
// The access token is cached until shortly before it expires: five minutes, or half its lifetime when that
// is shorter. A [Source] is safe for concurrent use: callers that need a token while an exchange is in
// progress share it, each waits only as long as its own context allows, and the exchange goes on while
// anyone still waits, so one canceled caller fails no one else. When the last waiter leaves, the exchange is
// canceled. An answer that arrives after the exchange's context ended is discarded: no status of it is
// classified and no token of it is cached. The source does not retry; its caller bounds retries.
//
// Errors carry fixed text, the status code and their class; never the projected token, the access token,
// the file path or a response body. Nothing is logged.
package sts
