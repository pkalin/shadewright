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
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/tools"
)

// Tracing (#90): each analysis is one trace. Spans carry only IDs, counts, durations, fixed result labels from
// code and the model and deployment names the operator configured (invariant 4): never a prompt, context or
// tool text, a tool's arguments, a reply, an error message or a token. An error is recorded as a status with a
// fixed description, never with its text, since an error can quote what it failed on.
//
// The span tree:
//
//	shadewright.analysis                 the whole analysis (Analyzer.Analyze)
//	├── shadewright.analysis.context     the context builder
//	├── shadewright.analysis.reserve     the unit of the daily budget
//	├── shadewright.analysis.triage      the triage phase
//	│   └── shadewright.model.call       each model call, retries included
//	└── shadewright.analysis.escalation  the escalation phase
//	    ├── shadewright.model.call
//	    └── shadewright.tool.call        each tool call the model asked for
const (
	// TracerName is the instrumentation scope of the analysis's spans.
	TracerName = "github.com/pkalin/shadewright/internal/analysis"

	spanAnalysis   = "shadewright.analysis"
	spanContext    = "shadewright.analysis.context"
	spanReserve    = "shadewright.analysis.reserve"
	spanTriage     = "shadewright.analysis.triage"
	spanEscalation = "shadewright.analysis.escalation"
	spanModelCall  = "shadewright.model.call"
	spanToolCall   = "shadewright.tool.call"
)

// Attribute keys. The model call's keys follow the OpenTelemetry semantic conventions for generative AI where
// one fits.
const (
	attrIncidentName    = attribute.Key("shadewright.incident.name")
	attrIncidentUID     = attribute.Key("shadewright.incident.uid")
	attrRun             = attribute.Key("shadewright.analysis.run")
	attrReason          = attribute.Key("shadewright.analysis.reason")
	attrEscalated       = attribute.Key("shadewright.analysis.escalated")
	attrCalls           = attribute.Key("shadewright.analysis.model_calls")
	attrToolCalls       = attribute.Key("shadewright.analysis.tool_calls")
	attrTokens          = attribute.Key("shadewright.analysis.tokens")
	attrPromptVersion   = attribute.Key("shadewright.analysis.prompt_version")
	attrModels          = attribute.Key("shadewright.analysis.models")
	attrProposed        = attribute.Key("shadewright.analysis.proposed")
	attrBlocks          = attribute.Key("shadewright.context.blocks")
	attrContextBytes    = attribute.Key("shadewright.context.bytes")
	attrTruncated       = attribute.Key("shadewright.context.truncated")
	attrFailedSources   = attribute.Key("shadewright.context.failed_sources")
	attrPods            = attribute.Key("shadewright.context.pods")
	attrResult          = attribute.Key("shadewright.result")
	attrRole            = attribute.Key("shadewright.model.role")
	attrAttempt         = attribute.Key("shadewright.model.attempt")
	attrCharged         = attribute.Key("shadewright.model.charged_tokens")
	attrUsageUnknown    = attribute.Key("shadewright.model.usage_unknown")
	attrRequestedTools  = attribute.Key("shadewright.model.tool_calls")
	attrOperation       = attribute.Key("gen_ai.operation.name")
	attrDeployment      = attribute.Key("gen_ai.request.model")
	attrResponseModel   = attribute.Key("gen_ai.response.model")
	attrInputTokens     = attribute.Key("gen_ai.usage.input_tokens")
	attrOutputTokens    = attribute.Key("gen_ai.usage.output_tokens")
	attrMaxOutputTokens = attribute.Key("gen_ai.request.max_tokens")
	attrTool            = attribute.Key("gen_ai.tool.name")
	attrToolStatus      = attribute.Key("shadewright.tool.status")
	attrQuotable        = attribute.Key("shadewright.tool.quotable")
	attrOutside         = attribute.Key("shadewright.tool.outside_attribution")
)

// Fixed result labels of the context, reserve and call spans (attrResult), besides the Reasons.
const (
	resultOK          = "ok"
	resultFailed      = "failed"
	resultCanceled    = "canceled"
	resultSpent       = "spent"
	resultUnavailable = "unavailable"
	resultRefused     = "refused"
)

// toolStatuses are the tool statuses a span may name; any other is named "unknown".
var toolStatuses = []tools.Status{tools.StatusOK, tools.StatusRefused, tools.StatusNotFound,
	tools.StatusNotAvailable, tools.StatusCouldNotFetch}

// toolStatusLabel returns s if it is one of the fixed statuses, else "unknown".
func toolStatusLabel(s tools.Status) string {
	for _, k := range toolStatuses {
		if s == k {
			return string(k)
		}
	}
	return unknownLabel
}

// unknownLabel stands for a tool name the escalation did not offer, or a tool status outside the fixed set.
const unknownLabel = "unknown"

// toolLabel names a tool, in a span or a refusal, only when the escalation offered it: a name the
// model made up is never recorded.
func toolLabel(name string, offered map[string]bool) string {
	if offered[name] {
		return name
	}
	return unknownLabel
}

// incidentAttrs are the identity attributes of the root span: the Incident's name and UID, and the run's
// lastAnalysisTime when the claim wrote one. All of them come from the API server.
func incidentAttrs(inc *remediationv1alpha1.Incident) []attribute.KeyValue {
	kv := []attribute.KeyValue{attrIncidentName.String(inc.Name), attrIncidentUID.String(string(inc.UID))}
	if t := inc.Status.LastAnalysisTime; t != nil {
		kv = append(kv, attrRun.String(t.UTC().Format(time.RFC3339)))
	}
	return kv
}

// endAnalysis records the outcome on the root span and ends it. err is non-nil only when the parent context was
// done; its text is not recorded.
func endAnalysis(span trace.Span, out Outcome, err error) {
	span.SetAttributes(
		attrReason.String(string(out.Reason)),
		attrEscalated.Bool(out.Escalated),
		attrCalls.Int(out.Calls),
		attrToolCalls.Int(out.ToolCalls),
		attrTokens.Int64(out.Tokens),
		attrPromptVersion.String(out.PromptVersion),
		attrModels.StringSlice(out.Models),
		attrProposed.Bool(out.Plan != nil),
	)
	if err != nil {
		span.SetAttributes(attrResult.String(resultCanceled))
		span.SetStatus(codes.Error, "the analysis was canceled")
	}
	span.End()
}
