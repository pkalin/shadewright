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

package collector

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/analyzer"
)

const (
	watchedNS   = "watched"
	unwatchedNS = "unwatched"
	operatorNS  = "shadewright-system"
)

// The envtest environment is started once, on first use, by the tests that need an API server.
var env struct {
	once    sync.Once
	err     error
	testEnv *envtest.Environment
	admin   client.Client
	col     *Collector
	cache   *recordingCache
	cancel  context.CancelFunc
}

func TestMain(m *testing.M) {
	code := m.Run()
	if env.cancel != nil {
		env.cancel()
	}
	if env.testEnv != nil {
		_ = env.testEnv.Stop()
	}
	os.Exit(code)
}

// recordingCache records the kind of every informer the collector asks for.
type recordingCache struct {
	cache.Cache
	scheme *runtime.Scheme
	mu     sync.Mutex
	kinds  []string
}

func (r *recordingCache) GetInformer(ctx context.Context, obj client.Object, opts ...cache.InformerGetOption) (cache.Informer, error) {
	if gvk, err := apiutil.GVKForObject(obj, r.scheme); err == nil {
		r.mu.Lock()
		r.kinds = append(r.kinds, gvk.GroupVersion().String()+"/"+gvk.Kind)
		r.mu.Unlock()
	}
	return r.Cache.GetInformer(ctx, obj, opts...)
}

func (r *recordingCache) informers() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := slices.Clone(r.kinds)
	slices.Sort(out)
	return slices.Compact(out)
}

func firstEnvtestBinaryDir() string {
	base := filepath.Join("..", "..", "..", "bin", "k8s")
	entries, err := os.ReadDir(base)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if e.IsDir() {
			return filepath.Join(base, e.Name())
		}
	}
	return ""
}

// startEnv starts an API server and a manager with the analyzer's cache options and a collector, watching
// only watchedNS.
func startEnv(t *testing.T) {
	t.Helper()
	env.once.Do(func() { env.err = bootEnv() })
	if env.err != nil {
		t.Fatalf("starting envtest: %v", env.err)
	}
}

func bootEnv() error {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return err
	}
	if err := remediationv1alpha1.AddToScheme(scheme); err != nil {
		return err
	}
	env.testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
		BinaryAssetsDirectory: firstEnvtestBinaryDir(),
	}
	cfg, err := env.testEnv.Start()
	if err != nil {
		return err
	}
	if env.admin, err = client.New(cfg, client.Options{Scheme: scheme}); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	env.cancel = cancel
	for _, ns := range []string{watchedNS, unwatchedNS, operatorNS} {
		if err := env.admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
			return err
		}
	}
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		Cache:                  analyzer.CacheOptions(operatorNS, []string{watchedNS}),
		NewCache: func(c *rest.Config, opts cache.Options) (cache.Cache, error) {
			inner, err := cache.New(c, opts)
			if err != nil {
				return nil, err
			}
			env.cache = &recordingCache{Cache: inner, scheme: scheme}
			return env.cache, nil
		},
	})
	if err != nil {
		return err
	}
	if env.col, err = New(Config{ClusterID: testCluster, Buffer: 64}); err != nil {
		return err
	}
	if err := env.col.SetupWithManager(mgr); err != nil {
		return err
	}
	go func() { _ = mgr.Start(ctx) }()
	syncCtx, done := context.WithTimeout(ctx, 30*time.Second)
	defer done()
	if !mgr.GetCache().WaitForCacheSync(syncCtx) {
		return context.DeadlineExceeded
	}
	return nil
}

// await returns the first observation matching match, discarding others, or fails after the timeout.
func await(t *testing.T, match func(Observation) bool) Observation {
	t.Helper()
	deadline := time.After(20 * time.Second)
	for {
		select {
		case o := <-env.col.Observations():
			if match(o) {
				return o
			}
		case <-deadline:
			t.Fatal("no matching observation")
			return Observation{}
		}
	}
}

// never fails if an observation matching match arrives within d.
func never(t *testing.T, d time.Duration, match func(Observation) bool) {
	t.Helper()
	deadline := time.After(d)
	for {
		select {
		case o := <-env.col.Observations():
			if match(o) {
				t.Fatalf("unexpected observation: %+v", o)
			}
		case <-deadline:
			return
		}
	}
}

func about(name string) func(Observation) bool {
	return func(o Observation) bool { return o.Subject.Name == name }
}

// envEvent is a Warning event the API server accepts, about a pod that does not exist.
func envEvent(ns, name string, series int32) *eventsv1.Event {
	now := metav1.NewMicroTime(time.Now())
	ev := &eventsv1.Event{
		ObjectMeta:          metav1.ObjectMeta{Name: name, Namespace: ns},
		EventTime:           now,
		Reason:              reasonBack,
		Action:              "Restarting",
		Type:                corev1.EventTypeWarning,
		ReportingController: "kubelet",
		ReportingInstance:   "node-1",
		Note:                "Back-off restarting failed container",
		Regarding: corev1.ObjectReference{
			APIVersion: apiCore, Kind: kindPod, Namespace: ns, Name: name + "-pod",
			UID: "0a1b2c3d-4e5f-4a6b-8c7d-" + "9e0f1a2b3c4d",
		},
	}
	if series > 0 {
		ev.Series = &eventsv1.EventSeries{Count: series, LastObservedTime: now}
	}
	return ev
}

func TestEnvtestSeriesIncreaseIsCountedOnce(t *testing.T) {
	startEnv(t)
	ctx := t.Context()
	ev := envEvent(watchedNS, "series", 3)
	if err := env.admin.Create(ctx, ev); err != nil {
		t.Fatal(err)
	}
	match := about("series-pod")
	if got := await(t, match); got.Count != 3 {
		t.Fatalf("new series of 3: Count = %d, want 3", got.Count)
	}

	ev.Series.Count = 7
	ev.Series.LastObservedTime = metav1.NewMicroTime(time.Now())
	if err := env.admin.Update(ctx, ev); err != nil {
		t.Fatal(err)
	}
	if got := await(t, match); got.Count != 4 {
		t.Fatalf("series 3 to 7: Count = %d, want 4", got.Count)
	}

	// Replaying the same event (an update that does not change the count) emits nothing.
	ev.Labels = map[string]string{"replay": "yes"}
	if err := env.admin.Update(ctx, ev); err != nil {
		t.Fatal(err)
	}
	never(t, 3*time.Second, match)
}

func TestEnvtestOOMKilledWithoutAnEvent(t *testing.T) {
	startEnv(t)
	ctx := t.Context()
	labels := map[string]string{containerApp: "oom"}
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "oom", Namespace: watchedNS},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: containerApp, Image: "example.invalid/app:1"}}},
			},
		},
	}
	if err := env.admin.Create(ctx, d); err != nil {
		t.Fatal(err)
	}
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "oom-7d4b9c8f6", Namespace: watchedNS,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: apiApps, Kind: kindDeployment, Name: d.Name, UID: d.UID, Controller: new(true),
			}},
		},
		Spec: appsv1.ReplicaSetSpec{Selector: d.Spec.Selector, Template: d.Spec.Template},
	}
	if err := env.admin.Create(ctx, rs); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "oom-7d4b9c8f6-q8wzt", Namespace: watchedNS, Labels: labels,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: apiApps, Kind: kindReplicaSet, Name: rs.Name, UID: rs.UID, Controller: new(true),
			}},
		},
		Spec: d.Spec.Template.Spec,
	}
	if err := env.admin.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: containerApp, Image: "example.invalid/app:1", ImageID: "", RestartCount: 1,
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()}},
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ContainerID: "containerd://oom-1", Reason: ReasonOOMKilled, ExitCode: 137, FinishedAt: metav1.Now(),
		}},
	}}
	if err := env.admin.Status().Update(ctx, pod); err != nil {
		t.Fatal(err)
	}
	match := func(o Observation) bool { return o.Reason == ReasonOOMKilled && o.Subject.Name == d.Name }
	got := await(t, match)
	if got.Subject.Kind != kindDeployment || got.Subject.UID != remediationv1alpha1.UID(d.UID) || got.Count != 1 {
		t.Fatalf("observation = %+v, want one OOMKilled for Deployment %s", got, d.Name)
	}

	// A later status update that shows the same termination emits nothing.
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	if err := env.admin.Status().Update(ctx, pod); err != nil {
		t.Fatal(err)
	}
	never(t, 3*time.Second, match)
}

func TestEnvtestOnlyWatchedNamespacesAndNoCoreEvents(t *testing.T) {
	startEnv(t)
	ctx := t.Context()
	if err := env.admin.Create(ctx, envEvent(unwatchedNS, "elsewhere", 0)); err != nil {
		t.Fatal(err)
	}
	// A core/v1 Event is the same object as its events.k8s.io/v1 form: it is seen once, through the one
	// events.k8s.io watch, with the count its recorder kept.
	core := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "legacy", Namespace: watchedNS},
		InvolvedObject: corev1.ObjectReference{APIVersion: apiCore, Kind: kindPod, Namespace: watchedNS, Name: "legacy-pod", UID: "1a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d"},
		Reason:         reasonBack,
		Message:        "Back-off restarting failed container",
		Type:           corev1.EventTypeWarning,
		Count:          5,
		Source:         corev1.EventSource{Component: "kubelet"},
		FirstTimestamp: metav1.Now(),
		LastTimestamp:  metav1.Now(),
	}
	if err := env.admin.Create(ctx, core); err != nil {
		t.Fatal(err)
	}
	if got := await(t, about("legacy-pod")); got.Count != 5 {
		t.Fatalf("core/v1 event with count 5: Count = %d, want 5", got.Count)
	}
	never(t, 3*time.Second, func(o Observation) bool {
		return o.Subject.Name == "elsewhere-pod" || o.Subject.Name == "legacy-pod"
	})

	want := []string{"events.k8s.io/v1/Event", "v1/Pod"}
	if got := env.cache.informers(); !slices.Equal(got, want) {
		t.Fatalf("informers = %v, want exactly %v (no core/v1 Events, no owner informers)", got, want)
	}
}
