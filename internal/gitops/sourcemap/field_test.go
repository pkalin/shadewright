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

package sourcemap_test

import (
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	"github.com/pkalin/shadewright/internal/gitops/sourcemap"
)

const (
	memoryKey    = "memory"
	mi256        = "256Mi"
	gi1          = "1Gi"
	e99          = "1e99"
	hugeExponent = "1e2147483647"
	e100         = "1e100"
)

var (
	memoryField   = sourcemap.Field{Steps: []sourcemap.Step{{Key: memoryKey}}, Quantity: true}
	replicasField = sourcemap.Field{Steps: []sourcemap.Step{{Key: "maxReplicaCount"}}}
)

func TestFieldEqualsComparesQuantities(t *testing.T) {
	for _, c := range []struct {
		a, b any
		want bool
	}{
		{mi256, "268435456", true},
		{mi256, int64(268435456), true},
		{mi256, float64(268435456), true},
		{"1k", "1e3", true},
		{"1.5Gi", "1536Mi", true},
		{"1E2", "100", true},
		{"+1Gi", gi1, true},
		{".5Gi", "512Mi", true},
		{e99, e99, true},
		{"9223372036854775807", "9223372036854775807", true},
		{mi256, "257Mi", false},
		{mi256, nil, false},
		{mi256, true, false},
		{mi256, 1.5, false},
	} {
		if got := memoryField.Equals(c.a, c.b); got != c.want {
			t.Errorf("Equals(%v, %v) = %v, want %v", c.a, c.b, got, c.want)
		}
		if got := memoryField.Equals(c.b, c.a); got != c.want {
			t.Errorf("Equals(%v, %v) = %v, want %v", c.b, c.a, got, c.want)
		}
	}
}

// TestFieldEqualsRefusesUnboundedQuantities is the regression test of #593: a memory value in Git with a huge
// decimal exponent made the comparison compute a power of ten of billions of digits, which ignored every
// context and outgrew the executor's 128 MiB limit. Such a value is not a quantity Shadewright reads.
func TestFieldEqualsRefusesUnboundedQuantities(t *testing.T) {
	defer debug.SetMemoryLimit(debug.SetMemoryLimit(128 << 20))
	for _, v := range []string{
		hugeExponent,
		"1E2147483647",
		"1e-2147483647",
		e100,
		"1e+100",
		"1" + strings.Repeat("0", 64),
		"0." + strings.Repeat("0", 64) + "1",
		"0x10",
		" 1Gi",
		"1Gi ",
		"1Gi\n",
		"1e",
		"Gi",
		"",
		"1Gi1",
		"1ki",
	} {
		for _, other := range []any{gi1, "1e9", "1e-9", v} {
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			done := make(chan bool, 1)
			go func() { done <- memoryField.Equals(v, other) }()
			select {
			case got := <-done:
				if got {
					t.Errorf("Equals(%q, %v) = true, want false", v, other)
				}
			case <-time.After(2 * time.Second):
				t.Fatalf("Equals(%q, %v) did not return within 2s", v, other)
			}
			runtime.ReadMemStats(&after)
			if grew := after.TotalAlloc - before.TotalAlloc; grew > 1<<20 {
				t.Errorf("Equals(%q, %v) allocated %d bytes, want at most 1 MiB", v, other, grew)
			}
			if memoryField.Comparable(v) {
				t.Errorf("Comparable(%q) = true, want false", v)
			}
		}
	}
}

func TestFieldComparable(t *testing.T) {
	for _, v := range []any{mi256, "1e18", int64(1), float64(2)} {
		if !memoryField.Comparable(v) {
			t.Errorf("Comparable(%v) = false, want true", v)
		}
	}
	for _, v := range []any{e100, 1.5, nil, true, map[string]any{}} {
		if memoryField.Comparable(v) {
			t.Errorf("Comparable(%v) = true, want false", v)
		}
	}
	replicas := replicasField
	if !replicas.Comparable(e100) || !replicas.Comparable(int64(3)) {
		t.Error("a field compared by its JSON form must read every value")
	}
}

func TestFieldCompare(t *testing.T) {
	replicas := replicasField
	for _, c := range []struct {
		field sourcemap.Field
		a, b  any
		want  int
	}{
		{memoryField, mi256, "512Mi", -1},
		{memoryField, gi1, mi256, 1},
		{memoryField, mi256, "268435456", 0},
		{replicas, int64(3), int64(5), -1},
		{replicas, int64(5), float64(3), 1},
		{replicas, float64(4), int64(4), 0},
	} {
		got, ok := c.field.Compare(c.a, c.b)
		if !ok || got != c.want {
			t.Errorf("Compare(%v, %v) = %d, %v; want %d, true", c.a, c.b, got, ok, c.want)
		}
	}
	for _, c := range []struct {
		field sourcemap.Field
		a, b  any
	}{
		{memoryField, e100, mi256}, {memoryField, mi256, nil}, {replicas, "3", int64(3)},
		{replicas, 1.5, int64(1)}, {replicas, int64(1), true},
	} {
		if _, ok := c.field.Compare(c.a, c.b); ok {
			t.Errorf("Compare(%v, %v) ordered a pair it cannot read", c.a, c.b)
		}
	}
}

// FuzzFieldEquals checks that Equals on values from Git returns quickly, is symmetric, and that a value it
// accepts equals itself.
func FuzzFieldEquals(f *testing.F) {
	for _, s := range [][2]string{
		{mi256, "268435456"}, {"1e3", "1k"}, {hugeExponent, gi1}, {"1.5Gi", "-1"}, {".5", "500m"},
		{e99, "1E-99"}, {"", "0"},
	} {
		f.Add(s[0], s[1])
	}
	f.Fuzz(func(t *testing.T, a, b string) {
		start := time.Now()
		ab := memoryField.Equals(a, b)
		ba := memoryField.Equals(b, a)
		if ab != ba {
			t.Fatalf("Equals(%q, %q) = %v but Equals(%q, %q) = %v", a, b, ab, b, a, ba)
		}
		if memoryField.Comparable(a) != memoryField.Equals(a, a) {
			t.Fatalf("Comparable(%q) and Equals(%q, %q) disagree", a, a, a)
		}
		if d := time.Since(start); d > time.Second {
			t.Fatalf("Equals(%q, %q) took %s", a, b, d)
		}
	})
}
