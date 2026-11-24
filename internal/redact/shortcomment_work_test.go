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
	"math"
	"strings"
	"testing"

	"github.com/shadewright-labs/shadewright/internal/redact/meter"
)

// shortCommentRun is about n bytes of a password flag with no value on every comment line after a sequence opened
// by a tool of the table: no flag finds a value, so each one looks over the comment lines after it.
func shortCommentRun(n int) string {
	return "[redis-cli,\n" + strings.Repeat("# -a\n", n/5+1)
}

// Comment lines after a flag with no value (#1075, decision of 2026-11-22): the comment lines a flag looks over for
// its value are looked over once in all, so the steps per byte of the rule stay the same from 4 KiB to 1.5 MiB, in
// a text and in a sequence of strings, and StringChecked releases the text of 40 KB instead of withholding it.
func TestShortCommentLinesStepsPerByte(t *testing.T) {
	const bound = 8.0
	shapes := map[string]func(n int) (string, func(*meter.Meter, string)){
		"text": func(n int) (string, func(*meter.Meter, string)) {
			return shortCommentRun(n), func(wm *meter.Meter, s string) { shortFlagScan(wm, s, func(int, int) {}) }
		},
		"strings": func(n int) (string, func(*meter.Meter, string)) {
			items := append([]string{"-v"}, strings.Split(strings.Repeat("# -a\n", n/5+1), "\n")...)
			return strings.Join(items, "\n"), func(wm *meter.Meter, s string) { shortFlagSequence(wm, s, 0) }
		},
	}
	for name, shape := range shapes {
		var first float64
		for _, size := range []int{4 << 10, 16 << 10, 32 << 10, 256 << 10, 1536 << 10} {
			in, read := shape(size)
			wm := meter.New(math.MaxInt64)
			read(wm, in)
			rule := float64(wm.Spent()) / float64(len(in))
			t.Logf("%s, %d bytes: %.2f steps per byte", name, len(in), rule)
			if rule > bound {
				t.Fatalf("%s, %d bytes: %.2f steps per byte, over %.0f", name, len(in), rule, bound)
			}
			if first == 0 {
				first = rule
			} else if rule > first*1.1 {
				t.Fatalf("%s: %.2f steps per byte at %d bytes, %.2f at 4 KiB", name, rule, len(in), first)
			}
		}
	}
	if _, ok := StringChecked(shortCommentRun(40_000)); !ok {
		t.Error("StringChecked withholds the text of 40 KB of comment lines")
	}
}
