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

package eval

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"sync"
	"testing"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/analysis"
	"github.com/pkalin/shadewright/internal/providers"
)

// roleSpy replays a scenario's recording and records, per call, the role the loop asked the factory for and
// the request it sent.
type roleSpy struct {
	replaySource
	mu    sync.Mutex
	roles []analysis.Role
	reqs  []providers.CompletionRequest
}

type roleSpyModel struct {
	inner providers.ModelProvider
	role  analysis.Role
	spy   *roleSpy
}

func (m roleSpyModel) Complete(ctx context.Context, req providers.CompletionRequest) (providers.Completion, error) {
	m.spy.mu.Lock()
	m.spy.roles = append(m.spy.roles, m.role)
	m.spy.reqs = append(m.spy.reqs, req)
	m.spy.mu.Unlock()
	return m.inner.Complete(ctx, req)
}

func (s *roleSpy) factory(sc *scenario) (analysis.ModelFactory, func() error, error) {
	f, done, err := s.replaySource.factory(sc)
	if err != nil {
		return nil, nil, err
	}
	return func(r analysis.Role, d remediationv1alpha1.ModelDeployment) (providers.ModelProvider, error) {
		p, err := f(r, d)
		return roleSpyModel{inner: p, role: r, spy: s}, err
	}, done, nil
}

// userMessages are the non-system messages of a request: what the model is shown of the incident.
func userMessages(req providers.CompletionRequest) []providers.Message {
	var out []providers.Message
	for _, m := range req.Messages {
		if m.Role != providers.RoleSystem {
			out = append(out, m)
		}
	}
	return out
}

// With escalation forced, every scenario goes to the escalation model with the read tools from its first call,
// is reported as escalated, and the model is shown the same context as without it; without it, the
// replayed scenarios stay on triage.
func TestForcedEscalationSendsEveryScenarioToEscalation(t *testing.T) {
	scheme, err := newScheme()
	if err != nil {
		t.Fatal(err)
	}
	scenarios, err := loadScenarios(scheme)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) {
			plain := &roleSpy{}
			pres, err := runScenario(t.Context(), scheme, s, plain)
			if err != nil {
				t.Fatal(err)
			}
			forced := &roleSpy{replaySource: replaySource{force: true}}
			res, err := runScenario(t.Context(), scheme, s, forced)
			if err != nil {
				t.Fatal(err)
			}
			if pres.Escalated || len(plain.roles) == 0 || plain.roles[0] != analysis.RoleTriage {
				t.Fatalf("without forcing, the scenario escalated or called no triage: %v, roles %v", pres.Escalated, plain.roles)
			}
			if !res.Escalated || len(forced.roles) == 0 {
				t.Fatalf("forced: escalated %v, calls %d", res.Escalated, len(forced.roles))
			}
			for i, r := range forced.roles {
				if r != analysis.RoleEscalation {
					t.Errorf("forced call %d went to %s", i, r)
				}
			}
			if len(forced.reqs[0].Tools) == 0 {
				t.Error("the forced escalation offered no read tools")
			}
			if got, want := userMessages(forced.reqs[0]), userMessages(plain.reqs[0]); len(got) != len(want) ||
				got[0].Content != want[0].Content {
				t.Error("forcing the escalation changed the context the model is shown")
			}
			esc, ok := res.Tokens.ByRole[string(analysis.RoleEscalation)]
			if _, tri := res.Tokens.ByRole[string(analysis.RoleTriage)]; !ok || tri {
				t.Errorf("tokens by role %+v", res.Tokens.ByRole)
			}
			if esc.Input != res.Tokens.Input || esc.Output != res.Tokens.Output {
				t.Errorf("escalation tokens %+v, total %+v", esc, res.Tokens)
			}
		})
	}
}

// The forced signal is the builder's own failed-source signal: the loop escalates on it and hands off when
// no escalation model is configured, as on a real outage.
func TestForcedEscalationWithoutEscalationModelHandsOff(t *testing.T) {
	scheme, err := newScheme()
	if err != nil {
		t.Fatal(err)
	}
	_, s := mustScenario(t, scnOOM)
	res, err := runScenario(t.Context(), scheme, s, noEscalation{replaySource{force: true}})
	if err != nil || res.Reason != string(analysis.ReasonEscalationNotConfigured) || res.Calls != 0 || res.Escalated {
		t.Fatalf("result %+v, error %v", res, err)
	}
}

// noEscalation is a source with no escalation deployment. It does not check that the recording was used.
type noEscalation struct{ replaySource }

func (n noEscalation) factory(sc *scenario) (analysis.ModelFactory, func() error, error) {
	f, _, err := n.replaySource.factory(sc)
	return f, nil, err
}

func (n noEscalation) deployments() (*remediationv1alpha1.ModelDeployment, *remediationv1alpha1.ModelDeployment) {
	tr, _ := n.replaySource.deployments()
	return tr, nil
}

func TestEnvForceEscalation(t *testing.T) {
	cases := []struct {
		value      string
		configured bool
		want       bool
		err        bool
	}{
		{"", false, false, false},
		{"", true, false, false},
		{"1", true, true, false},
		{"1", false, false, true},
		{"true", true, false, true},
		{"0", true, false, true},
	}
	for _, c := range cases {
		t.Setenv(envForceEsc, c.value)
		got, err := envForceEscalation(c.configured)
		if got != c.want || (err != nil) != c.err {
			t.Errorf("%q, configured %v: %v, %v", c.value, c.configured, got, err)
		}
	}
}

// The manifest names a forced run, so its digest differs from a run of the deployment as it is configured.
func TestManifestNamesForcedEscalation(t *testing.T) {
	src, err := loadSource()
	if err != nil {
		t.Fatal(err)
	}
	plain, err := buildManifest(src, replaySource{}, nil, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	forced, err := buildManifest(src, replaySource{force: true}, nil, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if plain.ForcedEscalation || !forced.ForcedEscalation || plain.Digest == forced.Digest {
		t.Errorf("plain %v %s, forced %v %s", plain.ForcedEscalation, plain.Digest, forced.ForcedEscalation, forced.Digest)
	}
}

// Tokens and cost are reported per role, the escalation at its own prices when it has them.
func TestReportCostByRole(t *testing.T) {
	tri := roleTokens{Input: 1000, CachedInput: 400, Output: 100}
	esc := roleTokens{Input: 2000, Output: 300, UsageUnknownCalls: 1}
	var a, b tokens
	a.addRole(string(analysis.RoleTriage), tri)
	b.addRole(string(analysis.RoleEscalation), esc)
	r := &report{Mode: "foundry", Manifest: manifest{MinPassRate: 1, Repeats: 1, ForcedEscalation: true},
		Scenarios: []scenarioResult{{Name: "a", Pass: true, Tokens: a}, {Name: "b", Pass: true, Escalated: true, Tokens: b}}}
	p := &prices{Input: 2, CachedInput: 1, Output: 8, Escalation: &prices{Input: 10, CachedInput: 5, Output: 40}}
	r.summarise(p)
	byRole := r.Totals.Tokens.ByRole
	if byRole[string(analysis.RoleEscalation)] != esc || byRole[string(analysis.RoleTriage)] != tri {
		t.Errorf("tokens by role %+v", r.Totals.Tokens.ByRole)
	}
	// Triage (600*2 + 400*1 + 100*8) / 1e6; escalation (2000*10 + 300*40) / 1e6.
	wantTri, wantEsc := 0.0024, 0.032
	near := func(x, y float64) bool { return math.Abs(x-y) < 1e-12 }
	if !near(r.Totals.CostUSDByRole[string(analysis.RoleTriage)], wantTri) ||
		!near(r.Totals.CostUSDByRole[string(analysis.RoleEscalation)], wantEsc) ||
		r.Totals.CostUSD == nil || !near(*r.Totals.CostUSD, wantTri+wantEsc) {
		t.Errorf("cost %v, by role %v", r.Totals.CostUSD, r.Totals.CostUSDByRole)
	}
	md := r.markdown()
	for _, want := range []string{"Escalation forced", "  - escalation: input 2000", "estimated cost USD 0.0320",
		"| Escalated |", "| yes |"} {
		if !strings.Contains(md, want) {
			t.Errorf("the markdown lacks %q:\n%s", want, md)
		}
	}
	// Without its own prices, the escalation is priced as the triage.
	p.Escalation = nil
	r.summarise(p)
	if !near(*r.Totals.CostUSD, wantTri+0.0064) {
		t.Errorf("cost without escalation prices %v", *r.Totals.CostUSD)
	}
}

// failingModel fails every call as a timeout would: no usage.
type failingModel struct{}

func (failingModel) Complete(context.Context, providers.CompletionRequest) (providers.Completion, error) {
	return providers.Completion{}, &providers.RetryableError{Err: errors.New("timeout")}
}

// malformedModel returns a billed reply that is malformed: its usage is known.
type malformedModel struct{}

func (malformedModel) Complete(context.Context, providers.CompletionRequest) (providers.Completion, error) {
	return providers.Completion{Usage: providers.Usage{InputTokens: 100, OutputTokens: 10}},
		fmt.Errorf("bad reply: %w", providers.ErrMalformedOutput)
}

// A failed call has no usage to price: it counts as unknown usage, as the loop charges it at its bound, and
// the cost is marked incomplete, so a candidate that errors never looks cheaper.
func TestFailedCallsMakeTheCostIncomplete(t *testing.T) {
	tl := &tally{}
	_, _ = recorded{inner: failingModel{}, role: analysis.RoleEscalation, t: tl}.Complete(t.Context(),
		providers.CompletionRequest{})
	_, _ = recorded{inner: malformedModel{}, role: analysis.RoleEscalation, t: tl}.Complete(t.Context(),
		providers.CompletionRequest{})
	u := tl.usage[analysis.RoleEscalation]
	if u == nil || u.usageUnknown != 1 || u.usage.InputTokens != 100 {
		t.Fatalf("usage %+v", u)
	}
	var tk tokens
	tk.addRole(string(analysis.RoleEscalation), roleTokens{UsageUnknownCalls: u.usageUnknown})
	r := &report{Manifest: manifest{MinPassRate: 1, Repeats: 1}, Scenarios: []scenarioResult{{Name: "a", Tokens: tk}}}
	r.summarise(&prices{Input: 1, CachedInput: 1, Output: 1})
	if !r.Totals.CostIncomplete || !strings.Contains(r.markdown(), "incomplete, a lower bound: 1 call(s)") {
		t.Errorf("totals %+v:\n%s", r.Totals, r.markdown())
	}
	r.Scenarios[0].Tokens = tokens{Input: 10}
	r.summarise(&prices{Input: 1, CachedInput: 1, Output: 1})
	if r.Totals.CostIncomplete || strings.Contains(r.markdown(), "incomplete") {
		t.Errorf("a complete run is marked incomplete: %+v", r.Totals)
	}
}

func TestEnvPrices(t *testing.T) {
	tri := []string{envPriceIn, envPriceHit, envPriceOut}
	esc := []string{envEscPrices + "INPUT_PER_MTOK", envEscPrices + "CACHED_INPUT_PER_MTOK",
		envEscPrices + "OUTPUT_PER_MTOK"}
	set := func(names []string, v string) {
		for _, n := range names {
			t.Setenv(n, v)
		}
	}
	cases := []struct {
		name            string
		tri, esc        string
		configured, err bool
		want            *prices
	}{
		{name: "none", configured: true},
		{name: "triage only", tri: "1", want: &prices{Input: 1, CachedInput: 1, Output: 1}},
		{name: "both", tri: "1", esc: "2", configured: true, want: &prices{Input: 1, CachedInput: 1, Output: 1,
			Escalation: &prices{Input: 2, CachedInput: 2, Output: 2}}},
		{name: "escalation without deployment", tri: "1", esc: "2", err: true},
		{name: "escalation without triage", esc: "2", configured: true, err: true},
		{name: "escalation not a number", tri: "1", esc: "x", configured: true, err: true},
	}
	for _, c := range cases {
		set(tri, c.tri)
		set(esc, c.esc)
		got, err := envPrices(c.configured)
		if (err != nil) != c.err || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %+v, %v", c.name, got, err)
		}
	}
	set(esc, "")
	t.Setenv(esc[0], "2")
	if _, err := envPrices(true); err == nil {
		t.Error("a partial set of escalation prices was accepted")
	}
}

// A wrong EVAL_FORCE_ESCALATION fails before the identity is built.
func TestForceEscalationCheckedBeforeIdentity(t *testing.T) {
	for _, n := range []string{"AZURE_CLIENT_ID", "AZURE_TENANT_ID", "AZURE_FEDERATED_TOKEN_FILE", envEscDep} {
		t.Setenv(n, "")
	}
	t.Setenv(envForceEsc, "1")
	if _, err := newFoundrySource(); err == nil || !strings.Contains(err.Error(), envForceEsc) {
		t.Errorf("error %v", err)
	}
}

// The replay refuses EVAL_FORCE_ESCALATION instead of ignoring it.
func TestReplayRefusesForcedEscalation(t *testing.T) {
	t.Setenv(envForceEsc, "")
	if err := replayEnvError(); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envForceEsc, "1")
	if err := replayEnvError(); err == nil {
		t.Error("the replay accepted a forced escalation")
	}
}
