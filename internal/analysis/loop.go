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
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
	"sigs.k8s.io/controller-runtime/pkg/log"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/incidentcontext"
	"github.com/pkalin/shadewright/internal/incidents"
	"github.com/pkalin/shadewright/internal/policy"
	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/redact"
	"github.com/pkalin/shadewright/internal/tools"
)

// Bounds of an analysis.
const (
	// DefaultTimeout is the deadline of one analysis when Config.Timeout is zero. It is under the trigger's
	// analysis timeout (10 minutes), after which an Incident left in Analyzing is taken as lost.
	DefaultTimeout = 8 * time.Minute
	// MaxTimeout is the longest Config.Timeout [New] accepts.
	MaxTimeout = 9 * time.Minute
	// DefaultMaxTokensPerAnalysis bounds the tokens one analysis may spend, all calls together, when
	// Config.MaxTokensPerAnalysis is zero.
	DefaultMaxTokensPerAnalysis = 500_000
	// MaxToolCallsPerReply bounds the tool calls one reply may ask for; a reply with more is invalid.
	MaxToolCallsPerReply = 16
	// minQuoteBytes is the shortest quote, spaces trimmed, that counts as evidence.
	minQuoteBytes = 8
	// maxRetryWait is the longest delay a retryable error may ask for that the loop waits out, once per call.
	maxRetryWait = 30 * time.Second
)

// Reason says how an analysis ended. Reasons are fixed strings from code, used as the reason of the
// Incident's Analyzed condition; nothing from the model or the cluster goes into them.
type Reason string

// Reasons.
const (
	// ReasonProposed: the result carried a valid proposal with evidence; a request is created.
	ReasonProposed Reason = "Proposed"
	// ReasonRunbookMatched: a deterministic runbook matched and proposed without a model call.
	ReasonRunbookMatched Reason = "RunbookMatched"
	// ReasonNoModelConfigured: RemediationSettings names no triage model, so no model was called and no unit
	// of the daily budget used.
	ReasonNoModelConfigured Reason = "NoModelConfigured"
	// ReasonBudgetSpent: the daily analysis budget is spent; no model was called.
	ReasonBudgetSpent Reason = "BudgetSpent"
	// ReasonBudgetUnavailable: the budget could not be read or trusted; no model was called.
	ReasonBudgetUnavailable Reason = "BudgetUnavailable"
	// ReasonNoProposal: the result made no proposal.
	ReasonNoProposal Reason = "NoProposal"
	// ReasonMissingData: the result made no proposal and listed missing data.
	ReasonMissingData Reason = "MissingData"
	// ReasonNoEvidence: the result proposed without a quote found in the blocks it cites.
	ReasonNoEvidence Reason = "NoEvidence"
	// ReasonTargetNotSupported: the proposed action may not target the Incident's subject, or the subject has
	// no namespace or UID.
	ReasonTargetNotSupported Reason = "TargetNotSupported"
	// ReasonNoPolicyForAction: no RemediationPolicy is configured for the proposed action type.
	ReasonNoPolicyForAction Reason = "NoPolicyForAction"
	// ReasonActionOutOfBounds: the proposed action failed the policy's catalog check (outside the catalog's
	// bounds). It gets no retry and no feedback, so the model cannot search for what the policy allows, and it
	// is not an escalation (decided 2026-11-05).
	ReasonActionOutOfBounds Reason = "ActionOutOfBounds"
	// ReasonInvalidOutput: the result did not match the schema, and no escalation could follow.
	ReasonInvalidOutput Reason = "InvalidOutput"
	// ReasonEscalationNotConfigured: an escalation signal was found and no escalation model is configured.
	ReasonEscalationNotConfigured Reason = "EscalationNotConfigured"
	// ReasonStepLimit: the escalated model asked for tool calls past maxToolSteps.
	ReasonStepLimit Reason = "StepLimit"
	// ReasonTokenLimit: the next call could exceed the analysis's or the deployment's token bound.
	ReasonTokenLimit Reason = "TokenLimit"
	// ReasonDeadlineExceeded: the analysis ran out of its deadline.
	ReasonDeadlineExceeded Reason = "DeadlineExceeded"
	// ReasonModelUnavailable: the model stayed throttled or failing after its bounded retries, or its retries
	// used up the analysis's bound of 3 + maxToolSteps model calls.
	ReasonModelUnavailable Reason = "ModelUnavailable"
	// ReasonModelNotFound: the deployment does not exist.
	ReasonModelNotFound Reason = "ModelNotFound"
	// ReasonModelForbidden: the endpoint refused the analyzer's identity.
	ReasonModelForbidden Reason = "ModelForbidden"
	// ReasonModelError: any other failure of the model call, or of building its provider.
	ReasonModelError Reason = "ModelError"
	// ReasonUnexpectedModel: a reply named another model than the one configured for its deployment's role
	// (ADR-0023 decision 8), or none; it is not used.
	ReasonUnexpectedModel Reason = "UnexpectedModel"
	// ReasonEndpointNotAllowed: the deployment's endpoint is outside the operator's allow-list of host
	// suffixes (ErrEndpointNotAllowed), so nothing was sent to it and no token requested for it.
	ReasonEndpointNotAllowed Reason = "EndpointNotAllowed"
)

// ErrEndpointNotAllowed is what a [ModelFactory] returns, wrapped, for an endpoint outside the operator's
// allow-list; the analysis hands off with ReasonEndpointNotAllowed.
var ErrEndpointNotAllowed = errors.New("the model endpoint is not allowed")

// factoryFailure is the hand-off reason of a ModelFactory error.
func factoryFailure(err error) Reason {
	if errors.Is(err, ErrEndpointNotAllowed) {
		return ReasonEndpointNotAllowed
	}
	return ReasonModelError
}

// ContextBuilder builds the context of an Incident; *incidentcontext.Builder implements it.
type ContextBuilder interface {
	Build(ctx context.Context, inc *remediationv1alpha1.Incident) (*incidentcontext.Context, error)
}

// ToolSet is the read tools offered on escalation; *tools.Set implements it.
type ToolSet interface {
	Definitions() []providers.ToolDefinition
	Call(ctx context.Context, call providers.ToolCall) (tools.Result, error)
}

// Role names the use of a model deployment.
type Role string

// Roles.
const (
	RoleTriage     Role = "triage"
	RoleEscalation Role = "escalation"
)

// ModelFactory returns the provider of one deployment named in RemediationSettings. It is where the endpoint
// is checked against the operator's allow-list before any call (internal/analyzer/models.NewModelFactory); an error
// hands the incident off.
type ModelFactory func(role Role, d remediationv1alpha1.ModelDeployment) (providers.ModelProvider, error)

// ReserveFunc takes one unit of the daily analysis budget, as [incidents.ReserveAnalysis]. Only a nil error
// allows the model call.
type ReserveFunc func(ctx context.Context, now time.Time) error

// Plan is a validated proposal: what the request the controller creates will carry.
type Plan struct {
	// Action is from the closed catalog and passed the policy's catalog check.
	Action remediationv1alpha1.Action
	// Checks are extra verification checks, each once.
	Checks []remediationv1alpha1.VerificationCheck
	// PolicyRef is the RemediationPolicy configured for the action type, never chosen by the model.
	PolicyRef remediationv1alpha1.Name
}

// Runbook is a deterministic rule that maps an Incident's symptoms to a known plan without a model call
// (ADR-0023 decision 4). None ships yet; the loop runs any it is given first.
type Runbook interface {
	Match(inc *remediationv1alpha1.Incident) (Plan, bool)
}

// Config configures an [Analyzer].
type Config struct {
	// Builder builds the context. Required.
	Builder ContextBuilder
	// Tools are the read tools offered on escalation; nil offers none.
	Tools ToolSet
	// Models returns the provider of a deployment. Required.
	Models ModelFactory
	// Reserve takes a unit of the daily budget before the first model call. Required.
	Reserve ReserveFunc
	// Policies names the RemediationPolicy a request of each action type refers to. An action type without
	// one is never proposed.
	Policies map[remediationv1alpha1.ActionType]remediationv1alpha1.Name
	// ExpectedModels names, per role, the model a reply must name in Completion.Model: the model and version
	// the operator configured for that role's deployment (ADR-0023 decision 8). A reply naming another model,
	// or none, is refused and the incident handed off (ReasonUnexpectedModel). A role with no entry is not
	// configured, whatever RemediationSettings names: no triage entry means no model call, and no escalation
	// entry hands an escalation off.
	ExpectedModels map[Role]string
	// Runbooks run before the model, in order.
	Runbooks []Runbook
	// Timeout is the deadline of one analysis; zero means [DefaultTimeout], at most [MaxTimeout].
	Timeout time.Duration
	// MaxTokensPerAnalysis bounds the tokens of one analysis; zero means [DefaultMaxTokensPerAnalysis].
	MaxTokensPerAnalysis int64
	// Now returns the current time; nil means time.Now.
	Now func() time.Time
	// Redactor redacts the result text the outcome carries for the logs (Outcome.Summary and
	// Outcome.MissingData): pass the one the context builder uses. Nil means the built-in rules of package
	// redact.
	Redactor *redact.Redactor
	// TracerProvider makes the spans of each analysis (#90): one trace per analysis, with a span per phase,
	// model call and tool call. The spans carry IDs, counts, fixed result labels and the configured model and
	// deployment names only, never text from the cluster, a tool or the model. Nil means no tracing.
	TracerProvider trace.TracerProvider
}

// Analyzer runs the bounded analysis of one Incident at a time per call. It is safe for concurrent use when
// its dependencies are.
type Analyzer struct {
	cfg    Config
	tracer trace.Tracer
}

// New checks the configuration.
func New(cfg Config) (*Analyzer, error) {
	if cfg.Builder == nil || cfg.Models == nil || cfg.Reserve == nil {
		return nil, errors.New("analysis: a context builder, a model factory and a budget reservation are required")
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = DefaultTimeout
	}
	if cfg.Timeout < 0 || cfg.Timeout > MaxTimeout {
		return nil, fmt.Errorf("analysis: timeout %s is outside 0 to %s", cfg.Timeout, MaxTimeout)
	}
	if cfg.MaxTokensPerAnalysis == 0 {
		cfg.MaxTokensPerAnalysis = DefaultMaxTokensPerAnalysis
	}
	if cfg.MaxTokensPerAnalysis < 0 {
		return nil, errors.New("analysis: the token bound must not be negative")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	tp := cfg.TracerProvider
	if tp == nil {
		tp = noop.NewTracerProvider()
	}
	return &Analyzer{cfg: cfg, tracer: tp.Tracer(TracerName)}, nil
}

// Timeout returns the deadline of one analysis.
func (a *Analyzer) Timeout() time.Duration {
	return a.cfg.Timeout
}

// Limits are the settings an analysis reads from RemediationSettings spec.analysis.
type Limits struct {
	// MaxToolSteps is the most tool calls of an escalation.
	MaxToolSteps int
	// MaxOutputTokens is sent with every call.
	MaxOutputTokens int
	// Triage and Escalation are the deployments; nil means not configured.
	Triage, Escalation *remediationv1alpha1.ModelDeployment
}

// LimitsOf reads the limits from the settings. A field the API server's defaults never leave unset is taken
// at its lowest safe value when it is missing: no tool steps and the smallest output bound.
func LimitsOf(s *remediationv1alpha1.RemediationSettings) Limits {
	l := Limits{MaxOutputTokens: 256}
	a := s.Spec.Analysis
	if a == nil {
		return l
	}
	if a.MaxToolSteps != nil {
		l.MaxToolSteps = int(min(max(*a.MaxToolSteps, 0), 16))
	}
	if a.MaxOutputTokens != nil {
		l.MaxOutputTokens = int(min(max(*a.MaxOutputTokens, 256), 16384))
	}
	if a.Models != nil {
		l.Triage, l.Escalation = a.Models.Triage, a.Models.Escalation
	}
	return l
}

// Outcome is how an analysis ended.
type Outcome struct {
	// Reason is fixed text from code.
	Reason Reason
	// Plan is set only with ReasonProposed or ReasonRunbookMatched.
	Plan *Plan
	// Escalated says the escalation model was called.
	Escalated bool
	// Calls is the number of model calls made, retries included.
	Calls int
	// ToolCalls is the number of tool calls run.
	ToolCalls int
	// Tokens is what the analysis was charged: reported usage, or the upper bound of a call whose usage is
	// unknown or that failed.
	Tokens int64
	// Models are the model IDs that answered, as the replies named them.
	Models []string
	// PromptVersion is [PromptVersion].
	PromptVersion string
	// Summary is the short diagnosis of a valid result, redacted, for the log line of the outcome; empty
	// without a result. Model text: it is logged, never read to decide anything.
	Summary string
	// MissingData is what a valid result said was missing, each item redacted, for the log line of the
	// outcome (#422); nil when the result listed nothing. Model text: logged, never read to decide anything.
	MissingData []string
	// QuoteFailures classifies every quote of a result refused with ReasonNoEvidence, for the evaluation's
	// report (#474): classes and lengths only, no text. Evaluation-only: the analyzer neither logs nor records it,
	// and never reads it to decide anything; nil for any other reason.
	QuoteFailures []QuoteFailure
}

// Analyze runs the analysis of inc with the limits of the settings: runbooks first, then (with a triage
// model configured) the context, one unit of the daily budget, the triage call with one retry after invalid
// output, and escalation on the deterministic signals only. It returns an error only when ctx is done (a
// shutdown or a lost leadership): every other end is an Outcome, a hand-off unless it carries a Plan. The
// analysis is one trace, whose root span ends with the outcome.
func (a *Analyzer) Analyze(ctx context.Context, inc *remediationv1alpha1.Incident, limits Limits) (Outcome, error) {
	ctx, span := a.tracer.Start(ctx, spanAnalysis, trace.WithAttributes(incidentAttrs(inc)...))
	out, err := a.analyze(ctx, inc, limits)
	endAnalysis(span, out, err)
	return out, err
}

func (a *Analyzer) analyze(ctx context.Context, inc *remediationv1alpha1.Incident, limits Limits) (Outcome, error) {
	out := Outcome{PromptVersion: PromptVersion()}
	for _, rb := range a.cfg.Runbooks {
		if p, ok := rb.Match(inc); ok {
			return a.accept(inc, out, ReasonRunbookMatched, p)
		}
	}
	if limits.Triage == nil || a.cfg.ExpectedModels[RoleTriage] == "" {
		out.Reason = ReasonNoModelConfigured
		return out, nil
	}
	if a.cfg.ExpectedModels[RoleEscalation] == "" {
		limits.Escalation = nil
	}
	actx, cancel := context.WithTimeout(ctx, a.cfg.Timeout)
	defer cancel()
	r := &run{a: a, parent: ctx, ctx: actx, inc: inc, limits: limits, out: &out, spent: map[string]int64{},
		maxCalls: 3 + max(limits.MaxToolSteps, 0)}

	c, err := r.build()
	if err != nil {
		if ctx.Err() != nil {
			return out, fmt.Errorf("analysis: %w", ctx.Err())
		}
		return r.handOff(ReasonDeadlineExceeded)
	}
	r.pods = c.Pods
	var shownContext string
	shownContext, r.shown = contextMessage(c)
	r.contextText = shownContext

	// The first model call is known before the budget is touched: a unit is reserved only when a provider for
	// it is configured and could be built, so a hand-off that makes no call spends nothing.
	role, deployment := RoleTriage, limits.Triage
	if c.Escalate() {
		if limits.Escalation == nil {
			return r.handOff(ReasonEscalationNotConfigured)
		}
		role, deployment = RoleEscalation, limits.Escalation
	}
	model, err := a.cfg.Models(role, *deployment)
	if err != nil {
		return r.handOff(factoryFailure(err))
	}

	if err := r.reserve(); err != nil {
		if ctx.Err() != nil {
			return out, fmt.Errorf("analysis: %w", ctx.Err())
		}
		if errors.Is(err, incidents.ErrBudgetSpent) {
			return r.handOff(ReasonBudgetSpent)
		}
		return r.handOff(ReasonBudgetUnavailable)
	}

	var res *Result
	var action *remediationv1alpha1.Action
	var reason Reason
	if role == RoleTriage {
		res, action, reason, err = r.tracedTriage(model)
		if err != nil {
			return out, err
		}
		if reason != "" {
			return r.handOff(reason)
		}
		if res != nil {
			return r.finish(res, action)
		}
		// The triage output was invalid twice: an escalation signal. The budget unit is already spent.
		if limits.Escalation == nil {
			return r.handOff(ReasonEscalationNotConfigured)
		}
		if model, err = a.cfg.Models(RoleEscalation, *limits.Escalation); err != nil {
			return r.handOff(factoryFailure(err))
		}
	}
	res, action, reason, err = r.tracedEscalate(model)
	if err != nil {
		return out, err
	}
	if reason != "" {
		return r.handOff(reason)
	}
	return r.finish(res, action)
}

// build runs the context builder in its span.
func (r *run) build() (*incidentcontext.Context, error) {
	ctx, span := r.a.tracer.Start(r.ctx, spanContext)
	defer span.End()
	c, err := r.a.cfg.Builder.Build(ctx, r.inc)
	if err != nil {
		if r.parent.Err() != nil {
			setCanceled(span)
			return nil, err
		}
		span.SetAttributes(attrResult.String(resultFailed))
		span.SetStatus(codes.Error, "the context could not be built")
		return nil, err
	}
	span.SetAttributes(attrResult.String(resultOK), attrBlocks.Int(len(c.Blocks)), attrContextBytes.Int(c.Size()),
		attrTruncated.Bool(c.Truncated), attrFailedSources.Int(len(c.FailedSources)), attrPods.Int(len(c.Pods)))
	return c, nil
}

// reserve takes the unit of the daily budget in its span.
func (r *run) reserve() error {
	ctx, span := r.a.tracer.Start(r.ctx, spanReserve)
	defer span.End()
	err := r.a.cfg.Reserve(ctx, r.a.cfg.Now())
	switch {
	case err == nil:
		span.SetAttributes(attrResult.String(resultOK))
	case r.parent.Err() != nil:
		setCanceled(span)
	case errors.Is(err, incidents.ErrBudgetSpent):
		span.SetAttributes(attrResult.String(resultSpent))
	default:
		span.SetAttributes(attrResult.String(resultUnavailable))
		span.SetStatus(codes.Error, "the budget could not be reserved")
	}
	return err
}

// phase starts the span of a phase and makes it the parent of the spans that follow, until the returned
// function ends it; callers defer it. r.ctx keeps its deadline and cancellation: only the span it carries
// changes.
func (r *run) phase(name string) func() {
	prev := r.ctx
	ctx, span := r.a.tracer.Start(r.ctx, name)
	r.ctx = ctx
	return func() {
		span.End()
		r.ctx = prev
	}
}

// tracedTriage runs triage in the span of its phase.
func (r *run) tracedTriage(model providers.ModelProvider) (*Result, *remediationv1alpha1.Action, Reason, error) {
	defer r.phase(spanTriage)()
	return r.triage(model)
}

// tracedEscalate runs escalate in the span of its phase.
func (r *run) tracedEscalate(model providers.ModelProvider) (*Result, *remediationv1alpha1.Action, Reason, error) {
	defer r.phase(spanEscalation)()
	return r.escalate(model)
}

// run is the state of one analysis.
type run struct {
	a           *Analyzer
	parent, ctx context.Context
	inc         *remediationv1alpha1.Incident
	limits      Limits
	out         *Outcome
	pods        []string
	contextText string
	// shown is each block and tool result as the model saw it, by ID, with its quotable parts.
	shown map[string]shownBlock
	// maxCalls is the most model calls of the analysis, retries included: 3 + MaxToolSteps (ADR-0023
	// decision 6).
	maxCalls int
	// spent is the tokens charged per deployment.
	spent map[string]int64
	tools int
}

func (r *run) handOff(reason Reason) (Outcome, error) {
	r.out.Reason = reason
	r.out.Plan = nil
	return *r.out, nil
}

// Codes the corrective message of the triage retry names. They are fixed: the message never echoes the
// model's reply.
const (
	retryMalformed = "malformed-reply"
	retryToolCall  = "tool-call"
)

// retryMessage is the corrective user message of the triage retry, from code: it names the rule the first
// reply broke by its fixed code and nothing else.
func retryMessage(code string) string {
	return "Your previous answer was refused (rule: " + code + "). Answer again with one JSON object that " +
		"matches the output schema, with no tool call and nothing before or after it.\n"
}

// lastTurnMessage tells the escalated model that its tool calls are spent. The tools stay offered, as on
// every turn of the escalation (the adapter refuses a call naming a tool the request did not offer), and a
// tool call on this turn ends the analysis with ReasonStepLimit.
const lastTurnMessage = "No tool calls are left. Answer now with the JSON object that matches the output " +
	"schema; a tool call on this turn ends the analysis without a result.\n"

// triage makes the one structured call without tools, and asks once more after invalid output, with a fixed
// corrective message. It returns a result and its converted action, or a hand-off reason, or neither when the
// output was invalid twice (an escalation signal).
func (r *run) triage(model providers.ModelProvider) (*Result, *remediationv1alpha1.Action, Reason, error) {
	msgs := []providers.Message{
		{Role: providers.RoleSystem, Content: systemPrompt(false, 0)},
		{Role: providers.RoleUser, Content: r.contextText},
	}
	var code string
	for attempt := range 2 {
		if attempt > 0 {
			msgs = append(msgs[:2:2], providers.Message{Role: providers.RoleUser, Content: retryMessage(code)})
		}
		c, reason, err := r.call(model, RoleTriage, r.limits.Triage, msgs, nil)
		if err != nil || (reason != "" && reason != ReasonInvalidOutput) {
			return nil, nil, reason, err
		}
		switch {
		case reason == ReasonInvalidOutput:
			code = retryMalformed
			continue
		case len(c.ToolCalls) > 0:
			code = retryToolCall
			continue
		}
		res, action, err := DecodeResult(c.Content)
		if err == nil {
			return res, action, "", nil
		}
		if errors.Is(err, errOutOfBounds) {
			return nil, nil, ReasonActionOutOfBounds, nil
		}
		code = errInvalid.code
		if de, ok := errors.AsType[*decodeError](err); ok {
			code = de.code
		}
	}
	return nil, nil, "", nil
}

// escalate calls the escalation model with the read tools, bound to the Incident's namespace, for at most
// MaxToolSteps tool calls and MaxToolSteps + 1 turns. Every turn offers the same tools; the last turn says
// in a fixed message that no calls are left, and a tool call on it is ReasonStepLimit. An invalid result is
// handed off, never retried.
func (r *run) escalate(model providers.ModelProvider) (*Result, *remediationv1alpha1.Action, Reason, error) {
	r.out.Escalated = true
	steps := r.limits.MaxToolSteps
	defs := r.offered()
	if steps == 0 {
		defs = nil
	}
	if len(defs) == 0 {
		steps = 0
	}
	offered := make(map[string]bool, len(defs))
	for _, d := range defs {
		offered[d.Name] = true
	}
	msgs := []providers.Message{
		{Role: providers.RoleSystem, Content: systemPrompt(steps > 0, steps)},
		{Role: providers.RoleUser, Content: r.contextText},
	}
	used := 0
	for range steps + 1 {
		last := used >= steps
		if last && steps > 0 {
			msgs = append(msgs, providers.Message{Role: providers.RoleUser, Content: lastTurnMessage})
		}
		c, reason, err := r.call(model, RoleEscalation, r.limits.Escalation, msgs, defs)
		if err != nil || reason != "" {
			return nil, nil, reason, err
		}
		if len(c.ToolCalls) == 0 {
			res, action, err := DecodeResult(c.Content)
			if errors.Is(err, errOutOfBounds) {
				return nil, nil, ReasonActionOutOfBounds, nil
			}
			if err != nil {
				return nil, nil, ReasonInvalidOutput, nil
			}
			return res, action, "", nil
		}
		if last {
			return nil, nil, ReasonStepLimit, nil
		}
		if len(c.ToolCalls) > MaxToolCallsPerReply {
			return nil, nil, ReasonInvalidOutput, nil
		}
		msgs = append(msgs, providers.Message{Role: providers.RoleAssistant, ToolCalls: c.ToolCalls})
		for _, call := range c.ToolCalls {
			var content string
			if used >= steps {
				content = refusedResult(call.Name, offered, "refused: no tool calls are left; answer with the JSON result\n")
			} else {
				used++
				content, err = r.callTool(call, offered)
				if err != nil {
					return nil, nil, "", err
				}
			}
			msgs = append(msgs, providers.Message{Role: providers.RoleTool, ToolCallID: call.ID, Content: content})
		}
	}
	// Unreachable: every turn before the last uses at least one step, so the turn with no step left returns.
	return nil, nil, ReasonStepLimit, nil
}

// offered returns the tool definitions offered on escalation: every configured tool except
// get_delivery_status, whose delivery namespaces are not decided yet (#76 comment of 2026-11-05).
func (r *run) offered() []providers.ToolDefinition {
	if r.a.cfg.Tools == nil {
		return nil
	}
	var defs []providers.ToolDefinition
	for _, d := range r.a.cfg.Tools.Definitions() {
		if d.Name != tools.NameGetDeliveryStatus {
			defs = append(defs, d)
		}
	}
	return defs
}

// namespaceFree are the tools whose arguments name no namespace: the Git tools, which read the GitOps
// repository, not the cluster.
var namespaceFree = []string{tools.NameReadGitFile, tools.NameGetRecentChanges}

// refusedResult renders a refusal. It names the tool only when the escalation offered it: a tool never
// offered is never described to the model, not even by its name.
func refusedResult(name string, offered map[string]bool, text string) string {
	return tools.Result{Tool: toolLabel(name, offered), Status: tools.StatusRefused, Text: text}.Content()
}

// callTool runs one tool call bound to the Incident's namespace. A call that names another namespace, or a
// tool not offered, is refused before the tool sees it. An answer about a pod the context builder did not
// attribute to the subject is labelled so. The result gets the next tool ID (t1, t2...) for the evidence
// check, which may quote only within one data value ([tools.Result.Data]) of a successful answer about the
// subject or one of its attributed pods ([run.aboutSubject]): never the block's header, labels or boilerplate, a refusal, an answer that was not
// found, not available or failed, an answer about another object or none, or one labelled outside the attribution.
func (r *run) callTool(call providers.ToolCall, offered map[string]bool) (string, error) {
	r.tools++
	r.out.ToolCalls++
	id := fmt.Sprintf("t%d", r.tools)
	ctx, span := r.a.tracer.Start(r.ctx, spanToolCall,
		trace.WithAttributes(attrTool.String(toolLabel(call.Name, offered))))
	defer span.End()
	content, quotable, err := r.boundCall(ctx, span, call, offered)
	if err != nil {
		setCanceled(span)
		return "", err
	}
	span.SetAttributes(attrQuotable.Bool(len(quotable) > 0))
	r.shown[id] = shownBlock{name: toolLabel(call.Name, offered), text: content, parts: quotable}
	return "Tool result " + id + ".\n" + content, nil
}

// boundCall runs one call within the bounds of the escalation. It returns the result as the model sees it,
// and the parts of it a quote may cite as evidence ([tools.Result.Data]; none when none may). It records the
// fixed status of the result on span, never its text or the call's arguments.
func (r *run) boundCall(ctx context.Context, span trace.Span, call providers.ToolCall,
	offered map[string]bool) (string, []string, error) {
	refused := func(text string) (string, []string, error) {
		span.SetAttributes(attrResult.String(resultRefused), attrToolStatus.String(string(tools.StatusRefused)))
		return refusedResult(call.Name, offered, text), nil, nil
	}
	// Defence in depth: the adapter already refuses a reply that names a tool the request did not offer.
	if !offered[call.Name] {
		return refused("refused: this tool is not offered\n")
	}
	// Fail closed: arguments that are too large or not one JSON object are refused here, and every tool but the
	// Git tools, which name no namespace, must name the subject's.
	var args map[string]json.RawMessage
	if len(call.Arguments) > tools.MaxArgumentBytes || json.Unmarshal(call.Arguments, &args) != nil || args == nil {
		return refused("refused: the arguments do not match the schema\n")
	}
	if !slices.Contains(namespaceFree, call.Name) {
		var ns string
		raw, ok := args["namespace"]
		if !ok || json.Unmarshal(raw, &ns) != nil || ns == "" || ns != r.inc.Spec.Subject.Namespace {
			return refused("refused: the analysis reads only the namespace of the incident's subject\n")
		}
	}
	res, err := r.a.cfg.Tools.Call(ctx, call)
	if err != nil {
		if r.parent.Err() != nil {
			return "", nil, fmt.Errorf("analysis: %w", r.parent.Err())
		}
		// The analysis deadline passed during the call: the next model call stops the loop.
		span.SetAttributes(attrResult.String(resultFailed), attrToolStatus.String(string(tools.StatusCouldNotFetch)))
		span.SetStatus(codes.Error, "the tool call failed")
		return refusedResult(call.Name, offered, "could not fetch\n"), nil, nil
	}
	span.SetAttributes(attrResult.String(resultOK), attrToolStatus.String(toolStatusLabel(res.Status)))
	var quotable []string
	if r.outsideAttribution(call.Name, args) {
		span.SetAttributes(attrOutside.Bool(true))
		res.Labels = append(res.Labels, tools.LabelOutsideAttribution)
	} else if res.Status == tools.StatusOK && r.aboutSubject(call.Name, args) {
		quotable = res.Data()
	}
	return res.Content(), quotable, nil
}

// kindPod is the kind of a pod in the read tools' arguments, and appsV1 the API version of the other kinds.
const (
	kindPod = "Pod"
	appsV1  = "apps/v1"
)

// subjectKinds are the API versions of the kinds the read tools name, for comparing a call's object with
// the Incident's subject.
var subjectKinds = map[string]string{kindPod: "v1", "Deployment": appsV1, "StatefulSet": appsV1, "ReplicaSet": appsV1}

// aboutSubject reports whether a call asks about the Incident's subject or one of its attributed pods (decided
// 2026-11-05): only such an answer is evidence. A call that names a pod is about that pod, whatever else it
// names. Otherwise only get_resource and list_events can name the subject, by kind and name; list_events
// without them, query_metrics without a pod (a Deployment's metric included), the Git tools and every other
// tool are shown to the model but are never evidence.
func (r *run) aboutSubject(name string, args map[string]json.RawMessage) bool {
	str := func(k string) string {
		var s string
		if raw, ok := args[k]; ok && json.Unmarshal(raw, &s) == nil {
			return s
		}
		return ""
	}
	if _, ok := args["pod"]; ok {
		pod := str("pod")
		return pod != "" && slices.Contains(r.pods, pod)
	}
	if name != tools.NameGetResource && name != tools.NameListEvents {
		return false
	}
	kind, obj := str("kind"), str("name")
	if kind == "" || obj == "" {
		return false
	}
	if kind == kindPod {
		return slices.Contains(r.pods, obj)
	}
	s := r.inc.Spec.Subject
	return kind == s.Kind && obj == s.Name && subjectKinds[kind] == s.APIVersion
}

// outsideAttribution reports whether the call names a pod that is not one of the context's attributed pods.
func (r *run) outsideAttribution(name string, args map[string]json.RawMessage) bool {
	str := func(k string) (string, bool) {
		raw, ok := args[k]
		if !ok {
			return "", false
		}
		var s string
		return s, json.Unmarshal(raw, &s) == nil
	}
	if pod, ok := str("pod"); ok {
		return !slices.Contains(r.pods, pod)
	}
	if kind, _ := str("kind"); kind == kindPod && (name == tools.NameGetResource || name == tools.NameListEvents) {
		if pod, ok := str("name"); ok {
			return !slices.Contains(r.pods, pod)
		}
	}
	return false
}

// call makes one model call within the token bounds and the deadline. It returns a hand-off reason for a
// failure (ReasonInvalidOutput for malformed output, which the caller may retry) and an error only when the
// parent context is done. A reply, or a billed failure, that names another model than the one expected for
// role is refused (ReasonUnexpectedModel), and so is a reply that names none. A retryable error that names a delay of at most maxRetryWait is waited out and the
// call made once more, within the deadline and within the analysis's bound of 3 + MaxToolSteps calls, which
// the retry counts toward.
func (r *run) call(model providers.ModelProvider, role Role, d *remediationv1alpha1.ModelDeployment,
	msgs []providers.Message, offer []providers.ToolDefinition) (providers.Completion, Reason, error) {
	req := providers.CompletionRequest{
		Messages: slices.Clone(msgs), Tools: offer, MaxOutputTokens: r.limits.MaxOutputTokens,
		ResponseSchema: ResponseSchema(),
	}
	upper := estimateInput(req) + int64(r.limits.MaxOutputTokens)
	key := d.Endpoint + "\x00" + d.Deployment
	for attempt := 0; ; attempt++ {
		if err := r.parent.Err(); err != nil {
			return providers.Completion{}, "", fmt.Errorf("analysis: %w", err)
		}
		if r.ctx.Err() != nil {
			return providers.Completion{}, ReasonDeadlineExceeded, nil
		}
		dayCap := int64(0)
		if d.MaxTokensPerDay != nil {
			dayCap = *d.MaxTokensPerDay
		}
		if r.out.Tokens+upper > r.a.cfg.MaxTokensPerAnalysis || r.spent[key]+upper > dayCap {
			return providers.Completion{}, ReasonTokenLimit, nil
		}
		// A retry counts toward the analysis's bound of model calls. Without retries the loop never reaches
		// it, so a call it stops is one the model's throttling took the room of.
		if r.out.Calls >= r.maxCalls {
			return providers.Completion{}, ReasonModelUnavailable, nil
		}
		r.out.Calls++
		a := r.attempt(model, role, d, req, upper, key, attempt)
		if !a.again {
			return a.c, a.reason, a.err
		}
		select {
		case <-time.After(a.wait):
		case <-r.ctx.Done():
		}
	}
}

// attemptResult is the end of one model call: a reply, a hand-off reason or an error as [run.call] returns
// them, or again with the delay to wait out before the call is made once more.
type attemptResult struct {
	c      providers.Completion
	reason Reason
	err    error
	again  bool
	wait   time.Duration
}

// attempt makes one model call, charges it and classifies its end, in its own span. The span records the role,
// the deployment, the model the reply named, the usage and the fixed result label, never the request or the
// reply.
func (r *run) attempt(model providers.ModelProvider, role Role, d *remediationv1alpha1.ModelDeployment,
	req providers.CompletionRequest, upper int64, key string, attempt int) (res attemptResult) {
	ctx, span := r.a.tracer.Start(r.ctx, spanModelCall, trace.WithAttributes(
		attrOperation.String("chat"), attrRole.String(string(role)), attrDeployment.String(d.Deployment),
		attrAttempt.Int(attempt+1), attrMaxOutputTokens.Int(req.MaxOutputTokens)))
	defer func() { endModelCall(span, res) }()
	c, err := model.Complete(ctx, req)
	// Fail closed: a call that failed, or whose usage is unknown, is charged its upper bound. Whether and
	// how much the service billed it cannot be told (a timeout may come after the reply was generated), so
	// the analysis and deployment bounds count the most it could have cost. A malformed reply was billed
	// and reports its usage, which is charged as reported.
	charge := upper
	billed := (err == nil || providers.IsMalformedOutput(err)) && !c.UsageUnknown
	if billed {
		charge = c.Usage.InputTokens + c.Usage.OutputTokens
		span.SetAttributes(attrInputTokens.Int64(c.Usage.InputTokens), attrOutputTokens.Int64(c.Usage.OutputTokens))
	}
	span.SetAttributes(attrCharged.Int64(charge), attrUsageUnknown.Bool(c.UsageUnknown))
	r.out.Tokens += charge
	r.spent[key] += charge
	if c.Model != "" {
		// The adapter checks the name's form, so it is safe to record, as it is to log below.
		span.SetAttributes(attrResponseModel.String(c.Model))
		if !slices.Contains(r.out.Models, c.Model) {
			r.out.Models = append(r.out.Models, c.Model)
		}
	}
	// ADR-0023 decision 8: only the model configured for the role may answer. The adapter checks the name's
	// form, so it is safe to log; a reply that names no model cannot be placed and is refused too.
	if want := r.a.cfg.ExpectedModels[role]; r.parent.Err() == nil && (err == nil || c.Model != "") && c.Model != want {
		log.FromContext(r.parent).Info("Refused a model reply from another model than the configured one",
			"role", role, "deployment", d.Deployment, "expected", want, "got", c.Model)
		return attemptResult{reason: ReasonUnexpectedModel}
	}
	switch {
	case err == nil:
		span.SetAttributes(attrRequestedTools.Int(len(c.ToolCalls)))
		return attemptResult{c: c}
	case r.parent.Err() != nil:
		return attemptResult{err: fmt.Errorf("analysis: %w", r.parent.Err())}
	case r.ctx.Err() != nil:
		return attemptResult{reason: ReasonDeadlineExceeded}
	case providers.IsMalformedOutput(err):
		return attemptResult{reason: ReasonInvalidOutput}
	case providers.IsNotFound(err):
		return attemptResult{reason: ReasonModelNotFound}
	case providers.IsForbidden(err):
		return attemptResult{reason: ReasonModelForbidden}
	case providers.IsRetryable(err):
		wait, ok := providers.RetryAfter(err)
		if attempt > 0 || !ok || wait > maxRetryWait {
			return attemptResult{reason: ReasonModelUnavailable}
		}
		return attemptResult{again: true, wait: wait}
	default:
		return attemptResult{reason: ReasonModelError}
	}
}

// resultRetry is the result label of a model call whose retryable error is waited out and the call made again.
const resultRetry = "retry"

// endModelCall records the fixed result label of a model call and ends its span. A call that did not return a
// usable reply gets the error status with a fixed description, never the error's text.
func endModelCall(span trace.Span, a attemptResult) {
	label := resultOK
	switch {
	case a.err != nil:
		label = resultCanceled
	case a.again:
		label = resultRetry
	case a.reason != "":
		label = string(a.reason)
	}
	span.SetAttributes(attrResult.String(label))
	if label != resultOK {
		span.SetStatus(codes.Error, "the model call returned no usable reply")
	}
	span.End()
}

// estimateInput is a conservative estimate of a request's input tokens: one token per two bytes of
// everything sent, which overestimates the tokenisers in use for English text and JSON.
func estimateInput(req providers.CompletionRequest) int64 {
	n := len(req.ResponseSchema)
	for _, m := range req.Messages {
		n += len(m.Content) + len(m.ToolCallID)
		for _, c := range m.ToolCalls {
			n += len(c.ID) + len(c.Name) + len(c.Arguments)
		}
	}
	for _, t := range req.Tools {
		n += len(t.Name) + len(t.Description) + len(t.Parameters)
	}
	return int64(n+1) / 2
}

// finish turns a valid result into the outcome: a hand-off without a proposal, or a plan once the proposal
// has evidence found in the blocks the model saw, may target the subject and has a policy configured. action
// is the proposal's action as [DecodeResult] converted and checked it, nil without a proposal.
func (r *run) finish(res *Result, action *remediationv1alpha1.Action) (Outcome, error) {
	r.out.Summary, r.out.MissingData = r.redact(res.Summary), nil
	for _, m := range res.MissingData {
		r.out.MissingData = append(r.out.MissingData, r.redact(m))
	}
	if res.Proposal == nil || action == nil {
		if len(res.MissingData) > 0 {
			return r.handOff(ReasonMissingData)
		}
		return r.handOff(ReasonNoProposal)
	}
	if !r.evidenced(res.Evidence) {
		r.out.QuoteFailures = r.quoteFailures(res.Evidence)
		return r.handOff(ReasonNoEvidence)
	}
	var checks []remediationv1alpha1.VerificationCheck
	for _, c := range res.Proposal.VerificationChecks {
		if !slices.Contains(checks, c) {
			checks = append(checks, c)
		}
	}
	return r.a.accept(r.inc, *r.out, ReasonProposed, Plan{Action: *action, Checks: checks})
}

// redact redacts model text for the logs, keeping whole occurrences of the Incident's name: the name was read
// from the API server, and the collector's names are a long hex run the redactor takes for a token, as in the
// pull request body. The output is never longer than the input, so the result's bounds (DecodeResult) still
// hold after it.
func (r *run) redact(s string) string {
	if r.a.cfg.Redactor == nil {
		return redact.StringKeeping(s, r.inc.Name)
	}
	return r.a.cfg.Redactor.StringKeeping(s, r.inc.Name)
}

// evidenced reports whether at least one quote is found verbatim in a quotable part of the block or tool
// result it cites.
func (r *run) evidenced(ev []Evidence) bool {
	for _, e := range ev {
		q := strings.TrimSpace(e.Quote)
		if len(q) < minQuoteBytes {
			continue
		}
		for _, part := range r.shown[e.Source].parts {
			if strings.Contains(part, q) {
				return true
			}
		}
	}
	return false
}

// accept checks a plan against the subject and the configured policies, from a runbook or a result.
func (a *Analyzer) accept(inc *remediationv1alpha1.Incident, out Outcome, reason Reason, p Plan) (Outcome, error) {
	s := inc.Spec.Subject
	if s.Namespace == "" || s.UID == "" || !policy.ActionMayTarget(p.Action.Type, s.APIVersion, s.Kind) {
		out.Reason = ReasonTargetNotSupported
		return out, nil
	}
	if reason, _ := policy.CheckAction(p.Action); reason != "" {
		out.Reason = ReasonActionOutOfBounds
		return out, nil
	}
	ref, ok := a.cfg.Policies[p.Action.Type]
	if !ok || ref == "" {
		out.Reason = ReasonNoPolicyForAction
		return out, nil
	}
	p.PolicyRef = ref
	out.Reason, out.Plan = reason, &p
	return out, nil
}
