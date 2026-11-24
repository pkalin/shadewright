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
	"testing"
	"time"

	"github.com/shadewright-labs/shadewright/internal/redact"
	"github.com/shadewright-labs/shadewright/internal/redact/redacttest"
)

// The redactions of a build wait for the limit on the heap of the redactions running at once only until the build
// deadline (#1087, decision of 2026-11-22): with the limit held whole by another redaction, a build with a short
// deadline returns soon after it, its blocks withheld and named as failed sources, not after the redactor's own
// longest wait of two minutes for each block.
func TestBuildDeadlineEndsTheWaitForTheHeapLimit(t *testing.T) {
	red, err := redact.New()
	if err != nil {
		t.Fatal(err)
	}
	redacttest.HoldHeapLimit(t, func(block func()) {
		red.PiecesFitting([]redact.Piece{{Text: "pwd=abcdef", Data: true}}, func([]string) bool { block(); return true })
	}, func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		_, ok := red.WithContext(ctx).StringChecked("x")
		return !ok
	})

	b := newBuilder(t, reader(t, webObjects()...), webSources(), func(c *Config) {
		c.BuildTimeout = 300 * time.Millisecond
	})
	type result struct {
		c   *Context
		err error
	}
	got := make(chan result, 1)
	start := time.Now()
	go func() {
		c, err := b.Build(context.Background(), incident(deploymentRef()))
		got <- result{c, err}
	}()
	select {
	case r := <-got:
		if r.err != nil {
			t.Fatalf("the build was abandoned: %v", r.err)
		}
		if took := time.Since(start); took > 10*time.Second {
			t.Errorf("the build took %s with a deadline of 300ms", took)
		}
		if one(t, r.c, SectionStatus).Availability != CouldNotFetch || !r.c.Escalate() {
			t.Errorf("with the limit held past the deadline: failed %v, blocks %+v", r.c.FailedSources, r.c.Blocks)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the build's redactions waited past its deadline")
	}
}
