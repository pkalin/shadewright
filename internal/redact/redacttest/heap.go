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

import (
	"sync"
	"testing"
	"time"
)

// maxHolders bounds the entries HoldHeapLimit starts: each takes at least redact.HeapFixed (4 MiB) of the limit,
// so 256 of them hold a limit of up to 1 GiB, more than a test binary's.
const maxHolders = 256

// HoldHeapLimit holds the whole limit on the heap of the redactions running at once (package redact, heaplimit.go)
// until the test ends or the returned function is called, so that a test can show what a caller does while an
// analysis's redaction holds it. It holds it with short entries, each cheap under the race detector: start runs
// one redaction whose callback calls block (redact.Redactor.PiecesFitting with block in its fits function), which
// keeps that entry's share until the limit is freed; held reports whether an ordinary entry can no longer run (a
// redaction under a context that ends at once is withheld). Entries are started until held is true; the test fails
// when maxHolders entries do not hold it.
func HoldHeapLimit(t testing.TB, start func(block func()), held func() bool) (free func()) {
	t.Helper()
	release := make(chan struct{})
	var (
		wg   sync.WaitGroup
		once sync.Once
	)
	free = func() {
		once.Do(func() {
			close(release)
			wg.Wait()
		})
	}
	t.Cleanup(free)
	for range maxHolders {
		entered := make(chan struct{})
		var enter sync.Once
		wg.Go(func() {
			start(func() {
				enter.Do(func() { close(entered) })
				<-release
			})
		})
		select {
		case <-entered:
		case <-time.After(time.Second): // it waits for the limit: the entries before it hold all of it
		}
		if held() {
			return free
		}
	}
	free()
	t.Fatalf("%d entries did not hold the heap limit", maxHolders)
	return free
}
