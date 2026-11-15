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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	opReviewCheckRun = "ReviewCheckRun"
	// opListReviews is the fake Git's name of ListReviews, for SetFail.
	opListReviews = "ListReviews"
	// opUpdateChangeRequest is the fake's name of UpdateChangeRequest, for SetFail.
	opUpdateChangeRequest = "UpdateChangeRequest"
	// oldName and newName name gateWorld's predecessor and replacement, and their commits.
	oldName = "old"
	newName = "new"
)

// The clocks of the worlds the hint, reply and handoff tests build (gateWorld, replyWorld,
// handoffAppendedWorld): reconcilerClock is the reconciler's (Now) and hostClock the fake code host's (the
// feedback clock and the times of its comments and reviews). Every fixture time is built from one of them, never
// from time.Now, so no test depends on the date it runs on. They differ on purpose: the reconciler never decides
// from a code host's time, and moving either one to another date, far in the past or the future, leaves every
// result the same.
var (
	reconcilerClock = time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	hostClock       = reconcilerClock.Add(-37 * time.Minute)
)

// fixClocks sets the clocks of a world: r's to reconcilerClock and g's to hostClock.
func fixClocks(r *RemediationRequestReconciler, g *fakegit.Git) {
	r.Now = func() time.Time { return reconcilerClock }
	g.SetFeedbackClock(func() time.Time { return hostClock })
}

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
	fixClocks(r, g)
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
		CommitID: head, CreatedAt: hostClock})
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
	now := hostClock
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

// passedWorld is a plain request waiting for review whose check run passed on its head through the approval
// (review 90) of reviewer 3003, who wrote no hint, while user 2002 wrote a hint that was declined. It returns
// the reconciler, the fake Git, the request as stored, the pull request number and the head.
func passedWorld(t *testing.T) (*RemediationRequestReconciler, *fakegit.Git, *api.RemediationRequest, int64, string) {
	t.Helper()
	r, g, p, _, cr, head := gateWorld(t)
	ctx := context.Background()
	plainWaiting(t, r, p, head)
	p.Status.Hints = []api.Hint{{Source: api.HintSourceGitHub, CommentID: 8, Digest: "sha256:" + strings.Repeat("b", 64),
		State: api.HintDeclined, Reason: "NoChange", Author: api.HintAuthor{ID: 2002, Login: "steerer"}}}
	if err := r.Status().Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	approve(g, cr.Number, 90, 3003, head)
	for range 2 {
		if err := r.reviewCheck(ctx, p, true); err != nil {
			t.Fatal(err)
		}
	}
	if rc := p.Status.PullRequest.ReviewCheck; rc == nil || rc.State != api.ReviewCheckPassed {
		t.Fatalf("the check did not pass: %+v", rc)
	}
	return r, g, p, cr.Number, head
}

// lastCheckRun returns the status of the latest check run on head, or "" when there is none.
func lastCheckRun(g *fakegit.Git, head string) providers.CheckRunStatus {
	runs := g.CheckRuns(head)
	if len(runs) == 0 {
		return ""
	}
	return runs[len(runs)-1].Status
}

// A check that passed is evaluated again on each look while the request waits for approval: it stays passed while
// the approval it rested on stands, and once that approval is gone, an approval by a hint author does not keep it
// passed (#748). It goes back to in progress on
// the Git host and in status, and passes again when someone who wrote no hint approves the head again.
func TestReviewCheckGoesBackWhenTheApprovalIsGone(t *testing.T) {
	r, g, p, number, head := passedWorld(t)
	ctx := context.Background()
	// While the approval stands, a look writes nothing: no check run, no status.
	approve(g, number, 91, 2002, head)
	runs, rv := len(g.CheckRuns(head)), p.ResourceVersion
	if err := r.reviewCheck(ctx, p, true); err != nil {
		t.Fatal(err)
	}
	if p.Status.PullRequest.ReviewCheck.State != api.ReviewCheckPassed || p.ResourceVersion != rv ||
		len(g.CheckRuns(head)) != runs {
		t.Fatalf("a standing approval changed the check: %+v, %+v", p.Status.PullRequest.ReviewCheck, g.CheckRuns(head))
	}
	g.SetReviewState(number, 90, "DISMISSED")
	if err := r.reviewCheck(ctx, p, true); err != nil {
		t.Fatal(err)
	}
	want := api.ReviewCheck{Head: head, State: api.ReviewCheckInProgress}
	if rc := p.Status.PullRequest.ReviewCheck; rc == nil || *rc != want {
		t.Fatalf("the check stayed passed without the approval it rested on: %+v", rc)
	}
	if got := lastCheckRun(g, head); got != providers.CheckRunInProgress {
		t.Fatalf("the check run on the Git host is %q, want in progress", got)
	}
	approve(g, number, 92, 3003, head)
	if err := r.reviewCheck(ctx, p, true); err != nil {
		t.Fatal(err)
	}
	if rc := p.Status.PullRequest.ReviewCheck; rc.State != api.ReviewCheckPassed {
		t.Fatalf("a new approval by someone who wrote no hint did not pass the check again: %+v", rc)
	}
	if got := lastCheckRun(g, head); got != providers.CheckRunSuccess {
		t.Fatalf("the check run on the Git host is %q, want success", got)
	}
}

// A failed read of the reviews of a passed check is returned for a retry on a later look and changes nothing:
// the check is neither passed again nor put back on a read that did not happen.
func TestReviewCheckPassedKeepsItsStateOnAFailedRead(t *testing.T) {
	r, g, p, number, head := passedWorld(t)
	ctx := context.Background()
	g.SetReviewState(number, 90, "DISMISSED")
	g.SetFail(func(op string) error {
		if op == opListReviews {
			return fmt.Errorf("status 502: %w", providers.ErrRetryable)
		}
		return nil
	})
	runs := len(g.CheckRuns(head))
	if err := r.reviewCheck(ctx, p, true); err == nil {
		t.Fatal("a failed read of the reviews was not returned for a retry")
	}
	if p.Status.PullRequest.ReviewCheck.State != api.ReviewCheckPassed || len(g.CheckRuns(head)) != runs {
		t.Fatalf("a failed read changed the check: %+v, %+v", p.Status.PullRequest.ReviewCheck, g.CheckRuns(head))
	}
	g.SetFail(nil)
	if err := r.reviewCheck(ctx, p, true); err != nil {
		t.Fatal(err)
	}
	if p.Status.PullRequest.ReviewCheck.State != api.ReviewCheckInProgress {
		t.Fatalf("the retry did not put the check back: %+v", p.Status.PullRequest.ReviewCheck)
	}
}

// A list of reviews too long to read on a passed check never keeps the pass: the check goes back to in progress
// and then fails on that head, as it does on a check that never passed.
func TestReviewCheckPassedFailsOnAListTooLong(t *testing.T) {
	r, g, p, _, head := passedWorld(t)
	ctx := context.Background()
	r.OpenRepository = func(context.Context, api.GitRepository) (providers.GitProvider, error) {
		return pagedReviews{g}, nil
	}
	if err := r.reviewCheck(ctx, p, true); err != nil {
		t.Fatal(err)
	}
	if rc := p.Status.PullRequest.ReviewCheck; rc.State != api.ReviewCheckFailed || rc.Head != head {
		t.Fatalf("review check %+v", rc)
	}
	if got := lastCheckRun(g, head); got != providers.CheckRunFailure {
		t.Fatalf("the check run on the Git host is %q, want failure", got)
	}
	if c := meta.FindStatusCondition(p.Status.Conditions, api.ConditionApproved); c == nil || c.Reason != ReasonReviewListTooLong {
		t.Fatalf("Approved: %+v", c)
	}
}

// staleCopy makes p's next status write fail with a conflict, as when another writer changed the request in
// between: it writes the stored request once more, so p's resourceVersion is old. It returns a function that reads
// the request again into p.
func staleCopy(t *testing.T, r *RemediationRequestReconciler, p *api.RemediationRequest) func() {
	t.Helper()
	ctx := context.Background()
	live := &api.RemediationRequest{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(p), live); err != nil {
		t.Fatal(err)
	}
	if live.Annotations == nil {
		live.Annotations = map[string]string{}
	}
	live.Annotations["test.shadewright.io/touched"] = "yes"
	if err := r.Update(ctx, live); err != nil {
		t.Fatal(err)
	}
	return func() {
		t.Helper()
		if err := r.Get(ctx, client.ObjectKeyFromObject(p), p); err != nil {
			t.Fatal(err)
		}
	}
}

// The check is level-based on what the Git host shows (#748): the host was put back in progress after the
// approval was dismissed, the status write after it failed, so status still says Passed; then the approval came
// back. The next look writes success to the host again, though status already says Passed.
func TestReviewCheckPassesAgainAfterALostStatusWrite(t *testing.T) {
	r, g, p, number, head := passedWorld(t)
	ctx := context.Background()
	g.SetReviewState(number, 90, "DISMISSED")
	reread := staleCopy(t, r, p)
	if err := r.reviewCheck(ctx, p, true); err == nil {
		t.Fatal("a status write that failed was not returned")
	}
	reread()
	if got := lastCheckRun(g, head); got != providers.CheckRunInProgress || p.Status.PullRequest.ReviewCheck.State != api.ReviewCheckPassed {
		t.Fatalf("setup: host %q, status %+v", got, p.Status.PullRequest.ReviewCheck)
	}
	approve(g, number, 92, 3003, head)
	if err := r.reviewCheck(ctx, p, true); err != nil {
		t.Fatal(err)
	}
	if got := lastCheckRun(g, head); got != providers.CheckRunSuccess {
		t.Fatalf("the Git host still shows %q on a head with an approval that counts", got)
	}
	if rc := p.Status.PullRequest.ReviewCheck; rc.State != api.ReviewCheckPassed {
		t.Fatalf("review check %+v", rc)
	}
}

// The other direction: the host accepted success, recording Passed failed, so status says InProgress; then every
// approval was dismissed. The next look puts the host back in progress, though status already says InProgress.
func TestReviewCheckGoesBackAfterALostPassWrite(t *testing.T) {
	r, g, p, _, cr, head := gateWorld(t)
	ctx := context.Background()
	plainWaiting(t, r, p, head)
	if err := r.reviewCheck(ctx, p, true); err != nil {
		t.Fatal(err)
	}
	approve(g, cr.Number, 90, 3003, head)
	reread := staleCopy(t, r, p)
	if err := r.reviewCheck(ctx, p, true); err == nil {
		t.Fatal("a status write that failed was not returned")
	}
	reread()
	if got := lastCheckRun(g, head); got != providers.CheckRunSuccess || p.Status.PullRequest.ReviewCheck.State != api.ReviewCheckInProgress {
		t.Fatalf("setup: host %q, status %+v", got, p.Status.PullRequest.ReviewCheck)
	}
	g.SetReviewState(cr.Number, 90, "DISMISSED")
	if err := r.reviewCheck(ctx, p, true); err != nil {
		t.Fatal(err)
	}
	if got := lastCheckRun(g, head); got != providers.CheckRunInProgress {
		t.Fatalf("the Git host still shows %q with no approval standing", got)
	}
	if rc := p.Status.PullRequest.ReviewCheck; rc.State != api.ReviewCheckInProgress {
		t.Fatalf("review check %+v", rc)
	}
}

// A look that finds the host already showing what the reviews call for only records it: no second write.
func TestReviewCheckRecordsWhatTheHostAlreadyShows(t *testing.T) {
	r, g, p, number, head := passedWorld(t)
	ctx := context.Background()
	g.SetReviewState(number, 90, "DISMISSED")
	reread := staleCopy(t, r, p)
	if err := r.reviewCheck(ctx, p, true); err == nil {
		t.Fatal("a status write that failed was not returned")
	}
	reread()
	runs := len(g.CheckRuns(head))
	if err := r.reviewCheck(ctx, p, true); err != nil {
		t.Fatal(err)
	}
	if rc := p.Status.PullRequest.ReviewCheck; rc.State != api.ReviewCheckInProgress || len(g.CheckRuns(head)) != runs {
		t.Fatalf("review check %+v, runs %d, want %d", rc, len(g.CheckRuns(head)), runs)
	}
}

// A review that only comments (a reply in a review thread) after the approval does not take it back, as on
// GitHub, whose count of approvals skips it.
func TestReviewCheckCommentAfterThePassKeepsIt(t *testing.T) {
	r, g, p, number, head := passedWorld(t)
	ctx := context.Background()
	g.AddReview(number, providers.Feedback{ID: 93, Author: providers.FeedbackAuthor{ID: 3003, Login: "reviewer-3003",
		Kind: providers.AuthorPerson}, ReviewState: reviewCommented, CommitID: head, CreatedAt: hostClock.Add(time.Minute)})
	runs := len(g.CheckRuns(head))
	if err := r.reviewCheck(ctx, p, true); err != nil {
		t.Fatal(err)
	}
	if rc := p.Status.PullRequest.ReviewCheck; rc.State != api.ReviewCheckPassed || len(g.CheckRuns(head)) != runs {
		t.Fatalf("a comment after the approval changed the check: %+v, %+v", rc, g.CheckRuns(head))
	}
}

// Writes paused after the look began, between the read of the Git host and the write it calls for, stop that
// write: the kill switch is asked right before the host call, and nothing changes.
func TestReviewCheckWritesNothingWhilePaused(t *testing.T) {
	r, g, p, number, head := passedWorld(t)
	ctx := context.Background()
	g.SetReviewState(number, 90, "DISMISSED")
	g.SetFail(func(op string) error {
		if op != opReviewCheckRun {
			return nil
		}
		s := &api.RemediationSettings{}
		if err := r.Get(ctx, client.ObjectKey{Name: api.SettingsName}, s); err != nil {
			return err
		}
		s.Spec.Paused = new(true)
		return r.Update(ctx, s)
	})
	runs := len(g.CheckRuns(head))
	if err := r.reviewCheck(ctx, p, true); err == nil {
		t.Fatal("a paused look was not returned for a retry")
	}
	if len(g.CheckRuns(head)) != runs || p.Status.PullRequest.ReviewCheck.State != api.ReviewCheckPassed {
		t.Fatalf("a paused look changed the check: %+v, %+v", p.Status.PullRequest.ReviewCheck, g.CheckRuns(head))
	}
}

// A final refusal to write the check run is recorded Refused with the time and count of refusals; condition
// Approved and one Warning Event per refusal say what the Git host shows. The host is not asked again before the
// backoff from the recorded refusal runs out, and once host and reviews agree again the refusal is cleared
// without a write.
func TestReviewCheckRefusedToGoBack(t *testing.T) {
	r, g, p, number, head := passedWorld(t)
	ctx := context.Background()
	events := record.NewFakeRecorder(10)
	r.Recorder = events
	now := reconcilerClock
	r.Now = func() time.Time { return now }
	g.SetReviewState(number, 90, "DISMISSED")
	asks := 0
	g.SetFail(func(op string) error {
		if op == opCreateCheckRun {
			asks++
			return fmt.Errorf("status 403: %w", providers.ErrForbidden)
		}
		return nil
	})
	look := func() {
		t.Helper()
		if err := r.reviewCheck(ctx, p, true); err != nil {
			t.Fatal(err)
		}
	}
	look()
	rc := p.Status.PullRequest.ReviewCheck
	if rc.State != api.ReviewCheckRefused || rc.Refusals != 1 || rc.RefusedAt == nil || !rc.RefusedAt.Time.Equal(now) {
		t.Fatalf("review check %+v", rc)
	}
	c := meta.FindStatusCondition(p.Status.Conditions, api.ConditionApproved)
	if c == nil || c.Reason != ReasonReviewCheckRefused || !strings.Contains(c.Message, "shows the check run shadewright/review as success") {
		t.Fatalf("condition Approved does not say what the host shows: %+v", c)
	}
	look()
	if asks != 1 || len(events.Events) != 1 {
		t.Fatalf("asked the Git host %d times, %d Events, before the backoff ran out", asks, len(events.Events))
	}
	<-events.Events
	now = now.Add(refusalRetryBase + time.Second)
	look()
	if rc := p.Status.PullRequest.ReviewCheck; asks != 2 || rc.Refusals != 2 || len(events.Events) != 1 {
		t.Fatalf("after the backoff: asks %d, review check %+v, %d Events", asks, rc, len(events.Events))
	}
	if ev := <-events.Events; !strings.Contains(ev, "Warning "+ReasonReviewCheckRefused) {
		t.Fatalf("event %q", ev)
	}
	// The approval comes back: the host's success is what the reviews call for, so the refusal ends unwritten.
	approve(g, number, 92, 3003, head)
	look()
	if rc := p.Status.PullRequest.ReviewCheck; asks != 2 || rc.State != api.ReviewCheckPassed || rc.Refusals != 0 || rc.RefusedAt != nil {
		t.Fatalf("asks %d, review check %+v", asks, rc)
	}
	if c := meta.FindStatusCondition(p.Status.Conditions, api.ConditionApproved); c.Reason != ReasonWaitingForReview {
		t.Fatalf("Approved still says the Git host refused: %+v", c)
	}
}

// A refusal recorded when the check is created on a head is asked again after the backoff, so a fixed App
// permission heals without a new head.
func TestReviewCheckRefusedAtCreateHeals(t *testing.T) {
	r, g, p, _, _, head := gateWorld(t)
	ctx := context.Background()
	plainWaiting(t, r, p, head)
	now := reconcilerClock
	r.Now = func() time.Time { return now }
	g.SetFail(func(op string) error {
		if op == opCreateCheckRun {
			return fmt.Errorf("status 403: %w", providers.ErrForbidden)
		}
		return nil
	})
	if err := r.reviewCheck(ctx, p, true); err != nil {
		t.Fatal(err)
	}
	c := meta.FindStatusCondition(p.Status.Conditions, api.ConditionApproved)
	if rc := p.Status.PullRequest.ReviewCheck; rc.State != api.ReviewCheckRefused || c == nil ||
		!strings.Contains(c.Message, "no check run shadewright/review from Shadewright") {
		t.Fatalf("review check %+v, Approved %+v", rc, c)
	}
	g.SetFail(nil)
	now = now.Add(refusalRetryBase + time.Second)
	if err := r.reviewCheck(ctx, p, true); err != nil {
		t.Fatal(err)
	}
	if rc := p.Status.PullRequest.ReviewCheck; rc.State != api.ReviewCheckInProgress || lastCheckRun(g, head) != providers.CheckRunInProgress {
		t.Fatalf("the refusal did not heal: %+v, %+v", rc, g.CheckRuns(head))
	}
	if c := meta.FindStatusCondition(p.Status.Conditions, api.ConditionApproved); c.Reason != ReasonWaitingForReview {
		t.Fatalf("Approved: %+v", c)
	}
}

func TestRefusalBackoff(t *testing.T) {
	at := metav1.NewTime(reconcilerClock)
	for _, tc := range []struct {
		refusals int32
		after    time.Duration
		want     time.Duration
	}{
		{1, 0, time.Minute}, {1, time.Minute, 0}, {2, 30 * time.Second, 90 * time.Second}, {7, 0, time.Hour},
		{1000, 0, time.Hour}, {3, -time.Minute, 0},
	} {
		rc := &api.ReviewCheck{State: api.ReviewCheckRefused, Refusals: tc.refusals, RefusedAt: &at}
		if got := refusalBackoff(rc, reconcilerClock.Add(tc.after)); got != tc.want {
			t.Errorf("%d refusals, %s after: %s, want %s", tc.refusals, tc.after, got, tc.want)
		}
	}
	if got := refusalBackoff(&api.ReviewCheck{State: api.ReviewCheckRefused}, reconcilerClock); got != 0 {
		t.Errorf("a refusal recorded without a time waits %s", got)
	}
}

// A gate whose look keeps failing raises one Warning Event after reviewCheckStuckLooks looks in a row, not one
// per look; a look that goes through starts the count again.
func TestReviewCheckStuckWarnsOnce(t *testing.T) {
	r, g, p, _, _ := passedWorld(t)
	ctx := context.Background()
	events := record.NewFakeRecorder(10)
	r.Recorder = events
	g.SetFail(func(op string) error {
		if op == opListReviews {
			return fmt.Errorf("status 502: %w", providers.ErrRetryable)
		}
		return nil
	})
	for i := range 2 * reviewCheckStuckLooks {
		r.watchReviewCheck(ctx, p, true)
		if want := min((i+1)/reviewCheckStuckLooks, 1); len(events.Events) != want {
			t.Fatalf("after %d failed looks: %d Events, want %d", i+1, len(events.Events), want)
		}
	}
	if ev := <-events.Events; !strings.Contains(ev, "Warning "+ReasonReviewCheckStuck) {
		t.Fatalf("event %q", ev)
	}
	g.SetFail(nil)
	r.watchReviewCheck(ctx, p, true)
	if _, ok := r.reviewCheckFailures.Load(client.ObjectKeyFromObject(p)); ok {
		t.Fatal("a look that went through kept the count")
	}
}
