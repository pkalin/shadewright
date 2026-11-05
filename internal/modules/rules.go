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

package modules

import (
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/pkalin/shadewright/internal/incidents/fingerprint"
	"github.com/pkalin/shadewright/internal/redact"
)

// Bounds of a [Symptom], checked by [ValidSymptom], and of what a rule is shown.
const (
	// MaxSummaryLength is the longest summary, in bytes.
	MaxSummaryLength = 256
	// MaxEvidence is the most evidence entries one symptom carries.
	MaxEvidence = 8
	// MaxEvidenceValueLength is the longest evidence value, in bytes, once the evaluator has bounded it.
	MaxEvidenceValueLength = 256
	// MaxRuleInput is the longest note or message, in bytes, a rule is shown. A longer one is shown as empty,
	// never cut: a cut before redaction could keep the tail of a secret. Event notes are at most 1 KiB, so this
	// only matters for node condition messages, which have no limit.
	MaxRuleInput = 16 << 10
)

var (
	symptomReasonPattern = regexp.MustCompile(`^[A-Z][A-Za-z0-9]{0,127}$`)
	evidenceFieldPattern = regexp.MustCompile(`^[A-Za-z0-9./_-]{1,128}$`)
	// The patterns of api/v1alpha1 ObjectRef, which a subject must fit.
	apiVersionPattern = regexp.MustCompile(`^([a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*/)?v[0-9]+((alpha|beta)[0-9]+)?$`)
	namespacePattern  = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
	objectNamePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)
)

// maxNameLength is the longest object name a subject keeps, the bound of ObjectRef.name.
const maxNameLength = 253

// ObjectRef names the object a symptom is about: an event's regarding object, or a node. Its fields come from
// the cluster and are untrusted. Before a rule sees them, each one that breaks the pattern of
// api/v1alpha1 ObjectRef (a group version, a CamelCase kind of at most 63 characters, a DNS label namespace, a
// DNS subdomain name of at most 253 bytes) is replaced with "".
type ObjectRef struct {
	APIVersion string
	Kind       string
	Namespace  string
	Name       string
}

// sanitize keeps the fields of r that fit their API pattern and empties the others.
func (r ObjectRef) sanitize() ObjectRef {
	keep := func(s string, re *regexp.Regexp, n int) string {
		if len(s) > n || !re.MatchString(s) {
			return ""
		}
		return s
	}
	return ObjectRef{
		APIVersion: keep(r.APIVersion, apiVersionPattern, 253),
		Kind:       keep(r.Kind, kindPattern, 63),
		Namespace:  keep(r.Namespace, namespacePattern, 63),
		Name:       keep(r.Name, objectNamePattern, maxNameLength),
	}
}

// Event is what a diagnosis rule reads from one event (events.k8s.io/v1). Every field is untrusted: whoever
// can create events in a namespace writes them, so a rule only compares and matches them and never treats
// them as instructions.
type Event struct {
	// Namespace is the event's own namespace. Events about cluster-scoped objects, such as Nodes, are written
	// to "default" by the Kubernetes event recorders.
	Namespace           string
	Reason              string
	Type                string
	ReportingController string
	// Note is the event's note, or "" when it is longer than [MaxRuleInput].
	Note      string
	Regarding ObjectRef
	// Related is the event's secondary object (events.k8s.io/v1 related), the zero value when it has none,
	// sanitized like Regarding. A policy engine, for example, regards its policy and relates the resource.
	Related ObjectRef
}

// NodeCondition is one entry of a node's status.conditions. Every field is untrusted: whoever can write node
// status (the kubelet, node-problem-detector) writes them.
type NodeCondition struct {
	Type   string
	Status string
	Reason string
	// Message is the condition's message, or "" when it is longer than [MaxRuleInput].
	Message string
}

// NodeState is what a diagnosis rule reads from one node: its name, labels and conditions.
type NodeState struct {
	Name       string
	Labels     map[string]string
	Conditions []NodeCondition
}

// Symptom is what a module's rule found: a reason and a summary the rule wrote itself, and evidence read from
// the cluster.
type Symptom struct {
	// Reason is a CamelCase word written by the rule (^[A-Z][A-Za-z0-9]{0,127}$), never cluster text.
	Reason string
	// Summary is fixed text written by the rule, never cluster text, at most 256 bytes.
	Summary string
	// Evidence is what the rule read from the cluster, at most 8 entries.
	Evidence []Evidence
	// Since is when the state a symptom source reports began: the lastTransitionTime of the condition it
	// reads. Zero when unknown, and for the symptoms of diagnosis rules. It is untrusted: the evaluator never
	// takes it as later than its own clock when it reports when the state began, though it identifies the
	// state by the value as reported. The evaluator counts one occurrence per object, reason and Since, so a
	// state that ends and comes back is a new occurrence and a state that lasts is counted once; a state it saw
	// end and come back with the same Since is a new occurrence too.
	Since time.Time
}

// Evidence is one value a rule read from the cluster.
type Evidence struct {
	// Field says where the value was read, written by the rule (^[A-Za-z0-9./_-]{1,128}$).
	Field string
	// Value is the value read, as the rule found it. It is untrusted. The evaluator redacts it with
	// fingerprint.Normalize ([BoundValue]), which by itself caps a value at 1024 bytes, and then cuts it to
	// [MaxEvidenceValueLength] bytes. The one exception is a value that is a whole Flux revision
	// (redact.IsFluxRevision), which holds only a reference name and a commit and is kept as it is.
	Value string
}

// ValidSymptom reports why a symptom breaks the contract a rule must keep: a reason that is not a CamelCase
// word of at most 128 characters, a summary over [MaxSummaryLength] bytes, more than [MaxEvidence] evidence
// entries, or an evidence field outside its pattern. Evidence values are not checked: they are untrusted
// and bounded by the evaluator. The evaluator calls it on every symptom a rule returns and drops one it refuses, and
// module tests call it on theirs.
func ValidSymptom(s Symptom) error {
	if !symptomReasonPattern.MatchString(s.Reason) {
		return fmt.Errorf("symptom reason %q is not a CamelCase word of at most 128 characters", s.Reason)
	}
	if len(s.Summary) > MaxSummaryLength {
		return fmt.Errorf("symptom %s: summary of %d bytes, at most %d are allowed", s.Reason, len(s.Summary), MaxSummaryLength)
	}
	if len(s.Evidence) > MaxEvidence {
		return fmt.Errorf("symptom %s: %d evidence entries, at most %d are allowed", s.Reason, len(s.Evidence), MaxEvidence)
	}
	for _, e := range s.Evidence {
		if !evidenceFieldPattern.MatchString(e.Field) {
			return fmt.Errorf("symptom %s: evidence field %q is invalid", s.Reason, e.Field)
		}
	}
	return nil
}

// DiagnosisRule is a deterministic diagnosis rule: a pure function from an event or a node to symptoms. A
// rule sets Event, Node or both. Rules must not keep state, read anything but their input or panic; a rule
// that panics is skipped.
type DiagnosisRule struct {
	// Name identifies the rule within its module, with the rules of a module name.
	Name string
	// Event returns the symptom an event shows, and false when the rule does not apply to it.
	Event func(Event) (Symptom, bool)
	// Node returns the symptoms a node shows, none when the rule does not apply to it.
	Node func(NodeState) []Symptom
}

// Finding is a symptom found by a rule of an active module, with redacted and bounded evidence.
type Finding struct {
	// Module is the name of the module whose rule found the symptom.
	Module string
	// Rule is the rule's name.
	Rule string
	// Subject is the object the symptom is about. It comes from the cluster and is untrusted: an event's
	// regarding object can name anything. Its fields fit the patterns of api/v1alpha1 ObjectRef or are empty.
	Subject ObjectRef
	Symptom
}

// boundInput replaces untrusted text longer than MaxRuleInput with "".
func boundInput(s string) string {
	if len(s) > MaxRuleInput {
		return ""
	}
	return s
}

// evaluateEvent runs the event rules of one module.
func evaluateEvent(module string, rules []DiagnosisRule, ev Event) []Finding {
	ev.Note = boundInput(ev.Note)
	ev.Regarding = ev.Regarding.sanitize()
	ev.Related = ev.Related.sanitize()
	if len(ev.Namespace) > 63 || !namespacePattern.MatchString(ev.Namespace) {
		ev.Namespace = ""
	}
	var out []Finding
	for _, r := range rules {
		if r.Event == nil {
			continue
		}
		s, ok := safeEvent(r.Event, ev)
		if !ok {
			continue
		}
		if f, ok := finding(module, r.Name, ev.Regarding, s); ok {
			out = append(out, f)
		}
	}
	return out
}

// evaluateNode runs the node rules of one module.
func evaluateNode(module string, rules []DiagnosisRule, n NodeState) []Finding {
	conds := make([]NodeCondition, len(n.Conditions))
	for i, c := range n.Conditions {
		c.Message = boundInput(c.Message)
		conds[i] = c
	}
	n.Conditions = conds
	subject := ObjectRef{APIVersion: "v1", Kind: "Node", Name: n.Name}.sanitize()
	n.Name = subject.Name
	var out []Finding
	for _, r := range rules {
		if r.Node == nil {
			continue
		}
		for _, s := range safeNode(r.Node, n) {
			if f, ok := finding(module, r.Name, subject, s); ok {
				out = append(out, f)
			}
		}
	}
	return out
}

// safeEvent calls an event rule and treats a panic as no match, so a rule bug cannot stop the analyzer.
func safeEvent(f func(Event) (Symptom, bool), ev Event) (s Symptom, ok bool) {
	defer func() {
		if recover() != nil {
			s, ok = Symptom{}, false
		}
	}()
	return f(ev)
}

// safeNode calls a node rule and treats a panic as no symptom.
func safeNode(f func(NodeState) []Symptom, n NodeState) (out []Symptom) {
	defer func() {
		if recover() != nil {
			out = nil
		}
	}()
	return f(n)
}

// finding checks a rule's symptom with [ValidSymptom], refuses an empty summary, and bounds its evidence
// values. A symptom it refuses is dropped: it is a bug in the rule, which the module's tests catch.
func finding(module, rule string, subject ObjectRef, s Symptom) (Finding, bool) {
	if ValidSymptom(s) != nil || s.Summary == "" {
		return Finding{}, false
	}
	ev := make([]Evidence, 0, len(s.Evidence))
	for _, e := range s.Evidence {
		ev = append(ev, Evidence{Field: e.Field, Value: BoundValue(e.Value)})
	}
	s.Evidence = ev
	return Finding{Module: module, Rule: rule, Subject: subject, Symptom: s}, true
}

// BoundValue redacts and normalizes untrusted text as the collector does for a symptom
// (fingerprint.Normalize: secrets redacted first, then UIDs, digests, times, IPs and durations replaced by
// placeholders) and cuts it to [MaxEvidenceValueLength] bytes, redacting again after the cut. The one
// exception is a value that is, as a whole, a Flux revision ([redact.IsFluxRevision]): it holds only a
// reference name and a commit and is kept as it is. A revision inside other text is redacted like the rest.
func BoundValue(s string) string {
	if redact.IsFluxRevision(s) {
		return s
	}
	s = fingerprint.Normalize(fingerprint.Observation{Note: s})
	if len(s) <= MaxEvidenceValueLength {
		return s
	}
	cut := MaxEvidenceValueLength
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return redact.String(strings.TrimRight(s[:cut], " "))
}
