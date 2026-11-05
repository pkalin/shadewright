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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

// finalWriteTimeout bounds the write of the outcome, which also runs after the analysis deadline.
const finalWriteTimeout = 30 * time.Second

// Controller runs the analysis of the Incidents the trigger marks due (design 3.2 and 3.3). It claims an
// Incident by writing the phase Analyzing, lastAnalysisTime and the AnalysisDue condition (not due, reason
// Analyzing) in one status update with the resourceVersion it read, so two runs never claim the same round.
// It then runs the [Analyzer], creates the RemediationRequest of a plan, and writes the phase Proposed or
// Escalated with the Analyzed condition and the request's name in one more status update.
type Controller struct {
	// Client reads Incidents and RemediationSettings from the cache and writes the Incident status and the
	// requests.
	Client client.Client
	// Namespace is the operator namespace: the Incidents' and the requests'.
	Namespace string
	// Analyzer runs the analysis.
	Analyzer *Analyzer
	// Now returns the current time; nil means time.Now.
	Now func() time.Time

	mu       sync.Mutex
	inFlight int
}

// SetupWithManager registers the controller. It runs on the leader only and looks at Incidents in the
// operator namespace whose AnalysisDue condition is True.
func (c *Controller) SetupWithManager(mgr ctrl.Manager) error {
	due := predicate.NewPredicateFuncs(func(o client.Object) bool {
		inc, ok := o.(*remediationv1alpha1.Incident)
		return ok && inc.Namespace == c.Namespace &&
			meta.IsStatusConditionTrue(inc.Status.Conditions, incidents.ConditionAnalysisDue)
	})
	return ctrl.NewControllerManagedBy(mgr).
		Named("analysis").
		For(&remediationv1alpha1.Incident{}, builder.WithPredicates(due)).
		WithOptions(controller.Options{MaxConcurrentReconciles: maxWorkers}).
		Complete(c)
}

func (c *Controller) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// claimable reports whether the trigger marked inc due in a phase an analysis may start from: Open (or none)
// or Analyzing taken as lost (reason AnalysisTimedOut). Never Resolved: reopening a Resolved Incident is a
// later part of #251, and until it is built a Resolved Incident is not analyzed again.
func claimable(inc *remediationv1alpha1.Incident) bool {
	cond := meta.FindStatusCondition(inc.Status.Conditions, incidents.ConditionAnalysisDue)
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

	claimed, err := c.claim(ctx, inc)
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
	var requestName remediationv1alpha1.Name
	if out.Plan != nil {
		requestName, err = c.createRequest(ctx, claimed, *out.Plan)
		if err != nil {
			if ctx.Err() != nil {
				return ctrl.Result{}, ctx.Err()
			}
			logger.Error(err, "Failed to create the RemediationRequest of an analysis", "incident", inc.Name)
			out.Reason, out.Plan = ReasonRequestNotCreated, nil
		}
	}
	logger.Info("Analyzed Incident", "incident", inc.Name, "reason", out.Reason, "escalated", out.Escalated,
		"calls", out.Calls, "toolCalls", out.ToolCalls, "tokens", out.Tokens, "models", out.Models,
		"promptVersion", out.PromptVersion, "request", requestName)
	if err := c.finish(ctx, claimed, out, requestName); err != nil {
		return ctrl.Result{}, fmt.Errorf("writing the analysis outcome of Incident %s: %w", inc.Name, err)
	}
	return ctrl.Result{}, nil
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
// another write came first, so the claim is not made.
func (c *Controller) claim(ctx context.Context, inc *remediationv1alpha1.Incident) (*remediationv1alpha1.Incident, error) {
	claimed := inc.DeepCopy()
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
// of adding another (invariant 5).
func requestName(inc *remediationv1alpha1.Incident, spec remediationv1alpha1.RemediationRequestSpec) (remediationv1alpha1.Name, error) {
	b, err := json.Marshal(spec)
	if err != nil {
		return "", fmt.Errorf("encoding the request spec: %w", err)
	}
	h := sha256.Sum256(append([]byte(string(inc.UID)+"/"), b...))
	return remediationv1alpha1.Name("remediation-" + hex.EncodeToString(h[:12])), nil
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
		ObjectMeta: metav1.ObjectMeta{Namespace: c.Namespace, Name: string(name)},
		Spec:       *spec.DeepCopy(),
	}
	err = c.Client.Create(ctx, rr, client.FieldOwner(FieldOwner))
	if apierrors.IsAlreadyExists(err) {
		existing := &remediationv1alpha1.RemediationRequest{}
		if gerr := c.Client.Get(ctx, client.ObjectKeyFromObject(rr), existing); gerr != nil {
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
}

// finish writes the outcome in one status update: Proposed with the request's name, or Escalated, and the
// Analyzed condition. It reads the Incident fresh and retries on conflict, and writes nothing when the round
// it claimed is no longer the Incident's (another phase or another lastAnalysisTime). It runs with its own
// timeout, after the analysis deadline too.
func (c *Controller) finish(ctx context.Context, claimed *remediationv1alpha1.Incident, out Outcome,
	request remediationv1alpha1.Name) error {
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finalWriteTimeout)
	defer cancel()
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		inc := &remediationv1alpha1.Incident{}
		if err := c.Client.Get(wctx, client.ObjectKeyFromObject(claimed), inc); err != nil {
			return client.IgnoreNotFound(err)
		}
		if inc.Status.Phase != remediationv1alpha1.IncidentAnalyzing || inc.Status.LastAnalysisTime == nil ||
			!inc.Status.LastAnalysisTime.Equal(claimed.Status.LastAnalysisTime) || inc.UID != claimed.UID {
			return nil
		}
		inc.Status.Phase = remediationv1alpha1.IncidentEscalated
		if out.Plan != nil && request != "" {
			inc.Status.Phase = remediationv1alpha1.IncidentProposed
			inc.Status.RemediationRequests = appendBounded(inc.Status.RemediationRequests, request, 20)
		}
		meta.SetStatusCondition(&inc.Status.Conditions, metav1.Condition{
			Type: ConditionAnalyzed, Status: metav1.ConditionTrue, Reason: string(out.Reason),
			Message: outcomeMessages[out.Reason], ObservedGeneration: inc.Generation,
			LastTransitionTime: metav1.NewTime(c.now().UTC().Truncate(time.Second)),
		})
		return c.Client.Status().Update(wctx, inc, client.FieldOwner(FieldOwner))
	})
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
