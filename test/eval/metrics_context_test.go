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
	"encoding/base64"
	"encoding/json"
	"errors"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/pkalin/shadewright/internal/analysis"
	"github.com/pkalin/shadewright/internal/incidentcontext"
	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/providers/metrics/prometheus"
	"github.com/pkalin/shadewright/internal/redact"
)

const (
	scnStatefulSetOOM = "statefulset-oom"
	podCache1         = "cache-1"
	tmplRestarts      = "container_restarts_total"
)

// buildContext builds the context of s as a run does, with metrics as its metrics source (nil: none) and
// mod applied to the builder's configuration.
func buildContext(t *testing.T, s *scenario, metrics providers.MetricsSource,
	mod func(*incidentcontext.Config)) *incidentcontext.Context {
	t.Helper()
	scheme, err := newScheme()
	if err != nil {
		t.Fatal(err)
	}
	c := newClient(scheme, s.objects)
	red, err := redact.New()
	if err != nil {
		t.Fatal(err)
	}
	cfg := incidentcontext.Config{Pods: c, Objects: c, Namespaces: watchedNamespaces, Metrics: metrics,
		Redactor: red, Now: func() time.Time { return fixtureNow }}
	if mod != nil {
		mod(&cfg)
	}
	b, err := incidentcontext.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := b.Build(t.Context(), s.incident)
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}

func mustLoad(t *testing.T, name string) *scenario {
	t.Helper()
	scheme, err := newScheme()
	if err != nil {
		t.Fatal(err)
	}
	s, err := loadScenario(scheme, name)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// metricsBlock returns the metrics block and its ID as the prompt numbers it (c1, c2...).
func metricsBlock(t *testing.T, c *incidentcontext.Context) (incidentcontext.Block, string) {
	t.Helper()
	for i, b := range c.Blocks {
		if b.Section == incidentcontext.SectionMetrics {
			return b, "c" + string(rune('1'+i))
		}
	}
	t.Fatal("no metrics block")
	return incidentcontext.Block{}, ""
}

func quotableIn(b incidentcontext.Block, q string) bool {
	return slices.ContainsFunc(b.Data(), func(part string) bool { return strings.Contains(part, q) })
}

// The OOM scenarios' recorded metrics reach the context through the MetricsSource interface, and every
// metric quote of their recorded reply names the metrics block and is quotable there, so the loop's evidence
// check (verbatim, within one data value) accepts it.
func TestRecordedMetricsReachTheContext(t *testing.T) {
	for name, pod := range map[string]string{scnOOM: "web-8d4c6b9f7-t5r9k", scnStatefulSetOOM: podCache1} {
		s := mustLoad(t, name)
		if s.metrics == nil {
			t.Fatalf("%s: no metrics.yaml", name)
		}
		c := buildContext(t, s, s.metricsSource(), nil)
		blk, id := metricsBlock(t, c)
		if blk.Availability != incidentcontext.Available || c.Escalate() {
			t.Fatalf("%s: metrics %s, escalate %v:\n%s", name, blk.Availability, c.Escalate(), blk.Text)
		}
		for _, want := range []string{"container_memory_working_set_bytes{", "pod=" + pod + "}",
			"container_memory_limit_bytes{"} {
			if !strings.Contains(blk.Text, want) {
				t.Errorf("%s: metrics block has no %q:\n%s", name, want, blk.Text)
			}
		}
		res, _, err := analysis.DecodeResult(s.recording.Replies[len(s.recording.Replies)-1].Content)
		if err != nil {
			t.Fatal(err)
		}
		quoted := 0
		for _, e := range res.Evidence {
			if !strings.HasPrefix(e.Quote, "container_") {
				continue
			}
			quoted++
			if e.Source != id || !quotableIn(blk, e.Quote) {
				t.Errorf("%s: quote %q of %s is not quotable in the metrics block %s", name, e.Quote, e.Source, id)
			}
		}
		if quoted == 0 {
			t.Errorf("%s: the recorded reply quotes no metric", name)
		}
	}
}

// A scenario without metrics.yaml has no metrics source, as an analyzer without --prometheus-url: the
// section says so and is no escalation signal.
func TestNoMetricsRecordingIsNotConfigured(t *testing.T) {
	for _, name := range []string{"keda-at-ceiling", "bad-image"} {
		s := mustLoad(t, name)
		if s.metrics != nil || s.metricsSource() != nil {
			t.Fatalf("%s: has a metrics source", name)
		}
		c := buildContext(t, s, s.metricsSource(), nil)
		blk, _ := metricsBlock(t, c)
		const notConfigured = "not available: no metrics source is configured\n"
		if blk.Availability != incidentcontext.NotAvailable || blk.Text != notConfigured ||
			slices.Contains(c.FailedSources, incidentcontext.SectionMetrics) {
			t.Errorf("%s: metrics %s, failed %v:\n%s", name, blk.Availability, c.FailedSources, blk.Text)
		}
	}
}

// metricsFunc adapts a function to providers.MetricsSource.
type metricsFunc func(context.Context, providers.MetricsQuery) (providers.MetricsResult, error)

func (f metricsFunc) Query(ctx context.Context, q providers.MetricsQuery) (providers.MetricsResult, error) {
	return f(ctx, q)
}

// A metrics source that hangs or fails neither blocks nor fails the build: each call is cut at the source
// timeout, the section says it could not fetch, the other sections are whole, and the context carries the
// failed-source escalation signal (design 3.3).
func TestSlowOrFailingMetricsSourceIsAFailedSource(t *testing.T) {
	s := mustLoad(t, scnOOM)
	for name, src := range map[string]providers.MetricsSource{
		"hangs": metricsFunc(func(ctx context.Context, _ providers.MetricsQuery) (providers.MetricsResult, error) {
			<-ctx.Done()
			return providers.MetricsResult{}, ctx.Err()
		}),
		"fails": metricsFunc(func(context.Context, providers.MetricsQuery) (providers.MetricsResult, error) {
			return providers.MetricsResult{}, &providers.RetryableError{Err: errors.New("connection refused")}
		}),
	} {
		start := time.Now()
		c := buildContext(t, s, src, func(cfg *incidentcontext.Config) { cfg.SourceTimeout = 20 * time.Millisecond })
		if took := time.Since(start); took > 5*time.Second {
			t.Errorf("%s: the build took %s", name, took)
		}
		blk, _ := metricsBlock(t, c)
		if blk.Availability != incidentcontext.CouldNotFetch || !c.Escalate() ||
			!slices.Contains(c.FailedSources, incidentcontext.SectionMetrics) || len(blk.Data()) > 0 {
			t.Errorf("%s: metrics %s, failed %v:\n%s", name, blk.Availability, c.FailedSources, blk.Text)
		}
		if strings.Contains(blk.Text, "connection refused") || strings.Contains(blk.Text, "deadline") {
			t.Errorf("%s: the error text reached the block:\n%s", name, blk.Text)
		}
		if c.Blocks[0].Availability != incidentcontext.Available || c.Blocks[1].Availability != incidentcontext.Available {
			t.Errorf("%s: the summary or status is missing", name)
		}
	}
}

// withRecording returns a copy of s whose recording is changed by mod.
func withRecording(t *testing.T, s *scenario, mod func(r *metricsRecording)) *scenario {
	t.Helper()
	raw, err := yaml.Marshal(s.metrics)
	if err != nil {
		t.Fatal(err)
	}
	rec := &metricsRecording{}
	if err := yaml.Unmarshal(raw, rec); err != nil {
		t.Fatal(err)
	}
	mod(rec)
	cp := *s
	cp.metrics = rec
	return &cp
}

// A label value from the source that looks like a token is redacted before the model sees it, whatever the
// source did: it is a plain name, so the adapter would pass it on.
func TestMetricLabelTokenIsRedacted(t *testing.T) {
	b64 := base64.RawURLEncoding.EncodeToString
	token := b64([]byte(`{"alg":"HS256"}`)) + "." + b64([]byte(`{"sub":"synthetic"}`)) + ".c2lnbmF0dXJlLXN5bnRoZXRpYw"
	if !plainName.MatchString(token) {
		t.Fatalf("the token %q is not a plain name, the adapter would refuse it", token)
	}
	s := withRecording(t, mustLoad(t, scnOOM), func(r *metricsRecording) {
		for i := range r.Templates {
			if r.Templates[i].Name == tmplRestarts {
				r.Templates[i].Series[0].Labels["container"] = token
			}
		}
	})
	if err := s.metrics.check(); err != nil {
		t.Fatal(err)
	}
	c := buildContext(t, s, s.metricsSource(), nil)
	blk, _ := metricsBlock(t, c)
	if !strings.Contains(blk.Text, "container_restarts_total{") {
		t.Fatalf("the series is missing:\n%s", blk.Text)
	}
	for _, b := range c.Blocks {
		if strings.Contains(b.Text, token) || strings.Contains(b.Text, token[:20]) {
			t.Errorf("section %s holds the token:\n%s", b.Section, b.Text)
		}
	}
}

// The metrics section keeps to its own bound, keeping its most relevant lines, and a cut to the section
// bound is a label only, never an escalation signal.
func TestMetricsSectionKeepsItsBound(t *testing.T) {
	s := mustLoad(t, scnStatefulSetOOM)
	limits := incidentcontext.DefaultLimits()
	limits.Metrics = incidentcontext.MinSectionBytes
	s = withRecording(t, s, func(r *metricsRecording) {
		// More containers than fit: every pod template answers a series per container.
		for i := range r.Templates {
			ts := &r.Templates[i]
			for j, n := range slices.Clone(ts.Series) {
				for k := range 8 {
					l := maps.Clone(n.Labels)
					l["container"] = "sidecar-" + string(rune('a'+k))
					ts.Series = append(ts.Series, recordedSeries{Labels: l, Samples: ts.Series[j].Samples})
				}
			}
		}
	})
	c := buildContext(t, s, s.metricsSource(), func(cfg *incidentcontext.Config) { cfg.Limits = limits })
	blk, _ := metricsBlock(t, c)
	if len(blk.Text) > limits.Metrics || !slices.Contains(blk.Labels, incidentcontext.LabelSectionCut) {
		t.Errorf("metrics block of %d bytes, labels %v", len(blk.Text), blk.Labels)
	}
	if c.Escalate() {
		t.Errorf("a section cut escalated: truncated %v, failed %v", c.Truncated, c.FailedSources)
	}
	if !strings.HasPrefix(blk.Text, "container_last_terminated_oomkilled{") {
		t.Errorf("the cut did not keep the head:\n%s", blk.Text)
	}
}

// The replay answers only what the adapter would: approved templates with their parameters, the pods or the
// workload asked for, the namespace recorded, and a sample only within the lookback.
func TestReplayAnswersLikeTheAdapter(t *testing.T) {
	m := newReplayMetrics(mustLoad(t, scnStatefulSetOOM).metrics)
	end := fixtureNow
	ns := map[string]string{prometheus.ParamNamespace: nsDemo}
	for name, tc := range map[string]struct {
		q    providers.MetricsQuery
		err  error
		pods []string
	}{
		"unknown template": {q: providers.MetricsQuery{Template: "up", Params: ns, End: end}, err: providers.ErrNotFound},
		"extra parameter": {q: providers.MetricsQuery{Template: tmplRestarts, Pods: []string{podCache1},
			Params: map[string]string{prometheus.ParamNamespace: nsDemo, "query": "up"}, End: end},
			err: prometheus.ErrInvalidQuery},
		"no namespace": {q: providers.MetricsQuery{Template: tmplRestarts, Pods: []string{podCache1},
			End: end}, err: prometheus.ErrInvalidQuery},
		"pod template without pods": {q: providers.MetricsQuery{Template: tmplRestarts, Params: ns,
			End: end}, err: prometheus.ErrInvalidQuery},
		"workload template with pods": {q: providers.MetricsQuery{Template: "deployment_replicas_unavailable",
			Params: map[string]string{prometheus.ParamNamespace: nsDemo, prometheus.ParamWorkload: "cache"},
			Pods:   []string{podCache1}, End: end}, err: prometheus.ErrInvalidQuery},
		"only the pods asked": {q: providers.MetricsQuery{Template: tmplRestarts, Params: ns,
			Pods: []string{podCache1}, End: end}, pods: []string{podCache1}},
		"another namespace": {q: providers.MetricsQuery{Template: tmplRestarts, Pods: []string{podCache1},
			Params: map[string]string{prometheus.ParamNamespace: "other"}, End: end}},
		"stale sample": {q: providers.MetricsQuery{Template: "container_memory_working_set_bytes", Params: ns,
			Pods: []string{podCache1}, End: end}},
	} {
		res, err := m.Query(t.Context(), tc.q)
		if tc.err != nil {
			if !errors.Is(err, tc.err) {
				t.Errorf("%s: error %v, want %v", name, err, tc.err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		var pods []string
		for _, s := range res.Series {
			pods = append(pods, s.Labels["pod"])
		}
		if !slices.Equal(pods, tc.pods) {
			t.Errorf("%s: series of %q, want %q", name, pods, tc.pods)
		}
	}
	// A range query has a point per step within the lookback of a sample, each at its step's time.
	res, err := m.Query(t.Context(), providers.MetricsQuery{Template: "container_memory_working_set_bytes", Params: ns,
		Pods: []string{podCache1}, Start: end.Add(-20 * time.Minute), End: end, Step: time.Minute})
	if err != nil || len(res.Series) != 1 {
		t.Fatalf("range: %v, %d series", err, len(res.Series))
	}
	got := res.Series[0].Samples
	// The last sample is at 11:54:55, so the steps 11:40 to 11:59 answer from 11:44:30 on, up to 11:59.
	if len(got) != 15 || !got[0].Time.Equal(end.Add(-15*time.Minute)) || got[len(got)-1].Value != 50118656 {
		t.Errorf("range: %d samples, first %v, last %v", len(got), got[0].Time, got[len(got)-1].Value)
	}
	if _, err := m.Query(t.Context(), providers.MetricsQuery{Template: tmplRestarts, Params: ns,
		Pods: []string{podCache1}, Start: end.Add(-time.Minute), End: end}); !errors.Is(err, prometheus.ErrInvalidQuery) {
		t.Errorf("a start without a step: %v", err)
	}
	if _, err := m.Query(t.Context(), providers.MetricsQuery{Template: tmplRestarts, Params: ns, Pods: []string{podCache1},
		Start: end.Add(-time.Hour), End: end, Step: time.Second}); !errors.Is(err, prometheus.ErrInvalidQuery) {
		t.Errorf("a step below the adapter's minimum: %v", err)
	}
}

// A recording is checked against the approved templates when it is loaded, so it can hold only what the
// adapter could answer.
func TestMetricsRecordingIsChecked(t *testing.T) {
	base := mustLoad(t, scnOOM)
	for name, mod := range map[string]func(r *metricsRecording){
		"template missing": func(r *metricsRecording) { r.Templates = r.Templates[1:] },
		"unknown template": func(r *metricsRecording) {
			r.Templates = append(r.Templates, recordedTemplate{Name: "up"})
		},
		"template twice": func(r *metricsRecording) { r.Templates = append(r.Templates, r.Templates[0]) },
		"label not of the template": func(r *metricsRecording) {
			r.Templates[0].Series[0].Labels["job"] = "kubelet"
		},
		"value not a plain name": func(r *metricsRecording) {
			r.Templates[0].Series[0].Labels["container"] = "web app"
		},
		"no pod label": func(r *metricsRecording) { delete(r.Templates[0].Series[0].Labels, "pod") },
		"samples out of order": func(r *metricsRecording) {
			s := r.Templates[0].Series[0].Samples
			s[0], s[1] = s[1], s[0]
		},
		"no source": func(r *metricsRecording) { r.Source = "" },
	} {
		s := withRecording(t, base, mod)
		if err := s.metrics.check(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// The namespace must be the Incident's.
	other := mustLoad(t, scnOOM)
	other.incident.Spec.Subject.Namespace = "other"
	if err := other.loadMetrics(filepath.Join(scenariosDir, scnOOM)); err == nil {
		t.Error("a recording of another namespace was accepted")
	}
}

// A run replays the recording into the context of the analysis loop: a proposal whose only evidence is a
// metric quote passes the loop's evidence check, and the same quote is refused (NoEvidence) when the scenario
// has no metrics source, so the check accepts only what the context shows.
func TestRunQuotesRecordedMetrics(t *testing.T) {
	for _, withMetrics := range []bool{true, false} {
		scheme, s := mustScenario(t, scnOOM)
		reply := s.recording.Replies[0]
		res, _, err := analysis.DecodeResult(reply.Content)
		if err != nil {
			t.Fatal(err)
		}
		res.Evidence = slices.DeleteFunc(res.Evidence, func(e analysis.Evidence) bool {
			return !strings.HasPrefix(e.Quote, "container_memory_working_set_bytes{")
		})
		if len(res.Evidence) != 1 {
			t.Fatalf("the recorded reply has %d working set quotes", len(res.Evidence))
		}
		raw, err := json.Marshal(res)
		if err != nil {
			t.Fatal(err)
		}
		reply.Content = string(raw)
		s.recording.Replies = []recordedReply{reply}
		if !withMetrics {
			s.metrics = nil
		}
		out, err := runScenario(t.Context(), scheme, s, replaySource{})
		if err != nil {
			t.Fatal(err)
		}
		want := analysis.ReasonProposed
		if !withMetrics {
			want = analysis.ReasonNoEvidence
		}
		if out.Reason != string(want) {
			t.Errorf("metrics %v: the loop ended %s, want %s", withMetrics, out.Reason, want)
		}
	}
}
