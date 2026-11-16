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
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
)

// The late-cause fold (ADR-0039 points 12 to 15 and 23): when a cause Incident (status.cause) is created, a
// consequence Incident of the same subject and container that opened at most RecurWithin before it, and none
// of whose requests reached a pull request, is closed in its favour: Resolved, Lifecycle FoldedIntoCause and
// status.foldedInto naming the cause's Incident.

// kubeletController is the reporting controller of the kubelet's Events, the only source of a consequence
// Incident (ADR-0039 point 12).
const kubeletController = "kubelet"

// errSelfFold is a fold of an Incident into itself. The schema cannot refuse it, so the controller does.
var errSelfFold = errors.New("an Incident is never folded into itself")

// consequenceIncident reports whether inc is itself a consequence (ADR-0039 point 12): its reason is BackOff
// or Unhealthy and the kubelet reported it. An Incident of a non-zero exit termination read from pod status,
// or of a Failed Event, is a symptom of its own and never folds.
func consequenceIncident(inc *remediationv1alpha1.Incident) bool {
	_, ok := absorbedReason(inc.Spec.Reason)
	return ok && inc.Spec.ReportingController == kubeletController
}

// reachedPullRequest reports whether rr has reached a pull request: the condition PullRequestOpened True, a
// pull request or merge recorded, or a phase that only follows one. A request in doubt counts as reached, so
// the fold never takes a pull request's Incident away (ADR-0039 point 14).
func reachedPullRequest(rr *remediationv1alpha1.RemediationRequest) bool {
	if meta.IsStatusConditionTrue(rr.Status.Conditions, remediationv1alpha1.ConditionPROpened) ||
		rr.Status.PullRequest != nil || rr.Status.MergeReceipt != nil {
		return true
	}
	switch rr.Status.Phase {
	case remediationv1alpha1.PhasePRCreated, remediationv1alpha1.PhaseAwaitingApproval,
		remediationv1alpha1.PhaseAutoMergeQueued, remediationv1alpha1.PhaseMerged,
		remediationv1alpha1.PhaseWaitingForDelivery, remediationv1alpha1.PhaseVerifying,
		remediationv1alpha1.PhaseSucceeded, remediationv1alpha1.PhaseInconclusive:
		return true
	}
	return false
}

// foldable decides whether inc is folded into cause (ADR-0039 points 12 to 14), from the two Incidents and the
// RemediationRequests of the namespace; only those whose incidentRef names inc, by name and UID, count.
//
//   - cause has status.cause and is not inc itself.
//   - inc is a consequence ([consequenceIncident]), not a cause, not folded and not Resolved (a Resolved
//     Incident is closed already, and its resolvedTime cannot change).
//   - Both name the same subject UID and share a container in status.containers.
//   - inc was created at most RecurWithin before cause, by their creationTimestamps: both stamped by the API
//     server, so one clock decides, also for a cause first seen much earlier and opened from the gate.
//   - No request of inc reached a pull request ([reachedPullRequest]).
func foldable(inc, cause *remediationv1alpha1.Incident, requests []remediationv1alpha1.RemediationRequest) bool {
	if !cause.Status.Cause || inc.Name == cause.Name || inc.UID == cause.UID || !inc.HasDerivedName() ||
		!consequenceIncident(inc) || inc.Status.Cause || inc.Status.FoldedInto != nil ||
		inc.Status.Phase == remediationv1alpha1.IncidentResolved {
		return false
	}
	if inc.Spec.Subject.UID == "" || inc.Spec.Subject.UID != cause.Spec.Subject.UID {
		return false
	}
	if !slices.ContainsFunc(inc.Status.Containers, func(n remediationv1alpha1.Name) bool {
		return slices.Contains(cause.Status.Containers, n)
	}) {
		return false
	}
	if inc.CreationTimestamp.IsZero() || cause.CreationTimestamp.IsZero() {
		return false
	}
	before := cause.CreationTimestamp.Sub(inc.CreationTimestamp.Time)
	if before < 0 || before > RecurWithin {
		return false
	}
	for i := range requests {
		rr := &requests[i]
		if rr.Spec.IncidentRef.Name == inc.Name && types.UID(rr.Spec.IncidentRef.UID) == inc.UID &&
			reachedPullRequest(rr) {
			return false
		}
	}
	return true
}

// fold closes inc in favour of cause at now: Resolved with resolvedTime, the Lifecycle condition
// FoldedIntoCause and status.foldedInto naming cause by name and UID, in one status write. It refuses to fold
// an Incident into itself ([errSelfFold]).
func fold(inc, cause *remediationv1alpha1.Incident, now time.Time) error {
	if inc.Name == cause.Name || inc.UID == cause.UID {
		return fmt.Errorf("folding Incident %s: %w", inc.Name, errSelfFold)
	}
	applyClosure(inc, remediationv1alpha1.IncidentResolved, ReasonFoldedIntoCause, now)
	inc.Status.FoldedInto = &remediationv1alpha1.IncidentRef{Name: cause.Name, UID: remediationv1alpha1.UID(cause.UID)}
	return nil
}

// owedFold is a cause Incident whose fold is not done yet: when it may be tried, and a sequence number that
// tells a fold owed again during an attempt from the one that attempt took.
type owedFold struct {
	at  time.Time
	seq uint64
}

// oweFold notes that the cause Incident name was created or became a cause, so its fold is owed. The caller
// holds mu.
func (c *Controller) oweFold(name string) {
	c.foldSeq++
	c.folds[name] = owedFold{seq: c.foldSeq}
}

// oweFoldAfter owes the fold of the Incident name after a status write that created it as a cause or set its
// status.cause (the status it read had none, and the one written has it), or whose answer was lost while it
// sent a batch that sets it: the write may have stored it, and the next write would then not tell. A write
// the API server refused stores nothing and owes nothing. The caller holds mu.
func (c *Controller) oweFoldAfter(name string, res writeResult, err error) {
	if res.stored == nil || res.stored.Cause {
		return
	}
	if (err == nil && res.inc.Status.Cause) || (err != nil && res.sent != nil && res.sent.cause) {
		c.oweFold(name)
	}
}

// foldDue tries every owed fold that is due, in the write loop's pass after the writes that created the cause
// Incidents. A fold that fails is owed again RetryAfter later; one done, or with nothing left to fold, is
// dropped. The retry ends when it succeeds, the cause's Incident is gone or is not a cause, or the process
// stops: an owed fold lives in memory only, so a restart between the write that created the cause and its
// fold leaves the consequence Incident unfolded, with its own analysis.
func (c *Controller) foldDue(ctx context.Context) {
	c.mu.Lock()
	now := c.cfg.Now()
	due := map[string]uint64{}
	for name, f := range c.folds {
		if !now.Before(f.at) {
			due[name] = f.seq
		}
	}
	c.mu.Unlock()
	for _, name := range slices.Sorted(maps.Keys(due)) {
		err := c.foldInto(ctx, name)
		c.mu.Lock()
		if f, ok := c.folds[name]; ok && f.seq == due[name] {
			if err != nil {
				c.folds[name] = owedFold{at: c.cfg.Now().Add(c.cfg.RetryAfter), seq: f.seq}
			} else {
				delete(c.folds, name)
			}
		}
		c.mu.Unlock()
		if err != nil {
			incidentWriteErrors.Inc()
			logf.FromContext(ctx).Error(err, "Failed to fold consequence Incidents into their cause's Incident, will retry",
				"incident", name)
			if ctx.Err() != nil {
				return
			}
		}
	}
}

// foldInto folds into the cause Incident name every consequence Incident the controller's own list shows as
// [foldable], each decided again on the Incident read live and the requests listed live ([Controller.foldOne]).
// A cause Incident gone, or not a cause, has nothing to fold. A failed read is an error, retried.
func (c *Controller) foldInto(ctx context.Context, name string) error {
	wctx, cancel := context.WithTimeout(ctx, c.cfg.WriteTimeout)
	defer cancel()
	cause := &remediationv1alpha1.Incident{}
	err := c.reader.Get(wctx, types.NamespacedName{Namespace: c.cfg.Namespace, Name: name}, cause)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading the cause Incident %s: %w", name, err)
	}
	if !cause.Status.Cause || !cause.HasDerivedName() {
		return nil
	}
	var list remediationv1alpha1.IncidentList
	if err := c.cache.List(wctx, &list, client.InNamespace(c.cfg.Namespace)); err != nil {
		return fmt.Errorf("listing the Incidents to fold into %s: %w", name, err)
	}
	var requests remediationv1alpha1.RemediationRequestList
	if err := c.cache.List(wctx, &requests, client.InNamespace(c.cfg.Namespace)); err != nil {
		return fmt.Errorf("listing the RemediationRequests to fold into %s: %w", name, err)
	}
	now := c.cfg.Now().UTC().Truncate(time.Second)
	var errs []error
	for i := range list.Items {
		if foldable(&list.Items[i], cause, requests.Items) {
			if err := c.foldOne(wctx, list.Items[i].Name, cause, now); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// foldOne reads the Incident name and the RemediationRequests from the API server, decides [foldable] on them
// and writes the fold with the resource version it read, so a write by another writer in between (the analysis
// moving it to Proposed, the closure rules) fails on a conflict and is decided again from the new state. An
// Incident gone meanwhile is no error.
func (c *Controller) foldOne(ctx context.Context, name string, cause *remediationv1alpha1.Incident, now time.Time) error {
	var folded *remediationv1alpha1.Incident
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		folded = nil
		inc := &remediationv1alpha1.Incident{}
		if err := c.reader.Get(ctx, types.NamespacedName{Namespace: c.cfg.Namespace, Name: name}, inc); err != nil {
			return err
		}
		var requests remediationv1alpha1.RemediationRequestList
		if err := c.reader.List(ctx, &requests, client.InNamespace(c.cfg.Namespace)); err != nil {
			return fmt.Errorf("listing the RemediationRequests: %w", err)
		}
		if !foldable(inc, cause, requests.Items) {
			return nil
		}
		if err := fold(inc, cause, now); err != nil {
			return err
		}
		if err := c.client.Status().Update(ctx, inc, client.FieldOwner(lifecycleFieldOwner)); err != nil {
			return err
		}
		folded = inc
		return nil
	})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("folding Incident %s into %s: %w", name, cause.Name, err)
	}
	if folded == nil {
		return nil
	}
	c.mu.Lock()
	c.noteState(folded)
	c.mu.Unlock()
	logf.FromContext(ctx).Info("Folded a consequence Incident into its cause's Incident", "incident", name,
		"cause", cause.Name)
	return nil
}
