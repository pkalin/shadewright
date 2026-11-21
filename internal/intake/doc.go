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

// Package intake scores one analysis record written by an analyzer we do not run (ADR-0047). The record is
// untrusted input (invariant 4): [Scorer.Run] checks its name, size and schema, runs the redactor over it again
// and quarantines it on any hit before any model sees it, then makes one judge call of the other model family
// than the record's, with one bounded cross-check tool, under per-call, per-run and monthly token caps.
//
// The package reads one record from a [Source] and writes its spend reservations to a [Ledger]. It imports
// nothing from the controller or the GitOps executor, and its models get no tool but the cross-check.
package intake
