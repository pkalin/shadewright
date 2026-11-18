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
	"os"
	"testing"

	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	crdvalidation "k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/yaml"

	api "github.com/pkalin/shadewright/api/v1alpha1"
)

// statusSchema checks a RemediationRequest status against the OpenAPI schema of the generated CRD (lengths,
// patterns, enums, required fields), as the API server does before any rule; the fake client checks none of it.
type statusSchema struct {
	validator crdvalidation.SchemaValidator
}

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
	return &statusSchema{validator: v}
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
