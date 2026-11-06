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

package controller

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/gitops/sourcemap"
	"github.com/pkalin/shadewright/internal/policy"
	"github.com/pkalin/shadewright/internal/policy/freshness"
	"github.com/pkalin/shadewright/internal/providers"
)

// The repository settings cannot be used to open a Git provider: permanent, so the request stops.
const (
	reasonRepositoryUnusable  = "RepositoryUnusable"
	messageRepositoryUnusable = "The plan cannot be checked and its change request, if any, was not closed: " +
		"the settings of its repository cannot be used to open it"
)

// Reasons the controller itself gives for a refusal, next to the reasons of package policy and sourcemap.
const (
	// ReasonPolicyNotFound: the policy the request names does not exist (Denied).
	ReasonPolicyNotFound = "PolicyNotFound"
	// ReasonSettingsNotFound: the RemediationSettings object does not exist (Denied).
	ReasonSettingsNotFound = "SettingsNotFound"
	// ReasonTargetMismatch: the live target has another UID than the request names (Denied).
	ReasonTargetMismatch = "TargetMismatch"
	// ReasonPreviousValueMismatch means the previous value the request claims is not the live value, so
	// the policy's increase and decrease limits would be measured from a wrong baseline.
	ReasonPreviousValueMismatch = "PreviousValueMismatch"
)

// SourceResolver maps the target to its file in Git (ADR-0012). [sourcemap.Resolver] implements it.
type SourceResolver interface {
	Resolve(ctx context.Context, in sourcemap.Input) (sourcemap.Result, error)
}

// RemediationRequestReconciler reconciles a RemediationRequest object. It runs the policy check phase: a
// Pending request becomes PolicyChecked, Denied or NeedsIntervention, and later phases are left to other steps.
type RemediationRequestReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Namespace is the operator namespace, where the requests are. The phase metric counts the requests there.
	// SetupWithManager requires it; Reconcile does not read it.
	Namespace string
	// Source resolves the target's declaration in Git.
	Source SourceResolver
	// APIReader reads straight from the API server (mgr.GetAPIReader()). The live target is read through it,
	// so the executor needs only get on target kinds and never starts an informer that would need list and
	// watch; and the policy and settings are read through it, so a decision never rests on a stale cached
	// copy. Nil means the client is used (tests).
	APIReader client.Reader
	// OpenRepository opens the Git repository a settings entry names. It is used to re-check a plan before a
	// write and to close the change request of a superseded one. cmd/main.go sets it from the provider
	// registry; nil (tests without a Git adapter) disables the freshness step of Reconcile, and
	// the freshness check (ensureFresh) then fails closed.
	OpenRepository freshness.RepositoryOpener
	// CheckTimeout bounds the policy check phase: the uncached reads and the source resolver together. Zero
	// means [DefaultCheckTimeout].
	CheckTimeout time.Duration
	// CleanupTimeout bounds each of the freshness check and the cleanup of a stale plan (open the repository,
	// find and close the change request), as a whole. Zero means [DefaultCleanupTimeout].
	CleanupTimeout time.Duration
	// WriteTimeout bounds one Git step of the pull request phase (rebuild the state, one write). Zero means
	// [DefaultWriteTimeout].
	WriteTimeout time.Duration
	// IncidentReadTimeout bounds the read of the Incident a request names, made before the pull request is
	// opened to decide whether its body may show the name (verifiedIncidentName). It runs inside the Git step's
	// deadline and takes at most half of what is left of it, so a stalled read cannot use that up; on timeout
	// the name is redacted. Zero means [DefaultIncidentReadTimeout].
	IncidentReadTimeout time.Duration
	// PullRequestTimeout is how long a request may take from PolicyChecked to an open pull request; it is
	// stored in status.deadline. Zero means [DefaultPullRequestTimeout].
	PullRequestTimeout time.Duration
	// TargetStateTimeout is how long the policy check waits for a target whose state is not known yet (a
	// ScaledObject KEDA has not reported on) before it ends the request in NeedsIntervention. Zero means
	// [DefaultTargetStateTimeout].
	TargetStateTimeout time.Duration
	// Now is the clock; nil means time.Now (tests set it).
	Now func() time.Time
	// Recorder records the kill switch's Events about a request (WritesPaused, WritesResumed). They are core/v1
	// Events in the request's namespace, the operator namespace, where the executor's leader election Role
	// grants create and patch on Events. Nil records none (tests).
	Recorder record.EventRecorder
	// BreakerThreshold is how many consecutive failed requests open the circuit breaker (limits.go). Zero means
	// [DefaultBreakerThreshold].
	BreakerThreshold int
	// BreakerCooldown is how long the circuit breaker stays open after the latest of those failures. Zero means
	// [DefaultBreakerCooldown].
	BreakerCooldown time.Duration
	// PullRequestPollInterval is how often a request whose pull request waits for review (PRCreated,
	// AwaitingApproval) looks at that pull request alone, between the full rechecks every freshRecheckInterval,
	// so a merge or a close is seen within it. Zero means [DefaultPullRequestPollInterval]; a value outside
	// [MinPullRequestPollInterval, MaxPullRequestPollInterval] is clamped to it.
	PullRequestPollInterval time.Duration

	// admitMu makes the check of the limits and the status write that lets a request through the policy check
	// one step, so two workers can never both take the last slot or the same target.
	admitMu sync.Mutex
	// checked holds, per request name, when the last full recheck of a waiting pull request ended (pollState).
	// It is memory only: after a restart or a change of leader every request gets a full recheck first.
	checked sync.Map
}

// DefaultTargetStateTimeout is how long the policy check waits by default for KEDA to report on a
// ScaledObject. KEDA reconciles a new or changed ScaledObject within seconds; ten minutes covers an operator
// restart without leaving a request undecided for long.
const DefaultTargetStateTimeout = 10 * time.Minute

// Retry delays while the policy check waits for a target's state: the delay doubles with the time waited, from
// targetStateMinDelay to targetStateMaxDelay, and never passes the end of TargetStateTimeout.
const (
	targetStateMinDelay = 15 * time.Second
	targetStateMaxDelay = 2 * time.Minute
)

// messageTargetStateTimeout is the fixed message of a request whose target's state stayed unknown too long.
const messageTargetStateTimeout = "KEDA did not report the ScaledObject ready or not ready within the time the " +
	"policy check waits; a person must look"

// Default deadlines of the phases that call external systems. Reconcile workers are bounded, so a stalled
// read must not hold one for long.
const (
	DefaultCheckTimeout   = 2 * time.Minute
	DefaultCleanupTimeout = 2 * time.Minute
)

// ErrTimeout means a phase reached its own deadline while the reconcile was still wanted. It is retryable: the
// request keeps its phase and is tried again with backoff, never ended. A cancelled reconcile context is a
// different error (context.Canceled).
var ErrTimeout = errors.New("the step reached its deadline")

// bound returns a context with the deadline d (def when d is zero). The defaults of the phases happen to be
// equal today; each phase still names its own.
//
//nolint:unparam // def is the phase's own default, see above
func bound(ctx context.Context, d, def time.Duration) (context.Context, context.CancelFunc) {
	if d <= 0 {
		d = def
	}
	return context.WithTimeout(ctx, d)
}

// boundErr tells the caller's cancellation from the phase's own deadline. parent is the reconcile context,
// phase the bounded one derived from it. It returns err unchanged when neither ended; when the phase deadline
// ended it, the result matches ErrTimeout (and context.DeadlineExceeded only through the original error).
func boundErr(parent, phase context.Context, name string, err error) error {
	switch {
	case parent.Err() != nil:
		// The callee's error is sanitised and may have lost context.Canceled, so the parent's error is wrapped.
		return fmt.Errorf("%s cancelled: %w", name, parent.Err())
	case phase.Err() != nil:
		return fmt.Errorf("%s: %w", name, ErrTimeout)
	}
	return err
}

// ErrSuperseded is returned by ensureFresh when the plan went stale: the request is now Superseded and its
// change request is closed. The caller must not write.
var ErrSuperseded = errors.New("the plan is superseded")

// freshRecheckDelay is how long a request waits to be re-checked when writes are paused.
const freshRecheckDelay = time.Minute

// freshRecheckInterval is how often a fresh request before merge gets a full recheck (the freshness check and,
// while its pull request waits, the render check). It bounds how long a stale plan keeps an open change request
// when nothing else wakes the request. A waiting pull request is looked at more often (PullRequestPollInterval).
const freshRecheckInterval = 5 * time.Minute

// Bounds of PullRequestPollInterval. One look reads the settings from the API server and lists the pull
// requests of the request's branch on the Git host (one GitHub REST call); a full recheck makes about eight
// (the look, three reads of the target branch head and the target file: its tree, one per directory and the
// blob). The lower bound keeps a waiting pull request to at most 240 looks an hour; the upper bound is
// freshRecheckInterval, where every look is a full recheck.
const (
	// DefaultPullRequestPollInterval is the poll interval of a waiting pull request by default: a merge is seen
	// within about a minute, for about 150 GitHub REST calls an hour per waiting pull request (48 looks and 12
	// full rechecks), against about 100 with full rechecks only.
	DefaultPullRequestPollInterval = time.Minute
	// MinPullRequestPollInterval is the shortest poll interval.
	MinPullRequestPollInterval = 15 * time.Second
	// MaxPullRequestPollInterval is the longest poll interval: every look is then a full recheck, as before #434.
	MaxPullRequestPollInterval = freshRecheckInterval
)

// errRepositoryGone means the repository a plan was made for is not in the settings any more (or was never
// recorded), so its change request cannot be closed safely.
var errRepositoryGone = errors.New("the repository of the plan is not configured")

// errChangeRequestNotOurs means a pull request is recorded, but the branch's pull request is not that number
// with this request's marker (or there is none), so the recorded number is not closed.
var errChangeRequestNotOurs = errors.New("the recorded change request is not the request's own")

// errTargetBranchChanged means the repository entry of the plan now names another target branch. Its provider
// is bound to the new branch and must not touch a change request against the old one, so nothing is closed.
var errTargetBranchChanged = errors.New("the target branch of the plan's repository changed")

// reader returns the uncached reader, or the client when none is set.
func (r *RemediationRequestReconciler) reader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// getLive reads a cluster-scoped authorizing object (the policy or the settings) straight from the API server.
// The cache is not used: a cached copy can still hold a deleted policy, an older approval mode or a repository
// removed from the settings, and the request's resourceVersion does not change when either of them does, so a
// stale copy would authorize a request the live configuration refuses. A policy created together with the
// request is found as well.
func (r *RemediationRequestReconciler) getLive(ctx context.Context, key types.NamespacedName, obj client.Object) error {
	return r.reader().Get(ctx, key, obj)
}

// Reconcile runs the policy check on a Pending request (design section 6, step 1 to 6).
//
// It is idempotent (invariant 5): only a request that has no phase or is Pending is looked at, the outcome is
// written once through the status subresource with the resourceVersion that was read, and a request that
// already has a phase is left untouched. A stale cache read therefore ends in a conflict, never in a second
// decision. Only deterministic code decides (invariant 1): the request's own fields never lower the policy.
//
// A missing policy or settings object and a target with another UID end the request as Denied at once, so a
// permanent refusal never becomes a retry loop. A target whose state is not known yet (a ScaledObject KEDA has
// not reported on) is checked again after a delay, with PolicyAllowed Unknown and no phase, for at most
// TargetStateTimeout. Only errors worth retrying (an API or Git host failure) are
// returned.
func (r *RemediationRequestReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	rr := &remediationv1alpha1.RemediationRequest{}
	if err := r.Get(ctx, req.NamespacedName, rr); err != nil {
		if apierrors.IsNotFound(err) {
			r.checked.Delete(req.NamespacedName)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !rr.DeletionTimestamp.IsZero() {
		r.checked.Delete(req.NamespacedName)
		return r.finalize(ctx, rr)
	}
	if err := r.backfillLock(ctx, rr); err != nil {
		return ctrl.Result{}, err
	}
	if rr.Status.Phase != "" && rr.Status.Phase != remediationv1alpha1.PhasePending {
		if !progresses(rr) {
			r.checked.Delete(req.NamespacedName)
			// Stopped: nothing is written to Git any more, but a lock still held waits for its pull request.
			return r.checkHeld(ctx, rr)
		}
		return r.recheck(ctx, rr)
	}
	// A request a limit holds back (Queued) checks only the limits until they let it through, so it does not
	// read Git while it waits. Every other request is decided first, so a refusal is never held back.
	if meta.IsStatusConditionTrue(rr.Status.Conditions, remediationv1alpha1.ConditionQueued) {
		if res, _, stop, err := r.holdBack(ctx, rr, true); stop {
			return res, err
		}
	}

	checkCtx, cancel := bound(ctx, r.CheckTimeout, DefaultCheckTimeout)
	out, err := r.check(checkCtx, rr)
	// A decision made after the deadline is not trusted: a read or the resolver may have been cut short.
	err = boundErr(ctx, checkCtx, "policy check", err)
	cancel()
	if reason, message, ok := refusal(err); ok {
		// The Git host refused the source mapping's reads for good (a permission the App lacks, a rule of the
		// repository): a person has to act, so the request is decided now instead of retried without end.
		out, err = needsIntervention(reason, message), nil
	}
	if err == nil && out.wait {
		var wait time.Duration
		if out, wait = r.waitForTarget(rr, out); wait > 0 {
			return r.recordWait(ctx, rr, out, wait)
		}
	}
	if err != nil {
		// A remote system that asked for a delay (Retry-After) gets it; controller-runtime's error backoff
		// does not read it and could retry sooner.
		if te, ok := errors.AsType[*sourcemap.TransientError](err); ok && te.RetryAfter > 0 {
			log.Info("Waiting before retrying RemediationRequest", "remediationRequest", req.NamespacedName,
				"error", err.Error(), "retryAfter", te.RetryAfter)
			return ctrl.Result{RequeueAfter: te.RetryAfter}, nil
		}
		return ctrl.Result{}, err
	}

	if out.phase == remediationv1alpha1.PhasePolicyChecked {
		// The limits are checked again, now, and the decision is written under the same lock, so the next
		// check of the limits sees this request as active, and as the breaker's probe when it is one. A
		// conflict on that write lets nothing through and marks nothing: the request is decided again.
		r.admitMu.Lock()
		defer r.admitMu.Unlock()
		res, probe, stop, err := r.holdBack(ctx, rr, false)
		if stop {
			return res, err
		}
		if probe {
			rr.Status.BreakerProbe = true
		}
		// The lock on the target is taken in the same status write (targetlock.go).
		rr.Status.HoldsTarget = new(true)
	} else if rr.Status.HoldsTarget == nil {
		// A refusal never takes the lock; false keeps it out of the limits' lock list (limitsSelectors).
		rr.Status.HoldsTarget = new(false)
	}

	rr.Status.ObservedGeneration = rr.Generation
	r.setPhase(rr, out.phase)
	// The request is decided: no limit holds it back any more.
	meta.RemoveStatusCondition(&rr.Status.Conditions, remediationv1alpha1.ConditionQueued)
	rr.Status.Source = out.source
	rr.Status.PolicyUID = out.policyUID
	rr.Status.PolicyRevision = out.policyRevision
	rr.Status.PlanDigest = out.planDigest
	rr.Status.PlanRepository = out.planRepository
	rr.Status.EffectiveApprovalRequired = out.approval
	rr.Status.Deadline = out.deadline
	setPolicyAllowed(&rr.Status.Conditions, metav1.Condition{
		Status:             out.status,
		Reason:             out.reason,
		Message:            out.message,
		ObservedGeneration: rr.Generation,
	}, r.now())
	if err := r.Status().Update(ctx, rr); err != nil {
		return ctrl.Result{}, fmt.Errorf("update status of RemediationRequest %s: %w", req.NamespacedName, err)
	}
	recordDecision(out.phase, out.reason)
	log.Info("Checked RemediationRequest against policy", "remediationRequest", req.NamespacedName,
		"phase", out.phase, "reason", out.reason)
	return ctrl.Result{}, nil
}

// outcome is what the policy check writes to the status.
type outcome struct {
	phase          remediationv1alpha1.RemediationPhase
	status         metav1.ConditionStatus
	reason         string
	message        string
	source         *remediationv1alpha1.ResolvedSource
	policyUID      remediationv1alpha1.UID
	policyRevision string
	planRepository *remediationv1alpha1.PlanRepository
	approval       *bool
	planDigest     string
	// deadline is the pull request step's deadline, started with an allowed decision unless writes are paused.
	deadline *metav1.Time
	// wait means the target's state is not known yet (policy.OutcomeWait): the request is decided again later,
	// and only the PolicyAllowed condition (Unknown, reason and message) is written meanwhile.
	wait bool
}

// waitForTarget handles a decision to wait for the target's state. The wait's clock is the PolicyAllowed
// condition's lastTransitionTime while the condition is Unknown with the wait's reason; recordWait (through
// setPolicyAllowed) starts it when the condition takes that reason and keeps it while the reason stays. With no
// such condition, or one whose time is after now, the wait has just started. While it has lasted less than
// TargetStateTimeout this returns the delay before the next check; past the timeout it returns a
// NeedsIntervention outcome and no delay.
func (r *RemediationRequestReconciler) waitForTarget(rr *remediationv1alpha1.RemediationRequest, out outcome) (
	outcome, time.Duration) {
	timeout := r.TargetStateTimeout
	if timeout <= 0 {
		timeout = DefaultTargetStateTimeout
	}
	var waited time.Duration
	if c := meta.FindStatusCondition(rr.Status.Conditions, remediationv1alpha1.ConditionPolicyAllowed); c != nil &&
		c.Status == metav1.ConditionUnknown && c.Reason == out.reason {
		waited = max(r.now().Sub(c.LastTransitionTime.Time), 0)
	}
	if waited >= timeout {
		return needsIntervention(string(policy.ReasonScaledObjectNotReady), messageTargetStateTimeout), 0
	}
	delay := min(max(waited, targetStateMinDelay), targetStateMaxDelay, timeout-waited)
	return out, delay
}

// recordWait writes the PolicyAllowed condition of a request that waits for its target's state, unless it
// already says so, and requeues it after delay. The phase is left as it is (none or Pending), so the next
// reconcile runs the policy check again.
//
// The wait clock (lastTransitionTime) starts when the condition takes the wait's reason and is kept while the
// reason stays (setPolicyAllowed), so the wait ends TargetStateTimeout after it started.
func (r *RemediationRequestReconciler) recordWait(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	out outcome, delay time.Duration) (ctrl.Result, error) {
	changed := setPolicyAllowed(&rr.Status.Conditions, metav1.Condition{
		Status:             metav1.ConditionUnknown,
		Reason:             out.reason,
		Message:            out.message,
		ObservedGeneration: rr.Generation,
	}, r.now())
	// The limits let the request through to this wait, so a Queued condition from before is no longer true.
	if meta.FindStatusCondition(rr.Status.Conditions, remediationv1alpha1.ConditionQueued) != nil {
		meta.RemoveStatusCondition(&rr.Status.Conditions, remediationv1alpha1.ConditionQueued)
		changed = true
	}
	if !changed {
		return ctrl.Result{RequeueAfter: delay}, nil
	}
	if err := r.Status().Update(ctx, rr); err != nil {
		return ctrl.Result{}, fmt.Errorf("update status of RemediationRequest %s/%s: %w", rr.Namespace, rr.Name, err)
	}
	logf.FromContext(ctx).Info("Waiting for the target's state before deciding RemediationRequest",
		"remediationRequest", client.ObjectKeyFromObject(rr), "reason", out.reason, "retryAfter", delay)
	return ctrl.Result{RequeueAfter: delay}, nil
}

// setPolicyAllowed sets the PolicyAllowed condition c in conditions and reports whether anything changed. A new
// reason starts a new transition time, now: meta.SetStatusCondition alone keeps the old time while the status
// stays the same, so an Unknown condition with another reason would lend its age to a wait (and its start to
// the decision that ends one). No path writes an Unknown PolicyAllowed with another reason before a wait today
// (recordWait is the only write without a phase and has one reason); this guards a second wait reason or
// another writer of the condition. A recorded time after now is not trusted either (a clock that went back, a
// status written by hand) and starts again at now, since a wait measured from it would never time out. A
// condition with the same reason and a time not after now keeps its time when its message or generation
// changes.
func setPolicyAllowed(conditions *[]metav1.Condition, c metav1.Condition, now time.Time) bool {
	c.Type = remediationv1alpha1.ConditionPolicyAllowed
	c.LastTransitionTime = metav1.Time{Time: now}
	if old := meta.FindStatusCondition(*conditions, c.Type); old != nil &&
		(old.Reason != c.Reason || old.LastTransitionTime.After(now)) {
		meta.RemoveStatusCondition(conditions, c.Type)
	}
	return meta.SetStatusCondition(conditions, c)
}

func denied(reason, message string) outcome {
	return outcome{phase: remediationv1alpha1.PhaseDenied, status: metav1.ConditionFalse, reason: reason, message: message}
}

func needsIntervention(reason, message string) outcome {
	return outcome{phase: remediationv1alpha1.PhaseNeedsIntervention, status: metav1.ConditionUnknown,
		reason: reason, message: message}
}

// check gathers the input and decides. The error is non-nil only for a failure worth retrying.
//
// The refusal order (design 5.3): a missing policy or settings object, a kind the action cannot change, a missing
// target or one with another UID, a previous value that is not the live one, then the policy's checks on the
// request's own fields (policy.Preflight). Only after all of them does the source mapping read Flux objects and
// Git, and policy.Decide decide on everything, the resolved source included.
func (r *RemediationRequestReconciler) check(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) (
	outcome, error) {
	pol := &remediationv1alpha1.RemediationPolicy{}
	switch err := r.getLive(ctx, types.NamespacedName{Name: string(rr.Spec.PolicyRef)}, pol); {
	case apierrors.IsNotFound(err):
		return denied(ReasonPolicyNotFound, fmt.Sprintf("Policy %q does not exist", rr.Spec.PolicyRef)), nil
	case err != nil:
		return outcome{}, fmt.Errorf("get RemediationPolicy %s: %w", rr.Spec.PolicyRef, err)
	}
	settings := &remediationv1alpha1.RemediationSettings{}
	switch err := r.getLive(ctx, types.NamespacedName{Name: remediationv1alpha1.SettingsName}, settings); {
	case apierrors.IsNotFound(err):
		return denied(ReasonSettingsNotFound, "RemediationSettings default does not exist"), nil
	case err != nil:
		return outcome{}, fmt.Errorf("get RemediationSettings %s: %w", remediationv1alpha1.SettingsName, err)
	}

	ref := rr.Spec.TargetRef
	// Refuse kinds the action may not change before reading anything, so the executor never reads, for
	// example, a Secret that a request names.
	field, err := sourcemap.FieldFor(rr.Spec.Action, ref)
	if err != nil {
		return needsIntervention(string(sourcemap.ReasonInvalidInput), "The action does not fit the target"), nil
	}
	live, ok, err := r.target(ctx, ref)
	if err != nil {
		return outcome{}, err
	}
	if !ok {
		return needsIntervention(string(sourcemap.ReasonTargetNotFound),
			fmt.Sprintf("%s %s/%s does not exist", ref.Kind, ref.Namespace, ref.Name)), nil
	}
	if live.GetUID() != types.UID(ref.UID) {
		return denied(ReasonTargetMismatch,
			fmt.Sprintf("%s %s/%s has another UID than the request names", ref.Kind, ref.Namespace, ref.Name)), nil
	}
	if o, stop := checkBaseline(rr.Spec.Action, field, live); stop {
		return o, nil
	}
	in := policy.DecisionInput{Request: rr, Policy: pol, Settings: settings, Target: policyTarget(live)}
	// Refuse what the policy refuses on the request's own fields before the source mapping, so such a request
	// costs no Git read and no token. Preflight runs the first checks of Decide and only refuses; a request it
	// lets through is decided by Decide in full below, once the source is resolved. Its refusal wins over
	// anything the mapping would have said (a stop, a refusal or an error to retry): the request's own fields are
	// checked first (#242).
	switch d, v := policy.Preflight(in); v {
	case policy.PreflightContinue:
	case policy.PreflightRefusedOnTarget:
		// The refusal rests on one read of the target (its labels, say), and ends the request for good. Flux may
		// have applied a change since, so confirm it is the object that was checked, as after the mapping below.
		if err := r.unchanged(ctx, ref, live); err != nil {
			return outcome{}, err
		}
		return preflightRefusal(d, pol), nil
	default:
		return preflightRefusal(d, pol), nil
	}

	res, err := r.Source.Resolve(ctx, sourcemap.Input{
		Target:       ref,
		Action:       rr.Spec.Action,
		Repositories: settings.Spec.Repositories,
		GitOps:       pol.Spec.GitOps,
	})
	if err != nil {
		// The error is sanitised by the resolver: no provider text, no URL.
		return outcome{}, fmt.Errorf("resolve source of %s %s/%s: %w", ref.Kind, ref.Namespace, ref.Name, err)
	}
	// The baseline was checked on the first read, and the resolver read the target again to compare it with
	// Git. If the target changed in between (Flux applied a new revision), the baseline and the result describe
	// different objects: a resolved source could let a change the policy refuses pass, and a stop (a value
	// that differs from Git, a field not found) could end a request that the current object does not stop.
	// Read once more, for every outcome, and start over when the object is not the one checked: an unchanged
	// resourceVersion means nothing changed in between.
	if err := r.unchanged(ctx, ref, live); err != nil {
		return outcome{}, err
	}

	switch res.Outcome {
	case sourcemap.Resolved:
		if res.Source == nil {
			return needsIntervention(string(sourcemap.ReasonInvalidInput), "The source mapping returned no source"), nil
		}
	case sourcemap.Denied:
		return denied(string(res.Reason), res.Message), nil
	default:
		// NeedsIntervention, and anything unknown: a person must look. Never Denied, never retried.
		return needsIntervention(string(res.Reason), res.Message), nil
	}

	in.Source = res.Source
	d := policy.Decide(in)
	o := decided(d, pol)
	o.source = res.Source
	if d.Outcome != policy.OutcomeAllowed {
		return o, nil
	}
	// The plan is fixed here, from the very objects the decision was made on, so no later read of the
	// policy or the settings can slip in between the decision and the digest.
	digest, err := freshness.ComputeDigest(rr, res.Source, pol, settings)
	if err != nil {
		return needsIntervention(string(sourcemap.ReasonInvalidInput), "The plan digest cannot be computed"), nil
	}
	o.planDigest = digest
	// The pull request step's deadline starts with the decision, so it also ends a request whose
	// freshness check never passes. A pause would use it up without any chance to act, so a request
	// decided while paused gets its deadline once the pause has ended (recheck).
	if !freshness.Paused(settings) {
		o.deadline = &metav1.Time{Time: r.now().Add(r.pullRequestTimeout())}
	}
	for _, repo := range settings.Spec.Repositories {
		if repo.Name == res.Source.RepositoryRef {
			o.planRepository = freshness.RepositoryOf(repo)
		}
	}
	return o, nil
}

// unchanged reads the target again and returns nil when it is still live, with the UID and resourceVersion of
// live, the object the check read first. Otherwise it returns an error, so the check starts over on the next
// attempt with the object as it is now: a terminal answer is never given on an object that has changed since.
func (r *RemediationRequestReconciler) unchanged(ctx context.Context, ref remediationv1alpha1.ObjectRef,
	live *unstructured.Unstructured) error {
	again, ok, err := r.target(ctx, ref)
	if err != nil {
		return err
	}
	if !ok || again.GetUID() != live.GetUID() || again.GetResourceVersion() != live.GetResourceVersion() {
		return fmt.Errorf("%s %s/%s changed during the policy check, checking again",
			ref.Kind, ref.Namespace, ref.Name)
	}
	return nil
}

// policyTarget is the live target as the policy reads it.
func policyTarget(live *unstructured.Unstructured) *policy.Target {
	return &policy.Target{
		APIVersion: live.GetAPIVersion(),
		Kind:       live.GetKind(),
		Namespace:  live.GetNamespace(),
		Name:       live.GetName(),
		UID:        remediationv1alpha1.UID(live.GetUID()),
		Labels:     live.GetLabels(),
		Object:     live.Object,
	}
}

// decided is the outcome of a policy decision d made under pol, without the source and without the plan an
// allowed decision fixes (digest, deadline, repository): the caller adds those, or returns another outcome when
// it cannot. Each outcome's phase and condition status match its reason; an outcome other than Allowed, Denied
// or Wait needs a person, so an unknown one never proceeds.
func decided(d policy.Decision, pol *remediationv1alpha1.RemediationPolicy) outcome {
	o := outcome{
		reason:         string(d.Reason),
		message:        d.Message,
		policyUID:      remediationv1alpha1.UID(pol.UID),
		policyRevision: fmt.Sprintf("%s/%d", pol.Name, pol.Generation),
	}
	switch d.Outcome {
	case policy.OutcomeAllowed:
		o.phase, o.status = remediationv1alpha1.PhasePolicyChecked, metav1.ConditionTrue
		o.approval = &d.EffectiveApprovalRequired
	case policy.OutcomeDenied:
		o.phase, o.status = remediationv1alpha1.PhaseDenied, metav1.ConditionFalse
	case policy.OutcomeWait:
		o.wait = true
	default:
		// NeedsIntervention, and anything unknown.
		o.phase, o.status = remediationv1alpha1.PhaseNeedsIntervention, metav1.ConditionUnknown
	}
	return o
}

// preflightRefusal is the outcome of a refusal by policy.Preflight. Preflight never allows; should it ever
// return Allowed, the request goes to a person instead, since no plan was fixed for it.
func preflightRefusal(d policy.Decision, pol *remediationv1alpha1.RemediationPolicy) outcome {
	if d.Outcome == policy.OutcomeAllowed {
		return needsIntervention(string(policy.ReasonIncompleteInput), "The policy preflight cannot allow a request")
	}
	return decided(d, pol)
}

// target reads the live target. It reports false when the object, or its kind, does not exist.
func (r *RemediationRequestReconciler) target(ctx context.Context, ref remediationv1alpha1.ObjectRef) (
	*unstructured.Unstructured, bool, error) {
	gv, err := schema.ParseGroupVersion(ref.APIVersion)
	if err != nil {
		return nil, false, nil
	}
	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(gv.WithKind(ref.Kind))
	err = r.reader().Get(ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, live)
	switch {
	case apierrors.IsNotFound(err) || meta.IsNoMatchError(err):
		return nil, false, nil
	case err != nil:
		return nil, false, fmt.Errorf("get %s %s/%s: %w", ref.Kind, ref.Namespace, ref.Name, err)
	}
	return live, true, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *RemediationRequestReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Source == nil {
		return errors.New("a SourceResolver is required")
	}
	if r.Namespace == "" {
		return errors.New("the operator namespace is required")
	}
	requestPhases.setNamespace(r.Namespace)
	if err := mgr.Add(requestPhases.source.Runnable(mgr.GetCache(), &remediationv1alpha1.RemediationRequest{})); err != nil {
		return fmt.Errorf("adding the source of the RemediationRequest phase metric: %w", err)
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&remediationv1alpha1.RemediationRequest{}).
		// A change of a request wakes the requests queued behind its lock on the same target.
		Watches(&remediationv1alpha1.RemediationRequest{}, handler.EnqueueRequestsFromMapFunc(r.lockWaiters)).
		Named("remediationrequest").
		Complete(countErrors{r})
}

// checkBaseline compares the previous value the request claims with the live value of the field. The policy
// measures increases and decreases from the claimed value, so a forged or stale one could pass a change that
// is far larger than the limit allows (invariant 1). A missing field needs a person; a different value is
// refused.
func checkBaseline(action remediationv1alpha1.Action, field sourcemap.Field, live *unstructured.Unstructured) (
	outcome, bool) {
	claimed, ok := sourcemap.PreviousValue(action)
	if !ok {
		return needsIntervention(string(sourcemap.ReasonInvalidInput), "The action has no previous value"), true
	}
	got, found := field.Get(live.Object)
	if !found {
		return needsIntervention(string(sourcemap.ReasonFieldNotFound),
			fmt.Sprintf("%s is not set on the live object", field)), true
	}
	if !field.Equals(got, claimed) {
		return denied(ReasonPreviousValueMismatch,
			fmt.Sprintf("The previous value in the request is not the live value of %s", field)), true
	}
	return outcome{}, false
}

// beforeMerge reports whether a phase is one in which the plan can still be superseded.
func beforeMerge(p remediationv1alpha1.RemediationPhase) bool {
	switch p {
	case remediationv1alpha1.PhasePolicyChecked, remediationv1alpha1.PhasePRCreated,
		remediationv1alpha1.PhaseAwaitingApproval, remediationv1alpha1.PhaseAutoMergeQueued:
		return true
	}
	return false
}

// recheck is the freshness step of Reconcile for a request that has left the policy check but has not merged.
// A recorded pull request is looked at first, so a merge or a rejection is seen before the freshness check
// (a merged change makes the plan look stale), and a request past its deadline expires even while the
// freshness check cannot finish. A recorded pull request found closed while the cleanup of a stale plan has
// started (staleMark) ends Superseded: the close was ours. It then makes a stale plan visible (and closes its
// change request) without waiting for the next write, and on a fresh plan runs the pull request step, which
// makes at most one Git write right after this check.
func (r *RemediationRequestReconciler) recheck(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) (
	ctrl.Result, error) {
	if r.OpenRepository == nil || !progresses(rr) {
		return ctrl.Result{}, nil
	}
	outcome := prOpen
	if rr.Status.PullRequest != nil {
		var lookErr error
		if outcome, lookErr = r.watchPullRequest(ctx, rr); outcome == prStopped {
			return ctrl.Result{}, nil
		}
		if retryAfterOf(lookErr) > 0 {
			// The Git host asked for a delay (a rate limit): the freshness check would ask it again at once.
			return r.waitOrFail(ctx, rr, lookErr)
		}
		if wait, ok := r.pollOnly(rr, outcome); ok {
			// Still open, and the last full recheck is recent: only the pull request was read (#434).
			return ctrl.Result{RequeueAfter: wait}, nil
		}
	}
	if mark := staleMark(rr); mark != nil && outcome == prClosed {
		// The cleanup of a stale plan started (ensureFresh) and its pull request is closed: the close that was
		// cut short was ours, not a person's rejection, whatever the plan looks like now.
		closedByUs(rr, staleClosed(mark.Reason))
		return ctrl.Result{}, r.ignoreSuperseded(r.endFreshness(ctx, rr, remediationv1alpha1.PhaseSuperseded,
			mark.Reason, mark.Message))
	}
	// A request whose cleanup started finishes it in ensureFresh, also past the deadline.
	if rr.Status.Phase == remediationv1alpha1.PhasePolicyChecked && rr.Status.Deadline != nil &&
		staleMark(rr) == nil && r.now().After(rr.Status.Deadline.Time) {
		return r.expire(ctx, rr)
	}
	fresh, err := r.ensureFresh(ctx, rr)
	if !errors.Is(err, freshness.ErrPaused) && !errors.Is(err, ErrSuperseded) {
		// The check did not stop at a pause: a request held back by the kill switch loses WritesPaused as
		// soon as the gate finds writes allowed, whatever comes next (a failure of the host, a refusal, a
		// wait), so the condition never outlives the pause.
		if err := r.resumeIfAllowed(ctx, rr); err != nil {
			return ctrl.Result{}, err
		}
	}
	switch {
	case errors.Is(err, ErrSuperseded):
		return ctrl.Result{}, nil
	case errors.Is(err, freshness.ErrPaused):
		return r.waitWhilePaused(ctx, rr)
	case rr.Status.Phase == remediationv1alpha1.PhasePolicyChecked && rr.Status.Deadline == nil:
		// Decided while writes were paused (the policy check starts no deadline then): the freshness check
		// has just found writes allowed again, so the deadline starts now, whether or not the plan was fresh.
		return r.startDeadline(ctx, rr)
	case err != nil:
		return r.refuseOrWait(ctx, rr, err)
	case outcome == prClosed:
		return r.rejected(ctx, rr)
	default:
		if err := r.clearWaits(ctx, rr); err != nil {
			return ctrl.Result{}, err
		}
		return r.advance(ctx, rr, fresh)
	}
}

// ignoreSuperseded returns nil for ErrSuperseded and err otherwise.
func (r *RemediationRequestReconciler) ignoreSuperseded(err error) error {
	if errors.Is(err, ErrSuperseded) {
		return nil
	}
	return err
}

// ensureFresh must run right before every Git write of a request (design section 6). It returns a nil error
// only when the plan is fresh, with what the freshness check read (the target branch head, the target file
// there and the repository), so the write and the render check work on that commit. When an input of the plan
// digest changed, it marks the cleanup as started (PlanFresh False with the stale reason, phase unchanged),
// closes the request's change request through the provider, sets the phase to Superseded and returns
// ErrSuperseded; the branch is never rebuilt or force-pushed. Any other error (an input could not be read,
// writes are paused) means "not fresh": the caller must not write and may retry.
//
// It is idempotent (invariant 5): closing a closed change request succeeds, and a request that is already
// Superseded returns ErrSuperseded without calling the provider again. The mark is written before the close and
// the final phase after it, so a crash or a deadline anywhere in between leaves a mark that the next call
// finishes (staleMark): it closes again, a no-op when the first close went through, and supersedes, without
// checking the plan again. A plan that looks fresh again by then does not reopen the decision, and the closed
// pull request is never taken for a person's rejection.
func (r *RemediationRequestReconciler) ensureFresh(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) (
	freshness.Result, error) {
	if rr.Status.Phase == remediationv1alpha1.PhaseSuperseded {
		return freshness.Result{}, ErrSuperseded
	}
	if !beforeMerge(rr.Status.Phase) {
		return freshness.Result{}, fmt.Errorf("a request in phase %q takes no write that needs a fresh plan", rr.Status.Phase)
	}
	if mark := staleMark(rr); mark != nil {
		return freshness.Result{}, r.cleanUpStale(ctx, rr, &freshness.StaleError{Reason: mark.Reason, Message: mark.Message})
	}
	freshCtx, cancel := bound(ctx, r.CleanupTimeout, DefaultCleanupTimeout)
	// The provider the check opens is gated (openRepository): the pull request step writes through it.
	fresh, err := freshness.Checker{Reader: r.reader(), OpenRepository: r.openRepository, Source: r.Source}.Check(freshCtx, rr)
	// A verdict that arrives after the deadline, fresh (nil), stale and unusable included, is not acted on: a
	// read may have been cut short, and a "fresh" from a read that ended with the deadline is not one the
	// caller may write on.
	err = boundErr(ctx, freshCtx, "freshness check", err)
	cancel()
	if err == nil {
		return fresh, nil
	}
	if errors.Is(err, freshness.ErrRepositoryUnusable) {
		return freshness.Result{}, r.endFreshness(ctx, rr, remediationv1alpha1.PhaseNeedsIntervention,
			reasonRepositoryUnusable, messageRepositoryUnusable)
	}
	staleErr, ok := errors.AsType[*freshness.StaleError](err)
	if !ok {
		return freshness.Result{}, err
	}
	// The mark comes before the close, in its own status write, so a close that goes through while its answer
	// is lost (the deadline, a restart) is known on the next reconcile to be ours.
	meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{
		Type:               remediationv1alpha1.ConditionPlanFresh,
		Status:             metav1.ConditionFalse,
		Reason:             staleErr.Reason,
		Message:            staleErr.Message,
		ObservedGeneration: rr.Generation,
	})
	if err := r.Status().Update(ctx, rr); err != nil {
		return freshness.Result{}, fmt.Errorf("mark the cleanup of RemediationRequest %s/%s: %w", rr.Namespace, rr.Name, err)
	}
	return freshness.Result{}, r.cleanUpStale(ctx, rr, staleErr)
}

// staleReasons are the reasons of a stale plan (freshness.StaleError), the only ones staleMark accepts.
var staleReasons = map[string]bool{
	freshness.ReasonPolicyChanged: true, freshness.ReasonSettingsChanged: true, freshness.ReasonTargetChanged: true,
	freshness.ReasonTargetFileChanged: true, freshness.ReasonPlanChanged: true, freshness.ReasonSourceChanged: true,
	freshness.ReasonScaledObjectNotReady: true, freshness.ReasonModuleNotEnabled: true,
}

// staleMark returns the PlanFresh condition that ensureFresh writes before it closes a stale plan's change
// request: False with a stale reason on a request that has not reached a final phase. Nil when there is none.
// The other PlanFresh values (Unknown while the Git host refuses the check, False with another reason in a
// final phase) are not a mark.
func staleMark(rr *remediationv1alpha1.RemediationRequest) *metav1.Condition {
	c := meta.FindStatusCondition(rr.Status.Conditions, remediationv1alpha1.ConditionPlanFresh)
	if c == nil || c.Status != metav1.ConditionFalse || !staleReasons[c.Reason] || !beforeMerge(rr.Status.Phase) {
		return nil
	}
	return c
}

// cleanUpStale closes the change request of a stale plan whose mark is written and ends the request: Superseded
// with the stale reason, or NeedsIntervention when the change request cannot be closed for good. A failure
// worth a retry is returned as it is and leaves the mark, so the retry closes again.
func (r *RemediationRequestReconciler) cleanUpStale(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	staleErr *freshness.StaleError) error {
	phase, reason, message := remediationv1alpha1.PhaseSuperseded, staleErr.Reason, staleErr.Message
	// The whole cleanup shares one deadline: reading the settings, opening the repository, finding and
	// closing the change request.
	cleanCtx, cleanCancel := bound(ctx, r.CleanupTimeout, DefaultCleanupTimeout)
	closeErr := r.closeChangeRequest(cleanCtx, rr)
	// Every result, nil and errRepositoryGone included, is checked against the deadline before a final phase
	// is written from it.
	closeErr = boundErr(ctx, cleanCtx, "cleanup of the change request", closeErr)
	cleanCancel()
	switch err := closeErr; {
	case err == nil:
		closedByUs(rr, staleClosed(staleErr.Reason))
	case errors.Is(err, freshness.ErrRepositoryUnusable):
		// The settings of the repository cannot be used, so the change request cannot be closed either.
		phase, reason, message = remediationv1alpha1.PhaseNeedsIntervention, reasonRepositoryUnusable,
			messageRepositoryUnusable
	case errors.Is(err, errTargetBranchChanged):
		phase, reason = remediationv1alpha1.PhaseNeedsIntervention, reasonTargetBranchChanged
		message = "The plan is stale and its repository now targets another branch; " +
			"its change request, if any, was not closed"
	case errors.Is(err, errChangeRequestNotOurs):
		phase, reason = remediationv1alpha1.PhaseNeedsIntervention, ReasonForeignChangeRequest
		message = "The plan is stale and the pull request recorded for it is not the branch's pull request " +
			"with this request's marker; it was not closed"
	case errors.Is(err, errRepositoryGone):
		// Nothing is closed: the only change request this plan could have is in a repository that cannot be
		// reached through the settings any more, and closing a number elsewhere could close someone else's.
		phase, reason = remediationv1alpha1.PhaseNeedsIntervention, reasonRepositoryUnavailable
		message = "The plan is stale and the repository it was made for is not configured any more; " +
			"its change request, if any, was not closed"
	case err != nil:
		refused, _, ok := refusal(err)
		if !ok {
			return err
		}
		// The Git host refuses the close for good (a permission the App lacks, a rule): the plan is stale
		// and nothing more may be written for it, so a person has to close the change request.
		phase, reason = remediationv1alpha1.PhaseNeedsIntervention, refused
		message = "The plan is stale (" + staleErr.Reason + ") and the Git host refused to close its " +
			"change request; it was not closed"
	}
	return r.endFreshness(ctx, rr, phase, reason, message)
}

// staleClosed is the fixed text of PullRequestOpened for a pull request closed because its plan is stale.
func staleClosed(reason string) string {
	return "the plan is stale (" + reason + ")"
}

// endFreshness writes the final phase of a plan that is not fresh, with PlanFresh False saying why, and returns
// ErrSuperseded (or the error of the status write).
func (r *RemediationRequestReconciler) endFreshness(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	phase remediationv1alpha1.RemediationPhase, reason, message string) error {
	r.setPhase(rr, phase)
	// A final phase waits for nothing, also not for the kill switch; the request does not go on, so no
	// WritesResumed Event is recorded.
	resume(rr)
	meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{
		Type:               remediationv1alpha1.ConditionPlanFresh,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: rr.Generation,
	})
	settleLock(rr)
	if err := r.Status().Update(ctx, rr); err != nil {
		return fmt.Errorf("update status of RemediationRequest %s/%s: %w", rr.Namespace, rr.Name, err)
	}
	logf.FromContext(ctx).Info("Superseded RemediationRequest", "remediationRequest", client.ObjectKeyFromObject(rr),
		"phase", phase, "reason", reason)
	return ErrSuperseded
}

// closeChangeRequest closes the request's change request, if it has one, in the repository the plan was made
// for and nowhere else. A change request that is already closed, or that was never opened, is not an error; a
// merged one is, because the change landed. It returns [errRepositoryGone] when that repository is not in the
// settings, [errTargetBranchChanged] when its entry now names another target branch, [errChangeRequestNotOurs]
// when a recorded pull request is not the branch's own, and [freshness.ErrPaused] when the settings read now
// pause writes.
func (r *RemediationRequestReconciler) closeChangeRequest(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) error {
	if r.OpenRepository == nil {
		return errors.New("cannot close the change request: no Git provider")
	}
	settings := &remediationv1alpha1.RemediationSettings{}
	switch err := r.getLive(ctx, types.NamespacedName{Name: remediationv1alpha1.SettingsName}, settings); {
	case apierrors.IsNotFound(err):
		return errRepositoryGone
	case err != nil:
		return fmt.Errorf("get RemediationSettings %s: %w", remediationv1alpha1.SettingsName, err)
	}
	if freshness.Paused(settings) {
		return freshness.ErrPaused
	}
	repo, ok := freshness.FindRepository(settings, rr.Status.PlanRepository)
	if !ok {
		return errRepositoryGone
	}
	if src := rr.Status.Source; src != nil && repo.TargetBranch != src.TargetBranch {
		return errTargetBranchChanged
	}
	git, err := r.openRepository(ctx, repo)
	if providers.IsInvalidConfig(err) {
		return freshness.ErrRepositoryUnusable
	}
	if err != nil {
		return freshness.WrapProvider("open the repository", err)
	}
	// The pull request is found by the request's branch, also when one is recorded: it may exist although the
	// status write that records it failed (design section 6), and a recorded number is closed only when the
	// branch's pull request is that number and carries this request's marker.
	cr, err := git.FindChangeRequest(ctx, freshness.BranchName(rr))
	recorded := rr.Status.PullRequest
	switch {
	case errors.Is(err, providers.ErrNotFound) && recorded == nil:
		return nil
	case errors.Is(err, providers.ErrNotFound):
		return errChangeRequestNotOurs
	case err != nil:
		return freshness.WrapProvider("find the change request", err)
	}
	if !hasMarker(cr.Body, rr) || (recorded != nil && cr.Number != recorded.Number) {
		if recorded == nil {
			// Someone else's pull request on our branch name, or one of another plan: never ours to close.
			return nil
		}
		return errChangeRequestNotOurs
	}
	if err := git.CloseChangeRequest(ctx, cr.Number); err != nil {
		return freshness.WrapProvider("close the change request", err)
	}
	return nil
}
