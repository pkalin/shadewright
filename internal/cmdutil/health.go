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
	"fmt"
	"net/http"
	"sync/atomic"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
)

// Names of the checks [AddHealthChecks] registers, served at /healthz/<name> and /readyz/<name>.
const (
	LivenessCheck  = "healthz"
	CacheSyncCheck = "cache-sync"
)

// errNotSynced is the answer of the readiness check until the caches have synced. It names no object.
var errNotSynced = errors.New("the manager's caches have not synced yet")

// AddHealthChecks registers the probes of a manager. /healthz answers a ping, so liveness never depends on the
// API server. /readyz passes once the manager's caches have synced and keeps passing; it serves the startup and
// the readiness probe. The check holds on every replica, leader or not: controller-runtime starts and syncs the
// caches before it runs leader election, and the goroutine that waits for them is a runnable that does not need
// the lease. The probe handler only reads a flag, so it never waits for the API server.
func AddHealthChecks(mgr ctrl.Manager) error {
	s := &cacheSync{cache: mgr.GetCache()}
	if err := mgr.Add(s); err != nil {
		return fmt.Errorf("adding the cache sync watcher: %w", err)
	}
	if err := mgr.AddHealthzCheck(LivenessCheck, healthz.Ping); err != nil {
		return fmt.Errorf("adding the liveness check: %w", err)
	}
	if err := mgr.AddReadyzCheck(CacheSyncCheck, s.check); err != nil {
		return fmt.Errorf("adding the readiness check: %w", err)
	}
	return nil
}

// syncWaiter is the part of the manager's cache the watcher uses.
type syncWaiter interface {
	WaitForCacheSync(ctx context.Context) bool
}

// cacheSync waits once for the manager's caches to sync and records it. It is a manager runnable that does not
// need leader election, so a standby replica runs it too.
type cacheSync struct {
	cache  syncWaiter
	synced atomic.Bool
}

// NeedLeaderElection is false: a standby replica that has synced is ready to act and must report it.
func (s *cacheSync) NeedLeaderElection() bool { return false }

// Start waits for the caches to sync and sets the flag. A wait that ends with ctx (a shutdown) is no error;
// one that ends otherwise is, so the manager stops and the kubelet restarts the container.
func (s *cacheSync) Start(ctx context.Context) error {
	if s.cache.WaitForCacheSync(ctx) {
		s.synced.Store(true)
		return nil
	}
	if ctx.Err() != nil {
		return nil
	}
	return errors.New("the manager's caches did not sync")
}

// check is the readiness check: it reads the flag and never blocks.
func (s *cacheSync) check(*http.Request) error {
	if s.synced.Load() {
		return nil
	}
	return errNotSynced
}
