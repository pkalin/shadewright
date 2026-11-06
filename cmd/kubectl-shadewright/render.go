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
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/duration"
)

// renderJSON writes the report as indented JSON. Its strings were cleaned in collect; the encoder also
// escapes what remains of HTML and the line separators.
func renderJSON(w io.Writer, r *Report) error {
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
		return "unknown (RemediationSettings: " + readFailure(s.State, s.Error) + ")"
	case s.Paused != nil && *s.Paused:
		return "ON: writes are paused (spec.paused)"
	}
	return "off: writes are allowed"
}

func budget(b BudgetReport, s SettingsReport) string {
	limit := "unknown limit"
	if s.MaxAnalysesPerDay != nil {
		limit = strconv.Itoa(*s.MaxAnalysesPerDay)
	}
	switch b.State {
	case stateOK:
		return fmt.Sprintf("%d of %s", *b.UsedToday, limit)
	case stateUnreadable:
		return "budget ConfigMap not trusted by the analyzer, taken as spent"
	}
	return "unknown (budget ConfigMap: " + readFailure(b.State, b.Error) + ")"
}

func readFailure(state, msg string) string {
	switch state {
	case stateNotFound:
		return "not found"
	case stateForbidden:
		return "forbidden, see the viewer role"
	}
	return "read failed: " + msg
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
	return s + " (since " + ago(&c.Since, now) + ")"
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
