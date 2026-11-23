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

package collector

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pkalin/shadewright/internal/modules"
	"github.com/pkalin/shadewright/internal/redact"
	"github.com/pkalin/shadewright/internal/redact/redacttest"
)

// The collector never waits for the limit on the heap of the redactions running at once (#1087, decision of
// 2026-11-22). With an analysis holding the whole limit, an event with a 5 KiB note goes through the event
// reconciler (fromEvent, observation: the fingerprint and the shown symptom) and returns at once, with the marker
// as its symptom; a 5 KiB module evidence value (modules.BoundValue, which EvaluateSource applies) does too.
func TestLongNoteEventDoesNotWaitForTheHeapLimit(t *testing.T) {
	red, err := redact.New()
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, Config{})
	ev := warning("long", 0)
	ev.Note = "Failed: password=s3cr3tLongValue " + strings.Repeat("x", 5<<10-len("Failed: password=s3cr3tLongValue "))
	h.create(ev)

	redacttest.HoldHeapLimit(t, func(block func()) {
		red.PiecesFitting([]redact.Piece{{Text: "pwd=abcdef", Data: true}}, func([]string) bool { block(); return true })
	}, func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		_, ok := red.WithContext(ctx).ReplaceChecked(ev.Note, func(string, string) string { return "" }, func(s string) string { return s })
		return !ok
	})

	done := make(chan error, 1)
	value := make(chan string, 1)
	go func() {
		_, err := h.events.Reconcile(t.Context(), reqFor(ev.Name))
		done <- err
		value <- modules.BoundValue(ev.Note)
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("event reconcile: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the collector waited for the heap limit on an event with a 5 KiB note")
	}
	if got := h.one().Symptom; got != "<long note: 5120 bytes>" {
		t.Errorf("symptom of a 5 KiB note = %q, want the marker with its length", got[:min(len(got), 80)])
	}
	select {
	case got := <-value:
		if got != "<long value: 5120 bytes>" {
			t.Errorf("evidence value of 5 KiB = %q, want the marker with its length", got[:min(len(got), 80)])
		}
	case <-time.After(5 * time.Second):
		t.Fatal("modules.BoundValue waited for the heap limit on a 5 KiB value")
	}
}
