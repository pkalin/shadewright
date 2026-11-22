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
	"strings"
	"testing"
	"time"
)

// seedStrings80K is the input FuzzDiffStrings wrote when its worker died on the test runner after 27 minutes
// (#962, testdata/fuzz/FuzzDiffStrings/found-962-strings-80k): two strings of 40 KiB, "DB_PASSWORD: |" blocks
// and "credentials: |2" blocks with probes, 17 million steps (208 per byte).
var seedStrings80K = []byte("\x87\x87\x87\x8710<<<<r\xc3\xcfM0<+<<rQ")

// FuzzDiffStrings runs its oracles on an entry whose product work is under oracleStepCap, and does not on one
// above it, where they would hold a fuzzing worker near its limit of a minute (#962).
func TestDiffStringsOraclesStopAtTheirCap(t *testing.T) {
	skipUncounted(t)
	if raceDetector {
		t.Skip("entries of 80 and 240 KiB; the seed of 80 KiB runs under -race as a seed of FuzzDiffStrings")
	}
	before := diffStringsOracles.Load()
	if fails, _ := diffStrings(seedStrings80K); len(fails) > 0 {
		t.Fatal(strings.Join(fails, "\n"))
	}
	if diffStringsOracles.Load() != before+1 {
		t.Fatal("the oracles did not run on the entry of 80 KiB, under the cap")
	}
	// Six strings of 40 KiB of the same two blocks: about 35 million steps.
	a := strings.Repeat("DB_PASSWORD: |\nSECRETPROBE0000", 2731)[:40<<10]
	b := strings.Repeat("SECRETPROBE0001credentials: |2\n   SECRETPROBE0002", 1366)[:40<<10]
	items := []string{a, b, a, b, a, b}
	before = diffStringsOracles.Load()
	if fails, _ := diffStringsOf(items); len(fails) > 0 {
		t.Fatal(strings.Join(fails, "\n"))
	}
	if n := diffStringsOracles.Load() - before; n != 0 {
		t.Fatalf("the oracles ran on six strings of 40 KiB, whose work is over the cap of %d steps", int64(oracleStepCap))
	}
}

// FuzzDiffStrings fails an entry as P3, the entry kept, when the product's Strings does not return within
// productDeadline, instead of the worker dying with nothing but "hung or terminated unexpectedly" (#962).
func TestDiffStringsFailsAProductPastItsDeadline(t *testing.T) {
	old := productDeadline
	productDeadline = time.Millisecond
	t.Cleanup(func() { productDeadline = old })
	fails, _ := diffStrings(seedStrings80K)
	if len(fails) != 1 || !strings.HasPrefix(fails[0], "P3: Strings did not return within 1ms") {
		t.Fatalf("got %q, want one P3 failure for the deadline", fails)
	}
}

// returnsWithin reports a function that returns in time, one that does not, and raises a panic of the function
// in its caller.
func TestReturnsWithin(t *testing.T) {
	if !returnsWithin(time.Minute, func() {}) {
		t.Error("a function that returns at once did not return within a minute")
	}
	release := make(chan struct{})
	defer close(release)
	if returnsWithin(10*time.Millisecond, func() { <-release }) {
		t.Error("a blocked function returned within 10ms")
	}
	defer func() {
		if r, ok := recover().(string); !ok || !strings.HasPrefix(r, "boom") {
			t.Errorf("recovered %v, want the panic of the function", r)
		}
	}()
	returnsWithin(time.Minute, func() { panic("boom") })
	t.Error("the panic of the function did not reach the caller")
}
