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

// TestNodeGraceFollowsTheValue: executor.verification.nodeGrace passes --node-grace to the executor only, one
// minute by default (ADR-0030 section 4).
func TestNodeGraceFollowsTheValue(t *testing.T) {
	for name, c := range map[string]struct {
		set  []string
		want string
	}{
		"chart default": {nil, "--node-grace=1m"},
		"set":           {[]string{"executor.verification.nodeGrace=90s"}, "--node-grace=90s"},
	} {
		t.Run(name, func(t *testing.T) {
			r := defaultRender(t, c.set...)
			if args := container(t, r, executorDeployment).Args; !slices.Contains(args, c.want) {
				t.Errorf("executor args %v do not contain %s", args, c.want)
			}
			for _, a := range container(t, r, analyzerDeployment).Args {
				if strings.HasPrefix(a, "--node-grace") {
					t.Errorf("analyzer got %s", a)
				}
			}
		})
	}
}

// TestNodeGraceIsRefusedAsAnExtraArgOrNotADuration: the flag is set only through the value, and the schema
// refuses a value that is not a Go duration.
func TestNodeGraceIsRefusedAsAnExtraArgOrNotADuration(t *testing.T) {
	for set, want := range map[string]string{
		"executor.extraArgs={--node-grace=5m}":   "executor.verification.nodeGrace",
		"analyzer.extraArgs={--node-grace=5m}":   "the chart sets it",
		"executor.verification.nodeGrace=a-week": "nodeGrace",
		"executor.verification.other=1":          "other",
	} {
		t.Run(set, func(t *testing.T) {
			if out := helmFails(t, set); !strings.Contains(out, want) {
				t.Errorf("error does not mention %q:\n%s", want, out)
			}
		})
	}
}
