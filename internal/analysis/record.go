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
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/redact"
)

// RecordSchemaVersion is the version of the analysis record's JSON; it changes when a field changes meaning or
// goes away.
const RecordSchemaVersion = 1

// storeTimeout bounds the write of the analysis record with the live read of the claim before it. It is cut so
// that the outcome write still has its own full timeout inside the claim; [Controller.CheckTimeBudget] counts
// it when a store is set.
const storeTimeout = 30 * time.Second

// EventRecordNotWritten is the reason of the Warning event on an Incident whose analysis record was not
// written.
const EventRecordNotWritten = "AnalysisRecordNotWritten"

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
	verified := []providers.VerifiedName{providers.VerifiedName(claimed.Name)}
	if request != "" {
		verified = append(verified, providers.VerifiedName(request))
	}
	keep := make([]string, 0, len(verified))
	for _, v := range verified {
		if !v.Valid() {
			return providers.AnalysisRecord{}, fmt.Errorf("a name of the analysis record is not an object name")
		}
		keep = append(keep, string(v))
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
		ID: rec.ID, Fingerprint: claimed.Spec.Fingerprint, CreatedAt: rec.FinishedAt, Data: data, Verified: verified,
	}, nil
}

// recordMessages are the messages of the Warning event EventRecordNotWritten, by result. They are fixed text:
// no error, URL or record data.
var recordMessages = map[string]string{
	recordFailed:  "The analysis record was not written: the write failed; the outcome is in the status and the analyzer log",
	recordSkipped: "The analysis record was not written: no time was left on the claim, or the claim was lost",
	recordRefused: "The analysis record was not written: the store refused it, or it could not be built; see the analyzer log",
}

// storeRecord writes the run's record when a store is configured, and returns its ID for
// status.analysisRecords, or "" when there is no store or no record was stored. It first reads the Incident
// live, so a record is written only for a run that still owns its claim: errClaimLost is returned then, and
// the caller drops the outcome as finish would. When the create failed (request empty, attempted its name),
// a request the server created anyway is named in the record, as finish records it.
//
// The write is bounded by storeTimeout and ends early enough that the outcome write keeps its full timeout
// inside the claim; with less time left it is skipped. Any other failure is logged, counted in
// shadewright_analysis_records_total and reported with a Warning event on the Incident, and never changes the
// outcome: the record is evidence, not state. A conflict means the record's ID exists; the ID is the run's
// own, so an earlier try of this write stored it (one whose answer was lost), and the ID is returned.
func (c *Controller) storeRecord(ctx context.Context, claimed *remediationv1alpha1.Incident, out Outcome,
	limits Limits, request, attempted remediationv1alpha1.Name) (remediationv1alpha1.RecordID, error) {
	if c.Store == nil {
		return "", nil
	}
	logger := log.FromContext(ctx).WithValues("incident", claimed.Name, "record", string(recordID(claimed)))
	sctx, cancel, err := c.withinClaim(ctx, claimed, storeTimeout, finalWriteTimeout, false)
	if err != nil {
		logger.Info("Skipped the analysis record: no time is left on the claim before the outcome write")
		c.recordResult(claimed, recordSkipped)
		return "", nil
	}
	defer cancel()
	live, err := c.recheck(sctx, claimed)
	switch {
	case errors.Is(err, errClaimLost):
		logger.Info("Skipped the analysis record: the run no longer owns its claim")
		c.recordResult(claimed, recordSkipped)
		return "", err
	case err != nil:
		logger.Error(err, "Failed to read the Incident before the analysis record; no record was written")
		c.recordResult(claimed, recordFailed)
		return "", nil
	}
	if request == "" && attempted != "" && c.createdAnyway(sctx, live, attempted) {
		request = attempted
	}
	rec, err := c.buildRecord(claimed, out, limits, request)
	if err != nil {
		logger.Error(err, "Failed to build the analysis record")
		c.recordResult(claimed, recordRefused)
		return "", nil
	}
	// The store's errors are fixed text: no URL, token or record data.
	_, err = c.Store.Put(sctx, rec)
	switch {
	case err == nil:
		c.recordResult(claimed, recordWritten)
	case providers.IsConflict(err):
		logger.Info("The analysis record exists already: its ID is this run's own, so an earlier try of the "+
			"write stored it; its ID is recorded", "error", err.Error())
		c.recordResult(claimed, recordExists)
	case providers.IsRejected(err) || providers.IsTooLarge(err):
		logger.Error(err, "The analysis store refused the analysis record")
		c.recordResult(claimed, recordRefused)
		return "", nil
	default:
		logger.Error(err, "Failed to write the analysis record", "retryable", providers.IsRetryable(err),
			"canceled", ctx.Err() != nil)
		c.recordResult(claimed, recordFailed)
		return "", nil
	}
	return remediationv1alpha1.RecordID(rec.ID), nil
}

// recordResult counts a record write and, when no record was stored, writes the Warning event on the Incident.
func (c *Controller) recordResult(claimed *remediationv1alpha1.Incident, result string) {
	analysisRecords.WithLabelValues(result).Inc()
	if msg, ok := recordMessages[result]; ok && c.Recorder != nil {
		c.Recorder.Event(claimed, corev1.EventTypeWarning, EventRecordNotWritten, msg)
	}
}
