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

package incidents

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
)

// otherUID is the UID of an Incident other than the one of closureIncident.
const otherUID remediationv1alpha1.UID = "22222222-2222-4222-8222-222222222222"

// hint is a Pending hint of comment id, accepted acceptedAgo before triggerNow.
func hint(id int64, acceptedAgo time.Duration) remediationv1alpha1.Hint {
	return remediationv1alpha1.Hint{
		Source: remediationv1alpha1.HintSourceGitHub, CommentID: id,
		Author: remediationv1alpha1.HintAuthor{ID: 7, Login: "reviewer"},
		Text:   "the leak is in the sidecar", Digest: "sha256:" + strings.Repeat("a", 64),
		UpdatedAt: *ago(acceptedAgo), AcceptedAt: *ago(acceptedAgo), State: remediationv1alpha1.HintPending,
	}
}

// hinted is a request named name for the Incident of closureIncident, waiting for approval with a pull
// request and holding hints.
func hinted(name string, hints ...remediationv1alpha1.Hint) remediationv1alpha1.RemediationRequest {
	rr := request(name, time.Hour, remediationv1alpha1.PhaseAwaitingApproval)
	rr.Status.PullRequest = &remediationv1alpha1.PullRequestStatus{Number: 1, URL: "https://github.com/o/r/pull/1"}
	rr.Status.Hints = hints
	return rr
}

// proposed is the Incident of closureIncident in phase Proposed, last analyzed analyzedAgo before triggerNow.
func proposed(analyzedAgo time.Duration) *remediationv1alpha1.Incident {
	inc := closureIncident(remediationv1alpha1.IncidentProposed, 2*time.Minute)
	inc.Status.LastAnalysisTime = ago(analyzedAgo)
	return inc
}

func refOf(h remediationv1alpha1.Hint) *remediationv1alpha1.HintRef {
	r := HintRefOf(&h)
	return &r
}

// HintByRef returns only the hint the ref pins, and only while it is Pending on a request that may hold hints
// for the Incident: an earlier pending hint is not taken instead.
func TestHintByRef(t *testing.T) {
	earlier, pinned := hint(1, 10*time.Minute), hint(2, 5*time.Minute)
	ref := *refOf(pinned)
	inc := proposed(20 * time.Minute)
	inc.Status.Phase = remediationv1alpha1.IncidentAnalyzing
	if got := HintByRef(inc, []remediationv1alpha1.RemediationRequest{hinted("a", earlier, pinned)}, ref); got == nil ||
		got.Ref() != ref {
		t.Errorf("HintByRef = %+v, want the pinned hint", got)
	}
	declined := pinned
	declined.State = remediationv1alpha1.HintDeclined
	waiting := hinted("a", pinned)
	waiting.Status.Phase = remediationv1alpha1.PhaseMerged
	for name, requests := range map[string][]remediationv1alpha1.RemediationRequest{
		"declined":          {hinted("a", earlier, declined)},
		"only another hint": {hinted("a", earlier)},
		"not awaiting":      {waiting},
	} {
		if got := HintByRef(inc, requests, ref); got != nil {
			t.Errorf("%s: HintByRef = %+v, want none", name, got.Hint)
		}
	}
}

func TestPendingHint(t *testing.T) {
	first, second := hint(1, 10*time.Minute), hint(2, 5*time.Minute)
	declined := hint(3, 20*time.Minute)
	declined.State, declined.Reason = remediationv1alpha1.HintDeclined, "CoolingDown"
	tests := []struct {
		name     string
		inc      func() *remediationv1alpha1.Incident
		requests func() []remediationv1alpha1.RemediationRequest
		want     *remediationv1alpha1.Hint
	}{
		{
			name: "the earliest Pending hint",
			inc:  func() *remediationv1alpha1.Incident { return proposed(time.Hour) },
			requests: func() []remediationv1alpha1.RemediationRequest {
				return []remediationv1alpha1.RemediationRequest{hinted("a", second, declined, first)}
			},
			want: &first,
		},
		{
			name: "the earliest Pending hint across requests",
			inc:  func() *remediationv1alpha1.Incident { return proposed(time.Hour) },
			requests: func() []remediationv1alpha1.RemediationRequest {
				return []remediationv1alpha1.RemediationRequest{hinted("a", second), hinted("b", first)}
			},
			want: &first,
		},
		{
			name: "answered: the earliest hint is handled outside Analyzing, so a later one waits",
			inc: func() *remediationv1alpha1.Incident {
				inc := proposed(time.Hour)
				inc.Status.HandledHint = refOf(first)
				return inc
			},
			requests: func() []remediationv1alpha1.RemediationRequest {
				return []remediationv1alpha1.RemediationRequest{hinted("a", first, second)}
			},
		},
		{
			name: "in Analyzing the handled hint is the run's own",
			inc: func() *remediationv1alpha1.Incident {
				inc := proposed(time.Hour)
				inc.Status.Phase = remediationv1alpha1.IncidentAnalyzing
				inc.Status.HandledHint = refOf(first)
				return inc
			},
			requests: func() []remediationv1alpha1.RemediationRequest {
				return []remediationv1alpha1.RemediationRequest{hinted("a", first)}
			},
			want: &first,
		},
		{
			name: "a hint handled earlier does not hide a new one",
			inc: func() *remediationv1alpha1.Incident {
				inc := proposed(time.Hour)
				inc.Status.HandledHint = refOf(declined)
				return inc
			},
			requests: func() []remediationv1alpha1.RemediationRequest {
				return []remediationv1alpha1.RemediationRequest{hinted("a", declined, second)}
			},
			want: &second,
		},
		{
			name: "no Pending hint",
			inc:  func() *remediationv1alpha1.Incident { return proposed(time.Hour) },
			requests: func() []remediationv1alpha1.RemediationRequest {
				return []remediationv1alpha1.RemediationRequest{hinted("a", declined)}
			},
		},
		{
			name: "a request not waiting for approval",
			inc:  func() *remediationv1alpha1.Incident { return proposed(time.Hour) },
			requests: func() []remediationv1alpha1.RemediationRequest {
				rr := hinted("a", first)
				rr.Status.Phase = remediationv1alpha1.PhaseSuperseded
				return []remediationv1alpha1.RemediationRequest{rr}
			},
		},
		{
			name: "a request without a pull request",
			inc:  func() *remediationv1alpha1.Incident { return proposed(time.Hour) },
			requests: func() []remediationv1alpha1.RemediationRequest {
				rr := hinted("a", first)
				rr.Status.PullRequest = nil
				return []remediationv1alpha1.RemediationRequest{rr}
			},
		},
		{
			name: "a request of another Incident with the same name",
			inc:  func() *remediationv1alpha1.Incident { return proposed(time.Hour) },
			requests: func() []remediationv1alpha1.RemediationRequest {
				rr := hinted("a", first)
				rr.Spec.IncidentRef.UID = otherUID
				return []remediationv1alpha1.RemediationRequest{rr}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := PendingHint(tt.inc(), tt.requests())
			switch {
			case tt.want == nil && got != nil:
				t.Fatalf("PendingHint = comment %d, want none", got.Hint.CommentID)
			case tt.want != nil && (got == nil || got.Ref() != *refOf(*tt.want)):
				t.Fatalf("PendingHint = %+v, want comment %d", got, tt.want.CommentID)
			}
		})
	}
}

// ADR-0028 section 2: a pending hint makes a Proposed Incident due, the cooldown applies and the budget is
// counted. Without the hint a Proposed Incident is never due.
func TestDueWithAReviewerHint(t *testing.T) {
	tests := []struct {
		name        string
		analyzedAgo time.Duration
		hinted      bool
		seenAgo     time.Duration
		settings    func(*TriggerSettings)
		used        int
		due         bool
		reason      string
	}{
		{name: "due with a pending hint", analyzedAgo: 20 * time.Minute, hinted: true, due: true, reason: ReasonReviewerHint},
		{name: "without a hint", analyzedAgo: 20 * time.Minute, reason: ReasonProposed},
		{name: "not due in the cooldown", analyzedAgo: 14 * time.Minute, hinted: true, reason: ReasonCoolingDown},
		{name: "not due when the budget is spent", analyzedAgo: 20 * time.Minute, hinted: true, used: 100, reason: ReasonDailyBudgetReached},
		{name: "not due when the budget is 0", analyzedAgo: 20 * time.Minute, hinted: true,
			settings: func(s *TriggerSettings) { s.MaxAnalysesPerDay = 0 }, reason: ReasonAnalysisDisabled},
		{name: "the debounce is about the incident's data, not the hint", analyzedAgo: 20 * time.Minute, hinted: true,
			seenAgo: time.Second, due: true, reason: ReasonReviewerHint},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := triggerSettings
			if tt.settings != nil {
				tt.settings(&s)
			}
			inc := proposed(tt.analyzedAgo)
			if tt.seenAgo > 0 {
				inc.Status.LastSeen = ago(tt.seenAgo)
			}
			due, reason := Due(inc, Memory{Hinted: tt.hinted}, triggerNow, s, tt.used)
			if due != tt.due || reason != tt.reason {
				t.Fatalf("Due = %v, %s; want %v, %s", due, reason, tt.due, tt.reason)
			}
			if _, ok := dueMessages[reason]; !ok {
				t.Fatalf("reason %s has no message", reason)
			}
		})
	}
	inc := proposed(time.Hour)
	inc.Status.LastAnalysisTime = nil
	if due, reason := Due(inc, Memory{Hinted: true}, triggerNow, triggerSettings, 0); due || reason != ReasonProposed {
		t.Errorf("without a lastAnalysisTime Due = %v, %s; want not due, Proposed", due, reason)
	}
}

// hintController is a controller on a fake API server holding inc, requests, the RemediationSettings with a
// daily limit of perDay and a budget of used analyses today.
func hintController(t *testing.T, inc *remediationv1alpha1.Incident, perDay int32, used int,
	requests ...remediationv1alpha1.RemediationRequest) (*Controller, client.Client) {
	t.Helper()
	return hintControllerWith(t, []*remediationv1alpha1.Incident{inc}, perDay, used, requests...)
}

// hintControllerWith is hintController with several Incidents.
func hintControllerWith(t *testing.T, incs []*remediationv1alpha1.Incident, perDay int32, used int,
	requests ...remediationv1alpha1.RemediationRequest) (*Controller, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := remediationv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	settings := &remediationv1alpha1.RemediationSettings{ObjectMeta: metav1.ObjectMeta{Name: remediationv1alpha1.SettingsName},
		Spec: remediationv1alpha1.RemediationSettingsSpec{Analysis: &remediationv1alpha1.AnalysisSettings{MaxAnalysesPerDay: &perDay}}}
	budget := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: fakeNamespace, Name: BudgetName}, Data: usedToday(used)}
	objs := make([]client.Object, 0, 2+len(incs)+len(requests))
	objs = append(objs, settings, budget)
	for _, inc := range incs {
		objs = append(objs, inc)
	}
	for i := range requests {
		objs = append(objs, &requests[i])
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&remediationv1alpha1.Incident{}, &remediationv1alpha1.RemediationRequest{}).
		WithObjects(objs...).Build()
	c := loaded(t, cl)
	c.SetAnalysisRequests(analysisRequest)
	return c, cl
}

// foreignLabel marks a request in the tests that the analysis would not take as its own.
const foreignLabel = "test.shadewright.io/foreign"

// analysisRequest stands in for the analysis's predicate (analysis.Controller.IsAnalysisRequest).
func analysisRequest(_ *remediationv1alpha1.Incident, rr *remediationv1alpha1.RemediationRequest) bool {
	return rr.Labels[foreignLabel] == ""
}

func analyzedCondition(inc *remediationv1alpha1.Incident) *metav1.Condition {
	return meta.FindStatusCondition(inc.Status.Conditions, ConditionAnalyzed)
}

// A trigger pass marks a Proposed Incident with a pending hint due, and takes a unit of the budget for it.
func TestPassMarksAHintedIncidentDue(t *testing.T) {
	h := hint(1, time.Minute)
	c, cl := hintController(t, proposed(20*time.Minute), 10, 0, hinted("a", h))
	if err := c.markDue(t.Context()); err != nil {
		t.Fatal(err)
	}
	got := getIncident(t, cl)
	if !dueIs(true, ReasonReviewerHint)(got) {
		t.Fatalf("AnalysisDue %+v, want due with ReviewerHint", got.Status.Conditions)
	}
	if got.Status.HandledHint != nil || analyzedCondition(got) != nil {
		t.Errorf("the mark answered the hint: handledHint %v, Analyzed %+v", got.Status.HandledHint, analyzedCondition(got))
	}

}

// The mark of a hinted Incident takes a unit of the budget like any other: with one analysis left, an Incident
// decided after it (seen later) is not due.
func TestHintedMarkTakesABudgetUnit(t *testing.T) {
	hintedInc := proposed(20 * time.Minute)
	hintedInc.Status.FirstSeen = ago(2 * time.Hour)
	other := &remediationv1alpha1.Incident{
		ObjectMeta: metav1.ObjectMeta{Name: Name(fp(2)), Namespace: fakeNamespace, UID: types.UID(otherUID)},
		Spec:       specOf(observation(2, 2, 1, 1, triggerNow)), Status: incidentStatus(2*time.Minute, time.Hour, 0),
	}
	other.Status.Occurrences[0].ID = eventID(2)
	c, cl := hintControllerWith(t, []*remediationv1alpha1.Incident{hintedInc, other}, 1, 0, hinted("a", hint(1, time.Minute)))
	if err := c.markDue(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := getIncident(t, cl); !dueIs(true, ReasonReviewerHint)(got) {
		t.Fatalf("hinted Incident: AnalysisDue %+v, want due with ReviewerHint", got.Status.Conditions)
	}
	got := &remediationv1alpha1.Incident{}
	if err := cl.Get(t.Context(), client.ObjectKeyFromObject(other), got); err != nil {
		t.Fatal(err)
	}
	if !dueIs(false, ReasonDailyBudgetReached)(got) {
		t.Errorf("other Incident: AnalysisDue %+v, want not due, DailyBudgetReached", got.Status.Conditions)
	}
}

// lostHintRun is the Incident of closureIncident as a run of hint h taken as lost: Analyzing since 20
// minutes, handledHint h, marked not due while it ran.
func lostHintRun(h remediationv1alpha1.Hint) *remediationv1alpha1.Incident {
	inc := proposed(20 * time.Minute)
	inc.Status.Phase = remediationv1alpha1.IncidentAnalyzing
	inc.Status.HandledHint = refOf(h)
	inc.Status = withDueCondition(inc.Status, metav1.ConditionFalse, ReasonAnalyzing)
	inc.Status.Conditions[0].ObservedGeneration = inc.Generation
	return inc
}

// A lost hint run the analysis ends without a model call (its replacement exists, or its hint is no longer
// Pending) is marked due with the budget spent or 0 too, and takes no unit: an Incident decided after it is
// still due on the last unit. One that would call the model again (its hint still Pending, no replacement)
// waits for the budget like any lost run.
func TestLostHintRunThatSettlesNeedsNoBudget(t *testing.T) {
	h := hint(1, time.Minute)
	replacement := func() remediationv1alpha1.RemediationRequest {
		rr := request("replacement", 19*time.Minute, remediationv1alpha1.PhasePending)
		rr.Spec.Replaces = &remediationv1alpha1.RequestReplacement{Name: "a", Hint: *refOf(h)}
		return rr
	}
	declined := h
	declined.State = remediationv1alpha1.HintDeclined
	for _, tt := range []struct {
		name     string
		perDay   int32
		used     int
		requests []remediationv1alpha1.RemediationRequest
		due      bool
	}{
		{"replacement, budget spent", 1, 1, []remediationv1alpha1.RemediationRequest{hinted("a", h), replacement()}, true},
		{"replacement, analysis disabled", 0, 0, []remediationv1alpha1.RemediationRequest{hinted("a", h), replacement()}, true},
		{"hint declined, budget spent", 1, 1, []remediationv1alpha1.RemediationRequest{hinted("a", declined)}, true},
		{"hint pending, budget spent", 1, 1, []remediationv1alpha1.RemediationRequest{hinted("a", h)}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c, cl := hintController(t, lostHintRun(h), tt.perDay, tt.used, tt.requests...)
			if err := c.markDue(t.Context()); err != nil {
				t.Fatal(err)
			}
			if got := getIncident(t, cl); meta.IsStatusConditionTrue(got.Status.Conditions, ConditionAnalysisDue) != tt.due {
				t.Errorf("AnalysisDue %+v, want due %v", meta.FindStatusCondition(got.Status.Conditions, ConditionAnalysisDue), tt.due)
			}
		})
	}
	t.Run("takes no unit", func(t *testing.T) {
		other := &remediationv1alpha1.Incident{
			ObjectMeta: metav1.ObjectMeta{Name: Name(fp(2)), Namespace: fakeNamespace, UID: types.UID(otherUID)},
			Spec:       specOf(observation(2, 2, 1, 1, triggerNow)), Status: incidentStatus(2*time.Minute, time.Hour, 0),
		}
		other.Status.Occurrences[0].ID = eventID(2)
		lost := lostHintRun(h)
		lost.Status.FirstSeen = ago(2 * time.Hour)
		c, cl := hintControllerWith(t, []*remediationv1alpha1.Incident{lost, other}, 1, 0, hinted("a", h), replacement())
		if err := c.markDue(t.Context()); err != nil {
			t.Fatal(err)
		}
		if got := getIncident(t, cl); !dueIs(true, ReasonAnalysisTimedOut)(got) {
			t.Fatalf("lost run: AnalysisDue %+v, want due with AnalysisTimedOut", got.Status.Conditions)
		}
		got := &remediationv1alpha1.Incident{}
		if err := cl.Get(t.Context(), client.ObjectKeyFromObject(other), got); err != nil {
			t.Fatal(err)
		}
		if !meta.IsStatusConditionTrue(got.Status.Conditions, ConditionAnalysisDue) {
			t.Errorf("other Incident: AnalysisDue %+v, want due on the unit the lost run did not take", got.Status.Conditions)
		}
	})
}

// A hint refused by the cooldown or the budget is an answer, not a queue (ADR-0028 section 2): the trigger
// records the hint in status.handledHint with the Analyzed reason the executor reads, once.
func TestPassAnswersARefusedHint(t *testing.T) {
	h := hint(1, time.Minute)
	for _, tt := range []struct {
		name        string
		analyzedAgo time.Duration
		perDay      int32
		used        int
		due, out    string
	}{
		{"in the cooldown", 5 * time.Minute, 10, 0, ReasonCoolingDown, OutcomeHintCoolingDown},
		{"budget spent", 20 * time.Minute, 2, 2, ReasonDailyBudgetReached, OutcomeHintBudgetSpent},
		{"budget 0", 20 * time.Minute, 0, 0, ReasonAnalysisDisabled, OutcomeHintBudgetSpent},
	} {
		t.Run(tt.name, func(t *testing.T) {
			inc := proposed(tt.analyzedAgo)
			// The condition already says not due, as after the analysis that proposed: an answer is written
			// anyway.
			inc.Status = withDueCondition(inc.Status, metav1.ConditionFalse, ReasonProposed)
			inc.Status.Conditions[0].ObservedGeneration = inc.Generation
			c, cl := hintController(t, inc, tt.perDay, tt.used, hinted("a", h))
			if err := c.markDue(t.Context()); err != nil {
				t.Fatal(err)
			}
			got := getIncident(t, cl)
			if got.Status.HandledHint == nil || *got.Status.HandledHint != *refOf(h) {
				t.Fatalf("handledHint %v, want the hint", got.Status.HandledHint)
			}
			cond := analyzedCondition(got)
			if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != tt.out || cond.Message == "" {
				t.Fatalf("Analyzed %+v, want True with %s", cond, tt.out)
			}
			if !dueIs(false, tt.due)(got) || got.Status.Phase != remediationv1alpha1.IncidentProposed {
				t.Errorf("phase %s, AnalysisDue %+v; want Proposed, not due with %s", got.Status.Phase, got.Status.Conditions, tt.due)
			}

			// The answer is final: the next pass neither writes again nor marks the Incident due.
			rv := got.ResourceVersion
			if err := c.markDue(t.Context()); err != nil {
				t.Fatal(err)
			}
			if again := getIncident(t, cl); again.ResourceVersion != rv {
				t.Errorf("a second pass wrote the Incident again: %+v", again.Status.Conditions)
			}
		})
	}
}

// A hint the trigger answered, or an analysis answered, is not marked due again while the executor has not
// moved it on, even past the cooldown: it never starts a second analysis.
func TestAnsweredHintIsNotDueAgain(t *testing.T) {
	h := hint(1, time.Minute)
	inc := proposed(20 * time.Minute)
	inc.Status.HandledHint = refOf(h)
	c, cl := hintController(t, inc, 10, 0, hinted("a", h))
	if err := c.markDue(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := getIncident(t, cl); meta.IsStatusConditionTrue(got.Status.Conditions, ConditionAnalysisDue) {
		t.Errorf("an answered hint marked the Incident due: %+v", got.Status.Conditions)
	}
}

// The trigger counts only the hints of requests the analysis takes as its own (the same predicate): a hint on
// any other request neither marks the Incident due (no unit of the budget) nor is refused, past the cooldown or
// within it; with the analysis off (no predicate) no hint counts.
func TestTriggerCountsOnlyTheAnalysisRequests(t *testing.T) {
	foreign := func() remediationv1alpha1.RemediationRequest {
		rr := hinted("a", hint(1, time.Minute))
		rr.Labels = map[string]string{foreignLabel: "true"}
		return rr
	}
	for _, tt := range []struct {
		name        string
		analyzedAgo time.Duration
		request     remediationv1alpha1.RemediationRequest
		off         bool
	}{
		{name: "foreign request past the cooldown", analyzedAgo: 20 * time.Minute, request: foreign()},
		{name: "foreign request within the cooldown", analyzedAgo: 5 * time.Minute, request: foreign()},
		{name: "analysis off", analyzedAgo: 20 * time.Minute, request: hinted("a", hint(1, time.Minute)), off: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c, cl := hintController(t, proposed(tt.analyzedAgo), 10, 0, tt.request)
			if tt.off {
				c.SetAnalysisRequests(nil)
			}
			if err := c.markDue(t.Context()); err != nil {
				t.Fatal(err)
			}
			got := getIncident(t, cl)
			if meta.IsStatusConditionTrue(got.Status.Conditions, ConditionAnalysisDue) {
				t.Errorf("a hint the analysis would not answer marked the Incident due: %+v", got.Status.Conditions)
			}
			if got.Status.HandledHint != nil || analyzedCondition(got) != nil {
				t.Errorf("the trigger answered a hint the analysis would not answer: handledHint %v, Analyzed %+v",
					got.Status.HandledHint, analyzedCondition(got))
			}
		})
	}
}

// A failed listing of the requests fails closed for the hints only: the pass marks nothing due for a hint and
// returns the error.
func TestUnreadableRequestsMarkNoHintDue(t *testing.T) {
	c, cl := hintController(t, proposed(20*time.Minute), 10, 0, hinted("a", hint(1, time.Minute)))
	c.cache = &failingRequests{Client: cl}
	if err := c.markDue(t.Context()); err == nil {
		t.Error("the pass hid the failed listing")
	}
	if got := getIncident(t, cl); meta.IsStatusConditionTrue(got.Status.Conditions, ConditionAnalysisDue) {
		t.Errorf("an Incident was marked due for a hint the pass could not read: %+v", got.Status.Conditions)
	}
}
