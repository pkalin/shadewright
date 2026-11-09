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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/policy/freshness"
	"github.com/pkalin/shadewright/internal/providers"
)

// The kill switch (design sections 5.4 and 10.7): RemediationSettings spec.paused stops every Git write.
//
// Every Git provider the executor opens is wrapped in a gatedGit (openRepository, and GatedRepository for the
// source mapping), whose write methods call writeAllowed immediately before they call the host, and pass it on
// as the write checkpoint of the context ([providers.WithWriteCheckpoint]): an adapter calls it again right
// before each write request it sends, so a method that makes several (the GitHub commit: blob, tree, commit,
// branch update) or retries one after a backoff sends no further write once the pause is seen. A request
// already handed to the HTTP client may still complete, and so may one whose check read the settings just
// before the pause was set (there is no lock with the API server); the idempotent writes of the next
// reconcile pick up from there. writeAllowed
// reads the settings from the API server, never from the cache, so a pause set at any moment, also after the
// freshness check passed and between the reads of one step, stops the next write. It fails closed: settings that cannot be read, or that are gone,
// block the write as well. The freshness check, the watch of a recorded pull request and the close of a change
// request also look at the switch first, so a paused executor does not even read Git for a write it may not
// make; the gate is the check that holds whatever the path.
//
// A request held back by the switch keeps its phase and the rest of its status (a stale mark, a recorded pull
// request, the deadline), says so with the condition WritesPaused and an Event, and checks again after
// freshRecheckDelay. When a recheck finds writes allowed again (resumeIfAllowed), the condition is removed with
// another Event at once, whatever the rest of that check finds, and the request goes on from where it stopped;
// a request that ends instead (a final phase) loses the condition without one.

// Reasons and Event reasons of the kill switch.
const (
	// ReasonWritesPaused: RemediationSettings spec.paused holds back the request's next Git write.
	ReasonWritesPaused = "WritesPaused"
	// EventWritesResumed: the request may write to Git again after a pause.
	EventWritesResumed = "WritesResumed"
)

// Fixed texts of the kill switch's condition and Events.
const (
	messageWritesPaused = "RemediationSettings default pauses every write (spec.paused); the request keeps its " +
		"state and goes on when the pause ends"
	messageWritesResumed = "RemediationSettings default no longer pauses writes; the request goes on"
)

// writeAllowed is the kill switch's gate. It returns nil only when the RemediationSettings object, read from
// the API server right now, exists and does not pause writes. A pause is [freshness.ErrPaused]; a settings
// object that cannot be read, or does not exist, is [freshness.ErrWriteBlocked]. In both cases the caller
// must not write. Without an APIReader the gate refuses too: unlike the other reads (reader), it never falls
// back to the client, whose cache may hold a copy from before the pause.
func (r *RemediationRequestReconciler) writeAllowed(ctx context.Context) error {
	if r.APIReader == nil {
		return fmt.Errorf("%w: no API server reader", freshness.ErrWriteBlocked)
	}
	settings := &remediationv1alpha1.RemediationSettings{}
	if err := r.APIReader.Get(ctx, types.NamespacedName{Name: remediationv1alpha1.SettingsName}, settings); err != nil {
		// The wrapped API error goes only to the log and the error backoff. It must never be put into a
		// condition message or an Event: those carry fixed text only.
		return fmt.Errorf("%w: get RemediationSettings %s: %w", freshness.ErrWriteBlocked,
			remediationv1alpha1.SettingsName, err)
	}
	if freshness.Paused(settings) {
		return freshness.ErrPaused
	}
	return nil
}

// openRepository opens a repository through OpenRepository and wraps the provider so that each of its writes
// passes writeAllowed first. Every provider the reconciler writes through comes from here.
func (r *RemediationRequestReconciler) openRepository(ctx context.Context, repo remediationv1alpha1.GitRepository) (
	providers.GitProvider, error) {
	if r.OpenRepository == nil {
		return nil, errors.New("no Git provider")
	}
	git, err := r.OpenRepository(ctx, repo)
	if err != nil {
		return nil, err
	}
	return &gatedGit{git: git, allow: r.writeAllowed}, nil
}

// GatedRepository is openRepository for providers the reconciler does not open itself: cmd/main.go gives it
// to the source mapping, which only reads, so that every provider the executor opens passes the kill switch
// before a write, whoever opened it. Reads are not gated.
func (r *RemediationRequestReconciler) GatedRepository(ctx context.Context, repo remediationv1alpha1.GitRepository) (
	providers.GitProvider, error) {
	return r.openRepository(ctx, repo)
}

// gatedGit is a GitProvider whose writes (CreateBranch, CommitFile, OpenChangeRequest, EnableAutoMerge,
// CommentChangeRequest, CloseChangeRequest) call allow immediately before the wrapped provider, and return its
// error instead of writing when it refuses; the wrapped provider gets allow as its write checkpoint too (gate).
// Reads pass straight through, without a checkpoint. It implements every method itself rather than embedding the
// interface, so a write method added to GitProvider cannot pass ungated: the build fails until it
// is added here.
type gatedGit struct {
	git   providers.GitProvider
	allow func(context.Context) error
}

var _ providers.GitProvider = (*gatedGit)(nil)

func (g *gatedGit) Capabilities() providers.GitCapabilities { return g.git.Capabilities() }

func (g *gatedGit) ReadFile(ctx context.Context, commit, path string, limit int) (providers.File, error) {
	return g.git.ReadFile(ctx, commit, path, limit)
}

func (g *gatedGit) ListDir(ctx context.Context, commit, path string) ([]providers.DirEntry, error) {
	return g.git.ListDir(ctx, commit, path)
}

func (g *gatedGit) BranchHead(ctx context.Context, branch string) (string, error) {
	return g.git.BranchHead(ctx, branch)
}

func (g *gatedGit) CheckBranch(ctx context.Context, commit, path string) (providers.BranchState, error) {
	return g.git.CheckBranch(ctx, commit, path)
}

func (g *gatedGit) CheckMerged(ctx context.Context, head, mergeCommit, path string) (providers.BranchState, string, error) {
	return g.git.CheckMerged(ctx, head, mergeCommit, path)
}

func (g *gatedGit) FindChangeRequest(ctx context.Context, headBranch string) (providers.ChangeRequest, error) {
	return g.git.FindChangeRequest(ctx, headBranch)
}

// IsAncestor is a read and passes straight through, when the wrapped provider can answer it
// ([providers.AncestryChecker]); otherwise it returns errNoAncestry. The check after the merge uses it.
func (g *gatedGit) IsAncestor(ctx context.Context, ancestor, commit string) (bool, error) {
	checker, ok := g.git.(providers.AncestryChecker)
	if !ok {
		return false, errNoAncestry
	}
	return checker.IsAncestor(ctx, ancestor, commit)
}

var _ providers.AncestryChecker = (*gatedGit)(nil)

// feedback returns the wrapped provider as a [providers.FeedbackSource] when it is one. Its methods only read,
// so they pass straight through, without a checkpoint.
func (g *gatedGit) feedback() (providers.FeedbackSource, bool) {
	fs, ok := g.git.(providers.FeedbackSource)
	return fs, ok
}

// gate calls allow before a write method and returns the context to call the wrapped provider with: it
// carries allow as the write checkpoint ([providers.WithWriteCheckpoint]), so an adapter that makes several
// requests in one method, or retries one, asks the gate again right before each of them. The adapter gets
// only the class of a refusal (a pause, or settings that cannot be read), never the API server's error text.
func (g *gatedGit) gate(ctx context.Context) (context.Context, error) {
	if err := g.allow(ctx); err != nil {
		return ctx, err
	}
	return providers.WithWriteCheckpoint(ctx, g.allow, freshness.ErrPaused, freshness.ErrWriteBlocked), nil
}

func (g *gatedGit) CreateBranch(ctx context.Context, branch, fromCommit string) error {
	ctx, err := g.gate(ctx)
	if err != nil {
		return err
	}
	return g.git.CreateBranch(ctx, branch, fromCommit)
}

func (g *gatedGit) CommitFile(ctx context.Context, branch, expectedHead string, change providers.FileChange) (string, error) {
	ctx, err := g.gate(ctx)
	if err != nil {
		return "", err
	}
	return g.git.CommitFile(ctx, branch, expectedHead, change)
}

func (g *gatedGit) OpenChangeRequest(ctx context.Context, req providers.NewChangeRequest) (providers.ChangeRequest, error) {
	ctx, err := g.gate(ctx)
	if err != nil {
		return providers.ChangeRequest{}, err
	}
	return g.git.OpenChangeRequest(ctx, req)
}

func (g *gatedGit) EnableAutoMerge(ctx context.Context, number int64, expectedHeadSHA string) error {
	ctx, err := g.gate(ctx)
	if err != nil {
		return err
	}
	return g.git.EnableAutoMerge(ctx, number, expectedHeadSHA)
}

func (g *gatedGit) CommentChangeRequest(ctx context.Context, number int64, marker, body string) error {
	ctx, err := g.gate(ctx)
	if err != nil {
		return err
	}
	return g.git.CommentChangeRequest(ctx, number, marker, body)
}

func (g *gatedGit) CloseChangeRequest(ctx context.Context, number int64) error {
	ctx, err := g.gate(ctx)
	if err != nil {
		return err
	}
	return g.git.CloseChangeRequest(ctx, number)
}

// waitWhilePaused records that the kill switch holds the request back and checks again after
// freshRecheckDelay. It sets WritesPaused True, and on the change also records an Event; the phase and the rest
// of the status are kept, so the request goes on from where it stopped once the pause ends.
func (r *RemediationRequestReconciler) waitWhilePaused(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) (
	reconcile.Result, error) {
	if meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{
		Type:               remediationv1alpha1.ConditionWritesPaused,
		Status:             metav1.ConditionTrue,
		Reason:             ReasonWritesPaused,
		Message:            messageWritesPaused,
		ObservedGeneration: rr.Generation,
	}) {
		if err := r.Status().Update(ctx, rr); err != nil {
			return reconcile.Result{}, fmt.Errorf("update status of RemediationRequest %s/%s: %w", rr.Namespace, rr.Name, err)
		}
		r.event(rr, ReasonWritesPaused, messageWritesPaused)
		logf.FromContext(ctx).Info("Paused Git writes of RemediationRequest", "remediationRequest",
			client.ObjectKeyFromObject(rr))
	}
	return reconcile.Result{RequeueAfter: freshRecheckDelay}, nil
}

// resume removes WritesPaused from the request's conditions, in memory, and reports whether it was there. The
// caller writes the status and then, when the request goes on rather than ends, calls resumed.
func resume(rr *remediationv1alpha1.RemediationRequest) bool {
	if meta.FindStatusCondition(rr.Status.Conditions, remediationv1alpha1.ConditionWritesPaused) == nil {
		return false
	}
	meta.RemoveStatusCondition(&rr.Status.Conditions, remediationv1alpha1.ConditionWritesPaused)
	return true
}

// resumeIfAllowed removes WritesPaused, records the WritesResumed Event and returns nil once writeAllowed, read
// right now, finds writes allowed. A request without the condition is left alone, without a read. While the
// gate still refuses (a pause, or settings that cannot be read) the condition stays, and nil is returned: the
// caller's own path decides what happens next. Only the error of the status write is returned.
func (r *RemediationRequestReconciler) resumeIfAllowed(ctx context.Context,
	rr *remediationv1alpha1.RemediationRequest) error {
	if meta.FindStatusCondition(rr.Status.Conditions, remediationv1alpha1.ConditionWritesPaused) == nil {
		return nil
	}
	readCtx, cancel := bound(ctx, r.WriteTimeout, DefaultWriteTimeout)
	allowed := r.writeAllowed(readCtx)
	cancel()
	if allowed != nil || ctx.Err() != nil {
		return nil
	}
	resume(rr)
	if err := r.Status().Update(ctx, rr); err != nil {
		return fmt.Errorf("update status of RemediationRequest %s/%s: %w", rr.Namespace, rr.Name, err)
	}
	r.resumed(ctx, rr)
	return nil
}

// resumed records the Event of a request that may write again, once the status without WritesPaused is written.
func (r *RemediationRequestReconciler) resumed(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) {
	r.event(rr, EventWritesResumed, messageWritesResumed)
	logf.FromContext(ctx).Info("Resumed Git writes of RemediationRequest", "remediationRequest",
		client.ObjectKeyFromObject(rr))
}

// event records a Normal Event about the request, with a fixed message. Without a Recorder it records none.
func (r *RemediationRequestReconciler) event(rr *remediationv1alpha1.RemediationRequest, reason, message string) {
	if r.Recorder != nil {
		r.Recorder.Event(rr, corev1.EventTypeNormal, reason, message)
	}
}
