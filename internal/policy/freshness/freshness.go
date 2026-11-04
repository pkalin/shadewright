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

// Package freshness binds every Git write of a RemediationRequest to the exact plan that was approved
// (design sections 5.1 and 6, ADR-0003, ADR-0011).
//
// [ComputeDigest] gives the plan digest that is stored in status.planDigest when the plan is fixed.
// [Checker.Check] runs right before every write and answers one question: is the world still the one the
// digest was made for? It is fail closed. Only a definite change is a [StaleError], which makes the request
// Superseded; any other error (an API or Git host failure, a paused system, a missing digest) also stops the
// write but says nothing about the plan, so the caller retries. The exception is [ErrRepositoryUnusable]:
// settings that can never open the repository end the request in NeedsIntervention without closing anything.
package freshness

import (
	"context"
	"errors"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/gitops/sourcemap"
	"github.com/pkalin/shadewright/internal/policy"
	"github.com/pkalin/shadewright/internal/policy/digest"
	"github.com/pkalin/shadewright/internal/providers"
)

// Reasons a plan is stale (the reason of the PlanFresh condition).
const (
	// ReasonPolicyChanged: the policy was deleted, recreated (other UID) or edited (other generation).
	ReasonPolicyChanged = "PolicyChanged"
	// ReasonSettingsChanged: the repository entry was removed, or its URL, installation or branch changed.
	ReasonSettingsChanged = "SettingsChanged"
	// ReasonTargetChanged: the target object is gone or has another UID.
	ReasonTargetChanged = "TargetChanged"
	// ReasonTargetFileChanged: the target file on the target branch no longer holds the previous value.
	ReasonTargetFileChanged = "TargetFileChanged"
	// ReasonPlanChanged: the inputs recorded in the request no longer give the recorded digest.
	ReasonPlanChanged = "PlanChanged"
	// ReasonSourceChanged: mapping the target to its source again gives another repository, branch or file,
	// or the mapping now denies the change or needs a person.
	ReasonSourceChanged = "SourceChanged"
	// ReasonScaledObjectNotReady: the plan is AdjustKedaMaxReplicas and the live ScaledObject is no longer ready
	// by the rule the policy check applied ([policy.CheckTargetState]), on two reads with the rest of the check
	// in between.
	ReasonScaledObjectNotReady = "ScaledObjectNotReady"
	// ReasonModuleNotEnabled: the plan's action is contributed by an integration module that RemediationSettings
	// spec.modules no longer enables ([policy.CheckModuleEnabled]).
	ReasonModuleNotEnabled = "ModuleNotEnabled"
)

// readTimeout bounds the reads one check makes of the Git host.
const readTimeout = 30 * time.Second

// ErrRepositoryUnusable means the settings entry of the plan's repository cannot be used to open a Git
// provider (wrong provider, no signer, malformed key ID). It is permanent: the plan can be neither checked nor
// closed until an administrator fixes the settings, so retrying would never end.
var ErrRepositoryUnusable = errors.New("the repository settings cannot be used to open it")

// ErrPaused means RemediationSettings pauses every write. The plan is not stale; the write waits.
var ErrPaused = errors.New("remediation is paused")

// ErrWriteBlocked means the kill switch could not be read right before a write (the settings are missing or
// the API server did not answer), so the write was not made: the gate fails closed. The write is retried.
var ErrWriteBlocked = errors.New("the write was blocked: the kill switch cannot be read")

// StaleError says the plan the request was approved for is no longer the plan the world calls for.
type StaleError struct {
	// Reason is one of the Reason constants.
	Reason string
	// Message is safe to put in a condition: it holds no URL, token or provider text.
	Message string
}

func (e *StaleError) Error() string { return e.Reason + ": " + e.Message }

func stale(reason, format string, args ...any) error {
	return &StaleError{Reason: reason, Message: fmt.Sprintf(format, args...)}
}

// Paused reports whether the settings pause every write.
func Paused(settings *v1alpha1.RemediationSettings) bool {
	return settings.Spec.Paused != nil && *settings.Spec.Paused
}

// RepositoryOf returns the identity recorded in status.planRepository for a settings entry.
func RepositoryOf(repo v1alpha1.GitRepository) *v1alpha1.PlanRepository {
	pr := &v1alpha1.PlanRepository{URL: repo.URL}
	if repo.GitHub != nil {
		pr.InstallationID = repo.GitHub.InstallationID
	}
	return pr
}

// FindRepository returns the settings entry that is the repository a plan was made for: the same URL and
// installation, whatever its name is now. It reports false when there is none, or when the plan recorded none.
func FindRepository(settings *v1alpha1.RemediationSettings, want *v1alpha1.PlanRepository) (v1alpha1.GitRepository, bool) {
	if want == nil {
		return v1alpha1.GitRepository{}, false
	}
	for _, r := range settings.Spec.Repositories {
		if *RepositoryOf(r) == *want {
			return r, true
		}
	}
	return v1alpha1.GitRepository{}, false
}

// ProviderError is a failed call to the Git provider. It deliberately wraps nothing: the text of a provider
// or transport error may hold a repository URL with credentials. It keeps only the class of the failure and
// the delay the remote system asked for.
type ProviderError struct {
	// Op says what failed, a fixed text.
	Op string
	// Class is the sentinel the failure matches (providers.ErrNotFound, ErrConflict, ErrForbidden, ErrRejected,
	// ErrRetryable, ErrTooLarge, context.Canceled, context.DeadlineExceeded), or nil for an unclassified failure.
	Class error
	// RetryAfter is the delay the remote system asked for; zero when it gave none.
	RetryAfter time.Duration
}

func (e *ProviderError) Error() string {
	if e.Class == nil {
		return e.Op + " failed"
	}
	return e.Op + " failed: " + e.Class.Error()
}

// Is matches the class, so errors.Is(err, providers.ErrRetryable) works on the sanitised error.
func (e *ProviderError) Is(target error) bool { return e.Class != nil && target == e.Class }

// provider replaces err by a ProviderError for op, dropping its text and chain. An error of the kill switch
// (ErrPaused, ErrWriteBlocked), which a gated provider returns instead of calling the host, is returned as it
// is: it holds no provider text, and the caller must still tell a pause from a failure of the host.
func provider(op string, err error) error {
	if errors.Is(err, ErrPaused) || errors.Is(err, ErrWriteBlocked) {
		return err
	}
	pe := &ProviderError{Op: op}
	for _, class := range []error{providers.ErrNotFound, providers.ErrConflict, providers.ErrForbidden,
		providers.ErrRejected, providers.ErrRetryable, providers.ErrTooLarge, context.Canceled,
		context.DeadlineExceeded} {
		if errors.Is(err, class) {
			pe.Class = class
			break
		}
	}
	pe.RetryAfter, _ = providers.RetryAfter(err)
	return pe
}

// WrapProvider is provider for callers outside the package.
func WrapProvider(op string, err error) error { return provider(op, err) }

// BranchName is the branch that carries the plan: shadewright/<request-uid> (design section 6).
func BranchName(rr *v1alpha1.RemediationRequest) string {
	return "shadewright/" + string(rr.UID)
}

// RevisionOf returns the policyRevision text for a policy: name/generation.
func RevisionOf(pol *v1alpha1.RemediationPolicy) string {
	return fmt.Sprintf("%s/%d", pol.Name, pol.Generation)
}

// repository finds the settings entry a source refers to.
func repository(settings *v1alpha1.RemediationSettings, name v1alpha1.Name) (v1alpha1.GitRepository, bool) {
	for _, r := range settings.Spec.Repositories {
		if r.Name == name {
			return r, true
		}
	}
	return v1alpha1.GitRepository{}, false
}

// DigestInput collects what the plan digest covers: the request's action and target UID, the resolved source
// with the repository URL and installation taken from settings, and the policy UID and generation.
func DigestInput(rr *v1alpha1.RemediationRequest, src *v1alpha1.ResolvedSource, pol *v1alpha1.RemediationPolicy,
	settings *v1alpha1.RemediationSettings) (digest.Input, error) {
	if src == nil {
		return digest.Input{}, errors.New("the request has no resolved source")
	}
	repo, ok := repository(settings, src.RepositoryRef)
	if !ok {
		return digest.Input{}, fmt.Errorf("repository %q is not in the settings", src.RepositoryRef)
	}
	var installation int64
	if repo.GitHub != nil {
		installation = repo.GitHub.InstallationID
	}
	return digest.Input{
		Action:    rr.Spec.Action,
		TargetUID: rr.Spec.TargetRef.UID,
		Source: digest.Source{
			RepositoryRef:  src.RepositoryRef,
			URL:            repo.URL,
			InstallationID: installation,
			TargetBranch:   src.TargetBranch,
			Path:           src.Path,
			BuildPath:      src.BuildPath,
			BaseCommit:     src.BaseCommit,
		},
		PolicyUID:        v1alpha1.UID(pol.UID),
		PolicyGeneration: pol.Generation,
	}, nil
}

// ComputeDigest returns the plan digest of a request, its source, its policy and the settings.
func ComputeDigest(rr *v1alpha1.RemediationRequest, src *v1alpha1.ResolvedSource, pol *v1alpha1.RemediationPolicy,
	settings *v1alpha1.RemediationSettings) (string, error) {
	in, err := DigestInput(rr, src, pol, settings)
	if err != nil {
		return "", err
	}
	return digest.Compute(in), nil
}

// RepositoryOpener opens the Git repository a settings entry names. The provider it returns is not bound to
// the context it was opened with, so [Result.Git] can be used after the check.
type RepositoryOpener func(ctx context.Context, repo v1alpha1.GitRepository) (providers.GitProvider, error)

// SourceResolver maps the target to its file in Git (ADR-0012). It is the resolver the policy check uses;
// [sourcemap.Resolver] implements it.
type SourceResolver interface {
	Resolve(ctx context.Context, in sourcemap.Input) (sourcemap.Result, error)
}

// Checker answers whether a request's plan is still fresh.
type Checker struct {
	// Reader must read straight from the API server, never from a cache: a cached copy can still hold a
	// deleted policy or an older setting.
	Reader client.Reader
	// OpenRepository opens the repository of the request's source.
	OpenRepository RepositoryOpener
	// Source is the resolver the policy check used. Check maps the target again with it before the file is
	// read, so a write never goes to a file that no longer declares the target. Nil fails closed.
	Source SourceResolver
}

// Result is what a check that found the plan fresh read: the head of the target branch, the target file at
// that head and the repository it was read from, so the caller can render the change at that commit without
// opening the repository or reading the head and the file again.
type Result struct {
	// Head is the head of the target branch the target file was checked at.
	Head string
	// File is the content of the target file at Head.
	File []byte
	// Git is the repository of the plan's settings entry, as the check opened it.
	Git providers.GitProvider
}

// Check returns a Result and a nil error only when every input of the plan is unchanged since the digest
// was made: the same policy UID and generation, the same settings entry, the module that contributes the action
// still enabled, the same target object (for AdjustKedaMaxReplicas, a ScaledObject that is still ready), and the
// target file on the target branch still holds the previous value. A merge to the target branch that does not touch the file leaves the plan fresh.
//
// It returns a [*StaleError] when an input definitely changed, [ErrPaused] when settings pause writes, and
// another error when an input could not be read. In every case except nil the caller must not write, and the
// Result is empty.
func (c Checker) Check(ctx context.Context, rr *v1alpha1.RemediationRequest) (Result, error) {
	src := rr.Status.Source
	if rr.Status.PlanDigest == "" || src == nil {
		return Result{}, errors.New("the request has no plan digest")
	}

	pol, settings, repo, err := c.loadPlan(ctx, rr)
	if err != nil {
		return Result{}, err
	}

	live, notReady, err := c.checkTarget(ctx, rr, pol)
	if err != nil {
		return Result{}, err
	}
	mapped, err := c.checkSource(ctx, rr, pol, settings)
	if _, isStale := errors.AsType[*StaleError](err); isStale {
		// A stale verdict closes the change request, so it gets the consistency checks a fresh one gets first.
		return Result{}, c.confirmStale(ctx, rr, repo, pol, settings, live, mapped, err)
	}
	if err != nil {
		return Result{}, err
	}
	git, file, err := c.checkFile(ctx, rr, repo, mapped.Source.BaseCommit)
	if err != nil {
		return Result{}, err
	}
	if notReady {
		// The target failed its state check on the first read. The mapping and the Git reads above took a
		// while; one blip of the target's status must not close an approved plan, so it is read again.
		return Result{}, c.confirmNotReady(ctx, rr, pol)
	}
	// The mapping and the Git reads (up to readTimeout each) take long. Revalidate last what they were based
	// on: the target (UID and resource version, since the mapping compared the live object with Git) and the
	// Flux objects the mapping saw. If any changed meanwhile, the checks above describe different objects: not
	// fresh, not stale; the caller retries.
	if err := c.recheckInputs(ctx, rr, live, mapped.Observed); err != nil {
		return Result{}, err
	}
	// Read the policy and the settings once more, last, before saying "fresh": a pause, a tightened or deleted
	// policy, or a changed repository entry meanwhile must not pass. It is not decided
	// as stale here; the next check does that on a calm read.
	switch _, _, _, err := c.loadPlan(ctx, rr); {
	case err == nil:
		// A read that answered as the context ended may have been cut short, and "fresh" has a deadline of its
		// own: nil is returned only while the context is live.
		if err := ctx.Err(); err != nil {
			return Result{}, fmt.Errorf("the plan was checked, but the check ran out of time: %w", err)
		}
		// checkFile compared the mapped commit with the head before and after its read, so it is the head the
		// file was read and checked at.
		return Result{Head: mapped.Source.BaseCommit, File: file, Git: git}, nil
	case errors.Is(err, ErrPaused):
		return Result{}, err
	default:
		if _, isStale := errors.AsType[*StaleError](err); isStale {
			return Result{}, errors.New("the policy or the settings changed while the plan was checked, checking again")
		}
		return Result{}, err
	}
}

// confirmNotReady decides on a target that failed its state check on the first read of the check: the target
// is read and checked again (UID, labels, previous value, then state), and the plan is stale only when the
// state check fails again; a target that passes now is a retry, not fresh, because the rest of the check ran
// on the first read. Last, as for every stale verdict, the policy and the settings are read again: a pause
// returns ErrPaused and any change a retry, so nothing is closed while writes are paused.
func (c Checker) confirmNotReady(ctx context.Context, rr *v1alpha1.RemediationRequest, pol *v1alpha1.RemediationPolicy) error {
	_, notReady, err := c.checkTarget(ctx, rr, pol)
	if err != nil {
		return err
	}
	if !notReady {
		return errors.New("the target's state changed while the plan was checked, checking again")
	}
	switch polNow, _, _, err := c.loadPlan(ctx, rr); {
	case errors.Is(err, ErrPaused):
		return err
	case err != nil:
		if _, isStale := errors.AsType[*StaleError](err); isStale {
			return errors.New("the policy or the settings changed while the plan was checked, checking again")
		}
		return err
	case polNow.UID != pol.UID || polNow.Generation != pol.Generation:
		return errors.New("the policy changed while the plan was checked, checking again")
	}
	return stale(ReasonScaledObjectNotReady, "%s %s/%s is not ready on two reads, so the raised ceiling is not the fix",
		rr.Spec.TargetRef.Kind, rr.Spec.TargetRef.Namespace, rr.Spec.TargetRef.Name)
}

// confirmStale returns staleErr, the verdict of a mapping that no longer gives the plan's source, only when
// that mapping still describes the world, checked in the order of the fresh path: when it read the plan's
// repository and target branch, that branch's head is still the commit it read (or the branch is still missing);
// then the target and the Flux objects it read are unchanged; last, the policy and the settings it was given
// are unchanged. Otherwise the mapping may describe a delivery, a commit or a configuration that has gone, while
// the current one still matches the plan: not stale, not fresh; the caller retries. A mapping that read another
// repository or branch needs no Git read: the unchanged Flux objects deliver the target from there, which is
// stale whatever the plan's branch holds.
func (c Checker) confirmStale(ctx context.Context, rr *v1alpha1.RemediationRequest, repo v1alpha1.GitRepository,
	pol *v1alpha1.RemediationPolicy, settings *v1alpha1.RemediationSettings, live *unstructured.Unstructured,
	mapped sourcemap.Result, staleErr error) error {
	src := rr.Status.Source
	if tree := mapped.Tree(); tree != nil && tree.RepositoryRef == src.RepositoryRef && tree.Branch == src.TargetBranch {
		if err := c.checkHead(ctx, repo, tree.Commit); err != nil {
			return err
		}
	}
	if err := c.recheckInputs(ctx, rr, live, mapped.Observed); err != nil {
		return err
	}
	// The mapping was given the repositories of these settings and the gitOps rules of this policy: an edit
	// meanwhile (another repository with the same URL, other allowed paths) may change its verdict.
	switch polNow, setNow, _, err := c.loadPlan(ctx, rr); {
	case errors.Is(err, ErrPaused):
		return err
	case err != nil:
		if _, isStale := errors.AsType[*StaleError](err); isStale {
			return errors.New("the policy or the settings changed while the plan was checked, checking again")
		}
		return err
	case polNow.UID != pol.UID || polNow.Generation != pol.Generation ||
		setNow.UID != settings.UID || setNow.Generation != settings.Generation:
		return errors.New("the policy or the settings changed while the plan was checked, checking again")
	}
	return staleErr
}

// checkHead reads the head of the plan's target branch and fails with a retry error unless it is want; an empty
// want means the branch was found missing, and it must still be.
func (c Checker) checkHead(ctx context.Context, repo v1alpha1.GitRepository, want string) error {
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	_, head, err := c.openHead(ctx, repo)
	switch {
	case want == "" && errors.Is(err, providers.ErrNotFound):
		return nil
	case err != nil:
		return err
	case head != want:
		return errors.New("the target branch moved while its source was mapped, checking again")
	}
	return nil
}

// openHead opens the plan's repository and reads the head of its target branch (loadPlan checked that the
// entry still names the plan's branch). Settings that can never open the repository give ErrRepositoryUnusable;
// a failed call gives a ProviderError, whose class (providers.ErrNotFound for a missing branch) is kept.
func (c Checker) openHead(ctx context.Context, repo v1alpha1.GitRepository) (providers.GitProvider, string, error) {
	if c.OpenRepository == nil {
		return nil, "", errors.New("no Git provider is available")
	}
	git, err := c.OpenRepository(ctx, repo)
	if providers.IsInvalidConfig(err) {
		return nil, "", ErrRepositoryUnusable
	}
	if err != nil {
		return nil, "", provider("open the repository", err)
	}
	head, err := git.BranchHead(ctx, string(repo.TargetBranch))
	if err != nil {
		return nil, "", provider("read the head of the target branch", err)
	}
	return git, head, nil
}

// recheckInputs reads the target and the Flux objects of the mapping again and fails with a retry error if
// any has another UID or resource version than when it was read.
func (c Checker) recheckInputs(ctx context.Context, rr *v1alpha1.RemediationRequest, live *unstructured.Unstructured,
	observed []sourcemap.Observed) error {
	again, err := c.readTarget(ctx, rr.Spec.TargetRef)
	if err != nil {
		return err
	}
	if again.GetUID() != live.GetUID() || again.GetResourceVersion() != live.GetResourceVersion() {
		return errors.New("the target changed while its source was checked, checking again")
	}
	for _, o := range observed {
		now := &unstructured.Unstructured{}
		now.SetGroupVersionKind(o.GVK)
		err := c.Reader.Get(ctx, client.ObjectKey{Namespace: o.Namespace, Name: o.Name}, now)
		missing := apierrors.IsNotFound(err) || meta.IsNoMatchError(err)
		switch {
		case missing && o.Missing:
			continue
		case missing:
			return fmt.Errorf("%s %s/%s is gone, checking again", o.GVK.Kind, o.Namespace, o.Name)
		case err != nil:
			return fmt.Errorf("get %s %s/%s: %w", o.GVK.Kind, o.Namespace, o.Name, err)
		case o.Missing:
			return fmt.Errorf("%s %s/%s appeared while the plan was checked, checking again", o.GVK.Kind, o.Namespace, o.Name)
		}
		if string(now.GetUID()) != o.UID || now.GetResourceVersion() != o.ResourceVersion {
			return fmt.Errorf("%s %s/%s changed while the plan was checked, checking again", o.GVK.Kind, o.Namespace, o.Name)
		}
	}
	return nil
}

// loadPlan reads the settings and the policy straight from the API server and checks them against the plan:
// not paused, the same policy UID and revision, the same repository entry, and the same digest. It returns the
// repository entry of the plan.
func (c Checker) loadPlan(ctx context.Context, rr *v1alpha1.RemediationRequest) (*v1alpha1.RemediationPolicy,
	*v1alpha1.RemediationSettings, v1alpha1.GitRepository, error) {
	src := rr.Status.Source
	// Settings first: while writes are paused nothing else is decided, because a stale plan leads to a
	// write (closing its change request).
	settings := &v1alpha1.RemediationSettings{}
	settingsErr := c.Reader.Get(ctx, client.ObjectKey{Name: v1alpha1.SettingsName}, settings)
	if settingsErr == nil && Paused(settings) {
		return nil, nil, v1alpha1.GitRepository{}, ErrPaused
	}
	pol := &v1alpha1.RemediationPolicy{}
	switch err := c.Reader.Get(ctx, client.ObjectKey{Name: string(rr.Spec.PolicyRef)}, pol); {
	case apierrors.IsNotFound(err):
		return nil, nil, v1alpha1.GitRepository{}, stale(ReasonPolicyChanged, "The policy %q no longer exists", rr.Spec.PolicyRef)
	case err != nil:
		return nil, nil, v1alpha1.GitRepository{}, fmt.Errorf("get RemediationPolicy %s: %w", rr.Spec.PolicyRef, err)
	}
	switch {
	case apierrors.IsNotFound(settingsErr):
		return nil, nil, v1alpha1.GitRepository{}, stale(ReasonSettingsChanged, "RemediationSettings default no longer exists")
	case settingsErr != nil:
		return nil, nil, v1alpha1.GitRepository{}, fmt.Errorf("get RemediationSettings default: %w", settingsErr)
	}

	if v1alpha1.UID(pol.UID) != rr.Status.PolicyUID || RevisionOf(pol) != rr.Status.PolicyRevision {
		return nil, nil, v1alpha1.GitRepository{}, stale(ReasonPolicyChanged, "The policy %q is not the one that allowed the plan", rr.Spec.PolicyRef)
	}
	// The action's module must still be enabled: a cluster admin who disables it withdraws the action.
	if reason, msg := policy.CheckModuleEnabled(settings, rr.Spec.Action.Type); reason != "" {
		return nil, nil, v1alpha1.GitRepository{}, stale(ReasonModuleNotEnabled, "%s", msg)
	}
	repo, ok := repository(settings, src.RepositoryRef)
	if !ok || repo.TargetBranch != src.TargetBranch {
		return nil, nil, v1alpha1.GitRepository{}, stale(ReasonSettingsChanged, "The repository %q or its target branch changed in the settings",
			src.RepositoryRef)
	}
	want, err := ComputeDigest(rr, src, pol, settings)
	if err != nil {
		return nil, nil, v1alpha1.GitRepository{}, fmt.Errorf("compute the plan digest: %w", err)
	}
	if want != rr.Status.PlanDigest {
		return nil, nil, v1alpha1.GitRepository{}, stale(ReasonPlanChanged, "The plan, its source or the repository settings changed")
	}
	return pol, settings, repo, nil
}

// readTarget reads the live target straight from the API server.
func (c Checker) readTarget(ctx context.Context, ref v1alpha1.ObjectRef) (*unstructured.Unstructured, error) {
	gv, err := schema.ParseGroupVersion(ref.APIVersion)
	if err != nil {
		return nil, fmt.Errorf("invalid target apiVersion: %w", err)
	}
	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(gv.WithKind(ref.Kind))
	switch err := c.Reader.Get(ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, live); {
	case apierrors.IsNotFound(err) || meta.IsNoMatchError(err):
		return nil, stale(ReasonTargetChanged, "%s %s/%s no longer exists", ref.Kind, ref.Namespace, ref.Name)
	case err != nil:
		return nil, fmt.Errorf("get %s %s/%s: %w", ref.Kind, ref.Namespace, ref.Name, err)
	}
	return live, nil
}

// checkSource runs the source mapping again (ADR-0012) and compares it with status.source. The repository,
// branch, path and build directory must be the recorded ones; the base commit is not compared, because it moves with every merge
// to the target branch and checkFile decides whether such a merge touched the file. A failed mapping is not
// stale (the caller retries); a mapping that now denies or needs a person never passes as fresh. The mapping
// is returned with a stale verdict too, for confirmStale; on a pass its Source is set.
func (c Checker) checkSource(ctx context.Context, rr *v1alpha1.RemediationRequest, pol *v1alpha1.RemediationPolicy,
	settings *v1alpha1.RemediationSettings) (sourcemap.Result, error) {
	if c.Source == nil {
		return sourcemap.Result{}, errors.New("no source resolver is available")
	}
	// The whole mapping runs under the caller's deadline (the controller's freshness deadline), not a fixed one.
	res, err := c.Source.Resolve(ctx, sourcemap.Input{
		Target:       rr.Spec.TargetRef,
		Action:       rr.Spec.Action,
		Repositories: settings.Spec.Repositories,
		GitOps:       pol.Spec.GitOps,
	})
	if err != nil {
		// The resolver sanitises its errors; the caller sees "not fresh" and retries.
		return sourcemap.Result{}, fmt.Errorf("resolve the source again: %w", err)
	}
	if res.Outcome != sourcemap.Resolved {
		// Denied, NeedsIntervention or anything unknown. Only the reason is reported: the message may hold
		// paths or text from Git.
		return res, stale(ReasonSourceChanged, "Mapping the target to its source again no longer resolves it (%s)", res.Reason)
	}
	got, want := res.Source, rr.Status.Source
	if got == nil || got.RepositoryRef != want.RepositoryRef || got.TargetBranch != want.TargetBranch ||
		got.Path != want.Path || got.BuildPath != want.BuildPath {
		return res, stale(ReasonSourceChanged, "The target is no longer declared in the repository, branch, file and build directory of the plan")
	}
	return res, nil
}

// checkTarget checks that the live target is still the object the plan is about. It returns the object read,
// so the caller can tell whether it changed later, and whether it failed the action's state check (for
// AdjustKedaMaxReplicas, a ScaledObject that is not ready), which the caller confirms before it calls the plan
// stale. The state is checked after the previous value, so a changed value is reported as TargetChanged.
func (c Checker) checkTarget(ctx context.Context, rr *v1alpha1.RemediationRequest, pol *v1alpha1.RemediationPolicy) (
	*unstructured.Unstructured, bool, error) {
	ref := rr.Spec.TargetRef
	live, err := c.readTarget(ctx, ref)
	if err != nil {
		return nil, false, err
	}
	if string(live.GetUID()) != string(ref.UID) {
		return nil, false, stale(ReasonTargetChanged, "%s %s/%s has another UID than the plan names", ref.Kind, ref.Namespace, ref.Name)
	}
	// The policy decided on the labels and on the previous value measured from the live field. Either may
	// have changed since, while the UID and the file in Git stayed the same.
	if reason, msg := policy.CheckObjectSelector(pol, live.GetLabels()); reason != "" {
		return nil, false, stale(ReasonTargetChanged, "%s", msg)
	}
	field, err := sourcemap.FieldFor(rr.Spec.Action, ref)
	if err != nil {
		return nil, false, fmt.Errorf("locate the field of the action: %w", err)
	}
	previous, ok := sourcemap.PreviousValue(rr.Spec.Action)
	if !ok {
		return nil, false, errors.New("the action has no previous value")
	}
	if got, found := field.Get(live.Object); !found || !field.Equals(got, previous) {
		return nil, false, stale(ReasonTargetChanged, "%s %s/%s no longer holds the previous value of %s on the live object",
			ref.Kind, ref.Namespace, ref.Name, field)
	}
	// The policy check refused AdjustKedaMaxReplicas on a ScaledObject that is not ready. A trigger that breaks
	// after the decision must not let the raised ceiling through, so the same rule runs before every write; a
	// state not known yet counts as not ready here.
	reason, _ := policy.CheckTargetState(rr.Spec.Action.Type, live.Object)
	return live, reason != "", nil
}

// checkFile reads the target file at the head of the target branch, not of the pull request branch, so a merge
// that changed the value is seen, and checks that it still holds the previous value. It reads the head again
// after the file and fails with a retry error if the branch moved. On a pass it returns the repository it
// opened and the file it read.
func (c Checker) checkFile(ctx context.Context, rr *v1alpha1.RemediationRequest, repo v1alpha1.GitRepository,
	mapped string) (providers.GitProvider, []byte, error) {
	src := rr.Status.Source
	field, err := sourcemap.FieldFor(rr.Spec.Action, rr.Spec.TargetRef)
	if err != nil {
		return nil, nil, fmt.Errorf("locate the field of the action: %w", err)
	}
	previous, ok := sourcemap.PreviousValue(rr.Spec.Action)
	if !ok {
		return nil, nil, errors.New("the action has no previous value")
	}
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	git, head, err := c.openHead(ctx, repo)
	if err != nil {
		return nil, nil, err
	}
	// The mapping and this read must see one commit. If the branch moved since the mapping, the mapped source
	// may describe another tree than the file read here: not fresh, not stale; the caller retries.
	if head != mapped {
		return nil, nil, errors.New("the target branch moved while its source was mapped, checking again")
	}
	file, err := git.ReadFile(ctx, head, string(src.Path))
	// A failed read is returned as it is, so a delay the host asked for (Retry-After) is kept and the host is
	// not called again at once.
	if err != nil && !errors.Is(err, providers.ErrNotFound) {
		return nil, nil, provider("read the target file on the target branch", err)
	}
	// The read can take long. If the branch moved meanwhile, the file (or its absence) may not be the one at
	// head, so neither a pass nor a "no longer exists" is trusted: not fresh, not stale; the caller retries.
	// This check comes before the outcome of the read is looked at.
	after, headErr := git.BranchHead(ctx, string(src.TargetBranch))
	if headErr != nil {
		return nil, nil, provider("read the head of the target branch", headErr)
	}
	if after != head {
		return nil, nil, errors.New("the target branch moved while the target file was read, checking again")
	}
	if err != nil { // ErrNotFound, the only error left
		return nil, nil, stale(ReasonTargetFileChanged, "%s no longer exists on the target branch", src.Path)
	}
	got, found, err := sourcemap.ReadField(file.Data, rr.Spec.TargetRef, field)
	if err != nil {
		return nil, nil, stale(ReasonTargetFileChanged, "%s on the target branch no longer declares the target alone: %v", src.Path, err)
	}
	if !found || !field.Equals(got, previous) {
		return nil, nil, stale(ReasonTargetFileChanged, "%s on the target branch no longer holds the previous value of %s",
			src.Path, field)
	}
	return git, file.Data, nil
}
