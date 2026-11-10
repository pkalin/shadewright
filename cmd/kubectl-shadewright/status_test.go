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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/analysis"
	"github.com/pkalin/shadewright/internal/incidents"
)

const (
	testNS       = "shadewright-system"
	testRepo     = "https://github.com/acme/fleet"
	testRepoName = "fleet"
	testSubject  = "oom-app"
)

var testNow = time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := remediationv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func testSettings() *remediationv1alpha1.RemediationSettings {
	return &remediationv1alpha1.RemediationSettings{
		ObjectMeta: metav1.ObjectMeta{Name: remediationv1alpha1.SettingsName},
		Spec: remediationv1alpha1.RemediationSettingsSpec{
			Paused:   new(true),
			Analysis: &remediationv1alpha1.AnalysisSettings{MaxAnalysesPerDay: ptr.To[int32](20)},
			Repositories: []remediationv1alpha1.GitRepository{{
				Name: testRepoName, URL: testRepo, Provider: remediationv1alpha1.GitProviderGitHub,
			}},
		},
	}
}

func testIncident(name, uid, subject, symptom string) *remediationv1alpha1.Incident {
	return &remediationv1alpha1.Incident{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS, UID: k8stypes.UID("uid-" + uid)},
		Spec: remediationv1alpha1.IncidentSpec{
			Subject: remediationv1alpha1.ObjectRef{APIVersion: "apps/v1", Kind: "Deployment", Namespace: "demo", Name: subject},
			Reason:  "OOMKilled", Symptom: symptom,
		},
		Status: remediationv1alpha1.IncidentStatus{
			Phase: remediationv1alpha1.IncidentProposed, Count: 3,
			LastSeen:         &metav1.Time{Time: testNow.Add(-2 * time.Minute)},
			LastAnalysisTime: &metav1.Time{Time: testNow.Add(-time.Minute)},
			Conditions: []metav1.Condition{
				{Type: incidents.ConditionAnalysisDue, Status: metav1.ConditionFalse, Reason: incidents.ReasonProposed,
					Message: "The last analysis made a proposal", LastTransitionTime: metav1.Time{Time: testNow.Add(-time.Minute)}},
				{Type: conditionAnalyzed, Status: metav1.ConditionTrue, Reason: "Proposed",
					Message: "The analysis proposed a change", LastTransitionTime: metav1.Time{Time: testNow.Add(-time.Minute)}},
			},
		},
	}
}

// testRequest returns a request for incident-a (with the UID suffix uid) on the test subject.
func testRequest(name, uid string, pr int64, prURL string) *remediationv1alpha1.RemediationRequest {
	rr := &remediationv1alpha1.RemediationRequest{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS},
		Spec: remediationv1alpha1.RemediationRequestSpec{
			IncidentRef: remediationv1alpha1.IncidentRef{Name: "incident-a", UID: remediationv1alpha1.UID("uid-" + uid)},
			TargetRef: remediationv1alpha1.ObjectRef{
				APIVersion: "apps/v1", Kind: "Deployment", Namespace: "demo", Name: testSubject},
			Action: remediationv1alpha1.Action{Type: "SetContainerMemoryLimit"},
		},
		Status: remediationv1alpha1.RemediationRequestStatus{
			Phase:  remediationv1alpha1.PhaseAwaitingApproval,
			Source: &remediationv1alpha1.ResolvedSource{RepositoryRef: testRepoName},
			Conditions: []metav1.Condition{{Type: remediationv1alpha1.ConditionPolicyAllowed,
				Status: metav1.ConditionTrue, Reason: "Allowed", LastTransitionTime: metav1.Time{Time: testNow}}},
		},
	}
	if pr > 0 {
		rr.Status.PullRequest = &remediationv1alpha1.PullRequestStatus{Number: pr, URL: prURL}
	}
	return rr
}

func budgetMap(day, used string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: incidents.BudgetName, Namespace: testNS},
		Data:       map[string]string{incidents.BudgetDayKey: day, incidents.BudgetUsedKey: used},
	}
}

func render(t *testing.T, r *Report) (string, string) {
	t.Helper()
	var text, js bytes.Buffer
	if err := renderText(&text, r, testNow); err != nil {
		t.Fatal(err)
	}
	if err := renderJSON(&js, r); err != nil {
		t.Fatal(err)
	}
	return text.String(), js.String()
}

// TestStatusPrintsNoEscapes feeds a subject name, a symptom (the summary of the incident) and condition
// messages holding ANSI and OSC sequences and C1 and bidi controls, and checks that neither output carries
// them or their payloads, while the visible text stays.
func TestStatusPrintsNoEscapes(t *testing.T) {
	inc := testIncident("incident-a", "a", "evil\x1b]0;pwned-title\x07app\x1b[2J",
		"\x1b[31mOOM\x1b[0m summary \u009b1m\u202ebidi")
	inc.Status.Conditions[0].Message = "due\x1b]8;;https://evil.example/\x1b\\link\x1b]8;;\x1b\\"
	inc.Status.Conditions[1].Message = "outcome\x1bP+q\x1b\\\x07"
	rr := testRequest("rr-a\x1b[1A", "a", 7, testRepo+"/pull/7")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(inc, rr, testSettings(), budgetMap("2026-11-06", "4")).Build()

	r, err := collect(context.Background(), c, Options{Namespace: testNS}, testNow)
	if err != nil {
		t.Fatal(err)
	}
	text, js := render(t, r)
	for name, out := range map[string]string{outputText: text, outputJSON: js} {
		for _, bad := range []string{"\x1b", "\x07", "\u009b", "\u202e", `\u001b`, `\u0007`, "pwned-title", "evil.example"} {
			if strings.Contains(out, bad) {
				t.Errorf("%s output holds %q:\n%s", name, bad, out)
			}
		}
		for _, want := range []string{"evilapp", "OOM summary", "duelink", "outcome", "rr-a"} {
			if !strings.Contains(out, want) {
				t.Errorf("%s output lacks %q:\n%s", name, want, out)
			}
		}
	}
	var back Report
	if err := json.Unmarshal([]byte(js), &back); err != nil {
		t.Fatalf("JSON output does not parse: %v", err)
	}
}

func TestStatusText(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(
		testIncident("incident-a", "a", testSubject, "container memory limit reached"),
		testRequest("rr-a", "a", 7, testRepo+"/pull/7"),
		testRequest("rr-old-uid", "other", 0, ""),
		testSettings(), budgetMap("2026-11-06", "4"),
	).Build()
	r, err := collect(context.Background(), c, Options{Namespace: testNS}, testNow)
	if err != nil {
		t.Fatal(err)
	}
	text, _ := render(t, r)
	for _, want := range []string{
		"Kill switch:  ON: writes are paused",
		"Analyses today (UTC): 4 of 20",
		"Incident incident-a",
		"Subject:   Deployment demo/oom-app",
		"Due:       no, Proposed: The last analysis made a proposal",
		"Analysis:  began 60s ago, outcome Proposed: The analysis proposed a change",
		"Request:   rr-a  AwaitingApproval  SetContainerMemoryLimit on Deployment/oom-app, policy Allowed, PR " +
			testRepo + "/pull/7",
		"Requests without a listed Incident:",
		"rr-old-uid",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("text lacks %q:\n%s", want, text)
		}
	}
	if got := len(r.Namespaces[0].Incidents[0].Requests); got != 1 {
		t.Errorf("requests under the Incident = %d, want 1 (a request naming another UID is not its own)", got)
	}
}

func TestPRLink(t *testing.T) {
	s := testSettings()
	other := testSettings()
	other.Spec.Repositories[0].URL = "https://github.com/acme/other"
	cases := map[string]struct {
		url      string
		repoRef  remediationv1alpha1.Name
		settings *remediationv1alpha1.RemediationSettings
		want     string
	}{
		"configured repository": {testRepo + "/pull/7", testRepoName, s, testRepo + "/pull/7"},
		"other host":            {"https://evil.example/acme/fleet/pull/7", testRepoName, s, ""},
		"other number":          {testRepo + "/pull/8", testRepoName, s, ""},
		"look-alike suffix":     {testRepo + "/pull/7/../../x", testRepoName, s, ""},
		"unknown repositoryRef": {testRepo + "/pull/7", "nope", s, ""},
		"settings changed":      {testRepo + "/pull/7", testRepoName, other, ""},
		"no settings":           {testRepo + "/pull/7", testRepoName, nil, ""},
	}
	for name, tc := range cases {
		rr := testRequest("rr", "a", 7, tc.url)
		rr.Status.Source.RepositoryRef = tc.repoRef
		if got := prLink(rr, tc.settings); got != tc.want {
			t.Errorf("%s: prLink = %q, want %q", name, got, tc.want)
		}
	}
	rr := testRequest("rr", "a", 7, "https://evil.example/x")
	r := requestReport(rr, s)
	if r.PullRequestURL != "" || r.PullRequest != 7 {
		t.Errorf("requestReport = %+v, want number 7 and no link", r)
	}
	var b bytes.Buffer
	report := &Report{Namespaces: []NamespaceReport{{OtherRequests: []RequestReport{r}}}}
	if err := renderText(&b, report, testNow); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), "evil.example") || !strings.Contains(b.String(), "PR #7 (link not shown") {
		t.Errorf("text = %s", b.String())
	}
}

func TestBudgetAndSettingsStates(t *testing.T) {
	forbidden := interceptor.Funcs{Get: func(_ context.Context, _ client.WithWatch, key client.ObjectKey, _ client.Object,
		_ ...client.GetOption) error {
		return apierrors.NewForbidden(schema.GroupResource{Resource: "x"}, key.Name, nil)
	}}
	failing := interceptor.Funcs{Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object,
		_ ...client.GetOption) error {
		return errors.New("connection refused")
	}}
	noLimit := testSettings()
	noLimit.Spec.Analysis = nil
	zero := testSettings()
	zero.Spec.Analysis.MaxAnalysesPerDay = new(int32(0))
	cases := map[string]struct {
		objs     []client.Object
		funcs    *interceptor.Funcs
		budget   string
		settings string
		stopped  string
		text     string
	}{
		"settings not found": {nil, nil, stateNotFound, stateNotFound, incidents.ReasonSettingsNotFound,
			"analysis stopped: the RemediationSettings object default does not exist (SettingsNotFound)"},
		"limit unset": {[]client.Object{noLimit, budgetMap("2026-11-06", "1")}, nil, stateOK, stateOK,
			incidents.ReasonSettingsIncomplete, "analysis stopped: RemediationSettings sets no"},
		"budget not found": {[]client.Object{testSettings()}, nil, stateNotFound, stateOK,
			incidents.ReasonBudgetNotFound, "analysis stopped: the ConfigMap shadewright-analysis-budget does not exist"},
		"limit 0": {[]client.Object{zero, budgetMap("2026-11-06", "0")}, nil, stateOK, stateOK,
			incidents.ReasonAnalysisDisabled, "0 of 0, analysis stopped: the daily limit is 0 (AnalysisDisabled)"},
		"limit reached": {[]client.Object{testSettings(), budgetMap("2026-11-06", "20")}, nil, stateOK, stateOK,
			incidents.ReasonDailyBudgetReached, "20 of 20, analysis stopped: the daily limit is reached"},
		"forbidden is not a stop": {nil, &forbidden, stateForbidden, stateForbidden, "",
			"unknown (RemediationSettings not readable with your permissions (see the viewer role); ConfigMap"},
		"read error is not a stop": {nil, &failing, stateError, stateError, "", "could not be read: connection refused"},
		"earlier day": {[]client.Object{testSettings(), budgetMap("2026-11-05", "9")}, nil,
			stateOK, stateOK, "", "Analyses today (UTC): 0 of 20\n"},
		"future day": {[]client.Object{testSettings(), budgetMap("2026-11-07", "1")}, nil,
			stateUnreadable, stateOK, incidents.ReasonBudgetUnreadable, "taken as spent"},
		"garbage is refused": {[]client.Object{testSettings(), budgetMap("2026-11-06", "\x1b[31m1")}, nil,
			stateUnreadable, stateOK, incidents.ReasonBudgetUnreadable, "taken as spent"},
	}
	for name, tc := range cases {
		b := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(tc.objs...)
		if tc.funcs != nil {
			b = b.WithInterceptorFuncs(*tc.funcs)
		}
		r, err := collect(context.Background(), b.Build(), Options{Namespace: testNS}, testNow)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		bud := r.Namespaces[0].Budget
		if bud.State != tc.budget || r.Settings.State != tc.settings || bud.AnalysisStopped != tc.stopped {
			t.Errorf("%s: budget %q, settings %q, stopped %q; want %q, %q, %q", name, bud.State, r.Settings.State,
				bud.AnalysisStopped, tc.budget, tc.settings, tc.stopped)
		}
		text, _ := render(t, r)
		if !strings.Contains(text, tc.text) {
			t.Errorf("%s: text lacks %q:\n%s", name, tc.text, text)
		}
		if tc.stopped == "" && strings.Contains(text, "analysis stopped") {
			t.Errorf("%s: says analysis stopped:\n%s", name, text)
		}
	}
}

func TestAllNamespaces(t *testing.T) {
	a := testIncident("incident-a", "a", testSubject, "")
	b := testIncident("incident-b", "b", testSubject, "")
	b.Namespace = "team-b"
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(a, b).Build()
	r, err := collect(context.Background(), c, Options{Namespace: testNS}, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Namespaces) != 1 || len(r.Namespaces[0].Incidents) != 1 {
		t.Errorf("one namespace: %+v", r.Namespaces)
	}
	r, err = collect(context.Background(), c, Options{Namespace: testNS, AllNamespaces: true}, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Namespaces) != 2 || r.Namespaces[0].Namespace != testNS || r.Namespaces[1].Namespace != "team-b" {
		t.Errorf("all namespaces: %+v", r.Namespaces)
	}
}

// TestAllNamespacesEmpty: -A on a cluster without Incidents still shows the kubeconfig namespace with its
// budget and "No Incidents.", and the JSON has an empty list, not null.
func TestAllNamespacesEmpty(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(testSettings(), budgetMap("2026-11-06", "2")).Build()
	r, err := collect(context.Background(), c, Options{Namespace: testNS, AllNamespaces: true}, testNow)
	if err != nil {
		t.Fatal(err)
	}
	text, js := render(t, r)
	for _, want := range []string{"Namespace " + testNS, "Analyses today (UTC): 2 of 20", "No Incidents."} {
		if !strings.Contains(text, want) {
			t.Errorf("text lacks %q:\n%s", want, text)
		}
	}
	if !strings.Contains(js, `"incidents": []`) {
		t.Errorf("JSON has no empty incidents list:\n%s", js)
	}
	empty, err := collect(context.Background(), c, Options{AllNamespaces: true}, testNow)
	if err != nil {
		t.Fatal(err)
	}
	_, js = render(t, empty)
	if !strings.Contains(js, `"namespaces": []`) {
		t.Errorf("JSON with no namespace is not an empty list:\n%s", js)
	}
}

func TestConditionWithoutTime(t *testing.T) {
	inc := testIncident("incident-a", "a", testSubject, "")
	inc.Status.Conditions[0].LastTransitionTime = metav1.Time{}
	r := incidentReport(inc)
	if r.Due.Since != nil {
		t.Errorf("Since = %v, want nil", r.Due.Since)
	}
	if got := condition(r.Due, testNow); strings.Contains(got, "since") {
		t.Errorf("condition = %q, want no since", got)
	}
	var b bytes.Buffer
	if err := renderJSON(&b, &Report{Namespaces: []NamespaceReport{{Incidents: []IncidentReport{r}}}}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), `"since": null`) || strings.Count(b.String(), `"since"`) != 1 {
		t.Errorf("JSON should hold one since (the Analyzed condition's):\n%s", b.String())
	}
}

func TestListFailureIsAnError(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			return apierrors.NewForbidden(schema.GroupResource{Resource: "incidents"}, "", nil)
		}}).Build()
	if _, err := collect(context.Background(), c, Options{Namespace: testNS}, testNow); err == nil {
		t.Fatal("collect succeeded with a forbidden list")
	}
}

func TestConditionAnalyzedMatchesAnalysis(t *testing.T) {
	if conditionAnalyzed != analysis.ConditionAnalyzed {
		t.Errorf("conditionAnalyzed = %q, analysis.ConditionAnalyzed = %q", conditionAnalyzed, analysis.ConditionAnalyzed)
	}
}

func TestParseStatus(t *testing.T) {
	f, err := parseStatus([]string{"-n", "x", "-o", outputJSON})
	if err != nil || f.namespace != "x" || f.output != outputJSON || f.timeout != 30*time.Second {
		t.Errorf("parseStatus = %+v, %v", f, err)
	}
	f, err = parseStatus([]string{"--all-namespaces"})
	if err != nil || !f.allNamespaces || f.output != outputText {
		t.Errorf("parseStatus = %+v, %v", f, err)
	}
	for _, args := range [][]string{
		{"-o", "yaml"}, {"extra"}, {"--request-timeout", "-1"}, {"--request-timeout", "1x"}, {"--follow"},
	} {
		if _, err := parseStatus(args); err == nil {
			t.Errorf("parseStatus(%q) succeeded", args)
		}
	}
	if _, err := parseStatus([]string{"-h"}); !errors.Is(err, flag.ErrHelp) {
		t.Errorf("-h gave %v, want flag.ErrHelp", err)
	}
}

func TestParseTimeout(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"30": 30 * time.Second, "0": 0, "1m": time.Minute, "0s": 0, "1.5s": 1500 * time.Millisecond,
	} {
		if got, err := parseTimeout(in); err != nil || got != want {
			t.Errorf("parseTimeout(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"-5", "-1s", "abc", "", "99999999999999999999"} {
		if _, err := parseTimeout(in); err == nil {
			t.Errorf("parseTimeout(%q) succeeded", in)
		}
	}
}

// TestRunPrintsErrorsOnce: run prints nothing itself for a bad flag, so main prints the error once, and
// status -h prints the usage on stdout and succeeds.
func TestRunPrintsErrorsOnce(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := run([]string{"status", "--follow"}, &stdout, &stderr)
	if err == nil || stdout.Len() != 0 || stderr.Len() != 0 {
		t.Errorf("bad flag: err %v, stdout %q, stderr %q; want an error and no output", err, stdout.String(), stderr.String())
	}
	if err := run([]string{"status", "-h"}, &stdout, &stderr); err != nil || !strings.Contains(stdout.String(), "Usage:") {
		t.Errorf("status -h: err %v, stdout %q", err, stdout.String())
	}
	if err := run([]string{"delete"}, &stdout, &stderr); err == nil {
		t.Error("an unknown command succeeded")
	}
}
