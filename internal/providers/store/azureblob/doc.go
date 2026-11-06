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

// Package azureblob is the Azure Blob Storage adapter for [providers.AnalysisStore] (ADR-0013, ADR-0015).
//
// It writes each analysis record as one block blob, "<record ID>.json", in one container of one storage
// account, both from configuration and validated before anything is sent: the account name is 3 to 24
// lower-case letters and digits and the host is always https://<account>.blob.core.windows.net, so a
// misconfiguration cannot send a token to another host. It authenticates with the pod's Azure workload
// identity through azidentity (a token for the Storage audience only): no account key, no SAS, no connection
// string.
//
// Writes are create-only: every request carries If-None-Match: *, so Blob Storage refuses to replace a blob
// that exists (409 or 412) instead of overwriting it, whatever the role allows. That answer is final and maps
// to [providers.ErrConflict]: the analyzer's identity may not read records (ADR-0015), so the adapter cannot
// tell a record it wrote in an earlier attempt from another one, and never retries it.
//
// Before any network call, a record is refused ([providers.ErrRejected]) unless its ID is a lower-case name
// safe as a blob name, its data is valid JSON of at most [MaxRecordBytes], and its data passes the redactor
// unchanged: a record in which a secret pattern is still found is never sent. The caller redacts; this is the
// last check before the data leaves the pod.
//
// A write has a deadline of 30 seconds (or the caller's, if shorter), 10 seconds per try, and at most three
// tries with exponential backoff and jitter. Only throttling (429), a timeout (408), transient server errors
// (500, 502, 503, 504), a network failure and a token request that got no answer are retried, within the
// deadline and honouring Retry-After; a refused certificate, a refused identity (401, 403), a missing
// container (404), an existing blob and any other answer are final. A canceled context ends the write at
// once. Redirects are never followed. Errors are fixed text with the kind from package providers, the status
// code and Blob Storage's short error code; they never carry the token, the URL, the record or a response body.
//
// This package and package azurekv are the only ones allowed to import the Azure SDK.
package azureblob
