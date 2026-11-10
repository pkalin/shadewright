package controller

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/pkalin/shadewright/api/v1alpha1"
)

// TestHintsOffAfterTakeoverRecordBeforeReviewCheck: an executor restarted with reviewer hints off after the
// replacement recorded the takeover (from Done, PRCreated), before it superseded the predecessor and before the
// check run shadewright/review was created on the new head. The replacement stops (NeedsIntervention,
// HintsTurnedOff): it never supersedes the predecessor, never rewrites the pull request and never reaches
// AwaitingApproval, so no head a hint steered waits for review without the merge gate.
func TestHintsOffAfterTakeoverRecordBeforeReviewCheck(t *testing.T) {
	r, g, pred, replacement, cr, head := handoffAppendedWorld(t, nil)
	ctx := context.Background()
	if err := r.recordHandoff(ctx, replacement, pred, cr, head); err != nil {
		t.Fatal(err)
	}
	title := g.ChangeRequests()[0].Title
	// Restart after recording the new head, before superseding the predecessor or creating its check.
	r.ReviewerHints = false
	if err := r.afterHandoff(ctx, replacement); err != nil {
		t.Fatal(err)
	}
	if _, err := r.watchPullRequest(ctx, replacement); err != nil {
		t.Fatal(err)
	}
	for _, rr := range []*api.RemediationRequest{pred, replacement} {
		if err := r.Get(ctx, client.ObjectKeyFromObject(rr), rr); err != nil {
			t.Fatal(err)
		}
	}
	if replacement.Status.Phase != api.PhaseNeedsIntervention || pred.Status.Phase != api.PhaseAwaitingApproval {
		t.Fatalf("hints off kept the hint-caused head adopted: replacement=%s, predecessor=%s, check runs=%v",
			replacement.Status.Phase, pred.Status.Phase, g.CheckRuns(head))
	}
	if c := meta.FindStatusCondition(replacement.Status.Conditions, api.ConditionApproved); c == nil ||
		c.Reason != api.ReasonHintsTurnedOff {
		t.Fatalf("condition Approved = %+v, want reason %s", c, api.ReasonHintsTurnedOff)
	}
	if replacement.Status.Handoff.From.PullRequestUpdate != "" || g.ChangeRequests()[0].Title != title {
		t.Fatalf("the pull request was rewritten with hints off: %q", replacement.Status.Handoff.From.PullRequestUpdate)
	}
	if runs := g.CheckRuns(head); len(runs) != 0 {
		t.Fatalf("check runs with hints off = %v, want none", runs)
	}
}

// TestHintsOffWatchNeverMovesTakeoverToAwaitingApproval: the watch of the pull request alone, without the steps
// after the handoff before it, stops a request that took the pull request over instead of moving it to
// AwaitingApproval when reviewer hints are off.
func TestHintsOffWatchNeverMovesTakeoverToAwaitingApproval(t *testing.T) {
	r, g, pred, replacement, cr, head := handoffAppendedWorld(t, nil)
	ctx := context.Background()
	if err := r.recordHandoff(ctx, replacement, pred, cr, head); err != nil {
		t.Fatal(err)
	}
	r.ReviewerHints = false
	if out, err := r.watchPullRequest(ctx, replacement); err != nil || out != prStopped {
		t.Fatalf("watchPullRequest() = %v, %v, want prStopped", out, err)
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(replacement), replacement); err != nil {
		t.Fatal(err)
	}
	if replacement.Status.Phase != api.PhaseNeedsIntervention {
		t.Fatalf("replacement=%s, want %s; check runs=%v", replacement.Status.Phase, api.PhaseNeedsIntervention,
			g.CheckRuns(head))
	}
}
