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

package mainsymptom

import (
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/shadewright-labs/shadewright/internal/redact"
)

// Copied from internal/modules (rules.go and revision.go) at origin/main 24cd00a6. Do not edit.

// MaxEvidenceValueLength is the longest evidence value BoundValue returns, in bytes.
const MaxEvidenceValueLength = 256

// fluxRevision is the whole of a Git revision as Flux writes it in a status field.
var fluxRevision = regexp.MustCompile(`^(?:[A-Za-z0-9._/-]{1,255}@)?sha1:[0-9a-f]{40}$`)

// IsFluxRevision reports whether the whole of s is a Flux Git revision.
func IsFluxRevision(s string) bool {
	return fluxRevision.MatchString(s)
}

// BoundValue is modules.BoundValue of origin/main.
func BoundValue(s string) (out string) {
	defer func() {
		if recover() != nil {
			out = "[REDACTED]"
		}
	}()
	return boundValue(s)
}

// boundValue is [BoundValue] without the recover.
func boundValue(s string) string {
	if IsFluxRevision(s) {
		return s
	}
	s = Value(s)
	if len(s) <= MaxEvidenceValueLength {
		return s
	}
	cut := MaxEvidenceValueLength
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return redact.StringShort(strings.TrimRight(s[:cut], " "))
}
