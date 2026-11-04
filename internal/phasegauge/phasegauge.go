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
// Package phasegauge holds what the phase gauges of the executor and the analyzer share: the closed set of
// their phase label, and the reader they list from, which is set only while the replica is the leader and the
// informer of the listed kind has synced. A scrape therefore never starts an informer or waits for one, and
// makes no API call.
package phasegauge

import (
	"context"
	"slices"
	"sync/atomic"

	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

// Values of a phase label outside the phase enum.
const (
	// None is the label of an object whose status has no phase yet.
	None = "None"
	// Other is the label of a phase the enum does not hold, for example one written by a newer version.
	Other = "Other"
)

// Label returns the phase label of p: p itself when known holds it, [None] when it is empty, and [Other]
// otherwise. The result is always one of known, None or Other, whatever the cluster wrote.
func Label[P ~string](p P, known []P) string {
	switch {
	case p == "":
		return None
	case slices.Contains(known, p):
		return string(p)
	}
	return Other
}

// Labels returns every value [Label] can return for known: known, then None and Other.
func Labels[P ~string](known []P) []string {
	out := make([]string, 0, len(known)+2)
	for _, p := range known {
		out = append(out, string(p))
	}
	return append(out, None, Other)
}

// Informers is the part of a manager's cache a [Source] needs.
type Informers interface {
	client.Reader
	GetInformer(ctx context.Context, obj client.Object, opts ...cache.InformerGetOption) (cache.Informer, error)
}

// Source is the reader a phase collector lists from. It is nil, and the collector reports nothing, until the
// runnable from [Source.Runnable] has run on the leader and the informer has synced, and again once that
// runnable stops (a lost leadership or shutdown).
type Source struct {
	reader atomic.Pointer[client.Reader]
}

// Reader returns the reader to list from, or nil.
func (s *Source) Reader() client.Reader {
	if r := s.reader.Load(); r != nil {
		return *r
	}
	return nil
}

// Set sets the reader; nil clears it.
func (s *Source) Set(r client.Reader) {
	if r == nil {
		s.reader.Store(nil)
		return
	}
	s.reader.Store(&r)
}

// Runnable returns a runnable for the manager that runs only on the leader. It gets the informer of obj's kind
// from c, waiting until it has synced (the controller that lists obj's kind uses the same informer, so no
// other watch is opened), then sets the source to c until ctx ends. If the informer cannot be had, the
// source stays empty and the error is logged; the manager is not stopped for a metric.
func (s *Source) Runnable(c Informers, obj client.Object) manager.Runnable {
	return &runnable{source: s, informers: c, obj: obj}
}

type runnable struct {
	source    *Source
	informers Informers
	obj       client.Object
}

// NeedLeaderElection reports that the source is set only on the leader.
func (r *runnable) NeedLeaderElection() bool { return true }

// Start sets the source once the informer has synced and clears it when ctx ends.
func (r *runnable) Start(ctx context.Context) error {
	defer r.source.Set(nil)
	if _, err := r.informers.GetInformer(ctx, r.obj); err != nil {
		if ctx.Err() == nil {
			logf.FromContext(ctx).Error(err, "Failed to get the informer for a phase metric, the metric stays empty",
				"kind", r.obj.GetObjectKind().GroupVersionKind().Kind)
		}
		return nil
	}
	r.source.Set(r.informers)
	<-ctx.Done()
	return nil
}
