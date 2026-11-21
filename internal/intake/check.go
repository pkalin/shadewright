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

package intake

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/analysis"
	"github.com/pkalin/shadewright/internal/redact"
)

// MaxRecordBytes bounds a record: the limit the analyzer's store applies to what it writes
// (azureblob.MaxRecordBytes, ADR-0047 section 3). A test keeps the two equal.
const MaxRecordBytes = 1 << 20

// Checks that quarantine a record, named in the score (ADR-0047 section 3, in this order).
const (
	CheckName     = "name"
	CheckSize     = "size"
	CheckSchema   = "schema"
	CheckRedactor = "redactor"
)

var (
	// blobNamePattern is "<record ID>.json" with the ID the analyzer makes (recordID in internal/analysis): the
	// Incident UID, a dash and the UTC time as 20060102t150405z, from 0-9, a-f, "-", "t" and "z" only. The
	// length (at most 64 bytes for the ID) is checked apart.
	blobNamePattern = regexp.MustCompile(`^([0-9a-f][0-9a-f-]*-[0-9]{8}t[0-9]{6}z)\.json$`)
	// fingerprintShape is an Incident fingerprint.
	fingerprintShape = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	// requestShape is a name RequestName gives.
	requestShape = regexp.MustCompile(`^remediation-[0-9a-f]{24}$`)
)

// maxRecordID bounds the ID in the blob name.
const maxRecordID = 64

// recordIDFromName returns the record ID of a blob name, and false for any other name.
func recordIDFromName(name string) (string, bool) {
	m := blobNamePattern.FindStringSubmatch(name)
	if m == nil || len(m[1]) > maxRecordID {
		return "", false
	}
	return m[1], true
}

// refusal is a check that quarantined a record, with a fixed reason.
type refusal struct {
	check  string
	reason string
}

// decodeRecord checks the schema: the bytes are one JSON object whose schemaVersion the intake knows (checked
// first, so a record of a newer version is refused for that and nothing else), which decodes into the record
// type with no unknown field and nothing after it, and whose id is the one of the blob name. The reasons are
// fixed text: the decoder's own errors quote the input and never leave this function.
func decodeRecord(data []byte, id string) (analysis.Record, *refusal) {
	schema := func(reason string) *refusal { return &refusal{check: CheckSchema, reason: reason} }
	var head struct {
		SchemaVersion *int `json:"schemaVersion"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return analysis.Record{}, schema("the record is not one JSON object")
	}
	if head.SchemaVersion == nil || *head.SchemaVersion != analysis.RecordSchemaVersion {
		return analysis.Record{}, schema("the record's schemaVersion is missing or not one the intake knows")
	}
	var rec analysis.Record
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rec); err != nil {
		return analysis.Record{}, schema("the record does not decode into the record type, or has an unknown field")
	}
	if _, err := dec.Token(); err != io.EOF {
		return analysis.Record{}, schema("the record has data after its JSON object")
	}
	if rec.ID != id {
		return analysis.Record{}, schema("the record's id is not the one of its name")
	}
	return rec, nil
}

// keptValues returns the values of the record that the redactor check keeps (ADR-0047 section 3): the ID of
// the blob name, the fingerprint, the Incident's name and the request's name, each only when its shape is
// checked here. A value that fails its check is not kept and quarantines the record.
func keptValues(rec analysis.Record, id string) ([]string, *refusal) {
	shape := &refusal{check: CheckRedactor, reason: "a name or the fingerprint of the record failed its shape check"}
	fp := rec.Incident.Fingerprint
	if !fingerprintShape.MatchString(fp) {
		return nil, shape
	}
	if got, _, ok := remediationv1alpha1.ParseIncidentName(rec.Incident.Name); !ok || got != fp {
		return nil, shape
	}
	keep := []string{id, fp, rec.Incident.Name}
	if rec.Request != "" {
		if !requestShape.MatchString(rec.Request) {
			return nil, shape
		}
		keep = append(keep, rec.Request)
	}
	return keep, nil
}

// checkRedaction runs the redactor over every decoded string of data, every key, and every member whose value
// is a scalar once more as "key: value" (as the analyzer's store checks before its write), keeping only the
// values of keep. It returns the rule kinds that matched, empty when the record is clean. Nothing is redacted
// and passed on.
func checkRedaction(r *redact.Redactor, data []byte, keep []string) ([]string, bool) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	kinds := map[string]bool{}
	clean := true
	walk(v, func(s string) {
		if ok, k := redactorClean(r, s, keep); !ok {
			clean = false
			for _, kind := range k {
				kinds[kind] = true
			}
		}
	})
	out := make([]string, 0, len(kinds))
	for k := range kinds {
		out = append(out, k)
	}
	slices.Sort(out)
	return out, clean
}

// walk calls f with each string of v, each key, and "key: value" for each member with a scalar value.
func walk(v any, f func(string)) {
	switch x := v.(type) {
	case string:
		f(x)
	case []any:
		for _, e := range x {
			walk(e, f)
		}
	case map[string]any:
		for k, e := range x {
			f(k)
			switch e.(type) {
			case string, json.Number, bool, nil:
				f(k + ": " + fmt.Sprint(e))
			}
			walk(e, f)
		}
	}
}

// redactorClean is the one call site of the redactor's check: whether s is clean keeping keep
// ([redact.Redactor.Clean]), and, when it is not, the kinds of the rules that matched outside the kept values.
// The kinds are names of rules, never text.
func redactorClean(r *redact.Redactor, s string, keep []string) (bool, []string) {
	if r.Clean(s, keep...) {
		return true, nil
	}
	var kinds []string
	r.Replace(s, func(kind, text string) string {
		if !slices.Contains(keep, text) {
			kinds = append(kinds, kind)
		}
		return ""
	}, func(string) string { return "" })
	if len(kinds) == 0 {
		// Every match was a kept value or a marker, yet Clean refused: a match ran over a kept value.
		kinds = []string{"unknown"}
	}
	return false, kinds
}

// Family is a model family: the judge is of the other one than the record's model.
type Family string

// Families the intake knows.
const (
	FamilyAnthropic Family = "anthropic"
	FamilyOpenAI    Family = "openai"
)

// Other returns the other family.
func (f Family) Other() Family {
	if f == FamilyAnthropic {
		return FamilyOpenAI
	}
	return FamilyAnthropic
}

// familyPrefixes is the fixed table of model ID prefixes by family. A prefix with word set matches the ID
// itself or the prefix and a "-" ("o3", "o3-mini"), not a longer word ("o3x").
var familyPrefixes = []struct {
	prefix string
	word   bool
	family Family
}{
	{prefix: "claude-", family: FamilyAnthropic},
	{prefix: "gpt-", family: FamilyOpenAI},
	{prefix: "o1", word: true, family: FamilyOpenAI},
	{prefix: "o3", word: true, family: FamilyOpenAI},
	{prefix: "o4", word: true, family: FamilyOpenAI},
}

// modelFamily returns the family of one model ID from the table, and false for an ID it does not know.
func modelFamily(model string) (Family, bool) {
	for _, p := range familyPrefixes {
		if p.word {
			if model == p.prefix || strings.HasPrefix(model, p.prefix+"-") {
				return p.family, true
			}
			continue
		}
		if strings.HasPrefix(model, p.prefix) && len(model) > len(p.prefix) {
			return p.family, true
		}
	}
	return "", false
}

// recordFamily returns the family of the record's models, or the reason it is unjudged: no model, a model the
// table does not know, models of both families, or a model of the OpenAI family under a provider that serves
// only Anthropic's (providers cannot decide the family: Foundry and OpenAI-compatible endpoints serve either).
func recordFamily(rec analysis.Record) (Family, string) {
	if len(rec.Models) == 0 {
		return "", "the record names no model"
	}
	var fam Family
	for _, m := range rec.Models {
		f, ok := modelFamily(m)
		if !ok {
			return "", "the record names a model of a family the intake does not know"
		}
		if fam != "" && f != fam {
			return "", "the record names models of both families"
		}
		fam = f
	}
	if fam == FamilyOpenAI && (rec.Providers.Triage == anthropicProvider || rec.Providers.Escalation == anthropicProvider) {
		return "", "the record's providers do not match its models"
	}
	return fam, ""
}

// anthropicProvider is the provider name of the Anthropic API in a record (RecordProviders).
const anthropicProvider = string(remediationv1alpha1.ModelProviderAnthropic)
