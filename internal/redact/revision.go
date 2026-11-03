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

package redact

import "regexp"

// fluxRevision is the whole of a Git revision as Flux writes it in a status field: an optional reference
// name, "@", then "sha1:" and exactly 40 lower-case hex digits ("main@sha1:1f3c...", "refs/heads/main@sha1:...").
var fluxRevision = regexp.MustCompile(`^(?:[A-Za-z0-9._/-]{1,255}@)?sha1:[0-9a-f]{40}$`)

// IsFluxRevision reports whether the whole of s is a Flux Git revision. [String] still redacts the commit
// hash wherever it appears in text, because an attacker who writes "sha1:" in front of a secret in a message
// must not be able to keep the secret. A caller that reads a value from a revision field (such as
// status.lastAppliedRevision) may keep that value unredacted when this returns true: the value is then only
// a reference name and a commit, and holds nothing else.
func IsFluxRevision(s string) bool {
	return fluxRevision.MatchString(s)
}
