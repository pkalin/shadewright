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
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
)

// e0 is the executor's clock at its first check in a test. It is unrelated to the verifier's t0.
var e0 = time.Date(2026, 11, 13, 17, 42, 7, 0, time.UTC)

// fresh observes a report twice, the counter rising in between, and returns the freshness of the second.
func fresh(r *v1alpha1.VerificationReport, now time.Time) Freshness {
	var c Counter
	c.Observe(r.UID, r.Status.Sample-1, now.Add(-FastPollInterval))
	return c.Observe(r.UID, r.Status.Sample, now)
}

// A report that is missing, stale or bound to something else, or that does not cover every desired replica,
// is no evidence.
func TestStaleOrMismatchedReportIsNoEvidence(t *testing.T) {
	good := report(samples(t, 3))
	if got := CheckReport(good, expected(), fresh(good, e0)); got != "" {
		t.Fatalf("a good report: %q", got)
	}
	cases := []struct {
		name   string
		report func() *v1alpha1.VerificationReport
		exp    func(*Expected)
		f      Freshness
		want   v1alpha1.ResetReason
	}{
		{"missing", func() *v1alpha1.VerificationReport { return nil }, nil, Freshness{Fresh: true}, v1alpha1.ResetStaleReport},
		{"counter stopped rising", good.DeepCopy, nil, Freshness{Stale: true}, v1alpha1.ResetStaleReport},
		{"another request", good.DeepCopy, func(e *Expected) { e.RequestUID = otherUID }, Freshness{Fresh: true}, v1alpha1.ResetReportMismatch},
		{"another merge commit", good.DeepCopy, func(e *Expected) { e.MergeCommitSHA = "89abcdef0123456789abcdef0123456789abcdef" }, Freshness{Fresh: true}, v1alpha1.ResetReportMismatch},
		{"another target", good.DeepCopy, func(e *Expected) { e.TargetUID = otherUID }, Freshness{Fresh: true}, v1alpha1.ResetReportMismatch},
		{"another template revision", good.DeepCopy, func(e *Expected) { e.TemplateRevision = "3" }, Freshness{Fresh: true}, v1alpha1.ResetReportMismatch},
		{"another container", good.DeepCopy, func(e *Expected) { e.Container = sidecar }, Freshness{Fresh: true}, v1alpha1.ResetReportMismatch},
		{"no container in the report", func() *v1alpha1.VerificationReport {
			r := good.DeepCopy()
			r.Spec.Container = ""
			return r
		}, nil, Freshness{Fresh: true}, v1alpha1.ResetReportMismatch},
		{"target scaled to zero", func() *v1alpha1.VerificationReport {
			r := good.DeepCopy()
			r.Status.PodsCovered, r.Status.DesiredReplicas = 0, 0
			return r
		}, func(e *Expected) { e.Replicas = 0 }, Freshness{Fresh: true}, v1alpha1.ResetPartialRollout},
		{"nothing held to compare", good.DeepCopy, func(e *Expected) { *e = Expected{Replicas: 3} }, Freshness{Fresh: true}, v1alpha1.ResetReportMismatch},
		{"fewer pods covered than desired", func() *v1alpha1.VerificationReport {
			r := good.DeepCopy()
			r.Status.PodsCovered = 2
			return r
		}, nil, Freshness{Fresh: true}, v1alpha1.ResetPartialRollout},
		{"desired replicas not the target's", good.DeepCopy, func(e *Expected) { e.Replicas = 4 }, Freshness{Fresh: true}, v1alpha1.ResetPartialRollout},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			exp := expected()
			if c.exp != nil {
				c.exp(&exp)
			}
			r := c.report()
			if got := CheckReport(r, exp, c.f); got != c.want {
				t.Fatalf("CheckReport = %q, want %q", got, c.want)
			}
			exec := Window{Start: mt(e0.Add(-time.Hour))}
			if Succeeds(Success{Report: r, Contract: CheckReport(r, exp, c.f), Freshness: c.f, Executor: exec, Now: e0,
				StableFor: time.Minute, NodeGrace: time.Second}) {
				t.Fatal("a report that is no evidence gave a success")
			}
		})
	}
}

// The counter rule: a report is fresh only when its counter is higher than the one read at a check made at
// most one poll interval earlier by the executor's clock, and stale when it has not risen for more than three.
func TestCounterRule(t *testing.T) {
	const uid types.UID = "e0000000-0000-4000-8000-000000000001"
	var c Counter
	if f := c.Observe(uid, 5, e0); f.Fresh || f.Stale {
		t.Fatalf("first check: %+v; a first check proves nothing", f)
	}
	if f := c.Observe(uid, 6, e0.Add(PollInterval)); !f.Fresh {
		t.Fatalf("counter rose since a check one poll interval earlier: %+v", f)
	}
	if f := c.Observe(uid, 6, e0.Add(PollInterval+FastPollInterval)); f.Fresh {
		t.Fatalf("counter did not rise since the last check: %+v", f)
	}
	if f := c.Observe(uid, 7, e0.Add(3*PollInterval)); f.Fresh {
		t.Fatalf("counter rose only against a check more than one poll interval earlier: %+v", f)
	}
	if f := c.Observe(uid, 7, e0.Add(3*PollInterval+StaleAfter)); f.Stale {
		t.Fatalf("stale after exactly StaleAfter: %+v", f)
	}
	if f := c.Observe(uid, 7, e0.Add(3*PollInterval+StaleAfter+time.Second)); !f.Stale || f.Fresh {
		t.Fatalf("counter has not risen for more than StaleAfter: %+v", f)
	}
	// A report created again under another UID starts over, as a restart of the executor would.
	if f := c.Observe("e0000000-0000-4000-8000-000000000002", 100, e0.Add(10*time.Minute)); f.Fresh || f.Stale {
		t.Fatalf("another report: %+v", f)
	}
	// The executor's clock going back proves no freshness.
	var back Counter
	back.Observe(uid, 1, e0)
	if f := back.Observe(uid, 2, e0.Add(-time.Second)); f.Fresh {
		t.Fatalf("a check before the previous one by the clock: %+v", f)
	}
}

func TestCounterRemembersABoundedNumberOfChecks(t *testing.T) {
	var c Counter
	for i := range 100 {
		c.Observe("e0000000-0000-4000-8000-000000000001", int64(i), e0.Add(time.Duration(i)*time.Second))
	}
	if len(c.checks) > maxChecks {
		t.Fatalf("remembers %d checks, bound %d", len(c.checks), maxChecks)
	}
}

// Each span is measured on one clock: shifting the verifier's times against the executor's clock (sampledAt
// and windowStart a day behind or ahead) changes no verdict, since the report's age comes from its counter.
func TestSkewedSampledAtChangesNoVerdict(t *testing.T) {
	stableFor, grace := 3*time.Minute, time.Minute
	need := stableFor + grace
	exec := Window{Start: mt(e0.Add(-need))}
	verdict := func(r *v1alpha1.VerificationReport) bool {
		return Succeeds(Success{Report: r, Contract: CheckReport(r, expected(), fresh(r, e0)), Freshness: fresh(r, e0),
			Executor: exec, Now: e0, StableFor: stableFor, NodeGrace: grace})
	}
	long := samples(t, int(need/PollInterval)+1) // a span of exactly need
	short := samples(t, int(need/PollInterval))  // one poll interval short
	for _, skew := range []time.Duration{0, -24 * time.Hour, 24 * time.Hour, e0.Sub(t0), -e0.Sub(t0) - time.Hour} {
		for name, c := range map[string]struct {
			st   v1alpha1.VerificationReportStatus
			want bool
		}{"long enough": {long, true}, "too short": {short, false}} {
			st := *c.st.DeepCopy()
			st.SampledAt, st.WindowStart = mt(st.SampledAt.Add(skew)), mt(st.WindowStart.Add(skew))
			if got := verdict(report(st)); got != c.want {
				t.Errorf("%s, skew %v: success %v, want %v", name, skew, got, c.want)
			}
		}
	}
}

// Success needs both spans of at least stableFor + nodeGrace, an open report window and no OOM kill.
func TestSuccessNeedsStableForPlusNodeGrace(t *testing.T) {
	stableFor, grace := 3*time.Minute, time.Minute
	need := stableFor + grace
	long := report(samples(t, int(need/PollInterval)+1))
	short := report(samples(t, int(need/PollInterval)))
	succeeds := func(r *v1alpha1.VerificationReport, execSpan time.Duration) bool {
		f := fresh(r, e0)
		return Succeeds(Success{Report: r, Contract: CheckReport(r, expected(), f), Freshness: f,
			Executor: Window{Start: mt(e0.Add(-execSpan))}, Now: e0, StableFor: stableFor, NodeGrace: grace})
	}
	if !succeeds(long, need) {
		t.Fatal("both spans equal to stableFor + nodeGrace: no success")
	}
	if succeeds(short, need) {
		t.Fatal("report span one poll interval short: success")
	}
	if succeeds(long, need-time.Second) {
		t.Fatal("executor span one second short: success")
	}
	if succeeds(long, stableFor) {
		t.Fatal("spans of stableFor without nodeGrace: success")
	}
	closed := long.DeepCopy()
	w := ReportWindow(&closed.Status)
	w.Reset(v1alpha1.ResetPodNotReady, closed.Status.SampledAt.Time)
	w.store(&closed.Status)
	if succeeds(closed, need) {
		t.Fatal("a report whose last sample reset its window: success")
	}
	oom := long.DeepCopy()
	oom.Status.OOMKill = &v1alpha1.OOMKill{PodUID: v1alpha1.UID(podUID(0)), Container: app}
	if succeeds(oom, need) {
		t.Fatal("a report with an OOM kill: success")
	}
	if f := fresh(long, e0); Succeeds(Success{Report: long, Freshness: Freshness{Stale: f.Stale}, Executor: Window{Start: mt(e0.Add(-need))},
		Now: e0, StableFor: stableFor, NodeGrace: grace}) {
		t.Fatal("a report that is not fresh by its counter: success")
	}
}

func TestStableFor(t *testing.T) {
	d := func(v time.Duration) *metav1.Duration { return &metav1.Duration{Duration: v} }
	cases := []struct {
		name      string
		requested *metav1.Duration
		action    v1alpha1.ActionType
		want      time.Duration
	}{
		{"memory, default", nil, v1alpha1.ActionSetContainerMemoryLimit, 10 * time.Minute},
		{"memory, shorter requested", d(5 * time.Minute), v1alpha1.ActionSetContainerMemoryLimit, 10 * time.Minute},
		{"memory, longer requested", d(15 * time.Minute), v1alpha1.ActionSetContainerMemoryLimit, 15 * time.Minute},
		{"KEDA, default", nil, v1alpha1.ActionAdjustKedaMaxReplicas, 3 * time.Minute},
		{"KEDA, shorter requested", d(time.Minute), v1alpha1.ActionAdjustKedaMaxReplicas, 3 * time.Minute},
		{"KEDA, longer requested", d(7 * time.Minute), v1alpha1.ActionAdjustKedaMaxReplicas, 7 * time.Minute},
	}
	for _, c := range cases {
		if got := StableFor(c.requested, c.action); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

// The timeout verdict follows the table of ADR-0030 section 4.
func TestTimeoutVerdict(t *testing.T) {
	withReset := func(reason v1alpha1.ResetReason) *v1alpha1.VerificationReport {
		r := report(samples(t, 2))
		w := ReportWindow(&r.Status)
		w.Reset(reason, r.Status.SampledAt.Time)
		w.store(&r.Status)
		return r
	}
	execReset := func(reason v1alpha1.ResetReason) Window {
		w := Window{}
		w.Reset(reason, e0)
		return w
	}
	cases := []struct {
		name    string
		in      Timeout
		verdict Verdict
		reason  string
	}{
		{"rollout stuck", Timeout{RolledOut: false, Report: withReset(v1alpha1.ResetPodNotReady)}, VerdictFailed, ReasonRolloutStuck},
		{"KEDA not ready", Timeout{RolledOut: true, ScaledObject: true}, VerdictFailed, ReasonKedaNotReady},
		{"KEDA ready, nothing to clear", Timeout{RolledOut: true, ScaledObject: true, KedaReady: true}, VerdictInconclusive, ReasonClearingNotChecked},
		{"last report reset restarted", Timeout{RolledOut: true, Report: withReset(v1alpha1.ResetRestarted)}, VerdictFailed, "Restarted"},
		{"restarted outweighs the executor's own reset", Timeout{RolledOut: true, Report: withReset(v1alpha1.ResetRestarted),
			Executor: execReset(v1alpha1.ResetStaleReport)}, VerdictFailed, "Restarted"},
		{"pods not ready", Timeout{RolledOut: true, Report: withReset(v1alpha1.ResetPodNotReady)}, VerdictInconclusive, "PodNotReady"},
		{"partial rollout", Timeout{RolledOut: true, Report: withReset(v1alpha1.ResetPartialRollout)}, VerdictInconclusive, "PartialRollout"},
		{"more pods than the bound", Timeout{RolledOut: true, Report: withReset(v1alpha1.ResetEvidenceMissing)}, VerdictInconclusive, "EvidenceMissing"},
		{"executor's own reset first", Timeout{RolledOut: true, Report: withReset(v1alpha1.ResetPodNotReady),
			Executor: execReset(v1alpha1.ResetNotAvailable)}, VerdictInconclusive, "NotAvailable"},
		{"no report", Timeout{RolledOut: true}, VerdictInconclusive, "StaleReport"},
		{"a restarted reset in a mismatched report", Timeout{RolledOut: true, Report: withReset(v1alpha1.ResetRestarted),
			Expected: Expected{RequestUID: otherUID}, Executor: execReset(v1alpha1.ResetReportMismatch)}, VerdictInconclusive, "ReportMismatch"},
		{"no reset at all", Timeout{RolledOut: true, Report: report(samples(t, 2))}, VerdictInconclusive, "EvidenceMissing"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.in.Expected == (Expected{}) {
				c.in.Expected = expected()
			}
			v, reason := AtTimeout(c.in)
			if v != c.verdict || reason != c.reason {
				t.Fatalf("AtTimeout = %s %s, want %s %s", v, reason, c.verdict, c.reason)
			}
		})
	}
}
