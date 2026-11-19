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
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	api "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/policy/freshness"
	fakegit "github.com/pkalin/shadewright/internal/providers/fake"
)

// After the takeover a person force-pushes the predecessor's commit back and merges it, and the host reports no merge
// commit ID. The predecessor reads the merge first and stores its MergeCheck wait (a claim); the replacement then waits
// for that record (MergeRecordPending). Its next look must write nothing to Git, not even the rewrite of the pull
// request of the handoff's last step (P4).
func TestHandoffModelReplacementAwaitingTheRecordDoesNotRewrite(t *testing.T) {
	hmDirected(t, hmStartIntent, []hmStep{stRecRepl, {Kind: hForce, Variant: forceDrop}, stMergeNoID, stRecPred,
		stRecRepl, stRecRepl})
}

// The same after a restart: the new reconciler has no answer of the rewrite in memory, so it would send it again.
func TestHandoffModelReplacementAwaitingTheRecordDoesNotRewriteAfterARestart(t *testing.T) {
	hmDirected(t, hmStartIntent, []hmStep{stRecRepl, {Kind: hForce, Variant: forceDrop}, stMergeNoID, stRecPred,
		stRecRepl, {Kind: hRestart}, stRecRepl, stRecRepl})
}

// The replacement's recovery meets a refusal (403) and stores the abort and its HandoffRecovery wait; reads answer
// again, a person merges its commit, the check at the merge cannot read the plan (503) and stores its MergeCheck
// wait, and the executor crashes before the takeover's status write. After a restart more than a day later (longer than any wait) both waits
// stand past their end: one reconcile ends both, or the next one does at the poll pace, and none asks to run again
// sooner than the pace floor (P5).
func TestHandoffModelRestartPastEveryWaitEndsThemAll(t *testing.T) {
	once := func(code int, op string) hmStep { return hmStep{Kind: hFault, Code: code, Op: op} }
	hmDirected(t, hmStartPushed, []hmStep{once(403, opBranchHead), stRecRepl, stAnswer, stMerge,
		once(503, hmOpPolicyRead), {Kind: hCrash, N: 1}, stRecRepl, {Kind: hRestartLate}, stRecRepl, stRecRepl})
}

// The record hold sits in the gate every Git write passes: a write of a request whose other request holds a claim on
// a merge of their pull request never reaches the host, nor one whose read of that request fails (then for a retry, as
// the Kubernetes API's); without a claim the write goes through.
func TestRecordHoldGatesEveryWrite(t *testing.T) {
	ctx := context.Background()
	other := &api.RemediationRequest{ObjectMeta: metav1.ObjectMeta{Namespace: requestNamespace, Name: oldName,
		UID: hmPredUID}}
	other.Status.PullRequest = &api.PullRequestStatus{Number: 1, HeadSHA: strings.Repeat("a", 40)}
	rr := &api.RemediationRequest{ObjectMeta: metav1.ObjectMeta{Namespace: requestNamespace, Name: "new"}}
	rr.Status.PullRequest = &api.PullRequestStatus{Number: 1, HeadSHA: strings.Repeat("b", 40)}
	rr.Status.Handoff = &api.Handoff{From: &api.IncomingHandoff{Name: oldName, UID: hmPredUID,
		Phase: api.HandoffDone}}
	claim := other.DeepCopy()
	claim.Status.GitWaits = []api.GitWait{{Kind: api.GitWaitMergeCheck, Since: metav1.Now(),
		PullRequest: &api.GitWaitPullRequest{Number: 1, HeadSHA: strings.Repeat("a", 40)}}}
	failing := interceptor.Funcs{Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object,
		...client.GetOption) error {
		return apierrors.NewServiceUnavailable("down")
	}}
	for _, c := range []struct {
		name   string
		other  *api.RemediationRequest
		funcs  interceptor.Funcs
		want   error
		called bool
	}{
		{"claim", claim, interceptor.Funcs{}, errRecordHeld, false},
		{"failed read", other, failing, freshness.ErrWriteBlocked, false},
		{"no claim", other, interceptor.Funcs{}, nil, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			reader := interceptor.NewClient(clientfake.NewClientBuilder().WithScheme(deadlineScheme(t)).
				WithObjects(c.other).Build(), c.funcs)
			r := &RemediationRequestReconciler{Client: reader, APIReader: reader}
			g := fakegit.NewGit(mainBranch, nil)
			called := false
			g.SetFail(func(op string) error {
				called = called || op == opUpdateChangeRequest
				return nil
			})
			git := &gatedGit{git: g, allow: func(context.Context) error { return nil }}
			err := git.UpdateChangeRequest(r.withRecordHold(ctx, rr), 1, "title", "body")
			if c.want != nil && !errors.Is(err, c.want) {
				t.Fatalf("got %v, want %v", err, c.want)
			}
			if called != c.called {
				t.Fatalf("the write reached the host: %t, want %t", called, c.called)
			}
		})
	}
}
