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
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/yaml"

	"github.com/pkalin/shadewright/internal/gitops/semdiff"
)

// TestRenderCPURefusalWithinTheStep checks, by computation, that the render process reaches its CPU time limit
// (semdiff.MaxRenderCPUSeconds) inside the pull request step that runs the render check (DefaultWriteTimeout),
// at the executor's CPU limit in the chart (#614, ADR-0035). At that share a second of CPU time takes 1/share
// seconds of wall time. The step also loads the build and may wait for one render ahead of it in the render
// slot, so two renders at the limit must fit in it. Otherwise the step's own deadline ends the render first,
// the check is retried as ErrTimeout, and the refusal never arrives.
func TestRenderCPURefusalWithinTheStep(t *testing.T) {
	b, err := os.ReadFile("../../dist/chart/values.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var values struct {
		Executor struct {
			Resources struct {
				Limits struct {
					CPU string `json:"cpu"`
				} `json:"limits"`
			} `json:"resources"`
		} `json:"executor"`
	}
	if err := yaml.Unmarshal(b, &values); err != nil {
		t.Fatal(err)
	}
	limit, err := resource.ParseQuantity(values.Executor.Resources.Limits.CPU)
	if err != nil || limit.MilliValue() <= 0 {
		t.Fatalf("executor CPU limit %q: %v", values.Executor.Resources.Limits.CPU, err)
	}
	wall := time.Duration(semdiff.MaxRenderCPUSeconds) * time.Second * 1000 / time.Duration(limit.MilliValue())
	if 2*wall >= DefaultWriteTimeout {
		t.Fatalf("at %s of CPU, %d s of CPU time take %s of wall time, and two renders need more than the step's %s",
			&limit, semdiff.MaxRenderCPUSeconds, wall, DefaultWriteTimeout)
	}
}
