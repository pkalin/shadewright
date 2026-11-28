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

package incidentcontext

import (
	"strings"

	"github.com/shadewright-labs/shadewright/internal/logcut"
	"github.com/shadewright-labs/shadewright/internal/providers"
)

// keepHead returns the longest prefix of s made of whole lines that fits in n bytes, and whether anything
// was cut. A line is whole when it ends with a newline, or ends s. It only ever removes text, and is applied
// to redacted text only.
func keepHead(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	if n <= 0 {
		return "", s != ""
	}
	i := strings.LastIndexByte(s[:n], '\n')
	if i < 0 {
		return "", true
	}
	return s[:i+1], true
}

// dropLeadingContinuation removes the lines at the start of a log excerpt that may continue an entry whose
// first line is not in the excerpt: blank lines, lines that start with a space or a tab, and YAML sequence
// entries ("-" alone or followed by a blank). Where an excerpt starts was chosen before any redaction, by the
// log source's cut to its byte bound and by the API server's tailLines, so such a line may be the value of a
// secret whose key line ("password: |", "- name: TOKEN") was left out, and the redactor does not recognise it
// alone. It reports whether it dropped anything. An excerpt whose every line is indented is dropped whole:
// the builder cannot tell where its first entry starts, unless the source read that entry's first line and
// redacted the excerpt with it (providers.LogExcerpt.HeaderLeftOut), which RenderLog checks.
func dropLeadingContinuation(s string) (string, bool) {
	start := providers.LeadingContinuation(s)
	return s[start:], start > 0
}

// cutLog is the one cut of a log excerpt in bytes ([logcut.Tail]) on redacted text a reader has already read:
// the newest whole lines of s within n bytes from a line a reader keeps, the first line of an entry ahead of its
// newest continuation lines, or, when no non-blank whole line fits, the tail of the last non-blank line. It
// reports whether anything was left out. It only ever removes text.
func cutLog(s string, n int) (string, bool) {
	out, c := cutLogKind(s, n)
	return out, c.Cut()
}

// cutLogKind is [cutLog] that returns what the cut kept.
func cutLogKind(s string, n int) (string, logcut.Cut) {
	c := logcut.Tail(s, n, nil)
	return c.Text(s), c
}

// lastNLines returns the suffix of s holding its last n lines (each ended by a newline, plus any trailing
// line without one). It only ever removes whole lines from the start of already-redacted text.
func lastNLines(s string, n int) string {
	if n <= 0 || s == "" {
		return ""
	}
	i := len(s)
	if s[i-1] == '\n' {
		i--
	}
	for seen := 0; i > 0; i-- {
		if s[i-1] == '\n' {
			seen++
			if seen == n {
				return s[i:]
			}
		}
	}
	return s
}

// KeepHead is keepHead for other packages that bound redacted text the same way (internal/tools).
func KeepHead(s string, n int) (string, bool) { return keepHead(s, n) }
