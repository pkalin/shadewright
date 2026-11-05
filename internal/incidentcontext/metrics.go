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
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"

	"github.com/pkalin/shadewright/internal/providers"
)

// MaxMetricPods bounds the pods named in one metrics query, most relevant first, so that the series of
// their containers stay within what the source answers (design section 4); the pod set is labelled capped
// when it has more.
const MaxMetricPods = 16

// Parameters of the metric templates (internal/providers/metrics/prometheus).
const (
	paramNamespace = "namespace"
	paramWorkload  = "workload"
)

// metricPlan is one approved template the builder asks for.
type metricPlan struct {
	template string
	// pods: the template selects the target's pods; otherwise it takes the workload parameter.
	pods bool
	// overWindow: a range query over the window, shown as last, minimum and maximum; otherwise an instant
	// query at the end of the window.
	overWindow bool
}

// The fixed lines of a metrics block follow a template name: code text with no data from the source, which
// [Block.Data] leaves out (only series lines are recorded as values).
const (
	fixedTooLarge      = ": answer too large for the pods asked"
	fixedCouldNotFetch = ": could not fetch"
	fixedNoSeries      = ": no series"
	partialSuffix      = " (partial answer)"
)

// metricPlans are in order of relevance, so that a cut keeps what explains a crash first.
var metricPlans = []metricPlan{
	{template: "container_last_terminated_oomkilled", pods: true},
	{template: "container_waiting_reason", pods: true},
	{template: "container_restarts_total", pods: true},
	{template: "container_memory_working_set_bytes", pods: true, overWindow: true},
	{template: "container_memory_limit_bytes", pods: true},
	{template: "container_cpu_usage_cores", pods: true, overWindow: true},
	{template: "container_cpu_throttled_ratio", pods: true},
	{template: "deployment_replicas_unavailable"},
}

// rangeStep is the step of a range query: about 60 points over the window, at least one minute.
func rangeStep(w window) time.Duration {
	return max(time.Minute, (w.end.Sub(w.start) / 60).Truncate(time.Minute))
}

// metricsBlock queries the approved templates for the target. It reports whether the metrics source failed,
// and returns an error only when ctx is done.
func (b *Builder) metricsBlock(ctx context.Context, t *target, w window) (Block, bool, error) {
	if b.metrics == nil {
		return notAvailable(SectionMetrics, ProvenanceMetric, "not available: no metrics source is configured\n"), false, nil
	}
	if t.state != targetResolved {
		return notAvailable(SectionMetrics, ProvenanceMetric, "not available: the target was not resolved\n"), false, nil
	}
	pods := make([]string, 0, min(len(t.pods), MaxMetricPods))
	for i := range t.pods[:min(len(t.pods), MaxMetricPods)] {
		pods = append(pods, t.pods[i].Name)
	}
	_, deployment := t.workload.(*appsv1.Deployment)
	blk := Block{Section: SectionMetrics, Provenance: ProvenanceMetric, Availability: Available}
	b.podLabels(&blk, t)
	if len(t.pods) > MaxMetricPods {
		blk.addLabel(LabelPodsCapped)
	}
	var l lines
	asked, failed := 0, 0
	for _, plan := range metricPlans {
		q := providers.MetricsQuery{Template: plan.template, Params: map[string]string{paramNamespace: t.ref.Namespace}, End: w.end}
		switch {
		case plan.pods && len(pods) > 0:
			q.Pods = pods
		case !plan.pods && deployment:
			q.Params[paramWorkload] = t.ref.Name
		default:
			continue
		}
		if plan.overWindow {
			q.Start, q.Step = w.start, rangeStep(w)
		}
		asked++
		var res providers.MetricsResult
		err := b.call(ctx, func(ctx context.Context) error {
			var err error
			res, err = b.metrics.Query(ctx, q)
			return err
		})
		switch {
		case err == nil:
			b.seriesLines(&l, plan, res)
			if res.Partial {
				blk.addLabel(LabelMetricsPartial)
			}
		case abandoned(ctx) != nil:
			return Block{}, false, abandoned(ctx)
		case errors.Is(err, providers.ErrTooLarge):
			// More series than the source answers: the evidence is capped, which is not a failure.
			blk.addLabel(LabelPodsCapped)
			l.add("%s%s", plan.template, fixedTooLarge)
		default:
			failed++
			l.add("%s%s", plan.template, fixedCouldNotFetch)
		}
	}
	if asked == 0 {
		return notAvailable(SectionMetrics, ProvenanceMetric, "not available: no pod of the target to query\n"), false, nil
	}
	switch {
	case failed == asked:
		blk.Availability = CouldNotFetch
	case failed > 0:
		blk.addLabel(LabelIncomplete)
	}
	b.finish(&blk, &l, b.limits.Metrics)
	return blk, failed > 0, nil
}

// seriesLines writes one line per series, sorted by labels.
func (b *Builder) seriesLines(l *lines, plan metricPlan, res providers.MetricsResult) {
	partial := ""
	if res.Partial {
		partial = partialSuffix
	}
	if len(res.Series) == 0 {
		l.add("%s%s%s", plan.template, fixedNoSeries, partial)
		return
	}
	out := make([]string, 0, len(res.Series))
	for _, s := range res.Series {
		keys := slices.Sorted(maps.Keys(s.Labels))
		pairs := make([]string, 0, len(keys))
		for _, k := range keys {
			pairs = append(pairs, b.value(k)+"="+b.value(s.Labels[k]))
		}
		// A series line is the source's answer; the partial mark after it is code.
		out = append(out, l.data(plan.template+"{"+strings.Join(pairs, ",")+"} "+valuesOf(plan, s.Samples))+partial)
	}
	slices.Sort(out)
	for _, line := range out {
		l.add("%s", line)
	}
}

func num(v float64) string {
	return strconv.FormatFloat(v, 'g', 6, 64)
}

func valuesOf(plan metricPlan, samples []providers.Sample) string {
	if len(samples) == 0 {
		return "no samples"
	}
	last := samples[len(samples)-1].Value
	if !plan.overWindow {
		return num(last)
	}
	lo, hi := last, last
	for _, s := range samples {
		lo, hi = min(lo, s.Value), max(hi, s.Value)
	}
	return "last " + num(last) + ", min " + num(lo) + ", max " + num(hi) + " over " + strconv.Itoa(len(samples)) + " samples"
}
