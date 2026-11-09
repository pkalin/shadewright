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
	"maps"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
)

// DefaultTriggerInterval is how often the controller decides again which Incidents are due.
const DefaultTriggerInterval = 10 * time.Second

// triggerFieldOwner is the field manager of the analysis trigger's writes (the AnalysisDue condition and the
// budget ConfigMap), so the managed fields tell them from the observation writes.
const triggerFieldOwner = "shadewright-analysis-trigger"

// triggerLoop marks the Incidents due for analysis every TriggerInterval until ctx ends. Each pass runs under
// PassTimeout, shorter than the interval: ctx lasts as long as the controller, so a call that stalls without
// a deadline would stop every later pass.
func (c *Controller) triggerLoop(ctx context.Context) {
	tick := time.NewTicker(c.cfg.TriggerInterval)
	defer tick.Stop()
	for {
		if err := c.pass(ctx); err != nil && ctx.Err() == nil {
			triggerPassErrors.Inc()
			logf.FromContext(ctx).Error(err, "Failed a pass of the analysis trigger (closure rules or due marks), will retry")
		}
		select {
		case <-tick.C:
		case <-ctx.Done():
			return
		}
	}
}

// pass reports how near the Incident cap is ([Controller.reportCap], from memory), runs the closure rules
// ([Controller.closeIncidents]) within the first half of PassTimeout, then one [Controller.markDue] within the
// rest, so a closure pass that runs into its deadline never takes the marks' time. A failure of one does not
// stop the other; the joined error names the Incidents whose writes failed.
func (c *Controller) pass(ctx context.Context) error {
	pctx, cancel := context.WithTimeout(ctx, c.cfg.PassTimeout)
	defer cancel()
	c.reportCap(pctx)
	cctx, cancelClose := context.WithTimeout(pctx, c.cfg.PassTimeout/2)
	closeErr := c.closeIncidents(cctx)
	cancelClose()
	if pctx.Err() != nil {
		return errors.Join(closeErr, pctx.Err())
	}
	return errors.Join(closeErr, c.markDue(pctx))
}

// markDue decides, for every Incident in the namespace, whether it is due for analysis ([Due]) and sets its
// AnalysisDue condition where that changed. The budget used today is read from the budget ConfigMap
// ([UsedToday]); each Incident marked due takes one more unit of it, so no more Incidents are due at once than
// the budget has left. Those already due go first, so the same ones stay due from one pass to the next; then
// the oldest by firstSeen, so a flood of new fingerprints does not push older incidents out of the budget.
//
// It fails closed: without the RemediationSettings object or the budget ConfigMap, or with a budget that
// cannot be trusted, nothing is due; when either cannot be read, the Incidents marked due are set not due and
// the pass returns the error. An Incident whose name does not belong to its fingerprint is not decided: if it
// is marked due, it is set not due. Before deciding, it updates what the controller remembers of each
// Incident ([Controller.watch]); the NotRecent writes come last and are bounded per pass.
func (c *Controller) markDue(ctx context.Context) error {
	var list remediationv1alpha1.IncidentList
	if err := c.cache.List(ctx, &list, client.InNamespace(c.cfg.Namespace)); err != nil {
		// A pass that looked at no Incident breaks every run of passes that watched one, and the lastSeen read
		// before it says nothing about a spell between it and the next pass that lists.
		for _, t := range c.memory {
			t.watchedSince = time.Time{}
			t.lastSeen = time.Time{}
		}
		return fmt.Errorf("listing the Incidents: %w", err)
	}
	c.noteStates(list.Items)
	// The requests whose reviewer hints make a Proposed Incident due (ADR-0028). A failed listing fails
	// closed for the hints only: no hint makes an Incident due in this pass, and the error is returned.
	var hintErr error
	var requests remediationv1alpha1.RemediationRequestList
	if err := c.cache.List(ctx, &requests, client.InNamespace(c.cfg.Namespace)); err != nil {
		c.hintRequests = nil
		hintErr = fmt.Errorf("listing the RemediationRequests for reviewer hints: %w", err)
	} else {
		c.hintRequests = requests.Items
	}
	now := c.cfg.Now()
	s := TriggerSettings{Debounce: c.cfg.Debounce, SettleLimit: c.cfg.SettleLimit, Cooldown: c.cfg.Cooldown,
		AnalysisTimeout: c.cfg.AnalysisTimeout, RecentWindow: c.cfg.RecentWindow}
	c.watch(list.Items, now, s)
	used, closed, readErr := c.readBudget(ctx, now, &s)

	items := list.Items
	slices.SortFunc(items, dueOrder)
	var errs []error
	// skips are the Incidents this pass decides NotRecent over another reason: their writes come after every
	// other decision, at most notRecentWritesPerPass of them, so a backlog of quiet Incidents (the first pass
	// after an upgrade, late settings or a budget reset) neither holds up the marks that say due nor runs the
	// pass into its deadline. The decision is made on the cached copy with the observations held unwritten,
	// like every other, so an Incident seen again is no skip and is decided in its place in dueOrder.
	var skips []*remediationv1alpha1.Incident
	for i := range items {
		inc := &items[i]
		wasDue := meta.IsStatusConditionTrue(inc.Status.Conditions, ConditionAnalysisDue)
		var decide func(*remediationv1alpha1.Incident) (bool, string)
		switch {
		case !inc.HasDerivedName():
			// Not an Incident this controller writes (another fingerprint holds the name): only a mark that says
			// due is taken back.
			if !wasDue {
				continue
			}
			decide = func(*remediationv1alpha1.Incident) (bool, string) { return false, ReasonForeignFingerprint }
		case closed != "":
			if _, outcome := outcomeReason(inc); readErr != nil && !wasDue && (!outcome || !saysAnalyzing(inc)) {
				// Without the settings or the budget nothing else changes: the marks that say due are taken
				// back, and the outcome of a run that ended replaces Analyzing, no more.
				continue
			}
			// The outcome of a run that ended is not a budget's state: Proposed or Escalated shows it, as
			// [Due] gives it with a known budget.
			decide = func(fresh *remediationv1alpha1.Incident) (bool, string) {
				if r, ok := outcomeReason(fresh); ok {
					return false, r
				}
				return false, closed
			}
		default:
			decide = c.decider(inc, now, s, &used)
		}
		due, reason := decide(inc)
		if closed == "" && reason == ReasonNotRecent &&
			!conditionAgrees(inc, due, reason, runEnded(inc, now, c.cfg.AnalysisTimeout)) {
			// A NotRecent decision takes no unit of the budget, so its write can wait for the others. A mark
			// that says due but is no mark ([markedDue]: written for another generation) still says due
			// until it is written over, so it keeps its unit meanwhile.
			c.rememberWaiting(inc.UID, reason, now)
			skips = append(skips, inc)
			if wasDue {
				used++
			}
			continue
		}
		due, reason, wrote, err := c.setDue(ctx, inc, now, due, reason, decide, "")
		if closed == "" {
			c.rememberWaiting(inc.UID, reason, now)
		}
		if err := c.account(inc, due, reason, wrote, wasDue, err, &used); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			errs = append(errs, err)
		}
	}
	writes := 0
	for _, inc := range skips {
		if writes == notRecentWritesPerPass {
			// The rest are NotRecent too, so the next pass, deciding them again, loses nothing.
			break
		}
		// Only NotRecent is written here: a fresh copy that says otherwise (seen again since the cache was
		// filled) is left to the next pass, which decides it in its place in dueOrder.
		wasDue := meta.IsStatusConditionTrue(inc.Status.Conditions, ConditionAnalysisDue)
		_, _, wrote, err := c.setDue(ctx, inc, now, false, ReasonNotRecent, c.decider(inc, now, s, &used), ReasonNotRecent)
		if wrote || err != nil {
			writes++
		}
		if err := c.account(inc, false, ReasonNotRecent, wrote, wasDue, err, nil); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			errs = append(errs, err)
		}
	}
	if readErr != nil {
		errs = append([]error{readErr}, errs...)
	}
	if hintErr != nil {
		errs = append([]error{hintErr}, errs...)
	}
	if len(errs) > 0 {
		return fmt.Errorf("deciding which Incidents are due, %d errors, first: %w", len(errs), errs[0])
	}
	return nil
}

// notRecentWritesPerPass bounds the NotRecent conditions one pass writes, each a status update and an event:
// a backlog of quiet Incidents is written over the next passes instead.
const notRecentWritesPerPass = 20

// trace is what the analysis trigger remembers of one Incident between passes (see [Memory]): when it first
// found it waiting only to settle or for budget ([Controller.rememberWaiting]), whether it found it gone
// quiet, when the passes in a row that looked at it began, and the lastSeen its status held on the last pass
// that read one, when that pass ran and whether its newest occurrence was a lasting state then
// ([Controller.watch]).
type trace struct {
	waiting      time.Time
	quiet        bool
	watchedSince time.Time
	lastSeen     time.Time
	sampled      time.Time
	lasting      bool
}

// decider returns the decision of [Due] for one Incident, with what the controller knows of it ([Memory])
// and the budget used so far in this pass (*used, read when called). Whether a reviewer hint waits for it
// ([Memory].Hinted) is decided on the copy decided, from the requests of the pass, since an answered hint
// depends on the Incident's own status.
func (c *Controller) decider(inc *remediationv1alpha1.Incident, now time.Time, s TriggerSettings,
	used *int) func(*remediationv1alpha1.Incident) (bool, string) {
	m := Memory{Held: c.heldSince(inc.Spec.Fingerprint)}
	if t := c.memory[inc.UID]; t != nil {
		m.Waiting, m.WentQuiet, m.WatchedSince = t.waiting, t.quiet, t.watchedSince
	}
	return func(fresh *remediationv1alpha1.Incident) (bool, string) {
		m := m
		m.Hinted = PendingHint(fresh, c.hintRequestsOf(fresh)) != nil
		return Due(fresh, m, now, s, *used)
	}
}

// account reports a NotRecent that setDue wrote, counts a unit of the budget in *used (when used is not nil)
// for a mark that says due, and returns err: a failed write may still have landed (a lost answer), and a
// mark it did not change still says due (wasDue), so either way the Incident takes its unit.
func (c *Controller) account(inc *remediationv1alpha1.Incident, due bool, reason string, wrote, wasDue bool, err error,
	used *int) error {
	if wrote && reason == ReasonNotRecent {
		c.reportNotRecent(inc)
	}
	if used != nil && (due || (err != nil && wasDue)) {
		*used++
	}
	return err
}

// watch updates what the controller remembers of the Incidents between passes, before anything is decided,
// on every pass (one that cannot read the budget included), from the cached list: when the passes in a row
// that looked at each began ([Memory].WatchedSince), and which ones it found gone quiet ([goneQuiet],
// [Memory].WentQuiet). When the stored lastSeen is not stale itself, a spell that ended between two passes
// counts when it is more than the recent window after the one read on the previous pass; it begins at that
// previous one. That previous lastSeen is used only when its pass ran within the recent window before now (a
// stalled or paused process did not sample the activity in between) and its newest occurrence was no lasting
// state then (a frozen lastSeen while the state lasted is no quiet spell). Finding one gone quiet forgets when
// it was first seen waiting if that was not after the spell's begin plus the recent window: such a wait
// belongs to the spell before. A wait from after it (the new burst seen again, its observation held while its
// write is pending or failing) is kept, so its settle limit keeps counting. An Incident analyzed since is no
// longer remembered as gone quiet; one not listed, or held by another fingerprint, is forgotten.
func (c *Controller) watch(items []remediationv1alpha1.Incident, now time.Time, s TriggerSettings) {
	listed := make(map[types.UID]struct{}, len(items))
	for i := range items {
		inc := &items[i]
		if !inc.HasDerivedName() {
			continue
		}
		listed[inc.UID] = struct{}{}
		t := c.memory[inc.UID]
		if t == nil {
			t = &trace{}
			c.memory[inc.UID] = t
		}
		if t.watchedSince.IsZero() {
			t.watchedSince = now
		}
		var since time.Time
		quiet := false
		if inc.Status.LastSeen != nil {
			cur := inc.Status.LastSeen.Time
			since, quiet = cur, goneQuiet(inc, cur, now, s)
			// A spell that ended between two passes: the lastSeen read on the previous pass was stale when the
			// new one was seen, so the gap itself was longer than the window. A shorter gap is no spell, however
			// late the pass that reads it. A stored lastSeen stale itself begins the later spell, so it wins.
			// Only the newest observation is stored, so within one pass interval of the window this is a guess:
			// observations landing together can hide one that broke the gap. It errs towards a spell, which
			// only delays the first analysis (another debounce), never brings it forward.
			prev := t.lastSeen
			if now.After(t.sampled.Add(recentWindow(s))) || t.lasting {
				prev = time.Time{}
			}
			if !quiet && !prev.IsZero() && prev.Before(cur) && goneQuiet(inc, prev, cur, s) {
				since, quiet = prev, true
			}
			t.lastSeen, t.sampled, t.lasting = cur, now, lastingState(&inc.Status)
		}
		switch {
		case quiet:
			t.quiet = true
			if !t.waiting.After(since.Add(recentWindow(s))) {
				t.waiting = time.Time{}
			}
		case inc.Status.LastAnalysisTime != nil || inc.Status.Phase == remediationv1alpha1.IncidentAnalyzing:
			t.quiet = false
		}
	}
	maps.DeleteFunc(c.memory, func(uid types.UID, _ *trace) bool {
		_, ok := listed[uid]
		return !ok
	})
}

// readBudget reads the daily limit from the RemediationSettings (into s, see [MaxAnalysesPerDay]) and the
// analyses used today from the budget ConfigMap. closed is the not-due reason of every Incident when the
// budget is unknown, settings without a limit included, and empty otherwise; readErr is set when a read
// failed for another reason than a missing object. Both reads together
// run under ReadTimeout, shorter than the pass, so a read that stalls leaves the rest of the pass to take the
// marks that say due back.
//
// It also reports the budget in the gauges analysesPerDayLimit and analysesUsedToday: each is set from what was
// read and cleared when its read failed or could not be trusted, so neither shows a value this pass does not
// go by.
func (c *Controller) readBudget(ctx context.Context, now time.Time, s *TriggerSettings) (used int, closed string, readErr error) {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.ReadTimeout)
	defer cancel()
	var settings remediationv1alpha1.RemediationSettings
	switch err := c.cache.Get(ctx, types.NamespacedName{Name: remediationv1alpha1.SettingsName}, &settings); {
	case apierrors.IsNotFound(err):
		analysesPerDayLimit.clear()
		analysesUsedToday.clear()
		return 0, ReasonSettingsNotFound, nil
	case err != nil:
		analysesPerDayLimit.clear()
		analysesUsedToday.clear()
		return 0, ReasonSettingsUnreadable, fmt.Errorf("reading RemediationSettings %s: %w", remediationv1alpha1.SettingsName, err)
	}
	limit, ok := MaxAnalysesPerDay(&settings)
	if !ok {
		analysesPerDayLimit.clear()
		analysesUsedToday.clear()
		return 0, ReasonSettingsIncomplete, nil
	}
	s.MaxAnalysesPerDay = limit
	analysesPerDayLimit.set(float64(s.MaxAnalysesPerDay))
	// The ConfigMap is read from the API server on every pass: the analyzer may get it by name only, so there
	// is no informer on it.
	var cm corev1.ConfigMap
	switch err := c.reader.Get(ctx, types.NamespacedName{Namespace: c.cfg.Namespace, Name: BudgetName}, &cm); {
	case apierrors.IsNotFound(err):
		analysesUsedToday.clear()
		return 0, ReasonBudgetNotFound, nil
	case err != nil:
		analysesUsedToday.clear()
		return 0, ReasonBudgetReadFailed, fmt.Errorf("reading ConfigMap %s/%s: %w", c.cfg.Namespace, BudgetName, err)
	}
	used, ok = UsedToday(&cm, now)
	if !ok {
		analysesUsedToday.clear()
		return 0, ReasonBudgetUnreadable, nil
	}
	analysesUsedToday.set(float64(used))
	return used, "", nil
}

// dueOrder is the order in which a pass decides the Incidents, so the budget left goes to them in that order:
// those marked due first, then the oldest by firstSeen (an Incident without one last), then by name.
func dueOrder(a, b remediationv1alpha1.Incident) int {
	ad, bd := meta.IsStatusConditionTrue(a.Status.Conditions, ConditionAnalysisDue),
		meta.IsStatusConditionTrue(b.Status.Conditions, ConditionAnalysisDue)
	if ad != bd {
		if ad {
			return -1
		}
		return 1
	}
	af, bf := a.Status.FirstSeen, b.Status.FirstSeen
	switch {
	case af != nil && bf == nil:
		return -1
	case af == nil && bf != nil:
		return 1
	case af != nil && !af.Equal(bf):
		return af.Compare(bf.Time)
	}
	return strings.Compare(a.Name, b.Name)
}

// rememberWaiting keeps when the controller first saw an Incident waiting only to settle or for budget (see
// [Due]), and forgets it once the Incident waits for data again (analyzed, cooling down, or in a phase that is
// not due). Only an Incident [Controller.watch] remembers is kept.
func (c *Controller) rememberWaiting(uid types.UID, reason string, now time.Time) {
	t := c.memory[uid]
	switch {
	case t == nil:
	case waitsForData(reason):
		t.waiting = time.Time{}
	case t.waiting.IsZero():
		t.waiting = now
	}
}

// heldSince returns the newest observation of the Incident of fp that the controller holds unwritten, in a
// pending or a failed batch, or zero.
func (c *Controller) heldSince(fp string) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.heldLocked(fp)
}

// heldLocked is [Controller.heldSince] for a caller that holds mu.
func (c *Controller) heldLocked(fp string) time.Time {
	var t time.Time
	for _, b := range []*batch{c.pending[fp], c.failed[fp]} {
		if b != nil {
			t = latest(t, b.last)
		}
	}
	return t
}

// reportNotRecent counts an Incident newly marked NotRecent and reports it in a Normal event about the
// Incident, so a skipped analysis is visible. It runs once per write of that reason ([conditionAgrees]), not on
// every pass: the condition keeps NotRecent until the Incident is next marked due, so a second quiet spell
// before that is neither written nor counted again. The event is written in the operator namespace, the
// Incident's, with the grant on core Events the analyzer holds there; its message holds nothing from the
// cluster. It names the window [Due] applies ([recentWindow]).
func (c *Controller) reportNotRecent(inc *remediationv1alpha1.Incident) {
	notRecentSkips.Inc()
	if c.cfg.Recorder == nil {
		return
	}
	c.cfg.Recorder.Eventf(inc, corev1.EventTypeNormal, "AnalysisSkipped",
		"Not analyzed: never analyzed and not seen within the recent window of %s; a new observation makes it due again once it has settled",
		recentWindow(TriggerSettings{RecentWindow: c.cfg.RecentWindow, Debounce: c.cfg.Debounce}))
}

// setDue sets the AnalysisDue condition of one Incident to what decide says. cached is the Incident as the
// cache holds it and (due, reason) what decide said of it: when its condition already agrees
// ([conditionAgrees]), nothing is written. Otherwise the Incident is read fresh from the API server, decided
// again on that copy, and its status updated, retrying on conflict (the observation writer updates the same
// status). With only set, a fresh decision of another reason is not written (reported, wrote false). It
// reports the decision it last made, also with an error, since a write whose answer was lost may have
// landed, and whether it wrote that decision (an answer received, not lost); an Incident gone meanwhile is
// reported not due (NotObserved). now is the pass's clock, against which [runEnded] places the analysis
// timeout.
func (c *Controller) setDue(ctx context.Context, cached *remediationv1alpha1.Incident, now time.Time, due bool, reason string,
	decide func(*remediationv1alpha1.Incident) (bool, string), only string) (bool, string, bool, error) {
	if ref, _ := HintAnswer(cached, c.hintRequestsOf(cached), due, reason); ref == nil &&
		conditionAgrees(cached, due, reason, runEnded(cached, now, c.cfg.AnalysisTimeout)) {
		return due, reason, false, nil
	}
	wrote := false
	wctx, cancel := context.WithTimeout(ctx, c.cfg.WriteTimeout)
	defer cancel()
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		inc := &remediationv1alpha1.Incident{}
		if err := c.reader.Get(wctx, types.NamespacedName{Namespace: c.cfg.Namespace, Name: cached.Name}, inc); err != nil {
			return err
		}
		due, reason = decide(inc)
		wrote = false
		// A reviewer hint refused without an analysis is answered in the same update (ADR-0028 section 3),
		// also when the condition already agrees: the answer is what the executor reads.
		ref, outcome := HintAnswer(inc, c.hintRequestsOf(inc), due, reason)
		if (only != "" && reason != only) ||
			(ref == nil && conditionAgrees(inc, due, reason, runEnded(inc, now, c.cfg.AnalysisTimeout))) {
			return nil
		}
		if ref != nil {
			answerHint(inc, *ref, outcome, c.cfg.Now())
		}
		status := metav1.ConditionFalse
		if due {
			status = metav1.ConditionTrue
		}
		// The transition time comes from the controller's clock, like every decision; SetStatusCondition keeps
		// the old one when only the reason changes.
		meta.SetStatusCondition(&inc.Status.Conditions, metav1.Condition{
			Type:               ConditionAnalysisDue,
			Status:             status,
			Reason:             reason,
			Message:            dueMessages[reason],
			ObservedGeneration: inc.Generation,
			LastTransitionTime: metav1.NewTime(c.cfg.Now()),
		})
		if err := c.client.Status().Update(wctx, inc, client.FieldOwner(triggerFieldOwner)); err != nil {
			return err
		}
		wrote = true
		return nil
	})
	if apierrors.IsNotFound(err) {
		return false, ReasonNotObserved, false, nil
	}
	if err != nil {
		return due, reason, false, fmt.Errorf("setting AnalysisDue on Incident %s: %w", cached.Name, err)
	}
	return due, reason, wrote, nil
}

// conditionAgrees reports whether the Incident's AnalysisDue condition already holds the decision (due,
// reason). A condition that says due must hold the same reason. One that says not due agrees with any
// not-due reason: the reason it gave first stays, so a change that turns every Incident's reason (the budget
// used up, or set to 0) writes nothing. NotRecent is the exception: it is written over another not-due reason,
// once, so the condition shows why an incident is skipped (with an event, [Controller.reportNotRecent]), and
// once written it stays while the incident is not due. A decision reads it back only to count the settle
// limit from the controller's first sight of the incident seen again ([unsettledSince]). Analyzing, which the
// analysis writes when it claims the incident, is the other exception (#424): it agrees with another not-due
// reason only while the run lasts; once the run has ended (ended, see [runEnded]) any other reason is written
// over it, so the condition no longer says an analysis is running.
func conditionAgrees(inc *remediationv1alpha1.Incident, due bool, reason string, ended bool) bool {
	cond := DueMark(inc)
	if cond == nil {
		return false
	}
	switch {
	case due:
		return cond.Status == metav1.ConditionTrue && cond.Reason == reason
	case reason == ReasonNotRecent:
		return cond.Status == metav1.ConditionFalse && cond.Reason == ReasonNotRecent
	case cond.Reason == ReasonAnalyzing && reason != ReasonAnalyzing:
		return cond.Status == metav1.ConditionFalse && !ended
	}
	return cond.Status == metav1.ConditionFalse
}

// runEnded reports whether the incident's last analysis run has ended at now: the phase is no longer
// Analyzing (the run wrote its outcome, or the status was changed), the run has reached the analysis timeout
// since lastAnalysisTime and is taken as lost, or there is no lastAnalysisTime to place it by.
func runEnded(inc *remediationv1alpha1.Incident, now time.Time, timeout time.Duration) bool {
	st := &inc.Status
	if st.Phase != remediationv1alpha1.IncidentAnalyzing || st.LastAnalysisTime == nil {
		return true
	}
	return !now.Before(st.LastAnalysisTime.Add(timeout))
}

// outcomeReason returns the not-due reason of an incident whose last run wrote its outcome (phase Proposed or
// Escalated), or that is Resolved, as [Due] gives it, and whether there is one.
func outcomeReason(inc *remediationv1alpha1.Incident) (string, bool) {
	switch inc.Status.Phase {
	case remediationv1alpha1.IncidentProposed:
		return ReasonProposed, true
	case remediationv1alpha1.IncidentEscalated:
		return escalatedReason(inc), true
	case remediationv1alpha1.IncidentResolved:
		return ReasonResolved, true
	}
	return "", false
}

// saysAnalyzing reports whether the incident's AnalysisDue condition is the claim's: not due, Analyzing.
func saysAnalyzing(inc *remediationv1alpha1.Incident) bool {
	cond := meta.FindStatusCondition(inc.Status.Conditions, ConditionAnalysisDue)
	return cond != nil && cond.Status == metav1.ConditionFalse && cond.Reason == ReasonAnalyzing
}

// dueMessages are the fixed messages of the AnalysisDue condition. They hold nothing from the cluster.
var dueMessages = map[string]string{
	ReasonFirstAnalysis:        "The incident was never analyzed and has settled",
	ReasonNewOccurrence:        "An occurrence began after the last analysis and the cooldown has passed",
	ReasonCooldownExpired:      "The symptom recurred and the cooldown since the last analysis has passed",
	ReasonAnalysisTimedOut:     "The last analysis did not finish within the analysis timeout and the cooldown has passed",
	ReasonReviewerHint:         "A request naming the incident holds a reviewer hint no analysis has answered, and the cooldown has passed",
	ReasonNotObserved:          "No observation is recorded yet",
	ReasonNotRecent:            "The incident was never analyzed and was not seen within the recent window; a new observation makes it due again once it has settled",
	ReasonAnalyzing:            "An analysis is running",
	ReasonAnalysisStartUnknown: "The incident is in phase Analyzing without a lastAnalysisTime, so the cooldown cannot be placed",
	ReasonProposed:             "The last analysis made a proposal",
	ReasonEscalated:            "The last analysis handed the incident to a person",
	ReasonResolved:             "The incident is Resolved and is never analyzed again",
	ReasonRecurred:             "The symptom came back after the incident was resolved; a person decides, no analysis runs",
	ReasonNoNewData:            "Nothing was observed since the last analysis",
	ReasonCoolingDown:          "The cooldown since the last analysis has not passed",
	ReasonDebouncing:           "Waiting for the incident to settle, at most the settle limit after its first unsettled observation",
	ReasonAnalysisDisabled:     "RemediationSettings spec.analysis.maxAnalysesPerDay, or the deprecated spec.maxAnalysesPerDay, is 0",
	ReasonDailyBudgetReached:   "The analyses of the current UTC day reached the daily limit of the RemediationSettings",
	ReasonBudgetUnreadable:     "The analysis budget ConfigMap cannot be trusted, so the budget is taken as spent",
	ReasonBudgetNotFound:       "The analysis budget ConfigMap does not exist, so the daily budget is unknown",
	ReasonBudgetReadFailed:     "The analysis budget ConfigMap could not be read, so the daily budget is unknown",
	ReasonSettingsNotFound:     "The RemediationSettings object does not exist, so the daily budget is unknown",
	ReasonSettingsUnreadable:   "The RemediationSettings object could not be read, so the daily budget is unknown",
	ReasonSettingsIncomplete:   "The RemediationSettings object sets no spec.analysis.maxAnalysesPerDay, so the daily budget is unknown",
	ReasonForeignFingerprint:   "The Incident's name belongs to another fingerprint",
}
