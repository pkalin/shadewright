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
