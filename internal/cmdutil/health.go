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
	"time"

	toolscache "k8s.io/client-go/tools/cache"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
)

// Names of the checks [AddHealthChecks] registers, served at /healthz/<name> and /readyz/<name>.
const (
	LivenessCheck  = "healthz"
	CacheSyncCheck = "cache-sync"
)

// The wait between two failed attempts to get an informer: doubled after each failure, up to the maximum.
const (
	informerRetryMin = time.Second
	informerRetryMax = 30 * time.Second
)

// errNotSynced is the answer of the readiness check until the required informers have synced. It names no object.
var errNotSynced = errors.New("the informers of the required kinds have not synced yet")

// AddHealthChecks registers the probes of a manager. /healthz answers a ping, so liveness never depends on the
// API server. /readyz passes once the informers of required (the kinds the component cannot act without, each
// one it may list and watch) have synced, and then keeps passing: it reports that the component was able to
// start, not that the API server answers now. It serves the startup and the readiness probe.
//
// The informers are the ones of the manager's cache, so the controllers that start on the leader use them too.
// A runnable that does not need the lease gets them and waits for their first list, so a standby replica
// syncs the same informers and turns Ready like the leader. A failed attempt (the API server unreachable, the
// kind not served, its list forbidden) is retried until the manager stops: it keeps the check failing, never
// stops the component. The probe handler only reads a flag, so it never waits for the API server. Optional
// kinds (an add-on that may not be installed or granted) must not be in required.
func AddHealthChecks(mgr ctrl.Manager, required ...client.Object) error {
	if len(required) == 0 {
		return errors.New("adding the readiness check: no required kind")
	}
	r := &readiness{
		informers: mgr.GetCache(),
		required:  required,
		retryMin:  informerRetryMin,
		retryMax:  informerRetryMax,
	}
	if err := mgr.Add(r); err != nil {
		return fmt.Errorf("adding the readiness watcher: %w", err)
	}
	if err := mgr.AddHealthzCheck(LivenessCheck, healthz.Ping); err != nil {
		return fmt.Errorf("adding the liveness check: %w", err)
	}
	if err := mgr.AddReadyzCheck(CacheSyncCheck, r.check); err != nil {
		return fmt.Errorf("adding the readiness check: %w", err)
	}
	return nil
}

// informerGetter is the part of the manager's cache the watcher uses.
type informerGetter interface {
	GetInformer(ctx context.Context, obj client.Object, opts ...cache.InformerGetOption) (cache.Informer, error)
}

// readiness gets the informers of the required kinds, waits once for their first list and records it. It is a
// manager runnable that does not need leader election, so a standby replica runs it too.
type readiness struct {
	informers informerGetter
	required  []client.Object
	retryMin  time.Duration
	retryMax  time.Duration
	synced    atomic.Bool
}

// NeedLeaderElection is false: a standby replica that has synced is ready to act and must report it.
func (r *readiness) NeedLeaderElection() bool { return false }

// Start gets each required informer, retrying a failed attempt, then waits for all of them to sync and sets the
// flag. It returns nil in every case: a shutdown before the sync leaves the flag unset, and nothing it meets is
// final, so it never stops the manager.
func (r *readiness) Start(ctx context.Context) error {
	log := ctrl.LoggerFrom(ctx).WithName("readiness")
	synced := make([]toolscache.InformerSynced, 0, len(r.required))
	for _, obj := range r.required {
		inf, ok := r.informer(ctx, obj, log.Error)
		if !ok {
			return nil
		}
		synced = append(synced, inf.HasSynced)
	}
	if !toolscache.WaitForCacheSync(ctx.Done(), synced...) {
		return nil
	}
	r.synced.Store(true)
	log.Info("Synced the informers of the required kinds", "kinds", len(r.required))
	return nil
}

// informer gets the informer of obj's kind without waiting for its sync, retrying after a growing wait until
// it has one or ctx ends (ok false).
func (r *readiness) informer(ctx context.Context, obj client.Object,
	logError func(error, string, ...any)) (cache.Informer, bool) {
	wait := r.retryMin
	for {
		inf, err := r.informers.GetInformer(ctx, obj, cache.BlockUntilSynced(false))
		if err == nil {
			return inf, true
		}
		if ctx.Err() != nil {
			return nil, false
		}
		logError(err, "Failed to get the informer of a required kind, retrying", "type", fmt.Sprintf("%T", obj),
			"retryAfter", wait.String())
		select {
		case <-ctx.Done():
			return nil, false
		case <-time.After(wait):
		}
		wait = min(2*wait, r.retryMax)
	}
}

// check is the readiness check: it reads the flag and never blocks.
func (r *readiness) check(*http.Request) error {
	if r.synced.Load() {
		return nil
	}
	return errNotSynced
}
