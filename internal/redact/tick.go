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

import "github.com/shadewright-labs/shadewright/internal/redact/meter"

// tick reports n steps of work to wm, the budget of the redaction it is part of (package meter, #873). Without
// the build tag redactwork nothing else counts them: the product build holds no counter and no other shared state
// for it (#840).
func tick(wm *meter.Meter, n int) { wm.Spend(n) }

// tickExpression reports n bytes handed to a regular expression to wm; without the build tag redactwork nothing
// else counts them.
func tickExpression(wm *meter.Meter, n int) { wm.Spend(n) }
