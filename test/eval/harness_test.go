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
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/analysis"
	"github.com/pkalin/shadewright/internal/incidentcontext"
	"github.com/pkalin/shadewright/internal/policy"
	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/redact"
	"github.com/pkalin/shadewright/internal/tools"
)

// evalPolicies names a RemediationPolicy for every action type, so that an outcome is never decided by a
// missing policy name. Each scenario's policy.yaml holds the policies (#493): the lab's demo-memory, and a
// demo-keda the lab does not have yet.
var evalPolicies = map[remediationv1alpha1.ActionType]remediationv1alpha1.Name{
	remediationv1alpha1.ActionSetContainerMemoryLimit: "demo-memory",
	remediationv1alpha1.ActionAdjustKedaMaxReplicas:   "demo-keda",
}

// evalRequestNamespace is the operator namespace, where the analyzer creates requests: the policies'
// requestNamespaces must list it.
const evalRequestNamespace = "shadewright-system"

// evalSettings is the cluster's RemediationSettings as far as the policy check reads it: the KEDA module
// enabled, so that AdjustKedaMaxReplicas is not refused for it.
func evalSettings() *remediationv1alpha1.RemediationSettings {
	return &remediationv1alpha1.RemediationSettings{
		ObjectMeta: metav1.ObjectMeta{Name: remediationv1alpha1.SettingsName},
		Spec: remediationv1alpha1.RemediationSettingsSpec{Modules: &remediationv1alpha1.Modules{
			Integrations: []remediationv1alpha1.ModuleName{"keda"}}},
	}
}

// clusterObjects are what the scenario's cluster holds for the analyzer: its recorded objects, its policies
// and the settings.
func (s *scenario) clusterObjects() []client.Object {
	objs := slices.Clone(s.objects)
	for _, p := range s.policies {
		objs = append(objs, p.DeepCopy())
	}
	return append(objs, evalSettings())
}

// Limits of an evaluation run: the sample RemediationSettings' maxToolSteps and maxOutputTokens.
const (
	evalMaxToolSteps    = 4
	evalMaxOutputTokens = 2048
)

// tally counts what the model calls of one scenario run returned, per role.
type tally struct {
	mu    sync.Mutex
	usage map[analysis.Role]*roleUsage
	// last is the content of the last reply that had any, the one the loop's outcome rests on.
	last string
}

// roleUsage is what the calls of one role returned.
type roleUsage struct {
	usage        providers.Usage
	usageUnknown int
}

// recorded wraps the provider of one role and counts its replies in a tally. It only reads what the provider
// returned.
type recorded struct {
	inner providers.ModelProvider
	role  analysis.Role
	t     *tally
}

func (r recorded) Complete(ctx context.Context, req providers.CompletionRequest) (providers.Completion, error) {
	c, err := r.inner.Complete(ctx, req)
	r.t.mu.Lock()
	defer r.t.mu.Unlock()
	if r.t.usage == nil {
		r.t.usage = map[analysis.Role]*roleUsage{}
	}
	u := r.t.usage[r.role]
	if u == nil {
		u = &roleUsage{}
		r.t.usage[r.role] = u
	}
	u.usage.InputTokens += c.Usage.InputTokens
	u.usage.CachedInputTokens += c.Usage.CachedInputTokens
	u.usage.OutputTokens += c.Usage.OutputTokens
	// As the loop charges it (run.attempt): a call that failed, other than with a malformed reply, or whose
	// usage is unknown, has no usage the cost can rest on; the loop charges its upper bound.
	if (err != nil && !providers.IsMalformedOutput(err)) || c.UsageUnknown {
		u.usageUnknown++
	}
	if err == nil && c.Content != "" {
		r.t.last = c.Content
	}
	return c, err
}

// forcedEscalationSource is the section a forced escalation marks as failed: no real section, so no block
// of the context changes and the model sees exactly the context the builder built.
const forcedEscalationSource incidentcontext.Section = "evaluation: forced escalation"

// forcedEscalation wraps the context builder of an evaluation run with EVAL_FORCE_ESCALATION set: it marks
// every context it returns with a failed source, the builder's own escalation signal (design 3.3), so the loop
// calls the escalation model with the read tools exactly as on a real outage. It lives only in this test
// package: the analyzer has no flag, setting or code path that reaches it, and the model's output never sets
// it.
type forcedEscalation struct {
	inner analysis.ContextBuilder
}

func (f forcedEscalation) Build(ctx context.Context,
	inc *remediationv1alpha1.Incident) (*incidentcontext.Context, error) {
	c, err := f.inner.Build(ctx, inc)
	if err != nil || c == nil {
		return c, err
	}
	if !slices.Contains(c.FailedSources, forcedEscalationSource) {
		c.FailedSources = append(c.FailedSources, forcedEscalationSource)
	}
	return c, nil
}

// modelSource gives the model factory of one scenario run and the models the replies must name.
type modelSource interface {
	// factory returns the factory for a run of s; each call is a new run. done, when not nil, is called after
	// the analysis and returns an error when the run did not use the source as recorded (a reply left over).
	factory(s *scenario) (f analysis.ModelFactory, done func() error, err error)
	// expectedModels names the model of each configured role (Config.ExpectedModels).
	expectedModels() map[analysis.Role]string
	// deployments are the triage and escalation deployments (escalation nil when not configured).
	deployments() (triage, escalation *remediationv1alpha1.ModelDeployment)
	// forcesEscalation says every run goes to the escalation model (EVAL_FORCE_ESCALATION, see
	// forcedEscalation).
	forcesEscalation() bool
}

// newClient is the fake API server of a run: it holds the recorded objects and nothing else.
func newClient(scheme *runtime.Scheme, objs []client.Object) client.Client {
	return clientfake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

// metricsSource is the metrics source of a run: the scenario's recording replayed through the
// providers.MetricsSource interface, or none (a nil interface, not a nil value) when the scenario has no
// metrics.yaml, so the context says no metrics source is configured, as an analyzer without --prometheus-url.
func (s *scenario) metricsSource() providers.MetricsSource {
	if s.metrics == nil {
		return nil
	}
	return newReplayMetrics(s.metrics)
}

// runScenario builds the scenario's context with the real context builder and runs the real analysis loop
// once.
func runScenario(ctx context.Context, scheme *runtime.Scheme, s *scenario, models modelSource) (scenarioResult, error) {
	res := scenarioResult{Name: s.name, Recording: s.recording.Source}
	c := newClient(scheme, s.clusterObjects())
	redactor, err := redact.New()
	if err != nil {
		return res, err
	}
	now := func() time.Time { return fixtureNow }
	metrics := s.metricsSource()
	builder, err := incidentcontext.New(incidentcontext.Config{Pods: c, Objects: c, Namespaces: watchedNamespaces,
		Metrics: metrics, Redactor: redactor, Now: now,
		Policies: evalPolicies, PolicyReader: c, RequestNamespace: evalRequestNamespace})
	if err != nil {
		return res, err
	}
	set, err := tools.New(tools.Config{Pods: c, Events: c, Objects: c, Metrics: metrics, Redactor: redactor,
		Namespaces: watchedNamespaces, Now: now})
	if err != nil {
		return res, err
	}
	factory, done, err := models.factory(s)
	if err != nil {
		return res, err
	}
	var cb analysis.ContextBuilder = builder
	if models.forcesEscalation() {
		cb = forcedEscalation{inner: builder}
	}
	t := &tally{}
	a, err := analysis.New(analysis.Config{
		Builder: cb, Tools: set, Policies: evalPolicies, ExpectedModels: models.expectedModels(),
		Redactor: redactor, Now: now,
		Models: func(role analysis.Role, d remediationv1alpha1.ModelDeployment) (providers.ModelProvider, error) {
			p, err := factory(role, d)
			if err != nil {
				return nil, err
			}
			return recorded{inner: p, role: role, t: t}, nil
		},
		// The evaluation has no daily budget: every scenario run is one unit the run itself bounds.
		Reserve: func(context.Context, time.Time) error { return nil },
	})
	if err != nil {
		return res, err
	}
	limits := analysis.Limits{MaxToolSteps: evalMaxToolSteps, MaxOutputTokens: evalMaxOutputTokens}
	limits.Triage, limits.Escalation = models.deployments()
	out, err := a.Analyze(ctx, s.incident.DeepCopy(), limits)
	if err != nil {
		return res, fmt.Errorf("the analysis stopped: %w", err)
	}
	if done != nil {
		if err := done(); err != nil {
			return res, err
		}
	}
	res.Reason, res.Escalated, res.Calls, res.ToolCalls = string(out.Reason), out.Escalated, out.Calls, out.ToolCalls
	res.Models = out.Models
	t.mu.Lock()
	res.Tokens = tokens{Charged: out.Tokens}
	for role, u := range t.usage {
		rt := roleTokens{Input: u.usage.InputTokens, CachedInput: u.usage.CachedInputTokens,
			Output: u.usage.OutputTokens, UsageUnknownCalls: u.usageUnknown}
		res.Tokens.addRole(string(role), rt)
	}
	last := t.last
	t.mu.Unlock()
	res.Outcome = scoreOutcome(s, out)
	for _, q := range out.QuoteFailures {
		res.QuoteFailures = append(res.QuoteFailures, quoteFailure{Source: q.Source, Block: q.Block, Class: string(q.Class),
			QuoteBytes: q.QuoteBytes, BlockBytes: q.BlockBytes, MatchedBytes: q.MatchedBytes})
	}
	if !slices.Contains(resultReasons, out.Reason) {
		// The loop did not accept a result (a refused model, invalid output, a limit): nothing to score.
		last = ""
	}
	res.Diagnosis = scoreDiagnosis(s.rubric, diagnosisText(last))
	if accepted, _, err := analysis.DecodeResult(last); err == nil && accepted != nil {
		// Redacted as the analyzer redacts the result's text for its logs and record, keeping the Incident's name.
		res.Reply = replyOf(accepted, func(t string) string { return redactor.StringKeeping(t, s.incident.Name) })
	}
	res.Pass = res.Outcome.Pass && res.Diagnosis.Pass
	return res, nil
}

// Reasons that come from a valid result without a proposal: the model itself concluded that no catalog
// action applies. A refusal for any other reason (a model failure, a limit, a proposal the loop dropped) is
// not the refusal the scenario expects.
var refusalReasons = []analysis.Reason{analysis.ReasonNoProposal, analysis.ReasonMissingData}

// resultReasons are the ends of an analysis whose last reply was a valid result the loop accepted, so its
// text is the diagnosis: the result's own ends, and the checks of its proposal after decoding.
var resultReasons = []analysis.Reason{analysis.ReasonProposed, analysis.ReasonNoProposal, analysis.ReasonMissingData,
	analysis.ReasonNoEvidence, analysis.ReasonTargetNotSupported, analysis.ReasonNoPolicyForAction,
	analysis.ReasonPreviousValueUnknown, analysis.ReasonNoChange, analysis.ReasonDirectionReversed}

// scoreOutcome compares the loop's outcome with expected.yaml's.
func scoreOutcome(scn *scenario, out analysis.Outcome) outcomeScore {
	e := scn.expected
	s := outcomeScore{Expected: e.Outcome.Type, Reason: string(out.Reason)}
	if out.Plan != nil {
		s.Action = string(out.Plan.Action.Type)
	}
	switch e.Outcome.Type {
	case outcomeRefusal:
		switch {
		case out.Plan != nil:
			s.Detail = fmt.Sprintf("a refusal was expected, the loop proposed %s", out.Plan.Action.Type)
		case !slices.Contains(refusalReasons, out.Reason):
			s.Detail = fmt.Sprintf("a refusal was expected from the model's result, the loop ended %s", out.Reason)
		default:
			s.Pass = true
		}
		if e.Outcome.Phase != "" {
			s.NotScored = append(s.NotScored, "phase "+e.Outcome.Phase+" (the executor's)")
		}
	case outcomePullRequest:
		s.Pass, s.Detail = scoreProposal(scn, out)
		if len(e.Outcome.Paths) > 0 {
			s.NotScored = append(s.NotScored, "paths "+strings.Join(e.Outcome.Paths, ", ")+" (source mapping, the executor's)")
		}
	default:
		s.Detail = fmt.Sprintf("expected.yaml has the unknown outcome type %q", e.Outcome.Type)
	}
	return s
}

// scoreProposal checks a plan against the expected action and bounds, and a memory plan against the
// scenario's workload: the container must exist in its pod template, and the previous limit must be the
// one it holds (and the one expected.yaml names, when it names one).
func scoreProposal(scn *scenario, out analysis.Outcome) (bool, string) {
	e := scn.expected
	if out.Plan == nil || out.Reason != analysis.ReasonProposed {
		return false, fmt.Sprintf("a %s proposal was expected, the loop ended %s without one", e.Outcome.Action, out.Reason)
	}
	a := out.Plan.Action
	if string(a.Type) != e.Outcome.Action {
		return false, fmt.Sprintf("a %s proposal was expected, the loop proposed %s", e.Outcome.Action, a.Type)
	}
	if a.SetContainerMemoryLimit != nil {
		if detail := checkMemoryTarget(scn, a.SetContainerMemoryLimit); detail != "" {
			return false, detail
		}
	}
	if detail := checkPolicy(scn, out.Plan); detail != "" {
		return false, detail
	}
	b := e.Outcome.Bounds
	if b == nil {
		return true, ""
	}
	switch {
	case a.SetContainerMemoryLimit != nil:
		lo, err1 := resource.ParseQuantity(b.Min.String())
		hi, err2 := resource.ParseQuantity(b.Max.String())
		if err1 != nil || err2 != nil {
			return false, fmt.Sprintf("expected.yaml's bounds %s to %s are not quantities", b.Min.String(), b.Max.String())
		}
		d := a.SetContainerMemoryLimit.Desired
		if d.Cmp(lo) < 0 || d.Cmp(hi) > 0 {
			return false, fmt.Sprintf("desired %s is outside %s to %s", d.String(), lo.String(), hi.String())
		}
	case a.AdjustKedaMaxReplicas != nil:
		d := a.AdjustKedaMaxReplicas.DesiredValue
		if b.Min.Type != intstr.Int || b.Max.Type != intstr.Int {
			return false, fmt.Sprintf("expected.yaml's bounds %s to %s are not integers", b.Min.String(), b.Max.String())
		}
		if d < b.Min.IntVal || d > b.Max.IntVal {
			return false, fmt.Sprintf("desired %d is outside %d to %d", d, b.Min.IntVal, b.Max.IntVal)
		}
	default:
		return false, "the plan carries no parameters to check against the bounds"
	}
	return true, ""
}

// checkPolicy returns why the scenario's policy would refuse the plan, or "": the checks of policy.Preflight,
// the executor's own, on a request of the plan for the Incident's subject (#493). The harness only scores with
// it: nothing it computes authorizes anything. The checks of the resolved source and the target's state need
// Git and the live object, which the scenarios do not record.
func checkPolicy(scn *scenario, p *analysis.Plan) string {
	i := slices.IndexFunc(scn.policies, func(q *remediationv1alpha1.RemediationPolicy) bool {
		return q.Name == string(p.PolicyRef)
	})
	if i < 0 {
		return fmt.Sprintf("the scenario has no policy %q", p.PolicyRef)
	}
	subject := scn.incident.Spec.Subject
	approval := true
	rr := &remediationv1alpha1.RemediationRequest{
		ObjectMeta: metav1.ObjectMeta{Namespace: evalRequestNamespace, Name: "eval"},
		Spec: remediationv1alpha1.RemediationRequestSpec{TargetRef: subject, PolicyRef: p.PolicyRef,
			Mode: remediationv1alpha1.RemediationModeGitOps, ApprovalRequired: &approval, Action: p.Action},
	}
	target := &policy.Target{APIVersion: subject.APIVersion, Kind: subject.Kind, Namespace: subject.Namespace,
		Name: subject.Name, UID: subject.UID}
	for _, o := range scn.objects {
		if remediationv1alpha1.UID(o.GetUID()) == subject.UID {
			target.Labels = o.GetLabels()
		}
	}
	d, verdict := policy.Preflight(policy.DecisionInput{Request: rr, Policy: scn.policies[i], Settings: evalSettings(),
		Target: target})
	if verdict != policy.PreflightContinue {
		return fmt.Sprintf("policy %s refuses the plan: %s (%s)", p.PolicyRef, d.Reason, d.Message)
	}
	return ""
}

// checkMemoryTarget returns why a SetContainerMemoryLimit does not fit the scenario, or "".
func checkMemoryTarget(scn *scenario, p *remediationv1alpha1.SetContainerMemoryLimit) string {
	e := scn.expected
	if e.Outcome.Container != "" && p.Container != e.Outcome.Container {
		return fmt.Sprintf("container %q was expected, the loop proposed %q", e.Outcome.Container, p.Container)
	}
	if e.Outcome.Previous != "" {
		want, err := resource.ParseQuantity(e.Outcome.Previous)
		if err != nil {
			return fmt.Sprintf("expected.yaml's previous %q is not a quantity", e.Outcome.Previous)
		}
		if p.Previous.Cmp(want) != 0 {
			return fmt.Sprintf("previous %s was expected, the loop proposed %s", want.String(), p.Previous.String())
		}
	}
	o := e.Incident.Object
	containers, ok := workloadContainers(scn.objects, o.Kind, o.Namespace, o.Name)
	if !ok {
		return fmt.Sprintf("the scenario holds no %s %s/%s with a pod template", o.Kind, o.Namespace, o.Name)
	}
	for _, c := range containers {
		if c.Name != p.Container {
			continue
		}
		// The loop takes the previous value from the pod template the context builder read (#494), so this
		// checks that substitution end to end, against the recorded objects: it fails when the plan carries
		// any other value, the model's included (the oom/rollout variant without the substitution).
		cur, ok := c.Resources.Limits[corev1.ResourceMemory]
		if !ok {
			return fmt.Sprintf("the container sets no memory limit, the loop proposed previous %s", p.Previous.String())
		}
		if cur.Cmp(p.Previous) != 0 {
			return fmt.Sprintf("previous %s is not the container's current limit %s", p.Previous.String(), cur.String())
		}
		return ""
	}
	return fmt.Sprintf("the %s %s/%s has no container %q", o.Kind, o.Namespace, o.Name, p.Container)
}

// workloadContainers returns the containers of the Deployment or StatefulSet among objs.
func workloadContainers(objs []client.Object, kind, namespace, name string) ([]corev1.Container, bool) {
	for _, o := range objs {
		if o.GetNamespace() != namespace || o.GetName() != name {
			continue
		}
		switch w := o.(type) {
		case *appsv1.Deployment:
			if kind == "Deployment" {
				return w.Spec.Template.Spec.Containers, true
			}
		case *appsv1.StatefulSet:
			if kind == "StatefulSet" {
				return w.Spec.Template.Spec.Containers, true
			}
		}
	}
	return nil, false
}

// diagnosisText is the text of the result the loop accepted, for the rubric: the summary, the hypotheses
// and the evidence quotes. It is empty when the last reply is not a valid result: a reply the loop refused
// (malformed, or an action outside the catalog's bounds) carries no diagnosis.
func diagnosisText(content string) string {
	res, _, err := analysis.DecodeResult(content)
	if err != nil || res == nil {
		return ""
	}
	parts := []string{res.Summary}
	for _, h := range res.Hypotheses {
		parts = append(parts, h.Description)
	}
	for _, e := range res.Evidence {
		parts = append(parts, e.Quote)
	}
	return strings.Join(parts, "\n")
}

// scoreDiagnosis scores each fact of the rubric against the diagnosis text.
func scoreDiagnosis(r rubric, text string) diagnosisScore {
	s := diagnosisScore{Pass: true}
	lower := strings.ToLower(text)
	for _, f := range r.Facts {
		if f.Unscored != "" {
			s.Unscored = append(s.Unscored, unscoredFact{Fact: f.Fact, Why: f.Unscored})
			continue
		}
		fs := factScore{Fact: f.Fact, Pass: true}
		for _, term := range f.All {
			if !strings.Contains(lower, strings.ToLower(term)) {
				fs.Pass, fs.Missing = false, append(fs.Missing, term)
			}
		}
		if len(f.Any) > 0 {
			found := false
			for _, term := range f.Any {
				found = found || strings.Contains(lower, strings.ToLower(term))
			}
			if !found {
				fs.Pass, fs.Missing = false, append(fs.Missing, "one of: "+strings.Join(f.Any, " | "))
			}
		}
		s.Facts = append(s.Facts, fs)
		s.Pass = s.Pass && fs.Pass
	}
	if text == "" {
		s.Pass, s.Detail = false, "no valid result to score"
	}
	return s
}
