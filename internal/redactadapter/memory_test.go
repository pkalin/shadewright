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

package redactadapter

import (
	"fmt"
	"math"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/go-logr/logr/funcr"
)

// The analyzer logs one warning when its Go memory limit is under the one the heap limit was measured for (the
// 256 Mi install), and none at or above it or without one (#1087, decision of 2026-11-22).
func TestWarningUnderTheMeasuredMemory(t *testing.T) {
	const measuredLimit int64 = 241591910
	if MeasuredGoMemoryLimit != measuredLimit {
		t.Fatalf("MeasuredGoMemoryLimit = %d, want %d", MeasuredGoMemoryLimit, measuredLimit)
	}
	defer debug.SetMemoryLimit(debug.SetMemoryLimit(math.MaxInt64))
	for _, c := range []struct {
		name  string
		limit int64
		want  int
	}{
		{"128Mi install", 128 << 20 * 9 / 10, 1},
		{"one byte under", measuredLimit - 1, 1},
		{"256Mi install", measuredLimit, 0},
		{"512Mi install", 512 << 20 * 9 / 10, 0},
		{"no Go memory limit", math.MaxInt64, 0},
	} {
		debug.SetMemoryLimit(c.limit)
		var lines []string
		log := funcr.New(func(prefix, args string) { lines = append(lines, args) }, funcr.Options{})
		warned := WarnIfMemoryUnderMeasured(log)
		if len(lines) != c.want || warned != (c.want == 1) {
			t.Errorf("%s: %d lines, warned %v, want %d: %v", c.name, len(lines), warned, c.want, lines)
		}
		if c.want == 1 && len(lines) == 1 {
			if !strings.Contains(lines[0], "killed for its memory") {
				t.Errorf("%s: the warning does not name the risk: %s", c.name, lines[0])
			}
			fields := fmt.Sprintf(` "goMemoryLimitBytes"=%d "measuredForBytes"=%d`, c.limit, measuredLimit)
			if !strings.HasSuffix(lines[0], fields) {
				t.Errorf("%s: warning %q, want log fields %s", c.name, lines[0], fields)
			}
		}
	}
}
