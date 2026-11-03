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

package incidents

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/analyzer/collector"
	"github.com/pkalin/shadewright/internal/incidents/fingerprint"
)

// Defaults for the zero values of [Config].
const (
	DefaultWindow       = 5 * time.Minute
	DefaultMaxIncidents = 500
	DefaultWriteTimeout = 10 * time.Second
	DefaultRetryAfter   = 5 * time.Second
)

// refusedRemembered bounds the fingerprints refused at the cap that are remembered, so each gets one
// Warning event per process.
const refusedRemembered = 1024

// Config configures a [Controller]. Zero values take the defaults above.
type Config struct {
	// Namespace is the operator namespace, where every Incident lives (ADR-0007). Required.
	Namespace string
	// Window is the aggregation window: the observations of one Incident that arrive within it after a
	// status write are batched into the next one.
	Window time.Duration
	// MaxIncidents caps the Incidents in Namespace. At the cap, an observation of a new fingerprint is not
	// stored: it is counted in a metric and reported in a Warning event.
	MaxIncidents int
	// WriteTimeout is the deadline of each write of one Incident, with its retries on conflict.
	WriteTimeout time.Duration
	// RetryAfter is how long a batch whose write failed waits before it is tried again.
	RetryAfter time.Duration
	// Recorder and Regarding report a refused fingerprint as a Warning event about Regarding (the analyzer's
	// Pod). No event is written when either is nil.
	Recorder  events.EventRecorder
	Regarding runtime.Object
	// Now is the clock; time.Now when nil.
	Now func() time.Time
}

// Controller keeps one durable Incident per fingerprint (design 3.2, ADR-0007). It consumes the collector's
// observations, batches them per Incident within the aggregation window, and writes the counters,
// timestamps and per-occurrence records to the Incident's status with optimistic concurrency. It also
// answers the collector's [collector.HandledFunc] from memory, loaded from the Incidents at start.
type Controller struct {
	cfg    Config
	client client.Client
	reader client.Reader
	cache  client.Reader
	obs    <-chan collector.Observation
	ready  chan struct{}

	mu sync.Mutex
	// records holds, per occurrence, what was counted and by which Incident: the records of every Incident
	// plus what is pending. It is bounded by MaxIncidents times MaxOccurrenceRecords plus pending batches.
	records map[string]record
	// droppedThrough is the newest begin time of a record any Incident dropped.
	droppedThrough time.Time
	// names are the Incidents that exist or are pending; their number is held to MaxIncidents.
	names     map[string]struct{}
	pending   map[string]*batch
	// nextWrite is when an Incident's pending batch is due: the window after its last write, or RetryAfter
	// after a failed one. An Incident without an entry is written at once.
	nextWrite map[string]time.Time
	refused   *boundedSet
}

// record is what was counted of one occurrence and by which Incident.
type record struct {
	counted
	incident string
}

// New validates cfg and returns a controller. Call [Controller.SetupWithManager] to start it.
func New(cfg Config) (*Controller, error) {
	if cfg.Namespace == "" {
		return nil, errors.New("incidents: the operator namespace is required")
	}
	if cfg.Window <= 0 {
		cfg.Window = DefaultWindow
	}
	if cfg.MaxIncidents <= 0 {
		cfg.MaxIncidents = DefaultMaxIncidents
	}
	if cfg.WriteTimeout <= 0 {
		cfg.WriteTimeout = DefaultWriteTimeout
	}
	if cfg.RetryAfter <= 0 {
		cfg.RetryAfter = DefaultRetryAfter
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Controller{
		cfg:       cfg,
		ready:     make(chan struct{}),
		records:   map[string]record{},
		names:     map[string]struct{}{},
		pending:   map[string]*batch{},
		nextWrite: map[string]time.Time{},
		refused:   newBoundedSet(refusedRemembered),
	}, nil
}

// SetupWithManager adds the controller to mgr, consuming obs. It writes with the manager's client, reads an
// Incident before each write with the API reader, so a retry never works from a stale copy, and loads the
// Incidents from the cache at start. It runs only on the leader, like the collector.
func (c *Controller) SetupWithManager(mgr ctrl.Manager, obs <-chan collector.Observation) error {
	c.client, c.reader, c.cache, c.obs = mgr.GetClient(), mgr.GetAPIReader(), mgr.GetClient(), obs
	if err := mgr.Add(c); err != nil {
		return fmt.Errorf("adding the Incident controller: %w", err)
	}
	return nil
}

// NeedLeaderElection reports that only the leader writes Incidents.
func (c *Controller) NeedLeaderElection() bool { return true }

// Name returns the incident's object name: the fingerprint's hex digest with a fixed prefix, so a
// fingerprint always names the same Incident and a restart finds it.
func Name(fp string) string {
	return "incident-" + strings.TrimPrefix(fp, fingerprint.Prefix)
}

// Start loads the Incidents, then consumes observations and writes due batches until ctx ends. Then it
// writes what is pending, each Incident within WriteTimeout, so a clean stop loses nothing.
func (c *Controller) Start(ctx context.Context) error {
	if err := c.load(ctx); err != nil {
		return err
	}
	tick := time.NewTicker(min(max(c.cfg.Window/10, 100*time.Millisecond), 30*time.Second))
	defer tick.Stop()
	for {
		select {
		case o := <-c.obs:
			if c.consume(ctx, o) {
				c.flush(ctx, o.Fingerprint)
			}
		case <-tick.C:
			c.flushDue(ctx, false)
		case <-ctx.Done():
			c.flushDue(context.WithoutCancel(ctx), true)
			return nil
		}
	}
}

// load reads every Incident in the namespace from the cache into memory: their names, their occurrence
// records and the newest dropped begin time. Until it is done, Handled waits.
func (c *Controller) load(ctx context.Context) error {
	var list remediationv1alpha1.IncidentList
	if err := c.cache.List(ctx, &list, client.InNamespace(c.cfg.Namespace)); err != nil {
		return fmt.Errorf("listing the Incidents: %w", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, inc := range list.Items {
		c.names[inc.Name] = struct{}{}
		c.remember(inc.Name, &inc.Status)
	}
	close(c.ready)
	logf.FromContext(ctx).Info("Loaded Incidents", "count", len(list.Items), "namespace", c.cfg.Namespace)
	return nil
}

// remember takes the records of one Incident's status into memory. The caller holds mu.
func (c *Controller) remember(name string, status *remediationv1alpha1.IncidentStatus) {
	for _, r := range status.Occurrences {
		cur := c.records[r.ID]
		c.records[r.ID] = record{counted: higher(cur.counted, counted{total: r.Total, began: timeFrom(r.Began)}), incident: name}
	}
	c.droppedThrough = latest(c.droppedThrough, timeFrom(status.OccurrencesDroppedThrough))
}

// Handled answers the collector (collector.HandledFunc): the highest total counted for the occurrence by
// any Incident. An occurrence with no record that began no later than the newest record any Incident
// dropped is answered as counted in full (collector.AllHandled), so the bound never counts twice; found is
// false only for an occurrence never counted. It answers from memory, and waits until the Incidents are
// loaded or ctx ends.
func (c *Controller) Handled(ctx context.Context, occurrence string, began time.Time) (int64, bool, error) {
	select {
	case <-c.ready:
	case <-ctx.Done():
		return 0, false, fmt.Errorf("waiting for the Incidents to load: %w", ctx.Err())
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	handled, found := c.handled(occurrence, began)
	return handled, found, nil
}

// handled is Handled without the wait. The caller holds mu.
func (c *Controller) handled(occurrence string, began time.Time) (int64, bool) {
	if r, ok := c.records[occurrence]; ok {
		return r.total, true
	}
	if !c.droppedThrough.IsZero() && !beganOrNone(began.Truncate(time.Second)).After(c.droppedThrough) {
		return collector.AllHandled, true
	}
	return 0, false
}

// consume counts one observation into the pending batch of its Incident. An observation marked MayRepeat
// counts Increase of what Handled would answer. Any other counts its Count (the collector knows that much
// is new), but never more than Increase of the occurrence's record, so an occurrence another process
// already counted is not counted again; the drop bound does not apply to it. A new fingerprint at the cap
// is refused.
//
// It reports whether the batch is due at once: the Incident has not been written by this process yet.
func (c *Controller) consume(ctx context.Context, o collector.Observation) bool {
	o.Began = o.Began.Truncate(time.Second)
	c.mu.Lock()
	defer c.mu.Unlock()
	n := o.Count
	if o.MayRepeat {
		handled, _ := c.handled(o.Occurrence, o.Began)
		n = o.Increase(handled)
	} else if r, ok := c.records[o.Occurrence]; ok {
		n = min(n, o.Increase(r.total))
	}
	if n <= 0 {
		return false
	}
	name := Name(o.Fingerprint)
	if _, ok := c.names[name]; !ok {
		if len(c.names) >= c.cfg.MaxIncidents {
			c.refuse(ctx, o)
			return false
		}
		c.names[name] = struct{}{}
	}
	b := c.pending[o.Fingerprint]
	if b == nil {
		b = &batch{spec: specOf(o), occurrences: map[string]counted{}}
		c.pending[o.Fingerprint] = b
	}
	at := c.notFuture(o.ObservedAt)
	b.add(&batch{count: n, first: at, last: at, occurrences: map[string]counted{
		o.Occurrence: {total: o.Total, began: o.Began},
	}})
	cur := c.records[o.Occurrence]
	c.records[o.Occurrence] = record{counted: higher(cur.counted, counted{total: o.Total, began: o.Began}), incident: name}
	_, scheduled := c.nextWrite[o.Fingerprint]
	return !scheduled
}

// refuse counts and reports an observation of a new fingerprint at the cap. The event names the subject and
// the fingerprint, nothing from the event's text, and is written once per fingerprint per process. The
// caller holds mu.
func (c *Controller) refuse(ctx context.Context, o collector.Observation) {
	refusedObservations.Inc()
	if !c.refused.add(o.Fingerprint) {
		return
	}
	s := o.Subject
	logf.FromContext(ctx).Info("Refused a new Incident at the cap", "max", c.cfg.MaxIncidents,
		"fingerprint", o.Fingerprint, "kind", s.Kind, "namespace", s.Namespace, "name", s.Name)
	if c.cfg.Recorder != nil && c.cfg.Regarding != nil {
		c.cfg.Recorder.Eventf(c.cfg.Regarding, nil, corev1.EventTypeWarning, "IncidentCapReached", "StoreIncident",
			"The cap of %d Incidents is reached: not storing a new Incident for %s %s/%s (fingerprint %s)",
			c.cfg.MaxIncidents, s.Kind, s.Namespace, s.Name, o.Fingerprint)
	}
}

// notFuture returns t, or the clock when t is zero or later.
func (c *Controller) notFuture(t time.Time) time.Time {
	if now := c.cfg.Now(); t.IsZero() || t.After(now) {
		return now
	}
	return t
}

// specOf returns the spec of the Incident an observation belongs to. It holds the fingerprint, the subject
// and the normalized, redacted symptom, nothing else from the cluster.
func specOf(o collector.Observation) remediationv1alpha1.IncidentSpec {
	return remediationv1alpha1.IncidentSpec{
		Fingerprint:         o.Fingerprint,
		Subject:             o.Subject,
		Reason:              o.Reason,
		Type:                o.Type,
		ReportingController: o.ReportingController,
		Symptom:             o.Symptom,
	}
}

// flushDue writes the pending batches whose Incident was not written within the window, or all of them.
func (c *Controller) flushDue(ctx context.Context, all bool) {
	c.mu.Lock()
	now := c.cfg.Now()
	var due []string
	for fp := range c.pending {
		if next, ok := c.nextWrite[fp]; all || !ok || !now.Before(next) {
			due = append(due, fp)
		}
	}
	c.mu.Unlock()
	for _, fp := range due {
		c.flush(ctx, fp)
	}
}

// flush writes one pending batch. The batch is taken out of pending first, so observations arriving during
// the write start a new one; if the write fails, the batch goes back and is tried again at the next tick,
// so nothing is lost. Each attempt reads the Incident fresh: a conflict is retried on the new version, and a
// batch the status already holds (a write that succeeded but whose answer was lost) is not added twice.
func (c *Controller) flush(ctx context.Context, fp string) {
	c.mu.Lock()
	b := c.pending[fp]
	delete(c.pending, fp)
	c.mu.Unlock()
	if b == nil {
		return
	}
	wctx, cancel := context.WithTimeout(ctx, c.cfg.WriteTimeout)
	defer cancel()
	inc, dropped, err := c.write(wctx, b)
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		if p := c.pending[fp]; p != nil {
			b.add(p)
		}
		c.pending[fp] = b
		c.nextWrite[fp] = c.cfg.Now().Add(c.cfg.RetryAfter)
		logf.FromContext(ctx).Error(err, "Failed to write the Incident status, will retry", "incident", Name(fp))
		return
	}
	c.nextWrite[fp] = c.cfg.Now().Add(c.cfg.Window)
	for _, id := range dropped {
		if r, ok := c.records[id]; ok && r.incident == inc.Name {
			delete(c.records, id)
		}
	}
	c.remember(inc.Name, &inc.Status)
}

// write creates the Incident if needed and adds b to its status, retrying on conflict. It returns the
// Incident as written and the IDs of the records the write dropped.
func (c *Controller) write(ctx context.Context, b *batch) (*remediationv1alpha1.Incident, []string, error) {
	key := types.NamespacedName{Namespace: c.cfg.Namespace, Name: Name(b.spec.Fingerprint)}
	var inc *remediationv1alpha1.Incident
	var dropped []string
	err := retry.OnError(retry.DefaultBackoff, retriable, func() error {
		inc, dropped = &remediationv1alpha1.Incident{}, nil
		err := c.reader.Get(ctx, key, inc)
		if apierrors.IsNotFound(err) {
			inc = &remediationv1alpha1.Incident{
				ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name},
				Spec:       b.spec,
			}
			err = c.client.Create(ctx, inc)
		}
		if err != nil {
			return err
		}
		if inc.Spec.Fingerprint != b.spec.Fingerprint {
			return fmt.Errorf("incident %s holds fingerprint %s, not %s", key.Name, inc.Spec.Fingerprint, b.spec.Fingerprint)
		}
		if applied(&inc.Status, b) {
			return nil
		}
		dropped = merge(&inc.Status, b)
		return c.client.Status().Update(ctx, inc)
	})
	if err != nil {
		return nil, nil, fmt.Errorf("writing Incident %s: %w", key.Name, err)
	}
	return inc, dropped, nil
}

// retriable reports whether a write is tried again at once on a fresh read: a conflict, or a create that
// lost a race to another writer.
func retriable(err error) bool {
	return apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err)
}
