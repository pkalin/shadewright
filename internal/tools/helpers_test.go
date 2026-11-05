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

package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/providers/fake"
	"github.com/pkalin/shadewright/internal/redact"
)

const (
	testNS    = "shop"
	otherNS   = "kube-system"
	podName   = "web-abc-1"
	container = "app"
	webName   = "web"

	secretKind       = "Secret"
	kustomization    = "Kustomization"
	restartsTemplate = "container_restarts_total"
	memoryTemplate   = "container_memory_working_set_bytes"
	manager          = "kubectl"
	unitName         = "apps"
)

// Argument names of the tools.
const (
	argNamespace     = "namespace"
	argKind          = "kind"
	argName          = "name"
	argLimit         = "limit"
	argContainer     = "container"
	argPath          = "path"
	argPod           = "pod"
	argDeployment    = "deployment"
	argTemplate      = "template"
	argPrevious      = "previous"
	argSinceMinutes  = "since_minutes"
	argMaxBytes      = "max_bytes"
	argWindowMinutes = "window_minutes"
	argRef           = "ref"
)

var testNow = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

// Synthetic credentials that the built-in redaction rules remove. They are built, not written as literals.
var (
	leakJWT = b64(`{"alg":"HS256","typ":"JWT"}`) + "." + b64(`{"sub":"synthetic-subject"}`) + "." +
		b64("synthetic-signature-0123456789")
	leakPassword = "Zq8-synthetic-" + "Pw0rd"
	leakDSN      = "postgres://app:" + leakPassword + "@db.shop.svc:5432/orders"
)

func b64(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

// countingReader counts every read, so a test can show that a refusal made none.
type countingReader struct {
	client.Reader
	n atomic.Int32
}

func (c *countingReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	c.n.Add(1)
	return c.Reader.Get(ctx, key, obj, opts...)
}

func (c *countingReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	c.n.Add(1)
	return c.Reader.List(ctx, list, opts...)
}

// blockingReader blocks every read until its context is done.
type blockingReader struct{ client.Reader }

func (blockingReader) Get(ctx context.Context, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
	<-ctx.Done()
	return ctx.Err()
}

func (blockingReader) List(ctx context.Context, _ client.ObjectList, _ ...client.ListOption) error {
	<-ctx.Done()
	return ctx.Err()
}

// blockingLogs blocks until its context is done.
type blockingLogs struct{}

func (blockingLogs) Logs(ctx context.Context, _ providers.LogQuery) (providers.LogExcerpt, error) {
	<-ctx.Done()
	return providers.LogExcerpt{}, ctx.Err()
}

// env is a tool set over fakes that count every call.
type env struct {
	set                   *Set
	pods, events, objects *countingReader
	logs                  *fake.Logs
	metrics               *fake.Metrics
	delivery              *fake.Delivery
	sourceCalls           atomic.Int32
}

// reads is the number of reads made of every reader and source.
func (e *env) reads() int32 {
	return e.pods.n.Load() + e.events.n.Load() + e.objects.n.Load() + e.sourceCalls.Load()
}

func scheme(t testing.TB) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func newEnv(t testing.TB, objs []client.Object, mutate ...func(*Config)) *env {
	t.Helper()
	c := clientfake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(objs...).Build()
	e := &env{
		pods: &countingReader{Reader: c}, events: &countingReader{Reader: c}, objects: &countingReader{Reader: c},
		logs: &fake.Logs{}, metrics: &fake.Metrics{}, delivery: &fake.Delivery{},
	}
	count := func(string) error { e.sourceCalls.Add(1); return nil }
	e.logs.SetFail(count)
	e.metrics.SetFail(count)
	e.delivery.SetFail(count)
	red, err := redact.New()
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		Pods: e.pods, Events: e.events, Objects: e.objects, Logs: e.logs, Metrics: e.metrics, Delivery: e.delivery,
		Redactor: red, Namespaces: []string{testNS}, Now: func() time.Time { return testNow },
	}
	for _, m := range mutate {
		m(&cfg)
	}
	set, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	e.set = set
	return e
}

func (e *env) call(t testing.TB, name, args string) Result {
	t.Helper()
	r, err := e.set.Call(context.Background(), providers.ToolCall{ID: "c1", Name: name, Arguments: json.RawMessage(args)})
	if err != nil {
		t.Fatalf("%s %s: %v", name, args, err)
	}
	return r
}

func secretRef(name string) *corev1.EnvVarSource {
	return &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: name}, Key: "password",
	}}
}

func pod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: podName, Namespace: testNS, UID: "uid-pod", Labels: map[string]string{container: webName},
			Annotations: map[string]string{"note": "an annotation " + leakPassword},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: container, Image: "registry.example/web:1",
			Args: []string{"--db-password=" + leakPassword, "--token", leakJWT, "--dsn=" + leakDSN},
			Env: []corev1.EnvVar{
				{Name: "PLAIN", Value: "literal-value-" + leakPassword},
				{Name: "DB_PASSWORD", ValueFrom: secretRef("db-creds")},
			},
		}}},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: container,
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
					Reason: "CreateContainerConfigError", Message: `secret "db-creds" not found`,
				}},
			}},
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse, Message: "dial " + leakDSN}},
		},
	}
}

func deployment() *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: webName, Namespace: testNS, UID: "uid-web"},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To[int32](2),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{container: webName}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{container: webName}, Annotations: map[string]string{"a": "b"}},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name: container, Image: "registry.example/web:1",
					Command: []string{"/app", "--api-key=" + leakPassword},
					Env:     []corev1.EnvVar{{Name: "TOKEN", Value: leakJWT}},
				}}},
			},
		},
		Status: appsv1.DeploymentStatus{Conditions: []appsv1.DeploymentCondition{{
			Type: appsv1.DeploymentAvailable, Status: corev1.ConditionFalse, Message: "Authorization: Bearer " + leakJWT,
		}}},
	}
}

func event(name, objName, note string, at time.Time) *eventsv1.Event {
	return &eventsv1.Event{
		ObjectMeta:          metav1.ObjectMeta{Name: name, Namespace: testNS},
		EventTime:           metav1.NewMicroTime(at),
		ReportingController: "kubelet", ReportingInstance: "node-1",
		Action: "Sync", Reason: "BackOff", Type: corev1.EventTypeWarning,
		Regarding: corev1.ObjectReference{Kind: kindPod, Namespace: testNS, Name: objName},
		Note:      note,
	}
}
