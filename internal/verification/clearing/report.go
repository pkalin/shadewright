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

package clearing

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
)

// Expected is what the executor holds and reads itself, against which it checks a report.
type Expected struct {
	// RequestUID is the request's UID.
	RequestUID types.UID
	// MergeCommitSHA is the merge commit of the request's receipt.
	MergeCommitSHA string
	// TargetUID is the UID of the target the executor read in this reconcile.
	TargetUID types.UID
	// TemplateRevision is the request's status.rollout.templateRevision.
	TemplateRevision string
	// Container is the action's container (spec.action.setContainerMemoryLimit.container), empty for an action
	// that names none.
	Container string
	// Replicas is the target's spec.replicas the executor read in this reconcile.
	Replicas int32
}

// Freshness is what a [Counter] knows about the age of a report, on the executor's clock.
type Freshness struct {
	// Fresh is true when the report's counter is higher than the one read at a check made at most one poll
	// interval earlier: the report was written after that check.
	Fresh bool
	// Stale is true when the counter has not risen for more than StaleAfter.
	Stale bool
}

// maxChecks bounds the checks a Counter remembers. Checks come every FastPollInterval or more seldom, and only
// those of the last poll interval matter.
const maxChecks = 8

type check struct {
	sample int64
	at     time.Time
}

// Counter is the executor's memory of a report's sample counter (ADR-0030 section 4): per check, the counter
// it read and the time of the check by its own clock. It lives in memory only; a restart of the executor
// forgets it, which delays success by one check. The zero value is ready to use. It is not safe for
// concurrent use.
type Counter struct {
	report  types.UID
	highest int64
	risenAt time.Time
	checks  []check
}

// Observe records a check at now (the executor's clock) that read sample from the report with UID report,
// and returns the report's freshness. A report with another UID than the one remembered (deleted and
// created again) starts the memory over, as a restart would.
func (c *Counter) Observe(report types.UID, sample int64, now time.Time) Freshness {
	if c.report != report || len(c.checks) == 0 {
		*c = Counter{report: report, highest: sample, risenAt: now, checks: []check{{sample, now}}}
		return Freshness{}
	}
	var f Freshness
	for _, ch := range c.checks {
		if age := now.Sub(ch.at); age >= 0 && age <= PollInterval && sample > ch.sample {
			f.Fresh = true
		}
	}
	if sample > c.highest {
		c.highest, c.risenAt = sample, now
	}
	f.Stale = now.Sub(c.risenAt) > StaleAfter
	c.checks = append(c.checks, check{sample, now})
	// Keep only the checks that can still prove freshness, at most maxChecks.
	c.checks = keepRecent(c.checks, now)
	return f
}

func keepRecent(checks []check, now time.Time) []check {
	kept := checks[:0]
	for _, ch := range checks {
		if age := now.Sub(ch.at); age >= 0 && age <= PollInterval {
			kept = append(kept, ch)
		}
	}
	if len(kept) > maxChecks {
		kept = kept[len(kept)-maxChecks:]
	}
	return kept
}

// CheckReport applies the report's trust contract (ADR-0030 section 7) and returns why the report is no
// evidence, or "" when it is: a missing report or one whose counter stopped rising is StaleReport; a spec
// that differs from what the executor holds is ReportMismatch; a report that does not cover every desired
// replica, or whose desired replicas differ from the target's spec.replicas as the executor read it, is
// PartialRollout, and so is any report for a target scaled to zero, which has no pods to cover. The returned reason resets the executor's own window.
func CheckReport(r *v1alpha1.VerificationReport, exp Expected, f Freshness) v1alpha1.ResetReason {
	switch {
	case r == nil || f.Stale:
		return v1alpha1.ResetStaleReport
	case !matches(r, exp):
		return v1alpha1.ResetReportMismatch
	case exp.Replicas < 1 || r.Status.PodsCovered != r.Status.DesiredReplicas || r.Status.DesiredReplicas != exp.Replicas:
		return v1alpha1.ResetPartialRollout
	}
	return ""
}

// matches reports whether the report is bound to the request, its merge, the target the executor reads, the
// template revision of the rollout and the action's container. A report naming another container (or none)
// would skip the OOM rule and the memory limit check, so it is no evidence.
func matches(r *v1alpha1.VerificationReport, exp Expected) bool {
	s := r.Spec
	return exp.RequestUID != "" && types.UID(s.RequestUID) == exp.RequestUID &&
		exp.MergeCommitSHA != "" && s.MergeCommitSHA == exp.MergeCommitSHA &&
		exp.TargetUID != "" && types.UID(s.TargetUID) == exp.TargetUID &&
		exp.TemplateRevision != "" && s.TemplateRevision == exp.TemplateRevision &&
		s.Container == exp.Container
}

// StableFor returns the effective stableFor (ADR-0030 section 4, design 5.1): the larger of the request's
// spec.verification.stableFor (default 3 minutes when unset) and the action's minimum. A request can only make
// it stricter.
func StableFor(requested *metav1.Duration, action v1alpha1.ActionType) time.Duration {
	d := DefaultStableFor
	if requested != nil {
		d = requested.Duration
	}
	return max(d, MinStableFor(action))
}

// The stableFor defaults and minimums.
const (
	// DefaultStableFor is the default of spec.verification.stableFor.
	DefaultStableFor = 3 * time.Minute
	// MinStableForMemory is the minimum for SetContainerMemoryLimit.
	MinStableForMemory = 10 * time.Minute
	// MinStableForKeda is the minimum for AdjustKedaMaxReplicas.
	MinStableForKeda = 3 * time.Minute
)

// MinStableFor returns the action's minimum stableFor, and DefaultStableFor for an action without one.
func MinStableFor(action v1alpha1.ActionType) time.Duration {
	switch action {
	case v1alpha1.ActionSetContainerMemoryLimit:
		return MinStableForMemory
	case v1alpha1.ActionAdjustKedaMaxReplicas:
		return MinStableForKeda
	}
	return DefaultStableFor
}

// Success is the executor's input for the success decision of one check.
type Success struct {
	// Report is the report read in this check, nil when missing.
	Report *v1alpha1.VerificationReport
	// Contract is the result of CheckReport for it.
	Contract v1alpha1.ResetReason
	// Freshness is the counter's verdict for it.
	Freshness Freshness
	// Executor is the executor's own window, with this check already applied.
	Executor Window
	// Now is the executor's clock.
	Now time.Time
	// StableFor is the effective stableFor (StableFor) and NodeGrace the executor's --node-grace.
	StableFor, NodeGrace time.Duration
}

// Succeeds reports whether a good executor check ends the request Succeeded (ADR-0030 section 4): the report
// passes the trust contract and is fresh by its counter, records no OOM kill, and its window is open; the
// report's span (sampledAt - windowStart, both on the verifier's clock) and the executor's own span (now -
// its windowStart, on its clock) are each at least stableFor + nodeGrace. No time of one clock is compared
// with a time of another.
func Succeeds(s Success) bool {
	need := s.StableFor + s.NodeGrace
	if need <= 0 || s.Report == nil || s.Contract != "" || !s.Freshness.Fresh || s.Freshness.Stale {
		return false
	}
	st := &s.Report.Status
	if st.OOMKill != nil || st.SampledAt == nil {
		return false
	}
	return ReportWindow(st).Span(st.SampledAt.Time) >= need && s.Executor.Span(s.Now) >= need
}

// OOMFails reports whether the report records an OOM kill of the action's container that ends the request
// Failed at once, whatever the window. Only a report bound to the request counts: a report that fails the
// spec check (ReportMismatch) is not about this request. Age does not matter, since oomKill is set once.
func OOMFails(r *v1alpha1.VerificationReport, exp Expected) bool {
	return r != nil && r.Status.OOMKill != nil && matches(r, exp) && exp.Container != "" &&
		r.Status.OOMKill.Container == exp.Container
}

// Verdict is the end of a request in Verifying.
type Verdict string

// The verdicts at the timeout.
const (
	// VerdictFailed: the change was shown not to work.
	VerdictFailed Verdict = "Failed"
	// VerdictInconclusive: neither success nor failure could be shown.
	VerdictInconclusive Verdict = "Inconclusive"
)

// Reasons of a failed verdict that are not reset reasons.
const (
	// ReasonRolloutStuck: RolledOut is False at the timeout.
	ReasonRolloutStuck = "RolloutStuck"
	// ReasonKedaNotReady: a ScaledObject's KedaReady does not hold at the timeout.
	ReasonKedaNotReady = "KedaNotReady"
	// ReasonClearingNotChecked: a ScaledObject has no pods of its own, so clearing is not checked.
	ReasonClearingNotChecked = "ClearingNotChecked"
)

// Timeout is the executor's input at the timeout of a request without a success.
type Timeout struct {
	// RolledOut is whether the RolledOut condition is True.
	RolledOut bool
	// ScaledObject is true for a KEDA ScaledObject target, and KedaReady whether KedaReady holds for it.
	ScaledObject, KedaReady bool
	// Report is the last report read, nil when missing, and Expected what it is checked against.
	Report   *v1alpha1.VerificationReport
	Expected Expected
	// Executor is the executor's own window.
	Executor Window
}

// AtTimeout returns the verdict at the timeout (ADR-0030 section 4): Failed when the rollout is stuck, when a
// ScaledObject's KedaReady does not hold, or when the report recorded durable trouble (troubleResets,
// lastTroubleReset) at any sample, whatever its last reset says: a crash loop after the window opened shows one
// Restarted reset per restart and PodNotReady resets in between, so the last reset alone would hide it. A
// Restarted reset that is not recorded as trouble (before the window first opened, or shown only by a node's
// termination time, see [Window.Reset]) does not fail the request. Otherwise Inconclusive with the reason that
// holds the window closed. Recorded state decides, never an order of times from two clocks: first the
// executor's own reset when its window is closed by a report that is no evidence (StaleReport,
// ReportMismatch); then the report's reset when the report's window is closed (the verifier's pod-level reason,
// such as PodNotReady, names why the target is not available); then the executor's reset of a closed window
// (NotAvailable, PartialRollout); then, with both windows open, the last reset of the report and then of the
// executor; then ClearingNotChecked for a ScaledObject, StaleReport without a report, and EvidenceMissing
// otherwise.
//
// Recorded trouble counts only in a report that matches the request (spec check), but it stands even when the
// report is no longer fresh: like the OOM record it is affirmative evidence, and the report's troubleResets
// and lastTroubleReset never move back (CEL), so an old report cannot have recorded less than it did.
func AtTimeout(t Timeout) (Verdict, string) {
	switch {
	case !t.RolledOut:
		return VerdictFailed, ReasonRolloutStuck
	case t.ScaledObject && !t.KedaReady:
		return VerdictFailed, ReasonKedaNotReady
	}
	var report *Window
	if t.Report != nil && matches(t.Report, t.Expected) {
		w := ReportWindow(&t.Report.Status)
		if w.Troubled() {
			return VerdictFailed, string(v1alpha1.ResetRestarted)
		}
		report = &w
	}
	exec := t.Executor
	execClosed := exec.LastReset != nil && !exec.Open()
	switch {
	case execClosed && untrusted(exec.LastReset.Reason):
		// The report is missing, stale or not this request's: what it says is no reason.
		return VerdictInconclusive, string(exec.LastReset.Reason)
	case report != nil && report.LastReset != nil && !report.Open():
		// The verifier's pod-level reason names the cause of a target that is not available.
		return VerdictInconclusive, string(report.LastReset.Reason)
	case execClosed:
		return VerdictInconclusive, string(exec.LastReset.Reason)
	case report != nil && report.LastReset != nil:
		return VerdictInconclusive, string(report.LastReset.Reason)
	case exec.LastReset != nil:
		return VerdictInconclusive, string(exec.LastReset.Reason)
	case t.ScaledObject:
		return VerdictInconclusive, ReasonClearingNotChecked
	case t.Report == nil:
		return VerdictInconclusive, string(v1alpha1.ResetStaleReport)
	}
	return VerdictInconclusive, string(v1alpha1.ResetEvidenceMissing)
}

// untrusted reports whether an executor's reset says the report itself is no evidence: missing, stale or bound
// to something else.
func untrusted(r v1alpha1.ResetReason) bool {
	return r == v1alpha1.ResetStaleReport || r == v1alpha1.ResetReportMismatch
}
