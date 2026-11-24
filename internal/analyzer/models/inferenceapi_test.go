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

package models

import (
	"testing"

	remediationv1alpha1 "github.com/shadewright-labs/shadewright/api/v1alpha1"
	"github.com/shadewright-labs/shadewright/internal/providers/model/foundry"
)

// TestInferenceAPIValuesMatchTheAdapter pins that the values of the settings field are the adapter's own: the
// deployment's value is passed to the adapter by a conversion of the string, so a value added to one side only
// would be refused late, when the provider is built.
func TestInferenceAPIValuesMatchTheAdapter(t *testing.T) {
	for crd, adapter := range map[remediationv1alpha1.InferenceAPI]foundry.API{
		remediationv1alpha1.InferenceAPIChatCompletions: foundry.APIChatCompletions,
		remediationv1alpha1.InferenceAPIResponses:       foundry.APIResponses,
	} {
		if string(crd) != string(adapter) {
			t.Errorf("settings value %q is %q in the adapter", crd, adapter)
		}
	}
}
