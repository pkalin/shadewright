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

package kyverno

import (
	"slices"
	"strings"
	"testing"

	"github.com/pkalin/shadewright/internal/modules"
)

// active returns a selection with kyverno enabled and detected.
func active(t *testing.T) modules.Selection {
	t.Helper()
	r, err := modules.New(nil, []modules.Integration{Module()})
	if err != nil {
		t.Fatal(err)
	}
	d := r.Detect(nil, []string{PolicyGroup + "/" + PolicyVersion})
	return r.Select(modules.Enabled{Integrations: []string{Name}}, d)
}

// recordedEvent reads the event fixture into what the collector passes to a rule.
func recordedEvent(t *testing.T) modules.Event {
	t.Helper()
	obj := fixture(t, "event-admission-refused.json")
	meta := obj["metadata"].(map[string]any)
	reg := obj["regarding"].(map[string]any)
	rel := obj["related"].(map[string]any)
	str := func(m map[string]any, k string) string { s, _ := m[k].(string); return s }
	return modules.Event{
		Namespace: str(meta, "namespace"), Reason: str(obj, "reason"), Type: str(obj, "type"),
		ReportingController: str(obj, "reportingController"), Note: str(obj, "note"),
		Regarding: modules.ObjectRef{APIVersion: str(reg, "apiVersion"), Kind: str(reg, "kind"),
			Namespace: str(reg, "namespace"), Name: str(reg, "name")},
		Related: modules.ObjectRef{APIVersion: str(rel, "apiVersion"), Kind: str(rel, "kind"),
			Namespace: str(rel, "namespace"), Name: str(rel, "name")},
	}
}

// ofRule returns the findings of the admission rule.
func ofRule(fs []modules.Finding) []modules.Finding {
	var out []modules.Finding
	for _, f := range fs {
		if f.Module == Name && f.Rule == RuleAdmissionRefused {
			out = append(out, f)
		}
	}
	return out
}

func value(f modules.Finding, field string) string {
	for _, e := range f.Evidence {
		if e.Field == field {
			return e.Value
		}
	}
	return ""
}

func TestAdmissionRefusedRecorded(t *testing.T) {
	fs := ofRule(active(t).EvaluateEvent(recordedEvent(t)))
	if len(fs) != 1 {
		t.Fatalf("got %d findings, want 1: %#v", len(fs), fs)
	}
	f := fs[0]
	if f.Reason != ReasonAdmissionRefused || f.Summary != summaryAdmissionRefused {
		t.Errorf("reason %q, summary %q", f.Reason, f.Summary)
	}
	if f.Subject.Kind != KindClusterPolicy || f.Subject.Name != policyNonroot {
		t.Errorf("subject %#v, want the ClusterPolicy", f.Subject)
	}
	want := []modules.Evidence{
		{Field: modules.EvidenceEventReason, Value: "PolicyViolation"},
		{Field: modules.EvidenceEventReportingController, Value: wantAdmission},
		{Field: fieldEventRule, Value: "autogen-run-as-non-root"},
		{Field: fieldRelatedKind, Value: kindDeploy},
		{Field: fieldRelatedNamespace, Value: nsDemo},
		{Field: fieldRelatedName, Value: nameWeb},
	}
	if len(f.Evidence) != len(want)+1 || !slices.Equal(f.Evidence[:len(want)], want) {
		t.Errorf("evidence %#v, want %#v and the note", f.Evidence, want)
	}
	for _, e := range f.Evidence {
		if strings.HasPrefix(e.Field, "event.regarding") {
			t.Errorf("evidence repeats the subject: %#v", e)
		}
	}
	// The evaluator normalizes and bounds the note; the rule read the rule's name before that.
	if note := value(f, modules.EvidenceEventNote); !strings.Contains(note, "autogen-run-as-non-root") ||
		!strings.Contains(note, "(blocked)") || len(note) > modules.MaxEvidenceValueLength {
		t.Errorf("note %q", note)
	}
}

func TestAdmissionRefusedRules(t *testing.T) {
	const blockedNote = "Pod demo/web-0: [check-registry] error (blocked); failed to fetch image"
	cases := []struct {
		name   string
		change func(ev *modules.Event)
		want   bool
	}{
		{asRecorded, func(*modules.Event) {}, true},
		{"rule error, blocked", func(ev *modules.Event) { ev.Note = blockedNote }, true},
		{"namespaced Policy", func(ev *modules.Event) {
			ev.Namespace, ev.Regarding.Kind, ev.Regarding.Namespace = nsDemo, KindPolicy, nsDemo
		}, true},
		{"forged: ClusterPolicy event in another namespace", func(ev *modules.Event) { ev.Namespace = nsDemo }, false},
		{"forged: ClusterPolicy event without a namespace", func(ev *modules.Event) { ev.Namespace = "" }, false},
		{"forged: ClusterPolicy with a namespace", func(ev *modules.Event) { ev.Regarding.Namespace = clusterEventNamespace }, false},
		{"forged: Policy event in another namespace", func(ev *modules.Event) {
			ev.Namespace, ev.Regarding.Kind, ev.Regarding.Namespace = "attacker", KindPolicy, nsDemo
		}, false},
		{"forged: Policy event in default", func(ev *modules.Event) {
			ev.Regarding.Kind, ev.Regarding.Namespace = KindPolicy, nsDemo
		}, false},
		{"forged: Policy without a namespace", func(ev *modules.Event) { ev.Regarding.Kind = KindPolicy }, false},
		{"forged: Policy without a namespace, event namespace invalid", func(ev *modules.Event) {
			ev.Namespace, ev.Regarding.Kind = "not a namespace", KindPolicy // the evaluator empties both
		}, false},
		{"unblocked note with a blocked-looking message", func(ev *modules.Event) {
			ev.Note = "Deployment demo/web: [autogen-run-as-non-root] fail; validation error: " +
				"Deployment demo/web: [autogen-run-as-non-root] fail (blocked)"
		}, false},
		{"kyverno.io apiVersion", func(ev *modules.Event) { ev.Regarding.APIVersion = gvPolicy }, true},
		{"another group", func(ev *modules.Event) { ev.Regarding.APIVersion = "example.com/v1" }, false},
		{"audit result, not blocked", func(ev *modules.Event) {
			ev.Note = "Deployment demo/web: [autogen-run-as-non-root] fail; validation error"
		}, false},
		{"Normal event", func(ev *modules.Event) { ev.Type = "Normal" }, false},
		{"another reason", func(ev *modules.Event) { ev.Reason = "PolicyApplied" }, false},
		{"background scan", func(ev *modules.Event) { ev.ReportingController = wantScan }, false},
		{"another controller", func(ev *modules.Event) { ev.ReportingController = "kyverno-admission-fake" }, false},
		{"regarding the resource", func(ev *modules.Event) {
			ev.Regarding = modules.ObjectRef{APIVersion: appsV1, Kind: kindDeploy, Namespace: nsDemo, Name: nameWeb}
		}, false},
		{"regarding another kind without an apiVersion", func(ev *modules.Event) { ev.Regarding.Kind = kindDeploy }, false},
		{"no policy name", func(ev *modules.Event) { ev.Regarding.Name = "" }, false},
		{"pass", func(ev *modules.Event) { ev.Note = "Deployment demo/web: [r] pass (blocked)" }, false},
		{"blocked only in the message", func(ev *modules.Event) {
			ev.Note = "Deployment demo/web: [r] fail; text (blocked)"
		}, false},
		{"rule name over 63 characters", func(ev *modules.Event) {
			ev.Note = "Deployment demo/web: [" + strings.Repeat("r", 64) + "] fail (blocked)"
		}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ev := recordedEvent(t)
			c.change(&ev)
			if got := len(ofRule(active(t).EvaluateEvent(ev))) == 1; got != c.want {
				t.Errorf("found = %v, want %v", got, c.want)
			}
		})
	}
}

// TestAdmissionRefusedFixedText checks that event text reaches only the evidence, never the reason or the
// summary, and that the symptom fits the contract.
func TestAdmissionRefusedFixedText(t *testing.T) {
	ev := recordedEvent(t)
	ev.Note = "Deployment demo/web: [" + hostile[:40] + "] fail (blocked); " + hostile
	s, ok := admissionRefused(ev)
	if !ok {
		t.Fatal("the hostile note did not match")
	}
	if err := modules.ValidSymptom(s); err != nil {
		t.Errorf("ValidSymptom: %v", err)
	}
	if s.Reason != ReasonAdmissionRefused || s.Summary != summaryAdmissionRefused || strings.Contains(s.Summary, "Ignore") {
		t.Errorf("cluster text reached the reason or summary: %#v", s)
	}
	if len(s.Evidence) >= modules.MaxEvidence {
		t.Errorf("the fullest symptom has %d evidence entries, want room under the cap of %d", len(s.Evidence), modules.MaxEvidence)
	}
}

// TestAdmissionRefusedResource checks where the refused resource is read: the related object when it is valid,
// otherwise the note, whose resource part may hold a colon or an empty name.
func TestAdmissionRefusedResource(t *testing.T) {
	const roleNote = "ClusterRole system:aggregate-to-edit: [restrict-aggregation] fail (blocked); " +
		"validation error: aggregated roles may not grant escalate"
	cases := []struct {
		name         string
		change       func(ev *modules.Event)
		wantRelated  string // the related name in evidence, or ""
		wantResource string // the note's resource part in evidence, or ""
	}{
		{"related object", func(*modules.Event) {}, nameWeb, ""},
		{"refused update of an existing object", func(ev *modules.Event) {
			ev.Related = modules.ObjectRef{APIVersion: appsV1, Kind: kindDeploy, Namespace: nsDemo, Name: "api"}
			ev.Note = "Deployment demo/api: [autogen-run-as-non-root] fail (blocked); validation error: Running as root is not allowed."
		}, "api", ""},
		{"name with a colon: related is sanitized away, the note gives it", func(ev *modules.Event) {
			ev.Related = modules.ObjectRef{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole", Name: "system:aggregate-to-edit"}
			ev.Note = roleNote
		}, "", "ClusterRole system:aggregate-to-edit"},
		{"no related object", func(ev *modules.Event) {
			ev.Related = modules.ObjectRef{}
		}, "", "Deployment demo/web"},
		{"cluster-scoped object with an empty name", func(ev *modules.Event) {
			ev.Related = modules.ObjectRef{}
			ev.Note = "ClusterRole : [restrict-aggregation] fail (blocked); validation error"
		}, "", "ClusterRole"}, // the evaluator trims the trailing space
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ev := recordedEvent(t)
			c.change(&ev)
			fs := ofRule(active(t).EvaluateEvent(ev))
			if len(fs) != 1 {
				t.Fatalf("got %d findings, want 1", len(fs))
			}
			if got := value(fs[0], fieldRelatedName); got != c.wantRelated {
				t.Errorf("related name %q, want %q", got, c.wantRelated)
			}
			if got := value(fs[0], fieldEventResource); got != c.wantResource {
				t.Errorf("resource from the note %q, want %q", got, c.wantResource)
			}
		})
	}
}
