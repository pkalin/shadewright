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

	"github.com/pkalin/shadewright/internal/redact/released"
)

// The count of [Work], built only with the tag redactwork, for the linearity tests (make test); the product
// build has none of it (tick.go).
var (
	workMu   sync.Mutex
	counting atomic.Bool
	steps    atomic.Int64
	// releasedSteps are the steps of steps that the reading of the release before (package released) took.
	releasedSteps atomic.Int64
)

// tick adds n to the count of work while [Work] counts: one turn of a scanning loop, or the bytes a library
// call (an index, a regular expression) is given to scan.
func tick(n int) {
	if counting.Load() {
		steps.Add(int64(n))
	}
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
	workMu.Lock()
	defer workMu.Unlock()
	steps.Store(0)
	releasedSteps.Store(0)
	counting.Store(true)
	defer counting.Store(false)
	f()
	return steps.Load(), releasedSteps.Load()
}

// tickReleased counts n steps of the reading of the release before: in the total and in its own count.
func tickReleased(n int) {
	if counting.Load() {
		steps.Add(int64(n))
		releasedSteps.Add(int64(n))
	}
}

// The reading of the release before (package released) counts its work with this package's, and on its own.
func init() { released.Counter = tickReleased }
