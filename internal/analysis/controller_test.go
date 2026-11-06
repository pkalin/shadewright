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

package analysis

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/incidents"
)

// The API server is started once, on first use; each test works in its own namespace. The RemediationSettings
// object is cluster-scoped, so the tests that write it do not run in parallel.
var env struct {
	once    sync.Once
	err     error
	testEnv *envtest.Environment
	admin   client.Client
	nsSeq   atomic.Int32
}

func TestMain(m *testing.M) {
	code := m.Run()
	if env.testEnv != nil {
		_ = env.testEnv.Stop()
	}
	os.Exit(code)
}

func envtestBinaryDir() string {
	base := filepath.Join("..", "..", "bin", "k8s")
	entries, err := os.ReadDir(base)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if e.IsDir() {
			return filepath.Join(base, e.Name())
		}
	}
	return ""
}

func namespace(t *testing.T) string {
	t.Helper()
	env.once.Do(func() {
		scheme := runtime.NewScheme()
		if env.err = clientgoscheme.AddToScheme(scheme); env.err != nil {
			return
		}
		if env.err = remediationv1alpha1.AddToScheme(scheme); env.err != nil {
			return
		}
		env.testEnv = &envtest.Environment{
			CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
			ErrorIfCRDPathMissing: true,
			BinaryAssetsDirectory: envtestBinaryDir(),
		}
		cfg, err := env.testEnv.Start()
		if err != nil {
			env.err = err
			return
		}
		env.admin, env.err = client.New(cfg, client.Options{Scheme: scheme})
	})
	if env.err != nil {
		t.Fatalf("starting envtest: %v", env.err)
	}
	ns := fmt.Sprintf("operator-%d", env.nsSeq.Add(1))
	if err := env.admin.Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
		t.Fatal(err)
	}
	return ns
}

// recordingClient keeps a copy of every Incident status update it sends.
type recordingClient struct {
	client.Client
	mu      sync.Mutex
	updates []remediationv1alpha1.IncidentStatus
}

func (c *recordingClient) Status() client.SubResourceWriter {
	return &recordingStatus{SubResourceWriter: c.Client.Status(), c: c}
}

type recordingStatus struct {
	client.SubResourceWriter
	c *recordingClient
}

func (s *recordingStatus) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	if inc, ok := obj.(*remediationv1alpha1.Incident); ok {
		s.c.mu.Lock()
		s.c.updates = append(s.c.updates, *inc.Status.DeepCopy())
		s.c.mu.Unlock()
	}
	return s.SubResourceWriter.Update(ctx, obj, opts...)
}

// setSettings writes the one RemediationSettings with a triage model and the daily limit.
func setSettings(t *testing.T, perDay int32) {
	t.Helper()
	ctx := t.Context()
	l := testLimits()
	s := &remediationv1alpha1.RemediationSettings{}
	err := env.admin.Get(ctx, types.NamespacedName{Name: remediationv1alpha1.SettingsName}, s)
	if apierrors.IsNotFound(err) {
		s = &remediationv1alpha1.RemediationSettings{ObjectMeta: metav1.ObjectMeta{Name: remediationv1alpha1.SettingsName}}
		if err := env.admin.Create(ctx, s); err != nil {
			t.Fatal(err)
		}
	} else if err != nil {
		t.Fatal(err)
	}
	steps, out := int32(2), int32(1024)
	s.Spec.Analysis = &remediationv1alpha1.AnalysisSettings{
		MaxAnalysesPerDay: &perDay, MaxToolSteps: &steps, MaxOutputTokens: &out,
		Models: &remediationv1alpha1.AnalysisModels{Triage: l.Triage},
	}
	if err := env.admin.Update(ctx, s); err != nil {
		t.Fatal(err)
	}
}

// dueIncident creates an Incident in ns marked due by the trigger, and the empty budget ConfigMap.
func dueIncident(t *testing.T, ns string, budget map[string]string) *remediationv1alpha1.Incident {
	t.Helper()
	ctx := t.Context()
	if err := env.admin.Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: incidents.BudgetName},
		Data: budget}); err != nil {
		t.Fatal(err)
	}
	inc := incident()
	inc.Namespace, inc.Name = ns, incidents.Name(inc.Spec.Fingerprint)
	if err := env.admin.Create(ctx, inc); err != nil {
		t.Fatal(err)
	}
	now := metav1.NewTime(time.Now().Add(-time.Minute).Truncate(time.Second))
	inc.Status.Phase = remediationv1alpha1.IncidentOpen
	inc.Status.FirstSeen, inc.Status.LastSeen, inc.Status.Count = &now, &now, 3
	meta.SetStatusCondition(&inc.Status.Conditions, metav1.Condition{Type: incidents.ConditionAnalysisDue,
		Status: metav1.ConditionTrue, Reason: incidents.ReasonFirstAnalysis, Message: "due"})
	if err := env.admin.Status().Update(ctx, inc); err != nil {
		t.Fatal(err)
	}
	return inc
}

func newController(t *testing.T, ns string, f *fixture) (*Controller, *recordingClient) {
	t.Helper()
	rc := &recordingClient{Client: env.admin}
	f.cfg.Reserve = func(ctx context.Context, now time.Time) error {
		f.reserved++
		return incidents.ReserveAnalysis(ctx, env.admin, env.admin, ns, now)
	}
	a, err := New(f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	return &Controller{Client: rc, Reader: env.admin, Namespace: ns, Analyzer: a}, rc
}

func reconcile(t *testing.T, c *Controller, inc *remediationv1alpha1.Incident) {
	t.Helper()
	if _, err := c.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(inc)}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
}

func TestControllerProposes(t *testing.T) {
	ns := namespace(t)
	setSettings(t, 10)
	inc := dueIncident(t, ns, nil)
	f := newFixture()
	f.triage.steps = []step{reply(validResult)}
	c, rc := newController(t, ns, f)
	reconcile(t, c, inc)

	if len(rc.updates) != 2 {
		t.Fatalf("%d status updates, want the claim and the outcome", len(rc.updates))
	}
	claim := rc.updates[0]
	if claim.Phase != remediationv1alpha1.IncidentAnalyzing || claim.LastAnalysisTime == nil {
		t.Errorf("the claim wrote phase %q and lastAnalysisTime %v, want both in one update", claim.Phase, claim.LastAnalysisTime)
	}
	if cond := meta.FindStatusCondition(claim.Conditions, incidents.ConditionAnalysisDue); cond == nil ||
		cond.Status != metav1.ConditionFalse || cond.Reason != incidents.ReasonAnalyzing {
		t.Errorf("the claim left AnalysisDue %+v", cond)
	}

	got := &remediationv1alpha1.Incident{}
	if err := env.admin.Get(t.Context(), client.ObjectKeyFromObject(inc), got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != remediationv1alpha1.IncidentProposed || len(got.Status.RemediationRequests) != 1 ||
		!got.Status.LastAnalysisTime.Equal(claim.LastAnalysisTime) {
		t.Fatalf("status = %+v", got.Status)
	}
	if cond := meta.FindStatusCondition(got.Status.Conditions, ConditionAnalyzed); cond == nil || cond.Reason != string(ReasonProposed) {
		t.Errorf("Analyzed condition %+v", cond)
	}
	rr := &remediationv1alpha1.RemediationRequest{}
	if err := env.admin.Get(t.Context(), types.NamespacedName{Namespace: ns, Name: string(got.Status.RemediationRequests[0])}, rr); err != nil {
		t.Fatal(err)
	}
	if rr.Spec.ApprovalRequired == nil || !*rr.Spec.ApprovalRequired || rr.Spec.IncidentRef.UID != remediationv1alpha1.UID(got.UID) ||
		rr.Spec.TargetRef != inc.Spec.Subject || rr.Spec.PolicyRef != memoryPolicy ||
		rr.Spec.Action.Type != remediationv1alpha1.ActionSetContainerMemoryLimit {
		t.Errorf("request spec = %+v", rr.Spec)
	}
	// The executor recomputes the name from the spec the API server stored, defaults filled in, to show it in a
	// pull request body (#433): it must be the name the analyzer gave.
	if name, err := remediationv1alpha1.RequestName(rr.Spec); err != nil || string(name) != rr.Name {
		t.Errorf("RequestName of the stored spec = %q, %v, want %q", name, err, rr.Name)
	}
	cm := &corev1.ConfigMap{}
	if err := env.admin.Get(t.Context(), types.NamespacedName{Namespace: ns, Name: incidents.BudgetName}, cm); err != nil {
		t.Fatal(err)
	}
	if cm.Data[incidents.BudgetUsedKey] != "1" {
		t.Errorf("budget used %q, want 1", cm.Data[incidents.BudgetUsedKey])
	}

	checkRequestReuse(t, c, ns, got, rr)

	// The Incident is no longer due: a second reconcile does nothing.
	reconcile(t, c, inc)
	if len(rc.updates) != 2 || f.triage.calls() != 1 {
		t.Errorf("a second reconcile wrote %d updates and made %d calls", len(rc.updates)-2, f.triage.calls()-1)
	}
}

// checkRequestReuse checks that a later round that proposes the same plan finds the request instead of adding
// another, and that one that finds a request of that name with another plan refuses it.
func checkRequestReuse(t *testing.T, c *Controller, ns string, got *remediationv1alpha1.Incident,
	rr *remediationv1alpha1.RemediationRequest) {
	t.Helper()
	again := got.DeepCopy()
	again.Status.LastAnalysisTime = &metav1.Time{Time: time.Now().Add(time.Hour)}
	plan := Plan{Action: rr.Spec.Action, Checks: rr.Spec.Verification.Checks, PolicyRef: rr.Spec.PolicyRef}
	if name, err := c.createRequest(t.Context(), again, plan); err != nil || name != got.Status.RemediationRequests[0] {
		t.Errorf("the same plan again: %q, %v", name, err)
	}
	bigger := plan
	bigger.Action = *plan.Action.DeepCopy()
	bigger.Action.SetContainerMemoryLimit.Desired = resource.MustParse("768Mi")
	squatName, err := requestName(again, requestSpec(again, bigger))
	if err != nil {
		t.Fatal(err)
	}
	squat := rr.DeepCopy()
	squat.ObjectMeta = metav1.ObjectMeta{Namespace: ns, Name: string(squatName)}
	if err := env.admin.Create(t.Context(), squat); err != nil {
		t.Fatal(err)
	}
	if _, err := c.createRequest(t.Context(), again, bigger); err == nil {
		t.Error("a request of the plan's name with another plan was accepted")
	}
	list := &remediationv1alpha1.RemediationRequestList{}
	if err := env.admin.List(t.Context(), list, client.InNamespace(ns)); err != nil || len(list.Items) != 2 {
		t.Errorf("%d requests, want the proposal and the one made here (%v)", len(list.Items), err)
	}
}

func TestControllerStopsWhenTheBudgetIsSpent(t *testing.T) {
	ns := namespace(t)
	setSettings(t, 1)
	today := time.Now().UTC().Format("2006-01-02")
	inc := dueIncident(t, ns, map[string]string{incidents.BudgetDayKey: today, incidents.BudgetUsedKey: "1"})
	f := newFixture()
	f.triage.steps = []step{reply(validResult)}
	c, _ := newController(t, ns, f)
	reconcile(t, c, inc)

	got := &remediationv1alpha1.Incident{}
	if err := env.admin.Get(t.Context(), client.ObjectKeyFromObject(inc), got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != remediationv1alpha1.IncidentEscalated || f.triage.calls() != 0 || len(got.Status.RemediationRequests) != 0 {
		t.Errorf("phase %s after %d model calls, requests %v", got.Status.Phase, f.triage.calls(), got.Status.RemediationRequests)
	}
	if cond := meta.FindStatusCondition(got.Status.Conditions, ConditionAnalyzed); cond == nil || cond.Reason != string(ReasonBudgetSpent) {
		t.Errorf("Analyzed condition %+v", cond)
	}
	list := &remediationv1alpha1.RemediationRequestList{}
	if err := env.admin.List(t.Context(), list, client.InNamespace(ns)); err != nil || len(list.Items) != 0 {
		t.Errorf("%d requests (%v)", len(list.Items), err)
	}
}

func TestControllerLeavesIncidentsNotDue(t *testing.T) {
	ns := namespace(t)
	setSettings(t, 10)
	inc := dueIncident(t, ns, nil)
	meta.SetStatusCondition(&inc.Status.Conditions, metav1.Condition{Type: incidents.ConditionAnalysisDue,
		Status: metav1.ConditionFalse, Reason: incidents.ReasonCoolingDown, Message: "x"})
	if err := env.admin.Status().Update(t.Context(), inc); err != nil {
		t.Fatal(err)
	}
	f := newFixture()
	c, rc := newController(t, ns, f)
	reconcile(t, c, inc)
	if len(rc.updates) != 0 || f.triage.calls() != 0 || f.reserved != 0 {
		t.Errorf("an Incident not due got %d updates, %d calls, %d reservations", len(rc.updates), f.triage.calls(), f.reserved)
	}
}

// A Resolved Incident is never claimed, even when marked due: reopening it (Resolved to Open) is a later
// part of #251.
func TestControllerLeavesResolvedIncidents(t *testing.T) {
	ns := namespace(t)
	setSettings(t, 10)
	inc := dueIncident(t, ns, nil)
	inc.Status.Phase = remediationv1alpha1.IncidentResolved
	if err := env.admin.Status().Update(t.Context(), inc); err != nil {
		t.Fatal(err)
	}
	f := newFixture()
	c, rc := newController(t, ns, f)
	reconcile(t, c, inc)
	if len(rc.updates) != 0 || f.triage.calls() != 0 || f.reserved != 0 {
		t.Errorf("a Resolved Incident got %d updates, %d calls, %d reservations", len(rc.updates), f.triage.calls(), f.reserved)
	}
}

func TestControllerHandsOffWithoutProposal(t *testing.T) {
	ns := namespace(t)
	setSettings(t, 10)
	inc := dueIncident(t, ns, nil)
	f := newFixture()
	f.triage.steps = []step{reply(noProposal)}
	c, rc := newController(t, ns, f)
	reconcile(t, c, inc)
	if len(rc.updates) != 2 || rc.updates[1].Phase != remediationv1alpha1.IncidentEscalated {
		t.Fatalf("updates %+v", rc.updates)
	}
	if cond := meta.FindStatusCondition(rc.updates[1].Conditions, ConditionAnalyzed); cond == nil || cond.Reason != string(ReasonNoProposal) {
		t.Errorf("Analyzed condition %+v", cond)
	}
}

// logged returns a context whose logger records every line, and a function that returns the last line with
// the message msg, or "" when none was logged.
func logged(t *testing.T) (context.Context, func(msg string) string) {
	var mu sync.Mutex
	var lines []string
	ctx := logr.NewContext(t.Context(), funcr.New(func(_, args string) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, args)
	}, funcr.Options{}))
	return ctx, func(msg string) string {
		mu.Lock()
		defer mu.Unlock()
		for _, l := range slices.Backward(lines) {
			if strings.Contains(l, `"msg"="`+msg+`"`) {
				return l
			}
		}
		return ""
	}
}

// The "Analyzed Incident" line names what a MissingData hand-off asked for, and leaves the field out when the
// result listed nothing (#422).
func TestControllerLogsMissingData(t *testing.T) {
	for name, tc := range map[string]struct {
		reply, want string
		missing     bool
	}{
		"missing data": {reply: strings.Replace(noProposal, `"missingData":[]`,
			`"missingData":["previous container logs","node memory pressure"]`, 1),
			want: `"missingData"=["previous container logs" "node memory pressure"]`, missing: true},
		"none": {reply: noProposal},
	} {
		t.Run(name, func(t *testing.T) {
			ns := namespace(t)
			setSettings(t, 10)
			inc := dueIncident(t, ns, nil)
			f := newFixture()
			f.triage.steps = []step{reply(tc.reply)}
			c, _ := newController(t, ns, f)
			ctx, last := logged(t)
			if _, err := c.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(inc)}); err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			line := last("Analyzed Incident")
			if line == "" {
				t.Fatal("no Analyzed Incident line was logged")
			}
			if !strings.Contains(line, `"summary"="unclear"`) {
				t.Errorf("line %s: want the summary", line)
			}
			if tc.missing && !strings.Contains(line, tc.want) {
				t.Errorf("line %s: want %s", line, tc.want)
			}
			if !tc.missing && strings.Contains(line, "missingData") {
				t.Errorf("line %s: want no missingData field", line)
			}
		})
	}
}

// A stop during the analysis leaves the Incident Analyzing for the trigger's timeout and writes nothing more.
func TestControllerStopLeavesTheClaim(t *testing.T) {
	ns := namespace(t)
	setSettings(t, 10)
	inc := dueIncident(t, ns, nil)
	f := newFixture()
	f.triage.steps = []step{{block: true}}
	c, rc := newController(t, ns, f)
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		for f.triage.calls() == 0 {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()
	if _, err := c.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(inc)}); err == nil {
		t.Error("a stopped analysis returned no error")
	}
	if len(rc.updates) != 1 || rc.updates[0].Phase != remediationv1alpha1.IncidentAnalyzing {
		t.Errorf("updates %+v, want the claim only", rc.updates)
	}
}
