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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/pkalin/shadewright/internal/kedaready"
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
	paramHPA       = "hpa"
)

// metricPlan is one approved template the builder asks for.
type metricPlan struct {
	template string
	// pods: the template selects the target's pods; otherwise it takes the workload parameter, or the hpa
	// parameter when hpa is set.
	pods bool
	// hpa: the template reads the HorizontalPodAutoscaler of a ScaledObject target, named by its status.
	hpa bool
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
	// fixedLeftOut follows the count of series the source left out for a free-text label value (the metric name
	// of an HPA) that is not a plain name; no part of the label is shown.
	fixedLeftOut  = " series left out because of a label value that is not a plain name"
	partialSuffix = " (partial answer)"
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
	{template: "hpa_metric_current", hpa: true, overWindow: true},
	{template: "hpa_metric_target", hpa: true},
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
	hpa := hpaOf(t)
	if t.scaledObject != nil && hpa == "" {
		return notAvailable(SectionMetrics, ProvenanceMetric, "not available: the ScaledObject reports no HPA name\n"), false, nil
	}
	if hpa != "" {
		blk, ok, err := b.checkHPA(ctx, t, hpa)
		if err != nil || !ok {
			return blk, blk.Availability == CouldNotFetch, err
		}
	}
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
		case plan.hpa:
			if hpa == "" {
				continue
			}
			q.Params[paramHPA] = hpa
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

// hpaOf returns the name of the HorizontalPodAutoscaler a ScaledObject target reports in status.hpaName, the
// HPA KEDA created for it, or "" when the target is not a ScaledObject or the name is missing or not a DNS-1123
// subdomain. The name selects the HPA's series of kube-state-metrics in the target's namespace, once
// [Builder.checkHPA] has found that HPA to be the ScaledObject's own.
func hpaOf(t *target) string {
	if t.scaledObject == nil {
		return ""
	}
	status, _ := t.scaledObject.Object["status"].(map[string]any)
	name, _ := status["hpaName"].(string)
	if len(validation.IsDNS1123Subdomain(name)) > 0 {
		return ""
	}
	return name
}

// Fixed texts of the metrics block of a ScaledObject whose HPA gives no templates. None quotes the cluster.
const (
	fixedHPANotFound   = "not available: the HPA the ScaledObject names was not found\n"
	fixedHPANotItsOwn  = "not available: the HPA the ScaledObject names does not scale its target for it\n"
	fixedHPAReadFailed = "could not fetch: reading the HPA the ScaledObject names failed\n"
)

// checkHPA reads the HPA a ScaledObject target names in status.hpaName from the analyzer's cache, which watches
// the HPAs of the watched namespaces (as unstructured autoscaling/v2 objects, the collector's informer), and
// reports whether its metric templates may be asked (ADR-0036, amendment of 2026-11-16). The HPA must be
// controlled by the ScaledObject of the Incident, matched by UID, and its spec.scaleTargetRef must name the
// ScaledObject's own scale target (kind, API group and name; KEDA's default kind is a Deployment). Otherwise the
// block is "not available" with a fixed reason. A failed read is a failed source, never "not found", so the
// analysis escalates and a later build reads again; the error text is not shown. It returns an error only when
// ctx is done.
func (b *Builder) checkHPA(ctx context.Context, t *target, name string) (Block, bool, error) {
	hpa := &unstructured.Unstructured{}
	hpa.SetGroupVersionKind(schema.GroupVersionKind{Group: "autoscaling", Version: "v2", Kind: "HorizontalPodAutoscaler"})
	err := b.call(ctx, func(ctx context.Context) error {
		return b.pods.Get(ctx, client.ObjectKey{Namespace: t.ref.Namespace, Name: name}, hpa)
	})
	switch {
	case err == nil:
	case abandoned(ctx) != nil:
		return Block{}, false, abandoned(ctx)
	case apierrors.IsNotFound(err):
		return notAvailable(SectionMetrics, ProvenanceMetric, fixedHPANotFound), false, nil
	default:
		blk := notAvailable(SectionMetrics, ProvenanceMetric, fixedHPAReadFailed)
		blk.Availability = CouldNotFetch
		return blk, false, nil
	}
	owner := metav1.GetControllerOfNoCopy(hpa)
	if owner == nil || owner.Kind != kedaready.KindScaledObject || groupOf(owner.APIVersion) != kedaready.Group ||
		string(owner.UID) != string(t.ref.UID) || t.scaledObject.GetUID() != owner.UID {
		return notAvailable(SectionMetrics, ProvenanceMetric, fixedHPANotItsOwn), false, nil
	}
	hpaSpec, _ := hpa.Object["spec"].(map[string]any)
	soSpec, _ := t.scaledObject.Object["spec"].(map[string]any)
	if !sameScaleTarget(hpaSpec["scaleTargetRef"], soSpec["scaleTargetRef"]) {
		return notAvailable(SectionMetrics, ProvenanceMetric, fixedHPANotItsOwn), false, nil
	}
	return Block{}, true, nil
}

// sameScaleTarget reports whether an HPA's scaleTargetRef names the same object as a ScaledObject's. KEDA's
// defaults apply to the ScaledObject's: kind Deployment and API version apps/v1 when they are not set. The
// version is not compared, only the group: KEDA writes the version the target is served in.
func sameScaleTarget(hpaRef, soRef any) bool {
	h, _ := hpaRef.(map[string]any)
	s, _ := soRef.(map[string]any)
	field := func(m map[string]any, key, def string) string {
		v, _ := m[key].(string)
		if v == "" {
			return def
		}
		return v
	}
	name := field(s, "name", "")
	return name != "" && field(h, "name", "") == name &&
		field(h, "kind", "") == field(s, "kind", "Deployment") &&
		groupOf(field(h, "apiVersion", "")) == groupOf(field(s, "apiVersion", "apps/v1"))
}

// groupOf returns the group of an API version: "" for the core group ("v1").
func groupOf(apiVersion string) string {
	group, _, found := strings.Cut(apiVersion, "/")
	if !found {
		return ""
	}
	return group
}

// seriesLines writes one line per series, sorted by labels.
func (b *Builder) seriesLines(l *lines, plan metricPlan, res providers.MetricsResult) {
	partial := ""
	if res.Partial {
		partial = partialSuffix
	}
	if res.LeftOut > 0 {
		l.add("%s: %d%s%s", plan.template, res.LeftOut, fixedLeftOut, partial)
	}
	if len(res.Series) == 0 {
		if res.LeftOut == 0 {
			l.add("%s%s%s", plan.template, fixedNoSeries, partial)
		}
		return
	}
	out := make([]frag, 0, len(res.Series))
	for _, s := range res.Series {
		keys := slices.Sorted(maps.Keys(s.Labels))
		pairs := make([]frag, 0, len(keys))
		for _, k := range keys {
			pairs = append(pairs, cat(b.value(k), "=", b.value(s.Labels[k])))
		}
		// A series line is the source's answer; the partial mark after it is code.
		out = append(out, cat(l.data(cat(plan.template+"{", joinFrags(pairs, ","), "} "+valuesOf(plan, s.Samples))), partial))
	}
	// Sorted as the release before #845 sorted them, by the lines as it wrote them (releasedKey), so that its
	// reading reads them in its order. One line needs no order, and a value repeated in the lines of an answer (a
	// pod or namespace label) is read for the order once: the decision of the section reads each value again, so
	// the order adds at most one reading of each distinct value of an answer to it (P3 of #845).
	if len(out) > 1 {
		written := make(map[string]string)
		keys := make([]string, len(out))
		idx := make([]int, len(out))
		for i, f := range out {
			idx[i], keys[i] = i, b.releasedKey(f, written)
		}
		slices.SortStableFunc(idx, func(x, y int) int { return strings.Compare(keys[x], keys[y]) })
		sorted := make([]frag, len(out))
		for i, j := range idx {
			sorted[i] = out[j]
		}
		out = sorted
	}
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
