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

const verifyOutcomesFlag = "--verify-outcomes"

// defaultCase names the case of a table that renders the chart with its default values.
const defaultCase = "default"

// TestVerifyOutcomesFollowTheValue: executor.verification.enabled passes --verify-outcomes to the executor only,
// and the default passes it to neither (off until the executor reads the verifier's report, ADR-0030).
func TestVerifyOutcomesFollowTheValue(t *testing.T) {
	cases := map[string]struct {
		set  []string
		want bool
	}{
		defaultCase: {nil, false},
		"off":       {[]string{"executor.verification.enabled=false"}, false},
		"on":        {[]string{"executor.verification.enabled=true"}, true},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			r := defaultRender(t, c.set...)
			args := container(t, r, executorDeployment).Args
			if got := slices.Contains(args, verifyOutcomesFlag); got != c.want {
				t.Errorf("executor args contain %s: %v, want %v", verifyOutcomesFlag, got, c.want)
			}
			if !c.want {
				for _, a := range args {
					if strings.HasPrefix(a, verifyOutcomesFlag) {
						t.Errorf("executor got %s", a)
					}
				}
			}
			for _, a := range container(t, r, analyzerDeployment).Args {
				if strings.HasPrefix(a, verifyOutcomesFlag) {
					t.Errorf("analyzer got %s", a)
				}
			}
		})
	}
}

// TestVerifyOutcomesAreRefusedAsAnExtraArgOrANonBoolean: the flag is set only through the value, and the schema
// refuses a value that is not a boolean.
func TestVerifyOutcomesAreRefusedAsAnExtraArgOrANonBoolean(t *testing.T) {
	for set, want := range map[string]string{
		"executor.extraArgs={--verify-outcomes}":      "executor.verification.enabled",
		"executor.extraArgs={--verify-outcomes=true}": "executor.verification.enabled",
		"executor.verification.enabled=yes":           "enabled",
	} {
		t.Run(set, func(t *testing.T) {
			if out := helmFails(t, set); !strings.Contains(out, want) {
				t.Errorf("error does not mention %q:\n%s", want, out)
			}
		})
	}
}
