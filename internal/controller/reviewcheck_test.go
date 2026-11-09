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
	"fmt"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	api "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/providers"
	fakegit "github.com/pkalin/shadewright/internal/providers/fake"
	fx "github.com/pkalin/shadewright/internal/testfixtures"
)

const (
	// opCreateCheckRun is the fake Git's name of CreateCheckRun, for SetFail.
	opCreateCheckRun = "CreateCheckRun"
	// oldName and newName name gateWorld's predecessor and replacement, and their commits.
	oldName = "old"
	newName = "new"
)

// gateWorld is a predecessor waiting for review with hint 7 Reserved for its replacement, the replacement's
// commit pushed on top of the predecessor's (handoff step 3 done), nothing recorded yet. It returns the
// reconciler, the fake Git, both requests as stored, the pull request and the new head.
func gateWorld(t *testing.T) (*RemediationRequestReconciler, *fakegit.Git, *api.RemediationRequest, *api.RemediationRequest,
	providers.ChangeRequest, string) {
	t.Helper()
	ctx := context.Background()
	p := fx.MemoryLimitRequest("ns", oldName)
	p.UID = "old-uid"
	p.Status.PlanDigest = "sha256:" + strings.Repeat("a", 64)
	p.Status.Phase = api.PhaseAwaitingApproval
	p.Status.Source = &api.ResolvedSource{Path: sourcePath, TargetBranch: mainBranch}
	s := fx.RemediationSettings()
	s.Spec.Repositories[0].TargetBranch = mainBranch
	p.Status.PlanRepository = &api.PlanRepository{URL: s.Spec.Repositories[0].URL, InstallationID: 1}
	h := api.Hint{Source: api.HintSourceGitHub, CommentID: 7, Digest: "sha256:" + strings.Repeat("b", 64),
		State: api.HintReserved, Author: api.HintAuthor{ID: 1001, Login: "first"}}
	p.Status.Hints = []api.Hint{h}
	rr := p.DeepCopy()
	rr.Name, rr.UID = newName, "new-uid"
	rr.Spec.ChainRoot = api.UID(p.UID)
	rr.Spec.Replaces = &api.RequestReplacement{Name: api.Name(p.Name), UID: api.UID(p.UID), Hint: refOf(&h)}
	rr.Spec.Action.SetContainerMemoryLimit.Desired = resource.MustParse("512Mi")
	rr.Status.Phase, rr.Status.Hints = api.PhasePolicyChecked, nil
	rr.Status.Handoff = &api.Handoff{From: &api.IncomingHandoff{Name: api.Name(p.Name), UID: api.UID(p.UID), Hint: refOf(&h), Phase: api.HandoffReserved}}
	g := fakegit.NewGit(mainBranch, map[string][]byte{sourcePath: deploymentManifest(p.Spec.TargetRef.Name, p.Spec.TargetRef.Namespace, workerContainer, "256Mi")})
	base, err := g.BranchHead(ctx, mainBranch)
	if err != nil {
		t.Fatal(err)
	}
	branch := "shadewright/" + string(p.UID)
	if err := g.CreateBranch(ctx, branch, base); err != nil {
		t.Fatal(err)
	}
	old, err := g.CommitFile(ctx, branch, base, providers.FileChange{Path: sourcePath, Content: deploymentManifest(p.Spec.TargetRef.Name, p.Spec.TargetRef.Namespace, workerContainer, "384Mi"), Message: oldName})
	if err != nil {
		t.Fatal(err)
	}
	cr, err := g.OpenChangeRequest(ctx, providers.NewChangeRequest{HeadBranch: branch, BaseBranch: mainBranch, Title: oldName, Body: marker(p)})
	if err != nil {
		t.Fatal(err)
	}
	p.Status.PullRequest = &api.PullRequestStatus{Number: cr.Number, URL: cr.URL, HeadSHA: old}
	p.Status.Handoff = &api.Handoff{To: &api.OutgoingHandoff{Name: api.Name(rr.Name), UID: api.UID(rr.UID), Hint: refOf(&h), ExpectedHead: old, Phase: api.HandoffIntent}}
	content, ok, err := handoffContent(ctx, g, rr, p, old)
	if err != nil || !ok {
		t.Fatalf("handoff content: %v, %v", ok, err)
	}
	head, err := g.AppendCommit(ctx, branch, old, providers.FileChange{Path: sourcePath, Content: content, Message: newName})
	if err != nil {
		t.Fatal(err)
	}
	cr, err = g.FindChangeRequest(ctx, branch)
	if err != nil {
		t.Fatal(err)
	}
	c := clientfake.NewClientBuilder().WithScheme(deadlineScheme(t)).WithStatusSubresource(p, rr).WithObjects(s, p, rr).Build()
	if err := c.Get(ctx, client.ObjectKeyFromObject(p), p); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(rr), rr); err != nil {
		t.Fatal(err)
	}
	r := &RemediationRequestReconciler{Client: c, APIReader: c, ReviewerHints: true,
		OpenRepository: func(context.Context, api.GitRepository) (providers.GitProvider, error) { return g, nil }}
	return r, g, p, rr, cr, head
}

// plainWaiting turns gateWorld's predecessor into a request no handoff touched, waiting for review with the
// branch's head recorded and no hints.
func plainWaiting(t *testing.T, r *RemediationRequestReconciler, p *api.RemediationRequest, head string) {
	t.Helper()
	p.Status.Handoff, p.Status.Hints = nil, nil
	p.Status.PullRequest.HeadSHA = head
	if err := r.Status().Update(context.Background(), p); err != nil {
		t.Fatal(err)
	}
}

func approve(g *fakegit.Git, number, id, author int64, head string) {
	g.AddReview(number, providers.Feedback{ID: id, Author: providers.FeedbackAuthor{ID: author,
		Login: fmt.Sprintf("reviewer-%d", author), Kind: providers.AuthorPerson}, ReviewState: reviewApproved,
		CommitID: head, CreatedAt: time.Now()})
}

func TestReviewCheckOnAHeadNoHandoffPushed(t *testing.T) {
	r, g, p, _, cr, head := gateWorld(t)
	ctx := context.Background()
	plainWaiting(t, r, p, head)
	if _, err := r.watchPullRequest(ctx, p); err != nil {
		t.Fatal(err)
	}
	want := api.ReviewCheck{Head: head, State: api.ReviewCheckInProgress}
	if rc := p.Status.PullRequest.ReviewCheck; rc == nil || *rc != want {
		t.Fatalf("the first pull request's head got no check in progress: %+v", rc)
	}
	if runs := g.CheckRuns(head); len(runs) != 1 || runs[0].Status != providers.CheckRunInProgress {
		t.Fatalf("check runs on the head: %+v", runs)
	}
	approve(g, cr.Number, 90, 3003, head)
	if _, err := r.watchPullRequest(ctx, p); err != nil {
		t.Fatal(err)
	}
	if rc := p.Status.PullRequest.ReviewCheck; rc == nil || rc.State != api.ReviewCheckPassed {
		t.Fatalf("an approval by someone who wrote no hint did not pass the gate: %+v", rc)
	}
	if runs := g.CheckRuns(head); len(runs) != 2 || runs[1].Status != providers.CheckRunSuccess {
		t.Fatalf("check runs on the head: %+v", runs)
	}
}

func TestReviewCheckOfAnotherHeadDoesNotCount(t *testing.T) {
	r, g, p, _, _, head := gateWorld(t)
	ctx := context.Background()
	plainWaiting(t, r, p, head)
	p.Status.PullRequest.ReviewCheck = &api.ReviewCheck{Head: strings.Repeat("c", 40), State: api.ReviewCheckPassed}
	if err := r.Status().Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	if _, err := r.watchPullRequest(ctx, p); err != nil {
		t.Fatal(err)
	}
	want := api.ReviewCheck{Head: head, State: api.ReviewCheckInProgress}
	if rc := p.Status.PullRequest.ReviewCheck; rc == nil || *rc != want {
		t.Fatalf("a check that passed on another head survived on this one: %+v", rc)
	}
	if runs := g.CheckRuns(head); len(runs) != 1 || runs[0].Status != providers.CheckRunInProgress {
		t.Fatalf("check runs on the head: %+v", runs)
	}
}

func TestReviewCheckRefusedSaysWhy(t *testing.T) {
	r, g, p, _, _, head := gateWorld(t)
	ctx := context.Background()
	events := record.NewFakeRecorder(10)
	r.Recorder = events
	plainWaiting(t, r, p, head)
	g.SetFail(func(op string) error {
		if op == opCreateCheckRun {
			return fmt.Errorf("status 403: %w", providers.ErrForbidden)
		}
		return nil
	})
	if _, err := r.watchPullRequest(ctx, p); err != nil {
		t.Fatal(err)
	}
	stored := &api.RemediationRequest{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(p), stored); err != nil {
		t.Fatal(err)
	}
	if rc := stored.Status.PullRequest.ReviewCheck; rc == nil || rc.State != api.ReviewCheckRefused {
		t.Fatalf("refused check not recorded: %+v", rc)
	}
	c := meta.FindStatusCondition(stored.Status.Conditions, api.ConditionApproved)
	if c == nil || c.Reason != ReasonReviewCheckRefused {
		t.Fatalf("condition Approved does not say why the pull request is blocked: %+v", c)
	}
	select {
	case e := <-events.Events:
		if !strings.Contains(e, "Warning "+ReasonReviewCheckRefused) {
			t.Fatalf("unexpected Event: %s", e)
		}
	default:
		t.Fatal("no Event says why the pull request is blocked")
	}
}

func TestHandoffKeepsDeclinedHintAuthorsOutOfTheGate(t *testing.T) {
	r, g, p, rr, cr, head := gateWorld(t)
	ctx := context.Background()
	declined := p.Status.Hints[0]
	declined.CommentID, declined.Author.ID, declined.State, declined.Reason = 8, 2002, api.HintDeclined, "NoChange"
	p.Status.Hints = append(p.Status.Hints, declined)
	if err := r.recordHandoff(ctx, rr, p, cr, head); err != nil {
		t.Fatal(err)
	}
	rr.Status.Phase = api.PhaseAwaitingApproval
	rr.Status.PullRequest.ReviewCheck = &api.ReviewCheck{Head: head, State: api.ReviewCheckInProgress}
	if err := r.Status().Update(ctx, rr); err != nil {
		t.Fatal(err)
	}
	approve(g, cr.Number, 90, 2002, head)
	if err := r.reviewGate(ctx, &gatedGit{git: g, allow: r.writeAllowed}, rr); err != nil {
		t.Fatal(err)
	}
	if rr.Status.PullRequest.ReviewCheck.State == api.ReviewCheckPassed {
		t.Fatal("a declined hint's author became an independent approver after the handoff dropped their hint")
	}
}

func TestReviewGateCollectsHintsBeforeItDecides(t *testing.T) {
	r, g, p, rr, cr, head := gateWorld(t)
	ctx := context.Background()
	if err := r.recordHandoff(ctx, rr, p, cr, head); err != nil {
		t.Fatal(err)
	}
	rr.Status.Phase = api.PhaseAwaitingApproval
	rr.Status.Handoff.From.PullRequestUpdate = api.HandoffStepDone
	rr.Status.PullRequest.ReviewCheck = &api.ReviewCheck{Head: head, State: api.ReviewCheckInProgress}
	if err := r.Status().Update(ctx, rr); err != nil {
		t.Fatal(err)
	}
	if err := g.UpdateChangeRequest(ctx, cr.Number, newName, marker(rr)); err != nil {
		t.Fatal(err)
	}
	p.Status.Handoff.To.Phase = api.HandoffDone
	if err := r.Status().Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	author := providers.FeedbackAuthor{ID: 2002, Login: "second", Kind: providers.AuthorPerson}
	g.SetWriter(author.ID, true)
	g.AddComment(cr.Number, providers.Feedback{ID: 8, Author: author, Body: hintCommand + " inspect the sidecar", CreatedAt: now})
	g.AddReview(cr.Number, providers.Feedback{ID: 90, Author: author, ReviewState: reviewApproved, CommitID: head, CreatedAt: now.Add(time.Second)})
	// Reconcile runs afterHandoff before the look at the pull request collects the comments.
	if err := r.afterHandoff(ctx, rr); err != nil {
		t.Fatal(err)
	}
	if _, err := r.watchPullRequest(ctx, rr); err != nil {
		t.Fatal(err)
	}
	if len(rr.Status.Hints) != 2 {
		t.Fatalf("new hint was not collected: %+v", rr.Status.Hints)
	}
	if rr.Status.PullRequest.ReviewCheck.State == api.ReviewCheckPassed {
		t.Fatal("the gate passed on an approval whose author's hint was on the pull request before the look")
	}
}

func TestStalePredecessorKeepsTheTransferredPullRequestOpen(t *testing.T) {
	r, g, p, rr, cr, head := gateWorld(t)
	ctx := context.Background()
	// This predecessor's reconcile read its status before the replacement reserved the hint, and was suspended
	// after deciding that its waiting plan failed the render check.
	stale := p.DeepCopy()
	stale.Status.Handoff = nil
	stale.Status.Hints[0].State = api.HintPending
	if err := r.recordHandoff(ctx, rr, p, cr, head); err != nil {
		t.Fatal(err)
	}
	if err := r.supersedePredecessor(ctx, rr); err != nil {
		t.Fatal(err)
	}
	// It resumes before the replacement rewrites the body (also possible after a refused rewrite).
	err := r.refuseWaiting(ctx, stale, &renderRefusal{reason: ReasonTargetFileDiverged, message: "render refused"})
	got, findErr := g.FindChangeRequest(ctx, "shadewright/"+string(p.UID))
	if findErr != nil {
		t.Fatal(findErr)
	}
	if got.State != providers.ChangeRequestOpen {
		t.Fatalf("a stale predecessor closed the transferred pull request: state=%s, error=%v", got.State, err)
	}
}
