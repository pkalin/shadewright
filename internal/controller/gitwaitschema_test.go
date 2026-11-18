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

package controller

import (
	"context"
	"os"
	"testing"
	"time"

	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	celvalidation "k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	crdvalidation "k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/yaml"

	api "github.com/pkalin/shadewright/api/v1alpha1"
	fx "github.com/pkalin/shadewright/internal/testfixtures"
)

// statusSchema checks a RemediationRequest status against the OpenAPI schema of the generated CRD (lengths,
// patterns, enums, required fields), as the API server does before any rule; the fake client checks none of it.
type statusSchema struct {
	validator crdvalidation.SchemaValidator
	// rules are the CEL rules of the whole object (x-kubernetes-validations), transition rules (oldSelf) included,
	// and structural their schema.
	rules      *celvalidation.Validator
	structural *structuralschema.Structural
}

// The limits the API server gives the CEL rules of a custom resource (k8s.io/apiserver/pkg/apis/cel: PerCallLimit
// and RuntimeCELCostBudget).
const (
	celPerCallLimit = 1000000
	celCostBudget   = 10000000
)

// requestStatusSchema loads the status schema of the RemediationRequest CRD.
func requestStatusSchema(t *testing.T) *statusSchema {
	t.Helper()
	data, err := os.ReadFile("../../config/crd/bases/remediation.shadewright.io_remediationrequests.yaml")
	if err != nil {
		t.Fatal(err)
	}
	crd := &apiextensionsv1.CustomResourceDefinition{}
	if err := yaml.Unmarshal(data, crd); err != nil {
		t.Fatal(err)
	}
	status := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["status"]
	internal := &apiextensions.JSONSchemaProps{}
	if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(&status, internal, nil); err != nil {
		t.Fatal(err)
	}
	v, _, err := crdvalidation.NewSchemaValidator(internal)
	if err != nil {
		t.Fatal(err)
	}
	whole := &apiextensions.JSONSchemaProps{}
	if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(
		crd.Spec.Versions[0].Schema.OpenAPIV3Schema, whole, nil); err != nil {
		t.Fatal(err)
	}
	structural, err := structuralschema.NewStructural(whole)
	if err != nil {
		t.Fatal(err)
	}
	rules := celvalidation.NewValidator(structural, true, celPerCallLimit)
	if rules == nil {
		t.Fatal("the RemediationRequest CRD has no CEL rules")
	}
	return &statusSchema{validator: v, rules: rules, structural: structural}
}

// validateUpdate returns what the CEL rules of the CRD refuse in an update of old to rr, as the API server checks them
// on a write: the rules on the object and its fields, and the transition rules that compare with oldSelf (an entry of
// status.gitWaits that keeps its start and end, a handoff that moves only forward). old nil is a create.
func (s *statusSchema) validateUpdate(old, rr *api.RemediationRequest) field.ErrorList {
	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(rr)
	if err != nil {
		return field.ErrorList{field.InternalError(field.NewPath(""), err)}
	}
	var oldObj any
	if old != nil {
		o, err := runtime.DefaultUnstructuredConverter.ToUnstructured(old)
		if err != nil {
			return field.ErrorList{field.InternalError(field.NewPath(""), err)}
		}
		oldObj = o
	}
	errs, _ := s.rules.Validate(context.Background(), nil, s.structural, obj, oldObj, celCostBudget)
	return errs
}

// validate returns what the schema refuses in rr's status.
func (s *statusSchema) validate(rr *api.RemediationRequest) field.ErrorList {
	u, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&rr.Status)
	if err != nil {
		return field.ErrorList{field.InternalError(field.NewPath("status"), err)}
	}
	return crdvalidation.ValidateCustomResource(field.NewPath("status"), u, s.validator)
}

// The schema check refuses an operation longer than the schema allows, so the model sees what the API server
// refuses.
func TestStatusSchemaRefusesALongOperation(t *testing.T) {
	s := requestStatusSchema(t)
	rr := &api.RemediationRequest{}
	rr.Status.GitWaits = []api.GitWait{{Kind: api.GitWaitPaced, Since: metav1.Now(), Operation: "count the commits the pull request " +
		"carried over " + longBranch}}
	if errs := s.validate(rr); len(errs) == 0 {
		t.Fatal("an operation past the schema's maxLength was accepted")
	}
	rr.Status.GitWaits[0].Operation = "count the commits"
	if errs := s.validate(rr); len(errs) != 0 {
		t.Fatalf("a valid entry was refused: %v", errs)
	}
}

// The transition rules refuse a Git wait whose start or end moved while it stands, and an incoming handoff moved from
// Aborted to Done, so the generated model, which checks every status write with them, fails on either.
func TestStatusTransitionsRefuseAMovedWaitAndAHandoffBack(t *testing.T) {
	s := requestStatusSchema(t)
	at := metav1.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	old := fx.MemoryLimitRequest("ns", "new")
	old.Spec.Replaces = &api.RequestReplacement{Name: oldName, UID: "u"}
	old.Spec.ChainRoot = "u"
	old.Status.Phase = api.PhasePolicyChecked
	old.Status.GitWaits = []api.GitWait{{Kind: api.GitWaitHandoffGrace, Since: at, Operation: branchHeadRead,
		Ends: &metav1.Time{Time: at.Add(15 * time.Minute)}}}
	if errs := s.validateUpdate(old, old.DeepCopy()); len(errs) > 0 {
		t.Fatalf("an unchanged request is refused: %v", errs)
	}
	moved := old.DeepCopy()
	moved.Status.GitWaits[0].Ends = &metav1.Time{Time: at.Add(24 * time.Hour)}
	if errs := s.validateUpdate(old, moved); len(errs) == 0 {
		t.Fatal("an end that moved was accepted")
	}
	moved = old.DeepCopy()
	moved.Status.GitWaits[0].Since = metav1.NewTime(at.Add(time.Minute))
	if errs := s.validateUpdate(old, moved); len(errs) == 0 {
		t.Fatal("a start that moved was accepted")
	}
	aborted := old.DeepCopy()
	aborted.Status.GitWaits = nil
	aborted.Status.Phase = api.PhaseNeedsIntervention
	aborted.Status.Handoff = &api.Handoff{From: &api.IncomingHandoff{Name: oldName, UID: "u", Phase: api.HandoffAborted}}
	done := aborted.DeepCopy()
	done.Status.Handoff.From.Phase = api.HandoffDone
	if errs := s.validateUpdate(aborted, done); len(errs) == 0 {
		t.Fatal("an incoming handoff moved from Aborted to Done was accepted")
	}
}
