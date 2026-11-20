//go:build !redactwork

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

import "testing"

// countWork skips the test: work is counted only in a build with the tag redactwork, which make test runs
// for this package.
func countWork(t *testing.T, _ func()) int64 {
	t.Helper()
	t.Skip("work is counted only with -tags redactwork (make test runs it)")
	return 0
}

// countWorkOfReleased skips the test, as countWork.
func countWorkOfReleased(t *testing.T, _ func()) (total, release int64) {
	t.Helper()
	t.Skip("work is counted only with -tags redactwork (make test runs it)")
	return 0, 0
}

// measureWork runs f and reports that work is not counted in this build, so that a fuzz target checks its other
// properties and leaves out the one about work.
func measureWork(f func()) (total, release int64, counted bool) {
	f()
	return 0, 0, false
}
