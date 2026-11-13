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
// and 7 as pure functions over typed objects. The verifier (#682) samples the pods of a rollout with
// [Sample] and writes the result into the request's VerificationReport; the executor (#683) checks that
// report with [CheckReport] and a [Counter], and decides with [Succeeds], [OOMFails] and [AtTimeout].
//
// Nothing here reads the cluster or a clock: callers pass the objects they read and the time of their own
// clock. Every span is measured on one clock. The report's span is sampledAt - windowStart, both written by
// the verifier; the executor's span is measured on its clock; the age of a report is judged by its sample
// counter on the executor's clock, never by sampledAt. Node times (container termination) are compared only
// with the report's windowStart and can only add resets, never recorded as trouble. So skew between two
// clocks can only delay or deny success, and missing or stale evidence is never success.
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

// Class is the class of a reset reason (ADR-0030 section 4). It follows from the reason and is not stored.
type Class string

// The reset classes.
const (
	// ClassTrouble: a restart was observed. Recorded trouble ends a request Failed at the timeout; what is
	// recorded is set out at [Window.Reset].
	ClassTrouble Class = "Trouble"
	// ClassNoEvidence: nothing could be shown either way.
	ClassNoEvidence Class = "NoEvidence"
)

// ClassOf returns the class of a reset reason: ClassTrouble for Restarted, ClassNoEvidence for every other.
func ClassOf(r v1alpha1.ResetReason) Class {
	if r == v1alpha1.ResetRestarted {
		return ClassTrouble
	}
	return ClassNoEvidence
}

// Window is a verification window: windowStart, the last reset, the reset count and the trouble recorded.
// Its times are all on the clock of its one writer. A window is open while Start is later than LastReset.Time:
// a reset closes it, and the next good sample or check opens it again at that sample's time.
//
// The report's window is stored in its status ([ReportWindow]). The checks here also take the executor's own
// window as a Window, held in memory, but its resets carry the executor's reasons (NotAvailable, StaleReport,
// ReportMismatch), which the enum of the report's WindowReset.reason refuses: the executor keeps its window in
// request status with its own type (status.verification, a VerificationReset), not in this API type.
type Window struct {
	Start     *metav1.Time
	LastReset *v1alpha1.WindowReset
	Resets    int64
	// LastTrouble is the last reset of the trouble class recorded as durable trouble ([Window.Reset]) and
	// TroubleResets their count. A later no-evidence reset leaves both as they are.
	LastTrouble   *v1alpha1.WindowReset
	TroubleResets int64
}

// ReportWindow returns the window of a report's status.
func ReportWindow(s *v1alpha1.VerificationReportStatus) Window {
	return Window{Start: s.WindowStart, LastReset: s.LastReset, Resets: s.Resets,
		LastTrouble: s.LastTroubleReset, TroubleResets: s.TroubleResets}
}

func (w Window) store(s *v1alpha1.VerificationReportStatus) {
	s.WindowStart, s.LastReset, s.Resets = w.Start, w.LastReset, w.Resets
	s.LastTroubleReset, s.TroubleResets = w.LastTrouble, w.TroubleResets
}

// Troubled reports whether the window recorded durable trouble.
func (w Window) Troubled() bool {
	return w.TroubleResets > 0 || w.LastTrouble != nil
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

// Reset records a sample or check that was not good: it closes the window and counts the reset. A reset of
// the trouble class is also recorded as durable trouble (LastTrouble, TroubleResets), but only once the window
// first opened (Start is set): a restart before the first good sample, such as a new pod restarting during a
// slow rollout, resets nothing the window had shown. A crash loop from the very start never opens the window;
// the executor ends it with RolloutStuck, not with this record. The time recorded is never earlier than Start
// or the previous reset, so a clock that went back cannot leave the window open or move a reset back.
func (w *Window) Reset(reason v1alpha1.ResetReason, now time.Time) {
	w.reset(reason, now, ClassOf(reason) == ClassTrouble)
}

// ResetUnrecorded is Reset without the durable trouble record, for a Restarted reset that only a node time
// shows (a termination at or after windowStart minus TerminationMargin, with no restart count above its
// baseline): a node clock ahead of the verifier's can delay or deny success, never fail a request.
func (w *Window) ResetUnrecorded(reason v1alpha1.ResetReason, now time.Time) {
	w.reset(reason, now, false)
}

func (w *Window) reset(reason v1alpha1.ResetReason, now time.Time, trouble bool) {
	at := seconds(now)
	if w.Start != nil && w.Start.After(at.Time) {
		at = *w.Start.DeepCopy()
	}
	if w.LastReset != nil && w.LastReset.Time.After(at.Time) {
		at = *w.LastReset.Time.DeepCopy()
	}
	w.LastReset = &v1alpha1.WindowReset{Reason: reason, Time: at}
	w.Resets++
	if trouble && w.Start != nil {
		w.LastTrouble = w.LastReset.DeepCopy()
		w.TroubleResets++
	}
}

// seconds truncates t to the precision a metav1.Time keeps in the API, so a value read back compares as the
// one written.
func seconds(t time.Time) metav1.Time {
	return metav1.NewTime(t.UTC().Truncate(time.Second))
}
