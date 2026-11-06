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
	"net/url"
	"slices"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/incidents"
)

// conditionAnalyzed is the Incident condition the analysis writes with its outcome (analysis.ConditionAnalyzed;
// a test keeps the two equal). The plugin does not import the analysis package, which carries the model adapters.
const conditionAnalyzed = "Analyzed"

// States of a read that may fail without failing the command: the settings and the budget ConfigMap.
const (
	stateOK         = "ok"
	stateNotFound   = "notFound"
	stateForbidden  = "forbidden"
	stateUnreadable = "unreadable"
	stateError      = "error"
)

// Report is what `kubectl shadewright status` shows. Every string in it that came from the cluster has been
// passed through clean, in collect, so the text and the JSON output are both safe to print.
type Report struct {
	// Settings is the cluster-wide RemediationSettings: the kill switch and the daily analysis limit.
	Settings SettingsReport `json:"settings"`
	// Namespaces are the operator namespaces read, each with its Incidents, requests and budget.
	Namespaces []NamespaceReport `json:"namespaces"`
}

// SettingsReport is the part of RemediationSettings the status shows.
type SettingsReport struct {
	// State is "ok", or why the settings could not be read: "notFound", "forbidden" or "error".
	State string `json:"state"`
	// Error is the reason the settings could not be read, when State is "error".
	Error string `json:"error,omitempty"`
	// Paused is the kill switch (spec.paused): true stops every Git write.
	Paused *bool `json:"paused,omitempty"`
	// MaxAnalysesPerDay is the daily analysis limit the analyzer applies.
	MaxAnalysesPerDay *int `json:"maxAnalysesPerDay,omitempty"`
}

// NamespaceReport is one operator namespace.
type NamespaceReport struct {
	// Namespace is the namespace name.
	Namespace string `json:"namespace"`
	// Budget is the day's use of the analysis budget in this namespace.
	Budget BudgetReport `json:"budget"`
	// Incidents are the Incidents of the namespace, most recently seen first.
	Incidents []IncidentReport `json:"incidents"`
	// OtherRequests are requests whose Incident is not among Incidents (deleted, or written by hand).
	OtherRequests []RequestReport `json:"otherRequests,omitempty"`
}

// BudgetReport is the analysis budget ConfigMap, read as the analyzer reads it.
type BudgetReport struct {
	// State is "ok", or why the count is not known: "notFound", "forbidden", "unreadable" (content the
	// analyzer does not trust, so it takes the budget as spent) or "error".
	State string `json:"state"`
	// Error is the reason the ConfigMap could not be read, when State is "error".
	Error string `json:"error,omitempty"`
	// UsedToday is the analyses started on the current UTC day.
	UsedToday *int `json:"usedToday,omitempty"`
}

// IncidentReport is one Incident.
type IncidentReport struct {
	// Name is the Incident's name.
	Name string `json:"name"`
	// Subject is the object the symptoms are correlated to.
	Subject SubjectReport `json:"subject"`
	// Reason is the symptom's reason, such as OOMKilled.
	Reason string `json:"reason"`
	// Symptom is spec.symptom, the normalized and redacted description.
	Symptom string `json:"symptom,omitempty"`
	// Phase is status.phase.
	Phase string `json:"phase,omitempty"`
	// Count is status.count.
	Count int64 `json:"count"`
	// LastSeen is status.lastSeen.
	LastSeen *metav1.Time `json:"lastSeen,omitempty"`
	// Due is the AnalysisDue condition: whether the Incident is due for analysis, and why.
	Due *ConditionReport `json:"due,omitempty"`
	// LastAnalysisTime is when the last analysis began.
	LastAnalysisTime *metav1.Time `json:"lastAnalysisTime,omitempty"`
	// Outcome is the Analyzed condition: the outcome of the last analysis that finished.
	Outcome *ConditionReport `json:"outcome,omitempty"`
	// Requests are the RemediationRequests that name this Incident, oldest first.
	Requests []RequestReport `json:"requests,omitempty"`
}

// SubjectReport names an Incident's subject.
type SubjectReport struct {
	// Kind of the subject.
	Kind string `json:"kind"`
	// Namespace of the subject.
	Namespace string `json:"namespace,omitempty"`
	// Name of the subject.
	Name string `json:"name"`
}

// ConditionReport is one condition.
type ConditionReport struct {
	// Status is True, False or Unknown.
	Status string `json:"status"`
	// Reason is the condition's reason.
	Reason string `json:"reason"`
	// Message is the condition's message.
	Message string `json:"message,omitempty"`
	// Since is the condition's lastTransitionTime.
	Since metav1.Time `json:"since"`
}

// RequestReport is one RemediationRequest.
type RequestReport struct {
	// Name is the request's name.
	Name string `json:"name"`
	// Incident is the name of the Incident it answers.
	Incident string `json:"incident"`
	// Phase is status.phase.
	Phase string `json:"phase,omitempty"`
	// Action is the action type.
	Action string `json:"action"`
	// Target is the target object, as kind/name.
	Target string `json:"target"`
	// Policy is the reason of the PolicyAllowed condition: the policy decision.
	Policy string `json:"policy,omitempty"`
	// PullRequest is the number of the pull request, 0 before one is recorded.
	PullRequest int64 `json:"pullRequest,omitempty"`
	// PullRequestURL is the link to the pull request, set only when it is on the Git host of the repository
	// the request was resolved to in RemediationSettings.
	PullRequestURL string `json:"pullRequestURL,omitempty"`
	// Created is the request's creation time.
	Created metav1.Time `json:"created"`
}

// Options select what collect reads.
type Options struct {
	// Namespace is the operator namespace; ignored with AllNamespaces.
	Namespace string
	// AllNamespaces reads Incidents and requests in every namespace.
	AllNamespaces bool
}

// collect reads the status with get and list only: the Incidents and RemediationRequests of the namespace (or
// of all namespaces), the RemediationSettings and, per namespace, the analysis budget ConfigMap. An Incident or
// request list that fails is an error; settings or a budget that cannot be read are reported in the result.
func collect(ctx context.Context, c client.Reader, opts Options, now time.Time) (*Report, error) {
	var listOpts []client.ListOption
	if !opts.AllNamespaces {
		listOpts = append(listOpts, client.InNamespace(opts.Namespace))
	}
	var incs remediationv1alpha1.IncidentList
	if err := c.List(ctx, &incs, listOpts...); err != nil {
		return nil, fmt.Errorf("listing Incidents: %w", err)
	}
	var rrs remediationv1alpha1.RemediationRequestList
	if err := c.List(ctx, &rrs, listOpts...); err != nil {
		return nil, fmt.Errorf("listing RemediationRequests: %w", err)
	}

	report := &Report{}
	settings := readSettings(ctx, c, &report.Settings)

	byNamespace := map[string]*NamespaceReport{}
	ns := func(name string) *NamespaceReport {
		if r, ok := byNamespace[name]; ok {
			return r
		}
		r := &NamespaceReport{Namespace: clean(name, maxName), Incidents: []IncidentReport{}}
		byNamespace[name] = r
		return r
	}
	if !opts.AllNamespaces {
		ns(opts.Namespace)
	}

	// A request is listed under an Incident only when its incidentRef names that Incident's name and UID.
	type key struct {
		namespace, name string
		uid             remediationv1alpha1.UID
	}
	index := map[key]int{}
	for i := range incs.Items {
		inc := &incs.Items[i]
		r := ns(inc.Namespace)
		index[key{inc.Namespace, inc.Name, remediationv1alpha1.UID(inc.UID)}] = len(r.Incidents)
		r.Incidents = append(r.Incidents, incidentReport(inc))
	}
	slices.SortFunc(rrs.Items, func(a, b remediationv1alpha1.RemediationRequest) int {
		return cmp.Or(a.CreationTimestamp.Compare(b.CreationTimestamp.Time), cmp.Compare(a.Name, b.Name))
	})
	for i := range rrs.Items {
		rr := &rrs.Items[i]
		r := ns(rr.Namespace)
		req := requestReport(rr, settings)
		if j, ok := index[key{rr.Namespace, rr.Spec.IncidentRef.Name, rr.Spec.IncidentRef.UID}]; ok {
			r.Incidents[j].Requests = append(r.Incidents[j].Requests, req)
			continue
		}
		r.OtherRequests = append(r.OtherRequests, req)
	}

	for name, r := range byNamespace {
		r.Budget = readBudget(ctx, c, name, now)
		slices.SortStableFunc(r.Incidents, func(a, b IncidentReport) int {
			return cmp.Or(compareTimeDesc(a.LastSeen, b.LastSeen), cmp.Compare(a.Name, b.Name))
		})
		report.Namespaces = append(report.Namespaces, *r)
	}
	slices.SortFunc(report.Namespaces, func(a, b NamespaceReport) int { return cmp.Compare(a.Namespace, b.Namespace) })
	return report, nil
}

// compareTimeDesc orders newer times first and absent times last.
func compareTimeDesc(a, b *metav1.Time) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return 1
	case b == nil:
		return -1
	}
	return b.Compare(a.Time)
}

// incidentReport takes the fields of one Incident the status shows, each string cleaned.
func incidentReport(inc *remediationv1alpha1.Incident) IncidentReport {
	return IncidentReport{
		Name: clean(inc.Name, maxName),
		Subject: SubjectReport{
			Kind:      clean(inc.Spec.Subject.Kind, maxName),
			Namespace: clean(inc.Spec.Subject.Namespace, maxName),
			Name:      clean(inc.Spec.Subject.Name, maxName),
		},
		Reason:           clean(inc.Spec.Reason, maxName),
		Symptom:          clean(inc.Spec.Symptom, maxMessage),
		Phase:            clean(string(inc.Status.Phase), maxName),
		Count:            inc.Status.Count,
		LastSeen:         inc.Status.LastSeen,
		Due:              conditionReport(inc.Status.Conditions, incidents.ConditionAnalysisDue),
		LastAnalysisTime: inc.Status.LastAnalysisTime,
		Outcome:          conditionReport(inc.Status.Conditions, conditionAnalyzed),
	}
}

// conditionReport returns the condition of type t, cleaned, or nil.
func conditionReport(conds []metav1.Condition, t string) *ConditionReport {
	c := meta.FindStatusCondition(conds, t)
	if c == nil {
		return nil
	}
	return &ConditionReport{
		Status:  clean(string(c.Status), maxName),
		Reason:  clean(c.Reason, maxName),
		Message: clean(c.Message, maxMessage),
		Since:   c.LastTransitionTime,
	}
}

// requestReport takes the fields of one request the status shows, each string cleaned. The pull request link
// is set only when prLink accepts it.
func requestReport(rr *remediationv1alpha1.RemediationRequest,
	settings *remediationv1alpha1.RemediationSettings) RequestReport {
	r := RequestReport{
		Name:     clean(rr.Name, maxName),
		Incident: clean(rr.Spec.IncidentRef.Name, maxName),
		Phase:    clean(string(rr.Status.Phase), maxName),
		Action:   clean(string(rr.Spec.Action.Type), maxName),
		Target:   clean(rr.Spec.TargetRef.Kind+"/"+rr.Spec.TargetRef.Name, maxName),
		Created:  rr.CreationTimestamp,
	}
	if c := meta.FindStatusCondition(rr.Status.Conditions, remediationv1alpha1.ConditionPolicyAllowed); c != nil {
		r.Policy = clean(c.Reason, maxName)
	}
	if pr := rr.Status.PullRequest; pr != nil && pr.Number > 0 {
		r.PullRequest = pr.Number
		r.PullRequestURL = prLink(rr, settings)
	}
	return r
}

// prLink returns the pull request's URL only when it is exactly the one the Git host gives a pull request of
// that number in the repository the request was resolved to: the RemediationSettings entry named by
// status.source.repositoryRef, a GitHub repository, its URL an https URL without credentials, query or
// fragment, and status.pullRequest.url equal to that URL, "/pull/" and the number. Anything else, such as a
// status written by someone other than the executor, a settings entry that changed since or another
// provider, gives "" and only the number is printed.
func prLink(rr *remediationv1alpha1.RemediationRequest, settings *remediationv1alpha1.RemediationSettings) string {
	pr, src := rr.Status.PullRequest, rr.Status.Source
	if settings == nil || pr == nil || src == nil || pr.Number < 1 {
		return ""
	}
	for _, repo := range settings.Spec.Repositories {
		if repo.Name != src.RepositoryRef {
			continue
		}
		if repo.Provider != remediationv1alpha1.GitProviderGitHub {
			return ""
		}
		u, err := url.Parse(repo.URL)
		if err != nil || u.Scheme != "https" || u.User != nil || u.Host == "" || u.RawQuery != "" ||
			u.Fragment != "" || u.Opaque != "" {
			return ""
		}
		// The CRD rule for a GitHub entry already pins the URL to https://github.com/<owner>/<repo>, set by
		// whoever may edit RemediationSettings (cluster-wide); the check here does not trust that alone.
		want := repo.URL + "/pull/" + strconv.FormatInt(pr.Number, 10)
		if pr.URL != want {
			return ""
		}
		// The settings URL passed the CRD's pattern; clean it anyway, and drop it if that changed anything.
		if clean(want, 2048) != want {
			return ""
		}
		return want
	}
	return ""
}

// readSettings reads the RemediationSettings into r and returns it, or nil when it could not be read.
func readSettings(ctx context.Context, c client.Reader, r *SettingsReport) *remediationv1alpha1.RemediationSettings {
	s := &remediationv1alpha1.RemediationSettings{}
	if err := c.Get(ctx, client.ObjectKey{Name: remediationv1alpha1.SettingsName}, s); err != nil {
		r.State, r.Error = readState(err)
		return nil
	}
	r.State = stateOK
	paused := s.Spec.Paused != nil && *s.Spec.Paused
	r.Paused = &paused
	if limit, ok := maxAnalysesPerDay(s); ok {
		r.MaxAnalysesPerDay = &limit
	}
	return s
}

// maxAnalysesPerDay is the limit the analyzer applies (internal/incidents, maxAnalysesPerDay):
// spec.analysis.maxAnalysesPerDay, or the deprecated spec.maxAnalysesPerDay when that is set and lower. ok
// is false when spec.analysis.maxAnalysesPerDay is unset, which the analyzer takes as an unknown budget.
func maxAnalysesPerDay(s *remediationv1alpha1.RemediationSettings) (int, bool) {
	a := s.Spec.Analysis
	if a == nil || a.MaxAnalysesPerDay == nil {
		return 0, false
	}
	limit := int(*a.MaxAnalysesPerDay)
	if old := s.Spec.MaxAnalysesPerDay; old != nil {
		limit = min(limit, int(*old))
	}
	return limit, true
}

// readBudget reads the analysis budget ConfigMap of namespace with the analyzer's own parser.
func readBudget(ctx context.Context, c client.Reader, namespace string, now time.Time) BudgetReport {
	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: incidents.BudgetName}, cm); err != nil {
		state, msg := readState(err)
		return BudgetReport{State: state, Error: msg}
	}
	used, ok := incidents.UsedToday(cm, now)
	if !ok {
		return BudgetReport{State: stateUnreadable}
	}
	return BudgetReport{State: stateOK, UsedToday: &used}
}

// readState names why a get failed. Only an unexpected error keeps its text, cleaned.
func readState(err error) (string, string) {
	switch {
	case apierrors.IsNotFound(err):
		return stateNotFound, ""
	case apierrors.IsForbidden(err):
		return stateForbidden, ""
	}
	return stateError, clean(err.Error(), maxMessage)
}
