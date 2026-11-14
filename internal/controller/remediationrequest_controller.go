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
	"slices"
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
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/gitops/semdiff"
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
	// opened to decide whether its body may show the name (verifiedIncidentName), and, for a revert, the read of
	// the request it reverts that the body names (revertedRequest). Each runs inside the Git step's deadline and
	// takes at most half of what is left of it, so a stalled read cannot use that up; on timeout the Incident's
	// name is redacted, and the revert's read is an error retried later. Zero means [DefaultIncidentReadTimeout].
	IncidentReadTimeout time.Duration
	// PullRequestTimeout is how long a request may take from PolicyChecked to an open pull request; it is
	// stored in status.deadline. Zero means [DefaultPullRequestTimeout].
	PullRequestTimeout time.Duration
	// TargetStateTimeout is how long the policy check waits for a target whose state is not known yet (a
	// ScaledObject KEDA has not reported on), or for Flux to finish reconciling the Kustomization that
	// delivers it (#556), before it ends the request in NeedsIntervention. Zero means
	// [DefaultTargetStateTimeout].
	TargetStateTimeout time.Duration
	// Delivery builds the delivery verifier of the check after the merge (rollout.go), given the Git host's
	// answer to whether one commit is in the history of another. Nil disables that check: a request stops in
	// Merged and nothing is read after it.
	Delivery func(ancestry providers.AncestryChecker) (DeliveryVerifier, error)
	// NodeGrace is how long past the stable window the verdict waits for a node that stopped reporting to have
	// its pods marked not Ready (ADR-0030 section 4); it is part of the verification timeout. cmd/main.go sets it
	// from --node-grace (chart value executor.verification.nodeGrace), which should be at least the cluster's
	// node monitor grace period. Zero means [DefaultNodeGrace] (1 minute); SetupWithManager refuses a negative
	// value.
	NodeGrace time.Duration
	// VerifyOutcomes turns on the phases after the merge (phases.go, clearing.go, ADR-0030): a Merged request with
	// a merge receipt moves to WaitingForDelivery and Verifying and ends Succeeded, Failed or Inconclusive, with
	// Cleared judged from the verifier's VerificationReport. It is off by default; cmd/main.go sets it from the
	// executor flag --verify-outcomes (chart value executor.verification.enabled). Without a verifier deployed no
	// report is written, so with it on every merge ends Inconclusive or Failed. On, the breaker reads
	// a Merged request with a merge receipt as undecided (verificationPending). Off, a Merged request keeps the
	// rollout report of #532 and is a success for the breaker, and a request found in WaitingForDelivery or
	// Verifying ends Inconclusive with reason VerificationDisabled.
	VerifyOutcomes bool
	// RolloutDeadline is how long after the merge commit is seen applied the target's rollout may take before it
	// is reported stuck (RolledOut False, RolloutStuck). Zero means rollout.DefaultDeadline; it is bounded to
	// [rollout.MinDeadline, rollout.MaxDeadline].
	RolloutDeadline time.Duration
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
	// AwaitingApproval) looks at that pull request alone, between the full rechecks every [FullRecheckInterval],
	// so a merge or a close is seen within it. Zero means [DefaultPullRequestPollInterval]; SetupWithManager
	// refuses a value outside [MinPullRequestPollInterval, MaxPullRequestPollInterval].
	PullRequestPollInterval time.Duration
	// ReviewerHints turns on the reading of reviewer hints from the comments of a waiting pull request
	// (hints.go, ADR-0028) and the check run shadewright/review on every recorded head of a waiting pull request
	// (reviewcheck.go). It is off by default; cmd/main.go sets it from the executor flag --reviewer-hints (chart
	// value executor.reviewerHints.enabled). A stored hint makes the analyzer propose a replacement, which takes
	// the pull request over by the supersede handoff (handoff.go), and the merge gate (ADR-0028 section 5) is
	// that check. The gate holds only once the target branch of the GitOps repository requires the check
	// shadewright/review with the App as its source, which Shadewright cannot verify: without that rule the
	// check informs and does not block, so turn hints on only after the rule is in place. Off fails closed
	// (ADR-0028 amendment of 2026-11-10): every Pending or Reserved hint is declined with HintsTurnedOff
	// (declineHintsOff), a replacement is Denied at admission (chainLink), one admitted before ends without
	// the handoff (handoffStep, recoverStep), and one that took the pull request over stops NeedsIntervention
	// (hintsOffTakenOver).
	ReviewerHints bool

	// admitMu makes the check of the limits and the status write that lets a request through the policy check
	// one step, so two workers can never both take the last slot or the same target.
	admitMu sync.Mutex
	// clearMu lets one clearing of the breaker's watermark run at a time, and orders it with the restore in the
	// finalizer of the success that cleared (breakercounts.go).
	clearMu sync.Mutex
	// closingOnDelete are the requests that ended whose finalizer closes their pull request now
	// (waitForCapOnDelete), guarded by admitMu.
	closingOnDelete map[types.UID]struct{}
	// checked holds, per request name, when the last full recheck of a waiting pull request ended, with the
	// request's UID (pollState): an entry of another UID under the same name is not used. It is memory only:
	// after a restart or a change of leader every request gets a full recheck first. Entries are dropped when the
	// request ends, is being deleted or is gone.
	checked sync.Map
	// permissionWarned holds, per request name, the comment the last HintPermissionUnknown Event was recorded for
	// (permissionWarning, hints.go), so each comment is warned about once. Memory only, dropped with checked.
	permissionWarned sync.Map
	// steeredCommented holds, per request name, the reviews of hint authors whose comment saying why they do not
	// count is on the pull request (reviewGate), so it is asked for once per review, not on every look. Memory
	// only, dropped with checked: after a restart the marker finds the comment already posted.
	steeredCommented sync.Map
	// rewritten holds, per request name, the answer of the Git host to the rewrite after a handoff whose status
	// write has not gone through (followHandoff), so a retry records it without sending the rewrite again.
	// Memory only, dropped with checked: after a restart the rewrite is sent again, which is idempotent.
	rewritten sync.Map
	// replyAttempts holds, per request name, the failed posts of each reply to a reviewer hint (replyTries,
	// hintreply.go), so a reply is given up after maxReplyAttempts. Memory only, dropped with checked: after a
	// restart a reply gets that many tries again, and its marker keeps a post that went through from being
	// repeated.
	replyAttempts sync.Map
	// renderEnds counts, per request and plan digest, the ends of the render process in a row without an answer
	// (renderends.go), so a build that ends it that way on every retry is refused. Memory only, dropped with
	// checked.
	renderEnds renderEnds
	// renderCheck is the render check of checkRender; nil is semdiff.Check. Tests replace it.
	renderCheck func(context.Context, semdiff.Input) (semdiff.Result, error)
	// reportCounters holds, per request in Verifying, the executor's memory of its VerificationReport's sample
	// counter (clearing.go). Memory only: after a restart success waits one more check. Dropped when the request
	// ends, is being deleted or is gone, not with forget, which runs on every reconcile of a request after the
	// merge.
	reportCounters reportCounters
	// unwrittenResets holds, per request in Verifying, a reset of the executor's window whose status write was lost
	// (clearing.go), applied again by the next check. Memory only; dropped like reportCounters.
	unwrittenResets unwrittenResets
	// mergeReadings holds, per request after the merge, the reading of its Incident's count at the merge that
	// this process took (incidentcount.go), the only one to which it adds a reading at the applied time. Memory
	// only: after a restart or a change of leader no reading at the applied time is taken. Dropped like
	// reportCounters.
	mergeReadings mergeReadings
	// goneLooks holds, per request, the first look of this process that found a pull request number of the
	// request missing from the Git host (change.go, #688). Memory only: after a restart or a change of leader
	// the confirmation wait starts again. Dropped by any other outcome of a read, when the request ends
	// (setPhase) and when it is gone. It also holds the lock per request that keeps calls of changeOf for one
	// request from overlapping, dropped with the request's entry. Tests that model one process over several reconcilers set the same one;
	// otherwise looks makes it on first use (goneLooksOnce).
	goneLooks     *goneLooks
	goneLooksOnce sync.Once
	// since is when this process began reconciling, on its clock (clearing.go): no window counts time before
	// it. Memory only, set once.
	since reconcilingSince
}

// forget drops what this process keeps in memory about a request: its last full recheck, its last
// permission warning, the comments of the merge gate it posted, a rewrite not recorded yet, the failed
// posts of its replies to hints and the ends of its render process in a row.
func (r *RemediationRequestReconciler) forget(key types.NamespacedName) {
	r.checked.Delete(key)
	r.permissionWarned.Delete(key)
	r.steeredCommented.Delete(key)
	r.rewritten.Delete(key)
	r.replyAttempts.Delete(key)
	r.renderEnds.forget(key)
}

// DefaultTargetStateTimeout is how long the policy check waits by default for KEDA to report on a
// ScaledObject, or for Flux to finish reconciling the delivery of the target. KEDA reconciles a new or changed
// ScaledObject within seconds, and Flux applies a merged revision within its interval and health check
// timeout (minutes); ten minutes covers an operator restart without leaving a request undecided for long.
const DefaultTargetStateTimeout = 10 * time.Minute

// targetStateTimeout is TargetStateTimeout, or [DefaultTargetStateTimeout] when it is zero.
func (r *RemediationRequestReconciler) targetStateTimeout() time.Duration {
	if r.TargetStateTimeout <= 0 {
		return DefaultTargetStateTimeout
	}
	return r.TargetStateTimeout
}

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

// orStepErr returns res and err, or, when err is nil, stepErr from the steps after a handoff for the error
// backoff (controller-runtime ignores res with an error).
func orStepErr(res ctrl.Result, err, stepErr error) (ctrl.Result, error) {
	if err == nil && stepErr != nil {
		return ctrl.Result{}, stepErr
	}
	return res, err
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

// FullRecheckInterval is freshRecheckInterval, for the executor's flag help.
const FullRecheckInterval = freshRecheckInterval

// Bounds of PullRequestPollInterval. One look reads the settings from the API server and lists the pull
// requests of the request's branch on the Git host (one GitHub REST call); a full recheck makes about eight
// (the look, three reads of the target branch head and the target file: its tree, one per directory and the
// blob). At the lower bound a waiting pull request costs about 200 calls an hour (108 looks and 12 full
// rechecks), so 20 of them (the most maxConcurrentRemediations allows) about 4,000, against an installation limit
// of at least 5,000 an hour that the policy check's source mapping and the writes share. A full recheck after a
// merge to the target branch also maps the source again at the new commit (its read cache is keyed by commit),
// up to 512 file reads. The upper bound is freshRecheckInterval, where every look is a full recheck.
// Each status write of the executor on a waiting request (the move to AwaitingApproval, a new render head, a
// refusal, a pause and its end) wakes the request once more, which costs one more look; those writes happen on a
// change only, not on every look.
const (
	// DefaultPullRequestPollInterval is the poll interval of a waiting pull request by default: a merge is seen
	// within about a minute, for about 150 GitHub REST calls an hour per waiting pull request (48 looks and 12
	// full rechecks), against about 100 with full rechecks only, plus one look per status write.
	DefaultPullRequestPollInterval = time.Minute
	// MinPullRequestPollInterval is the shortest poll interval.
	MinPullRequestPollInterval = 30 * time.Second
	// MaxPullRequestPollInterval is the longest poll interval: every look is then a full recheck, as before #434.
	MaxPullRequestPollInterval = freshRecheckInterval
)

// errRepositoryGone means the repository a plan was made for is not in the settings any more (or was never
// recorded), so its change request cannot be closed safely.
var errRepositoryGone = errors.New("the repository of the plan is not configured")

// afterRecheckEnd is what reconcile does after the recheck ended the request, with the recheck's result and error.
// A step not recorded is not retried for an ended request: the next reconcile does not run it. An end that closed
// the recorded pull request or found it closed kept the lock, because only the read of the branch may release it
// then (settleLock): that read is made now.
func (r *RemediationRequestReconciler) afterRecheckEnd(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	res ctrl.Result, err error) (ctrl.Result, error) {
	if err != nil || !ptr.Deref(rr.Status.HoldsTarget, false) || !closedOnly(rr) {
		return res, err
	}
	held, err := r.checkHeld(ctx, rr)
	return earlier(res, held), err
}

// errChangeRequestNotOurs means a pull request is recorded but cannot be confirmed as this request's: the Git host
// has no pull request with that number and the branch confirms it, or it is from another branch or base branch, or
// lacks this request's marker (changeOf's gone). An answer about another pull request and a 404 the
// branch does not confirm are failed reads, never this error (#688). The recorded number is not closed.
var errChangeRequestNotOurs = errors.New("the recorded change request could not be confirmed as the request's own")

// errHandoffInFlight means the request's intent to hand its pull request over stands (ADR-0028 section 4): the
// replacement may hold the pull request, so nothing closes it until the intent is done or aborted.
var errHandoffInFlight = errors.New("the change request is being handed over to a replacement")

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
	res, err := r.reconcile(ctx, req)
	if err != nil && errors.Is(ctx.Err(), context.Canceled) {
		// The manager cancelled the reconcile (it is shutting down): not a failure to log and retry with
		// backoff. Whatever was cut short is found again from the status on the next start. A reconcile timeout
		// (DeadlineExceeded) is still returned, so it is retried.
		logf.FromContext(ctx).V(1).Info("Stopped reconciling RemediationRequest: the reconcile was cancelled",
			"remediationRequest", req.NamespacedName, "error", err.Error())
		return ctrl.Result{}, nil
	}
	return res, err
}

// afterStop handles a request that no longer progresses: nothing is written to Git any more, but a lock still
// held waits for its pull request, and a merged request is followed after its merge. stepErr is the error of a
// step after the handoff whose status write failed, returned after this for a retry with backoff.
func (r *RemediationRequestReconciler) afterStop(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	stepErr error) (ctrl.Result, error) {
	res, err := r.checkHeld(ctx, rr)
	switch {
	case err != nil:
		return orStepErr(res, err, stepErr)
	case rr.Status.Phase == remediationv1alpha1.PhaseWaitingForDelivery || rr.Status.Phase == remediationv1alpha1.PhaseVerifying:
		// The phases after the merge (phases.go): they only read and write the request's own status and Events.
		after, err := r.afterMerge(ctx, rr)
		return orStepErr(earlier(res, after), err, stepErr)
	case rr.Status.Phase != remediationv1alpha1.PhaseMerged:
		return orStepErr(res, err, stepErr)
	}
	// A merge recorded without a receipt ends Inconclusive here, and one recorded before receipts existed is
	// marked once (merge.go).
	if done, err := r.settleReceipt(ctx, rr); done || err != nil {
		return orStepErr(res, err, stepErr)
	}
	// A merged request with a merge receipt moves on to WaitingForDelivery (phases.go); one merged before
	// receipts existed is followed until its rollout is reported (rollout.go). Both only read and write the
	// request's own status and Events.
	after, err := r.afterMerge(ctx, rr)
	return orStepErr(earlier(res, after), err, stepErr)
}

// reconcile is Reconcile before its handling of a cancellation.
func (r *RemediationRequestReconciler) reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	rr := &remediationv1alpha1.RemediationRequest{}
	if err := r.Get(ctx, req.NamespacedName, rr); err != nil {
		if apierrors.IsNotFound(err) {
			r.forget(req.NamespacedName)
			r.reportCounters.forget(req.NamespacedName)
			r.unwrittenResets.forget(req.NamespacedName)
			r.mergeReadings.forget(req.NamespacedName)
			r.looks().forget(req.NamespacedName)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !rr.DeletionTimestamp.IsZero() {
		r.forget(req.NamespacedName)
		r.reportCounters.forget(req.NamespacedName)
		r.unwrittenResets.forget(req.NamespacedName)
		r.mergeReadings.forget(req.NamespacedName)
		return r.finalize(ctx, rr)
	}
	if err := r.backfill(ctx, rr); err != nil {
		if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
			// A newer version was written, or the request is gone: it is read again shortly, not reported as a
			// failed reconcile.
			log.V(1).Info("Retrying the backfill of RemediationRequest", "remediationRequest", req.NamespacedName,
				"error", err.Error())
			return ctrl.Result{RequeueAfter: backfillRetryDelay}, nil
		}
		return ctrl.Result{}, err
	}
	if rr.Status.Phase != "" && rr.Status.Phase != remediationv1alpha1.PhasePending {
		// A step after the handoff whose status write failed does not hold back the look at the pull request,
		// so a merge or a close is still recorded; its error is returned after it, for a retry with backoff.
		stepErr := r.afterHandoff(ctx, rr)
		if stepErr != nil && !errors.Is(stepErr, errStepNotRecorded) {
			return ctrl.Result{}, stepErr
		}
		if !progresses(rr) {
			r.forget(req.NamespacedName)
			if rr.Status.Phase != remediationv1alpha1.PhaseVerifying {
				// Windows exist only in Verifying. A request seen in another phase after the merge was ended,
				// by this executor or by another writer, and a reset kept for a lost write has no window left.
				r.unwrittenResets.forget(req.NamespacedName)
			}
			if !awaitsCountAtApplied(rr) {
				r.mergeReadings.forget(req.NamespacedName)
			}
			return r.afterStop(ctx, rr, stepErr)
		}
		res, err := r.recheck(ctx, rr)
		if !progresses(rr) {
			// The recheck ended the request (Merged, Superseded, NeedsIntervention, ...): it is never looked at again.
			r.forget(req.NamespacedName)
			return r.afterRecheckEnd(ctx, rr, res, err)
		}
		return orStepErr(res, err, stepErr)
	}
	if res, stop, err := r.beforeCheck(ctx, rr); stop {
		return res, err
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

	unlock := func() {}
	var clr clearing
	if out.phase == remediationv1alpha1.PhasePolicyChecked {
		// The limits are checked again, now, and the decision is written under the same lock, so the next
		// check of the limits sees this request as active, and as the breaker's probe when it is one. A
		// conflict on that write lets nothing through and marks nothing: the request is decided again.
		r.admitMu.Lock()
		locked := true
		unlock = func() {
			if locked {
				locked = false
				r.admitMu.Unlock()
			}
		}
		defer unlock()
		var res ctrl.Result
		var probe, stop bool
		res, probe, stop, clr, err = r.holdBack(ctx, rr, false)
		if stop {
			unlock()
			r.clearStale(ctx, rr.Namespace, clr)
			return res, err
		}
		if probe {
			rr.Status.BreakerProbe = true
		}
		// The lock on the target is taken in the same status write (targetlock.go).
		rr.Status.HoldsTarget = new(true)
		// A replacement names the request it takes over in the same write (handoff step 1, handoff.go).
		reserveOnAdmission(rr)
	} else if rr.Status.HoldsTarget == nil {
		// A refusal never takes the lock; false keeps it out of the limits' lock list (limitsSelectors).
		release(rr)
	}

	rr.Status.ObservedGeneration = rr.Generation
	// The request is decided: no limit holds it back any more.
	meta.RemoveStatusCondition(&rr.Status.Conditions, remediationv1alpha1.ConditionQueued)
	rr.Status.Source = out.source
	rr.Status.PolicyUID = out.policyUID
	rr.Status.PolicyRevision = out.policyRevision
	rr.Status.PlanDigest = out.planDigest
	// After the plan digest, which tells the breaker whether a refusal counts (classifyBreaker).
	r.setPhase(rr, out.phase)
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
	// The decision is written: the admission lock is released before the breaker's watermark is cleared.
	unlock()
	r.clearStale(ctx, rr.Namespace, clr)
	return ctrl.Result{}, nil
}

// beforeCheck runs what comes before the policy check of an undecided request, and reports whether the
// reconcile stops there (with its result and error). A request a limit holds back (Queued) checks only the
// limits until they let it through, so it does not read Git while it waits; holdBack lets it through only after
// it found no merged duplicate (endIfDuplicate). Every other request is decided first, so a refusal is never
// held back, but a duplicate of a merged request ends Superseded before the policy check, so neither a wait for
// the target's state nor a refusal comes first (#556): a request in a wait is not Queued.
func (r *RemediationRequestReconciler) beforeCheck(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) (
	ctrl.Result, bool, error) {
	if meta.IsStatusConditionTrue(rr.Status.Conditions, remediationv1alpha1.ConditionQueued) {
		res, _, stop, clr, err := r.holdBack(ctx, rr, true)
		if stop {
			r.clearStale(ctx, rr.Namespace, clr)
		}
		return res, stop, err
	}
	ended, err := r.endIfDuplicate(ctx, rr)
	if ended {
		r.forget(client.ObjectKeyFromObject(rr))
	}
	return ctrl.Result{}, ended || err != nil, err
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
	// wait means the target's state is not known yet (policy.OutcomeWait), or Flux is still reconciling its
	// delivery (sourcemap.Reason.Reconciling): the request is decided again later, and only the PolicyAllowed
	// condition (Unknown, reason and message) is written meanwhile.
	wait bool
	// timeoutReason and timeoutMessage are the NeedsIntervention reason and message a wait ends with once it
	// has lasted TargetStateTimeout (waitForTarget).
	timeoutReason, timeoutMessage string
}

// waitForTarget handles a decision to wait for the target's state. The wait's clock is the PolicyAllowed
// condition's lastTransitionTime while the condition is Unknown with a wait reason (waitReason); recordWait
// (through setPolicyAllowed) starts it when the condition takes a wait reason and keeps it while the reason stays
// a wait reason, so one wait episode is bounded as a whole, also when its reason alternates (KEDA has not
// reported, then Flux reconciles, then KEDA again, #556). With no such condition, or one whose time is after now,
// the wait has just started. While it has lasted less than TargetStateTimeout this returns the delay before the
// next check; past the timeout it returns a NeedsIntervention outcome (the reason of the wait it ends with) and
// no delay.
func (r *RemediationRequestReconciler) waitForTarget(rr *remediationv1alpha1.RemediationRequest, out outcome) (
	outcome, time.Duration) {
	timeout := r.targetStateTimeout()
	var waited time.Duration
	if c := meta.FindStatusCondition(rr.Status.Conditions, remediationv1alpha1.ConditionPolicyAllowed); c != nil &&
		c.Status == metav1.ConditionUnknown && waitReason(c.Reason) && waitReason(out.reason) {
		waited = max(r.now().Sub(c.LastTransitionTime.Time), 0)
	}
	if waited >= timeout {
		reason, message := out.timeoutReason, out.timeoutMessage
		if reason == "" {
			// Every wait names its end; one that does not still ends, as a person's case.
			reason, message = string(sourcemap.ReasonInvalidInput), "The wait for the target's state has no end reason"
		}
		return needsIntervention(reason, message), 0
	}
	delay := min(max(waited, targetStateMinDelay), targetStateMaxDelay, timeout-waited)
	return out, delay
}

// recordWait writes the PolicyAllowed condition of a request that waits for its target's state, unless it
// already says so, and requeues it after delay. The phase is left as it is (none or Pending), so the next
// reconcile runs the policy check again.
//
// The wait clock (lastTransitionTime) starts when the condition takes a wait reason and is kept while the
// reason stays a wait reason (setPolicyAllowed), so the wait ends TargetStateTimeout after it started.
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
// the decision that ends one). The one exception is a move between two wait reasons (waitReason) of an Unknown
// condition: that is the same wait, so it keeps its start and TargetStateTimeout bounds it as a whole (#556).
// A recorded time after now is not trusted either (a clock that went back, a status written by hand) and starts
// again at now, since a wait measured from it would never time out. A condition with the same reason and a time
// not after now keeps its time when its message or generation changes.
func setPolicyAllowed(conditions *[]metav1.Condition, c metav1.Condition, now time.Time) bool {
	c.Type = remediationv1alpha1.ConditionPolicyAllowed
	c.LastTransitionTime = metav1.Time{Time: now}
	if old := meta.FindStatusCondition(*conditions, c.Type); old != nil &&
		((old.Reason != c.Reason && !sameWait(old, &c)) || old.LastTransitionTime.After(now)) {
		meta.RemoveStatusCondition(conditions, c.Type)
	}
	return meta.SetStatusCondition(conditions, c)
}

// endWait removes a PolicyAllowed condition that records a wait for the target's state (Unknown with a wait
// reason) and reports whether it did. It is called when the limits queue a request, which happens only after a
// policy check that did not wait, so the wait it records is over; a wait after that starts its own clock
// (waitForTarget), instead of one counted from before the request was Queued.
func endWait(conditions *[]metav1.Condition) bool {
	c := meta.FindStatusCondition(*conditions, remediationv1alpha1.ConditionPolicyAllowed)
	if c == nil || c.Status != metav1.ConditionUnknown || !waitReason(c.Reason) {
		return false
	}
	meta.RemoveStatusCondition(conditions, remediationv1alpha1.ConditionPolicyAllowed)
	return true
}

// waitReason reports whether reason is one the policy check waits on rather than decides: KEDA has not reported
// on the ScaledObject (policy.ReasonScaledObjectReadinessUnknown), or Flux is reconciling the Kustomization that
// delivers the target (sourcemap.Reason.Reconciling).
func waitReason(reason string) bool {
	return reason == string(policy.ReasonScaledObjectReadinessUnknown) || sourcemap.Reason(reason).Reconciling()
}

// sameWait reports whether old and c are both an Unknown PolicyAllowed with a wait reason: one wait episode.
func sameWait(old, c *metav1.Condition) bool {
	return old.Status == metav1.ConditionUnknown && c.Status == metav1.ConditionUnknown &&
		waitReason(old.Reason) && waitReason(c.Reason)
}

func denied(reason, message string) outcome {
	return outcome{phase: remediationv1alpha1.PhaseDenied, status: metav1.ConditionFalse, reason: reason, message: message}
}

func needsIntervention(reason, message string) outcome {
	return outcome{phase: remediationv1alpha1.PhaseNeedsIntervention, status: metav1.ConditionUnknown,
		reason: reason, message: message}
}

// checkChainLink is the chain link check of check (chainLink): refused is true with the Denied outcome when the
// link does not hold (ChainLinkInvalid) or reviewer hints are off (HintsTurnedOff).
func (r *RemediationRequestReconciler) checkChainLink(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) (
	out outcome, refused bool, err error) {
	switch reason, why, err := r.chainLink(ctx, rr); {
	case err != nil:
		return outcome{}, false, err
	case reason == remediationv1alpha1.ReasonHintsTurnedOff:
		return denied(remediationv1alpha1.ReasonHintsTurnedOff, why+"; nothing was taken over"), true, nil
	case reason != "":
		return denied(remediationv1alpha1.ReasonChainLinkInvalid, why+"; nothing was taken over"), true, nil
	}
	return outcome{}, false, nil
}

// checkLinks runs the link checks that come before anything else is read about a request: a replacement's
// chain link (ADR-0028 section 4 item 2), then a revert's link (ADR-0030 section 8). The API refuses a revert that
// replaces a request, so the two never meet. refused is true with the Denied outcome of the first that fails.
func (r *RemediationRequestReconciler) checkLinks(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) (
	out outcome, refused bool, err error) {
	if out, refused, err := r.checkChainLink(ctx, rr); err != nil || refused {
		return out, refused, err
	}
	return r.checkRevertLink(ctx, rr)
}

// check gathers the input and decides. The error is non-nil only for a failure worth retrying.
//
// The refusal order (design 5.3): a missing policy or settings object, a kind the action cannot change, a missing
// target or one with another UID, a previous value that is not the live one, then the policy's checks on the
// request's own fields (policy.Preflight). Only after all of them does the source mapping read Flux objects and
// Git, and policy.Decide decide on everything, the resolved source included.
func (r *RemediationRequestReconciler) check(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) (
	outcome, error) {
	if out, refused, err := r.checkLinks(ctx, rr); err != nil || refused {
		return out, err
	}
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
	case sourcemap.NeedsIntervention:
		if res.Reason.Reconciling() {
			// Flux is still applying a revision (a pull request just merged, #556): wait for it, bounded by
			// TargetStateTimeout, rather than stop the request on a passing state.
			return fluxWait(res), nil
		}
		return needsIntervention(string(res.Reason), res.Message), nil
	default:
		// Anything unknown: a person must look. Never Denied, never retried.
		return needsIntervention(string(res.Reason), res.Message), nil
	}

	in.Source = res.Source
	d := policy.Decide(in)
	o := revertApproval(rr, decided(d, pol))
	o.source = res.Source
	if d.Outcome != policy.OutcomeAllowed {
		return o, nil
	}
	stop, tooLarge, err := r.buildTooLarge(ctx, settings, res.Source)
	if err != nil {
		return outcome{}, err
	}
	if tooLarge {
		// The refusal ends the request for good, and the build was read at the commit the mapping found for
		// the object checked above. Flux may have applied a new revision during those reads, so confirm the
		// target is still that object, and start over against the current one when it is not.
		if err := r.unchanged(ctx, ref, live); err != nil {
			return outcome{}, err
		}
		return stop, nil
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

// fluxWait is the outcome of a mapping that stopped because Flux is still reconciling the Kustomization: a
// wait whose end, after TargetStateTimeout, is NeedsIntervention with KustomizationNotReady, as before #556.
func fluxWait(res sourcemap.Result) outcome {
	return outcome{wait: true, status: metav1.ConditionUnknown, reason: string(res.Reason), message: res.Message,
		timeoutReason: string(sourcemap.ReasonKustomizationNotReady), timeoutMessage: res.Message +
			"; Flux did not finish reconciling it within the time the policy check waits, a person must look"}
}

// buildTooLarge loads the build the plan names, at the commit the mapping read, with the render check's own
// limits (semdiff.Load), and returns a NeedsIntervention outcome and true when the build is over one of them
// (TooLarge). The mapping reads up to 16 MiB while a render takes at most semdiff.MaxBuildBytes, so without this
// a build in between would be planned and approved, and refused only at the render before the commit (#482).
// Any other refusal of the tree is left to that render, which gives it with the change in place. A Git failure
// is returned for a retry. Without a Git adapter (tests) nothing is loaded and the render refuses later.
func (r *RemediationRequestReconciler) buildTooLarge(ctx context.Context,
	settings *remediationv1alpha1.RemediationSettings, src *remediationv1alpha1.ResolvedSource) (outcome, bool, error) {
	if r.OpenRepository == nil || src.BuildPath == "" {
		return outcome{}, false, nil
	}
	i := slices.IndexFunc(settings.Spec.Repositories, func(g remediationv1alpha1.GitRepository) bool {
		return g.Name == src.RepositoryRef
	})
	if i < 0 {
		return needsIntervention(string(sourcemap.ReasonInvalidInput),
			"The source mapping named a repository the settings do not hold"), true, nil
	}
	git, err := r.openRepository(ctx, settings.Spec.Repositories[i])
	if providers.IsInvalidConfig(err) {
		return needsIntervention(string(sourcemap.ReasonRepositoryUnusable),
			"The settings of the repository cannot be used to open it"), true, nil
	}
	if err != nil {
		d, _ := providers.RetryAfter(err)
		return outcome{}, false, &sourcemap.TransientError{Op: "open the repository of the plan", RetryAfter: d}
	}
	_, err = semdiff.Load(ctx, git, src.BaseCommit, string(src.BuildPath))
	if se, ok := errors.AsType[*semdiff.Error](err); ok {
		if se.Reason != semdiff.ReasonTooLarge {
			return outcome{}, false, nil
		}
		return needsIntervention(string(se.Reason), buildTooLargeMessage), true, nil
	}
	return outcome{}, false, err
}

// buildTooLargeMessage is the status message of a request whose build is over a limit of the render check's input
// (semdiff reason TooLarge), when the policy check finds it. It is fixed text, never content from Git.
var buildTooLargeMessage = fmt.Sprintf("The build is over a limit of the render check (at most %d bytes of input "+
	"and %d YAML documents counted with repetition, %d directory visits, %d files of at most %d bytes, %d bytes that "+
	"patches and transformers can add to the render, counting %d for each YAML node, %d keys or items that patches "+
	"can merge into one mapping or list); no plan was made",
	semdiff.MaxBuildBytes, semdiff.MaxBuildDocuments, semdiff.MaxVisits, semdiff.MaxFiles, semdiff.MaxFileBytes,
	semdiff.MaxTransformBytes, semdiff.NodeBytes, semdiff.MaxMergedKeys)

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
		o.timeoutReason, o.timeoutMessage = string(policy.ReasonScaledObjectNotReady), messageTargetStateTimeout
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
	if err := r.validatePullRequestPollInterval(); err != nil {
		return err
	}
	if r.NodeGrace < 0 {
		return errNegativeNodeGrace
	}
	requestPhases.setNamespace(r.Namespace)
	if err := mgr.Add(requestPhases.source.Runnable(mgr.GetCache(), &remediationv1alpha1.RemediationRequest{})); err != nil {
		return fmt.Errorf("adding the source of the RemediationRequest phase metric: %w", err)
	}
	if err := mgr.Add(sinceMarker{r: r}); err != nil {
		return fmt.Errorf("adding the start of reconciling of the RemediationRequest controller: %w", err)
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
		if err := r.clearConflictSeen(ctx, rr, outcome); err != nil {
			return ctrl.Result{}, err
		}
		if outcome == prMergeCommitPending {
			// Merged without a merge commit yet: looked at again until the host reports one or the wait ends.
			return ctrl.Result{RequeueAfter: min(r.mergeCommitWaitLeft(rr), r.pullRequestPollInterval())}, nil
		}
		if pe, ok := errors.AsType[*freshness.ProviderError](lookErr); ok && pe.RetryAfter > 0 && lookOnly(rr) {
			// The Git host asked for a delay (a rate limit): the freshness check would ask it again at once. A
			// request that carries a stale plan's mark or WritesPaused goes on to the full recheck instead, which
			// settles those (and honours a delay of its own calls). The wait never passes the full recheck's period.
			capped := *pe
			capped.RetryAfter = min(pe.RetryAfter, freshRecheckInterval)
			return r.waitOrFail(ctx, rr, &capped)
		}
		if wait, ok := r.pollOnly(rr, outcome); ok {
			// Still open, and the last full recheck is recent: only the pull request was read (#434). Replies to
			// hints that wait (a rate limit, a post that may have gone through) get the look at the end of the wait.
			if d := r.replyWait(rr); d > 0 {
				wait = min(wait, d)
			}
			return ctrl.Result{RequeueAfter: wait}, nil
		}
	}
	if mark := staleMark(rr); mark != nil && outcome == prClosed {
		return r.endStaleClosed(ctx, rr, mark)
	}
	if intent(rr) != nil {
		return r.waitHandedOver(ctx, rr, outcome)
	}
	// A replacement whose branch update happened before a restart or a lost write records it before the
	// freshness check, which would take its own commit, or a merge of it, for a stale plan.
	if res, done, err := r.recoverHandoff(ctx, rr); done {
		return res, err
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
	case outcome == prClosed, outcome == prConflicted:
		return r.endLooked(ctx, rr, outcome, fresh)
	default:
		if err := r.clearWaits(ctx, rr); err != nil {
			return ctrl.Result{}, err
		}
		return r.advance(ctx, rr, fresh)
	}
}

// endStaleClosed ends a request whose stale plan's cleanup started (the mark) and whose pull request is closed:
// Superseded with the stale reason, the end of a stale plan Shadewright closed. While the Git host refused the close
// (PullRequestNotClosed, #199) the executor kept trying it, and a retried close whose answer was lost (a timeout, a
// 5xx, a failed status write) may be the one that closed it, so PROpened says that it is not known whether that
// close or a person closed it. Either way it is no failure for the circuit breaker.
func (r *RemediationRequestReconciler) endStaleClosed(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	mark *metav1.Condition) (ctrl.Result, error) {
	if notClosed(rr) != nil {
		markClosed(rr, ReasonPullRequestClosed, "is closed ("+staleClosed(mark.Reason)+"); Shadewright kept trying "+
			"to close it, and it is not known whether its retried close or a person closed it")
	} else {
		closedByUs(rr, staleClosed(mark.Reason))
	}
	return ctrl.Result{}, r.ignoreSuperseded(r.endFreshness(ctx, rr, remediationv1alpha1.PhaseSuperseded,
		mark.Reason, mark.Message))
}

// waitHandedOver is the recheck of a request handed over in flight (handoff.go): the replacement decides what
// happens to the pull request, and this request writes nothing to Git, so neither a stale plan nor a render
// refusal closes the pull request the replacement may hold. It goes on once the intent is Done (it ends
// HandedOff) or aborted (settleHandoff). A pull request closed meanwhile was closed by a person (neither side
// closes it while the intent stands): the rejection ends this request and aborts the intent in the same write
// (declineOnEnd).
func (r *RemediationRequestReconciler) waitHandedOver(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	outcome prOutcome) (ctrl.Result, error) {
	if outcome == prClosed {
		return r.rejected(ctx, rr)
	}
	return ctrl.Result{RequeueAfter: r.pullRequestPollInterval()}, nil
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
	if errors.Is(err, freshness.ErrReconciling) {
		over, werr := r.reconcilingTooLong(ctx, rr)
		if werr != nil || !over {
			return freshness.Result{}, errors.Join(err, werr)
		}
		err = &freshness.StaleError{Reason: freshness.ReasonSourceChanged, Message: messageReconcilingTooLong}
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

// messageReconcilingTooLong is the fixed PlanFresh message of a plan taken as stale because Flux did not finish
// reconciling the delivery of the target within TargetStateTimeout.
const messageReconcilingTooLong = "Flux did not finish reconciling the Kustomization that delivers the target " +
	"within the time the freshness check waits, so the plan cannot be confirmed fresh"

// reconcilingTooLong bounds the retries of a freshness check that stops because Flux is still reconciling the
// delivery of the target (freshness.ErrReconciling, #556), in every phase, also once the pull request is open
// and no deadline bounds them. The clock is the PlanFresh condition Unknown with reason KustomizationReconciling:
// written now when there is none (or one with another reason, or a time after now), kept while the retries
// last, and removed by clearWaits once a check passes. It reports true once TargetStateTimeout has passed since
// then: the caller takes the plan as stale (SourceChanged), as it did at once before #556, so a reconcile that
// never ends cannot hold the request, or its pull request, for good. A failed status write is returned.
func (r *RemediationRequestReconciler) reconcilingTooLong(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) (
	bool, error) {
	now := r.now()
	reason := string(sourcemap.ReasonKustomizationReconciling)
	if c := meta.FindStatusCondition(rr.Status.Conditions, remediationv1alpha1.ConditionPlanFresh); c != nil &&
		c.Status == metav1.ConditionUnknown && c.Reason == reason && !c.LastTransitionTime.After(now) {
		return now.Sub(c.LastTransitionTime.Time) >= r.targetStateTimeout(), nil
	}
	meta.RemoveStatusCondition(&rr.Status.Conditions, remediationv1alpha1.ConditionPlanFresh)
	meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{
		Type:   remediationv1alpha1.ConditionPlanFresh,
		Status: metav1.ConditionUnknown,
		Reason: reason,
		Message: "Flux is reconciling the Kustomization that delivers the target; Shadewright writes nothing to " +
			"Git until the freshness check passes",
		ObservedGeneration: rr.Generation,
		LastTransitionTime: metav1.Time{Time: now},
	})
	if err := r.Status().Update(ctx, rr); err != nil {
		return false, fmt.Errorf("update status of RemediationRequest %s/%s: %w", rr.Namespace, rr.Name, err)
	}
	logf.FromContext(ctx).Info("Waiting for Flux before the freshness check of RemediationRequest",
		"remediationRequest", client.ObjectKeyFromObject(rr))
	return false, nil
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
// with the stale reason, or NeedsIntervention when the change request cannot be closed for good and no pull
// request is recorded. A close of the recorded pull request that the Git host refuses for good keeps the request
// in its phase, watching the pull request (keepWatchingStale, errStaleNotClosed). A failure worth a retry, and any
// failed read of a change request, is returned as it is and leaves the mark, so the retry closes again.
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
		message = "The plan is stale and the pull request recorded for it could not be confirmed as this " +
			"request's (neither the Git host nor the request's branch has a pull request with that number, or it is " +
			"from another branch or lacks this request's marker); it was not closed"
	case errors.Is(err, errRepositoryGone):
		// Nothing is closed: the only change request this plan could have is in a repository that cannot be
		// reached through the settings any more, and closing a number elsewhere could close someone else's.
		phase, reason = remediationv1alpha1.PhaseNeedsIntervention, reasonRepositoryUnavailable
		message = "The plan is stale and the repository it was made for is not configured any more; " +
			"its change request, if any, was not closed"
	case isMergedUnrecorded(err), providers.IsConflict(err) && rr.Status.PullRequest == nil && !isUnknownChange(err):
		// The branch's marked pull request was merged before any status recorded it (#524), or a close was refused
		// as a conflict: it ends Merged or MergedWithForeignChange, not Superseded (endUnrecordedMerge). The end is
		// a final phase, as ErrSuperseded tells the caller.
		done, endErr := r.endUnrecordedMerge(ctx, rr, err)
		switch {
		case !done:
			return err
		case errors.Is(endErr, errStopped):
			return ErrSuperseded
		}
		return endErr
	case err != nil:
		if cf, refused, ok := refusedClose(err); ok && rr.Status.PullRequest != nil {
			// The Git host refuses the close of the recorded pull request for good (#199): the request keeps its
			// phase and the mark, and its pull request is still watched (recheck), so a merge or a close by a
			// person is recorded; the close is tried again on every look, and nothing else is written to it.
			return r.keepWatchingStale(ctx, rr, cf, refused)
		}
		if isUnknownChange(err) {
			// A failed read of the change request, a refused one included (PullRequestUnreadable names it), is
			// retried with the mark kept, never an end.
			return err
		}
		refused, _, ok := refusal(err)
		if !ok {
			// A failed call never clears PullRequestNotClosed, also one that may have closed the pull request (a
			// timeout, a 5xx): only a close that went through or a read that finds it closed or merged does.
			return err
		}
		// The Git host refuses the close of a pull request found by the branch (none is recorded, so none is
		// watched), or a step before it, for good (a permission the App lacks, a rule): the plan is stale and
		// nothing more may be written for it, so a person has to close the change request. The lock stays
		// (prMayBeOpen keeps it) until checkHeld finds none open.
		phase, reason = remediationv1alpha1.PhaseNeedsIntervention, refused
		message = "The plan is stale (" + staleErr.Reason + ") and the Git host refused to close its " +
			"change request; it was not closed"
	}
	return r.endFreshness(ctx, rr, phase, reason, message)
}

// errStaleNotClosed is cleanUpStale's answer when the Git host refused for good to close a stale plan's recorded
// pull request: the request keeps its phase and watches the pull request (recheck requeues after the poll
// interval). It is not a final phase.
var errStaleNotClosed = errors.New("the Git host refused to close the stale plan's pull request")

// keepWatchingStale records condition PullRequestNotClosed for a stale plan whose close the Git host refused for
// good (cf, refused), in one status write when it changes, with one Warning Event when it is set or its reason
// changes, and returns errStaleNotClosed. PlanFresh keeps the mark (False, the stale reason), so the plan is not
// checked again and nothing but the close is written for it.
func (r *RemediationRequestReconciler) keepWatchingStale(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	cf *closeFailure, refused string) error {
	want := notClosedCondition(rr, cf, refused)
	changed, event := r.setRefused(rr, want)
	if !changed {
		return errStaleNotClosed
	}
	if err := r.Status().Update(ctx, rr); err != nil {
		return fmt.Errorf("update status of RemediationRequest %s/%s: %w", rr.Namespace, rr.Name, err)
	}
	if event {
		r.refusedEvent(rr, EventPullRequestNotClosed, want)
	}
	logf.FromContext(ctx).Info("The Git host refused to close the pull request of a stale RemediationRequest; "+
		"watching it", "remediationRequest", client.ObjectKeyFromObject(rr), "pullRequest", cf.number,
		"reason", refused)
	return errStaleNotClosed
}

// endUnrecordedMerge handles the answer of closeChangeRequest for a request with no recorded pull request whose
// own pull request may have been merged before any status recorded it (#524): a *mergedUnrecordedError (the
// listing of the branch found it merged) is checked and ended through endMerged; a *closeConflictError (a close
// refused as a conflict, maybe merged between the read and the close) is read again by its number
// (mergedUnrecorded). done is false for any other closeErr, and for a request with a recorded pull request; the
// caller then handles closeErr itself. A read that fails in the check is returned for a retry, and the retry finds
// it merged again; errStopped means the request ended. Nothing is closed here. A failed read, whatever its class,
// is never taken for the merge (isUnknownChange).
func (r *RemediationRequestReconciler) endUnrecordedMerge(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	closeErr error) (done bool, err error) {
	if rr.Status.PullRequest != nil || closeErr == nil || isUnknownChange(closeErr) {
		return false, nil
	}
	cleanCtx, cancel := bound(ctx, r.CleanupTimeout, DefaultCleanupTimeout)
	defer cancel()
	if mu, ok := errors.AsType[*mergedUnrecordedError](closeErr); ok {
		err = r.endMerged(cleanCtx, mu.git, rr, mu.cr)
		return true, boundErr(ctx, cleanCtx, "check of the merged change request", err)
	}
	if _, ok := errors.AsType[*closeConflictError](closeErr); ok {
		err = r.mergedUnrecorded(cleanCtx, rr, closeErr)
		return true, boundErr(ctx, cleanCtx, "look-up of the merged change request", err)
	}
	return false, nil
}

// mergedUnrecordedError is closeChangeRequest's answer for a pull request of the request, found by its marker,
// that was merged before any status recorded it: cr as the look-up returned it, in the repository git.
type mergedUnrecordedError struct {
	git providers.GitProvider
	cr  providers.ChangeRequest
}

// Unwrap makes it a conflict for the callers that do not look for it, as closing a merged change request is.
func (e *mergedUnrecordedError) Unwrap() error { return providers.ErrConflict }

func (e *mergedUnrecordedError) Error() string {
	return fmt.Sprintf("the change request #%d was merged before it was recorded", e.cr.Number)
}

// isMergedUnrecorded reports whether err is a [*mergedUnrecordedError].
func isMergedUnrecorded(err error) bool {
	_, ok := errors.AsType[*mergedUnrecordedError](err)
	return ok
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
	clearNotClosed(rr)
	settleLock(rr, r.VerifyOutcomes)
	if err := r.Status().Update(ctx, rr); err != nil {
		return fmt.Errorf("update status of RemediationRequest %s/%s: %w", rr.Namespace, rr.Name, err)
	}
	logf.FromContext(ctx).Info("Superseded RemediationRequest", "remediationRequest", client.ObjectKeyFromObject(rr),
		"phase", phase, "reason", reason)
	return ErrSuperseded
}

// stillOwns reads rr again from the API server, not the cache, right before closeChangeRequest closes its pull
// request: the copy the reconcile holds may predate a handoff (a predecessor suspended after its render check
// that resumes after its replacement took the pull request over), and a close is the one write that cannot be
// undone. A request handed over meanwhile, or with an intent that stands, never closes; a request gone or
// recreated under its name returns errChangeRequestNotOurs too. A read that fails is returned for a retry.
func (r *RemediationRequestReconciler) stillOwns(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) error {
	fresh := &remediationv1alpha1.RemediationRequest{}
	switch err := r.reader().Get(ctx, client.ObjectKeyFromObject(rr), fresh); {
	case apierrors.IsNotFound(err):
		return errChangeRequestNotOurs
	case err != nil:
		return fmt.Errorf("get RemediationRequest %s/%s: %w", rr.Namespace, rr.Name, err)
	}
	switch {
	case fresh.UID != rr.UID, handedOff(fresh):
		return errChangeRequestNotOurs
	case intent(fresh) != nil:
		return errHandoffInFlight
	}
	return nil
}

// closeChangeRequest closes the request's change request, if it has one, in the repository the plan was made
// for and nowhere else. A change request that is already closed, or that was never opened, is not an error; a
// merged one is, because the change landed. It returns [errRepositoryGone] when that repository is not in the
// settings, [errTargetBranchChanged] when its entry now names another target branch, [errChangeRequestNotOurs]
// when a recorded pull request is gone (changeOf: not this request's, or without its marker), and
// [freshness.ErrPaused] when the settings read now pause writes. A request with an outgoing handoff that stands or is done never closes anything: the pull
// request is, or may become, its replacement's ([errChangeRequestNotOurs] when handed over,
// [errHandoffInFlight] while the intent stands), judged on the copy it was given and, right before the close,
// on a fresh read (stillOwns).
func (r *RemediationRequestReconciler) closeChangeRequest(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) error {
	return r.closeOwn(ctx, rr, false)
}

// closeOnDeleteOf is closeChangeRequest for the finalizer. With a pull request recorded it closes every open one
// that is this request's change (closeAllOwn), and returns nil only when none is left open.
func (r *RemediationRequestReconciler) closeOnDeleteOf(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) error {
	return r.closeOwn(ctx, rr, true)
}

// closeOwn is closeChangeRequest; deleting is true for the finalizer (closeOnDeleteOf).
func (r *RemediationRequestReconciler) closeOwn(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	deleting bool) error {
	if r.OpenRepository == nil {
		return errors.New("cannot close the change request: no Git provider")
	}
	switch {
	case handedOff(rr):
		return errChangeRequestNotOurs
	case intent(rr) != nil:
		return errHandoffInFlight
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
		return &stepFailure{step: stepToken, err: freshness.WrapProvider("open the repository", err)}
	}
	// Without a recorded pull request it is found by the request's branch: it may exist although the status
	// write that records it failed (design section 6). A recorded one is read by its number, so a newer pull
	// request on the same branch never stands in for it (ADR-0030 section 1), and it is closed only when it is
	// from the request's branch to the target branch and carries this request's marker (changeOf).
	recorded := rr.Status.PullRequest
	if recorded != nil && deleting {
		return r.closeAllOwn(ctx, git, rr)
	}
	if recorded == nil {
		return r.closeUnrecorded(ctx, git, rr)
	}
	v := r.changeOf(ctx, git, rr, changeQuery{marker: true})
	switch v.state {
	case changeUnknown:
		// A 404 the branch does not confirm, an answer about another pull request and any other failure are
		// retried (#688), so a stale plan's cleanup, the deadline's and a render refusal's never end the request
		// with its pull request open on a single read.
		return v.err
	case changeGone:
		return fmt.Errorf("%w: %s", errChangeRequestNotOurs, v.gone)
	}
	if err := r.stillOwns(ctx, rr); err != nil {
		return err
	}
	return closeFailed(v.pr, git.CloseChangeRequest(ctx, v.pr.Number))
}

// closeUnrecorded is closeOwn for a request with no recorded pull request: one may exist although the status write
// that records it failed (design section 6). It reads every pull request of the branch (changeOf), and looks at
// each that carries this request's marker: an open one is closed, after stillOwns. One without the marker is
// someone else's, or another plan's, and is never touched. When one of them is merged, the change landed before
// any status recorded it (#524), and it returns a *mergedUnrecordedError for the newest merged one once the open
// ones are closed; the caller checks the merge. A close refused as a conflict (maybe merged between the read and
// the close) is a *closeConflictError, which mergedUnrecorded reads again by its number. Unknown is returned for a
// retry: a failed listing never means none was opened.
func (r *RemediationRequestReconciler) closeUnrecorded(ctx context.Context, git providers.GitProvider,
	rr *remediationv1alpha1.RemediationRequest) error {
	v := r.changeOf(ctx, git, rr, changeQuery{})
	if v.state == changeUnknown {
		return v.err
	}
	for _, cr := range v.own {
		if cr.State != providers.ChangeRequestOpen {
			continue
		}
		if err := r.stillOwns(ctx, rr); err != nil {
			return err
		}
		if err := closeFailed(cr, git.CloseChangeRequest(ctx, cr.Number)); err != nil {
			if providers.IsConflict(err) {
				return &closeConflictError{number: cr.Number, err: err}
			}
			return err
		}
	}
	if merged := v.ownMerged(); merged != nil {
		return &mergedUnrecordedError{git: git, cr: *merged}
	}
	return nil
}

// closeAllOwn is the finalizer's close of a request with a recorded pull request. It returns nil, and the
// finalizer may go, only when the recorded pull request is not open, or open without this request's marker, and
// no open pull request of the request's branch carries this request's marker:
//   - the recorded one and the open pull requests of the branch are read together (changeOf with the open
//     listing). Unknown (a failed read, a 404 the branch does not confirm, an answer about another pull request, or
//     the recorded one read as not open while the branch lists it open) is returned for a retry and keeps the
//     finalizer; its error class, ErrConflict included, is never taken for a merge.
//   - the recorded one, open with this request's marker, is closed; gone (not this request's, or without its
//     marker), closed or merged, nothing is closed on its account;
//   - then every open pull request of the branch that carries this request's marker is this request's change,
//     whatever its number, and is closed. One without the marker is left alone and does not hold the finalizer;
//   - a recorded number that is missing, or not this request's, while a listing of the branch shows a pull request
//     with this request's marker (errOwnListed, retried on every other path) is the one exception: a person deleted
//     the holder, so every open one with the marker is closed and the finalizer goes (closeListedOwn).
//
// Every close is preceded by stillOwns. A close that fails, a conflict included (the pull request may have been
// merged since the read, or the host refused for another reason), is returned for a retry, which reads again.
func (r *RemediationRequestReconciler) closeAllOwn(ctx context.Context, git providers.GitProvider,
	rr *remediationv1alpha1.RemediationRequest) error {
	closeIt := func(cr providers.ChangeRequest) error {
		if err := r.stillOwns(ctx, rr); err != nil {
			return err
		}
		return closeFailed(cr, git.CloseChangeRequest(ctx, cr.Number))
	}
	v := r.changeOf(ctx, git, rr, changeQuery{marker: true, openOnBranch: true})
	if v.state == changeUnknown && v.ownListed {
		// A person deleted the holder (ADR-0030): the recorded number is not this request's while the branch lists
		// pull requests with its marker, so those are its change. Each open one is closed, and the finalizer goes,
		// whatever the recorded number was read as. The verdict carries both listings of the branch (ownListed); one
		// that a later failure made unknown again does not, and is retried.
		return r.closeListedOwn(ctx, rr, v, closeIt)
	}
	switch finalizeRule[v.state] {
	case finalizeRetry:
		return v.err
	case finalizeClose:
		// Open with this request's marker: changeOf checked both (an unmarked one is gone).
		if err := closeIt(v.pr); err != nil {
			return err
		}
	}
	for _, o := range v.open {
		if o.Number == v.pr.Number && v.state == changeOpen {
			continue
		}
		switch ours, err := r.ownsMarker(ctx, o.Body, rr); {
		case err != nil:
			return err
		case !ours:
			continue
		}
		if err := closeIt(o); err != nil {
			return err
		}
	}
	return nil
}

// closeListedOwn is closeAllOwn on errOwnListed: it closes every pull request with this request's marker that a
// listing of the branch in v shows open (v.own, from the listing that checked the 404, and v.open), each once.
func (r *RemediationRequestReconciler) closeListedOwn(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	v changeVerdict, closeIt func(providers.ChangeRequest) error) error {
	closed := map[int64]bool{}
	for _, cr := range v.own {
		if cr.State != providers.ChangeRequestOpen || closed[cr.Number] {
			continue
		}
		if err := closeIt(cr); err != nil {
			return err
		}
		closed[cr.Number] = true
	}
	for _, o := range v.open {
		if closed[o.Number] {
			continue
		}
		switch ours, err := r.ownsMarker(ctx, o.Body, rr); {
		case err != nil:
			return err
		case !ours:
			continue
		}
		if err := closeIt(o); err != nil {
			return err
		}
		closed[o.Number] = true
	}
	return nil
}

// finalizeOutcome is what closeAllOwn does with the recorded pull request per verdict.
type finalizeOutcome int

const (
	finalizeRetry finalizeOutcome = iota
	finalizeClose
	finalizeNothing
)

// finalizeRule is closeAllOwn's rule per verdict: open is closed, merged, closed and gone close nothing on the
// recorded one's account (the open pull requests of the branch are looked at either way), unknown is retried
// and keeps the finalizer.
var finalizeRule = map[changeState]finalizeOutcome{
	changeOpen: finalizeClose, changeMerged: finalizeNothing, changeClosed: finalizeNothing,
	changeGone: finalizeNothing, changeUnknown: finalizeRetry,
}

// closeConflictError is closeChangeRequest's answer when the close of a pull request it found by the branch of a
// request with none recorded was a conflict: number is the pull request it tried to close.
type closeConflictError struct {
	number int64
	err    error
}

func (e *closeConflictError) Unwrap() error { return e.err }

func (e *closeConflictError) Error() string { return e.err.Error() }
