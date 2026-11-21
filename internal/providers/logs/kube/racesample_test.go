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

package kube

import (
	"hash/fnv"
	"testing"
)

// raceSampleEvery sets the share of the tests on logs of about MaxReadBytes that run under -race: about one in
// raceSampleEvery. Each redacts one or two reads of up to 1 MiB, which the race detector's instrumentation
// makes 10 to 20 times slower, and the redaction of #845 does about a quarter more work per byte than before:
// in full they took more than five minutes of the package under -race on a loaded machine. They are single
// goroutine apart from the fake API server; make test runs this package a second time without -race (and with
// the tag redactwork), where every one of them runs. The same scheme as package redact's racesample_test.go.
const raceSampleEvery = 3

// raceSampleSeed fixes the sample, so that a failure under -race fails again in the same test.
const raceSampleSeed = "shadewright-logs-kube-race-sample-1"

// largeLog skips the test under -race unless it is in the seeded sample; without -race it always runs.
func largeLog(t *testing.T) {
	t.Helper()
	if !raceDetector {
		return
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(raceSampleSeed + "\x00" + t.Name()))
	if h.Sum64()%raceSampleEvery != 0 {
		t.Skip("a log of about MaxReadBytes: outside the seeded sample under -race (racesample_test.go)")
	}
}
