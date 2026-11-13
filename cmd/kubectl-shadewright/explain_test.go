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

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
)

// Names of the explain tests.
const (
	testRR     = "rr-a"
	testOldUID = "uid-old"
)

// cond is a condition at testNow minus ago.
func cond(t string, s metav1.ConditionStatus, reason, msg string, ago time.Duration) metav1.Condition {
	return metav1.Condition{Type: t, Status: s, Reason: reason, Message: msg,
		LastTransitionTime: metav1.Time{Time: testNow.Add(-ago)}}
}

// explainRequestObj returns a request for incident-a in phase, with a memory action, a decided policy, a plan,
// a source and the given conditions.
func explainRequestObj(name string, phase remediationv1alpha1.RemediationPhase, created time.Duration,
	conds ...metav1.Condition) *remediationv1alpha1.RemediationRequest {
	rr := testRequest(name, "a", 0, "")
	rr.CreationTimestamp = metav1.Time{Time: testNow.Add(-created)}
	rr.Spec.PolicyRef = "default"
	rr.Spec.Action.SetContainerMemoryLimit = &remediationv1alpha1.SetContainerMemoryLimit{
		Container: "app", Previous: resource.MustParse("256Mi"), Desired: resource.MustParse("512Mi")}
	rr.Status.Phase = phase
	rr.Status.LastPhaseTransitionTime = &metav1.MicroTime{Time: testNow.Add(-time.Minute)}
	rr.Status.PolicyRevision = "default/3"
	rr.Status.EffectiveApprovalRequired = new(true)
	rr.Status.PlanDigest = "sha256:" + strings.Repeat("ab", 32)
	rr.Status.Source = &remediationv1alpha1.ResolvedSource{RepositoryRef: testRepoName, TargetBranch: "main",
		Path: "apps/demo/deployment.yaml"}
	rr.Status.Conditions = conds
	return rr
}

func explainClient(t *testing.T, objs ...client.Object) client.Reader {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
}

func explainOut(t *testing.T, e *Explanation) (string, string) {
	t.Helper()
	var text, js bytes.Buffer
	if err := renderExplainText(&text, e, testNow); err != nil {
		t.Fatal(err)
	}
	if err := renderJSON(&js, e); err != nil {
		t.Fatal(err)
	}
	return text.String(), js.String()
}

// TestExplainTerminalPhases explains a request in each phase that ends it without a merge, and checks the
// stopping condition, its troubleshooting link and the condition timeline, oldest first.
func TestExplainTerminalPhases(t *testing.T) {
	allowed := cond(remediationv1alpha1.ConditionPolicyAllowed, metav1.ConditionTrue, "WithinApprovedBounds",
		"Every check passed", 30*time.Minute)
	opened := cond(remediationv1alpha1.ConditionPROpened, metav1.ConditionTrue, "PullRequestOpened", "", 20*time.Minute)
	for _, tc := range []struct {
		phase              remediationv1alpha1.RemediationPhase
		conds              []metav1.Condition
		condition, reason  string
		link               string
		listed             bool
		pullRequestCleared bool
	}{
		{phase: remediationv1alpha1.PhaseDenied,
			conds: []metav1.Condition{cond(remediationv1alpha1.ConditionPolicyAllowed, metav1.ConditionFalse,
				"IncreaseTooLarge", "The increase is 100%, the policy allows 50%", 5*time.Minute)},
			condition: remediationv1alpha1.ConditionPolicyAllowed, reason: "IncreaseTooLarge",
			link: troubleshootingURL + "#policy-decision", listed: true},
		{phase: remediationv1alpha1.PhaseNeedsIntervention,
			conds: []metav1.Condition{allowed, opened, cond(remediationv1alpha1.ConditionApproved,
				metav1.ConditionFalse, "BranchDiverged", "Someone pushed to the branch", 2*time.Minute)},
			condition: remediationv1alpha1.ConditionApproved, reason: "BranchDiverged",
			link: troubleshootingURL + "#pull-request-step", listed: true},
		{phase: remediationv1alpha1.PhaseSuperseded,
			conds: []metav1.Condition{allowed, opened, cond(remediationv1alpha1.ConditionPlanFresh,
				metav1.ConditionFalse, "TargetChanged", "The target has another UID", 3*time.Minute)},
			// TargetChanged is listed under the request checks and freshness; on PlanFresh it is freshness.
			condition: remediationv1alpha1.ConditionPlanFresh, reason: "TargetChanged",
			link: troubleshootingURL + "#freshness", listed: true},
		{phase: remediationv1alpha1.PhaseExpired,
			conds: []metav1.Condition{allowed, cond(remediationv1alpha1.ConditionPROpened, metav1.ConditionFalse,
				"PullRequestDeadline", "No pull request within 30 minutes", time.Minute),
				// WritesPaused only holds a request back; it is never the stop.
				cond(remediationv1alpha1.ConditionWritesPaused, metav1.ConditionFalse, "Paused", "", 0)},
			condition: remediationv1alpha1.ConditionPROpened, reason: "PullRequestDeadline",
			link: troubleshootingURL + "#pull-request-step", listed: true},
		{phase: remediationv1alpha1.PhaseCancelled,
			conds: []metav1.Condition{cond(remediationv1alpha1.ConditionPolicyAllowed, metav1.ConditionFalse,
				remediationv1alpha1.ReasonHandoffRefused, "Another replacement reserved the hint", time.Minute)},
			condition: remediationv1alpha1.ConditionPolicyAllowed, reason: remediationv1alpha1.ReasonHandoffRefused,
			link: troubleshootingURL + "#how-to-read-the-result", listed: true},
		{phase: remediationv1alpha1.PhaseInconclusive,
			conds: []metav1.Condition{allowed, opened, cond(remediationv1alpha1.ConditionApproved, metav1.ConditionTrue,
				"ChangeRequestMerged", "The pull request was merged", 3*time.Minute),
				cond(remediationv1alpha1.ConditionVerified, metav1.ConditionUnknown, "NoMergeReceipt",
					"The merge cannot be bound to a merge receipt", 2*time.Minute),
				// A newer False condition does not name the stop of an Inconclusive request: Verified does.
				cond(remediationv1alpha1.ConditionPlanFresh, metav1.ConditionFalse, "TargetChanged", "", time.Minute)},
			condition: remediationv1alpha1.ConditionVerified, reason: "NoMergeReceipt",
			link: troubleshootingURL + "#merge-receipt", listed: true},
		{phase: remediationv1alpha1.PhaseFailed,
			conds: []metav1.Condition{allowed, cond(remediationv1alpha1.ConditionVerified, metav1.ConditionFalse,
				"SomethingNew", "A reason the page does not list", time.Minute)},
			condition: remediationv1alpha1.ConditionVerified, reason: "SomethingNew",
			link: troubleshootingURL, listed: false},
	} {
		t.Run(string(tc.phase), func(t *testing.T) {
			rr := explainRequestObj(testRR, tc.phase, time.Hour, tc.conds...)
			c := explainClient(t, rr, testIncident("incident-a", "a", testSubject, "OOM"), testSettings())
			e, err := explain(context.Background(), c, testNS, testRR)
			if err != nil {
				t.Fatal(err)
			}
			if e.Kind != kindRequest || len(e.Requests) != 1 || e.IncidentState != stateOK || e.Incident == nil {
				t.Fatalf("explanation = %+v", e)
			}
			r := e.Requests[0]
			if r.Stop == nil || r.Stop.Condition != tc.condition || r.Stop.Reason != tc.reason ||
				r.Stop.Link != tc.link || r.Stop.Listed != tc.listed {
				t.Fatalf("stop = %+v, want %s %s %s listed=%v", r.Stop, tc.condition, tc.reason, tc.link, tc.listed)
			}
			for i := 1; i < len(r.Conditions); i++ {
				if r.Conditions[i].Time.Before(r.Conditions[i-1].Time) {
					t.Errorf("conditions not oldest first: %+v", r.Conditions)
				}
			}
			text, _ := explainOut(t, e)
			for _, want := range []string{
				"Request rr-a in namespace " + testNS,
				"Phase:        " + string(tc.phase) + ", since 60s ago",
				"Stopped by:   " + tc.condition + " ",
				"Read more:    " + tc.link,
				"Incident:     incident-a (Deployment demo/oom-app, OOMKilled, phase Proposed)",
				"Action:       SetContainerMemoryLimit on Deployment demo/oom-app, container app, 256Mi to 512Mi",
				"(policy default, revision default/3), approval required",
				"Plan digest:  sha256:abab",
				"Source:       repository fleet, branch main, file apps/demo/deployment.yaml",
				"Conditions, oldest first:",
				"Kill switch:  ON",
			} {
				if !strings.Contains(text, want) {
					t.Errorf("text lacks %q:\n%s", want, text)
				}
			}
		})
	}
}

// TestExplainMidFlight explains a request waiting for review after a handoff: no stop, the pull request with
// its link, head and merge commit, and both sides of the handoff.
func TestExplainMidFlight(t *testing.T) {
	rr := explainRequestObj("rr-b", remediationv1alpha1.PhaseAwaitingApproval, time.Hour,
		cond(remediationv1alpha1.ConditionPolicyAllowed, metav1.ConditionTrue, "WithinApprovedBounds", "", 30*time.Minute),
		cond(remediationv1alpha1.ConditionPROpened, metav1.ConditionTrue, "PullRequestOpened", "", 20*time.Minute),
		cond(remediationv1alpha1.ConditionApproved, metav1.ConditionFalse, "WaitingForReview", "Waits for a person",
			10*time.Minute))
	rr.Status.PullRequest = &remediationv1alpha1.PullRequestStatus{Number: 7, URL: testRepo + "/pull/7",
		HeadSHA: "abc123", MergeCommitSHA: "def456"}
	rr.Spec.Replaces = &remediationv1alpha1.RequestReplacement{Name: "rr-old", UID: testOldUID}
	rr.Spec.ChainRoot = testOldUID
	rr.Status.Handoff = &remediationv1alpha1.Handoff{
		From: &remediationv1alpha1.IncomingHandoff{Name: "rr-old", UID: testOldUID, Phase: remediationv1alpha1.HandoffDone},
		To:   &remediationv1alpha1.OutgoingHandoff{Name: "rr-new", UID: "uid-new", Phase: remediationv1alpha1.HandoffIntent},
	}
	c := explainClient(t, rr, testIncident("incident-a", "a", testSubject, "OOM"), testSettings())
	e, err := explain(context.Background(), c, testNS, "rr-b")
	if err != nil {
		t.Fatal(err)
	}
	r := e.Requests[0]
	if r.Stop != nil {
		t.Errorf("a request in flight has a stop: %+v", r.Stop)
	}
	text, js := explainOut(t, e)
	for _, want := range []string{
		"Phase:        AwaitingApproval",
		"Policy:       WithinApprovedBounds (policy default",
		"Pull request: " + testRepo + "/pull/7, head abc123, merge commit def456",
		"Replaces:     rr-old (handoff Done)",
		"Replaced by:  rr-new (handoff Intent)",
		"Chain root:   uid-old",
		"Approved=False WaitingForReview: Waits for a person",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("text lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "Stopped by") {
		t.Errorf("text names a stop for a request in flight:\n%s", text)
	}

	// The JSON shape: the keys a script reads.
	var got map[string]any
	if err := json.Unmarshal([]byte(js), &got); err != nil {
		t.Fatal(err)
	}
	wantKeys := strings.Fields("incident incidentState kind name namespace requests settings")
	if keys := slices.Sorted(maps.Keys(got)); !slices.Equal(keys, wantKeys) {
		t.Errorf("top-level keys = %v, want %v", keys, wantKeys)
	}
	req := got["requests"].([]any)[0].(map[string]any)
	wantReq := strings.Fields("action chainRoot conditions created handoffFrom handoffTo incident " +
		"name phase phaseSince planDigest policy pullRequest replaces source target")
	if keys := slices.Sorted(maps.Keys(req)); !slices.Equal(keys, wantReq) {
		t.Errorf("request keys = %v, want %v", keys, wantReq)
	}
	pr := req["pullRequest"].(map[string]any)
	if pr["url"] != testRepo+"/pull/7" || pr["head"] != "abc123" || pr["mergeCommit"] != "def456" || pr["number"] != 7.0 {
		t.Errorf("pullRequest = %v", pr)
	}
	action := req["action"].(map[string]any)
	if action["type"] != "SetContainerMemoryLimit" || action["previous"] != "256Mi" || action["desired"] != "512Mi" {
		t.Errorf("action = %v", action)
	}
	conds := req["conditions"].([]any)
	if len(conds) != 3 || conds[0].(map[string]any)["type"] != remediationv1alpha1.ConditionPolicyAllowed {
		t.Errorf("conditions = %v", conds)
	}
}

// TestExplainOutcomeLines shows the three outcomes of verification as three lines (ADR-0030 section 3): rolled
// out, configured and cleared, with "not reported yet" for one the executor has not written, and in JSON under
// outcome. A request before the merge has none of them and no outcome lines.
func TestExplainOutcomeLines(t *testing.T) {
	rolled := cond(conditionRolledOut, metav1.ConditionTrue, "RolloutComplete", "Every pod runs the applied template",
		5*time.Minute)
	configured := cond(remediationv1alpha1.ConditionConfigured, metav1.ConditionTrue, "ValueConfigured",
		"The live target holds the desired value", 5*time.Minute)
	cleared := cond(remediationv1alpha1.ConditionCleared, metav1.ConditionTrue, "NoRecurrence",
		"No OOM kill and no restart for 11m0s", time.Minute)
	for _, tc := range []struct {
		name  string
		phase remediationv1alpha1.RemediationPhase
		conds []metav1.Condition
		want  []string
	}{
		{"succeeded", remediationv1alpha1.PhaseSucceeded, []metav1.Condition{rolled, configured, cleared}, []string{
			"Rolled out:   True, RolloutComplete: Every pod runs the applied template",
			"Configured:   True, ValueConfigured: The live target holds the desired value",
			"Cleared:      True, NoRecurrence: No OOM kill and no restart for 11m0s",
		}},
		{"verifying", remediationv1alpha1.PhaseVerifying, []metav1.Condition{configured}, []string{
			"Rolled out:   not reported yet",
			"Configured:   True, ValueConfigured",
			"Cleared:      not reported yet",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rr := explainRequestObj(testRR, tc.phase, time.Hour, tc.conds...)
			c := explainClient(t, rr, testIncident("incident-a", "a", testSubject, "OOM"), testSettings())
			e, err := explain(context.Background(), c, testNS, testRR)
			if err != nil {
				t.Fatal(err)
			}
			text, js := explainOut(t, e)
			for _, want := range tc.want {
				if !strings.Contains(text, want) {
					t.Errorf("text lacks %q:\n%s", want, text)
				}
			}
			var got struct {
				Requests []struct {
					Outcome map[string]any `json:"outcome"`
				} `json:"requests"`
			}
			if err := json.Unmarshal([]byte(js), &got); err != nil {
				t.Fatal(err)
			}
			if len(got.Requests) != 1 || got.Requests[0].Outcome["configured"] == nil {
				t.Errorf("JSON outcome = %+v", got.Requests)
			}
		})
	}
	rr := explainRequestObj(testRR, remediationv1alpha1.PhaseAwaitingApproval, time.Hour)
	c := explainClient(t, rr, testIncident("incident-a", "a", testSubject, "OOM"), testSettings())
	e, err := explain(context.Background(), c, testNS, testRR)
	if err != nil {
		t.Fatal(err)
	}
	if text, _ := explainOut(t, e); strings.Contains(text, "Rolled out:") || e.Requests[0].Outcome != nil {
		t.Errorf("a request before the merge shows outcome lines:\n%s", text)
	}
}

// TestExplainIncident explains an Incident: its requests, newest first, without the requests of another
// Incident of the same name (another UID).
func TestExplainIncident(t *testing.T) {
	older := explainRequestObj("rr-older", remediationv1alpha1.PhaseDenied, 2*time.Hour,
		cond(remediationv1alpha1.ConditionPolicyAllowed, metav1.ConditionFalse, "PolicyNotFound", "", time.Hour))
	newer := explainRequestObj("rr-newer", remediationv1alpha1.PhasePending, time.Hour)
	other := explainRequestObj("rr-other-uid", remediationv1alpha1.PhasePending, time.Minute)
	other.Spec.IncidentRef.UID = "uid-other"
	c := explainClient(t, older, newer, other, testIncident("incident-a", "a", testSubject, "OOM"), testSettings())
	e, err := explain(context.Background(), c, testNS, "incident-a")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(e.Requests))
	for _, r := range e.Requests {
		names = append(names, r.Name)
	}
	if e.Kind != kindIncident || !slices.Equal(names, []string{"rr-newer", "rr-older"}) {
		t.Fatalf("kind %s, requests %v; want Incident, [rr-newer rr-older]", e.Kind, names)
	}
	text, _ := explainOut(t, e)
	for _, want := range []string{
		"Incident incident-a in namespace " + testNS,
		"Subject:      Deployment demo/oom-app",
		"Requests, newest first: 2",
		"Request rr-newer",
		"Conditions:   none yet",
		"Read more:    " + troubleshootingURL + "#request-and-target",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("text lacks %q:\n%s", want, text)
		}
	}
	if strings.Index(text, "rr-newer") > strings.Index(text, "rr-older") {
		t.Errorf("requests not newest first:\n%s", text)
	}
}

// TestExplainIncidentStates: the request's Incident may be gone, another one of the same name, or not
// readable; the request is still explained.
func TestExplainIncidentStates(t *testing.T) {
	rr := explainRequestObj(testRR, remediationv1alpha1.PhasePending, time.Hour)
	e, err := explain(context.Background(), explainClient(t, rr, testSettings()), testNS, testRR)
	if err != nil || e.IncidentState != stateNotFound || e.Incident != nil {
		t.Errorf("gone: %+v, %v", e, err)
	}
	e, err = explain(context.Background(),
		explainClient(t, rr, testIncident("incident-a", "b", testSubject, "OOM")), testNS, testRR)
	if err != nil || e.IncidentState != stateUIDMismatch || e.Incident != nil {
		t.Errorf("other UID: %+v, %v", e, err)
	}
	text, _ := explainOut(t, e)
	if !strings.Contains(text, "is not the one this request answers") || !strings.Contains(text, "Kill switch:  unknown") {
		t.Errorf("text:\n%s", text)
	}
	forbidIncident := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(rr).
		WithInterceptorFuncs(interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey,
			obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*remediationv1alpha1.Incident); ok {
				return apierrors.NewForbidden(schema.GroupResource{Resource: "incidents"}, key.Name, nil)
			}
			return c.Get(ctx, key, obj, opts...)
		}}).Build()
	e, err = explain(context.Background(), forbidIncident, testNS, testRR)
	if err != nil || e.IncidentState != stateForbidden {
		t.Errorf("forbidden: %+v, %v", e, err)
	}
}

func TestExplainErrors(t *testing.T) {
	c := explainClient(t, testSettings())
	if _, err := explain(context.Background(), c, testNS, "nothing"); err == nil ||
		!strings.Contains(err.Error(), "no RemediationRequest or Incident nothing") {
		t.Errorf("missing name: %v", err)
	}
	if _, err := explain(context.Background(), c, testNS, "a/b"); err == nil {
		t.Error("a name with a slash was read")
	}
	forbidden := fake.NewClientBuilder().WithScheme(testScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object,
			opts ...client.GetOption) error {
			return apierrors.NewForbidden(schema.GroupResource{Resource: "remediationrequests"}, key.Name, nil)
		}}).Build()
	if _, err := explain(context.Background(), forbidden, testNS, testRR); !apierrors.IsForbidden(err) {
		t.Errorf("forbidden read: %v", err)
	}
}

// TestExplainPrintsNoEscapes feeds escape sequences and control characters through every kind of string the
// explanation prints, and checks that neither output carries them or their payloads.
func TestExplainPrintsNoEscapes(t *testing.T) {
	rr := explainRequestObj(testRR, remediationv1alpha1.PhaseNeedsIntervention, time.Hour,
		cond(remediationv1alpha1.ConditionPolicyAllowed, metav1.ConditionUnknown, "Target\x1b[2JNotFound",
			"gone\x1b]0;pwned-title\x07 now\u202e\x1b]8;;https://evil.example/\x1b\\x\x1b]8;;\x1b\\", time.Minute))
	rr.Spec.Action.SetContainerMemoryLimit.Container = "app\x1b[1A\u009b2K"
	rr.Status.Source.Path = "apps/\x07demo\u200b.yaml"
	rr.Status.PullRequest = &remediationv1alpha1.PullRequestStatus{Number: 3, URL: "https://evil.example/pull/3",
		HeadSHA: "abc\x1bP+q\x1b\\"}
	rr.Status.Handoff = &remediationv1alpha1.Handoff{To: &remediationv1alpha1.OutgoingHandoff{
		Name: "rr-\x1b[31mnew", Phase: remediationv1alpha1.HandoffIntent}}
	inc := testIncident("incident-a", "a", "evil\x1b]0;pwned-title\x07app", "sym")
	c := explainClient(t, rr, inc, testSettings())
	e, err := explain(context.Background(), c, testNS, testRR)
	if err != nil {
		t.Fatal(err)
	}
	text, js := explainOut(t, e)
	for name, out := range map[string]string{outputText: text, outputJSON: js} {
		for _, bad := range []string{"\x1b", "\x07", "\u009b", "\u202e", "\u200b", `\u001b`, `\u0007`,
			"pwned-title", "evil.example"} {
			if strings.Contains(out, bad) {
				t.Errorf("%s output holds %q:\n%s", name, bad, out)
			}
		}
		for _, want := range []string{"TargetNotFound", "gone now", "evilapp", "apps/demo.yaml", "rr-new"} {
			if !strings.Contains(out, want) {
				t.Errorf("%s output lacks %q:\n%s", name, want, out)
			}
		}
	}
	if !strings.Contains(text, "#3 (link not shown") {
		t.Errorf("text shows a pull request link off the configured repository:\n%s", text)
	}
	// A reason is looked up as written, before cleaning: one with control characters is no listed reason,
	// and the link is the page, never text from the cluster.
	if s := e.Requests[0].Stop; s.Link != troubleshootingURL || s.Listed {
		t.Errorf("stop = %+v, want the page, not listed", s)
	}
}

func TestParseExplain(t *testing.T) {
	for _, args := range [][]string{
		{testRR, "-n", "x", "-o", outputJSON},
		{"-n", "x", testRR, "--output", outputJSON},
		{"-n", "x", "-o", outputJSON, testRR},
	} {
		f, err := parseExplain(args)
		if err != nil || f.name != testRR || f.namespace != "x" || f.output != outputJSON {
			t.Errorf("parseExplain(%q) = %+v, %v", args, f, err)
		}
	}
	for _, args := range [][]string{{}, {"a", "b"}, {"a", "-A"}, {"a", "-o", "yaml"}, {"-n", "x"}} {
		if _, err := parseExplain(args); err == nil {
			t.Errorf("parseExplain(%q) succeeded", args)
		}
	}
	if _, err := parseExplain([]string{"--help"}); !errors.Is(err, flag.ErrHelp) {
		t.Errorf("--help gave %v, want flag.ErrHelp", err)
	}
	var stdout, stderr bytes.Buffer
	if err := run([]string{"explain"}, &stdout, &stderr); err == nil || stdout.Len() != 0 {
		t.Errorf("explain without a name: %v, %q", err, stdout.String())
	}
}

// TestTroubleshootingSections keeps troubleshootingSections equal to the tables of docs/troubleshooting.md:
// every reason in the first column of a table whose first header is Reason, and every "with reason `X`" of
// the phase table, under the anchor the docs site gives its "## " section.
func TestTroubleshootingSections(t *testing.T) {
	f, err := os.Open("../../docs/troubleshooting.md")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	reasonCell := regexp.MustCompile("^`([A-Za-z]+)`$")
	withReason := regexp.MustCompile("with reason `([A-Za-z]+)`")
	got := map[string][]string{}
	add := func(reason, section string) {
		if !slices.Contains(got[reason], section) {
			got[reason] = append(got[reason], section)
		}
	}
	var section, header string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if heading, ok := strings.CutPrefix(line, "## "); ok {
			section, header = slug(heading), ""
			continue
		}
		if !strings.HasPrefix(line, "|") {
			header = ""
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		first := strings.TrimSpace(cells[0])
		switch {
		case header == "":
			header = first
		case strings.HasPrefix(first, "---"):
		case header == "Reason":
			if m := reasonCell.FindStringSubmatch(first); m != nil {
				add(m[1], section)
			}
		case header == "Phase":
			for _, m := range withReason.FindAllStringSubmatch(first, -1) {
				add(m[1], section)
			}
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	for r, s := range got {
		if !slices.Equal(troubleshootingSections[r], s) {
			t.Errorf("reason %s: troubleshootingSections has %v, the page %v", r, troubleshootingSections[r], s)
		}
	}
	for r := range troubleshootingSections {
		if _, ok := got[r]; !ok {
			t.Errorf("reason %s is in troubleshootingSections but in no table of the page", r)
		}
	}
	for ct, s := range conditionSections {
		found := false
		for _, ss := range got {
			found = found || slices.Contains(ss, s)
		}
		if !found {
			t.Errorf("conditionSections[%s] = %s, a section with no reasons", ct, s)
		}
	}
}

// slug is the heading anchor of Python-Markdown's toc extension, which the docs site uses: punctuation
// removed, lower case, runs of spaces and hyphens as one hyphen.
func slug(heading string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(heading) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '-', r == ' ':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + 'a' - 'A')
		}
	}
	return regexp.MustCompile(`[-\s]+`).ReplaceAllString(b.String(), "-")
}

// The stop is named by the newest False condition, not by a newer Unknown one that only says what the request
// waited for (an ended request keeps Approved Unknown WaitingForReview).
func TestStopReportPrefersFalseOverUnknown(t *testing.T) {
	at := func(m int) metav1.Time { return metav1.NewTime(time.Date(2026, 11, 9, 12, m, 0, 0, time.UTC)) }
	conds := []metav1.Condition{
		{Type: "PlanFresh", Status: metav1.ConditionFalse, Reason: "TargetFileChanged", LastTransitionTime: at(1)},
		{Type: "Approved", Status: metav1.ConditionUnknown, Reason: "WaitingForReview", LastTransitionTime: at(2)},
		{Type: "PolicyAllowed", Status: metav1.ConditionTrue, Reason: "Allowed", LastTransitionTime: at(3)},
	}
	if got := stopReport(conds); got == nil || got.Condition != "PlanFresh" || got.Reason != "TargetFileChanged" {
		t.Errorf("stopReport = %+v, want PlanFresh TargetFileChanged", got)
	}
	waiting := conds[1:]
	if got := stopReport(waiting); got == nil || got.Condition != "Approved" {
		t.Errorf("stopReport without a False condition = %+v, want Approved", got)
	}
}
