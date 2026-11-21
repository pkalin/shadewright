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
	"hash/fnv"
	"strconv"
	"testing"
)

// raceSampleEvery sets the share of a generated corpus that a test checks under -race: about one input in
// raceSampleEvery. Each input costs the same redaction work with or without the race detector, whose
// instrumentation makes it 10 to 20 times slower, and the corpora are single goroutine apart from the entry
// points run in parallel: in full, the corpora of #840 ran the package past the 10 minutes that go test
// allows it under -race -coverprofile in CI. One in 16 was not enough once #873 metered every step: the package
// ran past 30 minutes under -race on a hosted runner (decision of 2026-11-21 on #873). make test runs this package
// a second time without -race, where every input is checked.
const raceSampleEvery = 64

// raceSampleSeed fixes the sample, so that a failure under -race fails again on the same input.
const raceSampleSeed = "shadewright-redact-race-sample-1"

// raceKeeps says whether input i of the corpus is checked by the test or entry point name in this run: always
// without -race, and for a fixed, seeded sample of about one input in raceSampleEvery under it. The sample
// depends on name, so the ten entry points together see about one input in seven of the corpus.
func raceKeeps(name string, i int) bool {
	if !raceDetector {
		return true
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(raceSampleSeed + "\x00" + name + "\x00" + strconv.Itoa(i)))
	return h.Sum64()%raceSampleEvery == 0
}

// parallelUnderRace marks t parallel under -race only. The tests that call it count no work, measure nothing under
// -race and share no state with another test, so under -race they run side by side (the race detector makes each
// 10 to 20 times slower; decision of 2026-11-21 on #873). Without -race they stay serial: that run times some of
// them (TestExpressionRescanIsChargedPerAttempt), and serial tests are not slowed by the others.
func parallelUnderRace(t *testing.T) {
	t.Helper()
	if raceDetector {
		t.Parallel()
	}
}
