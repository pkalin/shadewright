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

// Package redactadapter connects redaction to the Shadewright operator.
package redactadapter

import (
	"math"
	"runtime/debug"

	"github.com/go-logr/logr"
)

// MeasuredGoMemoryLimit is 90 percent of the analyzer's default 256 Mi memory limit, the Go memory limit used
// to measure redaction heap usage.
const MeasuredGoMemoryLimit = 256 << 20 * 9 / 10

// WarnIfMemoryUnderMeasured logs a warning and returns true when the process has a Go memory limit below
// [MeasuredGoMemoryLimit]. The analyzer calls it once at startup. A large entry can hold about 117 MiB of
// heap when it runs alone, so a lower limit can put the analyzer at risk of an out-of-memory kill.
func WarnIfMemoryUnderMeasured(log logr.Logger) bool {
	gomem := debug.SetMemoryLimit(-1)
	if gomem <= 0 || gomem == math.MaxInt64 || gomem >= MeasuredGoMemoryLimit {
		return false
	}
	log.Info("Warning: the Go memory limit (GOMEMLIMIT) is under the size redaction's heap limit was measured for; "+
		"one large entry can still hold up to about 117 MiB and the analyzer may be killed for its memory. Set the "+
		"analyzer's memory limit to at least 256Mi",
		"goMemoryLimitBytes", gomem, "measuredForBytes", int64(MeasuredGoMemoryLimit))
	return true
}
