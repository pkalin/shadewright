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

// Package github is the GitHub adapter of [providers.GitProvider] (ADR-0004, ADR-0013, design section 8.2).
//
// A [Provider] is bound at construction to one repository and one target branch, and authenticates with
// installation tokens from package auth. It talks to the REST API and the GraphQL endpoint at https://api.github.com only: the host
// is pinned in code, redirects are never followed, every response body is read with a bound, and error
// statuses are classified from the status code and headers without reading the body (the only exception is
// a 403 without rate limit headers, whose body is searched, never echoed, for a secondary rate limit).
//
// Scope of every method:
//
//   - Writes go to head branches named shadewright/<name> only, never to the target branch or any other.
//   - CreateBranch accepts only a base commit that is on the target branch (the head or an ancestor of it),
//     so a branch cannot be started from a commit of a fork or of another branch.
//   - CommitFile builds the commit with the Git data API (blob, tree, commit, each content addressed, so a
//     retry repeats no work; the file keeps its mode) and moves the branch with GraphQL updateRefs, an atomic
//     compare-and-swap on the expected head without force. A head that moved in any direction, a rewind to
//     an ancestor included, is ErrConflict; the update never forces.
//   - Change requests are opened, found and closed only when their head is a shadewright/ branch of this
//     repository and their base is the target branch.
//   - ReadFile resolves the path through the trees of the commit, refuses anything that is not a regular file,
//     checks the size before downloading the blob, reads at most MaxFileBytes+1 bytes and checks the content
//     against its Git hash.
//
// Writes are idempotent: creating a branch that already exists at the same commit succeeds, and opening a
// change request for a head branch that already has an open one returns that one.
//
// Transient failures (network errors, timeouts, 408, 429, 5xx and rate limits) are retried with exponential
// backoff and jitter up to a bounded number of attempts. A delay GitHub asks for (Retry-After, an exhausted
// quota with X-RateLimit-Reset, or the one-minute fallback for a secondary rate limit) is waited for when it
// is short; a longer one is returned as a [providers.RetryableError] carrying the delay, so the caller can
// requeue instead of blocking.
//
// A 403 that is not a rate limit (a permission the App lacks, a rule of the repository) is
// [providers.ErrForbidden], and a 422 that reading the state does not resolve (an existing branch at the same
// commit, an open pull request of the branch) is [providers.ErrRejected]. Both are final and never retried.
//
// Errors carry fixed text, the status code and their class (not found, conflict, forbidden, rejected, too
// large, retryable, canceled or deadline exceeded); never a URL, a header, response text, a repository or
// branch name, or a token.
//
// Auto-merge (design section 8.3) is not part of this adapter yet: Capabilities reports AutoMerge false and
// EnableAutoMerge returns an error.
package github
