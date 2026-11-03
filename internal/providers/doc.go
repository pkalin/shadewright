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

// Package providers defines the interfaces through which Shadewright reaches every external system
// (ADR-0013): Git hosting, delivery, models, metrics, logs, notifications, analysis storage and signing.
//
// The package holds interfaces and shared types only. Each product gets its own adapter package, which is
// the only place allowed to import that product's SDK. Package fake holds in-memory implementations for
// tests. Names are neutral (a change request, not a pull request), and adapters declare what they support
// instead of the core assuming one product.
//
// Every method takes a context and returns errors that can be classified with [IsNotFound], [IsConflict],
// [IsForbidden], [IsRejected] and [IsRetryable], so callers can tell missing state, a stale base, a refusal
// for good (of the identity, or of the request as invalid) and a transient failure apart.
package providers
