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
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/providers/fake"
	"github.com/pkalin/shadewright/internal/providers/metrics/prometheus"
)

func podNames(t *target) []string {
	out := make([]string, len(t.pods))
	for i := range t.pods {
		out[i] = t.pods[i].Name
	}
	slices.Sort(out)
	return out
}

func resolveWith(t *testing.T, ref remediationv1alpha1.ObjectRef, objs ...client.Object) target {
	t.Helper()
	b := newBuilder(t, reader(t, objs...), sources{}, nil)
	inc := incident(ref)
	return b.resolve(context.Background(), ref, windowOf(inc, testNow))
}

// Only the Deployment's own pods are its pods: owner references are followed and checked, never names.
func TestDeploymentPodsByOwnerReferences(t *testing.T) {
	d := deployment()
	rs := replicaSet("web-abc", "uid-rs-abc", d, longAgo)
	rsRef := controllerRef(kindReplicaSet, rs.Name, rs.UID)
	// A ReplicaSet of another Deployment, selected by the same labels.
	otherD := deployment()
	otherD.Name, otherD.UID = "web-v2", "uid-web-v2"
	otherRS := replicaSet("web-v2-abc", "uid-rs-v2", otherD, longAgo)
	other := pod("other", map[string]string{appName: "other"}, longAgo)
	other.Namespace = otherNS
	objs := []client.Object{
		d, rs, otherD, otherRS,
		pod(crashPod, rs.Labels, longAgo, rsRef),
		// Named like the target's pods, but owned by another ReplicaSet.
		pod("web-abc-2", otherRS.Labels, longAgo, controllerRef(kindReplicaSet, otherRS.Name, otherRS.UID)),
		// Owner reference to the real ReplicaSet with a forged UID.
		pod("forged-uid", rs.Labels, longAgo, controllerRef(kindReplicaSet, rs.Name, "uid-forged")),
		// The right owner reference, but not selected by the ReplicaSet.
		pod("unselected", map[string]string{appName: webName}, longAgo, rsRef),
		// The target's labels without an owner, a StatefulSet pod named like the Deployment, a non-controller
		// reference, and the right labels in another namespace.
		pod("web-orphan", rs.Labels, longAgo),
		pod("web-v2-0", rs.Labels, longAgo, controllerRef(kindStatefulSet, "web-v2", "uid-sts")),
		pod("not-controller", rs.Labels, longAgo, metav1.OwnerReference{APIVersion: apiApps, Kind: kindReplicaSet, Name: rs.Name, UID: rs.UID}),
		func() *corev1.Pod {
			p := pod("web-abc-9", rs.Labels, longAgo, rsRef)
			p.Namespace = otherNS
			return p
		}(),
		other,
	}
	got := resolveWith(t, deploymentRef(), objs...)
	if got.state != targetResolved || strings.Join(podNames(&got), ",") != crashPod {
		t.Fatalf("state %d, pods %v", got.state, podNames(&got))
	}
	if got.uncertain || got.capped {
		t.Errorf("uncertain %t, capped %t", got.uncertain, got.capped)
	}
}

// A ReplicaSet that names the Deployment with another UID is not one of its ReplicaSets.
func TestReplicaSetOfAnotherDeploymentIsRefused(t *testing.T) {
	d := deployment()
	impostor := deployment()
	impostor.UID = "uid-impostor"
	rs := replicaSet("web-abc", "uid-rs-abc", impostor, longAgo)
	got := resolveWith(t, deploymentRef(), d, rs, pod(crashPod, rs.Labels, longAgo, controllerRef(kindReplicaSet, rs.Name, rs.UID)))
	if len(got.pods) != 0 {
		t.Fatalf("pods %v", podNames(&got))
	}
}

func statefulSet() *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: testNS, UID: dbUID, CreationTimestamp: longAgo},
		Spec: appsv1.StatefulSetSpec{
			Replicas: ptr.To[int32](2), Selector: &metav1.LabelSelector{MatchLabels: map[string]string{appName: "db"}},
		},
		Status: appsv1.StatefulSetStatus{CurrentRevision: dbRevision, UpdateRevision: dbRevision},
	}
}

func TestStatefulSetPods(t *testing.T) {
	s := statefulSet()
	ref := controllerRef(kindStatefulSet, s.Name, s.UID)
	l := map[string]string{appName: "db", revisionLabel: dbRevision}
	target := remediationv1alpha1.ObjectRef{APIVersion: apiApps, Kind: kindStatefulSet, Namespace: testNS, Name: "db", UID: dbUID}
	objs := []client.Object{s, pod("db-0", l, longAgo, ref), pod(dbRevision, l, longAgo, ref),
		pod("db-2", l, longAgo, controllerRef(kindStatefulSet, s.Name, "uid-old-db"))}
	got := resolveWith(t, target, objs...)
	if strings.Join(podNames(&got), ",") != "db-0,db-1" || got.uncertain {
		t.Fatalf("pods %v, uncertain %t", podNames(&got), got.uncertain)
	}
	// A rolling update in progress makes the attribution uncertain.
	s.Status.UpdateRevision = "db-2"
	if got := resolveWith(t, target, objs...); !got.uncertain {
		t.Error("a StatefulSet between revisions is not uncertain")
	}
	s.Status.UpdateRevision = dbRevision
	newer := pod(dbRevision, map[string]string{appName: "db", revisionLabel: "db-2"}, longAgo, ref)
	if got := resolveWith(t, target, s, pod("db-0", l, longAgo, ref), newer); !got.uncertain {
		t.Error("pods of two revisions are not uncertain")
	}
}

func TestReplicaSetAndPodTargets(t *testing.T) {
	d := deployment()
	rs := replicaSet("web-abc", "uid-rs-abc", d, longAgo)
	p := pod(crashPod, rs.Labels, longAgo, controllerRef(kindReplicaSet, rs.Name, rs.UID))
	rsTarget := remediationv1alpha1.ObjectRef{APIVersion: apiApps, Kind: kindReplicaSet, Namespace: testNS, Name: rs.Name, UID: remediationv1alpha1.UID(rs.UID)}
	if got := resolveWith(t, rsTarget, d, rs, p); strings.Join(podNames(&got), ",") != crashPod {
		t.Errorf("ReplicaSet target pods %v", podNames(&got))
	}
	podTarget := remediationv1alpha1.ObjectRef{APIVersion: apiCore, Kind: kindPod, Namespace: testNS, Name: p.Name, UID: remediationv1alpha1.UID(p.UID)}
	if got := resolveWith(t, podTarget, p); got.state != targetResolved || len(got.pods) != 1 {
		t.Errorf("Pod target: state %d, pods %v", got.state, podNames(&got))
	}
	podTarget.UID = "uid-recreated"
	if got := resolveWith(t, podTarget, p); got.state != targetGone {
		t.Errorf("a recreated pod is not gone: %d", got.state)
	}
}

// The pod set is uncertain when the Deployment rolled or a pod started inside the window.
func TestDeploymentAttributionUncertainty(t *testing.T) {
	d := deployment()
	old := replicaSet("web-old", "uid-rs-old", d, longAgo)
	cur := replicaSet("web-new", "uid-rs-new", d, longAgo)
	oldRef := controllerRef(kindReplicaSet, old.Name, old.UID)
	curRef := controllerRef(kindReplicaSet, cur.Name, cur.UID)
	for name, tc := range map[string]struct {
		objs []client.Object
		want bool
	}{
		"steady":                {[]client.Object{d, old, pod("web-old-1", old.Labels, longAgo, oldRef)}, false},
		"two ReplicaSets":       {[]client.Object{d, old, cur, pod("web-old-1", old.Labels, longAgo, oldRef), pod("web-new-1", cur.Labels, longAgo, curRef)}, true},
		"pod started in window": {[]client.Object{d, old, pod("web-old-1", old.Labels, recent, oldRef)}, true},
		"ReplicaSet created in window": {[]client.Object{d, replicaSet("web-new", "uid-rs-new", d, recent),
			pod("web-new-1", cur.Labels, longAgo, curRef)}, true},
	} {
		if got := resolveWith(t, deploymentRef(), tc.objs...); got.uncertain != tc.want || len(got.pods) == 0 {
			t.Errorf("%s: uncertain %t, want %t (pods %v)", name, got.uncertain, tc.want, podNames(&got))
		}
	}
}

// Pods and ReplicaSets beyond the bounds are left out and the set is marked capped; the ReplicaSets named
// like the Deployment's are read first, so pods naming many others cannot crowd them out.
func TestPodSetBounds(t *testing.T) {
	d := deployment()
	rs := replicaSet("web-abc", "uid-rs-abc", d, longAgo)
	rsRef := controllerRef(kindReplicaSet, rs.Name, rs.UID)
	objs := make([]client.Object, 0, 2+MaxPods+5+MaxReplicaSets+2)
	objs = append(objs, d, rs)
	for i := range MaxPods + 5 {
		objs = append(objs, pod(fmt.Sprintf("web-abc-%02d", i), rs.Labels, longAgo, rsRef))
	}
	for i := range MaxReplicaSets + 2 {
		objs = append(objs, pod(fmt.Sprintf("decoy-%d", i), rs.Labels, longAgo,
			controllerRef(kindReplicaSet, fmt.Sprintf("a-decoy-%d", i), "uid-decoy")))
	}
	got := resolveWith(t, deploymentRef(), objs...)
	if len(got.pods) != MaxPods || !got.capped {
		t.Fatalf("%d pods, capped %t", len(got.pods), got.capped)
	}
}

// Pods naming more made-up ReplicaSets, named like the Deployment's, than the bound do not crowd out the real
// ReplicaSet when each is named by fewer pods.
func TestDecoyReplicaSetsDoNotCrowdOut(t *testing.T) {
	d := deployment()
	rs := replicaSet("web-zzz", "uid-rs-zzz", d, longAgo)
	rsRef := controllerRef(kindReplicaSet, rs.Name, rs.UID)
	objs := make([]client.Object, 0, 4+MaxReplicaSets+2)
	objs = append(objs, d, rs, pod("web-zzz-1", rs.Labels, longAgo, rsRef), pod("web-zzz-2", rs.Labels, longAgo, rsRef))
	for i := range MaxReplicaSets + 2 {
		name := fmt.Sprintf("web-aaa%d", i)
		objs = append(objs, pod(name+"-1", rs.Labels, longAgo, controllerRef(kindReplicaSet, name, "uid-decoy")))
	}
	got := resolveWith(t, deploymentRef(), objs...)
	if strings.Join(podNames(&got), ",") != "web-zzz-1,web-zzz-2" || !got.capped {
		t.Fatalf("pods %v, capped %t", podNames(&got), got.capped)
	}
}

// The metrics source gets exactly the resolved pods, never more than MaxMetricPods, and each template is
// one the Prometheus adapter approves, with or without pods as it requires.
func TestMetricQueriesUseResolvedPods(t *testing.T) {
	approved := map[string]prometheus.TemplateInfo{}
	var podTemplates []string
	for _, info := range prometheus.Templates() {
		approved[info.Name] = info
		if info.Pods {
			podTemplates = append(podTemplates, info.Name)
		}
	}
	for _, p := range metricPlans {
		info, ok := approved[p.template]
		if !ok || info.Pods != p.pods {
			t.Errorf("plan %q: approved %t, pods %t", p.template, ok, info.Pods)
		}
		if !p.pods && !slices.Contains(info.Params, paramWorkload) {
			t.Errorf("plan %q takes no workload", p.template)
		}
	}
	src := webSources()
	src.metrics.PodTemplates = podTemplates
	var mu sync.Mutex
	var queries []providers.MetricsQuery
	b := newBuilder(t, reader(t, webObjects()...), src, func(c *Config) {
		inner := c.Metrics
		c.Metrics = metricsFunc(func(ctx context.Context, q providers.MetricsQuery) (providers.MetricsResult, error) {
			mu.Lock()
			queries = append(queries, q)
			mu.Unlock()
			return inner.Query(ctx, q)
		})
	})
	c := build(t, b, incident(deploymentRef()))
	if m := one(t, c, SectionMetrics); m.Availability != Available || hasLabel(m, LabelIncomplete) {
		t.Fatalf("metrics %q %v:\n%s", m.Availability, m.Labels, m.Text)
	}
	if len(queries) != len(metricPlans) {
		t.Fatalf("%d queries", len(queries))
	}
	for _, q := range queries {
		if q.Params[paramNamespace] != testNS {
			t.Errorf("%s: params %v", q.Template, q.Params)
		}
		if approved[q.Template].Pods && strings.Join(q.Pods, ",") != "web-abc-1,web-abc-2" {
			t.Errorf("%s: pods %v", q.Template, q.Pods)
		}
		if !approved[q.Template].Pods && q.Params[paramWorkload] != webName {
			t.Errorf("%s: params %v", q.Template, q.Params)
		}
		if !q.End.Equal(testNow) || (!q.Start.IsZero() && q.Step < 60e9) {
			t.Errorf("%s: start %s end %s step %s", q.Template, q.Start, q.End, q.Step)
		}
	}
}

// A StatefulSet target asks no Deployment template.
func TestMetricsOfAStatefulSet(t *testing.T) {
	s := statefulSet()
	ref := controllerRef(kindStatefulSet, s.Name, s.UID)
	target := remediationv1alpha1.ObjectRef{APIVersion: apiApps, Kind: kindStatefulSet, Namespace: testNS, Name: "db", UID: dbUID}
	src := sources{metrics: &fake.Metrics{}}
	var asked []string
	b := newBuilder(t, reader(t, s, pod("db-0", map[string]string{appName: "db"}, longAgo, ref)), src, func(c *Config) {
		c.Metrics = metricsFunc(func(_ context.Context, q providers.MetricsQuery) (providers.MetricsResult, error) {
			asked = append(asked, q.Template)
			return providers.MetricsResult{}, nil
		})
	})
	build(t, b, incident(target))
	if slices.Contains(asked, "deployment_replicas_unavailable") || len(asked) != len(metricPlans)-1 {
		t.Fatalf("asked %v", asked)
	}
}

// The log section reads the previous instance of a container that terminated, then a waiting one, at most
// MaxLogExcerpts, and splits its bound between them.
func TestLogPicks(t *testing.T) {
	d := deployment()
	rs := replicaSet("web-abc", "uid-rs-abc", d, longAgo)
	ref := controllerRef(kindReplicaSet, rs.Name, rs.UID)
	waiting := pod("web-abc-2", rs.Labels, longAgo, ref)
	waiting.Status.ContainerStatuses[0].Ready = false
	waiting.Status.ContainerStatuses[0].State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}}
	objs := []client.Object{d, rs, pod("web-abc-0", rs.Labels, longAgo, ref), crashing(pod(crashPod, rs.Labels, longAgo, ref), "x"), waiting}
	var mu sync.Mutex
	var asked []providers.LogQuery
	b := newBuilder(t, reader(t, objs...), sources{}, func(c *Config) {
		c.Logs = logFunc(func(_ context.Context, q providers.LogQuery) (providers.LogExcerpt, error) {
			mu.Lock()
			asked = append(asked, q)
			mu.Unlock()
			return providers.LogExcerpt{Text: "line\n"}, nil
		})
	})
	c := build(t, b, incident(deploymentRef()))
	if len(asked) != MaxLogExcerpts || len(blocks(c, SectionLogs)) != MaxLogExcerpts {
		t.Fatalf("asked %+v", asked)
	}
	if q := asked[0]; q.Pod != crashPod || !q.Previous || q.Since != 0 || q.MaxBytes != DefaultLogBytes/2 {
		t.Errorf("first query %+v", q)
	}
	if q := asked[1]; q.Pod != "web-abc-2" || q.Previous || q.Since != 40*60e9 {
		t.Errorf("second query %+v", q)
	}
	// Without trouble, one excerpt of the current instance.
	b2 := newBuilder(t, reader(t, d, rs, pod("web-abc-0", rs.Labels, longAgo, ref)), sources{}, func(c *Config) { c.Logs = b.logs })
	asked = nil
	build(t, b2, incident(deploymentRef()))
	if len(asked) != 1 || asked[0].Previous || asked[0].MaxBytes != DefaultLogBytes {
		t.Errorf("no trouble: %+v", asked)
	}
}

func TestKeepHeadAndTail(t *testing.T) {
	const s = "one\ntwo\nthree\n"
	for _, tc := range []struct {
		n          int
		head, tail string
	}{
		{100, s, s}, {len(s), s, s}, {13, "one\ntwo\n", "two\nthree\n"}, {8, "one\ntwo\n", "three\n"},
		{6, "one\n", "three\n"}, {3, "", ""}, {0, "", ""}, {-1, "", ""},
	} {
		if h, _ := keepHead(s, tc.n); h != tc.head {
			t.Errorf("keepHead(%d) = %q, want %q", tc.n, h, tc.head)
		}
		if tl, _ := keepTail(s, tc.n); tl != tc.tail {
			t.Errorf("keepTail(%d) = %q, want %q", tc.n, tl, tc.tail)
		}
	}
	if tl, cut := keepTail("partial\nlast", 3); tl != "" || !cut {
		t.Errorf("keepTail kept a partial line: %q", tl)
	}
	if tl, _ := keepTail("a\nlast", 4); tl != "last" {
		t.Errorf("keepTail = %q", tl)
	}
}
