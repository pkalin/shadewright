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

package eval

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/providers/metrics/prometheus"
)

// metricsFile is the optional file of a scenario that records what its Prometheus holds. A scenario without
// it has no metrics source, as an analyzer without --prometheus-url.
const metricsFile = "metrics.yaml"

// metricsRecording is what the Prometheus of a scenario holds: for every approved template, the series it
// answers in the scenario's namespace. It is replayed through the providers.MetricsSource interface, the one
// the context builder and the query_metrics tool use, by [replayMetrics].
type metricsRecording struct {
	// Source says how the values were made: "synthetic" (written by hand from the scenario) or the run that
	// recorded them.
	Source string `json:"source"`
	// Namespace is the namespace the series are in; a query about any other has no series.
	Namespace string `json:"namespace"`
	// Templates lists every approved template, each once, with its series (none is an empty answer).
	Templates []recordedTemplate `json:"templates"`
}

// recordedTemplate is the answer of one template.
type recordedTemplate struct {
	Name   string           `json:"name"`
	Series []recordedSeries `json:"series"`
}

// recordedSeries is one series as the adapter returns it: the template's labels only, and its samples in time
// order.
type recordedSeries struct {
	Labels  map[string]string `json:"labels"`
	Samples []recordedSample  `json:"samples"`
}

type recordedSample struct {
	At    time.Time `json:"at"`
	Value float64   `json:"value"`
}

// lookback is how long a sample answers a query after its time, as Prometheus's default lookback delta.
const lookback = 5 * time.Minute

// workloadLabel is the label a template that takes the workload parameter matches it against. A new such
// template must be added here, or loading a recording fails.
var workloadLabel = map[string]string{"deployment_replicas_unavailable": "deployment"}

// plainName is what the adapter accepts as a label value of a result (internal/providers/metrics/prometheus,
// decode.go): a recording may hold only what the real source could return.
var plainName = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,253}$`)

// check validates a recording against the approved templates, so a replay answers only what the real
// adapter could: every template listed once, labels within the template's, plain-name values, a pod label
// for a pod template, samples in time order.
func (r *metricsRecording) check() error {
	if r.Source == "" {
		return errors.New("metrics.yaml: source is required")
	}
	if len(validation.IsDNS1123Label(r.Namespace)) > 0 {
		return errors.New("metrics.yaml: namespace is not a DNS-1123 label")
	}
	infos := make(map[string]prometheus.TemplateInfo)
	for _, ti := range prometheus.Templates() {
		infos[ti.Name] = ti
		if slices.Contains(ti.Params, prometheus.ParamWorkload) && workloadLabel[ti.Name] == "" {
			return fmt.Errorf("template %s takes a workload: add the label it matches to workloadLabel", ti.Name)
		}
	}
	seen := map[string]bool{}
	for _, t := range r.Templates {
		ti, ok := infos[t.Name]
		if !ok {
			return fmt.Errorf("metrics.yaml: %q is not an approved template", t.Name)
		}
		if seen[t.Name] {
			return fmt.Errorf("metrics.yaml: template %s is listed twice", t.Name)
		}
		seen[t.Name] = true
		if len(t.Series) > prometheus.MaxSeries {
			return fmt.Errorf("metrics.yaml: template %s has more than %d series", t.Name, prometheus.MaxSeries)
		}
		for i, s := range t.Series {
			if err := checkSeries(ti, s); err != nil {
				return fmt.Errorf("metrics.yaml: template %s series %d: %w", t.Name, i, err)
			}
		}
	}
	missing := slices.DeleteFunc(slices.Sorted(maps.Keys(infos)), func(n string) bool { return seen[n] })
	if len(missing) > 0 {
		return fmt.Errorf("metrics.yaml: templates %q are not listed (list one with no series if the source has none)",
			missing)
	}
	return nil
}

func checkSeries(ti prometheus.TemplateInfo, s recordedSeries) error {
	for k, v := range s.Labels {
		if !slices.Contains(ti.Labels, k) {
			return fmt.Errorf("label %q is not one of the template's %q", k, ti.Labels)
		}
		if !plainName.MatchString(v) {
			return fmt.Errorf("label %s has a value that is not a plain name", k)
		}
	}
	if ti.Pods && s.Labels["pod"] == "" {
		return errors.New("a series of a pod template needs a pod label")
	}
	if w := workloadLabel[ti.Name]; w != "" && s.Labels[w] == "" {
		return fmt.Errorf("a series needs the %s label", w)
	}
	if len(s.Samples) == 0 {
		return errors.New("no samples")
	}
	for i := 1; i < len(s.Samples); i++ {
		if !s.Samples[i].At.After(s.Samples[i-1].At) {
			return errors.New("samples are not in time order")
		}
	}
	return nil
}

// replayMetrics answers metric queries from a recording, as a Prometheus holding those series would through
// the adapter: only approved templates with the template's parameters, only the pods (or the workload) asked
// for, an instant query at its end time and a range query at each step, a sample answering for [lookback]
// after its time.
type replayMetrics struct {
	rec   *metricsRecording
	infos map[string]prometheus.TemplateInfo
}

var _ providers.MetricsSource = replayMetrics{}

func newReplayMetrics(rec *metricsRecording) replayMetrics {
	infos := make(map[string]prometheus.TemplateInfo)
	for _, ti := range prometheus.Templates() {
		infos[ti.Name] = ti
	}
	return replayMetrics{rec: rec, infos: infos}
}

func (m replayMetrics) Query(ctx context.Context, q providers.MetricsQuery) (providers.MetricsResult, error) {
	if err := ctx.Err(); err != nil {
		return providers.MetricsResult{}, err
	}
	ti, ok := m.infos[q.Template]
	if !ok {
		return providers.MetricsResult{}, fmt.Errorf("template: %w", providers.ErrNotFound)
	}
	if err := checkQuery(ti, q); err != nil {
		return providers.MetricsResult{}, err
	}
	var res providers.MetricsResult
	if q.Params[prometheus.ParamNamespace] != m.rec.Namespace {
		return res, nil
	}
	idx := slices.IndexFunc(m.rec.Templates, func(t recordedTemplate) bool { return t.Name == q.Template })
	if idx < 0 {
		// Only a recording that was never checked (the manifest's tool definitions) lacks a template.
		return res, nil
	}
	for _, s := range m.rec.Templates[idx].Series {
		if ti.Pods && !slices.Contains(q.Pods, s.Labels["pod"]) {
			continue
		}
		if w := workloadLabel[ti.Name]; w != "" && s.Labels[w] != q.Params[prometheus.ParamWorkload] {
			continue
		}
		if samples := samplesFor(s.Samples, q); len(samples) > 0 {
			res.Series = append(res.Series, providers.Series{Labels: maps.Clone(s.Labels), Samples: samples})
		}
	}
	return res, nil
}

// checkQuery refuses what the adapter refuses before a request: parameters other than the template's,
// pods for a template that takes none (or none for one that does), an instant query with a start, and a range
// query with a step below prometheus.MinStep, a start not before its end or a span over prometheus.MaxRange.
func checkQuery(ti prometheus.TemplateInfo, q providers.MetricsQuery) error {
	for k := range q.Params {
		if !slices.Contains(ti.Params, k) {
			return fmt.Errorf("unexpected parameter: %w", prometheus.ErrInvalidQuery)
		}
	}
	for _, p := range ti.Params {
		if q.Params[p] == "" {
			return fmt.Errorf("parameter %s is required: %w", p, prometheus.ErrInvalidQuery)
		}
	}
	if ti.Pods != (len(q.Pods) > 0) || len(q.Pods) > prometheus.MaxPods {
		return fmt.Errorf("pods: %w", prometheus.ErrInvalidQuery)
	}
	instant := q.Step == 0 && !q.End.IsZero() && q.Start.IsZero()
	ranged := q.Step >= prometheus.MinStep && !q.Start.IsZero() && q.Start.Before(q.End) &&
		q.End.Sub(q.Start) <= prometheus.MaxRange
	if !instant && !ranged {
		return fmt.Errorf("time range: %w", prometheus.ErrInvalidQuery)
	}
	return nil
}

// samplesFor evaluates a series at the query's end (instant) or at each step from its start (range): the
// latest sample at or before that time, if it is at most [lookback] old.
func samplesFor(recorded []recordedSample, q providers.MetricsQuery) []providers.Sample {
	at := func(t time.Time) (providers.Sample, bool) {
		for _, s := range slices.Backward(recorded) {
			if s.At.After(t) {
				continue
			}
			if t.Sub(s.At) > lookback {
				return providers.Sample{}, false
			}
			return providers.Sample{Time: t, Value: s.Value}, true
		}
		return providers.Sample{}, false
	}
	if q.Step == 0 {
		if s, ok := at(q.End); ok {
			return []providers.Sample{s}
		}
		return nil
	}
	var out []providers.Sample
	for t := q.Start; !t.After(q.End); t = t.Add(q.Step) {
		if s, ok := at(t); ok {
			out = append(out, s)
		}
	}
	return out
}
