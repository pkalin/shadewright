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

package released

// Counter receives the steps of work of this package in a build with the tag redactwork, so that the
// linearity tests of package redact count the work of the reading of the release before with its own. Package
// redact sets it once, when it is initialised; nothing else writes it.
var Counter func(n int)

// tick reports n steps of work to Counter.
func tick(n int) {
	if Counter != nil {
		Counter(n)
	}
}
