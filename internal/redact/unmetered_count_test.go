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
	"testing"
)

// Unmetered reads its count while it holds the count: another count that starts at once cannot reset it to zero
// before it is read, so a step on no budget is never reported as none (#873, review of 2026-11-21).
func TestUnmeteredReadsItsOwnCount(t *testing.T) {
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_, _, _ = WorkOfExpressions(func() {})
			}
		}
	}()
	zero := 0
	for range 2000 {
		if Unmetered(func() { tick(nil, 1) }) == 0 {
			zero++
		}
	}
	close(stop)
	wg.Wait()
	if zero > 0 {
		t.Errorf("%d of 2000 counts of one step on no budget read zero", zero)
	}
}
