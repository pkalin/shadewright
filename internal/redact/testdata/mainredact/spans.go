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

// Package mainredact is the redactor of internal/redact as it is on main at commit 28a8212c, before #769:
// redact.go and extent.go are copies of that commit with only the package name changed, and this file is
// added. The differential tests of internal/redact (monotonic_test.go) check that each input byte this
// matcher redacts is still redacted. It lives under testdata so that the go tool, the linter and the
// coverage report leave it out of ./..., and it is never imported outside tests. Do not change it: a new
// baseline is a new copy from the commit it names.
package mainredact

// Spans returns the byte ranges of s that the redactor removes, sorted and disjoint.
func Spans(s string) [][2]int {
	var out [][2]int
	for _, sp := range defaultRedactor.spans(s) {
		out = append(out, [2]int{sp.start, sp.end})
	}
	return out
}
