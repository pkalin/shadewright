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
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/analysis"
	"github.com/pkalin/shadewright/internal/providers"
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
	// Incident UID as a lower-case UUID, a dash and the UTC time as 20060102t150405z. The time is parsed apart,
	// and decodeRecord checks that the UID is the record's incident.uid.
	blobNamePattern = regexp.MustCompile(
		`^(([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})-([0-9]{8}t[0-9]{6}z))\.json$`)
	// fingerprintShape is an Incident fingerprint.
	fingerprintShape = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	// requestShape is a name RequestName gives.
	requestShape = regexp.MustCompile(`^remediation-[0-9a-f]{24}$`)
)

// recordTimeLayout is the time of a record ID.
const recordTimeLayout = "20060102t150405z"

// uidLength is the length of the UUID at the start of a record ID.
const uidLength = 36

// recordIDFromName returns the record ID of a blob name, and false for any other name.
func recordIDFromName(name string) (string, bool) {
	m := blobNamePattern.FindStringSubmatch(name)
	if m == nil {
		return "", false
	}
	if _, err := time.Parse(recordTimeLayout, m[3]); err != nil {
		return "", false
	}
	return m[1], true
}

// maxJSONDepth bounds the nesting of objects and arrays in a record or an answer. A record of the analyzer
// nests three levels deep.
const maxJSONDepth = 16

// reasonDuplicateKey is the schema refusal of a record that checkKeys refuses.
const reasonDuplicateKey = "the record has a key twice in one object (as written, escaped or in another case), " +
	"or nests deeper than 16 levels"

var (
	errDuplicateKey = errors.New("a key appears twice in one object")
	errTooDeep      = errors.New("the document nests too deeply")
	errTrailing     = errors.New("data after the JSON value")
)

// checkKeys walks the tokens of one JSON value and refuses a key that appears twice in one object, compared
// after unescaping and under the case folding encoding/json uses to match field names, so that every decoder
// reads the same members; and nesting deeper than maxJSONDepth. Any other syntax error is returned as it is.
func checkKeys(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	type frame struct {
		object  bool
		wantKey bool
		keys    map[string]bool
	}
	var stack []*frame
	started := false
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			if len(stack) > 0 || !started {
				return io.ErrUnexpectedEOF
			}
			return nil
		}
		if err != nil {
			return err
		}
		if started && len(stack) == 0 {
			return errTrailing
		}
		started = true
		if n := len(stack); n > 0 && stack[n-1].object && stack[n-1].wantKey {
			if k, ok := tok.(string); ok {
				f := foldKey(k)
				if stack[n-1].keys[f] {
					return errDuplicateKey
				}
				stack[n-1].keys[f] = true
				stack[n-1].wantKey = false
				continue
			}
		}
		switch tok {
		case json.Delim('{'), json.Delim('['):
			if len(stack) >= maxJSONDepth {
				return errTooDeep
			}
			stack = append(stack, &frame{object: tok == json.Delim('{'), wantKey: true, keys: map[string]bool{}})
			continue
		case json.Delim('}'), json.Delim(']'):
			stack = stack[:len(stack)-1]
		}
		// A value ended: the object around it, if any, wants its next key.
		if n := len(stack); n > 0 && stack[n-1].object {
			stack[n-1].wantKey = true
		}
	}
}

// foldKey maps each rune of k to the smallest rune of its simple case folding orbit, so that two keys fold to
// the same string exactly when strings.EqualFold holds, the equality encoding/json uses for field names.
func foldKey(k string) string {
	var b strings.Builder
	for _, r := range k {
		m := r
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			m = min(m, f)
		}
		b.WriteRune(m)
	}
	return b.String()
}

// refusal is a check that quarantined a record, with a fixed reason.
type refusal struct {
	check  string
	reason string
}

// decodeRecord checks the schema: the bytes are one JSON object whose schemaVersion the intake knows (checked
// first, so a record of a newer version is refused for that and nothing else), with no key twice in one object
// and no deep nesting (checkKeys), which decodes into the record type with no unknown field and nothing after it,
// whose id is the one of the blob name, and whose incident.uid is the UID in that id. The reasons are fixed
// text: the decoder's own errors quote the input and never leave this function.
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
	if err := checkKeys(data); errors.Is(err, errDuplicateKey) || errors.Is(err, errTooDeep) {
		return analysis.Record{}, schema(reasonDuplicateKey)
	} else if err != nil {
		return analysis.Record{}, schema("the record is not one JSON object")
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
	if len(id) < uidLength || rec.Incident.UID != id[:uidLength] {
		return analysis.Record{}, schema("the record's incident.uid is not the UID of its id")
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

// checkJudgeInput runs the same check over the values the judge is given, built from the decoded record, so
// that what the check read and what the judge reads cannot differ.
func checkJudgeInput(r *redact.Redactor, rec analysis.Record, keep []string) ([]string, bool) {
	data, err := json.Marshal(judgeInputOf(rec))
	if err != nil {
		return []string{"unknown"}, false
	}
	return checkRedaction(r, data, keep)
}

// Reasons of a request that failed the check of what is sent.
const (
	reasonUnsent       = "the redactor found text it would remove; nothing was sent to a model"
	reasonSecondUnsent = "the redactor found text it would remove in the judge's second request; it was not sent " +
		"and the run ended without an answer"
)

// checkedRequest is a model request every text of which passed checkRequest exactly as it is sent. Only
// checkRequest makes one, and send, the only function that calls a model, takes nothing else, so no call can be
// made around the check (a test holds both).
type checkedRequest struct {
	req providers.CompletionRequest
}

// checkRequest is the deciding redaction check (ADR-0047 section 3): the redactor's Clean over each text of req
// whole, as the adapter gets it: every message's content after formatting, its tool calls (ID, name and
// arguments) and tool call ID, the tool definitions and the answer schema, with nothing kept: the record's
// names were replaced by fixed words when the texts were built (nameWords). It returns the rule kinds that matched and false when any text fails. The checks of the raw document and of each
// value run earlier and do not replace this one. ProviderBlocks are opaque to the intake and not read: they are
// the judge's own reply, handed back to the adapter that made them.
func checkRequest(r *redact.Redactor, req providers.CompletionRequest) (checkedRequest, []string, bool) {
	kinds := map[string]bool{}
	clean := true
	check := func(s string) {
		if s == "" {
			return
		}
		if ok, k := redactorClean(r, s, nil); !ok {
			clean = false
			for _, kind := range k {
				kinds[kind] = true
			}
		}
	}
	for _, m := range req.Messages {
		check(m.Content)
		check(m.ToolCallID)
		for _, tc := range m.ToolCalls {
			check(tc.ID)
			check(tc.Name)
			check(string(tc.Arguments))
		}
	}
	for _, td := range req.Tools {
		check(td.Name)
		check(td.Description)
		check(string(td.Parameters))
	}
	check(string(req.ResponseSchema))
	if !clean {
		out := make([]string, 0, len(kinds))
		for k := range kinds {
			out = append(out, k)
		}
		slices.Sort(out)
		return checkedRequest{}, out, false
	}
	return checkedRequest{req: req}, nil, true
}

// Fixed words that stand for the record's names in every text built from the record for a model. The judge's
// prompt says what each stands for.
const (
	wordRecord      = "<record>"
	wordIncident    = "<incident>"
	wordRequest     = "<request>"
	wordFingerprint = "<fingerprint>"
)

// nameWords maps each value of keep, as keptValues returned it for rec (so each passed its shape check), to the
// fixed word that replaces it, and the fingerprint's bare digits to the fingerprint's word. A bare run of the
// digits is not a kept value, so the earlier check refuses a record that holds one; replacing it too only
// sends less.
func nameWords(rec analysis.Record, keep []string) map[string]string {
	words := map[string]string{}
	for _, k := range keep {
		switch k {
		case rec.ID:
			words[k] = wordRecord
		case rec.Incident.Name:
			words[k] = wordIncident
		case rec.Incident.Fingerprint:
			words[k] = wordFingerprint
			words[strings.TrimPrefix(k, "sha256:")] = wordFingerprint
		case rec.Request:
			words[k] = wordRequest
		}
	}
	return words
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
