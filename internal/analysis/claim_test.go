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
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/providers"
)

// hookModel runs before ahead of each call of the model it wraps.
type hookModel struct {
	inner  providers.ModelProvider
	before func(ctx context.Context)
}

func (m hookModel) Complete(ctx context.Context, req providers.CompletionRequest) (providers.Completion, error) {
	m.before(ctx)
	return m.inner.Complete(ctx, req)
}

// withHook makes every model of f run before ahead of each call.
func withHook(f *fixture, before func(ctx context.Context)) {
	models := f.cfg.Models
	f.cfg.Models = func(role Role, d remediationv1alpha1.ModelDeployment) (providers.ModelProvider, error) {
		m, err := models(role, d)
		if err != nil {
			return nil, err
		}
		return hookModel{inner: m, before: before}, nil
	}
}

func requestsIn(t *testing.T, ns string) []remediationv1alpha1.RemediationRequest {
	t.Helper()
	list := &remediationv1alpha1.RemediationRequestList{}
	if err := env.admin.List(t.Context(), list, client.InNamespace(ns)); err != nil {
		t.Fatal(err)
	}
	return list.Items
}

func liveIncident(t *testing.T, inc *remediationv1alpha1.Incident) *remediationv1alpha1.Incident {
	t.Helper()
	got := &remediationv1alpha1.Incident{}
	if err := env.admin.Get(t.Context(), client.ObjectKeyFromObject(inc), got); err != nil {
		t.Fatal(err)
	}
	return got
}

// ADR-0025 decision 1: a run whose claim another run took over while it waited for the model publishes
// nothing: no request and no outcome write over the successor's claim.
func TestControllerDropsTheResultOfALostClaim(t *testing.T) {
	for name, result := range map[string]string{"proposal": validResult, "hand-off": noProposal} {
		t.Run(name, func(t *testing.T) {
			ns := namespace(t)
			setSettings(t, 10)
			inc := dueIncident(t, ns, nil)
			f := newFixture()
			f.triage.steps = []step{reply(result)}
			successor := metav1.NewTime(time.Now().Add(time.Minute).Truncate(time.Second))
			withHook(f, func(ctx context.Context) {
				live := liveIncident(t, inc)
				live.Status.LastAnalysisTime = &successor
				if err := env.admin.Status().Update(ctx, live); err != nil {
					t.Error(err)
				}
			})
			c, rc := newController(t, ns, f)
			ctx, last := logged(t)
			if _, err := c.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(inc)}); err != nil {
				t.Fatalf("Reconcile: %v", err)
			}

			if n := len(requestsIn(t, ns)); n != 0 {
				t.Errorf("%d requests created after the claim was taken over", n)
			}
			// The dropped result still logs its own words (#422); a hand-off logged them before the drop.
			if line := last("Dropped the result of an analysis whose claim was lost"); result == validResult &&
				!strings.Contains(line, `"summary"="The container runs out of memory."`) {
				t.Errorf("line %q: want the summary of the dropped result", line)
			}
			if len(rc.updates) != 1 {
				t.Errorf("%d status updates, want the claim only", len(rc.updates))
			}
			got := liveIncident(t, inc)
			if got.Status.Phase != remediationv1alpha1.IncidentAnalyzing || !got.Status.LastAnalysisTime.Equal(&successor) {
				t.Errorf("the successor's claim was overwritten: %+v", got.Status)
			}
		})
	}
}

// ADR-0025 decision 1: once the claim expired, neither the create nor the outcome write starts.
func TestControllerDropsTheResultOfAnExpiredClaim(t *testing.T) {
	for name, result := range map[string]string{"proposal": validResult, "hand-off": noProposal} {
		t.Run(name, func(t *testing.T) {
			ns := namespace(t)
			setSettings(t, 10)
			inc := dueIncident(t, ns, nil)
			f := newFixture()
			f.triage.steps = []step{reply(result)}
			var offset atomic.Int64
			withHook(f, func(context.Context) { offset.Store(int64(11 * time.Minute)) })
			c, rc := newController(t, ns, f)
			c.Now = func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }
			reconcile(t, c, inc)

			if n := len(requestsIn(t, ns)); n != 0 {
				t.Errorf("%d requests created after the claim expired", n)
			}
			if len(rc.updates) != 1 {
				t.Errorf("%d status updates, want the claim only", len(rc.updates))
			}
		})
	}
}

// previousClaim gives inc the lastAnalysisTime of an earlier run that began ago before now.
func previousClaim(t *testing.T, inc *remediationv1alpha1.Incident, ago time.Duration) {
	t.Helper()
	live := liveIncident(t, inc)
	at := metav1.NewTime(time.Now().Add(-ago).Truncate(time.Second))
	live.Status.LastAnalysisTime = &at
	if err := env.admin.Status().Update(t.Context(), live); err != nil {
		t.Fatal(err)
	}
	*inc = *live
}

// analysisRequest creates a labelled request of inc with the form an analysis gives it: the plan of
// validResult with checks and policy, named by requestName unless name is set.
func analysisRequest(t *testing.T, inc *remediationv1alpha1.Incident, checks []remediationv1alpha1.VerificationCheck,
	policyRef remediationv1alpha1.Name, name string, incidentName string) *remediationv1alpha1.RemediationRequest {
	t.Helper()
	_, action, err := DecodeResult(validResult)
	if err != nil {
		t.Fatal(err)
	}
	spec := requestSpec(inc, Plan{Action: *action, Checks: checks, PolicyRef: policyRef})
	if incidentName != "" {
		spec.IncidentRef.Name = incidentName
	}
	if name == "" {
		derived, err := requestName(inc, spec)
		if err != nil {
			t.Fatal(err)
		}
		name = string(derived)
	}
	rr := &remediationv1alpha1.RemediationRequest{
		ObjectMeta: metav1.ObjectMeta{Namespace: inc.Namespace, Name: name,
			Labels: map[string]string{LabelIncidentUID: string(inc.UID)}},
		Spec: spec,
	}
	if err := env.admin.Create(t.Context(), rr); err != nil {
		t.Fatal(err)
	}
	return rr
}

// ADR-0025 decision 6: requests carry the Incident's UID as a label, and a claim records at most one labelled
// request of the previous run that the status does not list yet, only with the form an analysis gives it: not
// one with another name, another policy or another Incident in its spec.
func TestControllerLabelsAndRecordsRequests(t *testing.T) {
	ns := namespace(t)
	setSettings(t, 10)
	inc := dueIncident(t, ns, nil)
	previousClaim(t, inc, time.Minute)
	oom := []remediationv1alpha1.VerificationCheck{remediationv1alpha1.CheckWorkloadHealthy}
	first := analysisRequest(t, inc, nil, memoryPolicy, "", "")
	second := analysisRequest(t, inc, oom, memoryPolicy, "", "")
	analysisRequest(t, inc, nil, memoryPolicy, "forged", "")
	analysisRequest(t, inc, nil, "another-policy", "", "")
	analysisRequest(t, inc, nil, memoryPolicy, "", "another-incident")
	f := newFixture()
	f.triage.steps = []step{reply(validResult)}
	c, rc := newController(t, ns, f)
	reconcile(t, c, inc)

	if len(rc.updates) != 2 || len(rc.updates[0].RemediationRequests) != 1 ||
		!slices.Contains([]string{first.Name, second.Name}, string(rc.updates[0].RemediationRequests[0])) {
		t.Fatalf("the claim recorded %v, want one of the previous run's requests", rc.updates[0].RemediationRequests)
	}
	got := liveIncident(t, inc)
	if len(got.Status.RemediationRequests) != 2 || got.Status.RemediationRequests[0] != rc.updates[0].RemediationRequests[0] {
		t.Fatalf("requests %v, want the recorded one and the proposal", got.Status.RemediationRequests)
	}
	rr := &remediationv1alpha1.RemediationRequest{}
	key := client.ObjectKey{Namespace: ns, Name: string(got.Status.RemediationRequests[1])}
	if err := env.admin.Get(t.Context(), key, rr); err != nil {
		t.Fatal(err)
	}
	if rr.Labels[LabelIncidentUID] != string(inc.UID) {
		t.Errorf("the request's labels %v do not name the Incident's UID", rr.Labels)
	}
}

// ADR-0025 decision 6: a labelled request created outside the claim window of the Incident's lastAnalysisTime,
// such as one the status dropped at its bound long ago, is not recorded again.
func TestControllerRecordsOnlyRequestsOfThePreviousClaim(t *testing.T) {
	ns := namespace(t)
	setSettings(t, 10)
	inc := dueIncident(t, ns, nil)
	previousClaim(t, inc, 20*time.Minute)
	analysisRequest(t, inc, nil, memoryPolicy, "", "")
	f := newFixture()
	f.triage.steps = []step{reply(noProposal)}
	c, rc := newController(t, ns, f)
	reconcile(t, c, inc)

	if len(rc.updates) == 0 || len(rc.updates[0].RemediationRequests) != 0 {
		t.Fatalf("the claim recorded %v, want none", rc.updates)
	}
	if got := liveIncident(t, inc); len(got.Status.RemediationRequests) != 0 {
		t.Errorf("requests %v, want none", got.Status.RemediationRequests)
	}
}

// flakyReader fails the first fails live reads of an Incident with a server timeout.
type flakyReader struct {
	client.Reader
	fails atomic.Int32
}

func (r *flakyReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*remediationv1alpha1.Incident); ok && r.fails.Add(-1) >= 0 {
		return apierrors.NewServerTimeout(remediationv1alpha1.GroupVersion.WithResource("incidents").GroupResource(), "get", 1)
	}
	return r.Reader.Get(ctx, key, obj, opts...)
}

// A transient error of the live read before the create is retried within the claim, not handed off.
func TestControllerRetriesATransientRecheckError(t *testing.T) {
	ns := namespace(t)
	setSettings(t, 10)
	inc := dueIncident(t, ns, nil)
	f := newFixture()
	f.triage.steps = []step{reply(validResult)}
	c, _ := newController(t, ns, f)
	r := &flakyReader{Reader: env.admin}
	r.fails.Store(2)
	c.Reader = r
	reconcile(t, c, inc)

	got := liveIncident(t, inc)
	if got.Status.Phase != remediationv1alpha1.IncidentProposed || len(requestsIn(t, ns)) != 1 {
		t.Errorf("phase %s, %d requests, want Proposed with one request", got.Status.Phase, len(requestsIn(t, ns)))
	}
}

// ADR-0025 decision 1: the analysis deadline, the create and the outcome write fit inside the claim.
func TestCheckTimeBudget(t *testing.T) {
	for _, tc := range []struct {
		timeout, claim time.Duration
		ok             bool
	}{
		{DefaultTimeout, 0, true},
		{MaxTimeout, 0, true},
		{MaxTimeout, 9*time.Minute + 50*time.Second, false},
		{DefaultTimeout, 8 * time.Minute, false},
	} {
		f := newFixture()
		f.cfg.Timeout = tc.timeout
		a, err := New(f.cfg)
		if err != nil {
			t.Fatal(err)
		}
		c := &Controller{Analyzer: a, Reader: env.admin, ClaimTimeout: tc.claim}
		if err := c.CheckTimeBudget(); (err == nil) != tc.ok {
			t.Errorf("deadline %s, claim %s: %v", tc.timeout, tc.claim, err)
		}
	}
	if err := (&Controller{}).CheckTimeBudget(); err == nil {
		t.Error("a controller without an analyzer passed")
	}
	a, err := New(newFixture().cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := (&Controller{Analyzer: a}).CheckTimeBudget(); err == nil {
		t.Error("a controller without a live reader passed")
	}
}

// The writes after the analysis get no more time than the claim has left.
func TestWritesStopAtTheClaimsExpiry(t *testing.T) {
	now := time.Now()
	c := &Controller{Now: func() time.Time { return now }}
	claimed := incident()
	start := metav1.NewTime(now.Add(-10*time.Minute + 5*time.Second))
	claimed.Status.LastAnalysisTime = &start
	ctx, cancel, err := c.withinClaim(t.Context(), claimed, finalWriteTimeout, true)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	if d, ok := ctx.Deadline(); !ok || time.Until(d) > 6*time.Second {
		t.Errorf("deadline %v, want at most the 5 s left on the claim", d)
	}
	late := metav1.NewTime(now.Add(-10 * time.Minute))
	claimed.Status.LastAnalysisTime = &late
	if _, _, err := c.withinClaim(t.Context(), claimed, finalWriteTimeout, true); err == nil {
		t.Error("a write started after the claim expired")
	}
}

// laggingClient creates a request on the API server and then returns createErr, as a create whose reply was
// lost does, and its Get of a RemediationRequest misses, as a cache that has not seen the request yet does.
type laggingClient struct {
	client.Client
	createErr error
}

func (c laggingClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if err := c.Client.Create(ctx, obj, opts...); err != nil {
		return err
	}
	return c.createErr
}

func (c laggingClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*remediationv1alpha1.RemediationRequest); ok {
		return apierrors.NewNotFound(requestsResource, key.Name)
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

var requestsResource = remediationv1alpha1.GroupVersion.WithResource("remediationrequests").GroupResource()

// A request that already exists is checked with a live read, not the cache, so a cache that has not seen it
// yet does not hand the proposal off.
func TestControllerReusesARequestTheCacheHasNotSeen(t *testing.T) {
	ns := namespace(t)
	setSettings(t, 10)
	inc := dueIncident(t, ns, nil)
	f := newFixture()
	f.triage.steps = []step{reply(validResult)}
	c, rc := newController(t, ns, f)
	c.Client = laggingClient{Client: rc, createErr: apierrors.NewAlreadyExists(requestsResource, "request")}
	reconcile(t, c, inc)

	got := liveIncident(t, inc)
	if got.Status.Phase != remediationv1alpha1.IncidentProposed || len(got.Status.RemediationRequests) != 1 {
		t.Errorf("phase %s, requests %v, want Proposed with the existing request", got.Status.Phase,
			got.Status.RemediationRequests)
	}
}

// A create that failed on the client side after the API server created the request still has the request
// recorded by the outcome write, so a later claim need not find it.
func TestControllerRecordsARequestWhoseCreateReplyWasLost(t *testing.T) {
	ns := namespace(t)
	setSettings(t, 10)
	inc := dueIncident(t, ns, nil)
	f := newFixture()
	f.triage.steps = []step{reply(validResult)}
	c, rc := newController(t, ns, f)
	c.Client = laggingClient{Client: rc, createErr: context.DeadlineExceeded}
	reconcile(t, c, inc)

	got := liveIncident(t, inc)
	created := requestsIn(t, ns)
	if len(created) != 1 || got.Status.Phase != remediationv1alpha1.IncidentEscalated ||
		!slices.Equal(got.Status.RemediationRequests, []remediationv1alpha1.Name{remediationv1alpha1.Name(created[0].Name)}) {
		t.Errorf("phase %s, requests %v, %d created, want Escalated with the created request recorded",
			got.Status.Phase, got.Status.RemediationRequests, len(created))
	}
}
