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
	"encoding/base64"
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/providers/fake"
	"github.com/pkalin/shadewright/internal/redact"
)

const (
	testNS      = "shop"
	otherNS     = "elsewhere"
	appName     = "app"
	webName     = "web"
	crashPod    = "web-abc-1"
	unavailable = "MinimumReplicasUnavailable"
	crashLoop   = "CrashLoopBackOff"
	dbUID       = "uid-db"
	dbRevision  = "db-1"
)

var (
	testNow = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	// longAgo is before any window; recent is inside the default window of the test Incident.
	longAgo = metav1.NewTime(testNow.Add(-48 * time.Hour))
	recent  = metav1.NewTime(testNow.Add(-5 * time.Minute))
)

// Synthetic credentials that the built-in redaction rules remove. They are built, not written as literals.
var (
	leakJWT = b64(`{"alg":"HS256","typ":"JWT"}`) + "." + b64(`{"sub":"synthetic-subject"}`) + "." +
		b64("synthetic-signature-0123456789")
	leakDSN = "postgres://app:" + "Zq8-synthetic-Pw0rd" + "@db.shop.svc:5432/orders"
)

func b64(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

func ptrTime(t time.Time) *metav1.Time {
	m := metav1.NewTime(t)
	return &m
}

func controllerRef(kind, name string, uid types.UID) metav1.OwnerReference {
	return metav1.OwnerReference{APIVersion: apiApps, Kind: kind, Name: name, UID: uid, Controller: new(true)}
}

func deployment() *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: webName, Namespace: testNS, UID: "uid-web", Generation: 3, CreationTimestamp: longAgo},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To[int32](2),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{appName: webName}},
		},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration: 3, UpdatedReplicas: 2, ReadyReplicas: 1, AvailableReplicas: 1, UnavailableReplicas: 1,
			Conditions: []appsv1.DeploymentCondition{{
				Type: appsv1.DeploymentAvailable, Status: corev1.ConditionFalse, Reason: unavailable,
				Message: "Deployment does not have minimum availability.",
			}},
		},
	}
}

func replicaSet(name string, uid types.UID, owner *appsv1.Deployment, created metav1.Time) *appsv1.ReplicaSet {
	l := map[string]string{appName: webName, "pod-template-hash": name}
	return &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: testNS, UID: uid, Labels: l, CreationTimestamp: created,
			OwnerReferences: []metav1.OwnerReference{controllerRef(kindDeployment, owner.Name, owner.UID)},
		},
		Spec:   appsv1.ReplicaSetSpec{Replicas: ptr.To[int32](2), Selector: &metav1.LabelSelector{MatchLabels: l}},
		Status: appsv1.ReplicaSetStatus{ReadyReplicas: 1},
	}
}

// pod returns a running, ready pod with one container appName.
func pod(name string, l map[string]string, created metav1.Time, owners ...metav1.OwnerReference) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: testNS, UID: types.UID("uid-" + name), Labels: l, CreationTimestamp: created,
			OwnerReferences: owners,
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: appName, Ready: true,
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: created}},
			}},
		},
	}
}

// crashing makes the pod's container wait in CrashLoopBackOff after a terminated instance.
func crashing(p *corev1.Pod, message string) *corev1.Pod {
	p.Status.ContainerStatuses[0] = corev1.ContainerStatus{
		Name: appName, RestartCount: 4,
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: crashLoop, Message: message}},
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			Reason: "Error", ExitCode: 1, Message: message, StartedAt: recent, FinishedAt: recent,
		}},
	}
	return p
}

func incident(subject remediationv1alpha1.ObjectRef) *remediationv1alpha1.Incident {
	first := metav1.NewTime(testNow.Add(-30 * time.Minute))
	last := metav1.NewTime(testNow.Add(-time.Minute))
	return &remediationv1alpha1.Incident{
		ObjectMeta: metav1.ObjectMeta{Name: "incident-1", Namespace: "shadewright-system"},
		Spec: remediationv1alpha1.IncidentSpec{
			Fingerprint: "sha256:" + strings.Repeat("0", 64), Subject: subject, Reason: "BackOff", Type: "Warning",
			ReportingController: "kubelet", Symptom: "Back-off restarting failed container app",
		},
		Status: remediationv1alpha1.IncidentStatus{Phase: remediationv1alpha1.IncidentOpen, FirstSeen: &first, LastSeen: &last, Count: 3},
	}
}

func deploymentRef() remediationv1alpha1.ObjectRef {
	return remediationv1alpha1.ObjectRef{APIVersion: apiApps, Kind: kindDeployment, Namespace: testNS, Name: webName, UID: "uid-web"}
}

// webObjects is a Deployment with one ReplicaSet and two pods, one of them crashing, all created long ago.
func webObjects() []client.Object {
	d := deployment()
	rs := replicaSet("web-abc", "uid-rs-abc", d, longAgo)
	ref := controllerRef(kindReplicaSet, rs.Name, rs.UID)
	return []client.Object{
		d, rs,
		crashing(pod(crashPod, rs.Labels, longAgo, ref), "exited"),
		pod("web-abc-2", rs.Labels, longAgo, ref),
	}
}

func scheme(t testing.TB) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := appsv1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func reader(t testing.TB, objs ...client.Object) client.Reader {
	t.Helper()
	return clientfake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(objs...).Build()
}

// sources are fakes with the logs and metrics of the web fixture registered.
type sources struct {
	logs    *fake.Logs
	metrics *fake.Metrics
}

func webSources() sources {
	s := sources{logs: &fake.Logs{}, metrics: &fake.Metrics{}}
	s.logs.Set(fake.LogKey{Namespace: testNS, Pod: crashPod, Container: appName, Previous: true}, "starting\npanic: boom\n")
	s.logs.SetInstance(fake.LogKey{Namespace: testNS, Pod: crashPod, Container: appName, Previous: true},
		providers.LogInstance{PodUID: "uid-web-abc-1", RestartCount: 4}, false)
	for _, p := range metricPlans {
		s.metrics.Set(p.template, []providers.Series{{
			Labels:  map[string]string{"pod": crashPod, "container": appName},
			Samples: []providers.Sample{{Time: testNow, Value: 1}},
		}})
	}
	return s
}

func newBuilder(t testing.TB, r client.Reader, s sources, mutate func(*Config)) *Builder {
	t.Helper()
	red, err := redact.New()
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Pods: r, Objects: r, Namespaces: []string{testNS}, Redactor: red, Now: func() time.Time { return testNow }}
	if s.logs != nil {
		cfg.Logs = s.logs
	}
	if s.metrics != nil {
		cfg.Metrics = s.metrics
	}
	if mutate != nil {
		mutate(&cfg)
	}
	b, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func build(t testing.TB, b *Builder, inc *remediationv1alpha1.Incident) *Context {
	t.Helper()
	c, err := b.Build(context.Background(), inc)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func blocks(c *Context, s Section) []Block {
	var out []Block
	for _, b := range c.Blocks {
		if b.Section == s {
			out = append(out, b)
		}
	}
	return out
}

func one(t testing.TB, c *Context, s Section) Block {
	t.Helper()
	bs := blocks(c, s)
	if len(bs) != 1 {
		t.Fatalf("%d blocks of section %q, want 1: %+v", len(bs), s, c.Blocks)
	}
	return bs[0]
}

func hasLabel(b Block, l Label) bool { return slices.Contains(b.Labels, l) }

func sectionSize(c *Context, s Section) int {
	n := 0
	for _, b := range blocks(c, s) {
		n += len(b.Text)
	}
	return n
}
