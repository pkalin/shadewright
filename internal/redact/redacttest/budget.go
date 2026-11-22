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

package redacttest

import "strings"

// LowWorkBudget is the work budget, in steps, of every entry on a redactor of LowBudget, whatever its size. Every
// entry point of package redact takes about 50 to 170 steps per byte of ordinary text, so an entry of up to about
// 3 KiB is decided and one of 12 KiB or more (OverLowBudget is 36 KiB) is withheld for its work.
const LowWorkBudget = 1 << 19

// OverLowBudget is an entry that a redactor of LowBudget withholds for its work at every entry point and that the
// package's own budget decides: "--token,}" repeated to 36 KiB, linear since #962. A test checks that no text of
// it reaches its output by looking for "--token,}--token".
var OverLowBudget = strings.Repeat("--token,}", 4096)

// lowerable is a redactor whose work budget a test can lower: *redact.Redactor. The type is named by its method so
// that package redact's own tests can use LowBudget (this package does not import redact).
type lowerable[R any] interface {
	WithWorkBudget(steps int64) R
}

// LowBudget returns a copy of r on which every entry has at most LowWorkBudget steps of work, so that a test of
// what a caller does with an entry withheld for its work (the marker, the withheld flag, the metric reason work,
// the escalation) can get one without depending on the cost of one input shape (#962, decision of 2026-11-22).
// Only tests may use it: TestLoweredBudgetOnlyInTests in package redact fails if code outside _test.go files and
// this package calls it or WithWorkBudget, or imports this package.
func LowBudget[R lowerable[R]](r R) R { return r.WithWorkBudget(LowWorkBudget) }
