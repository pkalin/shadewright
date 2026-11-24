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

import (
	"sync"
	"sync/atomic"

	"github.com/shadewright-labs/shadewright/internal/redact/released"

	"github.com/shadewright-labs/shadewright/internal/redact/meter"
)

// The count of [Work], built only with the tag redactwork, for the linearity tests (make test); the product
// build has none of it (tick.go).
var (
	workMu   sync.Mutex
	counting atomic.Bool
	steps    atomic.Int64
	// releasedSteps are the steps of steps that the reading of the release before (package released) took.
	releasedSteps atomic.Int64
	// expressionSteps are the steps of steps that are bytes handed to a regular expression of this package.
	expressionSteps atomic.Int64
	// unmeteredSteps are the steps of steps that were counted on no budget (a nil meter.Meter): none inside an
	// entry point, as TestEveryStepIsMetered shows.
	unmeteredSteps atomic.Int64
)

// tick adds n to the count of work while [Work] counts: one turn of a scanning loop, or the bytes a library
// call (an index, a regular expression) is given to scan.
func tick(wm *meter.Meter, n int) {
	if counting.Load() {
		steps.Add(int64(n))
		unmetered(wm, n)
	}
	wm.Spend(n)
}

// unmetered counts n steps that no budget counts, while [Work] counts.
func unmetered(wm *meter.Meter, n int) {
	if wm == nil {
		unmeteredSteps.Add(int64(n))
	}
}

// Unmetered runs f and returns the steps of redaction it took on no budget: work of an entry point that its
// budget (package meter) does not see.
func Unmetered(f func()) int64 {
	_, _, _, n := workCounts(f)
	return n
}

// Work runs f and returns the steps of redaction it took. It exists only with the build tag redactwork. Calls
// are serialized, so two tests cannot mix their counts; redaction that other goroutines run meanwhile is
// counted too, so a test that calls it runs nothing else at the same time (no t.Parallel).
func Work(f func()) int64 {
	total, _ := WorkOfReleased(f)
	return total
}

// WorkOfReleased runs f and returns the steps of redaction it took, as [Work], and the part of them that the
// reading of the release before took (package released, counted by its own counter), so that the linearity
// tests can bound what the rules of this package add to that reading (#845, fourteenth decision of 2026-11-18).
func WorkOfReleased(f func()) (total, release int64) {
	total, release, _ = WorkOfExpressions(f)
	return total, release
}

// WorkOfExpressions is [WorkOfReleased] that also returns the part of the work that is bytes handed to the
// regular expressions of this package: Go's matcher costs per byte in proportion to the threads of the expression
// alive there, far more than one step, so the tests that keep the reading of a repeated text to one count these
// bytes on their own (#873, third round).
func WorkOfExpressions(f func()) (total, release, expressions int64) {
	total, release, expressions, _ = workCounts(f)
	return total, release, expressions
}

// workCounts runs f and returns its counts, read while it holds workMu: a count started after it cannot reset them
// before they are read.
func workCounts(f func()) (total, release, expressions, unmetered int64) {
	workMu.Lock()
	defer workMu.Unlock()
	steps.Store(0)
	releasedSteps.Store(0)
	expressionSteps.Store(0)
	unmeteredSteps.Store(0)
	counting.Store(true)
	defer counting.Store(false)
	f()
	return steps.Load(), releasedSteps.Load(), expressionSteps.Load(), unmeteredSteps.Load()
}

// tickExpression counts n bytes handed to a regular expression of this package: in the total and on their own.
func tickExpression(wm *meter.Meter, n int) {
	if counting.Load() {
		steps.Add(int64(n))
		expressionSteps.Add(int64(n))
		unmetered(wm, n)
	}
	wm.Spend(n)
}

// tickReleased counts n steps of the reading of the release before: in the total and in its own count.
// It is called after the release's own tick spent the steps on its meter (metered false when it had none).
func tickReleased(metered bool, n int) {
	if counting.Load() {
		steps.Add(int64(n))
		releasedSteps.Add(int64(n))
		if !metered {
			unmeteredSteps.Add(int64(n))
		}
	}
}

// The reading of the release before (package released) counts its work with this package's, and on its own.
func init() { released.Counter = tickReleased }
