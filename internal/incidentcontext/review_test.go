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
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/analyzer/ownership"
	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/redact"
)

// A live workload whose labels or selector are beyond the bounds is out of bounds, not gone.
func TestOutOfBoundsWorkloadIsNotGone(t *testing.T) {
	manyLabels := map[string]string{}
	for i := range ownership.MaxLabels + 1 {
		manyLabels[fmt.Sprintf("l%d", i)] = "v"
	}
	for name, mutate := range map[string]func(objs []client.Object){
		"labels": func(objs []client.Object) { objs[0].SetLabels(manyLabels) },
		"selector": func(objs []client.Object) {
			d := deployment()
			d.Spec.Selector.MatchLabels = manyLabels
			objs[0] = d
		},
	} {
		objs := webObjects()
		mutate(objs)
		b := newBuilder(t, reader(t, objs...), webSources(), nil)
		c := build(t, b, incident(deploymentRef()))
		status := one(t, c, SectionStatus)
		if status.Availability != Available || strings.Contains(status.Text, "no longer exists") ||
			!strings.Contains(status.Text, "larger than the builder follows") ||
			!strings.HasPrefix(status.Text, "Deployment shop/web,") {
			t.Errorf("%s: %q\n%s", name, status.Availability, status.Text)
		}
		got := b.resolve(context.Background(), deploymentRef(), windowOf(incident(deploymentRef()), testNow))
		if got.state != targetResolved || !got.outOfBounds || len(got.pods) != 0 {
			t.Errorf("%s: state %d, out of bounds %t, pods %v", name, got.state, got.outOfBounds, podNames(&got))
		}
	}
}

func onePodTarget(p *corev1.Pod) *target {
	return &target{state: targetResolved, pods: []corev1.Pod{*p}}
}

// A container whose last instance terminated before the window is not read as a previous instance.
func TestStaleTerminationIsNotPickedAsPrevious(t *testing.T) {
	w := windowOf(incident(deploymentRef()), testNow)
	p := pod(crashPod, nil, longAgo)
	p.Status.ContainerStatuses[0].RestartCount = 1
	p.Status.ContainerStatuses[0].LastTerminationState = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
		Reason: "Error", ExitCode: 1, StartedAt: longAgo, FinishedAt: longAgo,
	}}
	picks := logPicks(onePodTarget(p), w)
	if len(picks) != 1 || picks[0].previous || picks[0].score != 0 {
		t.Fatalf("stale termination: %+v", picks)
	}
	p.Status.ContainerStatuses[0].LastTerminationState.Terminated.FinishedAt = recent
	if picks := logPicks(onePodTarget(p), w); len(picks) != 1 || !picks[0].previous {
		t.Fatalf("termination inside the window: %+v", picks)
	}
}

// A completed init container is neither trouble nor the fallback: the regular container is read.
func TestCompletedInitContainerIsNotPicked(t *testing.T) {
	w := windowOf(incident(deploymentRef()), testNow)
	p := pod(crashPod, nil, longAgo)
	p.Status.InitContainerStatuses = []corev1.ContainerStatus{{
		Name: "migrate", Ready: false,
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Completed", ExitCode: 0, FinishedAt: longAgo}},
	}}
	picks := logPicks(onePodTarget(p), w)
	if len(picks) != 1 || picks[0].init || picks[0].container != appName {
		t.Fatalf("picks %+v", picks)
	}
	if tr := troubleOf(p); tr != (trouble{}) {
		t.Fatalf("a completed init container counts as trouble: %+v", tr)
	}
	// An init container that failed is still trouble, and read.
	p.Status.InitContainerStatuses[0].State.Terminated.ExitCode = 1
	if picks := logPicks(onePodTarget(p), w); len(picks) != 1 || !picks[0].init {
		t.Fatalf("failed init container: %+v", picks)
	}
}

func TestDropLeadingContinuation(t *testing.T) {
	for _, tc := range []struct {
		in, want string
		dropped  bool
	}{
		{"first\n  second\n", "first\n  second\n", false},
		{"  value\nkey: v\n", "key: v\n", true},
		{"\tvalue\n\n- item\n-\n-\tx\nnext\n", "next\n", true},
		{"-x\n", "-x\n", false},
		{"  only\n  indented\n", "", true},
		{"\r\n  x\r\nline\r\n", "line\r\n", true},
		{"  tail without newline", "", true},
		{"", "", false},
	} {
		got, dropped := dropLeadingContinuation(tc.in)
		if got != tc.want || dropped != tc.dropped {
			t.Errorf("dropLeadingContinuation(%q) = %q, %t; want %q, %t", tc.in, got, dropped, tc.want, tc.dropped)
		}
	}
}

// A log excerpt that the source cut just below the key line of a block-scalar secret starts with the secret's
// value alone, which the redactor does not recognise; the builder drops it.
func TestOrphanedBlockScalarSecretIsDropped(t *testing.T) {
	const secret = "correct horse battery staple"
	red, err := redact.New()
	if err != nil {
		t.Fatal(err)
	}
	whole := "loading config\npassword: |\n  " + secret + "\nlistening on :8080\n"
	// The source cut its byte bound at the start of the value line.
	excerpt := whole[strings.Index(whole, "  "+secret):]
	if !strings.Contains(red.String(excerpt), secret) {
		t.Fatal("precondition: the redactor alone keeps the orphaned value")
	}
	b := &Builder{redactor: red, limits: DefaultLimits()}
	pick := logPick{namespace: testNS, pod: crashPod, container: appName}
	blk, failed := b.logBlock(pick, providers.LogExcerpt{Text: excerpt, Truncated: true}, nil, 4096)
	if failed || strings.Contains(blk.Text, secret) || !strings.HasSuffix(blk.Text, "listening on :8080\n") ||
		!hasLabel(blk, LabelLeadingLinesDropped) {
		t.Fatalf("failed %t, labels %v:\n%s", failed, blk.Labels, blk.Text)
	}
}

// The build deadline bounds the whole build: once it passes, the remaining source calls are not made and
// their sections are "could not fetch", an escalation; the build itself does not fail.
func TestBuildDeadline(t *testing.T) {
	var metricCalls, logCalls atomic.Int32
	src := webSources()
	b := newBuilder(t, reader(t, webObjects()...), src, func(c *Config) {
		c.BuildTimeout = 50 * time.Millisecond
		c.Logs = logFunc(func(ctx context.Context, _ providers.LogQuery) (providers.LogExcerpt, error) {
			logCalls.Add(1)
			select {
			case <-ctx.Done():
				return providers.LogExcerpt{}, ctx.Err()
			case <-time.After(2 * time.Second):
				return providers.LogExcerpt{}, errors.New("slow source")
			}
		})
		c.Metrics = metricsFunc(func(context.Context, providers.MetricsQuery) (providers.MetricsResult, error) {
			metricCalls.Add(1)
			return providers.MetricsResult{}, nil
		})
	})
	start := time.Now()
	c := build(t, b, incident(deploymentRef()))
	if d := time.Since(start); d > time.Second {
		t.Errorf("the build took %s", d)
	}
	if metricCalls.Load() != 0 || logCalls.Load() != 1 {
		t.Errorf("%d log calls and %d metric calls after the deadline", logCalls.Load(), metricCalls.Load())
	}
	if one(t, c, SectionMetrics).Availability != CouldNotFetch || one(t, c, SectionLogs).Availability != CouldNotFetch ||
		!c.Escalate() {
		t.Fatalf("after the deadline: failed %v, blocks %+v", c.FailedSources, c.Blocks)
	}
	if _, err := New(Config{Pods: reader(t), Objects: reader(t), Redactor: canary{}, BuildTimeout: MaxBuildTimeout + 1}); err == nil {
		t.Error("accepted a build timeout over the maximum")
	}
	if _, err := New(Config{Pods: reader(t), Objects: reader(t), Redactor: canary{}, BuildTimeout: -1}); err == nil {
		t.Error("accepted a negative build timeout")
	}
}

// The pod list asks the cache for the workload's selector, not the whole namespace.
func TestListPodsPassesTheSelector(t *testing.T) {
	var selectors []string
	r := clientfake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(webObjects()...).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				var lo client.ListOptions
				lo.ApplyOptions(opts)
				s := "none"
				if lo.LabelSelector != nil {
					s = lo.LabelSelector.String()
				}
				selectors = append(selectors, lo.Namespace+" "+s)
				return c.List(ctx, list, opts...)
			},
		}).Build()
	b := newBuilder(t, r, sources{}, nil)
	got := b.resolve(context.Background(), deploymentRef(), windowOf(incident(deploymentRef()), testNow))
	if got.state != targetResolved || strings.Join(selectors, ",") != "shop app=web" {
		t.Fatalf("state %d, list calls %v", got.state, selectors)
	}
}

// The status names the real number of current pods and says when only the most troubled are shown.
func TestStatusCountsAllPods(t *testing.T) {
	d := deployment()
	rs := replicaSet("web-abc", "uid-rs-abc", d, longAgo)
	rsRef := controllerRef(kindReplicaSet, rs.Name, rs.UID)
	objs := make([]client.Object, 0, 2+MaxPods+5)
	objs = append(objs, d, rs)
	for i := range MaxPods + 5 {
		objs = append(objs, pod(fmt.Sprintf("web-abc-%02d", i), rs.Labels, longAgo, rsRef))
	}
	b := newBuilder(t, reader(t, objs...), sources{}, func(c *Config) { c.Limits = DefaultLimits(); c.Limits.Status = MaxLimitBytes })
	c := build(t, b, incident(deploymentRef()))
	want := fmt.Sprintf("current pods: %d, the %d most troubled shown\n", MaxPods+5, MaxPods)
	if status := one(t, c, SectionStatus); !strings.Contains(status.Text, want) || !hasLabel(status, LabelPodsCapped) {
		t.Fatalf("labels %v:\n%s", status.Labels, status.Text)
	}
	c = build(t, newBuilder(t, reader(t, webObjects()...), sources{}, nil), incident(deploymentRef()))
	if status := one(t, c, SectionStatus); !strings.Contains(status.Text, "current pods: 2\n") {
		t.Fatalf("uncapped:\n%s", status.Text)
	}
}

// A Pod subject is matched by its UID, so a pod created inside the window is not uncertain.
func TestRecentPodSubjectIsCertain(t *testing.T) {
	p := pod(crashPod, nil, recent)
	ref := remediationv1alpha1.ObjectRef{APIVersion: apiCore, Kind: kindPod, Namespace: testNS, Name: p.Name, UID: remediationv1alpha1.UID(p.UID)}
	got := resolveWith(t, ref, p)
	if got.state != targetResolved || got.uncertain || len(got.pods) != 1 {
		t.Fatalf("state %d, uncertain %t, pods %v", got.state, got.uncertain, podNames(&got))
	}
}
