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

package clearing

import (
	"testing"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
)

func TestReportNameDerivesFromTheRequestUID(t *testing.T) {
	a := types.UID("0f6a3c2e-1b4d-4e5f-8a9b-0c1d2e3f4a5b")
	b := types.UID("1f6a3c2e-1b4d-4e5f-8a9b-0c1d2e3f4a5b")
	if got := ReportName(a); got != "vr-"+string(a) {
		t.Errorf("ReportName(%q) = %q, want vr- and the UID", a, got)
	}
	if ReportName(a) == ReportName(b) {
		t.Error("two request UIDs give the same report name")
	}
	if errs := validation.IsDNS1123Subdomain(ReportName(a)); len(errs) > 0 {
		t.Errorf("ReportName(%q) = %q is not a valid object name: %v", a, ReportName(a), errs)
	}
}
