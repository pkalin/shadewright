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

package openaicompatible

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/analysis"
	"github.com/pkalin/shadewright/internal/providers"
)

// These tests check decision 7 of #834: every attempt the endpoint may have billed is charged, and the
// provider makes no attempt the remaining budget cannot cover at the call's ceiling.

// failingBody is a 200 body whose read fails: the endpoint answered, so billed, but the usage cannot be read.
type failingBody struct{}

func (failingBody) Read([]byte) (int, error) { return 0, errors.New("test: connection reset") }
func (failingBody) Close() error             { return nil }

// unreadable200 answers every request with a 200 whose body fails on read, and counts the requests.
func unreadable200(n *atomic.Int32) roundTrip {
	return func(*http.Request) (*http.Response, error) {
		n.Add(1)
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
			Body: failingBody{}, ContentLength: -1}, nil
	}
}

func noSleep(context.Context, time.Duration) error { return nil }

// analyzeWith runs one analysis whose escalation model is p, with the given daily token cap.
func analyzeWith(t *testing.T, p *Provider, dayCap int64) analysis.Outcome {
	t.Helper()
	a, err := analysis.New(analysis.Config{
		Builder: loopContext{},
		Tools:   &loopTools{},
		Models: func(analysis.Role, remediationv1alpha1.ModelDeployment) (providers.ModelProvider, error) {
			return p, nil
		},
		Reserve: func(context.Context, time.Time) error { return nil },
		ExpectedModels: map[analysis.Role]string{analysis.RoleTriage: replyModel,
			analysis.RoleEscalation: replyModel},
	})
	if err != nil {
		t.Fatal(err)
	}
	d := &remediationv1alpha1.ModelDeployment{Endpoint: testEndpoint, Deployment: testDeployment, APIVersion: "v1",
		MaxTokensPerDay: new(dayCap)}
	inc := &remediationv1alpha1.Incident{Spec: remediationv1alpha1.IncidentSpec{Subject: remediationv1alpha1.ObjectRef{
		APIVersion: "apps/v1", Kind: "Deployment", Namespace: "shop", Name: "web", UID: "uid-1"}}}
	out, err := a.Analyze(t.Context(), inc, analysis.Limits{MaxToolSteps: 4, MaxOutputTokens: 1024,
		Triage: d, Escalation: d})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	return out
}

// ceilingOfOneAttempt is what the loop charges one unreadable 200: the ceiling C of the call.
func ceilingOfOneAttempt(t *testing.T) int64 {
	t.Helper()
	var n atomic.Int32
	p, err := newProvider(Config{Endpoint: endpoint, Deployment: testDeployment, MaxAttempts: 1},
		testHooks{transport: unreadable200(&n), sleep: noSleep})
	if err != nil {
		t.Fatal(err)
	}
	out := analyzeWith(t, p, 10_000_000)
	if n.Load() != 1 || out.Tokens <= 1024 {
		t.Fatalf("one attempt: %d requests, charged %d", n.Load(), out.Tokens)
	}
	return out.Tokens
}

// A 200 whose body fails on read is retried; each of the three requests was sent and may have been billed,
// so the analysis is charged the ceiling three times, not once.
func TestLoopChargesEveryBilledAttempt(t *testing.T) {
	c := ceilingOfOneAttempt(t)
	var n atomic.Int32
	p, err := newProvider(Config{Endpoint: endpoint, Deployment: testDeployment},
		testHooks{transport: unreadable200(&n), sleep: noSleep})
	if err != nil {
		t.Fatal(err)
	}
	out := analyzeWith(t, p, 10_000_000)
	if n.Load() != DefaultMaxAttempts {
		t.Fatalf("%d requests, want %d", n.Load(), DefaultMaxAttempts)
	}
	if out.Tokens != int64(DefaultMaxAttempts)*c {
		t.Fatalf("charged %d tokens for %d billed attempts, want %d", out.Tokens, n.Load(), int64(DefaultMaxAttempts)*c)
	}
}

// When the day's remaining budget covers one attempt at the ceiling but not two, the provider does not retry:
// one request, one charge, and the day's cap is never passed.
func TestNoRetryTheBudgetCannotCover(t *testing.T) {
	c := ceilingOfOneAttempt(t)
	for _, tc := range []struct {
		cap  int64
		want int32
	}{{2*c - 1, 1}, {2 * c, 2}, {3 * c, 3}} {
		var n atomic.Int32
		p, err := newProvider(Config{Endpoint: endpoint, Deployment: testDeployment},
			testHooks{transport: unreadable200(&n), sleep: noSleep})
		if err != nil {
			t.Fatal(err)
		}
		out := analyzeWith(t, p, tc.cap)
		if n.Load() != tc.want || out.Tokens != int64(tc.want)*c || out.Tokens > tc.cap {
			t.Errorf("cap %d: %d requests charged %d, want %d requests charged %d", tc.cap, n.Load(), out.Tokens,
				tc.want, int64(tc.want)*c)
		}
	}
}

// A dial the guard refuses sends nothing: the call is charged nothing.
func TestLoopChargesNothingForADialRefusal(t *testing.T) {
	p, err := newProvider(Config{Endpoint: "https://gateway.example.net/v1", Deployment: testDeployment},
		testHooks{sleep: noSleep, dialer: func(d *net.Dialer) {
			d.Resolver = fakeResolver(t, netip.MustParseAddr("10.2.3.4"))
		}})
	if err != nil {
		t.Fatal(err)
	}
	out := analyzeWith(t, p, 10_000_000)
	if out.Tokens != 0 {
		t.Fatalf("a refused dial was charged %d tokens", out.Tokens)
	}
}

// The provider reports each attempt that may have been billed: a non-200 that was sent counts (unknown
// usage), the 200 that answered counts with its reported usage.
func TestCompletionListsBilledAttempts(t *testing.T) {
	calls := 0
	p, err := newProvider(Config{Endpoint: endpoint, Deployment: testModel},
		testHooks{sleep: noSleep, transport: roundTrip(func(*http.Request) (*http.Response, error) {
			calls++
			if calls == 1 {
				return response(503, "application/json", ""), nil
			}
			return response(200, "application/json", chatOK), nil
		})})
	if err != nil {
		t.Fatal(err)
	}
	c, err := p.Complete(t.Context(), request())
	if err != nil {
		t.Fatal(err)
	}
	if !c.AttemptsReported || len(c.BilledAttempts) != 2 || !c.BilledAttempts[0].UsageUnknown ||
		c.BilledAttempts[1].UsageUnknown || c.BilledAttempts[1].Usage != c.Usage {
		t.Fatalf("billed attempts %+v, usage %+v", c.BilledAttempts, c.Usage)
	}
	// A request that allows one billed attempt is not retried.
	calls = 0
	req := request()
	req.MaxBilledAttempts = 1
	if _, err := p.Complete(t.Context(), req); err == nil || calls != 1 {
		t.Fatalf("with one billed attempt allowed: %d requests, err %v", calls, err)
	}
}
