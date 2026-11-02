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
// [Checker.Fresh] runs right before every write and answers one question: is the world still the one the
// digest was made for? It is fail closed. Only a definite change is a [StaleError], which makes the request
// Superseded; any other error (an API or Git host failure, a paused system, a missing digest) also stops the
// write but says nothing about the plan, so the caller retries.
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
)

// readTimeout bounds the reads one check makes of the Git host.
const readTimeout = 30 * time.Second

// ErrPaused means RemediationSettings pauses every write. The plan is not stale; the write waits.
var ErrPaused = errors.New("remediation is paused")

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
	// Class is the sentinel the failure matches (providers.ErrNotFound, ErrConflict, ErrRetryable, ErrTooLarge,
	// context.Canceled, context.DeadlineExceeded), or nil for an unclassified failure.
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

// provider replaces err by a ProviderError for op, dropping its text and chain.
func provider(op string, err error) error {
	pe := &ProviderError{Op: op}
	for _, class := range []error{providers.ErrNotFound, providers.ErrConflict, providers.ErrRetryable,
		providers.ErrTooLarge, context.Canceled, context.DeadlineExceeded} {
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

// RepositoryOpener opens the Git repository a settings entry names.
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
	// Source is the resolver the policy check used. Fresh maps the target again with it before the file is
	// read, so a write never goes to a file that no longer declares the target. Nil fails closed.
	Source SourceResolver
}

// Fresh returns nil only when every input of the plan is unchanged since the digest was made: the same policy
// UID and generation, the same settings entry, the same target object, and the target file on the target
// branch still holds the previous value. A merge to the target branch that does not touch the file leaves the
// plan fresh.
//
// It returns a [*StaleError] when an input definitely changed, [ErrPaused] when settings pause writes, and
// another error when an input could not be read. In every case except nil the caller must not write.
func (c Checker) Fresh(ctx context.Context, rr *v1alpha1.RemediationRequest) error {
	src := rr.Status.Source
	if rr.Status.PlanDigest == "" || src == nil {
		return errors.New("the request has no plan digest")
	}

	// Settings first: while writes are paused nothing else is decided, because a stale plan leads to a
	// write (closing its change request).
	settings := &v1alpha1.RemediationSettings{}
	settingsErr := c.Reader.Get(ctx, client.ObjectKey{Name: "default"}, settings)
	if settingsErr == nil && Paused(settings) {
		return ErrPaused
	}
	pol := &v1alpha1.RemediationPolicy{}
	switch err := c.Reader.Get(ctx, client.ObjectKey{Name: string(rr.Spec.PolicyRef)}, pol); {
	case apierrors.IsNotFound(err):
		return stale(ReasonPolicyChanged, "The policy %q no longer exists", rr.Spec.PolicyRef)
	case err != nil:
		return fmt.Errorf("get RemediationPolicy %s: %w", rr.Spec.PolicyRef, err)
	}
	switch {
	case apierrors.IsNotFound(settingsErr):
		return stale(ReasonSettingsChanged, "RemediationSettings default no longer exists")
	case settingsErr != nil:
		return fmt.Errorf("get RemediationSettings default: %w", settingsErr)
	}

	if v1alpha1.UID(pol.UID) != rr.Status.PolicyUID || RevisionOf(pol) != rr.Status.PolicyRevision {
		return stale(ReasonPolicyChanged, "The policy %q is not the one that allowed the plan", rr.Spec.PolicyRef)
	}
	repo, ok := repository(settings, src.RepositoryRef)
	if !ok || repo.TargetBranch != src.TargetBranch {
		return stale(ReasonSettingsChanged, "The repository %q or its target branch changed in the settings",
			src.RepositoryRef)
	}
	want, err := ComputeDigest(rr, src, pol, settings)
	if err != nil {
		return fmt.Errorf("compute the plan digest: %w", err)
	}
	if want != rr.Status.PlanDigest {
		return stale(ReasonPlanChanged, "The plan, its source or the repository settings changed")
	}

	live, err := c.checkTarget(ctx, rr, pol)
	if err != nil {
		return err
	}
	commit, err := c.checkSource(ctx, rr, pol, settings)
	if err != nil {
		return err
	}
	// The mapping compared the live object with Git. If the object changed while it ran, the mapping and the
	// checks above describe different objects: not fresh, not stale; the caller retries.
	again, err := c.readTarget(ctx, rr.Spec.TargetRef)
	if err != nil {
		return err
	}
	if again.GetUID() != live.GetUID() || again.GetResourceVersion() != live.GetResourceVersion() {
		return errors.New("the target changed while its source was mapped, checking again")
	}
	return c.checkFile(ctx, rr, repo, commit)
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
// branch and path must be the recorded ones; the base commit is not compared, because it moves with every merge
// to the target branch and checkFile decides whether such a merge touched the file. A failed mapping is not
// stale (the caller retries); a mapping that now denies or needs a person never passes as fresh.
func (c Checker) checkSource(ctx context.Context, rr *v1alpha1.RemediationRequest, pol *v1alpha1.RemediationPolicy,
	settings *v1alpha1.RemediationSettings) (string, error) {
	if c.Source == nil {
		return "", errors.New("no source resolver is available")
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
		return "", fmt.Errorf("resolve the source again: %w", err)
	}
	if res.Outcome != sourcemap.Resolved {
		// Denied, NeedsIntervention or anything unknown. Only the reason is reported: the message may hold
		// paths or text from Git.
		return "", stale(ReasonSourceChanged, "Mapping the target to its source again no longer resolves it (%s)", res.Reason)
	}
	got, want := res.Source, rr.Status.Source
	if got == nil || got.RepositoryRef != want.RepositoryRef || got.TargetBranch != want.TargetBranch ||
		got.Path != want.Path {
		return "", stale(ReasonSourceChanged, "The target is no longer declared in the repository, branch and file of the plan")
	}
	return got.BaseCommit, nil
}

// checkTarget checks that the live target is still the object the plan is about. It returns the object read,
// so the caller can tell whether it changed later.
func (c Checker) checkTarget(ctx context.Context, rr *v1alpha1.RemediationRequest, pol *v1alpha1.RemediationPolicy) (
	*unstructured.Unstructured, error) {
	ref := rr.Spec.TargetRef
	live, err := c.readTarget(ctx, ref)
	if err != nil {
		return nil, err
	}
	if string(live.GetUID()) != string(ref.UID) {
		return nil, stale(ReasonTargetChanged, "%s %s/%s has another UID than the plan names", ref.Kind, ref.Namespace, ref.Name)
	}
	// The policy decided on the labels and on the previous value measured from the live field. Either may
	// have changed since, while the UID and the file in Git stayed the same.
	if reason, msg := policy.CheckObjectSelector(pol, live.GetLabels()); reason != "" {
		return nil, stale(ReasonTargetChanged, "%s", msg)
	}
	field, err := sourcemap.FieldFor(rr.Spec.Action, ref)
	if err != nil {
		return nil, fmt.Errorf("locate the field of the action: %w", err)
	}
	previous, ok := sourcemap.PreviousValue(rr.Spec.Action)
	if !ok {
		return nil, errors.New("the action has no previous value")
	}
	if got, found := field.Get(live.Object); !found || !field.Equals(got, previous) {
		return nil, stale(ReasonTargetChanged, "%s %s/%s no longer holds the previous value of %s on the live object",
			ref.Kind, ref.Namespace, ref.Name, field)
	}
	return live, nil
}

// checkFile reads the target file at the head of the target branch, not of the pull request branch, so a merge
// that changed the value is seen, and checks that it still holds the previous value.
func (c Checker) checkFile(ctx context.Context, rr *v1alpha1.RemediationRequest, repo v1alpha1.GitRepository,
	mapped string) error {
	src := rr.Status.Source
	field, err := sourcemap.FieldFor(rr.Spec.Action, rr.Spec.TargetRef)
	if err != nil {
		return fmt.Errorf("locate the field of the action: %w", err)
	}
	previous, ok := sourcemap.PreviousValue(rr.Spec.Action)
	if !ok {
		return errors.New("the action has no previous value")
	}
	if c.OpenRepository == nil {
		return errors.New("no Git provider is available")
	}
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	git, err := c.OpenRepository(ctx, repo)
	if err != nil {
		return provider("open the repository", err)
	}
	head, err := git.BranchHead(ctx, string(src.TargetBranch))
	if err != nil {
		return provider("read the head of the target branch", err)
	}
	// The mapping and this read must see one commit. If the branch moved since the mapping, the mapped source
	// may describe another tree than the file read here: not fresh, not stale; the caller retries.
	if head != mapped {
		return errors.New("the target branch moved while its source was mapped, checking again")
	}
	file, err := git.ReadFile(ctx, head, string(src.Path))
	switch {
	case errors.Is(err, providers.ErrNotFound):
		return stale(ReasonTargetFileChanged, "%s no longer exists on the target branch", src.Path)
	case err != nil:
		return provider("read the target file on the target branch", err)
	}
	got, found, err := sourcemap.ReadField(file.Data, rr.Spec.TargetRef, field)
	if err != nil {
		return stale(ReasonTargetFileChanged, "%s on the target branch no longer declares the target alone: %v", src.Path, err)
	}
	if !found || !field.Equals(got, previous) {
		return stale(ReasonTargetFileChanged, "%s on the target branch no longer holds the previous value of %s",
			src.Path, field)
	}
	return nil
}
