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
	corerecord "k8s.io/client-go/tools/record"
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
	// DefaultPassTimeout bounds one pass of the analysis trigger, its reads and writes together. It stays
	// under DefaultTriggerInterval, so a call that stalls costs at most the pass it is in.
	DefaultPassTimeout = 8 * time.Second
	// DefaultReadTimeout bounds the trigger's budget read within a pass. It leaves the rest of the pass to
	// take back the marks that say due when the read fails.
	DefaultReadTimeout = 5 * time.Second
	DefaultRetryAfter  = 5 * time.Second
	// DefaultEventInterval is the least time between two Warning events at the Incident cap.
	DefaultEventInterval = 10 * time.Minute
	// DefaultStopTimeout bounds the last writes when the controller stops. It stays under the analyzer Pod's
	// termination grace period (10 s, config/manager/analyzer.yaml), which is shorter than the manager's
	// graceful shutdown timeout (30 s).
	DefaultStopTimeout = 5 * time.Second
	// DefaultMaxPendingOccurrences and DefaultMaxPendingOccurrencesPerIncident bound the occurrences held
	// unwritten, in pending, failed and in-flight batches together. One Incident's cap is eight times the
	// records its status keeps, so a burst of up to that many new occurrences within one window is counted
	// in full.
	DefaultMaxPendingOccurrences            = 10000
	DefaultMaxPendingOccurrencesPerIncident = 8 * remediationv1alpha1.MaxOccurrenceRecords
)

// overflowLogInterval is the least time between two log lines about occurrences left uncounted at the caps
// on occurrences held unwritten; the metric counts every one.
const overflowLogInterval = time.Minute

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
	// PassTimeout is the deadline of one pass of the analysis trigger: the list from the cache, the budget
	// read and the writes of the AnalysisDue conditions together (each write keeps its WriteTimeout within
	// it). It must be shorter than TriggerInterval; zero takes DefaultPassTimeout, or nine tenths of
	// TriggerInterval when that is shorter. A pass that runs into it stops; the Incidents it did not decide
	// keep their marks until the next pass.
	PassTimeout time.Duration
	// ReadTimeout is the deadline of the analysis trigger's budget read in each pass: the RemediationSettings
	// and the budget ConfigMap together. A read that runs into it counts as a read that failed
	// (SettingsUnreadable or BudgetReadFailed): the Incidents marked due are set not due in the rest of the
	// pass. It must be shorter than PassTimeout; zero takes DefaultReadTimeout, or three quarters of
	// PassTimeout when that is shorter.
	ReadTimeout time.Duration
	// RetryAfter is how long a batch whose write failed waits before it is tried again.
	RetryAfter time.Duration
	// Recorder and Regarding report a refused fingerprint as a Warning event about Regarding (the analyzer's
	// Pod). No event is written when either is nil. It is a core/v1 recorder, so the event is written with the
	// grant on core Events that the analyzer's leader election Role already holds in the operator namespace,
	// and no other.
	Recorder  corerecord.EventRecorder
	Regarding runtime.Object
	// EventInterval is the least time between two Warning events at the cap: whoever can create events in a
	// watched namespace can make new fingerprints at will, so later refusals within it are only logged and
	// counted.
	EventInterval time.Duration
	// StopTimeout bounds the last writes when the controller stops (shutdown or lost leadership): batches
	// still unwritten then are logged and counted as lost.
	StopTimeout time.Duration
	// MaxPendingOccurrences caps the occurrences held unwritten by all Incidents together, and
	// MaxPendingOccurrencesPerIncident those of one Incident: the distinct occurrences in its pending, failed
	// and in-flight batches, each batch counted on its own. A new occurrence over either cap is not counted
	// (an undercount); a new occurrence is one its Incident's pending batch does not hold yet, so one held
	// only in a failed or in-flight batch counts as new there. Its observation still moves the Incident's
	// last seen, and it is counted in
	// shadewright_incident_occurrences_overflow_total and logged at most once per minute. An Incident that
	// holds nothing unwritten always takes its first occurrence, so the total can exceed
	// MaxPendingOccurrences by one per Incident.
	MaxPendingOccurrences            int
	MaxPendingOccurrencesPerIncident int
	// Debounce, SettleLimit, Cooldown, AnalysisTimeout and TriggerInterval drive the analysis trigger
	// ([Due]): how long an Incident must go without a new observation, the longest it waits to settle after
	// its first unsettled observation (the aggregation window by default), the least time between the start
	// of one analysis and the next, how long an analysis may run before it is taken as lost, and how often
	// the Incidents are decided again.
	Debounce        time.Duration
	SettleLimit     time.Duration
	Cooldown        time.Duration
	AnalysisTimeout time.Duration
	TriggerInterval time.Duration
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
	// plus what is held unwritten. It is bounded by the records of the Incidents in memory (MaxIncidents
	// times MaxOccurrenceRecords) plus the occurrences held unwritten.
	records map[string]record
	// recordsOf indexes records by Incident name, so the records of one Incident are found without visiting
	// every record. setRecord and deleteRecord keep the two in step.
	recordsOf map[string]map[string]struct{}
	// held is, per fingerprint, the number of occurrences in its pending, failed and in-flight batches (an
	// occurrence in two batches counts twice), and heldTotal their sum; consume holds them to the caps.
	held      map[string]int
	heldTotal int
	// overflowLogged is when occurrences left uncounted at the caps were last logged, and overflowed how
	// many were left uncounted since then.
	overflowLogged time.Time
	overflowed     int
	// droppedThrough is the newest begin time of a record any Incident dropped.
	droppedThrough time.Time
	// names are the Incidents that exist or are pending; their number is held to MaxIncidents.
	names   map[string]struct{}
	pending map[string]*batch
	// failed holds, per Incident, the batches of a status write whose answer never came: the API server may
	// have stored them or not. They stay apart from pending, so the next write can tell from the fresh status
	// whether they were applied (applied) without new observations hiding it.
	failed map[string]*batch
	// kick wakes the writer when a batch is due at once.
	kick chan struct{}
	// nextWrite is when an Incident's pending batch is due: the window after its last write, or RetryAfter
	// after a failed one. An Incident without an entry is written at once; entries past due with nothing to
	// write are pruned.
	nextWrite map[string]time.Time
	refused   *boundedSet
	// waiting holds, per Incident name, when the analysis trigger first saw the Incident waiting only to
	// settle or for budget ([Controller.rememberWaiting]). Only the trigger goroutine uses it.
	waiting map[string]time.Time
	// lastEvent is when the last Warning event at the cap was written.
	lastEvent time.Time
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
	if cfg.EventInterval <= 0 {
		cfg.EventInterval = DefaultEventInterval
	}
	if cfg.StopTimeout <= 0 {
		cfg.StopTimeout = DefaultStopTimeout
	}
	if cfg.MaxPendingOccurrences <= 0 {
		cfg.MaxPendingOccurrences = DefaultMaxPendingOccurrences
	}
	if cfg.MaxPendingOccurrencesPerIncident <= 0 {
		cfg.MaxPendingOccurrencesPerIncident = DefaultMaxPendingOccurrencesPerIncident
	}
	if cfg.Debounce <= 0 {
		cfg.Debounce = DefaultDebounce
	}
	if cfg.SettleLimit <= 0 {
		cfg.SettleLimit = cfg.Window
	}
	if cfg.Cooldown <= 0 {
		cfg.Cooldown = DefaultCooldown
	}
	if cfg.AnalysisTimeout <= 0 {
		cfg.AnalysisTimeout = DefaultAnalysisTimeout
	}
	if cfg.TriggerInterval <= 0 {
		cfg.TriggerInterval = DefaultTriggerInterval
	}
	if cfg.PassTimeout <= 0 {
		cfg.PassTimeout = min(DefaultPassTimeout, cfg.TriggerInterval*9/10)
	}
	if cfg.PassTimeout >= cfg.TriggerInterval {
		return nil, fmt.Errorf("incidents: the trigger pass timeout %s must be shorter than the trigger interval %s",
			cfg.PassTimeout, cfg.TriggerInterval)
	}
	if cfg.ReadTimeout <= 0 {
		cfg.ReadTimeout = min(DefaultReadTimeout, cfg.PassTimeout*3/4)
	}
	if cfg.ReadTimeout >= cfg.PassTimeout {
		return nil, fmt.Errorf("incidents: the budget read timeout %s must be shorter than the trigger pass timeout %s",
			cfg.ReadTimeout, cfg.PassTimeout)
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Controller{
		cfg:       cfg,
		ready:     make(chan struct{}),
		records:   map[string]record{},
		recordsOf: map[string]map[string]struct{}{},
		held:      map[string]int{},
		names:     map[string]struct{}{},
		pending:   map[string]*batch{},
		failed:    map[string]*batch{},
		kick:      make(chan struct{}, 1),
		nextWrite: map[string]time.Time{},
		refused:   newBoundedSet(refusedRemembered),
		waiting:   map[string]time.Time{},
	}, nil
}

// SetupWithManager adds the controller to mgr, consuming obs. It writes with the manager's client, reads an
// Incident before each write with the API reader, so a retry never works from a stale copy, and loads the
// Incidents from the cache at start. It runs only on the leader, like the collector.
func (c *Controller) SetupWithManager(mgr ctrl.Manager, obs <-chan collector.Observation) error {
	c.client, c.reader, c.cache, c.obs = mgr.GetClient(), mgr.GetAPIReader(), mgr.GetClient(), obs
	incidentPhases.setSource(mgr.GetCache(), c.cfg.Namespace)
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

// Start loads the Incidents, then consumes observations until ctx ends. Writes run in a goroutine of their
// own, so a slow API server delays them but not the consumption of observations. When ctx ends (shutdown,
// or a lost leadership), it writes what is pending and what failed, trying again until StopTimeout; see
// [Controller.stop] for what can still be lost.
func (c *Controller) Start(ctx context.Context) error {
	if err := c.load(ctx); err != nil {
		return err
	}
	written, marked := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(written)
		c.writeLoop(ctx)
	}()
	go func() {
		defer close(marked)
		c.triggerLoop(ctx)
	}()
	for {
		select {
		case o := <-c.obs:
			if c.consume(ctx, o) {
				c.wake()
			}
		case <-ctx.Done():
			<-written
			<-marked
			c.stop(ctx)
			return nil
		}
	}
}

// wake tells the writer that a batch is due, without waiting.
func (c *Controller) wake() {
	select {
	case c.kick <- struct{}{}:
	default:
	}
}

// writeLoop writes due batches when woken and on every tick, until ctx ends.
func (c *Controller) writeLoop(ctx context.Context) {
	tick := time.NewTicker(min(max(c.cfg.Window/10, 100*time.Millisecond), 30*time.Second))
	defer tick.Stop()
	for {
		select {
		case <-c.kick:
		case <-tick.C:
		case <-ctx.Done():
			return
		}
		c.flushDue(ctx, false)
	}
}

// stop writes every pending and failed batch within StopTimeout, trying the failed ones again until then.
// Each write reads the Incident fresh from the API server and skips what it already holds, so after a lost
// leadership, when a new leader may already be writing, nothing is added twice by this process. What it
// cannot write in time is logged and counted in shadewright_incident_batches_dropped_total (reason
// "stopped"). Also lost: observations still in the channel, and everything pending when the process is
// killed rather than stopped. A new leader that loaded the Incidents before this stop's writes landed may
// count those occurrences again (an overcount).
func (c *Controller) stop(ctx context.Context) {
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.cfg.StopTimeout)
	defer cancel()
	for {
		c.flushDue(sctx, true)
		if c.unwritten() == 0 || sctx.Err() != nil {
			break
		}
		select {
		case <-time.After(min(c.cfg.RetryAfter, time.Second)):
		case <-sctx.Done():
		}
	}
	if n := c.unwritten(); n > 0 {
		droppedBatches.WithLabelValues(dropStopped).Add(float64(n))
		logf.FromContext(ctx).Error(sctx.Err(), "Stopped with Incident observations not written", "batches", n)
	}
}

// unwritten returns the number of pending and failed batches.
func (c *Controller) unwritten() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.pending) + len(c.failed)
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
		c.setRecord(r.ID, record{counted: higher(cur.counted, counted{total: r.Total, began: timeFrom(r.Began)}), incident: name})
	}
	c.droppedThrough = latest(c.droppedThrough, timeFrom(status.OccurrencesDroppedThrough))
}

// setRecord sets the record of one occurrence and moves it in recordsOf to its Incident. The caller holds
// mu.
func (c *Controller) setRecord(id string, r record) {
	if old, ok := c.records[id]; ok && old.incident != r.incident {
		c.unindex(id, old.incident)
	}
	c.records[id] = r
	ids := c.recordsOf[r.incident]
	if ids == nil {
		ids = map[string]struct{}{}
		c.recordsOf[r.incident] = ids
	}
	ids[id] = struct{}{}
}

// deleteRecord forgets the record of one occurrence. The caller holds mu.
func (c *Controller) deleteRecord(id string) {
	if old, ok := c.records[id]; ok {
		c.unindex(id, old.incident)
		delete(c.records, id)
	}
}

// unindex takes id out of the records of name in recordsOf. The caller holds mu.
func (c *Controller) unindex(id, name string) {
	if ids := c.recordsOf[name]; ids != nil {
		delete(ids, id)
		if len(ids) == 0 {
			delete(c.recordsOf, name)
		}
	}
}

// Handled answers the collector (collector.HandledFunc): the highest total counted for the occurrence by
// any Incident. An occurrence with no record that began no later than the newest record any Incident
// dropped is answered as counted in full (collector.AllHandled), so the bound never counts twice; one
// without a begin time never is, so it may be counted again once its record is dropped. found is false
// otherwise. It answers from memory, and waits until the Incidents are loaded or ctx ends.
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
	// The drop mark answers only for an occurrence with a known begin time: without one it cannot be placed
	// before or after the mark, and a possible overcount is preferred to a count silently lost.
	if began = began.Truncate(time.Second); !began.IsZero() && !c.droppedThrough.IsZero() &&
		!began.After(c.droppedThrough) {
		return collector.AllHandled, true
	}
	return 0, false
}

// consume counts one observation into the pending batch of its Incident. An observation marked MayRepeat
// counts Increase of what Handled would answer. Any other counts its Count (the collector knows that much
// is new), but never more than Increase of the occurrence's record, so an occurrence another process
// already counted is not counted again; the drop bound does not apply to it. A new fingerprint at the cap
// is refused. A new occurrence over the caps on occurrences held unwritten is not counted ([Config]); its
// observation only moves the batch's first and last seen, so the Incident is still written.
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
	_, scheduled := c.nextWrite[o.Fingerprint]
	if _, ok := b.occurrences[o.Occurrence]; !ok {
		if c.full(o.Fingerprint) {
			b.add(&batch{first: at, last: at})
			c.overflow(ctx, o)
			return !scheduled
		}
		c.held[o.Fingerprint]++
		c.heldTotal++
	}
	b.add(&batch{count: n, first: at, last: at, occurrences: map[string]counted{
		o.Occurrence: {total: o.Total, began: o.Began},
	}})
	// An ID the schema refuses is counted but never stored, so it is not remembered either: nothing would
	// ever drop its record from memory.
	if occurrenceID.MatchString(o.Occurrence) {
		cur := c.records[o.Occurrence]
		c.setRecord(o.Occurrence, record{counted: higher(cur.counted, counted{total: o.Total, began: o.Began}), incident: name})
	}
	return !scheduled
}

// full reports whether a new occurrence of fp would go over the caps on occurrences held unwritten. An
// Incident that holds none takes one whatever the total, so no Incident is left without its first
// occurrence. The caller holds mu.
func (c *Controller) full(fp string) bool {
	h := c.held[fp]
	return h >= c.cfg.MaxPendingOccurrencesPerIncident || h > 0 && c.heldTotal >= c.cfg.MaxPendingOccurrences
}

// overflow counts an observation of a new occurrence left uncounted at the caps, and logs at most once per
// overflowLogInterval how many were since the last line. It names the Incident, nothing from the event.
// The caller holds mu.
func (c *Controller) overflow(ctx context.Context, o collector.Observation) {
	overflowedOccurrences.Inc()
	c.overflowed++
	now := c.cfg.Now()
	if !c.overflowLogged.IsZero() && now.Sub(c.overflowLogged) < overflowLogInterval {
		return
	}
	logf.FromContext(ctx).Info("Left new occurrences uncounted at the cap on occurrences held unwritten",
		"incident", Name(o.Fingerprint), "uncounted", c.overflowed, "held", c.held[o.Fingerprint],
		"heldTotal", c.heldTotal, "maxPerIncident", c.cfg.MaxPendingOccurrencesPerIncident,
		"max", c.cfg.MaxPendingOccurrences)
	c.overflowLogged, c.overflowed = now, 0
}

// recount sets held for fp to what its pending and failed batches hold, once no batch of fp is in flight.
// The caller holds mu.
func (c *Controller) recount(fp string) {
	n := 0
	for _, b := range []*batch{c.pending[fp], c.failed[fp]} {
		if b != nil {
			n += len(b.occurrences)
		}
	}
	c.heldTotal += n - c.held[fp]
	if n == 0 {
		delete(c.held, fp)
	} else {
		c.held[fp] = n
	}
}

// refuse counts and reports an observation of a new fingerprint at the cap. It is logged once per
// fingerprint (of the last refusedRemembered), and a Warning event is written for at most one of them per
// EventInterval. Both name the subject and the fingerprint, nothing from the event's text. The caller holds
// mu.
func (c *Controller) refuse(ctx context.Context, o collector.Observation) {
	refusedObservations.Inc()
	if !c.refused.add(o.Fingerprint) {
		return
	}
	s := o.Subject
	logf.FromContext(ctx).Info("Refused a new Incident at the cap", "max", c.cfg.MaxIncidents,
		"fingerprint", o.Fingerprint, "kind", s.Kind, "namespace", s.Namespace, "name", s.Name)
	now := c.cfg.Now()
	if c.cfg.Recorder == nil || c.cfg.Regarding == nil ||
		(!c.lastEvent.IsZero() && now.Sub(c.lastEvent) < c.cfg.EventInterval) {
		return
	}
	c.lastEvent = now
	c.cfg.Recorder.Eventf(c.cfg.Regarding, corev1.EventTypeWarning, "IncidentCapReached",
		"The cap of %d Incidents is reached: not storing a new Incident for %s %s/%s (fingerprint %s); "+
			"further refusals for %s are only logged and counted", c.cfg.MaxIncidents, s.Kind, s.Namespace, s.Name,
		o.Fingerprint, c.cfg.EventInterval)
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

// flushDue writes the pending and failed batches whose Incident was not written within the window (or
// whose retry is due), or all of them. It prunes nextWrite of the Incidents past due with nothing to write.
func (c *Controller) flushDue(ctx context.Context, all bool) {
	c.mu.Lock()
	now := c.cfg.Now()
	for fp, next := range c.nextWrite {
		if !now.Before(next) && c.pending[fp] == nil && c.failed[fp] == nil {
			delete(c.nextWrite, fp)
		}
	}
	var due []string
	isDue := func(fp string) bool {
		next, ok := c.nextWrite[fp]
		return all || !ok || !now.Before(next)
	}
	for fp := range c.pending {
		if isDue(fp) {
			due = append(due, fp)
		}
	}
	for fp := range c.failed {
		if c.pending[fp] == nil && isDue(fp) {
			due = append(due, fp)
		}
	}
	c.mu.Unlock()
	for _, fp := range due {
		c.flush(ctx, fp)
	}
}

// flush writes the failed and the pending batch of one Incident in one status write. Both are taken out
// first, so observations arriving during the write start a new pending batch. Each attempt reads the
// Incident fresh: a conflict is retried on the new version, and a batch the status already holds (a write
// that succeeded but whose answer was lost) is not added twice.
//
// If the write fails, the batches it sent become the failed batch, tried again RetryAfter later; batches it
// never sent go back to pending. A write that can never succeed (the API server refuses it as invalid, or
// the Incident of that name holds another fingerprint) drops its batches, and the memory of their
// occurrences goes back to what the Incident stores, so Handled does not answer "counted" for what was
// never stored.
//
// The batches taken out still count as held while they are in flight, so the observations arriving
// meanwhile cannot take the Incident over its cap; held is set to what is left once the write returns.
func (c *Controller) flush(ctx context.Context, fp string) {
	c.mu.Lock()
	f, p := c.failed[fp], c.pending[fp]
	delete(c.failed, fp)
	delete(c.pending, fp)
	c.mu.Unlock()
	var batches []*batch
	for _, b := range []*batch{f, p} {
		if b != nil {
			batches = append(batches, b)
		}
	}
	if len(batches) == 0 {
		return
	}
	wctx, cancel := context.WithTimeout(ctx, c.cfg.WriteTimeout)
	defer cancel()
	res, err := c.write(wctx, batches)
	c.mu.Lock()
	defer c.mu.Unlock()
	defer c.recount(fp)
	name := Name(fp)
	if refused, ok := errors.AsType[*refusedError](err); ok {
		gone, stored, reason := batches, (*remediationv1alpha1.IncidentStatus)(nil), dropFingerprintConflict
		if !refused.conflict {
			reason = dropInvalid
		}
		if refused.exists && !refused.conflict {
			// The status update was refused: only what it sent is dropped, and the status as read is what
			// the Incident holds.
			gone, stored = []*batch{res.sent}, res.stored
		}
		c.forget(fp, gone, stored)
		// A write before this one whose answer was lost may have dropped records that no batch names: they go
		// too, as after a write that succeeds. What the Incident holds stays: the status as read, or of the
		// Incident with another fingerprint the records load remembered that its status still holds. That
		// status is not ours: it is only swept against, never remembered, so neither its records nor its drop
		// mark enter memory at runtime.
		if refused.conflict {
			c.prune(fp, nil, &res.inc.Status, true)
		} else if stored != nil {
			c.settle(fp, nil, stored, true)
		} else {
			c.prune(fp, nil, &remediationv1alpha1.IncidentStatus{}, true)
		}
		if !refused.exists && c.pending[fp] == nil {
			// An Incident that was never created frees its place under the cap.
			delete(c.names, name)
		}
		c.nextWrite[fp] = c.cfg.Now().Add(c.cfg.Window)
		droppedBatches.WithLabelValues(reason).Add(float64(len(gone)))
		var count int64
		for _, b := range gone {
			count += b.count
		}
		logf.FromContext(ctx).Error(err, "Dropped observations the API server refuses to store", "incident", name,
			"reason", reason, "count", count)
		return
	}
	if err != nil {
		if res.sent != nil {
			// The last attempt sent these batches together and its answer is unknown: they were stored together
			// or not at all, so they are checked together next time.
			c.failed[fp] = res.sent
		} else {
			// Nothing was sent that may have been stored: the failed batch stays apart, and the pending one,
			// never sent, takes the observations that arrived meanwhile.
			if f != nil {
				c.failed[fp] = f
			}
			if p != nil {
				if q := c.pending[fp]; q != nil {
					p.add(q)
				}
				c.pending[fp] = p
			}
		}
		c.nextWrite[fp] = c.cfg.Now().Add(c.cfg.RetryAfter)
		incidentWriteErrors.Inc()
		logf.FromContext(ctx).Error(err, "Failed to write the Incident status, will retry", "incident", name)
		return
	}
	c.nextWrite[fp] = c.cfg.Now().Add(c.cfg.Window)
	c.settle(fp, res.dropped, &res.inc.Status, f != nil)
}

// settle takes into memory the status of fp's Incident after a write that succeeded or was refused: prune
// forgets what the status no longer holds, and its records and drop mark are remembered. The caller holds
// mu.
func (c *Controller) settle(fp string, dropped []string, status *remediationv1alpha1.IncidentStatus, sweep bool) {
	c.prune(fp, dropped, status, sweep)
	c.remember(Name(fp), status)
}

// prune forgets this Incident's records of dropped that status no longer holds, unless a batch still
// unwritten holds them. With sweep (after a failed write, or a refused one), every record of this Incident
// is checked the same way, through recordsOf: a write whose answer was lost may have dropped records that
// neither this write's dropped nor its batches name, and they would otherwise stay in memory for good. It
// only forgets: nothing of status is taken into memory. The caller holds mu.
func (c *Controller) prune(fp string, dropped []string, status *remediationv1alpha1.IncidentStatus, sweep bool) {
	name := Name(fp)
	stored := make(map[string]struct{}, len(status.Occurrences))
	for _, r := range status.Occurrences {
		stored[r.ID] = struct{}{}
	}
	stale := func(id string, r record) bool {
		if r.incident != name {
			return false
		}
		if _, ok := stored[id]; ok {
			return false
		}
		_, pending := c.pending[fp].occurrenceOf(id)
		_, failed := c.failed[fp].occurrenceOf(id)
		return !pending && !failed
	}
	for _, id := range dropped {
		if r, ok := c.records[id]; ok && stale(id, r) {
			c.deleteRecord(id)
		}
	}
	if sweep {
		for id := range c.recordsOf[name] {
			if stale(id, c.records[id]) {
				c.deleteRecord(id)
			}
		}
	}
}

// forget takes the memory of the occurrences of dropped batches back to what stored (the Incident's status,
// or nil when it holds none of them) records, keeping what the current pending batch holds. Only records of
// this Incident are touched. The caller holds mu.
func (c *Controller) forget(fp string, gone []*batch, stored *remediationv1alpha1.IncidentStatus) {
	name := Name(fp)
	kept := map[string]counted{}
	if stored != nil {
		for _, r := range stored.Occurrences {
			kept[r.ID] = counted{total: r.Total, began: timeFrom(r.Began)}
		}
	}
	p := c.pending[fp]
	for _, b := range gone {
		for id := range b.occurrences {
			if r, ok := c.records[id]; !ok || r.incident != name {
				continue
			}
			k, has := kept[id]
			if o, again := p.occurrenceOf(id); again {
				k, has = higher(k, o), true
			}
			if has {
				c.setRecord(id, record{counted: k, incident: name})
			} else {
				c.deleteRecord(id)
			}
		}
	}
}

// writeResult is what write did: the Incident as written, the IDs of the records it dropped, and of its last
// attempt the status as read (stored) and the batches it sent, combined (sent, nil when it sent none).
type writeResult struct {
	inc     *remediationv1alpha1.Incident
	dropped []string
	stored  *remediationv1alpha1.IncidentStatus
	sent    *batch
}

// write creates the Incident if needed and adds to its status each batch it does not already hold, retrying
// on conflict. A conflict or a lost create race means the attempt stored nothing, so only the last attempt's
// answer can be unknown.
func (c *Controller) write(ctx context.Context, batches []*batch) (writeResult, error) {
	spec := batches[0].spec
	key := types.NamespacedName{Namespace: c.cfg.Namespace, Name: Name(spec.Fingerprint)}
	var res writeResult
	err := retry.OnError(retry.DefaultBackoff, retriable, func() error {
		res = writeResult{inc: &remediationv1alpha1.Incident{}}
		err := c.reader.Get(ctx, key, res.inc)
		if apierrors.IsNotFound(err) {
			res.inc = &remediationv1alpha1.Incident{
				ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name},
				Spec:       spec,
			}
			if err = c.client.Create(ctx, res.inc); apierrors.IsInvalid(err) {
				return &refusedError{err: err}
			}
		}
		if err != nil {
			return err
		}
		if res.inc.Spec.Fingerprint != spec.Fingerprint {
			return &refusedError{exists: true, conflict: true,
				err: fmt.Errorf("incident %s holds fingerprint %s, not %s", key.Name, res.inc.Spec.Fingerprint, spec.Fingerprint)}
		}
		var send []*batch
		for _, b := range batches {
			if !applied(&res.inc.Status, b) {
				send = append(send, b)
			}
		}
		if len(send) == 0 {
			return nil
		}
		res.stored = res.inc.Status.DeepCopy()
		res.sent = combine(send)
		for _, b := range send {
			res.dropped = append(res.dropped, merge(&res.inc.Status, b)...)
		}
		if err = c.client.Status().Update(ctx, res.inc); apierrors.IsInvalid(err) {
			return &refusedError{exists: true, err: err}
		}
		return err
	})
	if err != nil {
		return res, fmt.Errorf("writing Incident %s: %w", key.Name, err)
	}
	return res, nil
}

// refusedError is a write that can never succeed: the API server refuses the object as invalid, or the
// Incident of that name holds another fingerprint (conflict). exists says whether the Incident exists.
type refusedError struct {
	err              error
	exists, conflict bool
}

func (e *refusedError) Error() string { return e.err.Error() }
func (e *refusedError) Unwrap() error { return e.err }

// retriable reports whether a write is tried again at once on a fresh read: a conflict, or a create that
// lost a race to another writer.
func retriable(err error) bool {
	return apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err)
}
