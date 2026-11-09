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
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/duration"

	"github.com/pkalin/shadewright/internal/incidents"
)

// renderJSON writes a report or an explanation as indented JSON. Its strings were cleaned in collect; the encoder also
// escapes what remains of HTML and the line separators.
func renderJSON(w io.Writer, r any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// renderText writes the report for a person: the settings, then per namespace the budget, each Incident with
// its due reason, last analysis and requests, and the requests no listed Incident claims.
func renderText(w io.Writer, r *Report, now time.Time) error {
	p := &printer{w: w}
	p.line("Kill switch:  %s", killSwitch(r.Settings))
	for _, ns := range r.Namespaces {
		p.line("")
		p.line("Namespace %s", ns.Namespace)
		p.line("  Analyses today (UTC): %s", budget(ns.Budget, r.Settings))
		if len(ns.Incidents) == 0 {
			p.line("  No Incidents.")
			if ns.Budget.AnalysisStopped == incidents.ReasonBudgetNotFound && len(ns.OtherRequests) == 0 {
				// The likeliest cause of both is a namespace that is not the operator namespace.
				p.line("  Is this the operator namespace? Pass it with -n.")
			}
		}
		for _, inc := range ns.Incidents {
			p.line("")
			p.line("  Incident %s", inc.Name)
			p.line("    Subject:   %s", subject(inc.Subject))
			p.line("    Seen:      %s, count %d, last seen %s", inc.Reason, inc.Count, ago(inc.LastSeen, now))
			if inc.Symptom != "" {
				p.line("    Symptom:   %s", inc.Symptom)
			}
			p.line("    Phase:     %s", orNone(inc.Phase))
			p.line("    Due:       %s", condition(inc.Due, now))
			p.line("    Analysis:  %s", analysisLine(inc, now))
			for _, rr := range inc.Requests {
				p.line("    Request:   %s", request(rr, now))
			}
		}
		if len(ns.OtherRequests) > 0 {
			p.line("")
			p.line("  Requests without a listed Incident:")
			for _, rr := range ns.OtherRequests {
				p.line("    %s (Incident %s)", request(rr, now), rr.Incident)
			}
		}
	}
	return p.err
}

// printer writes lines and keeps the first error.
type printer struct {
	w   io.Writer
	err error
}

func (p *printer) line(format string, args ...any) {
	if p.err == nil {
		_, p.err = fmt.Fprintf(p.w, format+"\n", args...)
	}
}

func killSwitch(s SettingsReport) string {
	switch {
	case s.State != stateOK:
		return "unknown (RemediationSettings " + readFailure(s.State, s.Error) + ")"
	case s.Paused != nil && *s.Paused:
		return "ON: writes are paused (spec.paused)"
	}
	return "off: writes are allowed"
}

// stoppedWords say in words why the analyzer refuses every analysis, by its AnalysisDue reason.
var stoppedWords = map[string]string{
	incidents.ReasonSettingsNotFound:   "the RemediationSettings object default does not exist",
	incidents.ReasonSettingsIncomplete: "RemediationSettings sets no spec.analysis.maxAnalysesPerDay",
	incidents.ReasonBudgetNotFound:     "the ConfigMap " + incidents.BudgetName + " does not exist; recreate it empty",
	incidents.ReasonBudgetUnreadable: "the ConfigMap " + incidents.BudgetName +
		" holds content the analyzer does not trust, so the budget is taken as spent",
	incidents.ReasonAnalysisDisabled:   "the daily limit is 0",
	incidents.ReasonDailyBudgetReached: "the daily limit is reached; it resets at 00:00 UTC",
}

func budget(b BudgetReport, s SettingsReport) string {
	used := ""
	if b.State == stateOK && b.UsedToday != nil {
		limit := "an unknown limit"
		if s.MaxAnalysesPerDay != nil {
			limit = strconv.Itoa(*s.MaxAnalysesPerDay)
		}
		used = fmt.Sprintf("%d of %s", *b.UsedToday, limit)
	}
	if b.AnalysisStopped != "" {
		out := "analysis stopped: " + stoppedWords[b.AnalysisStopped] + " (" + b.AnalysisStopped + ")"
		if used != "" {
			out = used + ", " + out
		}
		return out
	}
	var unknown []string
	if s.State != stateOK {
		unknown = append(unknown, "RemediationSettings "+readFailure(s.State, s.Error))
	}
	if b.State != stateOK {
		unknown = append(unknown, "ConfigMap "+incidents.BudgetName+" "+readFailure(b.State, b.Error))
	}
	if len(unknown) > 0 {
		prefix := "unknown"
		if used != "" {
			prefix = used + ", but whether analysis may run is unknown"
		}
		return prefix + " (" + strings.Join(unknown, "; ") + ")"
	}
	return used
}

// readFailure says why the plugin could not read an object. Forbidden and read errors are the plugin's own
// reads, with the caller's permissions, not the analyzer's.
func readFailure(state, msg string) string {
	switch state {
	case stateNotFound:
		return "not found"
	case stateForbidden:
		return "not readable with your permissions (see the viewer role)"
	}
	return "could not be read: " + msg
}

func subject(s SubjectReport) string {
	if s.Namespace == "" {
		return s.Kind + " " + s.Name
	}
	return s.Kind + " " + s.Namespace + "/" + s.Name
}

func condition(c *ConditionReport, now time.Time) string {
	if c == nil {
		return "not set yet"
	}
	verdict := map[string]string{"True": "yes", "False": "no"}[c.Status]
	if verdict == "" {
		verdict = c.Status
	}
	s := verdict + ", " + c.Reason
	if c.Message != "" {
		s += ": " + c.Message
	}
	if c.Since == nil {
		return s
	}
	return s + " (since " + ago(c.Since, now) + ")"
}

func analysisLine(inc IncidentReport, now time.Time) string {
	if inc.LastAnalysisTime == nil {
		return "never"
	}
	s := "began " + ago(inc.LastAnalysisTime, now)
	if inc.Outcome == nil {
		return s + ", no outcome recorded"
	}
	s += ", outcome " + inc.Outcome.Reason
	if inc.Outcome.Message != "" {
		s += ": " + inc.Outcome.Message
	}
	return s
}

func request(rr RequestReport, now time.Time) string {
	s := fmt.Sprintf("%s  %s  %s on %s", rr.Name, orNone(rr.Phase), rr.Action, rr.Target)
	if rr.Policy != "" {
		s += ", policy " + rr.Policy
	}
	switch {
	case rr.PullRequestURL != "":
		s += ", PR " + rr.PullRequestURL
	case rr.PullRequest > 0:
		s += fmt.Sprintf(", PR #%d (link not shown: not on the configured Git repository)", rr.PullRequest)
	}
	return s + ", created " + ago(&rr.Created, now)
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

// ago prints a time as kubectl prints an age.
func ago(t *metav1.Time, now time.Time) string {
	if t == nil || t.IsZero() {
		return "never"
	}
	return duration.HumanDuration(now.Sub(t.Time)) + " ago"
}
