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

package main

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
)

// The kinds explain accepts by name.
const (
	kindRequest  = "RemediationRequest"
	kindIncident = "Incident"
)

// stateUIDMismatch is the Incident state when an Incident of the request's incidentRef name exists, but with
// another UID: the request answered an earlier Incident of that name.
const stateUIDMismatch = "uidMismatch"

// troubleshootingURL is the published troubleshooting page. Links are this URL and a fixed anchor from
// troubleshootingSections; nothing from the cluster goes into a link.
const troubleshootingURL = "https://shadewright.io/troubleshooting/"

// The anchors of the "## " sections of the troubleshooting page that list reasons, as the docs site makes them
// from the headings.
const (
	secPhases      = "how-to-read-the-result"
	secLimits      = "limits"
	secRequest     = "request-and-target"
	secFlux        = "delivery-from-flux"
	secRepository  = "repository-branch-and-path"
	secFiles       = "files-in-git"
	secPolicy      = "policy-decision"
	secPullRequest = "pull-request-step"
	secRollout     = "rollout-after-the-merge"
	secReceipt     = "merge-receipt"
	secAfterMerge  = "after-the-merge"
	secFreshness   = "freshness"
	secRender      = "render-check"
	secNotRequest  = "not-request-reasons"
)

// troubleshootingSections maps each reason listed in a table of docs/troubleshooting.md to the anchors of the
// sections whose tables list it, in page order. Table rows have no anchors of their own on the docs site, so a
// link goes to the section that holds the row. TestTroubleshootingSections keeps this map equal to the page.
var troubleshootingSections = map[string][]string{
	"AboveMaximum":                      {secPolicy},
	"ActionNotAllowed":                  {secPolicy},
	"ActionUnknown":                     {secPolicy},
	"AmbiguousDeclaration":              {secFiles},
	"AmbiguousTarget":                   {secRender},
	"AnotherRequestClosed":              {secPhases},
	"AnotherRequestMerged":              {secPhases},
	"BelowMinimum":                      {secPolicy},
	"BranchDiverged":                    {secPullRequest},
	"BranchMismatch":                    {secRepository},
	"BranchNotAllowed":                  {secRepository},
	"BranchNotFound":                    {secRepository},
	"ChainLinkInvalid":                  {secPhases},
	"ChangeRequestClosed":               {secPullRequest},
	"ChangeRequestGone":                 {secPullRequest},
	"ChangeRequestMerged":               {secPullRequest},
	"CheckNotAvailable":                 {secAfterMerge},
	"CircuitHalfOpen":                   {secLimits},
	"CircuitOpen":                       {secLimits},
	"ClearingNotChecked":                {secAfterMerge},
	"ConcurrencyLimitReached":           {secLimits},
	"DeclarationNotFound":               {secFiles},
	"DecreaseNotAllowed":                {secPolicy},
	"DecreaseTooLarge":                  {secPolicy},
	"Decryption":                        {secFlux},
	"DeliveryBlocked":                   {secAfterMerge},
	"DeliveryFailed":                    {secAfterMerge},
	"DeliveryNotChecked":                {secAfterMerge},
	"DeliveryNotObserved":               {secAfterMerge, secRollout},
	"DeliverySuspended":                 {secAfterMerge},
	"DesiredValueMissing":               {secRender},
	"DirectionReversed":                 {secNotRequest},
	"EvidenceMissing":                   {secAfterMerge},
	"FieldNotFound":                     {secRequest, secRender},
	"FileMissing":                       {secRender},
	"FileNotInBuild":                    {secRender},
	"FileNotReadable":                   {secFiles},
	"ForeignChangeRequest":              {secPullRequest},
	"GitForbidden":                      {secPullRequest},
	"GitRejected":                       {secPullRequest},
	"HandedOff":                         {secPhases},
	"HandoffAborted":                    {secPhases},
	"HandoffRecoveryFailed":             {secPullRequest},
	"HandoffRefused":                    {secPhases},
	"HeadMoved":                         {secPullRequest},
	"HelmManaged":                       {secFlux},
	"HintsTurnedOff":                    {secPhases},
	"IncompleteInput":                   {secPolicy},
	"IncreaseTooLarge":                  {secPolicy},
	"InputTooLarge":                     {secFiles},
	"InvalidFluxLabels":                 {secFlux},
	"InvalidInput":                      {secRequest, secRender},
	"InvalidParameters":                 {secPolicy},
	"InvalidPath":                       {secFiles},
	"InvalidReference":                  {secRender},
	"InvalidYAML":                       {secRender},
	"KedaNotReady":                      {secAfterMerge},
	"KindExcluded":                      {secPolicy},
	"KindNotAllowed":                    {secPolicy},
	"KindNotChecked":                    {secRollout},
	"KustomizationNotFound":             {secFlux},
	"KustomizationNotReady":             {secFlux},
	"KustomizationReconciling":          {secFlux},
	"KustomizationSuspended":            {secFlux},
	"KustomizationTransforms":           {secFlux},
	"MergedBeforeVerification":          {secReceipt},
	"MergedWithForeignChange":           {secPullRequest},
	"ModeNotSupported":                  {secPolicy},
	"ModuleNotEnabled":                  {secPolicy, secFreshness},
	"NoChange":                          {secNotRequest},
	"NoFluxLabels":                      {secFlux},
	"NoKustomizationFile":               {secFiles},
	"NoMergeReceipt":                    {secReceipt},
	"NoRecurrence":                      {secAfterMerge},
	"NotAvailable":                      {secAfterMerge},
	"NotInInventory":                    {secFlux},
	"OOMKilled":                         {secAfterMerge},
	"ObjectsAddedOrRemoved":             {secRender},
	"ObservationGap":                    {secAfterMerge},
	"OtherFieldChanged":                 {secRender},
	"OtherObjectChanged":                {secRender},
	"PartialRollout":                    {secAfterMerge},
	"PatchRefused":                      {secPullRequest},
	"PathNotAllowed":                    {secRepository},
	"PlanChanged":                       {secFreshness},
	"PlanIncomplete":                    {secPullRequest},
	"PodNotReady":                       {secAfterMerge},
	"PolicyChanged":                     {secFreshness},
	"PolicyInvalid":                     {secPolicy},
	"PolicyMismatch":                    {secPolicy},
	"PolicyNotFound":                    {secRequest},
	"PreviousValueDiffers":              {secRender},
	"PreviousValueMismatch":             {secRequest},
	"PreviousValueUnknown":              {secNotRequest},
	"PullRequestClosed":                 {secPullRequest},
	"PullRequestDeadline":               {secPullRequest},
	"PullRequestDeadlineCleanupRefused": {secPullRequest},
	"PullRequestOpened":                 {secPullRequest},
	"PullRequestRewriteRefused":         {secPullRequest},
	"RemoteCluster":                     {secFlux},
	"RenderFailed":                      {secRender},
	"ReportMismatch":                    {secAfterMerge},
	"RepositoryNotAllowed":              {secRepository},
	"RepositoryNotConfigured":           {secRepository},
	"RepositoryNotInPolicy":             {secRepository},
	"RepositoryUnavailable":             {secPullRequest},
	"RepositoryUnusable":                {secRepository},
	"RequestNamespaceNotAllowed":        {secPolicy},
	"Restarted":                         {secAfterMerge},
	"ReviewCheckRefused":                {secPullRequest},
	"ReviewListTooLong":                 {secPullRequest},
	"RevisionMismatch":                  {secFlux},
	"RolloutComplete":                   {secRollout},
	"RolloutNotDetermined":              {secRollout},
	"RolloutPartitioned":                {secRollout},
	"RolloutProgressing":                {secRollout},
	"RolloutStuck":                      {secAfterMerge, secRollout},
	"RolloutSuperseded":                 {secAfterMerge, secRollout},
	"ScaledObjectNotReady":              {secPolicy, secFreshness},
	"ScaledObjectReadinessUnknown":      {secPolicy},
	"ScaledToZero":                      {secAfterMerge},
	"SchemaInvalid":                     {secRender},
	"SettingsChanged":                   {secFreshness},
	"SettingsNotFound":                  {secRequest},
	"SourceChanged":                     {secFreshness},
	"SourceIgnored":                     {secRender},
	"SourceIncludes":                    {secFlux},
	"SourceNotBranch":                   {secFlux},
	"SourceNotFound":                    {secFlux},
	"SourceNotReady":                    {secFlux},
	"SourceOptions":                     {secFlux},
	"StaleReport":                       {secAfterMerge},
	"TargetBranchChanged":               {secPullRequest},
	"TargetChanged":                     {secRequest, secFreshness},
	"TargetFileChanged":                 {secFreshness},
	"TargetFileDiverged":                {secPullRequest},
	"TargetGone":                        {secAfterMerge},
	"TargetLabelsNotMatched":            {secPolicy},
	"TargetLocked":                      {secLimits},
	"TargetMismatch":                    {secRequest},
	"TargetNamespaceNotAllowed":         {secPolicy},
	"TargetNotChecked":                  {secAfterMerge},
	"TargetNotFound":                    {secRequest, secRender},
	"TargetReplaced":                    {secAfterMerge},
	"TooLarge":                          {secFiles, secRender},
	"TooManyFiles":                      {secFiles},
	"UnsupportedKustomization":          {secFiles, secRender},
	"UnsupportedSource":                 {secFlux},
	"ValueChanged":                      {secAfterMerge},
	"ValueMismatch":                     {secRequest},
	"VerificationDisabled":              {secAfterMerge},
	"WaitingForReview":                  {secPullRequest},
	"WithinApprovedBounds":              {secRequest},
}

// conditionSections is the section a reason on that condition belongs to when the reason is listed in more
// than one section: a stale plan (PlanFresh) is a freshness reason, not a policy check one.
var conditionSections = map[string]string{
	remediationv1alpha1.ConditionPlanFresh: secFreshness,
	remediationv1alpha1.ConditionQueued:    secLimits,
	conditionRolledOut:                     secRollout,
}

// conditionRolledOut is the executor's rollout condition type (rollout.ConditionRolledOut).
const conditionRolledOut = "RolledOut"

// troubleshootingLink returns the link to the section of the troubleshooting page that lists reason, preferring
// the section of conditionType when the reason is listed in several. A reason the page does not list gets
// the page itself and false.
func troubleshootingLink(conditionType, reason string) (string, bool) {
	sections, ok := troubleshootingSections[reason]
	if !ok {
		return troubleshootingURL, false
	}
	anchor := sections[0]
	if want, ok := conditionSections[conditionType]; ok && slices.Contains(sections, want) {
		anchor = want
	}
	return troubleshootingURL + "#" + anchor, true
}

// terminalPhases are the phases in which nothing more happens to a request: explain names the condition that
// stopped it. Merged and Succeeded are ends, not stops.
var terminalPhases = map[remediationv1alpha1.RemediationPhase]bool{
	remediationv1alpha1.PhaseDenied:            true,
	remediationv1alpha1.PhaseFailed:            true,
	remediationv1alpha1.PhaseExpired:           true,
	remediationv1alpha1.PhaseCancelled:         true,
	remediationv1alpha1.PhaseSuperseded:        true,
	remediationv1alpha1.PhaseNeedsIntervention: true,
	remediationv1alpha1.PhaseInconclusive:      true,
}

// Explanation is what `kubectl shadewright explain` shows. Every string in it that came from the cluster has
// been passed through clean, so the text and the JSON output are both safe to print.
type Explanation struct {
	// Namespace is the namespace read.
	Namespace string `json:"namespace"`
	// Kind is what the name given named: RemediationRequest or Incident.
	Kind string `json:"kind"`
	// Name is the name given.
	Name string `json:"name"`
	// Settings is the cluster-wide RemediationSettings, read for the kill switch and the pull request link.
	Settings SettingsReport `json:"settings"`
	// IncidentState is "ok", or why the Incident is not shown: "notFound", "forbidden", "error", or
	// "uidMismatch" (an Incident of that name exists, but it is not the one the request answers).
	IncidentState string `json:"incidentState"`
	// IncidentError is the reason the Incident could not be read, when IncidentState is "error".
	IncidentError string `json:"incidentError,omitempty"`
	// Incident is the Incident named, or the one the request answers.
	Incident *IncidentReport `json:"incident,omitempty"`
	// Requests is the request named, or the Incident's requests, newest first.
	Requests []RequestExplanation `json:"requests"`
}

// RequestExplanation is one RemediationRequest, explained.
type RequestExplanation struct {
	// Name is the request's name.
	Name string `json:"name"`
	// Phase is status.phase.
	Phase string `json:"phase,omitempty"`
	// PhaseSince is status.lastPhaseTransitionTime.
	PhaseSince *metav1.Time `json:"phaseSince,omitempty"`
	// Created is the request's creation time.
	Created metav1.Time `json:"created"`
	// Stop is the condition that ended the request, set only in a phase that ends it without a merge:
	// Denied, NeedsIntervention, Superseded, Expired, Cancelled or Failed, and in Inconclusive, where it is
	// condition Verified.
	Stop *StopReport `json:"stop,omitempty"`
	// Incident is the name in spec.incidentRef.
	Incident string `json:"incident"`
	// Target is spec.targetRef.
	Target SubjectReport `json:"target"`
	// Action is the typed change with its parameters.
	Action ActionReport `json:"action"`
	// Policy is what the policy check decided.
	Policy PolicyReport `json:"policy"`
	// PlanDigest is status.planDigest.
	PlanDigest string `json:"planDigest,omitempty"`
	// Source is status.source: where the change is made.
	Source *SourceReport `json:"source,omitempty"`
	// PullRequest is status.pullRequest.
	PullRequest *PullRequestReport `json:"pullRequest,omitempty"`
	// ChainRoot is spec.chainRoot, the UID of the first request of a supersede chain.
	ChainRoot string `json:"chainRoot,omitempty"`
	// Replaces is the name in spec.replaces: the request this one replaces.
	Replaces string `json:"replaces,omitempty"`
	// HandoffFrom is status.handoff.from: the request this one took the pull request over from.
	HandoffFrom *HandoffReport `json:"handoffFrom,omitempty"`
	// HandoffTo is status.handoff.to: the request that took the pull request over from this one.
	HandoffTo *HandoffReport `json:"handoffTo,omitempty"`
	// Outcome is the three outcomes of verification after the merge (ADR-0030 section 3): conditions RolledOut,
	// Configured and Cleared. Set once any of them is reported.
	Outcome *OutcomeReport `json:"outcome,omitempty"`
	// Conditions is the condition timeline, oldest first.
	Conditions []TimelineEntry `json:"conditions"`
}

// OutcomeReport is the three outcomes of verification, one condition each; nil when not reported.
type OutcomeReport struct {
	// RolledOut is condition RolledOut: every desired replica runs the applied template.
	RolledOut *ConditionReport `json:"rolledOut,omitempty"`
	// Configured is condition Configured: the live target holds the desired value.
	Configured *ConditionReport `json:"configured,omitempty"`
	// Cleared is condition Cleared: the symptom did not come back over a quiet window.
	Cleared *ConditionReport `json:"cleared,omitempty"`
}

// StopReport is the condition that ended a request, with where the troubleshooting page explains it.
type StopReport struct {
	// Condition is the condition type.
	Condition string `json:"condition"`
	// Status is True, False or Unknown.
	Status string `json:"status"`
	// Reason is the condition's reason.
	Reason string `json:"reason"`
	// Message is the condition's message.
	Message string `json:"message,omitempty"`
	// Link is the section of the troubleshooting page that lists Reason, or the page when no table lists it.
	Link string `json:"link"`
	// Listed says whether a table of the troubleshooting page lists Reason.
	Listed bool `json:"listed"`
}

// ActionReport is the typed change.
type ActionReport struct {
	// Type is the action type.
	Type string `json:"type"`
	// Container is the container a memory limit change is for.
	Container string `json:"container,omitempty"`
	// Previous is the value the change starts from.
	Previous string `json:"previous,omitempty"`
	// Desired is the value the change sets.
	Desired string `json:"desired,omitempty"`
}

// PolicyReport is what the policy check decided.
type PolicyReport struct {
	// Ref is spec.policyRef.
	Ref string `json:"ref"`
	// Revision is status.policyRevision, the policy's name/generation the decision used.
	Revision string `json:"revision,omitempty"`
	// Decision is the PolicyAllowed condition, absent before the check.
	Decision *ConditionReport `json:"decision,omitempty"`
	// ApprovalRequired is status.effectiveApprovalRequired: whether a person must approve the pull request.
	ApprovalRequired *bool `json:"approvalRequired,omitempty"`
}

// SourceReport is where the change is made.
type SourceReport struct {
	// Repository is the RemediationSettings repository entry.
	Repository string `json:"repository"`
	// Branch is the target branch.
	Branch string `json:"branch"`
	// Path is the file that declares the target.
	Path string `json:"path"`
}

// PullRequestReport is the request's pull request.
type PullRequestReport struct {
	// Number is the pull request's number.
	Number int64 `json:"number"`
	// URL is its link, set only when it is on the Git host of the repository the request was resolved to
	// (as in the status command).
	URL string `json:"url,omitempty"`
	// Head is the head commit Shadewright recorded.
	Head string `json:"head,omitempty"`
	// MergeCommit is the merge commit, once merged.
	MergeCommit string `json:"mergeCommit,omitempty"`
}

// HandoffReport is one side of a supersede handoff.
type HandoffReport struct {
	// Name is the other request.
	Name string `json:"name"`
	// Phase is how far this side has come: Reserved, Intent, Done or Aborted.
	Phase string `json:"phase"`
}

// TimelineEntry is one condition.
type TimelineEntry struct {
	// Type is the condition type.
	Type string `json:"type"`
	// Status is True, False or Unknown.
	Status string `json:"status"`
	// Reason is the condition's reason.
	Reason string `json:"reason"`
	// Message is the condition's message.
	Message string `json:"message,omitempty"`
	// Time is the condition's lastTransitionTime; absent when it has none.
	Time *metav1.Time `json:"time,omitempty"`
}

// explain reads, with get and list only, the request or Incident called name in namespace: a request of that
// name first, else an Incident and its requests. It also reads the request's Incident and the
// RemediationSettings; those may fail without failing the command.
func explain(ctx context.Context, c client.Reader, namespace, name string) (*Explanation, error) {
	if errs := validation.IsDNS1123Subdomain(name); len(errs) > 0 {
		return nil, fmt.Errorf("%q is not a valid object name", clean(name, maxName))
	}
	e := &Explanation{Namespace: clean(namespace, maxName), Name: clean(name, maxName), Requests: []RequestExplanation{}}
	settings := readSettings(ctx, c, &e.Settings)

	rr := &remediationv1alpha1.RemediationRequest{}
	err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, rr)
	switch {
	case err == nil:
		e.Kind = kindRequest
		e.Requests = append(e.Requests, explainRequest(rr, settings))
		inc := &remediationv1alpha1.Incident{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: rr.Spec.IncidentRef.Name}, inc); err != nil {
			e.IncidentState, e.IncidentError = readState(err)
			return e, nil
		}
		if remediationv1alpha1.UID(inc.UID) != rr.Spec.IncidentRef.UID {
			e.IncidentState = stateUIDMismatch
			return e, nil
		}
		e.IncidentState = stateOK
		r := incidentReport(inc)
		e.Incident = &r
		return e, nil
	case !apierrors.IsNotFound(err):
		return nil, fmt.Errorf("reading RemediationRequest %s: %w", e.Name, err)
	}

	inc := &remediationv1alpha1.Incident{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, inc); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("no RemediationRequest or Incident %s in namespace %s", e.Name, e.Namespace)
		}
		return nil, fmt.Errorf("reading Incident %s: %w", e.Name, err)
	}
	e.Kind = kindIncident
	e.IncidentState = stateOK
	r := incidentReport(inc)
	e.Incident = &r
	var rrs remediationv1alpha1.RemediationRequestList
	if err := c.List(ctx, &rrs, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("listing RemediationRequests: %w", err)
	}
	// A request belongs to the Incident only when its incidentRef names the Incident's name and UID.
	var own []*remediationv1alpha1.RemediationRequest
	for i := range rrs.Items {
		ref := rrs.Items[i].Spec.IncidentRef
		if ref.Name == inc.Name && ref.UID == remediationv1alpha1.UID(inc.UID) {
			own = append(own, &rrs.Items[i])
		}
	}
	slices.SortFunc(own, func(a, b *remediationv1alpha1.RemediationRequest) int {
		return cmp.Or(b.CreationTimestamp.Compare(a.CreationTimestamp.Time), cmp.Compare(a.Name, b.Name))
	})
	for _, rr := range own {
		e.Requests = append(e.Requests, explainRequest(rr, settings))
	}
	return e, nil
}

// explainRequest takes the fields of one request explain shows, each string cleaned.
func explainRequest(rr *remediationv1alpha1.RemediationRequest,
	settings *remediationv1alpha1.RemediationSettings) RequestExplanation {
	st := &rr.Status
	r := RequestExplanation{
		Name:     clean(rr.Name, maxName),
		Phase:    clean(string(st.Phase), maxName),
		Created:  rr.CreationTimestamp,
		Incident: clean(rr.Spec.IncidentRef.Name, maxName),
		Target: SubjectReport{
			Kind:      clean(rr.Spec.TargetRef.Kind, maxName),
			Namespace: clean(rr.Spec.TargetRef.Namespace, maxName),
			Name:      clean(rr.Spec.TargetRef.Name, maxName),
		},
		Action: actionReport(rr.Spec.Action),
		Policy: PolicyReport{
			Ref:              clean(string(rr.Spec.PolicyRef), maxName),
			Revision:         clean(st.PolicyRevision, maxName),
			Decision:         conditionReport(st.Conditions, remediationv1alpha1.ConditionPolicyAllowed),
			ApprovalRequired: st.EffectiveApprovalRequired,
		},
		PlanDigest: clean(st.PlanDigest, maxName),
		ChainRoot:  clean(string(rr.Spec.ChainRoot), maxName),
		Conditions: timeline(st.Conditions),
	}
	if o := (OutcomeReport{RolledOut: conditionReport(st.Conditions, conditionRolledOut),
		Configured: conditionReport(st.Conditions, remediationv1alpha1.ConditionConfigured),
		Cleared:    conditionReport(st.Conditions, remediationv1alpha1.ConditionCleared)}); o != (OutcomeReport{}) {
		r.Outcome = &o
	}
	if st.LastPhaseTransitionTime != nil && !st.LastPhaseTransitionTime.IsZero() {
		r.PhaseSince = &metav1.Time{Time: st.LastPhaseTransitionTime.Time}
	}
	if terminalPhases[st.Phase] {
		conds := st.Conditions
		if st.Phase == remediationv1alpha1.PhaseInconclusive {
			// Merged, so Approved is True; Verified carries why nothing could be shown (ADR-0030 section 5).
			for _, c := range conds {
				if c.Type == remediationv1alpha1.ConditionVerified {
					conds = []metav1.Condition{c}
					break
				}
			}
		}
		r.Stop = stopReport(conds)
	}
	if s := st.Source; s != nil {
		r.Source = &SourceReport{
			Repository: clean(string(s.RepositoryRef), maxName),
			Branch:     clean(string(s.TargetBranch), maxName),
			Path:       clean(string(s.Path), maxMessage),
		}
	}
	if pr := st.PullRequest; pr != nil && pr.Number > 0 {
		r.PullRequest = &PullRequestReport{
			Number:      pr.Number,
			URL:         prLink(rr, settings),
			Head:        clean(pr.HeadSHA, maxName),
			MergeCommit: clean(pr.MergeCommitSHA, maxName),
		}
	}
	if rep := rr.Spec.Replaces; rep != nil {
		r.Replaces = clean(string(rep.Name), maxName)
	}
	if h := st.Handoff; h != nil {
		if h.From != nil {
			r.HandoffFrom = &HandoffReport{Name: clean(string(h.From.Name), maxName),
				Phase: clean(string(h.From.Phase), maxName)}
		}
		if h.To != nil {
			r.HandoffTo = &HandoffReport{Name: clean(string(h.To.Name), maxName),
				Phase: clean(string(h.To.Phase), maxName)}
		}
	}
	return r
}

// actionReport takes the action type and the parameters of the catalog action it names.
func actionReport(a remediationv1alpha1.Action) ActionReport {
	r := ActionReport{Type: clean(string(a.Type), maxName)}
	switch {
	case a.SetContainerMemoryLimit != nil:
		p := a.SetContainerMemoryLimit
		r.Container = clean(p.Container, maxName)
		r.Previous = clean(p.Previous.String(), maxName)
		r.Desired = clean(p.Desired.String(), maxName)
	case a.AdjustKedaMaxReplicas != nil:
		r.Previous = strconv.Itoa(int(a.AdjustKedaMaxReplicas.PreviousValue))
		r.Desired = strconv.Itoa(int(a.AdjustKedaMaxReplicas.DesiredValue))
	}
	return r
}

// timeline returns the conditions, cleaned, oldest first; a condition without a time comes first.
func timeline(conds []metav1.Condition) []TimelineEntry {
	sorted := slices.Clone(conds)
	slices.SortStableFunc(sorted, func(a, b metav1.Condition) int {
		return a.LastTransitionTime.Compare(b.LastTransitionTime.Time)
	})
	out := make([]TimelineEntry, 0, len(sorted))
	for _, c := range sorted {
		out = append(out, TimelineEntry{
			Type:    clean(c.Type, maxName),
			Status:  clean(string(c.Status), maxName),
			Reason:  clean(c.Reason, maxName),
			Message: clean(c.Message, maxMessage),
			Time:    nonZero(c.LastTransitionTime),
		})
	}
	return out
}

// stopReport picks the condition that ended a request: the most recent one that is not True, leaving out
// WritesPaused and Queued, which only hold a request back; when every condition is True (or none is left), the
// most recent condition. It returns nil when the request has no conditions.
func stopReport(conds []metav1.Condition) *StopReport {
	// The newest False condition names the stop; an Unknown one (a wait, such as Approved WaitingForReview) only
	// when none is False, and the newest condition when none is either.
	var falsePick, unknownPick, latest *metav1.Condition
	newer := func(c, than *metav1.Condition) bool {
		return than == nil || c.LastTransitionTime.After(than.LastTransitionTime.Time)
	}
	for i := range conds {
		c := &conds[i]
		if c.Type == remediationv1alpha1.ConditionWritesPaused || c.Type == remediationv1alpha1.ConditionQueued {
			continue
		}
		if newer(c, latest) {
			latest = c
		}
		switch {
		case c.Status == metav1.ConditionFalse && newer(c, falsePick):
			falsePick = c
		case c.Status == metav1.ConditionUnknown && newer(c, unknownPick):
			unknownPick = c
		}
	}
	pick := falsePick
	if pick == nil {
		pick = unknownPick
	}
	if pick == nil {
		pick = latest
	}
	if pick == nil {
		return nil
	}
	link, listed := troubleshootingLink(pick.Type, pick.Reason)
	return &StopReport{
		Condition: clean(pick.Type, maxName),
		Status:    clean(string(pick.Status), maxName),
		Reason:    clean(pick.Reason, maxName),
		Message:   clean(pick.Message, maxMessage),
		Link:      link,
		Listed:    listed,
	}
}

// renderExplainText writes the explanation for a person.
func renderExplainText(w io.Writer, e *Explanation, now time.Time) error {
	p := &printer{w: w}
	switch e.Kind {
	case kindIncident:
		p.line("Incident %s in namespace %s", e.Name, e.Namespace)
		if e.Incident != nil {
			incidentLines(p, e.Incident, now)
		}
		if len(e.Requests) == 0 {
			p.line("  No RemediationRequests name this Incident.")
		} else {
			p.line("  Requests, newest first: %d", len(e.Requests))
		}
		for i := range e.Requests {
			p.line("")
			requestLines(p, e, &e.Requests[i], now)
		}
	default:
		for i := range e.Requests {
			requestLines(p, e, &e.Requests[i], now)
		}
	}
	p.line("")
	p.line("Kill switch:  %s", killSwitch(e.Settings))
	return p.err
}

func incidentLines(p *printer, inc *IncidentReport, now time.Time) {
	p.line("  Subject:      %s", subject(inc.Subject))
	p.line("  Seen:         %s, count %d, last seen %s", inc.Reason, inc.Count, ago(inc.LastSeen, now))
	p.line("  Phase:        %s", orNone(inc.Phase))
	p.line("  Analysis:     %s", analysisLine(*inc, now))
}

func requestLines(p *printer, e *Explanation, r *RequestExplanation, now time.Time) {
	p.line("Request %s in namespace %s", r.Name, e.Namespace)
	phase := orNone(r.Phase)
	if r.PhaseSince != nil {
		phase += ", since " + ago(r.PhaseSince, now)
	}
	p.line("  Phase:        %s", phase)
	if s := r.Stop; s != nil {
		line := s.Condition + " " + s.Status + ", " + s.Reason
		if s.Message != "" {
			line += ": " + s.Message
		}
		p.line("  Stopped by:   %s", line)
		if s.Listed {
			p.line("  Read more:    %s", s.Link)
		} else {
			p.line("  Read more:    %s (this reason is not listed there)", s.Link)
		}
	}
	if e.Kind == kindRequest {
		p.line("  Incident:     %s", incidentSummary(e, r))
	}
	p.line("  Action:       %s", actionLine(r))
	p.line("  Policy:       %s", policyLine(r.Policy))
	if r.PlanDigest != "" {
		p.line("  Plan digest:  %s", r.PlanDigest)
	}
	if s := r.Source; s != nil {
		p.line("  Source:       repository %s, branch %s, file %s", s.Repository, s.Branch, s.Path)
	}
	if pr := r.PullRequest; pr != nil {
		p.line("  Pull request: %s", pullRequestLine(pr))
	}
	if o := r.Outcome; o != nil {
		p.line("  Rolled out:   %s", outcomeLine(o.RolledOut))
		p.line("  Configured:   %s", outcomeLine(o.Configured))
		p.line("  Cleared:      %s", outcomeLine(o.Cleared))
	}
	if r.Replaces != "" || r.HandoffFrom != nil {
		line := r.Replaces
		if h := r.HandoffFrom; h != nil {
			line = h.Name + " (handoff " + h.Phase + ")"
		}
		p.line("  Replaces:     %s", line)
	}
	if h := r.HandoffTo; h != nil {
		p.line("  Replaced by:  %s (handoff %s)", h.Name, h.Phase)
	}
	if r.ChainRoot != "" {
		p.line("  Chain root:   %s", r.ChainRoot)
	}
	if len(r.Conditions) == 0 {
		p.line("  Conditions:   none yet")
		return
	}
	p.line("  Conditions, oldest first:")
	for _, c := range r.Conditions {
		line := fmt.Sprintf("    %-10s %s=%s %s", ago(c.Time, now), c.Type, c.Status, c.Reason)
		if c.Message != "" {
			line += ": " + c.Message
		}
		p.line("%s", line)
	}
}

// outcomeLine is one outcome line: the condition's status and reason, and its message when it has one.
func outcomeLine(c *ConditionReport) string {
	if c == nil {
		return "not reported yet"
	}
	line := c.Status + ", " + c.Reason
	if c.Message != "" {
		line += ": " + c.Message
	}
	return line
}

func incidentSummary(e *Explanation, r *RequestExplanation) string {
	switch e.IncidentState {
	case stateOK:
		inc := e.Incident
		return fmt.Sprintf("%s (%s, %s, phase %s)", inc.Name, subject(inc.Subject), inc.Reason, orNone(inc.Phase))
	case stateUIDMismatch:
		return r.Incident + " (not shown: an Incident of that name exists, but it is not the one this request answers)"
	}
	return r.Incident + " (" + readFailure(e.IncidentState, e.IncidentError) + ")"
}

func actionLine(r *RequestExplanation) string {
	a := r.Action
	s := a.Type + " on " + subject(r.Target)
	if a.Container != "" {
		s += ", container " + a.Container
	}
	if a.Previous != "" || a.Desired != "" {
		s += ", " + orNone(a.Previous) + " to " + orNone(a.Desired)
	}
	return s
}

func policyLine(p PolicyReport) string {
	s := "not checked yet"
	if d := p.Decision; d != nil {
		s = d.Reason
		if d.Message != "" {
			s += ": " + d.Message
		}
	}
	s += " (policy " + p.Ref
	if p.Revision != "" {
		s += ", revision " + p.Revision
	}
	s += ")"
	if p.ApprovalRequired != nil {
		if *p.ApprovalRequired {
			s += ", approval required"
		} else {
			s += ", approval not required"
		}
	}
	return s
}

func pullRequestLine(pr *PullRequestReport) string {
	parts := []string{}
	if pr.URL != "" {
		parts = append(parts, pr.URL)
	} else {
		parts = append(parts, fmt.Sprintf("#%d (link not shown: not on the configured Git repository)", pr.Number))
	}
	if pr.Head != "" {
		parts = append(parts, "head "+pr.Head)
	}
	if pr.MergeCommit != "" {
		parts = append(parts, "merge commit "+pr.MergeCommit)
	}
	return strings.Join(parts, ", ")
}
