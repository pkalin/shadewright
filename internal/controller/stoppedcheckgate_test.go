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
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/pkalin/shadewright/api/v1alpha1"
)

// TestStoppedHolderLookOnARequestBeingDeleted (#1008): a deletion that starts after the look read the request and
// before its exit (afterLook) reads it again. The live request has the same UID and still holds the lock; the test
// records whether the exit writes the failed check on it.
func TestStoppedHolderLookOnARequestBeingDeleted(t *testing.T) {
	r, g, rr := settlementWorld(t, api.PhaseNeedsIntervention)
	ctx := context.Background()
	r.ReviewerHints = true
	setTargetMain(t, r)
	r.Recorder = record.NewFakeRecorder(100)
	head := rr.Status.PullRequest.HeadSHA
	key := client.ObjectKeyFromObject(rr)
	stale := &api.RemediationRequest{}
	if err := r.Get(ctx, key, stale); err != nil {
		t.Fatal(err)
	}
	stale.Finalizers = append(stale.Finalizers, FinalizerName)
	if err := r.Update(ctx, stale); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(ctx, key, stale); err != nil {
		t.Fatal(err)
	}
	deleting := &api.RemediationRequest{}
	if err := r.Get(ctx, key, deleting); err != nil {
		t.Fatal(err)
	}
	if err := r.Delete(ctx, deleting); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(ctx, key, deleting); err != nil || deleting.DeletionTimestamp.IsZero() ||
		deleting.UID != stale.UID {
		t.Fatalf("err %v, deletion %v: want the request kept by its finalizer with a deletion timestamp", err,
			deleting.DeletionTimestamp)
	}
	_, err := r.checkHeld(ctx, stale)
	runs := g.CheckRuns(head)
	t.Logf("checkHeld on a copy read before the deletion: err %v; check runs on the head of the request being "+
		"deleted: %+v", err, runs)
	if len(runs) != 0 {
		t.Errorf("the look wrote %d check run(s) on a request with a deletion timestamp: %+v", len(runs), runs)
	}
}

// TestStoppedCheckGate is the table of the gate of the failed review check of a stopped request (writeStoppedCheck,
// decision of 2026-11-21 on #1008): one row per refusal, each changing only the condition it names, and one row that
// writes. A row that passes live as nil makes the gate read the request itself.
func TestStoppedCheckGate(t *testing.T) {
	cases := []struct {
		name string
		// change changes live, a copy of the stored request, before the gate decides; nil keeps it.
		change func(live *api.RemediationRequest)
		// read makes the gate read the request itself, after prepare.
		read    bool
		prepare func(t *testing.T, r *RemediationRequestReconciler, rr *api.RemediationRequest)
		hints   *bool
		write   bool
		err     bool
	}{
		{name: "writes", write: true},
		{name: "writes after its own read", read: true, write: true},
		{name: "zero value", change: func(live *api.RemediationRequest) { *live = api.RemediationRequest{} }},
		{name: "another UID", change: func(live *api.RemediationRequest) { live.UID = "another" }},
		{name: "being deleted", change: func(live *api.RemediationRequest) {
			live.DeletionTimestamp = &metav1.Time{Time: time.Unix(1, 0)}
		}},
		{name: "another head", change: func(live *api.RemediationRequest) {
			live.Status.PullRequest.HeadSHA = strings.Repeat("b", 40)
		}},
		{name: "no pull request recorded", change: func(live *api.RemediationRequest) { live.Status.PullRequest = nil }},
		{name: "merge recorded", change: func(live *api.RemediationRequest) {
			live.Status.MergeReceipt = &api.MergeReceipt{}
		}},
		{name: "lock released", change: func(live *api.RemediationRequest) { live.Status.HoldsTarget = new(false) }},
		{name: "lock never recorded", change: func(live *api.RemediationRequest) { live.Status.HoldsTarget = nil }},
		{name: "owns no check: reviewer hints off", hints: new(false)},
		{name: "owns no check: handed over", change: func(live *api.RemediationRequest) {
			live.Status.Handoff = &api.Handoff{To: &api.OutgoingHandoff{Name: "replacement", Phase: api.HandoffDone}}
		}},
		{name: "gone", read: true, prepare: func(t *testing.T, r *RemediationRequestReconciler, rr *api.RemediationRequest) {
			t.Helper()
			stored := &api.RemediationRequest{}
			if err := r.Get(context.Background(), client.ObjectKeyFromObject(rr), stored); err != nil {
				t.Fatal(err)
			}
			stored.Finalizers = nil
			if err := r.Update(context.Background(), stored); err != nil {
				t.Fatal(err)
			}
			if err := r.Delete(context.Background(), stored); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "read fails", read: true, err: true,
			prepare: func(_ *testing.T, r *RemediationRequestReconciler, rr *api.RemediationRequest) {
				r.APIReader = failingRequestReader{Reader: r.Client, name: rr.Name}
			}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, g, rr := settlementWorld(t, api.PhaseNeedsIntervention)
			ctx := context.Background()
			setTargetMain(t, r)
			r.ReviewerHints = c.hints == nil || *c.hints
			key := client.ObjectKeyFromObject(rr)
			if err := r.Get(ctx, key, rr); err != nil {
				t.Fatal(err)
			}
			if !ptr.Deref(rr.Status.HoldsTarget, false) || rr.Status.PullRequest == nil || mergeRecorded(rr) {
				t.Fatalf("the stored request must hold the lock with an open pull request: %+v", rr.Status)
			}
			head := rr.Status.PullRequest.HeadSHA
			var live *api.RemediationRequest
			if !c.read {
				live = rr.DeepCopy()
				if c.change != nil {
					c.change(live)
				}
			}
			if c.prepare != nil {
				c.prepare(t, r, rr)
			}
			git, _, _, err := r.openPlanRepositoryFor(ctx, rr)
			if err != nil {
				t.Fatal(err)
			}
			wrote, err := r.writeStoppedCheck(ctx, ctx, git, rr, live, string(api.PhaseNeedsIntervention))
			if (err != nil) != c.err {
				t.Fatalf("error %v, want one: %v", err, c.err)
			}
			if c.err && !isClusterError(err) {
				t.Errorf("error %v: want a failure of the Kubernetes API, for the error backoff", err)
			}
			runs := g.CheckRuns(head)
			if wrote != c.write || (len(runs) == 1) != c.write || len(runs) > 1 {
				t.Errorf("wrote %v, check runs on the head %+v: want a write %v", wrote, runs, c.write)
			}
		})
	}
}

// TestStoppedCheckRefusalLeavesStatus: a look whose gate refuses records nothing about the check (no Stale, no
// condition ReviewCheckNotFailed) and asks for no early run: the request being deleted gets no status write from it.
func TestStoppedCheckRefusalLeavesStatus(t *testing.T) {
	r, g, rr := settlementWorld(t, api.PhaseNeedsIntervention)
	ctx := context.Background()
	setTargetMain(t, r)
	r.ReviewerHints = true
	if err := r.Get(ctx, client.ObjectKeyFromObject(rr), rr); err != nil {
		t.Fatal(err)
	}
	live := rr.DeepCopy()
	live.DeletionTimestamp = &metav1.Time{Time: time.Unix(1, 0)}
	before := rr.Status.DeepCopy()
	if err := r.failStoppedCheck(ctx, rr, live, string(api.PhaseNeedsIntervention)); err != nil {
		t.Fatal(err)
	}
	if runs := g.CheckRuns(rr.Status.PullRequest.HeadSHA); len(runs) != 0 {
		t.Errorf("check runs %+v: want none", runs)
	}
	if !equality.Semantic.DeepEqual(before, &rr.Status) {
		t.Errorf("status changed by a refused write: %+v, was %+v", rr.Status, *before)
	}
}

// stoppedCheckGate is the one function that may build the failed review check of a stopped request, so the one that
// writes it (TestStoppedCheckWrittenOnlyByItsGate).
const stoppedCheckGate = "writeStoppedCheck"

// stoppedCheckWriters lists where f names stoppedCheckRun (the run the Git host's check write takes for this purpose)
// or its title outside the gate, keyed by the function it is in; the declarations of both are left out.
func stoppedCheckWriters(fset *token.FileSet, owner string, n ast.Node) []string {
	id, ok := n.(*ast.Ident)
	if !ok || (id.Name != "stoppedCheckRun" && id.Name != "stoppedCheckTitle") {
		return nil
	}
	switch {
	case id.Name == "stoppedCheckRun" && (owner == stoppedCheckGate || owner == "stoppedCheckRun"):
		return nil
	case id.Name == "stoppedCheckTitle" && (owner == "stoppedCheckRun" || owner == ""):
		// The constant's own declaration (owner "") and the one function that builds the run.
		return nil
	}
	return []string{fmt.Sprintf("%s: %q names %s", fset.Position(n.Pos()), owner, id.Name)}
}

// TestStoppedCheckWrittenOnlyByItsGate: no function of the package but the gate (writeStoppedCheck) builds or writes
// the failed review check of a stopped request, so every caller goes through its refusals (decision of 2026-11-21 on
// #1008).
func TestStoppedCheckWrittenOnlyByItsGate(t *testing.T) {
	forEachSource(t, func(fset *token.FileSet, owner string, n ast.Node) {
		for _, what := range stoppedCheckWriters(fset, owner, n) {
			t.Errorf("%s; write the failed review check of a stopped request through %s", what, stoppedCheckGate)
		}
	})
}

// TestStoppedCheckGuardCatchesAWriter checks the guard above on code it must refuse (a direct write, a run built
// elsewhere, the function taken as a value, the title reused) and on the gate and the builder, which pass.
func TestStoppedCheckGuardCatchesAWriter(t *testing.T) {
	const src = `package controller
const stoppedCheckTitle = "x"
func stoppedCheckRun(reason string) R { return R{Title: stoppedCheckTitle} }
func (r *X) writeStoppedCheck(g G) error { return g.CreateCheckRun(ctx, h, stoppedCheckRun("x")) }
func (r *X) direct(g G) error { return g.CreateCheckRun(ctx, h, stoppedCheckRun("x")) }
func (r *X) built() R { return stoppedCheckRun("x") }
func (r *X) value() { f := stoppedCheckRun; _ = f }
func (r *X) title(g G) error { return g.CreateCheckRun(ctx, h, R{Title: stoppedCheckTitle}) }
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "x.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	forEachDecl(f, func(owner string, n ast.Node) {
		if len(stoppedCheckWriters(fset, owner, n)) > 0 {
			found[owner] = true
		}
	})
	for _, want := range []string{"direct", "built", "value", "title"} {
		if !found[want] {
			t.Errorf("the guard missed %s", want)
		}
		delete(found, want)
	}
	if len(found) > 0 {
		t.Errorf("the guard refused %v, which may name the run", found)
	}
}
