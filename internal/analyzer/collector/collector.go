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
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
)

// Defaults for the zero values of [Config].
const (
	DefaultBuffer        = 256
	DefaultMaxEvents     = 10000
	DefaultMaxPods       = 5000
	DefaultMaxOwners     = 4096
	DefaultOwnerTTL      = 10 * time.Minute
	DefaultLookupTimeout = 5 * time.Second
	DefaultSendTimeout   = 10 * time.Second
)

// retryAfter is how long a reconcile waits before trying again when the consumer did not take an
// observation within the send timeout.
const retryAfter = 5 * time.Second

// Observation is one symptom ready for the Incident controller: the fingerprint and the normalized,
// redacted symptom are computed here, so no raw event or status text ever leaves the collector.
type Observation struct {
	// Fingerprint identifies the incident (package fingerprint).
	Fingerprint string
	// Subject is the object the symptom is correlated to: the owning workload when the owner chain could be
	// verified, otherwise the object itself.
	Subject remediationv1alpha1.ObjectRef
	// Reason, Type and ReportingController are the event's, or OOMKilled (or the termination reason),
	// Warning and kubelet for a container termination read from pod status.
	Reason              string
	Type                string
	ReportingController string
	// Symptom is the normalized, redacted note (at most 1024 bytes).
	Symptom string
	// Count is how many new occurrences this observation stands for, at least 1: the increase of an event
	// series, never its total.
	Count int64
	// ObservedAt is when the latest of these occurrences happened, as reported by the cluster, never later
	// than the collector's clock.
	ObservedAt time.Time
	// MayRepeat is set when the collector cannot rule out that some of Count was already emitted before:
	// the event or termination is older than this collector's first reconcile (a restart or a new leader),
	// or the collector dropped state of objects at least as old to stay within its bounds. The consumer
	// compares ObservedAt with what the Incident already counted.
	MayRepeat bool
}

// LastSeenFunc returns when the Incident with this fingerprint last counted an observation, and false when
// there is no such Incident. The Incident controller (#42) supplies it, so the dedup state of a series that
// started before a restart lives in the Incident counters, not in memory alone.
type LastSeenFunc func(ctx context.Context, fingerprint string) (time.Time, bool, error)

// Config configures a [Collector]. Zero values take the defaults above.
type Config struct {
	// ClusterID is hashed into every fingerprint. Required.
	ClusterID string
	// Buffer is the capacity of the observation channel.
	Buffer int
	// MaxEvents bounds the number of event series whose last handled count is remembered.
	MaxEvents int
	// MaxPods bounds the number of pods whose handled container terminations are remembered.
	MaxPods int
	// MaxOwners bounds the owner correlation cache.
	MaxOwners int
	// OwnerTTL is how long a resolved owner is reused before it is read again.
	OwnerTTL time.Duration
	// LookupTimeout is the deadline of each read of an owner from the API server.
	LookupTimeout time.Duration
	// SendTimeout is how long a reconcile waits for room in the channel before it tries again later.
	SendTimeout time.Duration
	// LastSeen, when set, suppresses observations marked MayRepeat that the Incident has already counted.
	LastSeen LastSeenFunc
	// Now is the clock; time.Now when nil.
	Now func() time.Time
}

// Collector turns events and pod status in the watched namespaces into [Observation] values.
type Collector struct {
	cfg  Config
	out  chan Observation
	now  func() time.Time
	once sync.Once
	// start is the time of the first reconcile; anything older may have been counted by an earlier process.
	start time.Time

	series *seriesTracker
	pods   *terminationTracker
	owners *ownerResolver
}

// New validates cfg, applies defaults and returns a collector. Call [Collector.SetupWithManager] to start
// watching, and read [Collector.Observations].
func New(cfg Config) (*Collector, error) {
	if cfg.ClusterID == "" {
		return nil, errors.New("collector: a cluster ID is required")
	}
	setDefault(&cfg.Buffer, DefaultBuffer)
	setDefault(&cfg.MaxEvents, DefaultMaxEvents)
	setDefault(&cfg.MaxPods, DefaultMaxPods)
	setDefault(&cfg.MaxOwners, DefaultMaxOwners)
	setDefault(&cfg.OwnerTTL, DefaultOwnerTTL)
	setDefault(&cfg.LookupTimeout, DefaultLookupTimeout)
	setDefault(&cfg.SendTimeout, DefaultSendTimeout)
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Collector{
		cfg:    cfg,
		out:    make(chan Observation, cfg.Buffer),
		now:    cfg.Now,
		series: newSeriesTracker(cfg.MaxEvents),
		pods:   newTerminationTracker(cfg.MaxPods),
	}, nil
}

func setDefault[T int | time.Duration](v *T, def T) {
	if *v <= 0 {
		*v = def
	}
}

// Observations returns the bounded channel the collector writes to. It is never closed.
func (c *Collector) Observations() <-chan Observation {
	return c.out
}

// SetupWithManager registers the two watches: events.k8s.io/v1 Events of type Warning and Pods with a
// terminated container. Both read through the manager's cache, which the analyzer restricts to the watched
// namespaces (analyzer.CacheOptions). Owners are read with the API reader, one object at a time with a
// deadline, so no informer is started for ReplicaSets, Deployments or StatefulSets. There is deliberately no
// watch on core/v1 Events: they are the same objects, and watching both would count each event twice.
func (c *Collector) SetupWithManager(mgr ctrl.Manager) error {
	c.owners = newOwnerResolver(mgr.GetClient(), mgr.GetAPIReader(), c.cfg)
	er := &eventReconciler{c: c, reader: mgr.GetClient()}
	err := ctrl.NewControllerManagedBy(mgr).
		Named("collector-events").
		For(&eventsv1.Event{}, builder.WithPredicates(predicate.NewPredicateFuncs(isWarning))).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Complete(er)
	if err != nil {
		return fmt.Errorf("setting up the event collector: %w", err)
	}
	pr := &podReconciler{c: c, reader: mgr.GetClient()}
	err = ctrl.NewControllerManagedBy(mgr).
		Named("collector-pods").
		For(&corev1.Pod{}, builder.WithPredicates(predicate.NewPredicateFuncs(hasTermination))).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Complete(pr)
	if err != nil {
		return fmt.Errorf("setting up the pod collector: %w", err)
	}
	return nil
}

// started returns the time of the first reconcile.
func (c *Collector) started() time.Time {
	c.once.Do(func() { c.start = c.now() })
	return c.start
}

// observedAt clamps a time reported by the cluster: never zero and never in the future, so a forged
// timestamp cannot hide later occurrences from the restart check.
func (c *Collector) observedAt(t time.Time) time.Time {
	now := c.now()
	if t.IsZero() || t.After(now) {
		return now
	}
	return t
}

// skipRepeat reports whether an observation marked MayRepeat was already counted by its Incident.
func (c *Collector) skipRepeat(ctx context.Context, o *Observation) (bool, error) {
	if !o.MayRepeat || c.cfg.LastSeen == nil {
		return false, nil
	}
	seen, ok, err := c.cfg.LastSeen(ctx, o.Fingerprint)
	if err != nil {
		return false, fmt.Errorf("reading the last counted time of an incident: %w", err)
	}
	return ok && !o.ObservedAt.After(seen), nil
}

// emit hands o to the consumer. It returns false when the channel stayed full for the send timeout or the
// context ended; the caller then keeps its state unchanged and tries again later, so nothing is lost and
// nothing is counted twice within one process.
func (c *Collector) emit(ctx context.Context, o Observation) bool {
	t := time.NewTimer(c.cfg.SendTimeout)
	defer t.Stop()
	select {
	case c.out <- o:
		return true
	case <-t.C:
		return false
	case <-ctx.Done():
		return false
	}
}

func isWarning(o client.Object) bool {
	ev, ok := o.(*eventsv1.Event)
	return ok && ev.Type == corev1.EventTypeWarning
}
