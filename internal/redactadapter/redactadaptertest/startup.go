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

// Package redactadaptertest checks the registry inherited from a binary's startup.
package redactadaptertest

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/shadewright-labs/shadewright/internal/redact"
)

const (
	heapWaits    = "shadewright_redaction_heap_waits_total"
	withheld     = "shadewright_redaction_withheld_total"
	startupChild = "SHADEWRIGHT_REDACTION_METRICS_CHILD"
)

const zeroMetrics = `# HELP shadewright_redaction_heap_waits_total Redactions that waited for others to finish before running, so that the heap of the redactions running at once stays within its limit.
# TYPE shadewright_redaction_heap_waits_total counter
shadewright_redaction_heap_waits_total 0
# HELP shadewright_redaction_withheld_total Entries redaction withheld whole, by reason (work: the work budget ran out; size: longer than the bound; heap: more pieces or strings than its bytes allow; wait: the wait for the heap limit of redactions at once ended).
# TYPE shadewright_redaction_withheld_total counter
shadewright_redaction_withheld_total{reason="heap"} 0
shadewright_redaction_withheld_total{reason="size"} 0
shadewright_redaction_withheld_total{reason="wait"} 0
shadewright_redaction_withheld_total{reason="work"} 0
`

// CheckStartup checks the metric families in a fresh copy of the caller's test binary.
func CheckStartup(t *testing.T) {
	t.Helper()
	if os.Getenv(startupChild) != "1" {
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, exe, "-test.run=^"+t.Name()+"$") // #nosec G204 -- reruns this test binary
		cmd.Env = append(os.Environ(), startupChild+"=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("fresh process: %v\n%s", err, out)
		}
		return
	}

	plain, err := redact.New()
	if err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("a", redact.MaxShortEntry+1)
	for _, c := range []struct {
		name    string
		run     func(string) string
		metrics string
	}{
		{"startup", nil, zeroMetrics},
		{"redactor without hooks", plain.StringShort, zeroMetrics},
		{"package default", redact.StringShort,
			strings.Replace(zeroMetrics, `{reason="size"} 0`, `{reason="size"} 1`, 1)},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.run != nil && c.run(long) != redact.Withheld {
				t.Error("oversized short entry was released")
			}
			if err := promtest.GatherAndCompare(metrics.Registry, strings.NewReader(c.metrics), heapWaits, withheld); err != nil {
				t.Error(err)
			}
		})
	}
}

// WithheldCount returns the registry count for a refusal reason.
func WithheldCount(t *testing.T, reason string) float64 {
	t.Helper()
	families, err := metrics.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != withheld {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, label := range m.GetLabel() {
				if label.GetName() == "reason" && label.GetValue() == reason {
					return m.GetCounter().GetValue()
				}
			}
		}
	}
	t.Fatalf("missing %s series for %q", withheld, reason)
	return 0
}
