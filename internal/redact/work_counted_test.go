//go:build redactwork

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

// countWork returns the work ([Work]) of f.
func countWork(_ *testing.T, f func()) int64 { return Work(f) }

// countWorkOfReleased returns the work of f and the part of it the reading of the release before took
// ([WorkOfReleased]).
func countWorkOfReleased(_ *testing.T, f func()) (total, release int64) { return WorkOfReleased(f) }

// measureWork is countWorkOfReleased for a check that must not skip its input when work is not counted: it
// reports whether it counted (here always).
func measureWork(f func()) (total, release int64, counted bool) {
	total, release = WorkOfReleased(f)
	return total, release, true
}
