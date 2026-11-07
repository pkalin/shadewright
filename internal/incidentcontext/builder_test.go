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
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/providers/fake"
)

// The happy path: every section is there in order, tagged with its provenance, and nothing escalates.
func TestBuildSectionsInOrder(t *testing.T) {
	b := newBuilder(t, reader(t, webObjects()...), webSources(), nil)
	c := build(t, b, incident(deploymentRef()))
	got := make([]Section, 0, len(c.Blocks))
	for _, blk := range c.Blocks {
		got = append(got, blk.Section)
	}
	want := []Section{SectionSummary, SectionStatus, SectionLogs, SectionMetrics, SectionGit, SectionPolicy}
	if strings.Join(sectionNames(got), ",") != strings.Join(sectionNames(want), ",") {
		t.Fatalf("sections %v, want %v", got, want)
	}
	if c.Escalate() {
		t.Fatalf("context escalates: truncated %t, failed %v", c.Truncated, c.FailedSources)
	}
	status := one(t, c, SectionStatus)
	for _, s := range []string{"Deployment shop/web", unavailable, "owns ReplicaSet web-abc",
		"pod web-abc-1", crashLoop} {
		if !strings.Contains(status.Text, s) {
			t.Errorf("status lacks %q:\n%s", s, status.Text)
		}
	}
	// The crashing pod comes first: a cut keeps the head.
	if strings.Index(status.Text, "pod web-abc-1") > strings.Index(status.Text, "pod web-abc-2") {
		t.Errorf("the crashing pod is not first:\n%s", status.Text)
	}
	logs := one(t, c, SectionLogs)
	if !strings.Contains(logs.Text, "previous instance") || !strings.Contains(logs.Text, "panic: boom") {
		t.Errorf("log block does not hold the previous instance's tail:\n%s", logs.Text)
	}
	if one(t, c, SectionSummary).Provenance != ProvenanceIncident || logs.Provenance != ProvenanceLogExcerpt ||
		one(t, c, SectionMetrics).Provenance != ProvenanceMetric || status.Provenance != ProvenanceObjectStatus {
		t.Errorf("provenance tags: %+v", c.Blocks)
	}
	// The attributed pods, most troubled first, for the analysis loop's attribution label.
	if strings.Join(c.Pods, ",") != "web-abc-1,web-abc-2" {
		t.Errorf("pods %v, want web-abc-1,web-abc-2", c.Pods)
	}
}

func sectionNames(s []Section) []string {
	out := make([]string, len(s))
	for i, x := range s {
		out[i] = string(x)
	}
	return out
}

// Acceptance: a huge log stays within its budget, and a log excerpt cut to its own bound does not mark the
// context truncated.
func TestHugeLogStaysWithinBudget(t *testing.T) {
	src := webSources()
	key := fake.LogKey{Namespace: testNS, Pod: crashPod, Container: appName, Previous: true}
	src.logs.Set(key, strings.Repeat("a log line that repeats itself\n", 64<<10)+"panic: the last line\n")
	b := newBuilder(t, reader(t, webObjects()...), src, nil)
	c := build(t, b, incident(deploymentRef()))
	if n := sectionSize(c, SectionLogs); n > DefaultLogBytes {
		t.Fatalf("log section is %d bytes, over its bound of %d", n, DefaultLogBytes)
	}
	logs := one(t, c, SectionLogs)
	if !strings.HasSuffix(logs.Text, "panic: the last line\n") {
		t.Errorf("the newest line was not kept: ...%q", logs.Text[max(0, len(logs.Text)-80):])
	}
	if !strings.HasPrefix(logs.Text, "pod web-abc-1 container app, previous instance\n") {
		t.Errorf("the header was not kept: %q", logs.Text[:min(len(logs.Text), 80)])
	}
	if !hasLabel(logs, LabelSectionCut) || !hasLabel(logs, LabelLogTruncated) {
		t.Errorf("labels %v, want the section cut and the source's truncation", logs.Labels)
	}
	if c.Truncated || c.Escalate() {
		t.Errorf("a log cut to its own bound marked the context: truncated %t, failed %v", c.Truncated, c.FailedSources)
	}
	if c.Size() > DefaultTotalBytes {
		t.Errorf("context is %d bytes", c.Size())
	}
}

// A source that sends far more than asked for is not read further and counts as failed.
func TestOversizedLogAnswerIsAFailure(t *testing.T) {
	b := newBuilder(t, reader(t, webObjects()...), sources{logs: nil, metrics: nil}, func(c *Config) {
		c.Logs = logFunc(func(context.Context, providers.LogQuery) (providers.LogExcerpt, error) {
			return providers.LogExcerpt{Text: strings.Repeat("x\n", 1<<20)}, nil
		})
	})
	c := build(t, b, incident(deploymentRef()))
	logs := one(t, c, SectionLogs)
	if logs.Availability != CouldNotFetch || !c.Escalate() || len(logs.Text) > DefaultLogBytes {
		t.Fatalf("availability %q, escalate %t, %d bytes", logs.Availability, c.Escalate(), len(logs.Text))
	}
}

type logFunc func(context.Context, providers.LogQuery) (providers.LogExcerpt, error)

func (f logFunc) Logs(ctx context.Context, q providers.LogQuery) (providers.LogExcerpt, error) {
	return f(ctx, q)
}

type metricsFunc func(context.Context, providers.MetricsQuery) (providers.MetricsResult, error)

func (f metricsFunc) Query(ctx context.Context, q providers.MetricsQuery) (providers.MetricsResult, error) {
	return f(ctx, q)
}

// Acceptance: given the whole context exceeds the total bound, it is cut to it and marked truncated (the
// escalation signal). Sections larger than the default are configured so that they add up to more.
func TestTotalBoundCutsAndMarksTruncated(t *testing.T) {
	src := webSources()
	key := fake.LogKey{Namespace: testNS, Pod: crashPod, Container: appName, Previous: true}
	src.logs.Set(key, strings.Repeat("a log line that repeats itself\n", 64<<10)+"panic: the last line\n")
	limits := DefaultLimits()
	limits.Logs = 128 << 10
	b := newBuilder(t, reader(t, webObjects()...), src, func(c *Config) { c.Limits = limits })
	c := build(t, b, incident(deploymentRef()))
	if c.Size() > DefaultTotalBytes {
		t.Fatalf("context is %d bytes, over %d", c.Size(), DefaultTotalBytes)
	}
	if !c.Truncated || !c.Escalate() {
		t.Fatalf("context over its bound not marked truncated")
	}
	logs := one(t, c, SectionLogs)
	if !hasLabel(logs, LabelContextCut) {
		t.Errorf("log labels %v", logs.Labels)
	}
	if !strings.HasPrefix(logs.Text, "pod web-abc-1 container app, previous instance\n") ||
		!strings.HasSuffix(logs.Text, "panic: the last line\n") {
		t.Errorf("the total cut lost the header or the newest lines")
	}
	// The other sections were not cut: the logs gave enough.
	for _, blk := range c.Blocks {
		if blk.Section != SectionLogs && hasLabel(blk, LabelContextCut) {
			t.Errorf("section %q cut too", blk.Section)
		}
	}
}

// Acceptance: given no Git source, the Git section says "not available" and the context is not marked as
// having failed a source. Unconfigured metrics and logs are the same.
func TestUnconfiguredSourcesAreNotAvailableNotFailed(t *testing.T) {
	b := newBuilder(t, reader(t, webObjects()...), sources{}, nil)
	c := build(t, b, incident(deploymentRef()))
	for _, s := range []Section{SectionGit, SectionLogs, SectionMetrics} {
		blk := one(t, c, s)
		if blk.Availability != NotAvailable || !strings.HasPrefix(blk.Text, "not available") {
			t.Errorf("section %q: %q %q", s, blk.Availability, blk.Text)
		}
	}
	if len(c.FailedSources) != 0 || c.Escalate() {
		t.Fatalf("failed sources %v, escalate %t", c.FailedSources, c.Escalate())
	}
	// Git is "not available" also when every other source is configured.
	c = build(t, newBuilder(t, reader(t, webObjects()...), webSources(), nil), incident(deploymentRef()))
	if g := one(t, c, SectionGit); g.Availability != NotAvailable || c.Escalate() {
		t.Fatalf("git %q, escalate %t", g.Availability, c.Escalate())
	}
}

// A configured source that fails is "could not fetch" and an escalation signal; its error text, which may
// carry untrusted data, never enters the context.
func TestFailedSourcesEscalate(t *testing.T) {
	src := webSources()
	boom := errors.New("connection reset by peer while reading " + leakDSN)
	src.logs.SetFail(func(string) error { return boom })
	src.metrics.SetFail(func(string) error { return boom })
	c := build(t, newBuilder(t, reader(t, webObjects()...), src, nil), incident(deploymentRef()))
	if one(t, c, SectionLogs).Availability != CouldNotFetch || one(t, c, SectionMetrics).Availability != CouldNotFetch {
		t.Fatalf("blocks %+v", c.Blocks)
	}
	if !c.Escalate() || strings.Join(sectionNames(c.FailedSources), ",") != "log excerpt,metrics" {
		t.Fatalf("failed sources %v", c.FailedSources)
	}
	for _, blk := range c.Blocks {
		if strings.Contains(blk.Text, "connection reset") {
			t.Errorf("error text in block %q", blk.Section)
		}
	}
}

// Some metric templates failing leaves the others, labelled incomplete, and is still a failed source.
func TestPartlyFailedMetrics(t *testing.T) {
	b := newBuilder(t, reader(t, webObjects()...), webSources(), func(c *Config) {
		inner := c.Metrics
		c.Metrics = metricsFunc(func(ctx context.Context, q providers.MetricsQuery) (providers.MetricsResult, error) {
			if q.Template == "container_restarts_total" {
				return providers.MetricsResult{}, errors.New("unavailable")
			}
			return inner.Query(ctx, q)
		})
	})
	c := build(t, b, incident(deploymentRef()))
	m := one(t, c, SectionMetrics)
	if m.Availability != Available || !hasLabel(m, LabelIncomplete) || !c.Escalate() {
		t.Fatalf("metrics %q %v, escalate %t", m.Availability, m.Labels, c.Escalate())
	}
	if !strings.Contains(m.Text, "container_restarts_total: could not fetch") ||
		!strings.Contains(m.Text, "container_memory_working_set_bytes{container=app,pod=web-abc-1} last 1") {
		t.Errorf("metrics text:\n%s", m.Text)
	}
}

// Answers the source gives about the pod (gone, or refused) are answers, not failures.
func TestLogNotFoundOrRejectedIsNotAFailure(t *testing.T) {
	for _, err := range []error{providers.ErrNotFound, providers.ErrRejected} {
		src := webSources()
		src.logs.SetFail(func(string) error { return err })
		c := build(t, newBuilder(t, reader(t, webObjects()...), src, nil), incident(deploymentRef()))
		if l := one(t, c, SectionLogs); l.Availability != Available || !strings.Contains(l.Text, "no logs:") || c.Escalate() {
			t.Errorf("%v: %q %q, escalate %t", err, l.Availability, l.Text, c.Escalate())
		}
	}
}

// A metrics answer refused as too large caps the evidence; it is not a failure (design section 4).
func TestMetricsTooLargeIsCappedNotFailed(t *testing.T) {
	src := webSources()
	src.metrics.SetFail(func(string) error { return providers.ErrTooLarge })
	c := build(t, newBuilder(t, reader(t, webObjects()...), src, nil), incident(deploymentRef()))
	if m := one(t, c, SectionMetrics); !hasLabel(m, LabelPodsCapped) || c.Escalate() {
		t.Fatalf("labels %v, escalate %t", m.Labels, c.Escalate())
	}
}

// The evidence markers of the sources and of the pod set are labels, never escalation signals.
func TestEvidenceMarkersAreLabelsOnly(t *testing.T) {
	d := deployment()
	old := replicaSet("web-old", "uid-rs-old", d, longAgo)
	cur := replicaSet("web-new", "uid-rs-new", d, recent)
	objs := []client.Object{
		d, old, cur,
		crashing(pod("web-new-1", cur.Labels, recent, controllerRef(kindReplicaSet, cur.Name, cur.UID)), "exited"),
		pod("web-old-1", old.Labels, longAgo, controllerRef(kindReplicaSet, old.Name, old.UID)),
	}
	src := sources{logs: &fake.Logs{}, metrics: &fake.Metrics{}}
	key := fake.LogKey{Namespace: testNS, Pod: "web-new-1", Container: appName, Previous: true}
	src.logs.Set(key, strings.Repeat("line\n", 100))
	src.logs.SetInstance(key, providers.LogInstance{PodUID: "uid-web-new-1"}, true)
	for _, p := range metricPlans {
		src.metrics.SetResult(p.template, providers.MetricsResult{Partial: true})
	}
	c := build(t, newBuilder(t, reader(t, objs...), src, func(c *Config) {
		inner := c.Logs
		c.Logs = logFunc(func(ctx context.Context, q providers.LogQuery) (providers.LogExcerpt, error) {
			ex, err := inner.Logs(ctx, q)
			ex.Truncated, ex.EndMissing = true, true
			return ex, err
		})
	}), incident(deploymentRef()))
	logs := one(t, c, SectionLogs)
	for _, l := range []Label{LabelLogTruncated, LabelLogEndMissing, LabelLogInstanceUncertain, LabelPodsUncertain} {
		if !hasLabel(logs, l) {
			t.Errorf("log block lacks %q: %v", l, logs.Labels)
		}
	}
	if m := one(t, c, SectionMetrics); !hasLabel(m, LabelMetricsPartial) || !hasLabel(m, LabelPodsUncertain) {
		t.Errorf("metrics labels %v", m.Labels)
	}
	if s := one(t, c, SectionStatus); !hasLabel(s, LabelPodsUncertain) {
		t.Errorf("status labels %v", s.Labels)
	}
	if c.Escalate() {
		t.Fatalf("evidence markers escalated: truncated %t, failed %v", c.Truncated, c.FailedSources)
	}
}

// A build whose context is cancelled, before or during a source call, returns an error instead of a context
// with a failed source.
func TestCancelledBuildReturnsAnError(t *testing.T) {
	b := newBuilder(t, reader(t, webObjects()...), webSources(), nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if c, err := b.Build(ctx, incident(deploymentRef())); err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled build: %v, %+v", err, c)
	}
	for _, during := range []string{"logs", "metrics"} {
		ctx, cancel := context.WithCancel(context.Background())
		b := newBuilder(t, reader(t, webObjects()...), webSources(), func(c *Config) {
			stop := func(ctx context.Context) error { cancel(); <-ctx.Done(); return ctx.Err() }
			if during == "logs" {
				c.Logs = logFunc(func(ctx context.Context, _ providers.LogQuery) (providers.LogExcerpt, error) {
					return providers.LogExcerpt{}, stop(ctx)
				})
			} else {
				c.Metrics = metricsFunc(func(ctx context.Context, _ providers.MetricsQuery) (providers.MetricsResult, error) {
					return providers.MetricsResult{}, stop(ctx)
				})
			}
		})
		if c, err := b.Build(ctx, incident(deploymentRef())); !errors.Is(err, context.Canceled) {
			t.Errorf("cancelled during %s: %v, %+v", during, err, c)
		}
		cancel()
	}
}

// A build whose context is cancelled after the last source answered still returns an error.
func TestCancelledAfterLastSourceReturnsAnError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := newBuilder(t, reader(t, webObjects()...), webSources(), func(c *Config) {
		c.Metrics = metricsFunc(func(context.Context, providers.MetricsQuery) (providers.MetricsResult, error) {
			cancel()
			return providers.MetricsResult{}, nil
		})
	})
	if c, err := b.Build(ctx, incident(deploymentRef())); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled after the last source: %v, %+v", err, c)
	}
}

// A value from the cluster never adds a line, or anything that reads as a line break, to its section.
func TestValueStaysOnOneLine(t *testing.T) {
	b := newBuilder(t, reader(t), sources{}, func(c *Config) { c.Redactor = canary{} })
	in := "a\nb\rc\vd\fe\u0085f\u2028g\u2029h\ti"
	if got := b.value(in); got != "a b c d e f g h i" {
		t.Fatalf("value(%q) = %q", in, got)
	}
}

// A source call that runs out of its own timeout while the build goes on is a failed source.
func TestSourceTimeoutIsAFailure(t *testing.T) {
	b := newBuilder(t, reader(t, webObjects()...), webSources(), func(c *Config) {
		c.SourceTimeout = 20 * time.Millisecond
		c.Logs = logFunc(func(ctx context.Context, _ providers.LogQuery) (providers.LogExcerpt, error) {
			<-ctx.Done()
			return providers.LogExcerpt{}, ctx.Err()
		})
	})
	c := build(t, b, incident(deploymentRef()))
	if one(t, c, SectionLogs).Availability != CouldNotFetch || !c.Escalate() {
		t.Fatalf("timed-out source: %+v", c)
	}
}

// Reads of the target that fail make the status "could not fetch", a failed source; the log and metrics
// sections then have no pods and are not counted again.
func TestFailedClusterReadEscalates(t *testing.T) {
	r := clientfake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(webObjects()...).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
				return errors.New("cache not synced")
			},
		}).Build()
	c := build(t, newBuilder(t, r, webSources(), nil), incident(deploymentRef()))
	if s := one(t, c, SectionStatus); s.Availability != CouldNotFetch {
		t.Fatalf("status %q", s.Availability)
	}
	if strings.Join(sectionNames(c.FailedSources), ",") != string(SectionStatus) {
		t.Fatalf("failed sources %v", c.FailedSources)
	}
	if one(t, c, SectionLogs).Availability != NotAvailable || one(t, c, SectionMetrics).Availability != NotAvailable {
		t.Errorf("logs and metrics without pods: %+v", c.Blocks)
	}
}

// A target that no longer exists, or exists under another UID, is a fact, not a failure.
func TestGoneTargetIsNotAFailure(t *testing.T) {
	ref := deploymentRef()
	ref.UID = "uid-of-an-older-web"
	c := build(t, newBuilder(t, reader(t, webObjects()...), webSources(), nil), incident(ref))
	if s := one(t, c, SectionStatus); !strings.Contains(s.Text, "no longer exists") || c.Escalate() {
		t.Fatalf("status %q, escalate %t", s.Text, c.Escalate())
	}
}

// A kind the analyzer does not read is "not available", not a failure.
func TestUnsupportedKindIsNotAvailable(t *testing.T) {
	ref := remediationv1alpha1.ObjectRef{APIVersion: apiApps, Kind: "DaemonSet", Namespace: testNS, Name: "agent", UID: "u"}
	c := build(t, newBuilder(t, reader(t, webObjects()...), webSources(), nil), incident(ref))
	if s := one(t, c, SectionStatus); s.Availability != NotAvailable || c.Escalate() {
		t.Fatalf("status %q, escalate %t", s.Availability, c.Escalate())
	}
}

// A subject outside the watched namespaces is refused before any read of the cluster or call of a source
// (#371), and an empty set refuses every subject.
func TestUnwatchedNamespaceIsRefusedBeforeAnyCall(t *testing.T) {
	for name, namespaces := range map[string][]string{"other namespace": {otherNS}, "empty set": nil} {
		reads := 0
		count := interceptor.Funcs{
			Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
				reads++
				return errors.New("read")
			},
			List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
				reads++
				return errors.New("read")
			},
		}
		r := clientfake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(webObjects()...).WithInterceptorFuncs(count).Build()
		src := webSources()
		calls := 0
		onCall := func(string) error { calls++; return nil }
		src.logs.SetFail(onCall)
		src.metrics.SetFail(onCall)
		b := newBuilder(t, r, src, func(c *Config) { c.Namespaces = namespaces })
		c := build(t, b, incident(deploymentRef()))
		if reads != 0 || calls != 0 {
			t.Errorf("%s: %d cluster reads and %d source calls, want none", name, reads, calls)
		}
		if s := one(t, c, SectionStatus); s.Availability != NotAvailable || !strings.Contains(s.Text, "does not watch") {
			t.Errorf("%s: status %q %q", name, s.Availability, s.Text)
		}
		if one(t, c, SectionLogs).Availability != NotAvailable || one(t, c, SectionMetrics).Availability != NotAvailable {
			t.Errorf("%s: logs and metrics: %+v", name, c.Blocks)
		}
	}
}

// canary is a redactor that removes one marker only.
type canary struct{}

const canaryMark = "CANARY-7f3a"

func (canary) String(s string) string {
	return strings.ReplaceAll(s, canaryMark, "[x]")
}

// Acceptance: redaction is always applied, to every section, the summary included, whatever the source.
func TestRedactionIsAlwaysApplied(t *testing.T) {
	d := deployment()
	d.Status.Conditions[0].Message = "check " + canaryMark + " " + leakJWT
	rs := replicaSet("web-abc", "uid-rs-abc", d, longAgo)
	p := crashing(pod(crashPod, rs.Labels, longAgo, controllerRef(kindReplicaSet, rs.Name, rs.UID)),
		"dial "+leakDSN+" failed "+canaryMark)
	p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse, Message: leakJWT}}
	inc := incident(deploymentRef())
	inc.Spec.Symptom = "symptom with " + canaryMark + " and " + leakJWT
	src := sources{logs: &fake.Logs{}, metrics: &fake.Metrics{}}
	src.logs.Set(fake.LogKey{Namespace: testNS, Pod: crashPod, Container: appName, Previous: true},
		"connecting to "+leakDSN+"\nAuthorization: Bearer "+leakJWT+"\n"+canaryMark+"\n")
	src.metrics.Set("container_restarts_total", []providers.Series{{
		Labels: map[string]string{"pod": crashPod, "container": canaryMark}, Samples: []providers.Sample{{Value: 4}},
	}})

	// The built-in rules remove the credentials; a redactor that removes only a canary shows that every
	// section, whatever its source, goes through the configured redactor.
	for name, secrets := range map[string][]string{
		"built-in rules": {leakJWT, "Zq8-synthetic-Pw0rd"},
		"canary":         {canaryMark},
	} {
		b := newBuilder(t, reader(t, d, rs, p), src, func(c *Config) {
			if name == "canary" {
				c.Redactor = canary{}
			}
		})
		c := build(t, b, inc)
		for _, blk := range c.Blocks {
			for _, secret := range secrets {
				if strings.Contains(blk.Text, secret) {
					t.Errorf("%s: section %q holds %q:\n%s", name, blk.Section, secret, blk.Text)
				}
			}
		}
	}
}

func TestNewRefusesBadConfig(t *testing.T) {
	r := reader(t)
	red := canary{}
	for name, cfg := range map[string]Config{
		"no pods reader":   {Objects: r, Redactor: red},
		"no object reader": {Pods: r, Redactor: red},
		"no redactor":      {Pods: r, Objects: r},
		"total too small":  {Pods: r, Objects: r, Redactor: red, Limits: Limits{Total: 1, Summary: 2048, Status: 2048, Logs: 2048, Metrics: 2048, Git: 2048}},
		"section too big":  {Pods: r, Objects: r, Redactor: red, Limits: Limits{Total: 4096, Summary: 2048, Status: 2048, Logs: MaxLimitBytes + 1, Metrics: 2048, Git: 2048}},
		"negative timeout": {Pods: r, Objects: r, Redactor: red, SourceTimeout: -time.Second},
		"timeout too long": {Pods: r, Objects: r, Redactor: red, SourceTimeout: MaxSourceTimeout + time.Second},
	} {
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := DefaultLimits().Validate(); err != nil {
		t.Fatal(err)
	}
	b, err := New(Config{Pods: r, Objects: r, Redactor: red})
	if err != nil || b.limits != DefaultLimits() || b.timeout != DefaultSourceTimeout {
		t.Fatalf("defaults: %v %+v", err, b)
	}
	if _, err := b.Build(context.Background(), nil); err == nil {
		t.Fatal("built a context without an Incident")
	}
}

// The window starts before firstSeen, at least MinWindow and at most MaxWindow before now.
func TestWindow(t *testing.T) {
	inc := incident(deploymentRef())
	for name, tc := range map[string]struct {
		first *time.Time
		want  time.Duration
	}{
		"no firstSeen":  {nil, DefaultWindow},
		"30m ago":       {new(testNow.Add(-30 * time.Minute)), 40 * time.Minute},
		"a moment ago":  {new(testNow.Add(-time.Second)), MinWindow},
		"two days ago":  {new(testNow.Add(-48 * time.Hour)), MaxWindow},
		"in the future": {new(testNow.Add(time.Hour)), MinWindow},
	} {
		inc.Status.FirstSeen = nil
		if tc.first != nil {
			inc.Status.FirstSeen = ptrTime(*tc.first)
		}
		w := windowOf(inc, testNow)
		if got := w.end.Sub(w.start); got != tc.want || !w.end.Equal(testNow) {
			t.Errorf("%s: window %s, want %s", name, got, tc.want)
		}
	}
}

// Data is what a quote may cite: nothing of a block that is not available, a log block without its header,
// and otherwise only the recorded values still in the text.
func TestBlockData(t *testing.T) {
	head := "pod web-1 container app, current instance\n"
	restarts := "container_restarts_total{pod=web-1} 7"
	limit := "container_memory_limit_bytes{pod=web-1} 268435456"
	for name, tc := range map[string]struct {
		blk  Block
		want []string
	}{
		"not available": {Block{Section: SectionGit, Availability: NotAvailable,
			Text: "not available: no Git source is configured\n"}, nil},
		"could not fetch": {Block{Section: SectionStatus, Availability: CouldNotFetch,
			Text: "could not fetch: reading the target or its pods failed\n", Values: []string{"target"}}, nil},
		"log header": {Block{Section: SectionLogs, Availability: Available, Text: head + "out of memory\n",
			headLen: len(head)}, []string{"out of memory\n"}},
		"metrics fixed lines": {Block{Section: SectionMetrics, Availability: Available,
			Text: restarts + "\ncontainer_waiting_reason" + fixedCouldNotFetch + "\n" +
				"container_cpu_usage_cores" + fixedNoSeries + partialSuffix + "\n" + limit + "\n",
			Values: []string{restarts, limit}},
			[]string{restarts, limit}},
		"summary values only": {Block{Section: SectionSummary, Availability: Available,
			Text: "symptom: Back-off restarting\noccurrences: 3\n", Values: []string{"Back-off restarting", ""}},
			[]string{"Back-off restarting"}},
		"value cut away": {Block{Section: SectionStatus, Availability: Available, Text: "pod web-abc-1\n",
			Values: []string{crashPod, "web-abc-2", crashPod}}, []string{crashPod}},
	} {
		if got := tc.blk.Data(); strings.Join(got, "|") != strings.Join(tc.want, "|") || len(got) != len(tc.want) {
			t.Errorf("%s: data %q, want %q", name, got, tc.want)
		}
	}
}

// quotable reports whether q lies within one part of the block's data, as the evidence check asks.
func quotable(blk Block, q string) bool {
	for _, part := range blk.Data() {
		if strings.Contains(part, q) {
			return true
		}
	}
	return false
}

// Labels, sentences and numbers the builder writes are never quotable, though they are in the block: only
// values from a source are (review pattern 23). A claimed Incident's phase is the analyzer's own word.
func TestOnlySourceValuesAreQuotable(t *testing.T) {
	inc := incident(deploymentRef())
	inc.Status.Phase = remediationv1alpha1.IncidentAnalyzing
	c := build(t, newBuilder(t, reader(t, webObjects()...), webSources(), nil), inc)
	for _, tc := range []struct {
		section Section
		quote   string
		want    bool
	}{
		{SectionSummary, "phase: Analyzing", false},
		{SectionSummary, "Analyzing", false},
		{SectionSummary, "occurrences: 3", false},
		{SectionSummary, "reason: BackOff", false},
		{SectionSummary, "Back-off restarting failed container app", true},
		{SectionStatus, "owns ReplicaSet web-abc", false},
		{SectionStatus, "current pods: 2", false},
		{SectionStatus, "restarts 4", false},
		{SectionStatus, "waiting, reason CrashLoopBackOff", false},
		{SectionStatus, crashLoop, true},
		{SectionStatus, unavailable, true},
		{SectionLogs, "previous instance", false},
		{SectionLogs, "panic: boom", true},
		{SectionMetrics, "container_restarts_total{container=app,pod=web-abc-1} 1", true},
	} {
		blk := one(t, c, tc.section)
		if !strings.Contains(blk.Text, tc.quote) {
			t.Fatalf("%s lacks %q:\n%s", tc.section, tc.quote, blk.Text)
		}
		if got := quotable(blk, tc.quote); got != tc.want {
			t.Errorf("%s: quoting %q is evidence: %t, want %t", tc.section, tc.quote, got, tc.want)
		}
	}

	ref := deploymentRef()
	ref.UID = "uid-of-an-older-web"
	gone := one(t, build(t, newBuilder(t, reader(t, webObjects()...), webSources(), nil), incident(ref)), SectionStatus)
	const sentence = "no longer exists, or was replaced by another object of the same name"
	if !strings.Contains(gone.Text, sentence) || quotable(gone, sentence) || quotable(gone, "Deployment shop/web") {
		t.Errorf("the gone-target sentence is quotable: %q of\n%s", gone.Data(), gone.Text)
	}
	if !quotable(gone, "Deployment") {
		t.Errorf("the subject's kind is not quotable: %q", gone.Data())
	}
}

// A later occurrence of a fingerprint (design 3.2, #251) is analyzed from its symptoms and context only: the
// link to the Incident before it never reaches the model, so neither does anything of that one's outcome or
// policy decision (invariant 1). Its context is the one occurrence 1 would get.
func TestLaterOccurrenceCarriesNothingOfThePreviousOne(t *testing.T) {
	b := newBuilder(t, reader(t, webObjects()...), webSources(), nil)
	first := incident(deploymentRef())
	later := first.DeepCopy()
	later.Name = remediationv1alpha1.IncidentName(later.Spec.Fingerprint, 2)
	later.Spec.OccurrenceNumber = 2
	later.Spec.PreviousIncident = remediationv1alpha1.IncidentName(later.Spec.Fingerprint, 1)
	want, got := build(t, b, first), build(t, b, later)
	if len(got.Blocks) != len(want.Blocks) {
		t.Fatalf("%d blocks, want %d", len(got.Blocks), len(want.Blocks))
	}
	for i := range got.Blocks {
		if got.Blocks[i].Text != want.Blocks[i].Text {
			t.Errorf("block %s differs:\n%s\nwant:\n%s", got.Blocks[i].Section, got.Blocks[i].Text, want.Blocks[i].Text)
		}
		if strings.Contains(got.Blocks[i].Text, later.Spec.PreviousIncident) {
			t.Errorf("block %s names the previous Incident", got.Blocks[i].Section)
		}
	}
}

// limits returns resources with a memory limit q, or none when q is empty.
func limits(q string) corev1.ResourceRequirements {
	if q == "" {
		return corev1.ResourceRequirements{}
	}
	return corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(q)}}
}

// During a rollout the pod template and a running pod hold different memory limits: the status block labels
// each, and the context carries the template's for the analysis loop, which takes a proposal's previous
// value from it (#494).
func TestTemplateAndRunningPodLimitsAreLabelled(t *testing.T) {
	d := deployment()
	d.Spec.Template.Spec.Containers = []corev1.Container{{Name: appName, Resources: limits("48Mi")}, {Name: "sidecar"}}
	rs := replicaSet("web-abc", "uid-rs-abc", d, longAgo)
	ref := controllerRef(kindReplicaSet, rs.Name, rs.UID)
	old := crashing(pod(crashPod, rs.Labels, longAgo, ref), "exited")
	old.Spec.Containers = []corev1.Container{{Name: appName, Resources: limits("64Mi")}}
	c := build(t, newBuilder(t, reader(t, d, rs, old), sources{}, nil), incident(deploymentRef()))

	want := []TemplateContainer{{Name: appName, MemoryLimit: new(resource.MustParse("48Mi"))}, {Name: "sidecar"}}
	if len(c.Template) != len(want) {
		t.Fatalf("template %+v", c.Template)
	}
	for i, w := range want {
		got := c.Template[i]
		if got.Name != w.Name || (got.MemoryLimit == nil) != (w.MemoryLimit == nil) ||
			(w.MemoryLimit != nil && got.MemoryLimit.Cmp(*w.MemoryLimit) != 0) {
			t.Errorf("template container %d: %+v, want %+v", i, got, w)
		}
	}
	text := one(t, c, SectionStatus).Text
	for _, line := range []string{
		"  pod template (the workload's current spec): container app, memory limit 48Mi\n",
		"  pod template (the workload's current spec): container sidecar, memory limit not set\n",
		"; this pod's memory limit 64Mi\n",
	} {
		if !strings.Contains(text, line) {
			t.Errorf("the status block lacks %q:\n%s", line, text)
		}
	}

	// A StatefulSet's template is carried too; a ReplicaSet, a pod or a target that is gone has none.
	s := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: testNS, UID: dbUID, CreationTimestamp: longAgo},
		Spec: appsv1.StatefulSetSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{appName: "db"}},
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{
				{Name: "db", Resources: limits("1Gi")}}}}},
	}
	sRef := remediationv1alpha1.ObjectRef{APIVersion: apiApps, Kind: kindStatefulSet, Namespace: testNS, Name: "db", UID: dbUID}
	c = build(t, newBuilder(t, reader(t, s), sources{}, nil), incident(sRef))
	if len(c.Template) != 1 || c.Template[0].MemoryLimit == nil || c.Template[0].MemoryLimit.String() != "1Gi" {
		t.Errorf("StatefulSet template %+v", c.Template)
	}
	rsRef := remediationv1alpha1.ObjectRef{APIVersion: apiApps, Kind: kindReplicaSet, Namespace: testNS, Name: rs.Name,
		UID: remediationv1alpha1.UID(rs.UID)}
	gone := deploymentRef()
	gone.UID = "uid-recreated"
	for name, subject := range map[string]remediationv1alpha1.ObjectRef{"ReplicaSet": rsRef, "gone": gone} {
		if c := build(t, newBuilder(t, reader(t, d, rs, old), sources{}, nil), incident(subject)); c.Template != nil {
			t.Errorf("%s: template %+v", name, c.Template)
		}
	}
}

// A running pod's own memory limit is left out only where the pod template has the same container with the
// same limit, written in any form: the template line shows it, and with it repeated on the line that holds
// the restart count, oom diagnoses stopped naming that count and quoting the metric lines (#499). Everywhere
// else it is shown: a pod of a rollout under another limit or none (#494), a container the template lacks
// (an injected sidecar), an init container, and every pod of a subject without a pod template.
func TestRunningPodLimitHiddenOnlyWhereTheTemplateShowsIt(t *testing.T) {
	d := deployment()
	d.Spec.Template.Spec.Containers = []corev1.Container{{Name: appName, Resources: limits("48Mi")}}
	rs := replicaSet("web-abc", "uid-rs-abc", d, longAgo)
	ref := controllerRef(kindReplicaSet, rs.Name, rs.UID)
	same := crashing(pod(crashPod, rs.Labels, longAgo, ref), "exited")
	same.Spec.Containers = []corev1.Container{{Name: appName, Resources: limits("50331648")}}
	older := pod("web-abc-2", rs.Labels, longAgo, ref)
	older.Spec.Containers = []corev1.Container{{Name: appName, Resources: limits("64Mi")}}
	unset := pod("web-abc-3", rs.Labels, longAgo, ref)
	unset.Spec.Containers = []corev1.Container{{Name: appName}}
	extra := pod("web-abc-4", rs.Labels, longAgo, ref)
	extra.Spec.Containers = []corev1.Container{{Name: appName, Resources: limits("48Mi")},
		{Name: "istio-proxy", Resources: limits("128Mi")}}
	extra.Spec.InitContainers = []corev1.Container{{Name: "setup", Resources: limits("32Mi")}}
	extra.Status.ContainerStatuses = append(extra.Status.ContainerStatuses, corev1.ContainerStatus{
		Name: "istio-proxy", Ready: true,
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: longAgo}},
	})
	extra.Status.InitContainerStatuses = []corev1.ContainerStatus{{
		Name: "setup", Ready: true,
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Completed"}},
	}}
	objs := []client.Object{d, rs, same, older, unset, extra}

	text := one(t, build(t, newBuilder(t, reader(t, objs...), sources{}, nil), incident(deploymentRef())),
		SectionStatus).Text
	for line := range strings.SplitSeq(text, "\n") {
		if strings.Contains(line, "restarts 4") && strings.Contains(line, "memory limit") {
			t.Errorf("the restart line of a pod whose limit is the template's repeats it: %q", line)
		}
	}
	for _, want := range []string{
		"; this pod's memory limit 64Mi\n",
		"; this pod's memory limit not set\n",
		"  container istio-proxy: ready true, restarts 0; now running since",
		"; this pod's memory limit 128Mi\n",
		"; this pod's memory limit 32Mi\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the status block lacks %q:\n%s", want, text)
		}
	}
	if got := strings.Count(text, "this pod's memory limit"); got != 4 {
		t.Errorf("%d pod limits shown, want 4 (all but the two equal to the template's):\n%s", got, text)
	}

	// A ReplicaSet subject has no pod template line: every pod shows its own limit.
	rsRef := remediationv1alpha1.ObjectRef{APIVersion: apiApps, Kind: kindReplicaSet, Namespace: testNS, Name: rs.Name,
		UID: remediationv1alpha1.UID(rs.UID)}
	text = one(t, build(t, newBuilder(t, reader(t, objs...), sources{}, nil), incident(rsRef)), SectionStatus).Text
	if got := strings.Count(text, "this pod's memory limit"); got != 6 {
		t.Errorf("a ReplicaSet subject shows %d pod limits, want 6:\n%s", got, text)
	}
}
