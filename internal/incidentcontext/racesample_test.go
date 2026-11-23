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

package incidentcontext

import (
	"hash/fnv"
	"strconv"
)

// raceSampleEvery sets the share of a generated corpus that a test checks under -race: about one input in
// raceSampleEvery. Each input builds a whole context, whose redaction the race detector's instrumentation makes
// 10 to 20 times slower, and the redaction of #845 and #873 also reads each text with the rules of the release
// before: in full, the package took about 1,185 s under -race in CI against about 356 s on main (2026-11-21,
// #873). make test runs this package a second time without -race (with the tag redactwork), where every input is
// checked. The same scheme as package redact's racesample_test.go.
const raceSampleEvery = 8

// raceSampleSeed fixes the sample, so that a failure under -race fails again on the same input.
const raceSampleSeed = "shadewright-incidentcontext-race-sample-1"

// raceKeeps says whether input i of the corpus of the test named name is checked in this run: always without
// -race, and for a fixed, seeded sample of about one input in raceSampleEvery under it.
func raceKeeps(name string, i int) bool {
	if !raceDetector {
		return true
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(raceSampleSeed + "\x00" + name + "\x00" + strconv.Itoa(i)))
	return h.Sum64()%raceSampleEvery == 0
}
