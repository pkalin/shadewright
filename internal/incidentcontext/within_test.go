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
	"context"
	"strings"
	"testing"

	"github.com/shadewright-labs/shadewright/internal/providers/fake"
)

func TestBuildWithinAboveTheSectionSum(t *testing.T) {
	src := webSources()
	key := fake.LogKey{Namespace: testNS, Pod: crashPod, Container: appName, Previous: true}
	line := strings.Repeat("<", 50) + "\n"
	src.logs.Set(key, strings.Repeat(line, 400)+"tail-marker\n")
	b := newBuilder(t, reader(t, webObjects()...), src, nil)
	inc := incident(deploymentRef())
	retained := func(limit int) int {
		t.Helper()
		c, err := b.BuildWithin(t.Context(), inc, limit)
		if err != nil {
			t.Fatal(err)
		}
		if n := len(c.Render()); n > limit {
			t.Fatalf("context renders to %d bytes, limit %d", n, limit)
		}
		return strings.Count(one(t, c, SectionLogs).Text, line)
	}
	l := DefaultLimits()
	sum := l.Summary + l.Status + l.Logs + l.Metrics + l.Git + l.Policy
	near, above := retained(sum+1), retained(2*sum)
	if above <= near {
		t.Errorf("retained %d lines near the section sum and %d above it", near, above)
	}
}

// #981: the total a build is given (maxContextBytes) bounds the context in place of the configured one; zero
// keeps the configured total, and a total outside the settings' range is refused before any read.
func TestBuildWithinBoundsTheContextAtTheGivenTotal(t *testing.T) {
	src := webSources()
	key := fake.LogKey{Namespace: testNS, Pod: crashPod, Container: appName, Previous: true}
	src.logs.Set(key, strings.Repeat("a log line that repeats itself\n", 4<<10)+"panic: the last line\n")
	b := newBuilder(t, reader(t, webObjects()...), src, nil)
	inc := incident(deploymentRef())

	def, err := b.BuildWithin(context.Background(), inc.DeepCopy(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if def.Truncated || def.Size() <= 8192 || def.Size() > DefaultTotalBytes {
		t.Fatalf("with the configured total: %d bytes, truncated %t; want over 8192 and within %d, not truncated",
			def.Size(), def.Truncated, DefaultTotalBytes)
	}

	for _, total := range []int{MinTotalBytes, 8192} {
		c, err := b.BuildWithin(context.Background(), inc.DeepCopy(), total)
		if err != nil {
			t.Fatal(err)
		}
		if c.Size() > total || !c.Truncated {
			t.Errorf("within %d: %d bytes, truncated %t", total, c.Size(), c.Truncated)
		}
	}
	// The builder's own bound is not changed by a build with another total.
	if again := build(t, b, inc.DeepCopy()); again.Truncated || again.Size() != def.Size() {
		t.Errorf("a later build is %d bytes, truncated %t; want %d as before", again.Size(), again.Truncated, def.Size())
	}

	for _, total := range []int{-1, MinTotalBytes - 1, MaxLimitBytes + 1} {
		if c, err := b.BuildWithin(context.Background(), inc.DeepCopy(), total); err == nil {
			t.Errorf("total %d: built %d bytes, want an error", total, c.Size())
		}
	}
}
