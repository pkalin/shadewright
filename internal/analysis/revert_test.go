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
	"reflect"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	fx "github.com/pkalin/shadewright/internal/testfixtures"
)

// TestModelCannotProposeARevert: a model proposal has no way to set spec.reverts (ADR-0030 section 8). The
// response schema and the typed result have no reverts field anywhere, a result that adds one is refused as not
// matching the schema, and the spec the loop builds from a plan never carries one.
func TestModelCannotProposeARevert(t *testing.T) {
	if strings.Contains(strings.ToLower(string(ResponseSchema())), "revert") {
		t.Error("the response schema names a revert")
	}
	for _, typ := range []reflect.Type{reflect.TypeFor[Result](), reflect.TypeFor[Plan]()} {
		if path := fieldNamed(typ, "revert", map[reflect.Type]bool{}); path != "" {
			t.Errorf("%s has a field %s", typ.Name(), path)
		}
	}

	injected := strings.Replace(validResult, `"expectedEffect":`,
		`"reverts":{"name":"remediation-failed","uid":"16161616-1616-4616-8616-161616161616"},"expectedEffect":`, 1)
	if injected == validResult {
		t.Fatal("the fixture result has no expectedEffect to inject before")
	}
	if _, _, err := DecodeResult(injected); err == nil {
		t.Error("a result whose proposal carries reverts was decoded")
	}
	injected = strings.Replace(validResult, `"proposal":`,
		`"reverts":{"name":"remediation-failed","uid":"16161616-1616-4616-8616-161616161616"},"proposal":`, 1)
	if _, _, err := DecodeResult(injected); err == nil {
		t.Error("a result that carries reverts was decoded")
	}

	_, action, err := DecodeResult(validResult)
	if err != nil {
		t.Fatal(err)
	}
	inc := fx.Incident("default", "incident-a")
	inc.UID = "17171717-1717-4717-8717-171717171717"
	spec := requestSpec(inc, Plan{Action: *action, PolicyRef: "mem-policy"})
	if spec.Reverts != nil {
		t.Errorf("the spec of a model plan has spec.reverts %+v", spec.Reverts)
	}
	if spec.ApprovalRequired == nil || !*spec.ApprovalRequired {
		t.Error("the spec of a model plan does not require approval")
	}
}

// TestAnalysisNeverTakesUpAnExistingRevert: a request at the name of the loop's plan that holds exactly that
// plan is taken up as the loop's own, unless it carries spec.reverts. A revert is made only by the deterministic
// revert proposer (ADR-0030 section 8), so the loop refuses it instead of recording it as its proposal.
func TestAnalysisNeverTakesUpAnExistingRevert(t *testing.T) {
	_, action, err := DecodeResult(validResult)
	if err != nil {
		t.Fatal(err)
	}
	inc := fx.Incident("default", "incident-a")
	inc.UID = "18181818-1818-4818-8818-181818181818"
	spec := requestSpec(inc, Plan{Action: *action, PolicyRef: "mem-policy"})
	name, err := requestName(inc, spec)
	if err != nil {
		t.Fatal(err)
	}
	scheme := runtime.NewScheme()
	if err := remediationv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		reverts *remediationv1alpha1.RequestRef
		accept  bool
	}{
		{nil, true},
		{&remediationv1alpha1.RequestRef{Name: "remediation-failed", UID: "19191919-1919-4919-8919-191919191919"}, false},
	} {
		existing := &remediationv1alpha1.RemediationRequest{
			ObjectMeta: metav1.ObjectMeta{Namespace: "shadewright-system", Name: string(name)}, Spec: *spec.DeepCopy()}
		existing.Spec.Reverts = tc.reverts
		k := kfake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).Build()
		c := &Controller{Client: k, Reader: k, Namespace: existing.Namespace}
		got, err := c.createRequest(t.Context(), inc, spec)
		switch {
		case tc.accept && (err != nil || got != name):
			t.Errorf("the same plan without reverts: %q, %v", got, err)
		case !tc.accept && err == nil:
			t.Errorf("an existing request with spec.reverts was taken up as the plan's request %q", got)
		}
	}
}

// fieldNamed returns the path of the first field of typ, at any depth, whose Go or JSON name contains part
// (case folded), or "".
func fieldNamed(typ reflect.Type, part string, seen map[reflect.Type]bool) string {
	for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice || typ.Kind() == reflect.Map {
		typ = typ.Elem()
	}
	if typ.Kind() != reflect.Struct || seen[typ] {
		return ""
	}
	seen[typ] = true
	for f := range typ.Fields() {
		tag, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if strings.Contains(strings.ToLower(f.Name), part) || strings.Contains(strings.ToLower(tag), part) {
			return f.Name
		}
		if sub := fieldNamed(f.Type, part, seen); sub != "" {
			return f.Name + "." + sub
		}
	}
	return ""
}

// The spec type does carry the field, so fieldNamed finds it there: the check above is not vacuous.
func TestFieldNamedFindsReverts(t *testing.T) {
	if fieldNamed(reflect.TypeFor[remediationv1alpha1.RemediationRequestSpec](), "revert", map[reflect.Type]bool{}) != "Reverts" {
		t.Error("fieldNamed does not find spec.reverts")
	}
}
