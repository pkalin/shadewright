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

// tick reports n steps of work. Without the build tag redactwork nothing counts them: the product build holds
// no counter and no other shared state for it (#840).
func tick(int) {}

// tickExpression reports n bytes handed to a regular expression; without the build tag redactwork nothing counts
// them.
func tickExpression(int) {}
