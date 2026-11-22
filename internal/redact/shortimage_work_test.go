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
	"testing"

	"github.com/pkalin/shadewright/internal/redact/meter"
)

// The image row reads each object once (#1075): on a pod of many containers, the steps per byte of the short flag
// rule, the image lookup and the command read again for arguments before it included, stay the same at 4, 16
// and 32 KiB and at 1.5 MiB (thousands of containers), and under a fixed bound; String's steps per byte are
// logged with them up to 32 KiB.
func TestShortImageStepsPerByte(t *testing.T) {
	const bound = 8.0
	for _, name := range []string{"short flag: many containers, JSON", "short flag: many containers, YAML",
		"short flag: open objects with images", "short flag: YAML staircase of images",
		"short flag: command after args, JSON", "short flag: command after args, YAML",
		"short flag: open objects with commands after args", "short flag: many args keys before a command",
		"short flag: block scalar items", "short flag: anchored block scalar items", "short flag: comment lines in args",
		"short flag: escaped quotes glued", "short flag: escaped quotes never closed", "short flag: quotes glued"} {
		var first float64
		for _, size := range []int{4 << 10, 16 << 10, 32 << 10, 1536 << 10} {
			in := shortFlagShapes(size)[name]
			wm := meter.New(math.MaxInt64)
			shortFlagScan(wm, in, func(int, int) {})
			rule := float64(wm.Spent()) / float64(len(in))
			if size > 32<<10 {
				t.Logf("%s, %d bytes: the rule %.2f steps per byte", name, len(in), rule)
			} else {
				whole := float64(spentOn(budgetEntries[0], in)) / float64(len(in))
				t.Logf("%s, %d bytes: the rule %.2f steps per byte, String %.1f", name, len(in), rule, whole)
			}
			if rule > bound {
				t.Errorf("%s, %d bytes: the rule takes %.2f steps per byte, over %.0f", name, len(in), rule, bound)
			}
			if first == 0 {
				first = rule
			} else if rule > first*1.1 {
				t.Errorf("%s: %.2f steps per byte at %d bytes, %.2f at 4 KiB", name, rule, len(in), first)
			}
		}
	}
}
