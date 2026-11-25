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

package snapshot

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/shadewright-labs/shadewright/internal/redact"
)

// Encoder checks snapshot strings with the installation's redaction patterns.
type Encoder struct {
	redactor *redact.Redactor
}

// NewEncoder compiles the built-in redaction rules and any installation-specific patterns.
func NewEncoder(extra ...redact.Pattern) (*Encoder, error) {
	r, err := redact.New(extra...)
	if err != nil {
		return nil, fmt.Errorf("snapshot redactor: %w", err)
	}
	return &Encoder{redactor: r}, nil
}

// Encode returns the final request body and true, or nil and false when withheld.
func (e *Encoder) Encode(ctx context.Context, m Mapping, c Candidate) ([]byte, bool) {
	if e == nil || e.redactor == nil || ctx.Err() != nil {
		return nil, false
	}
	return encode(ctx, m, c, e.redactor.WithContext(ctx).StringTwiceChecked)
}

type stringCheck func(string, func(string) int) (string, bool)

const (
	maxEvidenceBytes = 2 << 10
	maxExcerptBytes  = 4 << 10
	maxHintBytes     = 1 << 10
	maxBodyBytes     = 16 << 10
	verdict          = "ApplicationCodeCandidate"
)

var (
	uuid   = regexp.MustCompile(`^[[:xdigit:]]{8}-[[:xdigit:]]{4}-[[:xdigit:]]{4}-[[:xdigit:]]{4}-[[:xdigit:]]{12}$`)
	digest = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	alias  = regexp.MustCompile(`^[a-z0-9-]{1,63}$`)
)

func encode(ctx context.Context, m Mapping, c Candidate, check stringCheck) ([]byte, bool) {
	if !alias.MatchString(m.RepositoryAlias) || !alias.MatchString(m.ContainerAlias) ||
		!check.exact(verdict, m.RepositoryAlias, m.ContainerAlias) {
		return nil, false
	}

	u, ok := check.identity(c)
	if !ok || !check.rules(c.MatchedRuleIDs) {
		return nil, false
	}
	u.ContainerAlias = m.ContainerAlias
	u.MatchedRuleIDs = c.MatchedRuleIDs
	u.Evidence, ok = check.evidence(c.Evidence, m.ContainerAlias)
	if !ok {
		return nil, false
	}
	u.Excerpts, ok = check.excerpts(c.Excerpts, m.ContainerAlias)
	if !ok {
		return nil, false
	}
	u.ModelHint = check.hint(c.ModelHint)
	body, err := json.Marshal(payload{
		SchemaVersion: 1, Verdict: verdict, RuleVersion: 1,
		RepositoryAlias: m.RepositoryAlias, Untrusted: u,
	})
	if err != nil || len(body) > maxBodyBytes || ctx.Err() != nil {
		return nil, false
	}
	return body, true
}

// Aliases, timestamps, digests and enums must still identify the same value on the wire.
func (check stringCheck) exact(values ...string) bool {
	for _, s := range values {
		if out, ok := check(s, nil); !ok || out != s {
			return false
		}
	}
	return true
}

func timestamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	t = t.UTC()
	if t.Year() < 0 || t.Year() > 9999 {
		return ""
	}
	return t.Format(time.RFC3339Nano)
}

func (w Workload) valid() bool {
	return (w.Kind == Deployment || w.Kind == StatefulSet) &&
		len(w.Namespace) <= validation.DNS1123LabelMaxLength && len(w.Name) <= validation.DNS1123SubdomainMaxLength &&
		len(validation.IsDNS1123Label(w.Namespace)) == 0 && len(validation.IsDNS1123Subdomain(w.Name)) == 0
}

func (check stringCheck) identity(c Candidate) (untrusted, bool) {
	first, last := timestamp(c.FirstSeen), timestamp(c.LastSeen)
	if !uuid.MatchString(c.IncidentUID) || !digest.MatchString(c.Fingerprint) || !c.Workload.valid() ||
		!c.Reason.valid() || c.Count <= 0 || first == "" || last == "" {
		return untrusted{}, false
	}
	if c.ImageDigest != "" && !digest.MatchString(c.ImageDigest) {
		return untrusted{}, false
	}
	w := c.Workload
	if !check.exact(c.IncidentUID, c.Fingerprint, string(c.Reason), string(w.Kind), first, last) {
		return untrusted{}, false
	}
	var ok bool
	w.Namespace, ok = check(w.Namespace, nil)
	if !ok {
		return untrusted{}, false
	}
	w.Name, ok = check(w.Name, nil)
	if !ok {
		return untrusted{}, false
	}
	u := untrusted{
		IncidentUID: c.IncidentUID, Fingerprint: c.Fingerprint, Reason: c.Reason,
		Count: c.Count, FirstSeen: first, LastSeen: last, Workload: w,
	}
	if c.ImageDigest != "" && check.exact(c.ImageDigest) {
		u.ImageDigest = c.ImageDigest
	}
	return u, true
}

func (check stringCheck) rules(ids []RuleID) bool {
	if len(ids) == 0 || len(ids) > 8 {
		return false
	}
	seen := make(map[RuleID]bool, len(ids))
	for _, id := range ids {
		switch id {
		case AnalysisEscalated, NoRequest, WorkloadMapped, PolicySourceExcluded, InfrastructureExcluded, DistinctObservations, CurrentFailure, CheckedLogs:
		default:
			return false
		}
		if seen[id] || !check.exact(string(id)) {
			return false
		}
		seen[id] = true
	}
	return true
}

func (check stringCheck) evidence(observations []Observation, container string) ([]string, bool) {
	if len(observations) == 0 || len(observations) > 16 {
		return nil, false
	}
	lines := make([]string, 0, len(observations))
	total := 0
	for _, o := range observations {
		at := timestamp(o.ObservedAt)
		if !o.Reason.valid() || at == "" {
			return nil, false
		}
		line := fmt.Sprintf("Observed %s for container %s at %s.", o.Reason, container, at)
		out, ok := check(line, nil)
		if !ok {
			return nil, false
		}
		total += len(out)
		if total > maxEvidenceBytes {
			return nil, false
		}
		lines = append(lines, out)
	}
	return lines, true
}

func (check stringCheck) text(s string, limit int) (string, bool) {
	if s == "" || len(s) > limit {
		return "", false
	}
	out, ok := check(s, nil)
	if !ok || !utf8.ValidString(out) {
		return "", false
	}
	return out, true
}

func (check stringCheck) excerpts(entries []Excerpt, container string) ([]wireExcerpt, bool) {
	if len(entries) == 0 || len(entries) > 2 {
		return nil, false
	}
	out := make([]wireExcerpt, 0, len(entries))
	hasRequired := false
	for _, e := range entries {
		at := timestamp(e.CollectedAt)
		if at == "" {
			return nil, false
		}
		ex := wireExcerpt{ContainerAlias: container, Cut: e.Cut, Withheld: true}
		if check.exact(at) {
			ex.CollectedAt = at
		}
		if ex.CollectedAt != "" && !e.Withheld {
			text, ok := check.text(e.Text, maxExcerptBytes)
			if ok && strings.TrimSpace(text) != "" {
				ex.Text, ex.Withheld = text, false
				hasRequired = hasRequired || !e.Optional
			}
		}
		out = append(out, ex)
	}
	return out, hasRequired
}

func (check stringCheck) hint(raw string) string {
	text, ok := check.text(raw, maxHintBytes)
	if !ok {
		return ""
	}
	stripped := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == '\u2028' || r == '\u2029' {
			return -1
		}
		return r
	}, text)
	if stripped == text {
		return text
	}
	// Removing a control can join two fragments of a credential.
	out, _ := check.text(stripped, maxHintBytes)
	return out
}
