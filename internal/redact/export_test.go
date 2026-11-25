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
	"github.com/shadewright-labs/shadewright/internal/redact/redacttest"
)

// EntryCase is the shared block corpus case used by entry-point tests.
type EntryCase = redacttest.EntryCase

// EntryCorpus returns the shared block corpus.
var EntryCorpus = redacttest.EntryCorpus

// RaceKeeps selects the shared seeded sample under the race detector.
var RaceKeeps = redacttest.RaceKeeps

// RaceSampleEvery is the share checked under the race detector.
const RaceSampleEvery = redacttest.RaceSampleEvery

// RaceDetector reports whether this binary uses the race detector.
const RaceDetector = redacttest.RaceDetector

// ParallelUnderRace marks independent tests parallel under the race detector.
var ParallelUnderRace = redacttest.ParallelUnderRace

// MaxShownBytes is maxShownBytes, for TestReleasedShownMatchesValue.
const MaxShownBytes = maxShownBytes
