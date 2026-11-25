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

package modules

import "regexp"

// fluxRevision is the whole of a Git revision as Flux writes it in a status field: an optional reference
// name, "@", then "sha1:" and exactly 40 lower-case hex digits ("main@sha1:1f3c...", "refs/heads/main@sha1:...").
var fluxRevision = regexp.MustCompile(`^(?:[A-Za-z0-9._/-]{1,255}@)?sha1:[0-9a-f]{40}$`)

// IsFluxRevision reports whether the whole of s is a Flux Git revision. [BoundValue] keeps such a value
// as evidence. The core redactor still hides the commit hash in free text, including when a message
// contains a valid revision.
func IsFluxRevision(s string) bool {
	return fluxRevision.MatchString(s)
}
