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
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
)

// enumMarker returns the values of the +kubebuilder:validation:Enum marker above "type <name> string".
func enumMarker(t *testing.T, file, name string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "api", "v1alpha1", file))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(b), "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, "type "+name+" string") && i > 0 {
			enum, ok := strings.CutPrefix(strings.TrimSpace(lines[i-1]), "// +kubebuilder:validation:Enum=")
			if !ok {
				t.Fatalf("no enum marker above %s", name)
			}
			return strings.Split(enum, ";")
		}
	}
	t.Fatalf("type %s not found", name)
	return nil
}

func TestActionTypesMatchTheEnum(t *testing.T) {
	if got, want := strs(remediationv1alpha1.ActionTypes()), enumMarker(t, "common_types.go", "ActionType"); !slices.Equal(got, want) {
		t.Errorf("ActionTypes() = %v, the CRD enum is %v", got, want)
	}
}

// schemaAt walks the generated schema along property names, taking the non-null member of a union.
func schemaAt(t *testing.T, path ...string) map[string]any {
	t.Helper()
	var s map[string]any
	if err := json.Unmarshal(ResponseSchema(), &s); err != nil {
		t.Fatal(err)
	}
	for _, p := range path {
		if union, ok := s["anyOf"].([]any); ok {
			s = union[0].(map[string]any)
		}
		props, _ := s["properties"].(map[string]any)
		next, ok := props[p].(map[string]any)
		if !ok {
			t.Fatalf("no property %q in the schema", p)
		}
		s = next
	}
	if union, ok := s["anyOf"].([]any); ok {
		s = union[0].(map[string]any)
	}
	return s
}

func TestSchemaIsGeneratedFromTheCatalog(t *testing.T) {
	enum := schemaAt(t, "proposal", "action", "type")["enum"].([]any)
	got := make([]string, 0, len(enum))
	for _, v := range enum {
		got = append(got, v.(string))
	}
	if want := strs(remediationv1alpha1.ActionTypes()); !slices.Equal(got, want) {
		t.Errorf("the schema's action enum is %v, the catalog %v", got, want)
	}
	checks := schemaAt(t, "proposal", "verificationChecks")["items"].(map[string]any)["enum"].([]any)
	if len(checks) != len(remediationv1alpha1.VerificationChecks()) {
		t.Errorf("the schema's check enum has %d values, the API %d", len(checks), len(remediationv1alpha1.VerificationChecks()))
	}
	if _, ok := schemaAt(t)["properties"].(map[string]any)["confidence"]; ok {
		t.Error("the schema has a confidence field")
	}
}

// Every action type has a member of ProposedAction named as in the API's Action, and a catalog description.
func TestProposedActionMirrorsTheAPI(t *testing.T) {
	api := map[string]bool{}
	at := reflect.TypeFor[remediationv1alpha1.Action]()
	for f := range at.Fields() {
		api[jsonName(f)] = true
	}
	ours := map[string]bool{}
	pt := reflect.TypeFor[ProposedAction]()
	for f := range pt.Fields() {
		ours[jsonName(f)] = true
	}
	if !reflect.DeepEqual(api, ours) {
		t.Errorf("ProposedAction has members %v, Action %v", ours, api)
	}
	for _, typ := range remediationv1alpha1.ActionTypes() {
		member := strings.ToLower(string(typ)[:1]) + string(typ)[1:]
		if !ours[member] {
			t.Errorf("no member %q for action %s", member, typ)
		}
		if catalogDescriptions[typ] == "" {
			t.Errorf("no catalog description for %s", typ)
		}
		if !strings.Contains(systemPrompt(false, 0), "- "+string(typ)+": ") {
			t.Errorf("the prompt's catalog does not list %s", typ)
		}
	}
}

// Every string of the result is bounded in Go: a length bound, or an enum.
func TestEveryStringIsBounded(t *testing.T) {
	var walk func(t reflect.Type, f string, b bounds)
	walk = func(typ reflect.Type, f string, b bounds) {
		switch typ.Kind() {
		case reflect.Pointer:
			walk(typ.Elem(), f, b)
		case reflect.Slice:
			if b.maxItems == 0 {
				t.Errorf("list %s has no maxItems", f)
			}
			walk(typ.Elem(), f, bounds{maxLen: b.maxLen})
		case reflect.Struct:
			for fld := range typ.Fields() {
				walk(fld.Type, f+"."+fld.Name, boundsOf(fld))
			}
		case reflect.String:
			if _, enum := enums[typ]; !enum && b.maxLen == 0 {
				t.Errorf("string %s has no maxLen", f)
			}
		}
	}
	walk(reflect.TypeFor[Result](), "Result", bounds{})
}

func TestDecodeResult(t *testing.T) {
	if _, a, err := DecodeResult(validResult); err != nil || a == nil || a.SetContainerMemoryLimit == nil {
		t.Fatalf("the valid result is refused: %v", err)
	}
	if _, a, err := DecodeResult(noProposal); err != nil || a != nil {
		t.Fatalf("a result without a proposal is refused: %v", err)
	}
	mem := `{"type":"SetContainerMemoryLimit","adjustKedaMaxReplicas":null,` +
		`"setContainerMemoryLimit":{"container":"app","previous":"256Mi","desired":"512Mi"}}`
	for name, bad := range map[string]string{
		"not catalog":       strings.Replace(validResult, `"type":"SetContainerMemoryLimit"`, `"type":"DeletePod"`, 1),
		"lower case type":   strings.Replace(validResult, `"type":"SetContainerMemoryLimit"`, `"type":"setcontainermemorylimit"`, 1),
		"both members":      strings.Replace(validResult, `"adjustKedaMaxReplicas":null`, `"adjustKedaMaxReplicas":{"previousValue":1,"desiredValue":2}`, 1),
		"no member":         strings.Replace(validResult, `{"container":"app","previous":"256Mi","desired":"512Mi"}`, `null`, 1),
		"replicas too high": strings.Replace(validResult, mem, `{"type":"AdjustKedaMaxReplicas","adjustKedaMaxReplicas":{"previousValue":2,"desiredValue":1001},"setContainerMemoryLimit":null}`, 1),
		"same replicas":     strings.Replace(validResult, mem, `{"type":"AdjustKedaMaxReplicas","adjustKedaMaxReplicas":{"previousValue":2,"desiredValue":2},"setContainerMemoryLimit":null}`, 1),
		"memory too high":   strings.Replace(validResult, `"desired":"512Mi"`, `"desired":"2Ti"`, 1),
		"memory decimal":    strings.Replace(validResult, `"desired":"512Mi"`, `"desired":"0.5Gi"`, 1),
		"memory equal":      strings.Replace(validResult, `"desired":"512Mi"`, `"desired":"256Mi"`, 1),
		"container pattern": strings.Replace(validResult, `"container":"app"`, `"container":"App;rm"`, 1),
		"unknown check":     strings.Replace(validResult, `"NoNewOOMKills","NoNewOOMKills"`, `"RunShell"`, 1),
		"confidence":        strings.Replace(validResult, `"summary"`, `"confidence":0.99,"summary"`, 1),
		"other field":       strings.Replace(validResult, `"summary"`, `"targetNamespace":"kube-system","summary"`, 1),
		"trailing data":     validResult + `{}`,
		"not an object":     `["proposal"]`,
		"too many hypotheses": strings.Replace(validResult, `[{"description":"limit too low"}]`,
			"["+strings.Repeat(`{"description":"x"},`, 5)+`{"description":"x"}]`, 1),
		"summary too long": strings.Replace(validResult, `"The container runs out of memory."`, `"`+strings.Repeat("x", 1025)+`"`, 1),
		"source pattern":   strings.Replace(validResult, `"source":"c2"`, `"source":"system"`, 1),
		"too large":        `{"summary":"` + strings.Repeat("x", MaxResultBytes) + `"}`,
		// The shape the strict schema asks for: encoding/json alone accepts each of these.
		"null":                 `null`,
		"empty object":         `{}`,
		"missing field":        strings.Replace(noProposal, `"uncertainty":"",`, ``, 1),
		"missing proposal":     strings.Replace(noProposal, `,"proposal":null`, ``, 1),
		"key in other case":    strings.Replace(validResult, `"summary"`, `"Summary"`, 1),
		"nested key case":      strings.Replace(validResult, `"container":"app"`, `"Container":"app"`, 1),
		"key twice in cases":   strings.Replace(validResult, `"summary"`, `"SUMMARY":"x","summary"`, 1),
		"null list":            strings.Replace(noProposal, `"missingData":[]`, `"missingData":null`, 1),
		"null string":          strings.Replace(noProposal, `"uncertainty":""`, `"uncertainty":null`, 1),
		"missing member":       strings.Replace(validResult, `"adjustKedaMaxReplicas":null,`, ``, 1),
		"unknown nested field": strings.Replace(validResult, `"container":"app"`, `"namespace":"kube-system","container":"app"`, 1),
		"string for number": strings.Replace(validResult, mem,
			`{"type":"AdjustKedaMaxReplicas","adjustKedaMaxReplicas":{"previousValue":"2","desiredValue":4},"setContainerMemoryLimit":null}`, 1),
	} {
		if _, _, err := DecodeResult(bad); err == nil {
			t.Errorf("%s: accepted", name)
		} else if strings.Contains(err.Error(), "DeletePod") || strings.Contains(err.Error(), "kube-system") {
			t.Errorf("%s: the error echoes the reply: %v", name, err)
		}
	}
}

func TestPromptVersionCoversTheGeneratedParts(t *testing.T) {
	v := PromptVersion()
	if !strings.HasPrefix(v, "sha256:") || len(v) != len("sha256:")+64 {
		t.Errorf("prompt version %q", v)
	}
	if !strings.Contains(systemPrompt(false, 0), string(ResponseSchema())) {
		t.Error("the system prompt does not hold the generated schema")
	}
}

// The prompt and the schema say which value a previous field holds: the workload's spec as it is now,
// written as in the manifest, not a running pod's or a metric's (#494).
func TestPreviousFieldsAreDescribed(t *testing.T) {
	schema := string(ResponseSchema())
	prompt := systemPrompt(false, 0)
	for _, want := range []string{
		"The memory limit the workload's pod template sets for the container now, written as in the manifest (for example 48Mi or 50M)",
		"The maxReplicaCount the ScaledObject's spec holds now",
	} {
		if !strings.Contains(schema, want) {
			t.Errorf("the schema lacks %q", want)
		}
	}
	for _, want := range []string{
		"previous is the limit the pod template sets now, written as in the manifest (for example 48Mi or 50M)",
		"previousValue is the maxReplicaCount the ScaledObject's spec holds now",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the prompt lacks %q", want)
		}
	}
}
