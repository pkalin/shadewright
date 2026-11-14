package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	api "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/providers"
	fakegit "github.com/pkalin/shadewright/internal/providers/fake"
	fx "github.com/pkalin/shadewright/internal/testfixtures"
)

// The first request of the chain in handoffAppendedWorld.
const (
	rootName = "root"
	rootUID  = "root-uid"
	newUID   = "new-uid"
)

// handoffAppendedWorld is a handoff stopped after the replacement ("new") appended its commit to the
// predecessor's ("old") branch and before it recorded the takeover: the predecessor records its intent, the
// replacement holds the hint Reserved, and the branch head is the replacement's commit. With root set, "old"
// is itself a replacement: it took the pull request over from "root", the chain's first request, whose marker
// the body still carries because the rewrite of "old" was refused; root may change "root" before it is stored.
func handoffAppendedWorld(t *testing.T, root func(*api.RemediationRequest)) (*RemediationRequestReconciler,
	*fakegit.Git, *api.RemediationRequest, *api.RemediationRequest, providers.ChangeRequest, string) {
	t.Helper()
	ctx := context.Background()
	p := fx.MemoryLimitRequest("ns", oldName)
	p.UID = linkPred
	p.Status.PlanDigest = "sha256:" + strings.Repeat("a", 64)
	p.Status.Phase = api.PhaseAwaitingApproval
	p.Status.Source = &api.ResolvedSource{Path: sourcePath, TargetBranch: mainBranch}
	s := fx.RemediationSettings()
	s.Spec.Repositories[0].TargetBranch = mainBranch
	p.Status.PlanRepository = &api.PlanRepository{URL: s.Spec.Repositories[0].URL, InstallationID: 1}
	h := api.Hint{Source: api.HintSourceGitHub, CommentID: 7, Digest: "sha256:" + strings.Repeat("b", 64),
		State: api.HintReserved, Author: api.HintAuthor{ID: 1001, Login: "first"}}
	p.Status.Hints = []api.Hint{h}
	objs := []client.Object{s}
	opener := p
	var from *api.IncomingHandoff
	if root != nil {
		first := p.DeepCopy()
		first.Name, first.UID = rootName, rootUID
		first.Status.Phase, first.Status.Hints = api.PhaseSuperseded, nil
		first.Status.Handoff = &api.Handoff{To: &api.OutgoingHandoff{Name: api.Name(p.Name), UID: api.UID(p.UID),
			Phase: api.HandoffDone}}
		p.Spec.ChainRoot = api.UID(first.UID)
		p.Spec.Replaces = &api.RequestReplacement{Name: api.Name(first.Name), UID: api.UID(first.UID)}
		from = &api.IncomingHandoff{Name: api.Name(first.Name), UID: api.UID(first.UID), Phase: api.HandoffDone,
			PullRequestUpdate: api.HandoffStepRefused}
		root(first)
		objs, opener = append(objs, first), first
	}
	rr := p.DeepCopy()
	rr.Name, rr.UID = newName, newUID
	rr.Spec.ChainRoot = chainRootOf(p)
	rr.Spec.Replaces = &api.RequestReplacement{Name: api.Name(p.Name), UID: api.UID(p.UID), Hint: refOf(&h)}
	rr.Spec.Action.SetContainerMemoryLimit.Desired = resource.MustParse("512Mi")
	rr.Status.Phase, rr.Status.Hints = api.PhasePolicyChecked, nil
	rr.Status.Handoff = &api.Handoff{From: &api.IncomingHandoff{Name: api.Name(p.Name), UID: api.UID(p.UID),
		Hint: refOf(&h), Phase: api.HandoffReserved}}
	g := fakegit.NewGit(mainBranch, map[string][]byte{sourcePath: deploymentManifest(p.Spec.TargetRef.Name,
		p.Spec.TargetRef.Namespace, workerContainer, "256Mi")})
	base, err := g.BranchHead(ctx, mainBranch)
	if err != nil {
		t.Fatal(err)
	}
	branch := "shadewright/" + string(chainRootOf(p))
	if err := g.CreateBranch(ctx, branch, base); err != nil {
		t.Fatal(err)
	}
	old, err := g.CommitFile(ctx, branch, base, providers.FileChange{Path: sourcePath, Content: deploymentManifest(
		p.Spec.TargetRef.Name, p.Spec.TargetRef.Namespace, workerContainer, "384Mi"), Message: oldName})
	if err != nil {
		t.Fatal(err)
	}
	cr, err := g.OpenChangeRequest(ctx, providers.NewChangeRequest{HeadBranch: branch, BaseBranch: mainBranch,
		Title: oldName, Body: marker(opener)})
	if err != nil {
		t.Fatal(err)
	}
	p.Status.PullRequest = &api.PullRequestStatus{Number: cr.Number, URL: cr.URL, HeadSHA: old}
	p.Status.Handoff = &api.Handoff{From: from, To: &api.OutgoingHandoff{Name: api.Name(rr.Name),
		UID: api.UID(rr.UID), Hint: refOf(&h), ExpectedHead: old, Phase: api.HandoffIntent}}
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
	objs = append(objs, p, rr)
	c := clientfake.NewClientBuilder().WithScheme(deadlineScheme(t)).WithStatusSubresource(objs[1:]...).
		WithObjects(objs...).Build()
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

// A stop after the append and before the takeover record, then a merge with a merge commit (#591): the branch
// head is on the target branch, and recovery must still see the merge of its own commit.
func TestRecoverHandoffMergedWithMergeCommit(t *testing.T) {
	r, g, p, rr, cr, head := handoffAppendedWorld(t, nil)
	ctx := context.Background()
	if ok, err := r.isHandoffCommit(ctx, g, rr, p, p.Status.Handoff.To, head); err != nil || !ok {
		t.Fatalf("before merge: ours=%v, err=%v", ok, err)
	}
	if _, err := g.MergeWithCommit(cr.Number); err != nil {
		t.Fatal(err)
	}
	_, recovered, err := r.recoverStep(ctx, rr)
	if err != nil {
		t.Fatal(err)
	}
	if !recovered || rr.Status.Phase != api.PhaseMerged {
		t.Fatalf("own merged handoff was not recovered: recovered=%v, phase=%s", recovered, rr.Status.Phase)
	}
}

// A merged head that changes another file beside the target file is not the handoff's commit, even though its
// parent is the recorded head and the target file holds the replacement's plan.
func TestRecoverHandoffMergedForeignCommit(t *testing.T) {
	r, g, p, rr, cr, head := handoffAppendedWorld(t, nil)
	ctx := context.Background()
	file, err := g.ReadFile(ctx, head, sourcePath, providers.MaxFileBytes)
	if err != nil {
		t.Fatal(err)
	}
	branch := "shadewright/" + string(chainRootOf(p))
	if err := g.ForcePush(branch, p.Status.Handoff.To.ExpectedHead); err != nil {
		t.Fatal(err)
	}
	foreign, err := g.PushFiles(branch, map[string][]byte{sourcePath: file.Data, "other.yaml": []byte("x: 1\n")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.MergeWithCommit(cr.Number); err != nil {
		t.Fatal(err)
	}
	if ok, err := r.isHandoffCommit(ctx, g, rr, p, p.Status.Handoff.To, foreign); err != nil || ok {
		t.Fatalf("a merged foreign commit was taken for the handoff's: ours=%v, err=%v", ok, err)
	}
}

// Two consecutive refused body rewrites (#591): the body keeps the marker of the chain's first request, and the
// latest request must still recognise the pull request as its own.
func TestCloseChangeRequestAfterTwoRefusedRewrites(t *testing.T) {
	r, g, p, rr, cr, head := handoffAppendedWorld(t, func(*api.RemediationRequest) {})
	ctx := context.Background()
	if !strings.Contains(cr.Body, "request-uid="+rootUID+" ") || !hasMarker(cr.Body, p) {
		t.Fatalf("fixture: the body does not carry the root's marker that the predecessor accepts: %q", cr.Body)
	}
	if err := r.recordHandoff(ctx, rr, p, cr, head); err != nil {
		t.Fatal(err)
	}
	rr.Status.Handoff.From.PullRequestUpdate = api.HandoffStepRefused
	if err := r.Status().Update(ctx, rr); err != nil {
		t.Fatal(err)
	}
	if hasMarker(cr.Body, rr) {
		t.Fatal("fixture: the replacement accepts the root's marker without reading the chain")
	}
	if v, _, err := r.lookHeld(ctx, rr); err != nil || v.state != changeOpen || v.pr.Number != cr.Number {
		t.Fatalf("the lock's look-up lost the inherited pull request: %v, %v", v, err)
	}
	if err := r.closeChangeRequest(ctx, rr); err != nil {
		t.Fatalf("replacement lost ownership after two refused rewrites: %v", err)
	}
	if got, err := g.FindChangeRequest(ctx, "shadewright/"+rootUID); err != nil || got.State != providers.ChangeRequestClosed {
		t.Fatalf("the pull request was not closed: %+v, %v", got, err)
	}
}

// The marker of an earlier request is accepted only through an unbroken chain of the replacement's own: a link
// of another chain, one that did not hand over to the request below it, one whose rewrite was done (the body
// then holds a later marker), or one that is gone never makes the pull request the replacement's.
func TestInheritedMarkerNeedsTheChain(t *testing.T) {
	for name, c := range map[string]struct {
		root func(*api.RemediationRequest)
		pred func(*api.RemediationRequest)
	}{
		"root of another chain": {root: func(x *api.RemediationRequest) { x.Spec.ChainRoot = "other-uid" }},
		"root handed over to another request": {root: func(x *api.RemediationRequest) {
			x.Status.Handoff.To.UID = "someone-uid"
		}},
		// Recreated under the same name and chain root: the name no longer reaches the request that handed over.
		"root with another UID": {root: func(x *api.RemediationRequest) {
			x.UID, x.Spec.ChainRoot = "elsewhere-uid", rootUID
		}},
		"predecessor's rewrite done": {root: func(*api.RemediationRequest) {}, pred: func(x *api.RemediationRequest) {
			x.Status.Handoff.From.PullRequestUpdate = api.HandoffStepDone
		}},
	} {
		t.Run(name, func(t *testing.T) {
			r, _, p, rr, cr, head := handoffAppendedWorld(t, c.root)
			ctx := context.Background()
			if c.pred != nil {
				c.pred(p)
				if err := r.Status().Update(ctx, p); err != nil {
					t.Fatal(err)
				}
			}
			if err := r.recordHandoff(ctx, rr, p, cr, head); err != nil {
				t.Fatal(err)
			}
			rr.Status.Handoff.From.PullRequestUpdate = api.HandoffStepRefused
			if err := r.Status().Update(ctx, rr); err != nil {
				t.Fatal(err)
			}
			if ok, err := r.ownsMarker(ctx, cr.Body, rr); err != nil || ok {
				t.Fatalf("ownsMarker = %v, %v, want false", ok, err)
			}
			if err := r.closeChangeRequest(ctx, rr); !errors.Is(err, errChangeRequestNotOurs) {
				t.Fatalf("closeChangeRequest = %v, want errChangeRequestNotOurs", err)
			}
		})
	}
	t.Run("root deleted", func(t *testing.T) {
		r, _, p, rr, cr, head := handoffAppendedWorld(t, func(*api.RemediationRequest) {})
		ctx := context.Background()
		if err := r.recordHandoff(ctx, rr, p, cr, head); err != nil {
			t.Fatal(err)
		}
		root := &api.RemediationRequest{}
		root.Namespace, root.Name = rr.Namespace, rootName
		if err := r.Delete(ctx, root); err != nil {
			t.Fatal(err)
		}
		if ok, err := r.ownsMarker(ctx, cr.Body, rr); err != nil || ok {
			t.Fatalf("ownsMarker = %v, %v, want false", ok, err)
		}
	})
}

// One 404 for the branch while the predecessor's pull request is open is a failed read, never "nothing to
// recover" (#688): the recovery is retried, and the replacement does not go on to the freshness check, which could
// let it expire with its commit on the pull request.
func TestRecoverHandoffRetriesABranch404WhileThePullRequestIsOpen(t *testing.T) {
	r, g, _, rr, _, _ := handoffAppendedWorld(t, nil)
	ctx := context.Background()
	g.SetFail(func(op string) error {
		if op == opBranchHead {
			return fmt.Errorf("branch: %w", providers.ErrNotFound)
		}
		return nil
	})
	_, recovered, err := r.recoverStep(ctx, rr)
	if err == nil || !recovered {
		t.Fatalf("a 404 for the branch of an open pull request was taken as nothing to recover: recovered=%v, err=%v",
			recovered, err)
	}
	if !errors.Is(err, errConflictingReads) {
		t.Fatalf("want the conflicting reads in the error, got %v", err)
	}
	if rr.Status.Phase != api.PhasePolicyChecked {
		t.Fatalf("the replacement moved on a failed read: %s", rr.Status.Phase)
	}
}

// A 404 for the branch of a merged pull request (a branch deleted after the merge) is judged by the pull request's
// head: the merge of the handoff's commit is the replacement's.
func TestRecoverHandoffJudgesTheMergedHeadWhenTheBranchIsGone(t *testing.T) {
	r, g, _, rr, cr, _ := handoffAppendedWorld(t, nil)
	ctx := context.Background()
	if _, err := g.MergeWithCommit(cr.Number); err != nil {
		t.Fatal(err)
	}
	g.SetFail(func(op string) error {
		if op == opBranchHead {
			return fmt.Errorf("branch: %w", providers.ErrNotFound)
		}
		return nil
	})
	_, recovered, err := r.recoverStep(ctx, rr)
	if err != nil {
		t.Fatal(err)
	}
	if !recovered || rr.Status.Phase != api.PhaseMerged {
		t.Fatalf("the merge of the handoff's commit was not recovered: recovered=%v, phase=%s", recovered, rr.Status.Phase)
	}
}

// A read of the predecessor's pull request that the Git host refuses (403) is an unknown verdict, not a refused
// write (#688): the recovery retries it before the deadline, and once the grace after it is over ends with the
// recovery's own reason, never with a refusal of a close.
func TestRecoverHandoffRetriesARefusedReadUntilTheGraceEnds(t *testing.T) {
	r, g, _, rr, _, _ := handoffAppendedWorld(t, nil)
	ctx := context.Background()
	g.SetFail(func(op string) error {
		switch op {
		case opBranchHead:
			return fmt.Errorf("branch: %w", providers.ErrNotFound)
		case opGet, opList:
			return fmt.Errorf("status 403: %w", providers.ErrForbidden)
		}
		return nil
	})
	if _, err := r.recheck(ctx, rr); err == nil {
		t.Fatal("a refused read was not handed back for a retry")
	}
	if rr.Status.Phase != api.PhasePolicyChecked {
		t.Fatalf("a refused read before the deadline ended the replacement: %s", rr.Status.Phase)
	}
	pastDeadline(t, r, rr, handoffRecoveryGrace+time.Minute)
	if _, err := r.recheck(ctx, rr); err != nil {
		t.Fatal(err)
	}
	if rr.Status.Phase != api.PhaseNeedsIntervention {
		t.Fatalf("phase %s", rr.Status.Phase)
	}
	if c := meta.FindStatusCondition(rr.Status.Conditions, api.ConditionPROpened); c == nil || c.Reason != ReasonHandoffRecoveryFailed {
		t.Fatalf("PullRequestOpened: %+v", c)
	}
}
