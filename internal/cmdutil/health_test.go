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

package cmdutil

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

// gatedCache syncs when release is closed, or reports false when ctx ends first, as the informer cache does.
type gatedCache struct{ release chan struct{} }

func (g gatedCache) WaitForCacheSync(ctx context.Context) bool {
	select {
	case <-g.release:
		return true
	case <-ctx.Done():
		return false
	}
}

// failedCache reports a sync that did not happen while ctx is still live.
type failedCache struct{}

func (failedCache) WaitForCacheSync(context.Context) bool { return false }

func TestCacheSyncCheckFailsBeforeSyncAndPassesAfter(t *testing.T) {
	g := gatedCache{release: make(chan struct{})}
	s := &cacheSync{cache: g}
	if err := s.check(nil); err == nil {
		t.Fatal("check passed before the watcher started")
	}
	done := make(chan error, 1)
	go func() { done <- s.Start(t.Context()) }()
	// The watcher is waiting: the check answers at once, and fails.
	if err := s.check(nil); !errors.Is(err, errNotSynced) {
		t.Fatalf("check while waiting for the sync = %v, want %v", err, errNotSynced)
	}
	close(g.release)
	if err := <-done; err != nil {
		t.Fatalf("Start after the sync = %v", err)
	}
	for range 3 {
		if err := s.check(nil); err != nil {
			t.Fatalf("check after the sync = %v", err)
		}
	}
}

func TestCacheSyncShutdownBeforeSyncIsNoError(t *testing.T) {
	s := &cacheSync{cache: gatedCache{release: make(chan struct{})}}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := s.Start(ctx); err != nil {
		t.Fatalf("Start ended by a shutdown = %v, want nil", err)
	}
	if err := s.check(nil); err == nil {
		t.Fatal("check passed though the caches never synced")
	}
}

func TestCacheSyncThatFailsStopsTheManager(t *testing.T) {
	s := &cacheSync{cache: failedCache{}}
	if err := s.Start(t.Context()); err == nil {
		t.Fatal("Start returned nil for a sync that failed with a live context")
	}
	if err := s.check(nil); err == nil {
		t.Fatal("check passed though the caches never synced")
	}
}

func TestCacheSyncRunsWithoutTheLease(t *testing.T) {
	if (&cacheSync{}).NeedLeaderElection() {
		t.Fatal("the cache sync watcher needs the lease: a standby replica would never be ready")
	}
}

// envtestBinaryDir is the directory make setup-envtest fills, for a run without KUBEBUILDER_ASSETS.
func envtestBinaryDir() string {
	base := filepath.Join("..", "..", "bin", "k8s")
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

// freeAddr returns a loopback address with a port that was free a moment ago.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

// TestReadyWithoutTheLease starts a manager with leader election against a Lease another replica holds for an
// hour, so it never becomes leader, and checks that /readyz passes once its caches have synced while it stays
// a standby: controller-runtime syncs the caches before it runs leader election.
func TestReadyWithoutTheLease(t *testing.T) {
	testEnv := &envtest.Environment{BinaryAssetsDirectory: envtestBinaryDir()}
	cfg, err := testEnv.Start()
	if err != nil {
		t.Fatalf("starting envtest: %v", err)
	}
	t.Cleanup(func() { _ = testEnv.Stop() })

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	admin, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	const ns, leaseName = "default", "standby-test"
	now := metav1.NewMicroTime(time.Now())
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: leaseName},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity:       new("another-replica"),
			LeaseDurationSeconds: new(int32(3600)),
			AcquireTime:          &now,
			RenewTime:            &now,
		},
	}
	if err := admin.Create(t.Context(), lease); err != nil {
		t.Fatal(err)
	}

	addr := freeAddr(t)
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                  scheme,
		Metrics:                 metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress:  addr,
		LeaderElection:          true,
		LeaderElectionID:        leaseName,
		LeaderElectionNamespace: ns,
	})
	if err != nil {
		t.Fatal(err)
	}
	// An informer the cache must sync before the check passes.
	if _, err := mgr.GetCache().GetInformer(t.Context(), &corev1.ConfigMap{}); err != nil {
		t.Fatal(err)
	}
	ranAsLeader := make(chan struct{})
	if err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		close(ranAsLeader)
		<-ctx.Done()
		return nil
	})); err != nil {
		t.Fatal(err)
	}
	if err := AddHealthChecks(mgr); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	stopped := make(chan error, 1)
	go func() { stopped <- mgr.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-stopped; err != nil {
			t.Errorf("manager: %v", err)
		}
	})

	get := func(path string) (int, string) {
		resp, err := http.Get("http://" + addr + path) //nolint:noctx // a test against a local listener
		if err != nil {
			return 0, err.Error()
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		if code, _ := get("/readyz"); code == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			code, body := get("/readyz?verbose")
			t.Fatalf("/readyz did not pass on a standby: %d %s", code, body)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if code, body := get("/readyz/" + CacheSyncCheck); code != http.StatusOK {
		t.Errorf("/readyz/%s = %d %s", CacheSyncCheck, code, body)
	}
	if code, body := get("/healthz/" + LivenessCheck); code != http.StatusOK {
		t.Errorf("/healthz/%s = %d %s", LivenessCheck, code, body)
	}
	select {
	case <-mgr.Elected():
		t.Fatal("the manager became leader: the test proves nothing about a standby")
	case <-ranAsLeader:
		t.Fatal("a leader-only runnable ran: the test proves nothing about a standby")
	default:
	}
}
