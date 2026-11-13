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

// Package aggregation holds the analyzer's default aggregation window on its own, so the executor can share the
// value (ADR-0030 section 4, ResolvedBeforeRollout) without importing the analyzer's incident package, whose
// metrics would register in the executor's registry.
package aggregation

import "time"

// DefaultWindow is the analyzer's default aggregation window: it writes an Incident's status.count at most once
// per window.
const DefaultWindow = 5 * time.Minute
