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
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	toolscache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

// WatchState is the state of the watch on one kind of an add-on whose module declares a symptom source for it
// (#409). The reads of those kinds are granted only when the chart's value of the module is on
// (analyzer.modules), so the collector does not assume them: it starts the watch only once the kind is served
// and the analyzer may list it, and reports every other state.
type WatchState string

// The states of [WatchState].
const (
	// WatchInactive: no active module declares a source for the kind; nothing is read.
	WatchInactive WatchState = "Inactive"
	// WatchWatched: the watch runs and its cache has synced.
	WatchWatched WatchState = "Watched"
	// WatchNotServed: the API server does not serve the kind (the add-on's CRD is not installed).
	WatchNotServed WatchState = "NotServed"
	// WatchNotGranted: the analyzer may not list the kind in every watched namespace (or cluster-wide, for a
	// cluster-scoped kind), or the chart did not render the kind's grant ([Config.Grants]): the chart's value
	// of the module is off.
	WatchNotGranted WatchState = "NotGranted"
	// WatchFailed: the check failed otherwise, or a watch failed: it could not sync, it stopped on its own, the
	// API server refused its list or watch while the check passed, or it kept reporting errors. A watch that
	// failed is started again only after a backoff.
	WatchFailed WatchState = "Failed"
	// WatchOmitted: the chart left out the kind's optional grant on purpose ([Config.Grants] without
	// kyverno-cluster, the documented least-privilege setup). The kind is not checked, not watched and not
	// reported as missing, and the gauge has no series for it.
	WatchOmitted WatchState = "Omitted"
)

// The module grants, as --module-grants names them: one per ClusterRole of package modulerbac that the chart
// renders from a value under analyzer.modules.
const (
	GrantKEDA           = "keda"
	GrantFlux           = "flux"
	GrantKyverno        = "kyverno"
	GrantKyvernoCluster = "kyverno-cluster"
)

// ModuleGrants are the grant names [Config.Grants] accepts.
var ModuleGrants = []string{GrantKEDA, GrantFlux, GrantKyverno, GrantKyvernoCluster}

// grantOf returns the grant that holds the reads of a kind of [SourceKinds] that is not built in, and whether
// it is optional: a kind whose optional grant the chart left out is [WatchOmitted], not [WatchNotGranted].
func grantOf(gvk schema.GroupVersionKind) (name string, optional bool) {
	switch gvk.Group + "/" + gvk.Kind {
	case "keda.sh/ScaledObject", "keda.sh/ScaledJob":
		return GrantKEDA, false
	case "kustomize.toolkit.fluxcd.io/Kustomization", "helm.toolkit.fluxcd.io/HelmRelease",
		"source.toolkit.fluxcd.io/GitRepository":
		return GrantFlux, false
	case "kyverno.io/Policy", "wgpolicyk8s.io/PolicyReport":
		return GrantKyverno, false
	case "kyverno.io/ClusterPolicy", "wgpolicyk8s.io/ClusterPolicyReport":
		return GrantKyvernoCluster, true
	}
	return "", false
}

// Bounds of a lazy watch.
const (
	// failedAfterChecks is how many checks in a row must see an error of a running watch (one the reflector
	// retries: the API server unavailable, a timeout) before the kind is reported as failed. The watch keeps
	// running, and the next check without an error reports it as watched again.
	failedAfterChecks = 3
	// maxWatchBackoff bounds the wait before a watch that failed is started again: twice SelectionPoll after
	// the first failure, then twice as long after each next one.
	maxWatchBackoff = 30 * time.Minute
)

// SourceWatch is the watch state of one kind of [SourceKinds] that is not built in.
type SourceWatch struct {
	Kind  schema.GroupVersionKind
	State WatchState
}

// builtinKind reports whether every cluster serves the kind and the watch role grants it, so its watch is set
// up with the manager: the HPA and the Pod.
func builtinKind(gvk schema.GroupVersionKind) bool { return gvk == sourceHPA || gvk == sourcePod }

// SourcesNotWatched returns the kinds that an active module declares a source for but that are not watched,
// with why, in the order of [SourceKinds], and complete: false while such a kind has no result yet (its first
// check has not run since its module became active, or its first watch has not synced). It is safe for
// concurrent use. Detection reports them in the condition SymptomSourcesNotWatched of RemediationSettings
// status, Unknown while the report is not complete. A kind left out on purpose ([WatchOmitted]) is neither.
func (c *Collector) SourcesNotWatched() (kinds []SourceWatch, complete bool) {
	if c.cfg.Modules == nil {
		return nil, true
	}
	sel := c.cfg.Modules()
	c.watchMu.Lock()
	defer c.watchMu.Unlock()
	complete = true
	for _, gvk := range SourceKinds {
		if builtinKind(gvk) {
			continue
		}
		if _, _, active := sourceIn(sel, gvk); !active {
			continue
		}
		switch s, ok := c.watches[gvk]; {
		case !ok || s == WatchInactive:
			complete = false
		case s != WatchWatched && s != WatchOmitted:
			kinds = append(kinds, SourceWatch{Kind: gvk, State: s})
		}
	}
	return kinds, complete
}

// setWatch records the state of a kind's watch: in the gauge, and, when it changed, in the log and by calling
// [Config.WatchChanged]. A cause is logged with a change to WatchFailed; with none, the caller logged it.
func (c *Collector) setWatch(log logr.Logger, module string, gvk schema.GroupVersionKind, s WatchState, cause error) {
	setWatchedGauge(gvk, s)
	c.watchMu.Lock()
	prev, known := c.watches[gvk]
	c.watches[gvk] = s
	c.watchMu.Unlock()
	if known && prev == s || !known && s == WatchInactive {
		return
	}
	kv := append(kindKV(gvk, module), "state", string(s))
	switch s {
	case WatchWatched:
		log.Info("Started watching a symptom source kind", kv...)
	case WatchInactive:
		if prev == WatchWatched {
			log.Info("Stopped watching a symptom source kind, no active module declares it", kv...)
		} else {
			log.Info("No longer checking a symptom source kind, no active module declares it", kv...)
		}
	case WatchNotServed:
		log.Info("Not watching a symptom source kind of an active module, the API server does not serve it", kv...)
	case WatchNotGranted:
		log.Info("Not watching a symptom source kind of an active module, the analyzer may not list it: "+
			"turn on the module's value under analyzer.modules in the chart", kv...)
	case WatchOmitted:
		log.Info("Not watching a symptom source kind of an active module, the chart leaves its optional grant "+
			"out (analyzer.modules)", kv...)
	default:
		if cause != nil {
			log.Error(cause, "Not watching a symptom source kind of an active module, the check failed", kv...)
		}
	}
	if c.cfg.WatchChanged != nil {
		c.cfg.WatchChanged()
	}
}

// kindKV are the log keys and values naming a kind and its module.
func kindKV(gvk schema.GroupVersionKind, module string) []any {
	return []any{"group", gvk.Group, "version", gvk.Version, "kind", gvk.Kind, "module", module}
}

// watchStarter starts the watch of a kind. done receives the controller's result when it stops (nil when ctx
// ended, else why it stopped on its own, such as a cache that did not sync), synced is closed once the watch's
// cache has synced, and errs receives the errors of the watch's reflector. It is replaced in tests.
type watchStarter func(ctx context.Context, r *sourceReconciler, errs *watchErrors) (done <-chan error,
	synced <-chan struct{})

// watchErrors collects the errors of a running watch's reflector, which retries them itself: the lazySource
// reads them at each check. A refusal (Forbidden, Unauthorized) or a kind no longer served stops the watch at
// once; another error is reported once it lasts failedAfterChecks checks. It is safe for concurrent use.
type watchErrors struct {
	mu        sync.Mutex
	fatal     error
	transient error
	wake      chan<- struct{} // a fatal error wakes the lazySource, so it does not wait for the next poll
}

// report records an error of the watch's reflector.
func (w *watchErrors) report(err error) {
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) ||
		apierrors.IsResourceExpired(err) || apierrors.IsGone(err) {
		// A watch that ends or expires is listed again by the reflector: part of a watch's life.
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if !apierrors.IsForbidden(err) && !apierrors.IsUnauthorized(err) && !apierrors.IsNotFound(err) &&
		!meta.IsNoMatchError(err) {
		w.transient = err
		return
	}
	if w.fatal == nil {
		w.fatal = err
	}
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// take returns the first refusal, and the last other error since the previous take.
func (w *watchErrors) take() (fatal, transient error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	fatal, transient, w.transient = w.fatal, w.transient, nil
	return fatal, transient
}

// lazySource watches one kind of [SourceKinds] that is not built in. Every SelectionPoll it checks whether an
// active module declares a source for the kind. While no watch runs, it then checks whether the chart rendered
// the kind's grant ([Config.Grants]), whether the API server serves the kind and whether the analyzer may list
// it in each watched namespace (one metadata-only list of at most one object per namespace, with the API reader
// and the lookup deadline; nothing is listed while a watch runs). Only then does it start the kind's
// controller, with a cache of its own, so an informer that cannot sync never holds up the manager's cache or
// the sources on it. A kind it may not read is reported (setWatch), never retried in a loop that fails, and
// starts no informer. While a watch runs, its reflector's errors decide (watchErrors): a refusal stops it, and
// a watch that failed is started again only after a backoff. A watch is stopped, and its cache with it, when
// the module is no longer active.
type lazySource struct {
	r      *sourceReconciler
	api    client.Reader
	mapper meta.RESTMapper
	log    logr.Logger
	start  watchStarter
	wake   chan struct{}

	running  *runningWatch
	failures int       // watches in a row that failed, for the backoff
	retryAt  time.Time // no watch is started before this, after a failure
	erring   int       // checks in a row that saw an error of the running watch
}

// runningWatch is a started controller.
type runningWatch struct {
	cancel context.CancelFunc
	done   <-chan error
	synced <-chan struct{}
	errs   *watchErrors
}

// NeedLeaderElection keeps the watch on the leader, with the collector's other controllers.
func (l *lazySource) NeedLeaderElection() bool { return true }

// Start runs the checks until ctx ends. It never returns an error, so a kind the analyzer may not read never
// stops the manager.
func (l *lazySource) Start(ctx context.Context) error {
	t := time.NewTicker(l.r.c.cfg.SelectionPoll)
	defer t.Stop()
	for {
		l.step(ctx)
		select {
		case <-ctx.Done():
			l.stop()
			// This process no longer watches anything (a stop or a lost leadership).
			setWatchedGauge(l.r.gvk, WatchInactive)
			return nil
		case <-t.C:
		case <-l.wake:
		}
	}
}

// step runs one check and starts or stops the watch.
func (l *lazySource) step(ctx context.Context) {
	gvk := l.r.gvk
	module, _, active := l.r.active()
	if !active {
		l.stop()
		l.failures, l.erring, l.retryAt = 0, 0, time.Time{}
		l.r.c.setWatch(l.log, module, gvk, WatchInactive, nil)
		return
	}
	var cause error
	if l.running != nil {
		var running bool
		if running, cause = l.check(module, false); running {
			return
		}
	} else if l.r.c.now().Before(l.retryAt) {
		// A watch failed: the kind stays reported as failed until the backoff ends.
		return
	}
	if s, ok := l.granted(); !ok {
		l.r.c.setWatch(l.log, module, gvk, s, nil)
		return
	}
	state, err := l.probe(ctx)
	if ctx.Err() != nil {
		// The process is stopping or lost leadership: the check was cut short, there is nothing to report.
		return
	}
	switch {
	case state != WatchWatched:
		// Also after a watch that failed: the kind is no longer served or granted, which says why.
		l.r.c.setWatch(l.log, module, gvk, state, err)
	case cause != nil:
		// The check passes but the watch failed (the list is granted but not the watch, or a refusal between
		// the check and the watch's first list): back off instead of starting it again at every check.
		l.fail(module, cause)
	default:
		l.begin(ctx, module)
	}
}

// begin starts the watch.
func (l *lazySource) begin(ctx context.Context, module string) {
	cctx, cancel := context.WithCancel(ctx)
	errs := &watchErrors{wake: l.wake}
	done, synced := l.start(cctx, l.r, errs)
	l.running = &runningWatch{cancel: cancel, done: done, synced: synced, errs: errs}
	l.erring = 0
	// A controller that could not be built stopped at once, and a cache may have synced already.
	if running, err := l.check(module, true); !running {
		l.fail(module, err)
	}
}

// check looks at the running watch and records its state. It returns false, with why, when the watch stopped
// (on its own) or was stopped (a refusal); the watch is then cleaned up. A watch that has run without an error
// since the previous check resets the backoff, unless it was started just now (fresh): a refusal of its watch
// may follow its first list within moments.
func (l *lazySource) check(module string, fresh bool) (bool, error) {
	w, gvk := l.running, l.r.gvk
	select {
	case err := <-w.done:
		w.cancel()
		l.running = nil
		if err == nil {
			err = errors.New("controller stopped")
		}
		return false, fmt.Errorf("the watch of %s stopped: %w", gvk, err)
	default:
	}
	fatal, transient := w.errs.take()
	if fatal != nil {
		l.stop()
		return false, fmt.Errorf("watching %s: %w", gvk, fatal)
	}
	select {
	case <-w.synced:
	default:
		// Not synced yet: the state is kept (the controller gives up after its cache sync timeout).
		return true, nil
	}
	if transient != nil {
		if l.erring++; l.erring >= failedAfterChecks {
			l.r.c.setWatch(l.log, module, gvk, WatchFailed, fmt.Errorf("watching %s: %w", gvk, transient))
		}
		return true, nil
	}
	l.erring = 0
	if !fresh {
		l.failures = 0
	}
	l.r.c.setWatch(l.log, module, gvk, WatchWatched, nil)
	return true, nil
}

// fail records a watch that failed and when it may be started again.
func (l *lazySource) fail(module string, err error) {
	l.failures++
	wait := watchBackoff(l.r.c.cfg.SelectionPoll, l.failures)
	l.retryAt = l.r.c.now().Add(wait)
	l.log.Error(err, "Watch of a symptom source kind failed, trying again later",
		append(kindKV(l.r.gvk, module), "retryAfter", wait.String())...)
	l.r.c.setWatch(l.log, module, l.r.gvk, WatchFailed, nil)
}

// watchBackoff is the wait after the n-th failed watch in a row (from 1): twice poll, doubled after each next
// one, never longer than maxWatchBackoff.
func watchBackoff(poll time.Duration, n int) time.Duration {
	d := 2 * poll
	for i := 1; i < n && d < maxWatchBackoff; i++ {
		d *= 2
	}
	return min(d, maxWatchBackoff)
}

// stop stops the watch, if it runs, with its cache.
func (l *lazySource) stop() {
	if l.running == nil {
		return
	}
	l.running.cancel()
	if err := <-l.running.done; err != nil {
		l.log.Error(err, "Watch of a symptom source kind stopped", "kind", l.r.gvk.Kind)
	}
	l.running = nil
}

// granted reports whether the chart rendered the grant of the kind ([Config.Grants]), and else the state to
// report: nothing is checked or watched then. Every kind is checked when the grants are not known.
func (l *lazySource) granted() (WatchState, bool) {
	grants := l.r.c.cfg.Grants
	if grants == nil {
		return "", true
	}
	name, optional := grantOf(l.r.gvk)
	switch {
	case slices.Contains(grants, name):
		return "", true
	case optional:
		return WatchOmitted, false
	default:
		return WatchNotGranted, false
	}
}

// probe reports whether the kind is served and may be listed in each watched namespace, or cluster-wide for a
// cluster-scoped kind. Each list asks for the metadata of at most one object. The error is returned for
// WatchFailed only.
func (l *lazySource) probe(ctx context.Context) (WatchState, error) {
	gvk := l.r.gvk
	mapping, err := l.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		if meta.IsNoMatchError(err) {
			return WatchNotServed, nil
		}
		return WatchFailed, fmt.Errorf("mapping %s: %w", gvk, err)
	}
	namespaces := l.r.c.cfg.Namespaces
	if mapping.Scope.Name() == meta.RESTScopeNameRoot {
		namespaces = []string{""}
	}
	if len(namespaces) == 0 {
		// No namespace to check the grant in: never watch unchecked (setupSources refuses this anyway).
		return WatchNotGranted, nil
	}
	for _, ns := range namespaces {
		list := &metav1.PartialObjectMetadataList{}
		list.SetGroupVersionKind(gvk.GroupVersion().WithKind(gvk.Kind + "List"))
		lctx, cancel := context.WithTimeout(ctx, l.r.c.cfg.LookupTimeout)
		err := l.api.List(lctx, list, client.InNamespace(ns), client.Limit(1))
		cancel()
		switch {
		case err == nil:
		case apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err):
			return WatchNotGranted, nil
		case apierrors.IsNotFound(err) || meta.IsNoMatchError(err):
			return WatchNotServed, nil
		default:
			return WatchFailed, fmt.Errorf("listing %s: %w", gvk, err)
		}
	}
	return WatchWatched, nil
}

// startController is the [watchStarter] of the binary: an unmanaged controller with the kind's predicate, on a
// cache of its own (newCache, from cfg and opts), run until ctx ends. The manager's cache never holds the kind:
// a source.Kind waits for every informer of its cache to sync, so an informer that cannot sync there would hold
// up, and after the sync timeout stop, the controllers of the built-in kinds and with them the manager. Its
// informer's first list passes every object an active source shows a symptom on, so no activation source is
// needed: the watch runs only while a module is active.
func startController(cfg *rest.Config, opts func() cache.Options, log logr.Logger) watchStarter {
	return func(ctx context.Context, r *sourceReconciler, errs *watchErrors) (<-chan error, <-chan struct{}) {
		done := make(chan error, 1)
		synced := make(chan struct{})
		o := opts()
		o.DefaultWatchErrorHandler = func(ctx context.Context, rf *toolscache.Reflector, err error) {
			toolscache.DefaultWatchErrorHandler(ctx, rf, err)
			errs.report(err)
		}
		c, err := cache.New(cfg, o)
		var ctl controller.Controller
		// The reconciler reads the objects from this cache.
		rr := *r
		rr.reader = c
		if err == nil {
			ctl, err = controller.NewUnmanaged(r.name(), controller.Options{
				Reconciler:              &rr,
				MaxConcurrentReconciles: 1,
				// Started again under the same name after a stop: one name per kind, collector-<kind>.
				SkipNameValidation: new(true),
				Logger:             log,
			})
		}
		if err == nil {
			err = ctl.Watch(source.Kind(c, rr.newObject(), &handler.EnqueueRequestForObject{}, rr.predicate()))
		}
		if err != nil {
			done <- err
			close(done)
			return done, synced
		}
		go func() {
			cctx, cancel := context.WithCancel(ctx)
			cacheDone := make(chan struct{})
			go func() {
				defer close(cacheDone)
				if err := c.Start(cctx); err != nil {
					log.Error(err, "Cache of a symptom source kind stopped", "kind", r.gvk.Kind)
				}
			}()
			inf, err := c.GetInformer(cctx, rr.newObject(), cache.BlockUntilSynced(false))
			if err == nil {
				go func() {
					if toolscache.WaitForCacheSync(cctx.Done(), inf.HasSynced) {
						close(synced)
					}
				}()
				err = ctl.Start(cctx)
			}
			cancel()
			<-cacheDone
			if ctx.Err() != nil {
				err = nil
			} else if err == nil {
				err = errors.New("controller stopped")
			}
			done <- err
			close(done)
		}()
		return done, synced
	}
}
