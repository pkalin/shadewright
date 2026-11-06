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

package analysis

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/incidents"
)

// ConditionAnalyzed is the Incident condition the analysis writes with the final phase: True, with the
// outcome's [Reason] and a fixed message. Nothing from the model or the cluster goes into it.
const ConditionAnalyzed = "Analyzed"

// FieldOwner is the field manager of the analysis's writes.
const FieldOwner = "shadewright-analysis"

// maxWorkers is the most analyses the controller runs at once, the largest maxConcurrentAnalyses.
const maxWorkers = 8

// busyRequeue is when an Incident due while maxConcurrentAnalyses analyses run is looked at again.
const busyRequeue = 10 * time.Second

// finalWriteTimeout bounds the write of the outcome, which also runs after the analysis deadline. It is cut
// to the time left on the claim when that is shorter.
const finalWriteTimeout = 30 * time.Second

// createTimeout bounds one live read of the recheck, and the create of a request with its reuse check (ADR-0025 decision 1). It is
// cut to the time left on the claim when that is shorter.
const createTimeout = 20 * time.Second

// maxRecorded is the most request names status.remediationRequests keeps.
const maxRecorded = 20

// maxListedRequests bounds the requests of one Incident a claim reads by label.
const maxListedRequests = 100

// LabelIncidentUID is the label every RemediationRequest the analysis creates carries: the UID of its
// Incident. A claim lists the requests with its Incident's UID and records in status.remediationRequests the
// one the previous run created and whose outcome write was lost (ADR-0025 decision 6). It is a lookup key
// only: a request is recorded only within that run's claim window and with the form an analysis gives it
// (isAnalysisRequest), and nothing reads it to decide what may run (invariant 1).
const LabelIncidentUID = "remediation.shadewright.io/incident-uid"

// Controller runs the analysis of the Incidents the trigger marks due (design 3.2 and 3.3). It claims an
// Incident by writing the phase Analyzing, lastAnalysisTime and the AnalysisDue condition (not due, reason
// Analyzing) in one status update with the resourceVersion it read, so two runs never claim the same round;
// the claim also records any request of the Incident that an earlier run created and did not record. It then
// runs the [Analyzer], creates the RemediationRequest of a plan, and writes the phase Proposed or Escalated
// with the Analyzed condition and the request's name in one more status update.
//
// The lastAnalysisTime the claim wrote identifies the run, and the claim expires ClaimTimeout after it
// (ADR-0025 decision 1). Just before the create and in the outcome update the run reads the Incident live and
// goes on only while it still owns the claim: phase Analyzing, the same lastAnalysisTime, UID, generation and
// subject, and the claim not expired. Otherwise it drops the result: no request, no outcome write, a log
// line. The analysis deadline, the create and the outcome write together fit inside the claim
// ([Controller.CheckTimeBudget]), and neither write starts after the claim expired or runs past it.
type Controller struct {
	// Client reads Incidents and RemediationSettings from the cache and writes the Incident status and the
	// requests.
	Client client.Client
	// Reader reads the Incident and its requests live, not from the cache, for the claim's recheck and for
	// the requests to record. Nil means the manager's API reader (SetupWithManager).
	Reader client.Reader
	// ClaimTimeout is how long a claim lasts: the trigger's analysis timeout, after which it takes an
	// Incident left in Analyzing as lost. Zero means [incidents.DefaultAnalysisTimeout].
	ClaimTimeout time.Duration
	// Namespace is the operator namespace: the Incidents' and the requests'.
	Namespace string
	// Analyzer runs the analysis.
	Analyzer *Analyzer
	// Now returns the current time; nil means time.Now.
	Now func() time.Time

	mu       sync.Mutex
	inFlight int
}

// claimTimeout returns ClaimTimeout or its default.
func (c *Controller) claimTimeout() time.Duration {
	if c.ClaimTimeout > 0 {
		return c.ClaimTimeout
	}
	return incidents.DefaultAnalysisTimeout
}

// CheckTimeBudget returns an error unless the analysis deadline, the create and the outcome write together
// fit inside the claim (ADR-0025 decision 1), with a second to spare for lastAnalysisTime, which is written
// in whole seconds. It also refuses a controller without an analyzer or a live reader.
func (c *Controller) CheckTimeBudget() error {
	if c.Analyzer == nil {
		return errors.New("analysis: the controller needs an analyzer")
	}
	if c.Reader == nil {
		return errors.New("analysis: the controller needs a live reader")
	}
	need := c.Analyzer.Timeout() + createTimeout + finalWriteTimeout + time.Second
	if need >= c.claimTimeout() {
		return fmt.Errorf("analysis: the deadline %s, the create (%s) and the outcome write (%s) do not fit "+
			"inside the claim of %s", c.Analyzer.Timeout(), createTimeout, finalWriteTimeout, c.claimTimeout())
	}
	return nil
}

// SetupWithManager registers the controller. It runs on the leader only and looks at Incidents in the
// operator namespace whose AnalysisDue condition is True for their current generation
// ([Controller.markedDue]).
func (c *Controller) SetupWithManager(mgr ctrl.Manager) error {
	if c.Reader == nil {
		c.Reader = mgr.GetAPIReader()
	}
	if err := c.CheckTimeBudget(); err != nil {
		return err
	}
	due := predicate.NewPredicateFuncs(c.markedDue)
	return ctrl.NewControllerManagedBy(mgr).
		Named("analysis").
		For(&remediationv1alpha1.Incident{}, builder.WithPredicates(due)).
		WithOptions(controller.Options{MaxConcurrentReconciles: maxWorkers}).
		Complete(c)
}

// markedDue reports whether o is an Incident in the operator namespace that the trigger marked due for its
// current generation ([incidents.DueMark]): the controller starts a reconcile for no other, so a mark of
// another generation does not.
func (c *Controller) markedDue(o client.Object) bool {
	inc, ok := o.(*remediationv1alpha1.Incident)
	if !ok || inc.Namespace != c.Namespace {
		return false
	}
	cond := incidents.DueMark(inc)
	return cond != nil && cond.Status == metav1.ConditionTrue
}

func (c *Controller) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// claimable reports whether the trigger marked inc due in a phase an analysis may start from: Open (or none)
// or Analyzing taken as lost (reason AnalysisTimedOut). Never Resolved: reopening a Resolved Incident is a
// later part of #251, and until it is built a Resolved Incident is not analyzed again. A mark written for
// another generation of the Incident is no mark, as the trigger takes it (#430): it fails closed, and the
// trigger writes it again for the current generation if the Incident is still due.
func claimable(inc *remediationv1alpha1.Incident) bool {
	cond := incidents.DueMark(inc)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		return false
	}
	switch inc.Status.Phase {
	case "", remediationv1alpha1.IncidentOpen:
		return true
	case remediationv1alpha1.IncidentAnalyzing:
		return cond.Reason == incidents.ReasonAnalysisTimedOut
	}
	return false
}

// Reconcile analyzes one due Incident.
func (c *Controller) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	inc := &remediationv1alpha1.Incident{}
	if err := c.Client.Get(ctx, req.NamespacedName, inc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if inc.Namespace != c.Namespace || !claimable(inc) {
		return ctrl.Result{}, nil
	}
	settings := &remediationv1alpha1.RemediationSettings{}
	if err := c.Client.Get(ctx, types.NamespacedName{Name: remediationv1alpha1.SettingsName}, settings); err != nil {
		// Without settings the trigger marks nothing due; a read that failed is tried again.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !c.acquire(settings) {
		return ctrl.Result{RequeueAfter: busyRequeue}, nil
	}
	defer c.release()

	unrecorded, err := c.unrecordedRequests(ctx, inc)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("listing the RemediationRequests of Incident %s: %w", inc.Name, err)
	}
	claimed, err := c.claim(ctx, inc, unrecorded)
	if err != nil {
		if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
		return ctrl.Result{}, fmt.Errorf("claiming Incident %s: %w", inc.Name, err)
	}
	out, err := c.Analyzer.Analyze(ctx, claimed, LimitsOf(settings))
	if err != nil {
		// A stop or a lost leadership: the Incident stays Analyzing and the trigger takes it as lost later.
		return ctrl.Result{}, err
	}
	// The run's identity in the logs (ADR-0025 decision 1): a correlation key, never read to decide anything.
	runID := claimed.Status.LastAnalysisTime.UTC().Format(time.RFC3339)
	var created, attempted remediationv1alpha1.Name
	if out.Plan != nil {
		// The name the create uses, kept for a create that fails after the server took it (finish).
		attempted, _ = requestName(claimed, requestSpec(claimed, *out.Plan))
		created, err = c.publish(ctx, claimed, *out.Plan)
		switch {
		case errors.Is(err, errClaimLost):
			logger.Info("Dropped the result of an analysis whose claim was lost", append([]any{"incident", inc.Name,
				"run", runID, "reason", out.Reason, "calls", out.Calls, "tokens", out.Tokens, "models", out.Models},
				resultFields(out)...)...)
			return ctrl.Result{}, nil
		case err != nil:
			if ctx.Err() != nil {
				return ctrl.Result{}, ctx.Err()
			}
			logger.Error(err, "Failed to create the RemediationRequest of an analysis", "incident", inc.Name, "run", runID)
			out.Reason, out.Plan, created = ReasonRequestNotCreated, nil, ""
		}
	}
	logger.Info("Analyzed Incident", append([]any{"incident", inc.Name, "run", runID, "reason", out.Reason,
		"escalated", out.Escalated, "calls", out.Calls, "toolCalls", out.ToolCalls, "tokens", out.Tokens,
		"models", out.Models, "promptVersion", out.PromptVersion, "request", created}, resultFields(out)...)...)
	switch err := c.finish(ctx, claimed, out, created, attempted); {
	case errors.Is(err, errClaimLost):
		logger.Info("Dropped the outcome of an analysis whose claim was lost", "incident", inc.Name, "run", runID,
			"reason", out.Reason, "request", created)
	case err != nil:
		return ctrl.Result{}, fmt.Errorf("writing the analysis outcome of Incident %s: %w", inc.Name, err)
	}
	return ctrl.Result{}, nil
}

// resultFields are the log fields of the result's own words, redacted in the loop and within the result's
// bounds, only those the result has (#422): what a hand-off asked for is otherwise lost while no analysis
// record is stored (#77).
func resultFields(out Outcome) []any {
	var kv []any
	if out.Summary != "" {
		kv = append(kv, "summary", out.Summary)
	}
	if len(out.MissingData) > 0 {
		kv = append(kv, "missingData", out.MissingData)
	}
	return kv
}

// errClaimLost: the run no longer owns its claim, or the claim expired, so its result is dropped.
var errClaimLost = errors.New("the analysis no longer owns its claim")

// expiry is when the claim of a run ends: lastAnalysisTime plus the claim timeout.
func (c *Controller) expiry(claimed *remediationv1alpha1.Incident) time.Time {
	return claimed.Status.LastAnalysisTime.Add(c.claimTimeout())
}

// withinClaim returns a context bounded by limit and by the claim's expiry, or errClaimLost when the claim
// has expired, so no write starts after it. With detach the context keeps no cancellation of ctx (the
// outcome write, which also runs after a stop).
func (c *Controller) withinClaim(ctx context.Context, claimed *remediationv1alpha1.Incident, limit time.Duration,
	detach bool) (context.Context, context.CancelFunc, error) {
	left := c.expiry(claimed).Sub(c.now())
	if left <= 0 {
		return nil, nil, errClaimLost
	}
	if detach {
		ctx = context.WithoutCancel(ctx)
	}
	wctx, cancel := context.WithTimeout(ctx, min(limit, left))
	return wctx, cancel, nil
}

// owns reports whether the live Incident is still the run's claim (ADR-0025 decision 1): the same UID,
// generation and subject, the phase Analyzing, the lastAnalysisTime the claim wrote, and the claim not expired.
func (c *Controller) owns(live, claimed *remediationv1alpha1.Incident) bool {
	return live.UID == claimed.UID && live.Generation == claimed.Generation &&
		live.Spec.Subject == claimed.Spec.Subject &&
		live.Status.Phase == remediationv1alpha1.IncidentAnalyzing && live.Status.LastAnalysisTime != nil &&
		live.Status.LastAnalysisTime.Equal(claimed.Status.LastAnalysisTime) && c.now().Before(c.expiry(claimed))
}

// recheck reads the Incident live and returns it while the run still owns its claim, errClaimLost otherwise.
func (c *Controller) recheck(ctx context.Context, claimed *remediationv1alpha1.Incident) (*remediationv1alpha1.Incident, error) {
	live := &remediationv1alpha1.Incident{}
	if err := c.Reader.Get(ctx, client.ObjectKeyFromObject(claimed), live); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, errClaimLost
		}
		return nil, fmt.Errorf("reading Incident %s: %w", claimed.Name, err)
	}
	if !c.owns(live, claimed) {
		return nil, errClaimLost
	}
	return live, nil
}

// publish creates the request of a plan within the claim: it rechecks the claim live just before the create
// (retrying a transient read error, see recheckRetrying), then the create and its reuse check share one
// timeout, cut to the time left on the claim, so the create cannot outlive it.
func (c *Controller) publish(ctx context.Context, claimed *remediationv1alpha1.Incident, p Plan) (remediationv1alpha1.Name, error) {
	if err := c.recheckRetrying(ctx, claimed); err != nil {
		return "", err
	}
	cctx, cancel, err := c.withinClaim(ctx, claimed, createTimeout, false)
	if err != nil {
		return "", err
	}
	defer cancel()
	return c.createRequest(cctx, claimed, p)
}

// recheckBackoff bounds the wait between two live reads of the recheck before the create.
const (
	recheckFirstBackoff = 200 * time.Millisecond
	recheckMaxBackoff   = 5 * time.Second
)

// recheckRetrying rechecks the claim before the create. A read error that is not a lost claim (a timeout, an
// unavailable API server) is retried with backoff while the claim has time left beyond the create and the outcome write;
// only when that time runs out is the error returned, and the result handed off. A lost claim or a stop is
// returned at once.
func (c *Controller) recheckRetrying(ctx context.Context, claimed *remediationv1alpha1.Incident) error {
	deadline := c.expiry(claimed).Add(-finalWriteTimeout - createTimeout)
	backoff := recheckFirstBackoff
	for {
		rctx, cancel, err := c.withinClaim(ctx, claimed, createTimeout, false)
		if err != nil {
			return err
		}
		_, err = c.recheck(rctx, claimed)
		cancel()
		if err == nil || errors.Is(err, errClaimLost) || ctx.Err() != nil {
			return err
		}
		if c.now().Add(backoff).After(deadline) {
			return err
		}
		log.FromContext(ctx).Info("Retrying the live read of an Incident before the create", "incident",
			claimed.Name, "error", err.Error())
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, recheckMaxBackoff)
	}
}

// unrecordedRequests lists, live, the requests labelled with inc's UID and returns at most one that inc's
// status does not record yet: a request the run of inc's lastAnalysisTime created before its outcome write
// was lost (ADR-0025 decision 6). Only a request created within that run's claim window (from lastAnalysisTime
// to its expiry) counts, so a name the status dropped at its bound never comes back, and only one per run,
// which creates at most one. The label and spec are set by whoever creates the request, so it must also have
// the form an analysis gives it (isAnalysisRequest). Without a lastAnalysisTime nothing counts.
func (c *Controller) unrecordedRequests(ctx context.Context, inc *remediationv1alpha1.Incident) ([]remediationv1alpha1.Name, error) {
	if inc.Status.LastAnalysisTime == nil {
		return nil, nil
	}
	start, end := inc.Status.LastAnalysisTime.Time, c.expiry(inc)
	list := &remediationv1alpha1.RemediationRequestList{}
	if err := c.Reader.List(ctx, list, client.InNamespace(c.Namespace),
		client.MatchingLabels{LabelIncidentUID: string(inc.UID)}, client.Limit(maxListedRequests)); err != nil {
		return nil, err
	}
	if list.Continue != "" {
		log.FromContext(ctx).Info("Listed only the first RemediationRequests labelled with an Incident's UID",
			"incident", inc.Name, "limit", maxListedRequests)
	}
	var found *remediationv1alpha1.RemediationRequest
	for i := range list.Items {
		rr := &list.Items[i]
		created := rr.CreationTimestamp.Time
		if created.Before(start) || !created.Before(end) ||
			slices.Contains(inc.Status.RemediationRequests, remediationv1alpha1.Name(rr.Name)) ||
			!c.isAnalysisRequest(inc, rr) {
			continue
		}
		// The earliest, then the first by name, for a stable choice.
		if found == nil || created.Before(found.CreationTimestamp.Time) ||
			(created.Equal(found.CreationTimestamp.Time) && rr.Name < found.Name) {
			found = rr
		}
	}
	if found == nil {
		return nil, nil
	}
	return []remediationv1alpha1.Name{remediationv1alpha1.Name(found.Name)}, nil
}

// isAnalysisRequest reports whether rr has the form the analysis gives a request of inc: its spec names inc
// and its subject, GitOps mode, approval required, the policy configured for its action type, and the name
// derived from inc's UID and that spec (requestName). It proves nothing about who created rr: whoever may
// create RemediationRequests in the operator namespace can still forge one (threat model G5.6).
func (c *Controller) isAnalysisRequest(inc *remediationv1alpha1.Incident, rr *remediationv1alpha1.RemediationRequest) bool {
	s := rr.Spec
	if s.IncidentRef != (remediationv1alpha1.IncidentRef{Name: inc.Name, UID: remediationv1alpha1.UID(inc.UID)}) ||
		s.TargetRef != inc.Spec.Subject || s.Mode != remediationv1alpha1.RemediationModeGitOps ||
		s.ApprovalRequired == nil || !*s.ApprovalRequired {
		return false
	}
	if c.Analyzer == nil {
		return false
	}
	if ref, ok := c.Analyzer.cfg.Policies[s.Action.Type]; !ok || ref == "" || ref != s.PolicyRef {
		return false
	}
	name, err := requestName(inc, requestSpec(inc, Plan{Action: s.Action, Checks: s.Verification.Checks, PolicyRef: s.PolicyRef}))
	return err == nil && string(name) == rr.Name
}

// ReasonRequestNotCreated: the analysis made a plan and the RemediationRequest could not be created.
const ReasonRequestNotCreated Reason = "RequestNotCreated"

// acquire takes a slot of spec.analysis.maxConcurrentAnalyses (1 when unset).
func (c *Controller) acquire(s *remediationv1alpha1.RemediationSettings) bool {
	limit := 1
	if a := s.Spec.Analysis; a != nil && a.MaxConcurrentAnalyses != nil {
		limit = int(min(max(*a.MaxConcurrentAnalyses, 1), maxWorkers))
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.inFlight >= limit {
		return false
	}
	c.inFlight++
	return true
}

func (c *Controller) release() {
	c.mu.Lock()
	c.inFlight--
	c.mu.Unlock()
}

// claim writes the phase Analyzing, lastAnalysisTime and AnalysisDue (not due, Analyzing) in one status
// update with the resourceVersion read: the trigger then sees the analysis running, and a conflict means
// another write came first, so the claim is not made. The same update records the requests in unrecorded.
func (c *Controller) claim(ctx context.Context, inc *remediationv1alpha1.Incident,
	unrecorded []remediationv1alpha1.Name) (*remediationv1alpha1.Incident, error) {
	claimed := inc.DeepCopy()
	for _, name := range unrecorded {
		claimed.Status.RemediationRequests = appendBounded(claimed.Status.RemediationRequests, name, maxRecorded)
	}
	now := metav1.NewTime(c.now().UTC().Truncate(time.Second))
	claimed.Status.Phase = remediationv1alpha1.IncidentAnalyzing
	claimed.Status.LastAnalysisTime = &now
	meta.SetStatusCondition(&claimed.Status.Conditions, metav1.Condition{
		Type: incidents.ConditionAnalysisDue, Status: metav1.ConditionFalse, Reason: incidents.ReasonAnalyzing,
		Message: "An analysis is running", ObservedGeneration: claimed.Generation, LastTransitionTime: now,
	})
	if err := c.Client.Status().Update(ctx, claimed, client.FieldOwner(FieldOwner)); err != nil {
		return nil, err
	}
	return claimed, nil
}

// requestName derives the request's name from the Incident's UID and the spec it carries, so a retried
// create, or a later round after a crash that proposes the same plan, finds the request already made instead
// of adding another (invariant 5). It is remediationv1alpha1.RequestName, which the executor recomputes to show
// the name in a pull request body; spec.incidentRef.uid must be the Incident's UID, as requestSpec sets it.
func requestName(inc *remediationv1alpha1.Incident, spec remediationv1alpha1.RemediationRequestSpec) (remediationv1alpha1.Name, error) {
	if spec.IncidentRef.UID != remediationv1alpha1.UID(inc.UID) {
		return "", errors.New("the request spec names another Incident")
	}
	return remediationv1alpha1.RequestName(spec)
}

// requestSpec is the spec of the request of a plan: nothing in it comes from the model but the action and the
// extra checks, which the loop validated.
func requestSpec(inc *remediationv1alpha1.Incident, p Plan) remediationv1alpha1.RemediationRequestSpec {
	approval := true
	return remediationv1alpha1.RemediationRequestSpec{
		IncidentRef:      remediationv1alpha1.IncidentRef{Name: inc.Name, UID: remediationv1alpha1.UID(inc.UID)},
		TargetRef:        inc.Spec.Subject,
		Mode:             remediationv1alpha1.RemediationModeGitOps,
		ApprovalRequired: &approval,
		PolicyRef:        p.PolicyRef,
		Action:           p.Action,
		Verification:     remediationv1alpha1.Verification{Checks: p.Checks},
	}
}

// createRequest creates the RemediationRequest of a plan in the operator namespace: the Incident's subject as
// the target, approval required, the configured policy. It is a proposal only (invariant 1); the executor's
// policy check decides it. An existing request of the same name is accepted only when it holds the same plan
// for this Incident.
func (c *Controller) createRequest(ctx context.Context, inc *remediationv1alpha1.Incident, p Plan) (remediationv1alpha1.Name, error) {
	spec := requestSpec(inc, p)
	name, err := requestName(inc, spec)
	if err != nil {
		return "", err
	}
	rr := &remediationv1alpha1.RemediationRequest{
		ObjectMeta: metav1.ObjectMeta{Namespace: c.Namespace, Name: string(name),
			Labels: map[string]string{LabelIncidentUID: string(inc.UID)}},
		Spec: *spec.DeepCopy(),
	}
	err = c.Client.Create(ctx, rr, client.FieldOwner(FieldOwner))
	if apierrors.IsAlreadyExists(err) {
		existing := &remediationv1alpha1.RemediationRequest{}
		// Read live: the cache may not hold the request yet, and a miss would hand the proposal off.
		if gerr := c.Reader.Get(ctx, client.ObjectKeyFromObject(rr), existing); gerr != nil {
			return "", fmt.Errorf("reading RemediationRequest %s: %w", name, gerr)
		}
		// The request is the one this plan would create only when every field the plan sets is the same; the
		// API server's defaults fill only fields the plan leaves out (the verification bounds).
		e := existing.Spec
		if e.IncidentRef != spec.IncidentRef || e.TargetRef != spec.TargetRef || e.PolicyRef != spec.PolicyRef ||
			e.Mode != spec.Mode || e.ApprovalRequired == nil || !*e.ApprovalRequired ||
			!equality.Semantic.DeepEqual(e.Action, spec.Action) ||
			!equality.Semantic.DeepEqual(e.Verification.Checks, spec.Verification.Checks) {
			return "", errors.New("a RemediationRequest of that name holds another plan")
		}
		return name, nil
	}
	if err != nil {
		return "", fmt.Errorf("creating RemediationRequest %s: %w", name, err)
	}
	return name, nil
}

// outcomeMessages are the fixed messages of the Analyzed condition.
var outcomeMessages = map[Reason]string{
	ReasonProposed:                "The analysis proposed a change; a RemediationRequest was created for the policy check",
	ReasonRunbookMatched:          "A runbook matched; a RemediationRequest was created for the policy check",
	ReasonNoModelConfigured:       "No triage model is configured in RemediationSettings; handed off to a person",
	ReasonBudgetSpent:             "The daily analysis budget is spent; handed off to a person",
	ReasonBudgetUnavailable:       "The daily analysis budget could not be read or trusted; handed off to a person",
	ReasonNoProposal:              "The analysis made no proposal; handed off to a person",
	ReasonMissingData:             "The analysis made no proposal and named missing data; handed off to a person",
	ReasonNoEvidence:              "The proposal quoted no evidence found in the context or tool results; handed off to a person",
	ReasonTargetNotSupported:      "The proposed action cannot target the incident's subject; handed off to a person",
	ReasonNoPolicyForAction:       "No RemediationPolicy is configured for the proposed action; handed off to a person",
	ReasonActionOutOfBounds:       "The proposed action is outside the catalog's bounds; handed off to a person",
	ReasonInvalidOutput:           "The model's output did not match the result schema; handed off to a person",
	ReasonEscalationNotConfigured: "An escalation signal was found and no escalation model is configured; handed off to a person",
	ReasonStepLimit:               "The escalated analysis reached its tool step limit; handed off to a person",
	ReasonTokenLimit:              "The analysis reached its token limit; handed off to a person",
	ReasonDeadlineExceeded:        "The analysis ran out of its deadline; handed off to a person",
	ReasonModelUnavailable:        "The model stayed unavailable after bounded retries, or its retries used up the call limit; handed off to a person",
	ReasonModelNotFound:           "The model deployment does not exist; handed off to a person",
	ReasonModelForbidden:          "The model endpoint refused the analyzer's identity; handed off to a person",
	ReasonModelError:              "The model call failed; handed off to a person",
	ReasonRequestNotCreated:       "The RemediationRequest of the proposal could not be created; handed off to a person",
	ReasonUnexpectedModel:         "A reply came from another model than the one configured for the deployment; handed off to a person",
	ReasonEndpointNotAllowed:      "The model endpoint is outside the analyzer's allow-list of host suffixes; nothing was sent; handed off to a person",
}

// finish writes the outcome in one status update: Proposed with the request's name, or Escalated, and the
// Analyzed condition. It rechecks the claim live and writes with the resourceVersion of that read, retrying
// on conflict with a new recheck, so a stale run never overwrites a newer one; it returns errClaimLost and
// writes nothing once the run no longer owns the claim. It runs with its own timeout, cut to the time left on
// the claim, after the analysis deadline and a stop too. When the create failed (request empty, attempted
// its name), it reads that request live and records it if the server created it anyway, as a create whose
// reply was lost does: the run's own request only, never another one.
func (c *Controller) finish(ctx context.Context, claimed *remediationv1alpha1.Incident, out Outcome,
	request, attempted remediationv1alpha1.Name) error {
	wctx, cancel, err := c.withinClaim(ctx, claimed, finalWriteTimeout, true)
	if err != nil {
		return err
	}
	defer cancel()
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		inc, err := c.recheck(wctx, claimed)
		if err != nil {
			return err
		}
		inc.Status.Phase = remediationv1alpha1.IncidentEscalated
		if request == "" && attempted != "" {
			if c.createdAnyway(wctx, inc, attempted) {
				inc.Status.RemediationRequests = appendBounded(inc.Status.RemediationRequests, attempted, maxRecorded)
			}
		}
		if out.Plan != nil && request != "" {
			inc.Status.Phase = remediationv1alpha1.IncidentProposed
			inc.Status.RemediationRequests = appendBounded(inc.Status.RemediationRequests, request, maxRecorded)
		}
		meta.SetStatusCondition(&inc.Status.Conditions, metav1.Condition{
			Type: ConditionAnalyzed, Status: metav1.ConditionTrue, Reason: string(out.Reason),
			Message: outcomeMessages[out.Reason], ObservedGeneration: inc.Generation,
			LastTransitionTime: metav1.NewTime(c.now().UTC().Truncate(time.Second)),
		})
		return c.Client.Status().Update(wctx, inc, client.FieldOwner(FieldOwner))
	})
}

// createdAnyway reports whether the request of a failed create exists, read live, with the form the analysis
// gives a request of inc. A read error is logged and counts as no: a later claim's record step finds it.
func (c *Controller) createdAnyway(ctx context.Context, inc *remediationv1alpha1.Incident, name remediationv1alpha1.Name) bool {
	rr := &remediationv1alpha1.RemediationRequest{}
	if err := c.Reader.Get(ctx, client.ObjectKey{Namespace: c.Namespace, Name: string(name)}, rr); err != nil {
		if !apierrors.IsNotFound(err) {
			log.FromContext(ctx).Error(err, "Failed to read the RemediationRequest of a failed create", "incident", inc.Name,
				"request", name)
		}
		return false
	}
	return c.isAnalysisRequest(inc, rr)
}

// appendBounded appends name unless present and keeps the newest n.
func appendBounded(names []remediationv1alpha1.Name, name remediationv1alpha1.Name, n int) []remediationv1alpha1.Name {
	if slices.Contains(names, name) {
		return names
	}
	names = append(names, name)
	if len(names) > n {
		names = names[len(names)-n:]
	}
	return names
}
