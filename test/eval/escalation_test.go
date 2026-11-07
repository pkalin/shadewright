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
	"math"
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
