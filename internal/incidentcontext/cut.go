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

import "strings"

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
// the builder cannot tell where its first entry starts.
func dropLeadingContinuation(s string) (string, bool) {
	start := 0
	for start < len(s) {
		end := strings.IndexByte(s[start:], '\n')
		if end < 0 {
			end = len(s)
		} else {
			end += start + 1
		}
		line := strings.TrimRight(s[start:end], "\r\n")
		if line != "" && line[0] != ' ' && line[0] != '\t' && line != "-" && !strings.HasPrefix(line, "- ") &&
			!strings.HasPrefix(line, "-\t") {
			break
		}
		start = end
	}
	return s[start:], start > 0
}

// keepTail returns the longest suffix of s made of whole lines that fits in n bytes, and whether anything was
// cut. A line is whole when it starts s or follows a newline. It only ever removes text, and is applied to
// redacted text only.
func keepTail(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	if n <= 0 {
		return "", s != ""
	}
	start := len(s) - n
	if s[start-1] != '\n' {
		i := strings.IndexByte(s[start:], '\n')
		if i < 0 {
			return "", true
		}
		start += i + 1
	}
	return s[start:], true
}
