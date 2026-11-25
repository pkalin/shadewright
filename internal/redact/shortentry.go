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
	"context"

	"golang.org/x/sync/semaphore"

	"github.com/shadewright-labs/shadewright/internal/redact/meter"
)

// Short entries (#1087, decisions of 2026-11-22). The collector redacts event notes, symptoms and module evidence
// values (package fingerprint, package modules) on one worker for every namespace. A fingerprint or a symptom must
// not depend on what the analyses are doing, so a short entry does not take a share of the limit on the heap of
// the redactions running at once (heaplimit.go) and never waits for it. It cannot be used for large text: an entry
// longer than MaxShortEntry is withheld (ReasonSize) before any work; the collector does not read such a note at all.
//
// The heap of the short entries is counted next to that limit, not in it: at most shortEntrySlots of them run at
// once, each holding at most ShortEntryHeap (TestHeapOfAShortEntry measures every shape known at MaxShortEntry).
// One more short entry waits only for another short entry to finish, never for an analysis.
//
// A short entry is decided by the readings of origin/main alone, without the controls view (controls.go, #852,
// decision of 2026-11-22): its callers redact, change the text and redact again, and the added reading there can
// show a key's bytes the second reading hid. A secret a control splits in an event note is shown there as before.

const (
	// MaxShortEntry is the longest entry, in bytes, the short entry points take: four times the 1,024 bytes the API
	// server allows the note of an event that has an eventTime (every event created through events.k8s.io/v1). An
	// event without one, written through the core API, is not checked for its note length there.
	MaxShortEntry = 4 << 10
	// ShortEntryHeap bounds the heap one short entry holds (TestHeapOfAShortEntry).
	ShortEntryHeap = 1 << 20
	// shortEntrySlots is how many short entries run at once.
	shortEntrySlots = 4
)

// shortEntries holds the slots of the short entries running at once.
var shortEntries = semaphore.NewWeighted(shortEntrySlots)

// ReplaceCheckedShort is ReplaceChecked for an entry of at most MaxShortEntry bytes, outside the limit on the heap
// of the redactions running at once: it never waits for an analysis. A longer entry is withheld.
func ReplaceCheckedShort(s string, repl func(kind, text string) string, between func(string) string) (out string, ok bool) {
	return defaultRedactor.ReplaceCheckedShort(s, repl, between)
}

// ReplaceCheckedShort is the package's ReplaceCheckedShort with r's rules and work budget.
func (r *Redactor) ReplaceCheckedShort(s string, repl func(kind, text string) string, between func(string) string) (out string, ok bool) {
	spans, ok := meteredShort(r, len(s), func(wm *meter.Meter) []span { return r.spansAsMain(wm, s) })
	if !ok {
		return Withheld, false
	}
	return r.replaceSpans(unstopped(), s, spans, repl, between), true
}

// meteredShort is metered for a short entry: f runs on r's budget of the entry in one of the short entries' slots,
// not in a share of the heap limit. An entry longer than MaxShortEntry is withheld before any work (ReasonSize).
func meteredShort[T any](r *Redactor, size int, f func(wm *meter.Meter) T) (out T, ok bool) {
	if size > MaxShortEntry {
		r.hooks.withheld(ReasonSize)
		return out, false
	}
	// A slot is held only for the work of one short entry, which its budget bounds, so this wait always ends and
	// takes no context.
	_ = shortEntries.Acquire(context.Background(), 1) // #nosec G104 -- Acquire fails only when its context ends
	defer shortEntries.Release(1)
	return onBudget(r, r.budgetOf(size), f)
}

// StringShort is String for an entry of at most MaxShortEntry bytes, outside the limit on the heap of the
// redactions running at once. A longer entry, or one over its work budget, is Withheld.
func StringShort(s string) string {
	return defaultRedactor.StringShort(s)
}

// StringShort is the package's StringShort with r's rules and work budget.
func (r *Redactor) StringShort(s string) string {
	out, ok := meteredShort(r, len(s), func(wm *meter.Meter) string { return render(wm, s, r.spansAsMain(wm, s)) })
	if !ok {
		return Withheld
	}
	return out
}
