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
	"strconv"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/pkalin/shadewright/internal/gitops/semdiff"
)

func goMemLimit(t *testing.T, r rendered, k string) string {
	t.Helper()
	var found []string
	for _, e := range container(t, r, k).Env {
		if e.Name == "GOMEMLIMIT" {
			found = append(found, e.Value)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%s: want one GOMEMLIMIT, got %v", k, found)
	}
	return found[0]
}

// TestGoMemLimitFollowsTheMemoryLimit: the analyzer and the verifier get 90 percent of their memory limit in
// bytes, the executor 45 percent (the rest is for its render process, ADR-0035), by default and after a change
// to the limit, and the override wins (#601). The executor's default stays 241591910 bytes, as it was with 90
// percent of the former 256Mi.
func TestGoMemLimitFollowsTheMemoryLimit(t *testing.T) {
	tests := map[string]struct {
		set          []string
		wantExecutor string
		wantAnalyzer string
	}{
		"default 512Mi and 128Mi": {nil, "241591910", "120795955"},
		"128Mi executor only":     {[]string{"executor.resources.limits.memory=128Mi"}, "60397977", "120795955"},
		"1Gi analyzer":            {[]string{"analyzer.resources.limits.memory=1Gi"}, "241591910", "966367641"},
		"decimal 1000M":           {[]string{"executor.resources.limits.memory=1000M"}, "450000000", "120795955"},
		"plain bytes":             {[]string{"executor.resources.limits.memory=134217728"}, "60397977", "120795955"},
		"override":                {[]string{"executor.goMemLimit=100MiB", "analyzer.goMemLimit=64MiB"}, "100MiB", "64MiB"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			r := defaultRender(t, append([]string{withVerifier}, tc.set...)...)
			if got := goMemLimit(t, r, executorDeployment); got != tc.wantExecutor {
				t.Errorf("executor GOMEMLIMIT = %s, want %s", got, tc.wantExecutor)
			}
			if got := goMemLimit(t, r, analyzerDeployment); got != tc.wantAnalyzer {
				t.Errorf("analyzer GOMEMLIMIT = %s, want %s", got, tc.wantAnalyzer)
			}
			// The verifier's memory limit is 128Mi in every case here, so its default is 90 percent of that.
			if got := goMemLimit(t, r, verifierDeployment); got != "120795955" {
				t.Errorf("verifier GOMEMLIMIT = %s, want 120795955", got)
			}
		})
	}
}

// TestExecutorMemoryLimitHoldsARenderProcess checks the executor's memory budget with the default values
// (ADR-0035): its Go soft limit, plus the render process's resident limit (semdiff.MaxRenderResident, where
// the executor kills it), plus the margin (semdiff.RenderOutsideData), fits in the container's memory limit.
// On cgroup v2 a container OOM ends the executor with the render process, so a render the samplers stop must
// not cause one; the test fails when either side changes.
func TestExecutorMemoryLimitHoldsARenderProcess(t *testing.T) {
	r := defaultRender(t)
	limit, ok := container(t, r, executorDeployment).Resources.Limits[corev1.ResourceMemory]
	if !ok {
		t.Fatal("the executor has no memory limit")
	}
	soft, err := strconv.ParseInt(goMemLimit(t, r, executorDeployment), 10, 64)
	if err != nil {
		t.Fatalf("the executor's GOMEMLIMIT is not in bytes: %v", err)
	}
	need := soft + semdiff.MaxRenderResident + semdiff.RenderOutsideData
	if need > limit.Value() {
		t.Fatalf("GOMEMLIMIT %d + MaxRenderResident %d + RenderOutsideData %d = %d bytes, over the memory limit %s (%d)",
			soft, semdiff.MaxRenderResident, semdiff.RenderOutsideData, need, limit.String(), limit.Value())
	}
}

func TestChartRefusesAMemoryLimitItCannotReadWithoutAnOverride(t *testing.T) {
	out := helmFails(t, "executor.resources.limits.memory=1.5Gi")
	if !strings.Contains(out, "executor.goMemLimit") {
		t.Errorf("error does not ask for executor.goMemLimit:\n%s", out)
	}
	if out := helmFails(t, "analyzer.goMemLimit=lots"); !strings.Contains(out, "goMemLimit") {
		t.Errorf("a malformed override is not refused:\n%s", out)
	}
	r := defaultRender(t, "executor.resources.limits.memory=1.5Gi", "executor.goMemLimit=1GiB")
	if got := goMemLimit(t, r, executorDeployment); got != "1GiB" {
		t.Errorf("GOMEMLIMIT = %s, want 1GiB", got)
	}
	if out := helmFails(t, "verifier.resources.limits.memory=1.5Gi"); !strings.Contains(out, "verifier.goMemLimit") {
		t.Errorf("error does not ask for verifier.goMemLimit:\n%s", out)
	}
	r = defaultRender(t, withVerifier, "verifier.goMemLimit=64MiB")
	if got := goMemLimit(t, r, verifierDeployment); got != "64MiB" {
		t.Errorf("verifier GOMEMLIMIT = %s, want 64MiB", got)
	}
}
