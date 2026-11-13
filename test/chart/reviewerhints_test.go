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

package chart

import (
	"slices"
	"strings"
	"testing"
)

const reviewerHintsFlag = "--reviewer-hints"

// TestReviewerHintsFollowTheValue: executor.reviewerHints.enabled passes --reviewer-hints to the executor only,
// and the default passes it to neither (hints are opt-in, ADR-0028 section 5).
func TestReviewerHintsFollowTheValue(t *testing.T) {
	cases := map[string]struct {
		set  []string
		want bool
	}{
		defaultCase: {nil, false},
		// A null value drops the key from the merged values; the chart reads it as off and does not fail.
		"null": {[]string{"executor.reviewerHints=null"}, false},
		"off":  {[]string{"executor.reviewerHints.enabled=false"}, false},
		"on":   {[]string{"executor.reviewerHints.enabled=true"}, true},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			r := defaultRender(t, c.set...)
			args := container(t, r, executorDeployment).Args
			if got := slices.Contains(args, reviewerHintsFlag); got != c.want {
				t.Errorf("executor args contain %s: %v, want %v", reviewerHintsFlag, got, c.want)
			}
			if !c.want {
				// Off means no form of the flag at all, not only the bare one.
				for _, a := range args {
					if strings.HasPrefix(a, reviewerHintsFlag) {
						t.Errorf("executor got %s", a)
					}
				}
			}
			for _, a := range container(t, r, analyzerDeployment).Args {
				if strings.HasPrefix(a, reviewerHintsFlag) {
					t.Errorf("analyzer got %s", a)
				}
			}
		})
	}
}

// TestReviewerHintsAreRefusedAsAnExtraArgOrANonBoolean: the flag is set only through the value, in any form, the
// analyzer never gets it, and the schema refuses a value that is not a boolean or an unknown key.
func TestReviewerHintsAreRefusedAsAnExtraArgOrANonBoolean(t *testing.T) {
	for set, want := range map[string]string{
		"executor.extraArgs={--reviewer-hints}":      "executor.reviewerHints.enabled",
		"executor.extraArgs={--reviewer-hints=true}": "executor.reviewerHints.enabled",
		// The analyzer has no such flag: the message names the executor's value, not a flag the chart sets.
		"analyzer.extraArgs={--reviewer-hints}":   "is an executor flag",
		"executor.reviewerHints.enabled=yes":      "enabled",
		"executor.reviewerHints.requiredCheck=on": "requiredCheck",
	} {
		t.Run(set, func(t *testing.T) {
			if out := helmFails(t, set); !strings.Contains(out, want) {
				t.Errorf("error does not mention %q:\n%s", want, out)
			}
		})
	}
}
