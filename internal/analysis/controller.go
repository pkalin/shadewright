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
	"io"
	"slices"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/apimachinery/pkg/util/wait"
	corerecord "k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/incidents"
	"github.com/pkalin/shadewright/internal/providers"
)

// ConditionAnalyzed is the Incident condition the analysis writes with the final phase: True, with the
// outcome's [Reason] and a fixed message. Nothing from the model or the cluster goes into it.
const ConditionAnalyzed = incidents.ConditionAnalyzed

// FieldOwner is the field manager of the analysis's writes.
const FieldOwner = "shadewright-analysis"

// maxWorkers is the most analyses the controller runs at once, the largest maxConcurrentAnalyses.
const maxWorkers = 8

// busyRequeue is when an Incident due while maxConcurrentAnalyses analyses run is looked at again.
const busyRequeue = 10 * time.Second

// finalWriteTimeout bounds the write of the outcome, which also runs after the analysis deadline. It is cut
// to the time left on the claim when that is shorter.
const finalWriteTimeout = 30 * time.Second

// recoverTimeout bounds the live read, with its retries, of the request of a failed create (recoverRequest).
// It is cut to the time left on the claim before the outcome write's timeout.
const recoverTimeout = 10 * time.Second

// createTimeout bounds one live read of the recheck, and the create of a request with its reuse check (ADR-0025 decision 1). It is
// cut to the time left on the claim when that is shorter.
const createTimeout = 20 * time.Second

// maxRecorded is the most request names status.remediationRequests keeps.
const maxRecorded = 20

// maxListedRequests is the page size of the live listing of one Incident's requests by label.
const maxListedRequests = 100

// LabelIncidentUID is the label every RemediationRequest the analysis creates carries: the UID of its
// Incident. A claim lists the requests with its Incident's UID and records in status.remediationRequests the
// one the previous run created and whose outcome write was lost (ADR-0025 decision 6). It is a lookup key
// only: a request is recorded only within that run's claim window and with the form an analysis gives it
// (IsAnalysisRequest), and nothing reads it to decide what may run (invariant 1).
const LabelIncidentUID = "remediation.shadewright.io/incident-uid"

// Controller runs the analysis of the Incidents the trigger marks due (design 3.2 and 3.3). It claims an
// Incident by writing the phase Analyzing, lastAnalysisTime and the AnalysisDue condition (not due, reason
// Analyzing) in one status update with the resourceVersion it read, so two runs never claim the same round;
// the claim also records any request of the Incident that an earlier run created and did not record. It then
// runs the [Analyzer], creates the RemediationRequest of a plan, and writes the phase Proposed or Escalated
// with the Analyzed condition, the request's name and the analysis record's ID (when Store is set) in one more
// status update.
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
	// Store keeps one redacted record of each analysis outside the cluster (ADR-0015); its ID goes into
	// status.analysisRecords. Nil means no record is written.
	Store providers.AnalysisStore
	// Recorder writes a Warning event on an Incident whose analysis record was not written. Nil writes none.
	Recorder corerecord.EventRecorder
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

// CheckTimeBudget returns an error unless the analysis deadline, the create, the record write (with a Store)
// and the outcome write together fit inside the claim (ADR-0025 decision 1), with a second to spare for
// lastAnalysisTime, which is written in whole seconds. It also refuses a controller without an analyzer or a
// live reader.
func (c *Controller) CheckTimeBudget() error {
	if c.Analyzer == nil {
		return errors.New("analysis: the controller needs an analyzer")
	}
	if c.Reader == nil {
		return errors.New("analysis: the controller needs a live reader")
	}
	need := c.Analyzer.Timeout() + createTimeout + c.afterCreate() + time.Second
	if need >= c.claimTimeout() {
		parts := []string{fmt.Sprintf("the deadline %s", c.Analyzer.Timeout()), fmt.Sprintf("the create (%s)", createTimeout)}
		if c.Store != nil {
			parts = append(parts, fmt.Sprintf("the record write (%s)", storeTimeout))
		}
		return fmt.Errorf("analysis: %s and the outcome write (%s) do not fit inside the claim of %s",
			strings.Join(parts, ", "), finalWriteTimeout, c.claimTimeout())
	}
	return nil
}

// afterCreate is the time the claim keeps for the writes after the create: the record write when a Store is
// set, and the outcome write.
func (c *Controller) afterCreate() time.Duration {
	if c.Store != nil {
		return storeTimeout + finalWriteTimeout
	}
	return finalWriteTimeout
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
	if c.Store != nil {
		initRecordMetrics(analysisRecords)
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

// claimable reports whether the trigger marked inc due in a phase an analysis may start from: Open (or none),
// Analyzing taken as lost (reason AnalysisTimedOut), or Proposed with a reviewer hint (reason ReviewerHint,
// ADR-0028; the claim also needs the hint, read live, [Controller.reviewerHint]). Never Resolved: a Resolved Incident is never
// analyzed again; a symptom seen again moves it to Escalated, or opens its next occurrence as a new Incident
// with nothing of this one's outcome (design 3.2, #251). A mark written for
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
	case remediationv1alpha1.IncidentProposed:
		return cond.Reason == incidents.ReasonReviewerHint
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

	requests, err := c.incidentRequests(ctx, inc)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("listing the RemediationRequests of Incident %s: %w", inc.Name, err)
	}
	hint, done, res, err := c.settleHint(ctx, inc, requests)
	if done {
		return res, err
	}
	claimed, err := c.claim(ctx, inc, c.unrecordedRequests(inc, requests), hint)
	if err != nil {
		if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
		return ctrl.Result{}, fmt.Errorf("claiming Incident %s: %w", inc.Name, err)
	}
	if res, done, err := c.hintStillPending(ctx, claimed, hint); done {
		return res, err
	}
	limits := LimitsOf(settings)
	var hintText string
	if hint != nil {
		hintText = hint.Hint.Text
	}
	out, err := c.Analyzer.AnalyzeWithHint(ctx, claimed, limits, hintText)
	if err != nil {
		// A stop or a lost leadership: the Incident stays Analyzing and the trigger takes it as lost later.
		return ctrl.Result{}, err
	}
	// The run's identity in the logs (ADR-0025 decision 1): a correlation key, never read to decide anything.
	runID := claimed.Status.LastAnalysisTime.UTC().Format(time.RFC3339)
	var created, attempted remediationv1alpha1.Name
	out, err = c.checkPlan(ctx, claimed, hint, out)
	if err == nil && out.Plan != nil {
		spec := withReplacement(requestSpec(claimed, *out.Plan), hint)
		// The name the create uses, kept for a create that fails after the server took it (finish).
		attempted, _ = requestName(claimed, spec)
		created, err = c.publish(ctx, claimed, spec)
		if err != nil && !errors.Is(err, errClaimLost) {
			if ctx.Err() != nil {
				return ctrl.Result{}, ctx.Err()
			}
			logger.Error(err, "Failed to create the RemediationRequest of an analysis", "incident", inc.Name, "run", runID)
			out.Reason, out.Plan, created, err = ReasonRequestNotCreated, nil, "", nil
		}
	}
	switch {
	case errors.Is(err, errClaimLost):
		logger.Info("Dropped the result of an analysis whose claim was lost", append([]any{"incident", inc.Name,
			"run", runID, "reason", out.Reason, "calls", out.Calls, "tokens", out.Tokens, "models", out.Models},
			resultFields(out)...)...)
		return ctrl.Result{}, nil
	case err != nil:
		// The request holding the hint could not be read: no outcome is written for it, so the hint is not
		// answered by an API failure. The Incident stays Analyzing and the trigger takes the run as lost later.
		return ctrl.Result{}, fmt.Errorf("reading the reviewer hint of Incident %s after its analysis: %w", inc.Name, err)
	}
	logger.Info("Analyzed Incident", append([]any{"incident", inc.Name, "run", runID, "reason", out.Reason,
		"escalated", out.Escalated, "calls", out.Calls, "toolCalls", out.ToolCalls, "tokens", out.Tokens,
		"models", out.Models, "promptVersion", out.PromptVersion, "request", created}, resultFields(out)...)...)
	// The record is written after a live read confirms the claim, so a run whose claim was lost before it leaves
	// no record, and before the outcome, which carries its ID: a record ID in the status always names a record
	// that was stored. A claim lost between the two leaves a record whose ID is not in the status.
	record, err := c.conclude(ctx, claimed, out, limits, created, attempted)
	switch {
	case errors.Is(err, errClaimLost):
		// A record stored before the claim was lost stays in the store, which is create-only: it is the
		// evidence of a run whose outcome was dropped, and this line names it.
		l := logger
		if record != "" {
			l = l.WithValues("record", record)
		}
		l.Info("Dropped the outcome of an analysis whose claim was lost", "incident", inc.Name, "run", runID,
			"reason", out.Reason, "request", created)
	case err != nil:
		if record != "" {
			logger.Error(err, "The analysis record was written but the outcome write failed, so its ID is not in the "+
				"status", "incident", inc.Name, "run", runID, "record", record)
		}
		return ctrl.Result{}, fmt.Errorf("writing the analysis outcome of Incident %s: %w", inc.Name, err)
	}
	return ctrl.Result{}, nil
}

// hintStillPending reads, after the claim and before the model call, whether the hint the run answers is
// still Pending (hintPendingRetrying): the executor may have declined it after the requests were listed. When
// it is not, the run ends (done) without a model call or a unit of the budget: the claim is released to
// Proposed with HintNotPending (releaseHintRun). A read that keeps failing ends it with the error and no
// outcome; the Incident stays Analyzing and the trigger takes the run as lost later. Without a hint it does
// nothing.
func (c *Controller) hintStillPending(ctx context.Context, claimed *remediationv1alpha1.Incident,
	hint *incidents.ReviewerHint) (ctrl.Result, bool, error) {
	if hint == nil {
		return ctrl.Result{}, false, nil
	}
	pending, err := c.hintPendingRetrying(ctx, claimed, hint)
	switch {
	case errors.Is(err, errClaimLost):
		return ctrl.Result{}, true, nil
	case err != nil:
		return ctrl.Result{}, true, fmt.Errorf("reading the reviewer hint of Incident %s: %w", claimed.Name, err)
	case pending:
		return ctrl.Result{}, false, nil
	}
	if err := c.releaseHintRun(ctx, claimed); err != nil {
		if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
			return ctrl.Result{RequeueAfter: time.Second}, true, nil
		}
		return ctrl.Result{}, true, fmt.Errorf("releasing the hint run of Incident %s: %w", claimed.Name, err)
	}
	log.FromContext(ctx).Info("Released an analysis whose reviewer hint is no longer pending", "incident", claimed.Name)
	return ctrl.Result{}, true, nil
}

// checkPlan reads, once the model proposed a plan for a hint, whether the hint is still Pending
// (hintPendingRetrying), before anything else is decided about the plan: a hint answered, declined or removed
// while the model ran gets no replacement (HintNotPending), whatever the plan. Then a plan equal to the one of
// the request it would replace creates nothing either (PlanUnchanged, ADR-0028 section 4). A read that keeps
// failing is returned, never turned into an outcome.
func (c *Controller) checkPlan(ctx context.Context, claimed *remediationv1alpha1.Incident, hint *incidents.ReviewerHint,
	out Outcome) (Outcome, error) {
	if out.Plan == nil || hint == nil {
		return out, nil
	}
	pending, err := c.hintPendingRetrying(ctx, claimed, hint)
	switch {
	case err != nil:
		return out, err
	case !pending:
		out.Reason, out.Plan = ReasonHintNotPending, nil
	case samePlan(hint.Request.Spec, requestSpec(claimed, *out.Plan)):
		out.Reason, out.Plan = ReasonPlanUnchanged, nil
	}
	return out, nil
}

// settleHint decides, before the claim, which reviewer hint the run of inc answers (reviewerHint), from
// requests listed live. It ends the reconcile (done) without a claim when there is nothing to run: a lost run
// of a hint whose replacement exists is finished (existingReplacement, finishLostHintRun), a Proposed
// Incident without a pending hint is left alone, and a lost run of a hint no longer Pending is released
// (lostHintGone, releaseHintRun). A conflicting or missing Incident is tried again in a second.
func (c *Controller) settleHint(ctx context.Context, inc *remediationv1alpha1.Incident,
	requests []remediationv1alpha1.RemediationRequest) (*incidents.ReviewerHint, bool, ctrl.Result, error) {
	logger := log.FromContext(ctx)
	written := func(err error, what string) (*incidents.ReviewerHint, bool, ctrl.Result, error) {
		if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
			return nil, true, ctrl.Result{RequeueAfter: time.Second}, nil
		}
		return nil, true, ctrl.Result{}, fmt.Errorf("%s of Incident %s: %w", what, inc.Name, err)
	}
	if repl := c.existingReplacement(inc, requests); repl != nil {
		// A lost run of a reviewer hint that already created its replacement: its outcome is written now,
		// with no model call and no second replacement for the same hint.
		if err := c.finishLostHintRun(ctx, inc, remediationv1alpha1.Name(repl.Name)); err != nil {
			return written(err, "recording the replacement of the lost hint run")
		}
		logger.Info("Recorded the replacement a lost analysis of a reviewer hint created", "incident", inc.Name,
			"request", repl.Name)
		return nil, true, ctrl.Result{}, nil
	}
	hint := c.reviewerHint(inc, requests)
	if hint == nil && inc.Status.Phase == remediationv1alpha1.IncidentProposed {
		// The hint the trigger saw is answered or gone: nothing to claim. The trigger takes the mark back.
		logger.Info("Did not claim a Proposed Incident without a pending reviewer hint", "incident", inc.Name)
		return nil, true, ctrl.Result{}, nil
	}
	if lostHintGone(inc, hint) {
		if err := c.releaseHintRun(ctx, inc); err != nil {
			return written(err, "releasing the lost hint run")
		}
		logger.Info("Released a lost analysis whose reviewer hint is no longer pending", "incident", inc.Name)
		return nil, true, ctrl.Result{}, nil
	}
	return hint, false, ctrl.Result{}, nil
}

// conclude writes the record and then the outcome, and returns the record's ID. When the create failed
// (created empty, attempted its name), it first reads once, live, whether the server created that request
// anyway, so the record and the outcome name the same request or neither does.
func (c *Controller) conclude(ctx context.Context, claimed *remediationv1alpha1.Incident, out Outcome, limits Limits,
	created, attempted remediationv1alpha1.Name) (remediationv1alpha1.RecordID, error) {
	var recovered remediationv1alpha1.Name
	if created == "" && attempted != "" {
		recovered = c.recoverRequest(ctx, claimed, attempted)
	}
	request := created
	if request == "" {
		request = recovered
	}
	record, err := c.storeRecord(ctx, claimed, out, limits, request)
	if err != nil {
		return "", err
	}
	return record, c.finish(ctx, claimed, out, created, recovered, record)
}

// resultFields are the log fields of the result's own words, redacted in the loop and within the result's
// bounds, only those the result has (#422): without an analysis store (#77) the log is the only place a
// hand-off's request is kept.
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

// claimLeft is the time left on the claim once reserve is kept for the writes after this one.
func (c *Controller) claimLeft(claimed *remediationv1alpha1.Incident, reserve time.Duration) time.Duration {
	return c.expiry(claimed).Add(-reserve).Sub(c.now())
}

// withinClaim returns a context bounded by limit and by the claim's expiry less reserve, or errClaimLost when
// no time is left, so no write starts after it. With detach the context keeps no cancellation of ctx (the
// outcome write, which also runs after a stop).
func (c *Controller) withinClaim(ctx context.Context, claimed *remediationv1alpha1.Incident, limit, reserve time.Duration,
	detach bool) (context.Context, context.CancelFunc, error) {
	left := c.claimLeft(claimed, reserve)
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
func (c *Controller) publish(ctx context.Context, claimed *remediationv1alpha1.Incident,
	spec remediationv1alpha1.RemediationRequestSpec) (remediationv1alpha1.Name, error) {
	if err := c.recheckRetrying(ctx, claimed); err != nil {
		return "", err
	}
	cctx, cancel, err := c.withinClaim(ctx, claimed, createTimeout, 0, false)
	if err != nil {
		return "", err
	}
	defer cancel()
	return c.createRequest(cctx, claimed, spec)
}

// hintPendingRetrying reads, within the claim, whether hint is still Pending (hintPending). A read error is
// retried with backoff as in recheckRetrying; only when the claim has no time left for it is the error
// returned, and never taken for an answer: the caller writes no outcome for it.
func (c *Controller) hintPendingRetrying(ctx context.Context, claimed *remediationv1alpha1.Incident,
	hint *incidents.ReviewerHint) (bool, error) {
	var pending bool
	err := c.readRetrying(ctx, claimed, "the request holding a reviewer hint", func(rctx context.Context) error {
		var err error
		pending, err = c.hintPending(rctx, hint)
		return err
	})
	return pending, err
}

// hintPending reads the request holding hint live and reports whether it is still the request read before
// (same UID), in AwaitingApproval with a pull request, and holds the hint, by its pin, in state Pending: the
// executor may have answered, declined or removed it while the model ran.
func (c *Controller) hintPending(ctx context.Context, hint *incidents.ReviewerHint) (bool, error) {
	live := &remediationv1alpha1.RemediationRequest{}
	if err := c.Reader.Get(ctx, client.ObjectKeyFromObject(hint.Request), live); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("reading RemediationRequest %s: %w", hint.Request.Name, err)
	}
	if live.UID != hint.Request.UID || live.Status.Phase != remediationv1alpha1.PhaseAwaitingApproval ||
		live.Status.PullRequest == nil {
		return false, nil
	}
	ref := hint.Ref()
	for i := range live.Status.Hints {
		if h := &live.Status.Hints[i]; incidents.HintRefOf(h) == ref {
			return h.State == remediationv1alpha1.HintPending, nil
		}
	}
	return false, nil
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
	return c.readRetrying(ctx, claimed, "an Incident", func(rctx context.Context) error {
		_, err := c.recheck(rctx, claimed)
		return err
	})
}

// readRetrying runs read, a live read within the claim, retrying an error that is not a lost claim with
// backoff while the claim has time left beyond the create and the outcome write (see recheckRetrying).
func (c *Controller) readRetrying(ctx context.Context, claimed *remediationv1alpha1.Incident, what string,
	read func(context.Context) error) error {
	backoff := recheckFirstBackoff
	for {
		rctx, cancel, err := c.withinClaim(ctx, claimed, createTimeout, 0, false)
		if err != nil {
			return err
		}
		err = read(rctx)
		cancel()
		if err == nil || errors.Is(err, errClaimLost) || ctx.Err() != nil {
			return err
		}
		if c.claimLeft(claimed, c.afterCreate()+createTimeout) < backoff {
			return err
		}
		log.FromContext(ctx).Info("Retrying a live read within the claim", "incident", claimed.Name,
			"read", what, "error", err.Error())
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, recheckMaxBackoff)
	}
}

// incidentRequests lists, live, every request labelled with inc's UID, in pages of maxListedRequests: the
// trigger counts the hints of all the requests its cache holds, so a hint past a cut here would keep the
// Incident due and never be claimed. Without a lastAnalysisTime it lists nothing: such an Incident was never
// analyzed, so no request of it is unrecorded and none holds a hint for it.
func (c *Controller) incidentRequests(ctx context.Context, inc *remediationv1alpha1.Incident) ([]remediationv1alpha1.RemediationRequest, error) {
	if inc.Status.LastAnalysisTime == nil {
		return nil, nil
	}
	var items []remediationv1alpha1.RemediationRequest
	next := ""
	for {
		list := &remediationv1alpha1.RemediationRequestList{}
		if err := c.Reader.List(ctx, list, client.InNamespace(c.Namespace),
			client.MatchingLabels{LabelIncidentUID: string(inc.UID)}, client.Limit(maxListedRequests),
			client.Continue(next)); err != nil {
			return nil, err
		}
		items = append(items, list.Items...)
		if next = list.Continue; next == "" {
			return items, nil
		}
	}
}

// reviewerHint returns the reviewer hint the analysis of inc answers, from requests read live with the form
// an analysis gives a request of inc (IsAnalysisRequest), or nil. A Proposed Incident answers the next
// pending one ([incidents.PendingHint]). An Analyzing Incident (its run taken as lost) answers only the hint
// its claim named in status.handledHint, while that hint is still Pending ([incidents.HintByRef]); a claim
// that named none runs again without a hint, so a retried run never takes a hint the trigger did not mark it
// due for (its cooldown and budget unchecked). The executor stores hints only for a request in
// AwaitingApproval, which keeps its Incident Proposed, so no other phase has one.
func (c *Controller) reviewerHint(inc *remediationv1alpha1.Incident,
	requests []remediationv1alpha1.RemediationRequest) *incidents.ReviewerHint {
	switch inc.Status.Phase {
	case remediationv1alpha1.IncidentProposed:
		return incidents.PendingHint(inc, c.ownRequests(inc, requests))
	case remediationv1alpha1.IncidentAnalyzing:
		if inc.Status.HandledHint == nil {
			return nil
		}
		return incidents.HintByRef(inc, c.ownRequests(inc, requests), *inc.Status.HandledHint)
	}
	return nil
}

// ownRequests returns the requests that have the form an analysis gives a request of inc (IsAnalysisRequest).
func (c *Controller) ownRequests(inc *remediationv1alpha1.Incident,
	requests []remediationv1alpha1.RemediationRequest) []remediationv1alpha1.RemediationRequest {
	own := make([]remediationv1alpha1.RemediationRequest, 0, len(requests))
	for i := range requests {
		if c.IsAnalysisRequest(inc, &requests[i]) {
			own = append(own, requests[i])
		}
	}
	return own
}

// existingReplacement returns, for a lost run of a reviewer hint (phase Analyzing, status.handledHint set), a
// request of inc (IsAnalysisRequest) that already replaces a request for that hint (spec.replaces.hint equal
// to status.handledHint), or nil: the lost run created it before its outcome write was lost. The run is then
// not analyzed again, whatever the hint's state now, so one hint never gets two replacements and the one made
// is recorded (ADR-0028, invariant 5). The trigger marks such a run due without a unit of the budget
// ([incidents.SettlesWithoutAnalysis]), from the same selection.
func (c *Controller) existingReplacement(inc *remediationv1alpha1.Incident,
	requests []remediationv1alpha1.RemediationRequest) *remediationv1alpha1.RemediationRequest {
	handled := inc.Status.HandledHint
	if inc.Status.Phase != remediationv1alpha1.IncidentAnalyzing || handled == nil {
		return nil
	}
	return incidents.ReplacementFor(inc, c.ownRequests(inc, requests), *handled)
}

// finishLostHintRun writes the outcome of a lost hint run whose replacement exists (existingReplacement), with
// the resourceVersion read: the phase Proposed, the replacement recorded in status.remediationRequests (once),
// the Analyzed condition Proposed, and status.handledHint and lastAnalysisTime kept. No model call, no unit of
// the budget and no request: the lost run already took its unit and made its request. The AnalysisDue mark is
// left to the trigger, as in releaseHintRun.
func (c *Controller) finishLostHintRun(ctx context.Context, inc *remediationv1alpha1.Incident,
	request remediationv1alpha1.Name) error {
	done := inc.DeepCopy()
	now := metav1.NewTime(c.now().UTC().Truncate(time.Second))
	done.Status.Phase = remediationv1alpha1.IncidentProposed
	if !slices.Contains(done.Status.RemediationRequests, request) {
		done.Status.RemediationRequests = appendBounded(done.Status.RemediationRequests, request, maxRecorded)
	}
	meta.SetStatusCondition(&done.Status.Conditions, metav1.Condition{
		Type: ConditionAnalyzed, Status: metav1.ConditionTrue, Reason: string(ReasonProposed),
		Message: outcomeMessages[ReasonProposed], ObservedGeneration: done.Generation, LastTransitionTime: now,
	})
	return c.Client.Status().Update(ctx, done, client.FieldOwner(FieldOwner))
}

// lostHintGone reports whether inc is a lost run of a reviewer hint (phase Analyzing, its claim taken as lost,
// status.handledHint set) whose hint is no longer Pending (hint, read live by reviewerHint, is nil): it was
// answered, declined or removed meanwhile. Such a run is not run again: a Proposed Incident is analyzed again
// only for a pending hint (ADR-0028), so a rerun without it would be a model call nobody asked for, and could
// create a replacement with no hint behind it.
func lostHintGone(inc *remediationv1alpha1.Incident, hint *incidents.ReviewerHint) bool {
	return inc.Status.Phase == remediationv1alpha1.IncidentAnalyzing && inc.Status.HandledHint != nil && hint == nil
}

// releaseHintRun ends a run of a hint that is no longer Pending without an analysis: a lost run
// ([lostHintGone]), or one just claimed whose hint the executor moved on meanwhile (hintStillPending). No model
// call, no unit of the budget, no request. It writes, with the resourceVersion read, the phase Proposed (the
// request the hint was given on, or its successor, still decides the Incident through the closure rules),
// keeps status.handledHint and lastAnalysisTime, and sets the Analyzed condition to HintNotPending, so the
// executor does not wait on the hint.
func (c *Controller) releaseHintRun(ctx context.Context, inc *remediationv1alpha1.Incident) error {
	released := inc.DeepCopy()
	now := metav1.NewTime(c.now().UTC().Truncate(time.Second))
	released.Status.Phase = remediationv1alpha1.IncidentProposed
	meta.SetStatusCondition(&released.Status.Conditions, metav1.Condition{
		Type: ConditionAnalyzed, Status: metav1.ConditionTrue, Reason: string(ReasonHintNotPending),
		Message: outcomeMessages[ReasonHintNotPending], ObservedGeneration: released.Generation, LastTransitionTime: now,
	})
	// The AnalysisDue mark is left to the trigger, which writes Proposed over it on its next pass; meanwhile a
	// Proposed Incident marked AnalysisTimedOut is not claimable.
	return c.Client.Status().Update(ctx, released, client.FieldOwner(FieldOwner))
}

// unrecordedRequests returns at most one of requests (inc's, listed live) that inc's status does not record
// yet: a request the run of inc's lastAnalysisTime created before its outcome write was lost (ADR-0025
// decision 6). Only a request created within that run's claim window (from lastAnalysisTime to its expiry)
// counts, so a name the status dropped at its bound never comes back, and only one per run, which creates at
// most one. The label and spec are set by whoever creates the request, so it must also have the form an
// analysis gives it (IsAnalysisRequest). Without a lastAnalysisTime nothing counts.
func (c *Controller) unrecordedRequests(inc *remediationv1alpha1.Incident,
	requests []remediationv1alpha1.RemediationRequest) []remediationv1alpha1.Name {
	if inc.Status.LastAnalysisTime == nil {
		return nil
	}
	start, end := inc.Status.LastAnalysisTime.Time, c.expiry(inc)
	var found *remediationv1alpha1.RemediationRequest
	for i := range requests {
		rr := &requests[i]
		created := rr.CreationTimestamp.Time
		if created.Before(start) || !created.Before(end) ||
			slices.Contains(inc.Status.RemediationRequests, remediationv1alpha1.Name(rr.Name)) ||
			!c.IsAnalysisRequest(inc, rr) {
			continue
		}
		// The earliest, then the first by name, for a stable choice.
		if found == nil || created.Before(found.CreationTimestamp.Time) ||
			(created.Equal(found.CreationTimestamp.Time) && rr.Name < found.Name) {
			found = rr
		}
	}
	if found == nil {
		return nil
	}
	return []remediationv1alpha1.Name{remediationv1alpha1.Name(found.Name)}
}

// IsAnalysisRequest reports whether rr has the form the analysis gives a request of inc: the label
// LabelIncidentUID with inc's UID, and a spec that names inc
// and its subject, GitOps mode, approval required, the policy configured for its action type, and the name
// derived from inc's UID and that spec (requestName), spec.replaces and spec.chainRoot included (a
// replacement's, ADR-0028; the link itself is checked by the executor at admission, never here). It proves
// nothing about who created rr: whoever may
// create RemediationRequests in the operator namespace can still forge one (threat model G5.6).
// The trigger selects the requests whose hints mark an Incident due with this same predicate (main.go), so it
// never counts a hint the analysis would not answer.
func (c *Controller) IsAnalysisRequest(inc *remediationv1alpha1.Incident, rr *remediationv1alpha1.RemediationRequest) bool {
	s := rr.Spec
	if rr.Labels[LabelIncidentUID] != string(inc.UID) {
		return false
	}
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
	spec := requestSpec(inc, Plan{Action: s.Action, Checks: s.Verification.Checks, PolicyRef: s.PolicyRef})
	spec.Replaces, spec.ChainRoot = s.Replaces, s.ChainRoot
	name, err := requestName(inc, spec)
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
// another write came first, so the claim is not made. The same update records the requests in unrecorded,
// and status.handledHint: the pin of the reviewer hint the run answers, or none (ADR-0028 section 3). The
// hint is thus claimed with the run, so a restart finds the claim or the outcome with it and does not run it
// twice ([incidents.PendingHint]).
func (c *Controller) claim(ctx context.Context, inc *remediationv1alpha1.Incident,
	unrecorded []remediationv1alpha1.Name, hint *incidents.ReviewerHint) (*remediationv1alpha1.Incident, error) {
	claimed := inc.DeepCopy()
	for _, name := range unrecorded {
		claimed.Status.RemediationRequests = appendBounded(claimed.Status.RemediationRequests, name, maxRecorded)
	}
	now := metav1.NewTime(c.now().UTC().Truncate(time.Second))
	claimed.Status.Phase = remediationv1alpha1.IncidentAnalyzing
	claimed.Status.LastAnalysisTime = &now
	claimed.Status.HandledHint = nil
	if hint != nil {
		ref := hint.Ref()
		claimed.Status.HandledHint = &ref
	}
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

// withReplacement returns spec as the replacement of the request that holds hint (ADR-0028 section 4):
// spec.replaces names that request by name and UID and pins the hint, and spec.chainRoot is that request's
// chain root, or its own UID when it has none. Both are part of the request's name, so each attempt (a plan
// answering one hint of one predecessor) is a request of its own, even with the plan of an earlier request of
// the chain, while a retry of the same attempt finds the request it made (invariant 5). Without a hint spec is
// returned as it is.
func withReplacement(spec remediationv1alpha1.RemediationRequestSpec, hint *incidents.ReviewerHint) remediationv1alpha1.RemediationRequestSpec {
	if hint == nil {
		return spec
	}
	pred := hint.Request
	spec.Replaces = &remediationv1alpha1.RequestReplacement{
		Name: remediationv1alpha1.Name(pred.Name), UID: remediationv1alpha1.UID(pred.UID), Hint: hint.Ref(),
	}
	spec.ChainRoot = pred.Spec.ChainRoot
	if spec.ChainRoot == "" {
		spec.ChainRoot = remediationv1alpha1.UID(pred.UID)
	}
	return spec
}

// samePlan reports whether spec holds the plan of the request whose spec is old: the same Incident, target,
// policy, action and checks.
func samePlan(old, spec remediationv1alpha1.RemediationRequestSpec) bool {
	return old.IncidentRef == spec.IncidentRef && old.TargetRef == spec.TargetRef && old.PolicyRef == spec.PolicyRef &&
		equality.Semantic.DeepEqual(old.Action, spec.Action) &&
		equality.Semantic.DeepEqual(old.Verification.Checks, spec.Verification.Checks)
}

// requestSpec is the spec of the request of a plan: nothing in it comes from the model but the action and the
// extra checks, which the loop validated. It never sets spec.reverts: a model proposal has no such field, and
// only the analyzer's deterministic revert proposer makes a revert (ADR-0030 section 8).
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

// createRequest creates the RemediationRequest of spec (requestSpec, and withReplacement for a reviewer hint)
// in the operator namespace: the Incident's subject as the target, approval required, the configured policy.
// It is a proposal only (invariant 1); the executor's policy check decides it. An existing request of the same
// name is accepted only when it holds the same plan for this Incident, replacing the same request for the same
// hint.
func (c *Controller) createRequest(ctx context.Context, inc *remediationv1alpha1.Incident,
	spec remediationv1alpha1.RemediationRequestSpec) (remediationv1alpha1.Name, error) {
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
			!equality.Semantic.DeepEqual(e.Verification.Checks, spec.Verification.Checks) ||
			!equality.Semantic.DeepEqual(e.Replaces, spec.Replaces) || e.ChainRoot != spec.ChainRoot ||
			e.Reverts != nil || spec.Reverts != nil {
			// A revert (spec.reverts) is made only by the deterministic revert proposer, never by this loop.
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
	ReasonPreviousValueUnknown:    "The previous value could not be taken from the subject's pod template (not read, no such container, or no memory limit set); handed off to a person",
	ReasonNoChange:                "The proposed value is the one the subject's pod template already sets; handed off to a person",
	ReasonDirectionReversed:       "The pod template's current value reverses the direction of the proposed change; handed off to a person",
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
	ReasonPlanUnchanged:           "The analysis with a reviewer hint proposed the plan of the request it would replace; no replacement was created",
	ReasonHintNotPending:          "The reviewer hint was no longer pending; no replacement was created for it",
}

// finish writes the outcome in one status update: Proposed with the request's name, or Escalated, and the
// Analyzed condition. A run with a reviewer hint (status.handledHint) that created no request goes back to
// Proposed instead of Escalated: the request the hint was given on still stands, its pull request untouched
// (ADR-0028 section 4), and the closure rules decide the Incident by it as before. status.handledHint, written
// by the claim, stays with the outcome, so the executor can match the outcome to the hint. It rechecks the claim live and writes with the resourceVersion of that read, retrying
// on conflict with a new recheck, so a stale run never overwrites a newer one; it returns errClaimLost and
// writes nothing once the run no longer owns the claim. It runs with its own timeout, cut to the time left on
// the claim, after the analysis deadline and a stop too. recovered is the run's own request of a failed
// create that the server created anyway (recoverRequest), recorded without the phase Proposed.
func (c *Controller) finish(ctx context.Context, claimed *remediationv1alpha1.Incident, out Outcome,
	request, recovered remediationv1alpha1.Name, record remediationv1alpha1.RecordID) error {
	wctx, cancel, err := c.withinClaim(ctx, claimed, finalWriteTimeout, 0, true)
	if err != nil {
		return err
	}
	defer cancel()
	// A conflict is retried with a new recheck, and so is a transient error of the API server or of the
	// connection to it (transientAPIError) while the write's own timeout lasts, so a passing failure does not
	// lose the outcome and the record ID with it (the next run has another ID). A status update that failed so
	// may still have been applied: from then on (uncertain, never reset) each try first looks whether the
	// outcome is there, so it is not taken for a lost claim; a failed look is retried like a failed write. A
	// failed read before the update sends nothing, so it does not make the write uncertain.
	uncertain := false
	return retry.OnError(finishBackoff, func(err error) bool {
		return apierrors.IsConflict(err) || (wctx.Err() == nil && transientAPIError(err))
	}, func() error {
		if uncertain {
			written, err := c.outcomeWritten(wctx, claimed, record)
			if err != nil || written {
				return err
			}
		}
		inc, err := c.recheck(wctx, claimed)
		if err != nil {
			return err
		}
		inc.Status.Phase = remediationv1alpha1.IncidentEscalated
		if inc.Status.HandledHint != nil {
			inc.Status.Phase = remediationv1alpha1.IncidentProposed
		}
		if record != "" {
			inc.Status.AnalysisRecords = appendBounded(inc.Status.AnalysisRecords, record,
				remediationv1alpha1.MaxAnalysisRecords)
		}
		if recovered != "" {
			inc.Status.RemediationRequests = appendBounded(inc.Status.RemediationRequests, recovered, maxRecorded)
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
		err = c.Client.Status().Update(wctx, inc, client.FieldOwner(FieldOwner))
		if wctx.Err() == nil && transientAPIError(err) {
			uncertain = true
		}
		return err
	})
}

// outcomeWritten reports whether the live Incident holds the outcome of the run: its lastAnalysisTime, a
// final phase and, when there is one, the record ID. A read error is returned, so that an outcome that may be
// there is never taken for a lost claim.
func (c *Controller) outcomeWritten(ctx context.Context, claimed *remediationv1alpha1.Incident,
	record remediationv1alpha1.RecordID) (bool, error) {
	live := &remediationv1alpha1.Incident{}
	if err := c.Reader.Get(ctx, client.ObjectKeyFromObject(claimed), live); err != nil {
		if apierrors.IsNotFound(err) {
			return false, errClaimLost
		}
		return false, fmt.Errorf("reading Incident %s: %w", claimed.Name, err)
	}
	return live.UID == claimed.UID && live.Status.LastAnalysisTime != nil &&
		live.Status.LastAnalysisTime.Equal(claimed.Status.LastAnalysisTime) &&
		(live.Status.Phase == remediationv1alpha1.IncidentProposed || live.Status.Phase == remediationv1alpha1.IncidentEscalated) &&
		(record == "" || slices.Contains(live.Status.AnalysisRecords, record)), nil
}

// finishBackoff spaces the tries of the outcome write: about 3 s in all, well inside finalWriteTimeout.
var finishBackoff = wait.Backoff{Steps: 5, Duration: 200 * time.Millisecond, Factor: 2, Jitter: 0.1}

// transientAPIError reports whether err is a failure that the same call may get past: a timeout, throttling
// or an unavailable API server, or a connection to it that closed or was reset before an answer came (the
// call may still have been applied).
func transientAPIError(err error) bool {
	return apierrors.IsServerTimeout(err) || apierrors.IsTimeout(err) || apierrors.IsTooManyRequests(err) ||
		apierrors.IsServiceUnavailable(err) || apierrors.IsInternalError(err) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || utilnet.IsProbableEOF(err) ||
		utilnet.IsConnectionReset(err)
}

// recoverRequest returns name when the request of a failed create exists, read live, with the form the
// analysis gives a request of claimed, and "" otherwise. A transient read error is retried within
// recoverTimeout, cut to the time left on the claim before the outcome write's timeout; it runs after a stop
// too, as the outcome write does. Any other read error, or no time left, is logged and counts as no: a later
// claim's record step finds the request.
func (c *Controller) recoverRequest(ctx context.Context, claimed *remediationv1alpha1.Incident,
	name remediationv1alpha1.Name) remediationv1alpha1.Name {
	logger := log.FromContext(ctx).WithValues("incident", claimed.Name, "request", name)
	rctx, cancel, err := c.withinClaim(ctx, claimed, recoverTimeout, finalWriteTimeout, true)
	if err != nil {
		logger.Info("Did not read the RemediationRequest of a failed create: no time is left on the claim")
		return ""
	}
	defer cancel()
	rr := &remediationv1alpha1.RemediationRequest{}
	err = retry.OnError(finishBackoff, func(err error) bool {
		return rctx.Err() == nil && transientAPIError(err)
	}, func() error {
		return c.Reader.Get(rctx, client.ObjectKey{Namespace: c.Namespace, Name: string(name)}, rr)
	})
	if err != nil {
		if !apierrors.IsNotFound(err) {
			logger.Error(err, "Failed to read the RemediationRequest of a failed create")
		}
		return ""
	}
	if !c.IsAnalysisRequest(claimed, rr) {
		return ""
	}
	return name
}

// appendBounded appends name unless present and keeps the newest n.
func appendBounded[T comparable](names []T, name T, n int) []T {
	if slices.Contains(names, name) {
		return names
	}
	names = append(names, name)
	if len(names) > n {
		names = names[len(names)-n:]
	}
	return names
}
