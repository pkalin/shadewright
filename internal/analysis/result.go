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
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"k8s.io/apimachinery/pkg/api/resource"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/policy"
)

// MaxResultBytes bounds the structured output the loop decodes. The largest valid result is far shorter.
const MaxResultBytes = 64 << 10

// Result is the structured output of a model call (design 3.3, ADR-0023 decision 5): the only way a proposal
// is made. Its JSON schema is generated from these types ([ResponseSchema]), and [DecodeResult] enforces the
// same bounds in Go. It has no confidence field: a model's self-reported confidence is never read, and a
// result that carries one is refused as not matching the schema.
type Result struct {
	// Summary is a short diagnosis.
	Summary string `json:"summary" bound:"maxLen=1024"`
	// Hypotheses are the causes considered.
	Hypotheses []Hypothesis `json:"hypotheses" bound:"maxItems=5"`
	// Evidence cites the context blocks and tool results the diagnosis rests on.
	Evidence []Evidence `json:"evidence" bound:"maxItems=10"`
	// MissingData says what the model would need and did not have. Non-empty is a hand-off that says what is
	// missing, never an escalation trigger (ADR-0023 decision 6).
	MissingData []string `json:"missingData" bound:"maxItems=10,maxLen=256"`
	// Uncertainty says what the diagnosis is unsure about, in words; it is never a number and never read as
	// eligibility.
	Uncertainty string `json:"uncertainty" bound:"maxLen=512"`
	// Proposal is the typed change, or null for no proposal.
	Proposal *Proposal `json:"proposal"`
}

// Hypothesis is one possible cause.
type Hypothesis struct {
	// Description of the cause.
	Description string `json:"description" bound:"maxLen=512"`
}

// Evidence is one quotation from what the model was shown.
type Evidence struct {
	// Source is the ID of the block quoted: c1, c2... for the context blocks, t1, t2... for tool results.
	Source string `json:"source" bound:"maxLen=3" pattern:"^[ct][1-9][0-9]?$"`
	// Quote is a verbatim excerpt of that block. A proposal needs at least one quote found in its block.
	Quote string `json:"quote" bound:"maxLen=512"`
}

// Proposal is a typed change from the closed catalog (invariant 2). Its target is always the Incident's
// subject: the model never names an object, a repository, a policy or a namespace.
type Proposal struct {
	// Action is the change.
	Action ProposedAction `json:"action"`
	// ExpectedEffect says what the change should do.
	ExpectedEffect string `json:"expectedEffect" bound:"maxLen=512"`
	// VerificationChecks are checks to run besides those the action always needs.
	VerificationChecks []remediationv1alpha1.VerificationCheck `json:"verificationChecks" bound:"maxItems=5"`
}

// ProposedAction mirrors [remediationv1alpha1.Action] in a form a strict schema can express: one member per
// action type, named as in the API, and null for the others. A test keeps the two in step.
type ProposedAction struct {
	// Type selects the action; its schema is the enum of [remediationv1alpha1.ActionTypes].
	Type remediationv1alpha1.ActionType `json:"type"`
	// AdjustKedaMaxReplicas holds the parameters of AdjustKedaMaxReplicas.
	AdjustKedaMaxReplicas *KedaMaxReplicasChange `json:"adjustKedaMaxReplicas"`
	// SetContainerMemoryLimit holds the parameters of SetContainerMemoryLimit.
	SetContainerMemoryLimit *MemoryLimitChange `json:"setContainerMemoryLimit"`
}

// KedaMaxReplicasChange are the parameters of AdjustKedaMaxReplicas.
type KedaMaxReplicasChange struct {
	// PreviousValue is the maxReplicaCount the plan was made against.
	PreviousValue int32 `json:"previousValue" bound:"min=1,max=1000"`
	// DesiredValue is the new maxReplicaCount.
	DesiredValue int32 `json:"desiredValue" bound:"min=1,max=1000"`
}

// MemoryLimitChange are the parameters of SetContainerMemoryLimit. The values are whole bytes with an
// optional binary suffix, such as 256Mi.
type MemoryLimitChange struct {
	// Container is the container name in the pod template.
	Container string `json:"container" bound:"maxLen=63" pattern:"^[a-z0-9]([-a-z0-9]*[a-z0-9])?$"`
	// Previous is the limit the plan was made against.
	Previous string `json:"previous" bound:"maxLen=24" pattern:"^[1-9][0-9]{0,15}(Ki|Mi|Gi|Ti)?$"`
	// Desired is the new limit.
	Desired string `json:"desired" bound:"maxLen=24" pattern:"^[1-9][0-9]{0,15}(Ki|Mi|Gi|Ti)?$"`
}

// keyType is the JSON schema keyword for a type.
const keyType = "type"

// enums are the string types whose schema is an enum, and their values, from the API's own lists.
var enums = map[reflect.Type]func() []string{
	reflect.TypeFor[remediationv1alpha1.ActionType](): func() []string {
		return strs(remediationv1alpha1.ActionTypes())
	},
	reflect.TypeFor[remediationv1alpha1.VerificationCheck](): func() []string {
		return strs(remediationv1alpha1.VerificationChecks())
	},
}

func strs[T ~string](in []T) []string {
	out := make([]string, len(in))
	for i, v := range in {
		out[i] = string(v)
	}
	return out
}

// bounds are the parsed bound and pattern tags of a field.
type bounds struct {
	maxLen, maxItems int
	min, max         *int64
	pattern          *regexp.Regexp
}

var patterns sync.Map // pattern text -> *regexp.Regexp

// sizeTag parses a length or item bound of a tag, at 32 bits so that it fits an int on every platform.
func sizeTag(v, field string) int {
	n, err := strconv.ParseInt(v, 10, 32)
	if err != nil {
		panic(fmt.Sprintf("analysis: bad bound tag on %s", field))
	}
	return int(n)
}

// boundsOf parses the tags of f. A malformed tag is a programming error that the package's tests catch, so it
// panics; tags are constants of this file, never input.
func boundsOf(f reflect.StructField) bounds {
	var b bounds
	if tag := f.Tag.Get("bound"); tag != "" {
		for kv := range strings.SplitSeq(tag, ",") {
			k, v, _ := strings.Cut(kv, "=")
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				panic(fmt.Sprintf("analysis: bad bound tag on %s", f.Name))
			}
			switch k {
			case "maxLen":
				b.maxLen = sizeTag(v, f.Name)
			case "maxItems":
				b.maxItems = sizeTag(v, f.Name)
			case "min":
				b.min = &n
			case "max":
				b.max = &n
			default:
				panic(fmt.Sprintf("analysis: unknown bound %q on %s", k, f.Name))
			}
		}
	}
	if p := f.Tag.Get("pattern"); p != "" {
		re, ok := patterns.Load(p)
		if !ok {
			re, _ = patterns.LoadOrStore(p, regexp.MustCompile(p))
		}
		b.pattern = re.(*regexp.Regexp)
	}
	return b
}

func jsonName(f reflect.StructField) string {
	name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
	return name
}

// schemaOf builds the JSON schema of t for a strict structured output: every object lists all its
// properties as required and allows no others, and an optional member is a union with null. It uses only
// keywords strict modes accept (type, enum, pattern, minimum, maximum, maxItems, anyOf); lengths are
// enforced in Go by [DecodeResult].
func schemaOf(t reflect.Type, b bounds) map[string]any {
	switch t.Kind() {
	case reflect.Pointer:
		return map[string]any{"anyOf": []any{schemaOf(t.Elem(), b), map[string]any{keyType: "null"}}}
	case reflect.Struct:
		props := map[string]any{}
		required := []string{}
		for f := range t.Fields() {
			name := jsonName(f)
			props[name] = schemaOf(f.Type, boundsOf(f))
			required = append(required, name)
		}
		return map[string]any{keyType: "object", "properties": props, "required": required, "additionalProperties": false}
	case reflect.Slice:
		s := map[string]any{keyType: "array", "items": schemaOf(t.Elem(), bounds{pattern: b.pattern})}
		if b.maxItems > 0 {
			s["maxItems"] = b.maxItems
		}
		return s
	case reflect.String:
		s := map[string]any{keyType: "string"}
		if e, ok := enums[t]; ok {
			s["enum"] = e()
		}
		if b.pattern != nil {
			s["pattern"] = b.pattern.String()
		}
		return s
	case reflect.Int32:
		s := map[string]any{keyType: "integer"}
		if b.min != nil {
			s["minimum"] = *b.min
		}
		if b.max != nil {
			s["maximum"] = *b.max
		}
		return s
	default:
		panic(fmt.Sprintf("analysis: no schema for kind %s", t.Kind()))
	}
}

var responseSchema = sync.OnceValue(func() json.RawMessage {
	b, err := json.Marshal(schemaOf(reflect.TypeFor[Result](), bounds{}))
	if err != nil {
		panic(err)
	}
	return b
})

// ResponseSchema returns the JSON schema of [Result], generated from the Go types, the API's enums
// included. It is the request's ResponseSchema and part of the prompt.
func ResponseSchema() json.RawMessage { return bytes.Clone(responseSchema()) }

// Errors of [DecodeResult]. Their text is fixed: nothing of the reply is echoed. Their code is the fixed code
// the triage retry names in its corrective message.
var (
	errInvalid = &decodeError{code: "schema", msg: "the reply does not match the result schema"}
	errAction  = &decodeError{code: "action", msg: "the proposed action does not match its type"}
	// errOutOfBounds is an action the policy's catalog check refuses. It is never retried and never named to
	// the model, so the model cannot search for what the policy allows: the analysis ends with
	// ReasonActionOutOfBounds.
	errOutOfBounds = errors.New("the proposed action is outside the catalog's bounds")
)

type decodeError struct{ code, msg string }

func (e *decodeError) Error() string { return e.msg }

// DecodeResult parses a model's reply into a Result and checks it against the schema in Go: one top-level JSON
// object of at most [MaxResultBytes] in which every object has exactly the properties of the schema, all
// present, in their exact case and no other (a confidence field included), and null only where the schema
// allows it; every string valid UTF-8 within its length and pattern, every enum value in the API's enum,
// every list within its bound, every integer within its range, and a proposal with exactly the member of its
// type set and parameters the policy's catalog check accepts. A reply that fails any check is refused whole;
// one whose only fault is an action the catalog check refuses returns errOutOfBounds.
// With a proposal it also returns the action converted to the API's type, the one a request carries; nil
// without one.
func DecodeResult(content string) (*Result, *remediationv1alpha1.Action, error) {
	if len(content) > MaxResultBytes {
		return nil, nil, errInvalid
	}
	// The shape first: encoding/json matches keys in any case, leaves a missing field at its zero value and
	// takes null for any slice or pointer, all of which the strict schema refuses.
	dec := json.NewDecoder(strings.NewReader(content))
	dec.UseNumber()
	var raw any
	if err := dec.Decode(&raw); err != nil {
		return nil, nil, errInvalid
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, nil, errInvalid
	}
	if !exactShape(raw, reflect.TypeFor[Result]()) {
		return nil, nil, errInvalid
	}
	dec = json.NewDecoder(strings.NewReader(content))
	dec.DisallowUnknownFields()
	var r Result
	if err := dec.Decode(&r); err != nil {
		return nil, nil, errInvalid
	}
	if !valid(reflect.ValueOf(r), bounds{}) {
		return nil, nil, errInvalid
	}
	if r.Proposal == nil {
		return &r, nil, nil
	}
	action, err := r.Proposal.Action.toAPI()
	if err != nil {
		return nil, nil, err
	}
	return &r, &action, nil
}

// exactShape reports whether the generic JSON value v has the shape the schema of t requires: an object with
// exactly the JSON names of t's fields, case included, an array for a slice, a string for a string, a number
// for an integer, and null only for a pointer.
func exactShape(v any, t reflect.Type) bool {
	switch t.Kind() {
	case reflect.Pointer:
		return v == nil || exactShape(v, t.Elem())
	case reflect.Struct:
		m, ok := v.(map[string]any)
		if !ok || len(m) != t.NumField() {
			return false
		}
		for f := range t.Fields() {
			fv, ok := m[jsonName(f)]
			if !ok || !exactShape(fv, f.Type) {
				return false
			}
		}
		return true
	case reflect.Slice:
		a, ok := v.([]any)
		if !ok {
			return false
		}
		for _, e := range a {
			if !exactShape(e, t.Elem()) {
				return false
			}
		}
		return true
	case reflect.String:
		_, ok := v.(string)
		return ok
	case reflect.Int32:
		_, ok := v.(json.Number)
		return ok
	default:
		return false
	}
}

func valid(v reflect.Value, b bounds) bool {
	switch v.Kind() {
	case reflect.Pointer:
		return v.IsNil() || valid(v.Elem(), b)
	case reflect.Struct:
		for f, fv := range v.Fields() {
			if !valid(fv, boundsOf(f)) {
				return false
			}
		}
		return true
	case reflect.Slice:
		if b.maxItems > 0 && v.Len() > b.maxItems {
			return false
		}
		for i := range v.Len() {
			if !valid(v.Index(i), bounds{maxLen: b.maxLen, pattern: b.pattern}) {
				return false
			}
		}
		return true
	case reflect.String:
		s := v.String()
		if !utf8.ValidString(s) || (b.maxLen > 0 && len(s) > b.maxLen) {
			return false
		}
		if b.pattern != nil && !b.pattern.MatchString(s) {
			return false
		}
		if e, ok := enums[v.Type()]; ok && !slices.Contains(e(), s) {
			return false
		}
		return true
	case reflect.Int32:
		n := v.Int()
		return (b.min == nil || n >= *b.min) && (b.max == nil || n <= *b.max)
	default:
		return false
	}
}

// toAPI converts the proposed action into the API's Action and runs the policy's catalog check on it. Only
// the member of the type may be set.
func (a ProposedAction) toAPI() (remediationv1alpha1.Action, error) {
	out := remediationv1alpha1.Action{Type: a.Type}
	switch a.Type {
	case remediationv1alpha1.ActionAdjustKedaMaxReplicas:
		if a.AdjustKedaMaxReplicas == nil || a.SetContainerMemoryLimit != nil {
			return out, errAction
		}
		p := a.AdjustKedaMaxReplicas
		out.AdjustKedaMaxReplicas = &remediationv1alpha1.AdjustKedaMaxReplicas{PreviousValue: p.PreviousValue, DesiredValue: p.DesiredValue}
	case remediationv1alpha1.ActionSetContainerMemoryLimit:
		if a.SetContainerMemoryLimit == nil || a.AdjustKedaMaxReplicas != nil {
			return out, errAction
		}
		p := a.SetContainerMemoryLimit
		prev, err1 := resource.ParseQuantity(p.Previous)
		desired, err2 := resource.ParseQuantity(p.Desired)
		if err1 != nil || err2 != nil {
			return out, errAction
		}
		out.SetContainerMemoryLimit = &remediationv1alpha1.SetContainerMemoryLimit{Container: p.Container, Previous: prev, Desired: desired}
	default:
		return out, errAction
	}
	if reason, _ := policy.CheckAction(out); reason != "" {
		return out, errOutOfBounds
	}
	return out, nil
}
