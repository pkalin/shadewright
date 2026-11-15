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
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// fakeInformer reports synced once the channel is closed. Only HasSynced is used.
type fakeInformer struct {
	cache.Informer
	synced chan struct{}
}

func (f fakeInformer) HasSynced() bool {
	select {
	case <-f.synced:
		return true
	default:
		return false
	}
}

// fakeInformers fails the first failures calls of GetInformer, then hands out inf.
type fakeInformers struct {
	failures int32
	calls    atomic.Int32
	inf      fakeInformer
}

func (f *fakeInformers) GetInformer(context.Context, client.Object, ...cache.InformerGetOption) (cache.Informer, error) {
	if f.calls.Add(1) <= f.failures {
		return nil, errors.New("connection refused")
	}
	return f.inf, nil
}

func newReadiness(f *fakeInformers) *readiness {
	return &readiness{informers: f, required: []client.Object{&corev1.ConfigMap{}, &corev1.Pod{}},
		retryMin: time.Millisecond, retryMax: 2 * time.Millisecond}
}

func TestReadinessFailsBeforeSyncAndLatchesAfter(t *testing.T) {
	f := &fakeInformers{failures: 3, inf: fakeInformer{synced: make(chan struct{})}}
	r := newReadiness(f)
	if err := r.check(nil); err == nil {
		t.Fatal("check passed before the watcher started")
	}
	done := make(chan error, 1)
	go func() { done <- r.Start(t.Context()) }()
	// The failed attempts are retried, not final: the watcher gets the informers and waits for their sync.
	for f.calls.Load() < f.failures+2 {
		time.Sleep(time.Millisecond)
	}
	if err := r.check(nil); !errors.Is(err, errNotSynced) {
		t.Fatalf("check while waiting for the sync = %v, want %v", err, errNotSynced)
	}
	close(f.inf.synced)
	if err := <-done; err != nil {
		t.Fatalf("Start after the sync = %v", err)
	}
	// Latched: the check passes from now on, whatever the informers report later.
	for range 3 {
		if err := r.check(nil); err != nil {
			t.Fatalf("check after the sync = %v", err)
		}
	}
}

func TestReadinessShutdownWhileRetryingIsNoError(t *testing.T) {
	f := &fakeInformers{failures: 1 << 30}
	r := newReadiness(f)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- r.Start(ctx) }()
	for f.calls.Load() < 3 {
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Start ended by a shutdown = %v, want nil", err)
	}
	if err := r.check(nil); err == nil {
		t.Fatal("check passed though no informer was had")
	}
}

func TestReadinessShutdownBeforeSyncIsNoError(t *testing.T) {
	r := newReadiness(&fakeInformers{inf: fakeInformer{synced: make(chan struct{})}})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := r.Start(ctx); err != nil {
		t.Fatalf("Start ended by a shutdown = %v, want nil", err)
	}
	if err := r.check(nil); err == nil {
		t.Fatal("check passed though the informers never synced")
	}
}

func TestReadinessRunsWithoutTheLease(t *testing.T) {
	if (&readiness{}).NeedLeaderElection() {
		t.Fatal("the readiness watcher needs the lease: a standby replica would never be ready")
	}
}
