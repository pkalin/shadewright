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
	"fmt"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/redact"
)

// RecordSchemaVersion is the version of the analysis record's JSON; it changes when a field changes meaning or
// goes away.
const RecordSchemaVersion = 1

// storeTimeout bounds the write of the analysis record. It is cut so that the outcome write still has its own
// full timeout inside the claim.
const storeTimeout = 30 * time.Second

// maxRecordIDs is the most record IDs status.analysisRecords keeps.
const maxRecordIDs = 20

// Record is the analysis record written to the [providers.AnalysisStore] (ADR-0015). Every string from the
// cluster, the settings or a model is redacted; the counts, the reason and the prompt version come from code. It holds no
// context, prompt or raw reply.
type Record struct {
	SchemaVersion int            `json:"schemaVersion"`
	ID            string         `json:"id"`
	Incident      RecordIncident `json:"incident"`
	// StartedAt is the run's lastAnalysisTime, which identifies it (ADR-0025).
	StartedAt  time.Time `json:"startedAt"`
	FinishedAt time.Time `json:"finishedAt"`
	Reason     Reason    `json:"reason"`
	Escalated  bool      `json:"escalated"`
	Calls      int       `json:"calls"`
	ToolCalls  int       `json:"toolCalls"`
	// Tokens is what the analysis was charged ([Outcome].Tokens). The key is "usage": a key named like
	// "tokens" with a value is what the redactor takes for a credential.
	Tokens int64 `json:"usage"`
	// Deployments are the model deployments of the settings the run read, by role.
	Deployments RecordDeployments `json:"deployments"`
	// Models are the model IDs the replies named.
	Models        []string `json:"models"`
	PromptVersion string   `json:"promptVersion"`
	Summary       string   `json:"summary,omitempty"`
	MissingData   []string `json:"missingData,omitempty"`
	// Request is the name of the RemediationRequest the run created, if any.
	Request string `json:"request,omitempty"`
}

// RecordDeployments are the deployment names of the triage and escalation models; empty when not configured.
type RecordDeployments struct {
	Triage     string `json:"triage,omitempty"`
	Escalation string `json:"escalation,omitempty"`
}

// RecordIncident identifies the Incident of a record.
type RecordIncident struct {
	Name        string `json:"name"`
	UID         string `json:"uid"`
	Fingerprint string `json:"fingerprint"`
	Reason      string `json:"reason"`
	SubjectKind string `json:"subjectKind"`
	Namespace   string `json:"namespace,omitempty"`
	SubjectName string `json:"subjectName"`
}

// recordID derives the record's ID from the Incident's UID and the run's lastAnalysisTime: one per run, so a
// second write for the same run is refused by the create-only store instead of replacing the first.
func recordID(claimed *remediationv1alpha1.Incident) remediationv1alpha1.RecordID {
	return remediationv1alpha1.RecordID(string(claimed.UID) + "-" +
		claimed.Status.LastAnalysisTime.UTC().Format("20060102t150405z"))
}

// buildRecord returns the record of a run. Text from the cluster or the model passes through the analyzer's
// redactor, keeping the Incident's name and the request's name, which the analysis read back or made.
func (c *Controller) buildRecord(claimed *remediationv1alpha1.Incident, out Outcome, limits Limits,
	request remediationv1alpha1.Name) (providers.AnalysisRecord, error) {
	red := c.Analyzer.cfg.Redactor
	keep := []string{claimed.Name}
	if request != "" {
		keep = append(keep, string(request))
	}
	r := func(s string) string {
		if red == nil {
			return redact.StringKeeping(s, keep...)
		}
		return red.StringKeeping(s, keep...)
	}
	subject := claimed.Spec.Subject
	rec := Record{
		SchemaVersion: RecordSchemaVersion,
		ID:            string(recordID(claimed)),
		Incident: RecordIncident{
			Name: claimed.Name, UID: string(claimed.UID), Fingerprint: claimed.Spec.Fingerprint,
			Reason: r(claimed.Spec.Reason), SubjectKind: r(subject.Kind), Namespace: r(subject.Namespace),
			SubjectName: r(subject.Name),
		},
		StartedAt:  claimed.Status.LastAnalysisTime.UTC(),
		FinishedAt: c.now().UTC().Truncate(time.Second),
		Reason:     out.Reason, Escalated: out.Escalated, Calls: out.Calls, ToolCalls: out.ToolCalls,
		Tokens: out.Tokens, Models: make([]string, 0, len(out.Models)), PromptVersion: out.PromptVersion,
		Summary: r(out.Summary), Request: string(request),
	}
	if d := limits.Triage; d != nil {
		rec.Deployments.Triage = r(d.Deployment)
	}
	if d := limits.Escalation; d != nil {
		rec.Deployments.Escalation = r(d.Deployment)
	}
	for _, m := range out.Models {
		rec.Models = append(rec.Models, r(m))
	}
	for _, m := range out.MissingData {
		rec.MissingData = append(rec.MissingData, r(m))
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return providers.AnalysisRecord{}, fmt.Errorf("encoding the analysis record: %w", err)
	}
	return providers.AnalysisRecord{
		ID: rec.ID, Fingerprint: claimed.Spec.Fingerprint, CreatedAt: rec.FinishedAt, Data: data, Verified: keep,
	}, nil
}

// storeRecord writes the run's record when a store is configured, and returns its ID, or "" when there is no
// store or the write failed. A failure is logged and never changes the outcome: the record is evidence, not
// state. The write is bounded by storeTimeout and ends early enough that the outcome write keeps its full
// timeout inside the claim; with less time left it is skipped.
func (c *Controller) storeRecord(ctx context.Context, claimed *remediationv1alpha1.Incident, out Outcome,
	limits Limits, request remediationv1alpha1.Name) remediationv1alpha1.RecordID {
	if c.Store == nil {
		return ""
	}
	logger := log.FromContext(ctx).WithValues("incident", claimed.Name)
	left := c.expiry(claimed).Add(-finalWriteTimeout).Sub(c.now())
	if left <= 0 {
		logger.Info("Skipped the analysis record: no time is left on the claim before the outcome write")
		return ""
	}
	rec, err := c.buildRecord(claimed, out, limits, request)
	if err != nil {
		logger.Error(err, "Failed to build the analysis record")
		return ""
	}
	sctx, cancel := context.WithTimeout(ctx, min(storeTimeout, left))
	defer cancel()
	// The store's errors are fixed text: no URL, token or record data.
	if _, err := c.Store.Put(sctx, rec); err != nil {
		logger.Error(err, "Failed to write the analysis record", "record", rec.ID,
			"exists", providers.IsConflict(err), "retryable", providers.IsRetryable(err))
		return ""
	}
	return remediationv1alpha1.RecordID(rec.ID)
}
