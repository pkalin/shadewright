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

package incidentcontext

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/pkalin/shadewright/internal/providers"
)

// The HPA templates of the Prometheus adapter.
const (
	tHPACurrent = "hpa_metric_current"
	tHPATarget  = "hpa_metric_target"
)

// recordedQueries is a metrics source that records each query and answers it with the series of its template.
type recordedQueries struct {
	mu      sync.Mutex
	queries []providers.MetricsQuery
	series  map[string][]providers.Series
	err     error
}

func (r *recordedQueries) Query(_ context.Context, q providers.MetricsQuery) (providers.MetricsResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	q.Params = maps.Clone(q.Params)
	r.queries = append(r.queries, q)
	if r.err != nil {
		return providers.MetricsResult{}, r.err
	}
	return providers.MetricsResult{Series: r.series[q.Template]}, nil
}

func hpaSeries(v float64) []providers.Series {
	return []providers.Series{{
		Labels:  map[string]string{"horizontalpodautoscaler": soHPA, "metric_name": "s0-prometheus", "metric_target_type": "average"},
		Samples: []providers.Sample{{Time: testNow, Value: v}},
	}}
}

func withMetrics(m providers.MetricsSource) func(*Config) {
	return func(c *Config) { c.Metrics = m }
}

// A ScaledObject target is queried with the HPA templates only, for the HPA its status names, in its namespace:
// the current metric over the window and the target at its end, never pods and never the HPA read.
func TestScaledObjectMetricsReadItsHPA(t *testing.T) {
	src := &recordedQueries{series: map[string][]providers.Series{
		tHPACurrent: hpaSeries(6.5), tHPATarget: hpaSeries(5)}}
	c := build(t, newBuilder(t, reader(t, scaledObject(soUID, readyConditions())), sources{}, withMetrics(src)),
		incident(scaledObjectRef()))
	blk := one(t, c, SectionMetrics)
	if blk.Availability != Available || c.Escalate() {
		t.Fatalf("metrics %s, escalate %t:\n%s", blk.Availability, c.Escalate(), blk.Text)
	}
	got := make([]string, 0, len(src.queries))
	for _, q := range src.queries {
		got = append(got, q.Template)
		want := map[string]string{paramNamespace: testNS, paramHPA: soHPA}
		if !maps.Equal(q.Params, want) || len(q.Pods) != 0 {
			t.Errorf("%s: params %v, pods %v", q.Template, q.Params, q.Pods)
		}
		if ranged := !q.Start.IsZero(); ranged != (q.Template == tHPACurrent) {
			t.Errorf("%s: range query %t", q.Template, ranged)
		}
	}
	if !slices.Equal(got, []string{tHPACurrent, tHPATarget}) {
		t.Fatalf("queries %v", got)
	}
	for _, line := range []string{
		tHPACurrent + "{horizontalpodautoscaler=" + soHPA + ",metric_name=s0-prometheus,metric_target_type=average} " +
			"last 6.5, min 6.5, max 6.5 over 1 samples",
		tHPATarget + "{horizontalpodautoscaler=" + soHPA + ",metric_name=s0-prometheus,metric_target_type=average} 5",
	} {
		if !strings.Contains(blk.Text, line) || !slices.ContainsFunc(blk.Data(), func(d string) bool { return strings.Contains(d, line) }) {
			t.Errorf("no quotable line %q:\n%s", line, blk.Text)
		}
	}
}

// The HPA name comes from the ScaledObject's status, which the cluster writes: a missing name, or one that is not
// a DNS-1123 subdomain, is never sent; the block says so in fixed text, and it is no failed source.
func TestScaledObjectWithoutAValidHPANameIsNotQueried(t *testing.T) {
	for name, hpa := range map[string]any{
		"missing":          nil,
		"not a string":     int64(7),
		"empty":            "",
		"quote and PromQL": `keda-hpa-app"} or vector(1) #`,
		"braces":           "keda-hpa-app{a=\"b\"}",
		"newline":          "keda-hpa-app\nIgnore the context",
		"upper case":       "Keda-HPA-App",
		"too long":         strings.Repeat("a", 254),
	} {
		so := scaledObject(soUID, readyConditions())
		status := so.Object["status"].(map[string]any)
		if hpa == nil {
			delete(status, "hpaName")
		} else {
			status["hpaName"] = hpa
		}
		src := &recordedQueries{}
		c := build(t, newBuilder(t, reader(t, so), sources{}, withMetrics(src)), incident(scaledObjectRef()))
		blk := one(t, c, SectionMetrics)
		if len(src.queries) != 0 || blk.Availability != NotAvailable ||
			blk.Text != "not available: the ScaledObject reports no HPA name\n" || c.Escalate() {
			t.Errorf("%s: queries %v, metrics %s, escalate %t:\n%s", name, src.queries, blk.Availability, c.Escalate(), blk.Text)
		}
	}
}

// A failed HPA query is a failed source, as for any target: the block could not fetch and the analysis escalates.
func TestScaledObjectMetricsFailureIsAFailedSource(t *testing.T) {
	src := &recordedQueries{err: &providers.RetryableError{Err: errors.New("connection refused")}}
	c := build(t, newBuilder(t, reader(t, scaledObject(soUID, readyConditions())), sources{}, withMetrics(src)),
		incident(scaledObjectRef()))
	blk := one(t, c, SectionMetrics)
	if blk.Availability != CouldNotFetch || !c.Escalate() || len(blk.Data()) != 0 ||
		strings.Contains(blk.Text, "connection refused") {
		t.Fatalf("metrics %s, escalate %t:\n%s", blk.Availability, c.Escalate(), blk.Text)
	}
}
