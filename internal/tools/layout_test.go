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
	"slices"
	"strings"
	"testing"
	"time"
)

// The sort of a map's keys is the one step of a tool result that is not linear (#845, fifth decision of
// 2026-11-18): n keys take O(n log n) comparisons, each reading at most the bytes two keys share at their start
// and one more. The worst case for the bytes read is many keys with one long shared start; here 128 to 8,192
// keys share 4 KiB. The test counts the comparisons of the same sort on the same keys and fails when they pass
// 2 n log2(n); the time of laying out the map, per byte of its keys, is logged.
func TestMapKeySortIsBounded(t *testing.T) {
	prefix := strings.Repeat("k", 4096)
	for _, n := range []int{128, 512, 2048, 8192} {
		m := make(map[string]string, n)
		keys := make([]string, 0, n)
		size := 0
		for i := range n {
			k := fmt.Sprintf("%s%06d", prefix, (i*7919)%n)
			m[k] = "v"
			keys = append(keys, k)
			size += len(k) + 1
		}
		compares := 0
		slices.SortFunc(keys, func(a, b string) int { compares++; return strings.Compare(a, b) })
		bound := 2 * float64(n) * math.Log2(float64(n))
		v := reflect.ValueOf(&m).Elem()
		var l layout
		start := time.Now()
		l.mapEntries(v, "")
		took := time.Since(start)
		t.Logf("%d keys sharing 4 KiB: %d comparisons (bound %.0f), %.2f ns per byte of the keys",
			n, compares, bound, float64(took.Nanoseconds())/float64(size))
		if float64(compares) > bound {
			t.Errorf("%d keys: %d comparisons, more than 2 n log2(n) = %.0f", n, compares, bound)
		}
	}
}
