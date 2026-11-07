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
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
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
	// WatchWatched: the watch runs.
	WatchWatched WatchState = "Watched"
	// WatchNotServed: the API server does not serve the kind (the add-on's CRD is not installed).
	WatchNotServed WatchState = "NotServed"
	// WatchNotGranted: the analyzer may not list the kind in every watched namespace (or cluster-wide, for a
	// cluster-scoped kind): the chart's value of the module is off.
	WatchNotGranted WatchState = "NotGranted"
	// WatchFailed: the check or the start of the watch failed otherwise; it is tried again.
	WatchFailed WatchState = "Failed"
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
// with why, in the order of [SourceKinds]. It is safe for concurrent use. Detection reports them in the
// condition SymptomSourcesNotWatched of RemediationSettings status.
func (c *Collector) SourcesNotWatched() []SourceWatch {
	c.watchMu.Lock()
	defer c.watchMu.Unlock()
	var out []SourceWatch
	for _, gvk := range SourceKinds {
		if s, ok := c.watches[gvk]; ok && s != WatchInactive && s != WatchWatched {
			out = append(out, SourceWatch{Kind: gvk, State: s})
		}
	}
	return out
}

// setWatch records the state of a kind's watch: in the gauge, and, when it changed, in the log and by calling
// [Config.WatchChanged].
func (c *Collector) setWatch(log logr.Logger, module string, gvk schema.GroupVersionKind, s WatchState, cause error) {
	setWatchedGauge(gvk, s)
	c.watchMu.Lock()
	prev, known := c.watches[gvk]
	c.watches[gvk] = s
	c.watchMu.Unlock()
	if known && prev == s || !known && s == WatchInactive {
		return
	}
	kv := []any{"group", gvk.Group, "version", gvk.Version, "kind", gvk.Kind, "module", module, "state", string(s)}
	switch s {
	case WatchWatched:
		log.Info("Started watching a symptom source kind", kv...)
	case WatchInactive:
		log.Info("Stopped watching a symptom source kind, no active module declares it", kv...)
	case WatchNotServed:
		log.Info("Not watching a symptom source kind of an active module, the API server does not serve it", kv...)
	case WatchNotGranted:
		log.Info("Not watching a symptom source kind of an active module, the analyzer may not list it: "+
			"turn on the module's value under analyzer.modules in the chart", kv...)
	default:
		log.Error(cause, "Not watching a symptom source kind of an active module, the check failed", kv...)
	}
	if c.cfg.WatchChanged != nil {
		c.cfg.WatchChanged()
	}
}

// watchStarter starts the watch of a kind and returns a channel that receives the controller's result when it
// stops (an error when its cache did not sync). It is replaced in tests.
type watchStarter func(ctx context.Context, r *sourceReconciler) <-chan error

// lazySource watches one kind of [SourceKinds] that is not built in. Every SelectionPoll it checks whether an
// active module declares a source for the kind, and then whether the API server serves the kind and the
// analyzer may list it in each watched namespace (one list of at most one object each, with the API reader and
// the lookup deadline). Only then does it start the kind's controller, whose informer comes from the manager's
// cache. A kind it may not read is reported (setWatch), never retried in a loop that fails, and starts no
// informer. A watch that runs is stopped, and its informer removed, when the module is no longer active or the
// kind is no longer served or granted.
type lazySource struct {
	r      *sourceReconciler
	api    client.Reader
	mapper meta.RESTMapper
	cache  cache.Informers
	log    logr.Logger
	start  watchStarter

	running *runningWatch
}

// runningWatch is a started controller.
type runningWatch struct {
	cancel context.CancelFunc
	done   <-chan error
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
			l.stop(context.Background())
			// This process no longer watches anything (a stop or a lost leadership).
			setWatchedGauge(l.r.gvk, WatchInactive)
			return nil
		case <-t.C:
		}
	}
}

// step runs one check and starts or stops the watch.
func (l *lazySource) step(ctx context.Context) {
	gvk := l.r.gvk
	if l.running != nil {
		select {
		case err := <-l.running.done:
			// The controller stopped on its own: its cache did not sync. Clean up and check again below.
			l.log.Error(err, "Watch of a symptom source kind stopped", "kind", gvk.Kind)
			l.running = nil
			l.removeInformer(ctx)
		default:
		}
	}
	module, _, active := l.r.active()
	if !active {
		l.stop(ctx)
		l.r.c.setWatch(l.log, module, gvk, WatchInactive, nil)
		return
	}
	state, err := l.probe(ctx)
	switch {
	case state == WatchFailed && l.running != nil:
		// A transient failure of the check: the watch that runs keeps running.
		return
	case state != WatchWatched:
		l.stop(ctx)
		l.r.c.setWatch(l.log, module, gvk, state, err)
		return
	}
	if l.running == nil {
		cctx, cancel := context.WithCancel(ctx)
		l.running = &runningWatch{cancel: cancel, done: l.start(cctx, l.r)}
	}
	l.r.c.setWatch(l.log, module, gvk, WatchWatched, nil)
}

// stop stops the watch, if it runs, and removes its informer from the cache.
func (l *lazySource) stop(ctx context.Context) {
	if l.running == nil {
		return
	}
	l.running.cancel()
	if err := <-l.running.done; err != nil {
		l.log.Error(err, "Watch of a symptom source kind stopped", "kind", l.r.gvk.Kind)
	}
	l.running = nil
	l.removeInformer(ctx)
}

func (l *lazySource) removeInformer(ctx context.Context) {
	if l.cache == nil {
		return
	}
	if err := l.cache.RemoveInformer(ctx, l.r.newObject()); err != nil {
		l.log.Error(err, "Failed to remove the informer of a symptom source kind", "kind", l.r.gvk.Kind)
	}
}

// probe reports whether the kind is served and may be listed in each watched namespace, or cluster-wide for a
// cluster-scoped kind. The error is returned for WatchFailed only.
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
	for _, ns := range namespaces {
		lctx, cancel := context.WithTimeout(ctx, l.r.c.cfg.LookupTimeout)
		err := l.api.List(lctx, l.r.newList(), client.InNamespace(ns), client.Limit(1))
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

// startController is the [watchStarter] of the binary: an unmanaged controller with the kind's predicate, run
// until ctx ends. Its informer's first list passes every object an active source shows a symptom on, so no
// activation source is needed: the watch runs only while a module is active.
func startController(c cache.Cache, log logr.Logger) watchStarter {
	return func(ctx context.Context, r *sourceReconciler) <-chan error {
		done := make(chan error, 1)
		ctl, err := controller.NewUnmanaged(r.name(), controller.Options{
			Reconciler:              r,
			MaxConcurrentReconciles: 1,
			// Started again under the same name after a stop: one name per kind, collector-<kind>.
			SkipNameValidation: new(true),
			Logger:             log,
		})
		if err == nil {
			err = ctl.Watch(source.Kind(c, r.newObject(), &handler.EnqueueRequestForObject{}, r.predicate()))
		}
		if err != nil {
			done <- err
			close(done)
			return done
		}
		go func() {
			err := ctl.Start(ctx)
			if ctx.Err() != nil {
				err = nil
			} else if err == nil {
				err = errors.New("controller stopped")
			}
			done <- err
			close(done)
		}()
		return done
	}
}
