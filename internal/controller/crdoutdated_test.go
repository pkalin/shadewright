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
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	api "github.com/shadewright-labs/shadewright/api/v1alpha1"
)

// The tests of #866: a CustomResourceDefinition of RemediationRequest older than the executor prunes the status
// fields added since v0.5.2, and the executor's status writes still succeed.

// pruneStatus strips from rr what a CustomResourceDefinition of v0.5.2 does not know.
func pruneStatus(rr *api.RemediationRequest) bool {
	pruned := len(rr.Status.GitWaits) > 0 || rr.Status.UnfinishedRenders != nil ||
		(rr.Status.PullRequest != nil && rr.Status.PullRequest.IncidentStopped != nil)
	rr.Status.GitWaits = nil
	rr.Status.UnfinishedRenders = nil
	if rr.Status.PullRequest != nil {
		rr.Status.PullRequest.IncidentStopped = nil
	}
	return pruned
}

// pruningClient is base as an API server with the old CustomResourceDefinition installed: every status write of a
// request succeeds, and what it stored and answers lacks the fields pruneStatus strips.
func pruningClient(base client.WithWatch) client.WithWatch {
	after := func(ctx context.Context, c client.Client, obj client.Object) error {
		rr, ok := obj.(*api.RemediationRequest)
		if !ok {
			return nil
		}
		stored := &api.RemediationRequest{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(rr), stored); err != nil {
			return err
		}
		if pruneStatus(stored) {
			if err := c.Status().Update(ctx, stored); err != nil {
				return err
			}
		}
		stored.DeepCopyInto(rr)
		return nil
	}
	return interceptor.NewClient(base, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object,
			opts ...client.SubResourceUpdateOption) error {
			if err := c.SubResource(sub).Update(ctx, obj, opts...); err != nil {
				return err
			}
			return after(ctx, c, obj)
		},
		SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch,
			opts ...client.SubResourcePatchOption) error {
			if err := c.SubResource(sub).Patch(ctx, obj, patch, opts...); err != nil {
				return err
			}
			return after(ctx, c, obj)
		},
	})
}

// withOldCRD makes the model's API server prune the new fields from now on, and records Events.
func withOldCRD(m *modelWorld) *record.FakeRecorder {
	m.r.Client = pruningClient(m.r.Client.(client.WithWatch))
	rec := record.NewFakeRecorder(16)
	m.r.Recorder = rec
	return rec
}

// outdatedEvents returns the CustomResourceDefinitionOutdated Events rec holds.
func outdatedEvents(rec *record.FakeRecorder) []string {
	var out []string
	for {
		select {
		case e := <-rec.Events:
			if strings.Contains(e, EventCustomResourceDefinitionOutdated) {
				out = append(out, e)
			}
		default:
			return out
		}
	}
}

// expectOutdated runs one reconcile of m and checks that it ends with the error that names the outdated
// CustomResourceDefinition and status.gitWaits, records the Warning Event, and leaves the phase as it was.
func expectOutdated(t *testing.T, m *modelWorld, rec *record.FakeRecorder) {
	t.Helper()
	before := m.live().Status.Phase
	_, live, err := m.once()
	if !IsCRDOutdated(err) || !isClusterError(err) {
		t.Fatalf("Reconcile error = %v; want the error of an outdated CustomResourceDefinition", err)
	}
	for _, want := range []string{"status.gitWaits", "CustomResourceDefinition", upgradeDocsURL} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error %q does not name %q", err, want)
		}
	}
	if ev := outdatedEvents(rec); len(ev) != 1 || !strings.HasPrefix(ev[0], "Warning ") {
		t.Errorf("Events = %v; want one Warning %s", ev, EventCustomResourceDefinitionOutdated)
	}
	if live.Status.Phase != before {
		t.Errorf("the phase moved from %s to %s in a reconcile whose wait was not stored", before, live.Status.Phase)
	}
}

// recordsWait checks the setup of a test: with the current CustomResourceDefinition, one reconcile of m records a
// wait of kind k.
func recordsWait(t *testing.T, m *modelWorld, k waitKind) {
	t.Helper()
	if _, live, err := m.once(); err != nil || gitWait(live, k) == nil {
		t.Fatalf("setup: with the current CRD the reconcile returned %v and recorded waits %v; want a %s wait", err,
			live.Status.GitWaits, gitWaitKinds[k])
	}
}

// A merge seen without its commit: the MergeCheck wait is pruned. The reconcile fails visibly at every look instead
// of starting the five minutes again in silence, and the merge is not recorded meanwhile.
func TestOutdatedCRDMergeWithoutItsCommit(t *testing.T) {
	setup := func() *modelWorld {
		m := scripted(t, modelStart{start: startRunning, fresh: true})
		m.steps(1)
		sha, err := m.g.Merge(m.number)
		if err != nil {
			t.Fatal(err)
		}
		m.merge, m.noCommit = sha, true
		return m
	}
	recordsWait(t, setup(), waitMergeCheck)
	m := setup()
	rec := withOldCRD(m)
	for range 3 {
		expectOutdated(t, m, rec)
		m.clock.advance(time.Minute)
	}
	if live := m.live(); mergeRecorded(live) {
		t.Fatalf("the merge was recorded (%s) though its wait was never stored", live.Status.Phase)
	}
}

// A request in PolicyChecked whose repository cannot be opened: the RequestDeadline wait is pruned.
func TestOutdatedCRDRequestDeadlineWait(t *testing.T) {
	recordsWait(t, scripted(t, modelStart{start: startPolicyChecked, open: true, lasting: 24 * time.Hour}),
		waitRequestDeadline)
	m := scripted(t, modelStart{start: startPolicyChecked, open: true, lasting: 24 * time.Hour})
	rec := withOldCRD(m)
	expectOutdated(t, m, rec)
	// Past the deadline the request still ends Expired: that end reads status.deadline, not the pruned entry.
	m.clock.advance(modelPolicyDeadline + time.Minute)
	if _, live, err := m.once(); err != nil || live.Status.Phase != api.PhaseExpired {
		t.Fatalf("past the deadline: phase %s, error %v; want Expired", live.Status.Phase, err)
	}
}

// A replacement recovering a handoff past its deadline, within the grace: the HandoffGrace wait is pruned.
func TestOutdatedCRDHandoffGraceWait(t *testing.T) {
	setup := func() *modelWorld {
		m := scripted(t, modelStart{start: startPolicyChecked, open: true, lasting: 24 * time.Hour, handoff: true})
		m.clock.advance(modelPolicyDeadline + time.Minute)
		return m
	}
	recordsWait(t, setup(), waitHandoffGrace)
	m := setup()
	rec := withOldCRD(m)
	expectOutdated(t, m, rec)
}

// The writer itself: a lost status.unfinishedRenders or status.pullRequest.incidentStopped fails the write too; a
// write that keeps what it sent, or removes entries, does not; and after a lost field every later status write of
// the request in the same reconcile is refused without being sent.
func TestStatusWriterChecksTheAnswer(t *testing.T) {
	r, _, rr := settlementWorld(t, api.PhaseAwaitingApproval)
	ctx := context.Background()
	key := client.ObjectKeyFromObject(rr)
	healthy := r.Client
	rec := record.NewFakeRecorder(16)
	r.Recorder = rec
	if err := r.Status().Update(ctx, rr); err != nil {
		t.Fatalf("a write with none of the new fields: %v", err)
	}
	rr.Status.GitWaits = []api.GitWait{{Kind: api.GitWaitPaced, Since: metav1.Now(), Operation: "read the pull request"}}
	if err := r.Status().Update(ctx, rr); err != nil || len(rr.Status.GitWaits) != 1 {
		t.Fatalf("a stored wait: %v, %v", err, rr.Status.GitWaits)
	}
	rr.Status.GitWaits = nil
	if err := r.Status().Update(ctx, rr); err != nil {
		t.Fatalf("a removed wait: %v", err)
	}
	r.Client = pruningClient(healthy.(client.WithWatch))
	cases := map[string]func(*api.RemediationRequest){
		"status.unfinishedRenders": func(rr *api.RemediationRequest) {
			rr.Status.UnfinishedRenders = &api.UnfinishedRenders{PlanDigest: rr.Status.PlanDigest, Count: 1, Process: "p"}
		},
		"status.pullRequest.incidentStopped": func(rr *api.RemediationRequest) {
			rr.Status.PullRequest.IncidentStopped = &api.IncidentCountReading{UID: "u", Count: 3,
				ReadAt: metav1.Now()}
		},
	}
	for field, set := range cases {
		_ = r.takeOutdatedCRD(key)
		if err := r.Get(ctx, key, rr); err != nil {
			t.Fatal(err)
		}
		base := rr.DeepCopy()
		set(rr)
		err := r.Status().Patch(ctx, rr, client.MergeFrom(base))
		if !IsCRDOutdated(err) || !strings.Contains(err.Error(), field) {
			t.Fatalf("%s pruned: error %v", field, err)
		}
		if ev := outdatedEvents(rec); len(ev) != 1 {
			t.Fatalf("%s pruned: Events %v", field, ev)
		}
	}
	// Refused in the same reconcile, without a write: the condition below never reaches the server.
	if err := r.Get(ctx, key, rr); err != nil {
		t.Fatal(err)
	}
	rv := rr.ResourceVersion
	rr.Status.Conditions = append(rr.Status.Conditions, metav1.Condition{Type: "Later", Status: metav1.ConditionTrue, Reason: "Later", Message: "a later write", LastTransitionTime: metav1.Now()})
	if err := r.Status().Update(ctx, rr); !IsCRDOutdated(err) {
		t.Fatalf("a later write in the same reconcile: %v", err)
	}
	if err := r.Get(ctx, key, rr); err != nil || rr.ResourceVersion != rv {
		t.Fatalf("the later write reached the server: %v, resourceVersion %s -> %s", err, rv, rr.ResourceVersion)
	}
}

// Every status write of the executor goes through the reconciler's Status() (crdoutdated.go): no file of the package
// reaches the status writer of the embedded client (r.Client.Status(), SubResource("status")) or of another client.
func TestEveryStatusWriteIsChecked(t *testing.T) {
	r, _, _ := settlementWorld(t, api.PhaseAwaitingApproval)
	if _, ok := r.Status().(*storedStatusWriter); !ok {
		t.Fatalf("the reconciler's Status() is %T, not the writer that checks the answer", r.Status())
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") || name == "crdoutdated.go" {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch {
			case sel.Sel.Name == "SubResource":
				t.Errorf("%s: a write through SubResource bypasses the reconciler's Status()", fset.Position(call.Pos()))
			case sel.Sel.Name == "Status" && len(call.Args) == 0:
				recv, ok := sel.X.(*ast.Ident)
				if !ok || recv.Name != "r" {
					if isAPIStatus(sel.X) {
						return true
					}
					t.Errorf("%s: %s.Status() is not the reconciler's status writer", fset.Position(call.Pos()),
						exprString(sel.X))
					return true
				}
				checked++
			}
			return true
		})
	}
	if checked < 40 {
		t.Fatalf("found %d status writes through r.Status(); the scan is broken", checked)
	}
}

// isAPIStatus reports the one other Status() call of the package: the status of an API error (incidentcancel.go).
func isAPIStatus(x ast.Expr) bool {
	id, ok := x.(*ast.Ident)
	return ok && id.Name == apiStatusVar
}

func exprString(x ast.Expr) string {
	switch v := x.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return exprString(v.X) + "." + v.Sel.Name
	default:
		return "an expression"
	}
}

// apiStatusVar is the name of the API error's status in incidentcancel.go.
const apiStatusVar = "status"
