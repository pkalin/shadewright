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

package tools

import (
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
)

// A measurement, not a check that can fail on its own (#845, sixth decision of 2026-11-18, point 6): the sort of
// a map's keys in mapEntries is the one step of get_resource's layout that is not linear, O(n log n)
// comparisons of n keys, each reading at most the bytes two keys share at their start and one more. A comparison
// sort keeps that bound by construction; this counts the comparisons of the sort mapEntries runs (through
// keyCompare) for 128 to 8,192 keys sharing 4 KiB and logs them with the bound 2 n log2(n) and the time per
// byte of the keys, so a change of the sort shows up in the log.
func TestMeasureMapKeySort(t *testing.T) {
	prefix := strings.Repeat("k", 4096)
	compares := 0
	defer func(f func(a, b string) int) { keyCompare = f }(keyCompare)
	keyCompare = func(a, b string) int { compares++; return strings.Compare(a, b) }
	for _, n := range []int{128, 512, 2048, 8192} {
		m := make(map[string]string, n)
		size := 0
		for i := range n {
			k := fmt.Sprintf("%s%06d", prefix, (i*7919)%n)
			m[k] = "v"
			size += len(k) + 1
		}
		compares = 0
		var o object
		start := time.Now()
		o.mapEntries(reflect.ValueOf(&m).Elem())
		took := time.Since(start)
		t.Logf("%d keys sharing 4 KiB: %d comparisons (2 n log2(n) = %.0f), %.2f ns per byte of the keys",
			n, compares, 2*float64(n)*math.Log2(float64(n)), float64(took.Nanoseconds())/float64(size))
	}
}
