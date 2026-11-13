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

// Package clearing decides whether a change cleared its symptom: the stable window of ADR-0030 sections 4
// and 7 as pure functions over typed objects. The verifier (#93c) samples the pods of a rollout with
// [Sample] and writes the result into the request's VerificationReport; the executor (#93b) checks that
// report with [CheckReport] and a [Counter], and decides with [Succeeds], [OOMFails] and [AtTimeout].
//
// Nothing here reads the cluster or a clock: callers pass the objects they read and the time of their own
// clock. Every span is measured on one clock. The report's span is sampledAt - windowStart, both written by
// the verifier; the executor's span is measured on its clock; the age of a report is judged by its sample
// counter on the executor's clock, never by sampledAt. Node times (container termination) are compared only
// with the report's windowStart and can only add resets. So skew between two clocks can only delay or deny
// success, and missing or stale evidence is never success.
package clearing

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
)

// Intervals and bounds of ADR-0030 section 4. They are fixed, not settings.
const (
	// PollInterval is how often the verifier samples and the executor checks while a request is Verifying.
	PollInterval = 30 * time.Second
	// FastPollInterval is how often the executor checks once both spans are long enough.
	FastPollInterval = 15 * time.Second
	// GapAfter: a stored sampledAt older than this by the verifier's clock is an observation gap.
	GapAfter = 2 * PollInterval
	// StaleAfter: a report whose sample counter has not risen for longer than this by the executor's clock is
	// stale.
	StaleAfter = 3 * PollInterval
	// TerminationMargin is subtracted from windowStart before a container's termination time is compared
	// with it, to absorb skew between the node's clock and the verifier's.
	TerminationMargin = PollInterval
	// PageSize and MaxPages bound the verifier's pod list: pages of 100, at most 2.
	PageSize = 100
	MaxPages = 2
	// MaxPods is the most pods of a rollout a report can cover, one baseline each.
	MaxPods = v1alpha1.MaxReportBaselines
	// DefaultNodeGrace is the default of the executor's --node-grace.
	DefaultNodeGrace = time.Minute
)

// Window is a verification window: the report's (status.windowStart, lastReset, resets) or the executor's own.
// Its times are all on the clock of its one writer. A window is open while Start is later than LastReset.Time:
// a reset closes it, and the next good sample or check opens it again at that sample's time.
type Window struct {
	Start     *metav1.Time
	LastReset *v1alpha1.WindowReset
	Resets    int64
}

// ReportWindow returns the window of a report's status.
func ReportWindow(s *v1alpha1.VerificationReportStatus) Window {
	return Window{Start: s.WindowStart, LastReset: s.LastReset, Resets: s.Resets}
}

func (w Window) store(s *v1alpha1.VerificationReportStatus) {
	s.WindowStart, s.LastReset, s.Resets = w.Start, w.LastReset, w.Resets
}

// Open reports whether the window has a start after its last reset.
func (w Window) Open() bool {
	return w.Start != nil && (w.LastReset == nil || w.Start.After(w.LastReset.Time.Time))
}

// Span is at - Start for an open window, both on the window's clock, and 0 for a closed one or when at is
// not after Start.
func (w Window) Span(at time.Time) time.Duration {
	if !w.Open() || !at.After(w.Start.Time) {
		return 0
	}
	return at.Sub(w.Start.Time)
}

// Good records a good sample or check at now: a closed window opens at now, an open one stays as it is.
// Start never moves back, so a clock that went back cannot reopen a window before its last reset.
func (w *Window) Good(now time.Time) {
	if w.Open() {
		return
	}
	start := seconds(now)
	if w.Start != nil && w.Start.After(start.Time) {
		start = *w.Start.DeepCopy()
	}
	w.Start = &start
}

// Reset records a sample or check that was not good: it closes the window and counts the reset. The time
// recorded is never earlier than Start or the previous reset, so a clock that went back cannot leave the
// window open.
func (w *Window) Reset(reason v1alpha1.ResetReason, now time.Time) {
	at := seconds(now)
	if w.Start != nil && w.Start.After(at.Time) {
		at = *w.Start.DeepCopy()
	}
	if w.LastReset != nil && w.LastReset.Time.After(at.Time) {
		at = *w.LastReset.Time.DeepCopy()
	}
	w.LastReset = &v1alpha1.WindowReset{Reason: reason, Class: v1alpha1.ClassOf(reason), Time: at}
	w.Resets++
}

// seconds truncates t to the precision a metav1.Time keeps in the API, so a value read back compares as the
// one written.
func seconds(t time.Time) metav1.Time {
	return metav1.NewTime(t.UTC().Truncate(time.Second))
}
