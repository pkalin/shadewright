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
	"runtime"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	crevent "sigs.k8s.io/controller-runtime/pkg/event"

	remediationv1alpha1 "github.com/shadewright-labs/shadewright/api/v1alpha1"
	"github.com/shadewright-labs/shadewright/internal/gitops/semdiff"
	fakegit "github.com/shadewright-labs/shadewright/internal/providers/fake"
)

// renderTestBuild is the render tests' build directory and renderTestFile its target file.
const (
	renderTestBuild = "deploy"
	renderTestFile  = renderTestBuild + "/app.yaml"
)

// newRenderTestGit returns a fake repository whose branch main holds the render tests' build directory.
func newRenderTestGit() *fakegit.Git {
	return fakegit.NewGit("main", map[string][]byte{
		renderTestBuild + "/kustomization.yaml": []byte("resources:\n- app.yaml\n"),
		renderTestFile:                          []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: app\n"),
	})
}

var (
	digestA = "sha256:" + strings.Repeat("a", 64)
	digestB = "sha256:" + strings.Repeat("b", 64)
)

// renderWorld is a request with a build directory in a fake repository and a fake API server that holds it.
// Every run of checkRender uses a new reconciler, as after a restart, and starts from the request as stored.
type renderWorld struct {
	t       *testing.T
	git     *fakegit.Git
	head    string
	c       client.Client
	key     client.ObjectKey
	renders int
	// failStatus, when set, fails status writes it returns true for.
	failStatus func(*remediationv1alpha1.RemediationRequest) bool
	// dropMark, when set, makes the API server drop status.unfinishedRenders from every status write, as one
	// whose CustomResourceDefinition predates the field does: the write succeeds without it.
	dropMark bool
	// process, when set, is the executor process of every run: the same live process. Unset, each run is
	// another process, as after a restart.
	process string
}

func newRenderWorld(t *testing.T) *renderWorld {
	t.Helper()
	w := &renderWorld{t: t, git: newRenderTestGit()}
	head, err := w.git.BranchHead(context.Background(), "main")
	if err != nil {
		t.Fatal(err)
	}
	w.head = head
	rr := renderEndsRequest("a", "uid-a", digestA)
	rr.Status.Source = &remediationv1alpha1.ResolvedSource{Path: renderTestFile, BuildPath: renderTestBuild}
	w.key = client.ObjectKeyFromObject(rr)
	w.c = clientfake.NewClientBuilder().WithScheme(deadlineScheme(t)).WithStatusSubresource(rr).WithObjects(rr).
		WithInterceptorFuncs(interceptor.Funcs{SubResourceUpdate: func(ctx context.Context, c client.Client,
			sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if err := ctx.Err(); err != nil {
				return err // as a real API client does
			}
			if w.failStatus != nil && w.failStatus(obj.(*remediationv1alpha1.RemediationRequest)) {
				return errors.New("etcdserver: request timed out")
			}
			if w.dropMark {
				obj.(*remediationv1alpha1.RemediationRequest).Status.UnfinishedRenders = nil
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		}}).Build()
	return w
}

// stored returns the request as the fake API server holds it.
func (w *renderWorld) stored() *remediationv1alpha1.RemediationRequest {
	w.t.Helper()
	rr := &remediationv1alpha1.RemediationRequest{}
	if err := w.c.Get(context.Background(), w.key, rr); err != nil {
		w.t.Fatal(err)
	}
	return rr
}

// run runs checkRender once with a new reconciler on the stored request, with check as the render.
func (w *renderWorld) run(ctx context.Context, check func() error) error {
	return w.runWith(ctx, check, nil)
}

// runWith is run with passed as the record of a pass.
func (w *renderWorld) runWith(ctx context.Context, check func() error,
	passed func(*remediationv1alpha1.RemediationRequest) bool) error {
	process := w.process
	if process == "" {
		process = newProcessID()
	}
	r := &RemediationRequestReconciler{Client: w.c, process: process,
		renderCheck: func(context.Context, semdiff.Input) (semdiff.Result, error) {
			w.renders++
			return semdiff.Result{}, check()
		}}
	return r.checkRender(ctx, w.git, w.head, []byte("changed"), w.stored(), passed)
}

// dies runs checkRender with a render that ends the goroutine, as the end of the executor's process would: no
// code after the render runs.
func (w *renderWorld) dies() {
	w.t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = w.run(context.Background(), func() error { runtime.Goexit(); return nil })
	}()
	<-done
}

// answers runs checkRender with a render that passes and returns what checkRender returned.
func (w *renderWorld) answers() error {
	return w.run(context.Background(), func() error { return nil })
}

// count returns the stored count of unfinished renders for the stored plan digest, as another process reads it.
func (w *renderWorld) count() int32 {
	return unfinishedRenders(w.stored(), "")
}

// TestUnfinishedRendersEndAtThree checks the rule of #714 across restarts: three renders in a row that took the
// executor down end the next check with RenderNeverFinished, without a fourth render.
func TestUnfinishedRendersEndAtThree(t *testing.T) {
	w := newRenderWorld(t)
	for i := range remediationv1alpha1.MaxUnfinishedRenders {
		w.dies()
		if got := w.count(); got != int32(i+1) {
			t.Fatalf("after death %d: count %d, want %d", i+1, got, i+1)
		}
	}
	err := w.answers()
	rf, ok := errors.AsType[*renderRefusal](err)
	if !ok || rf.reason != ReasonRenderNeverFinished || rf.message != messageRenderNeverFinished {
		t.Fatalf("got %v, want the RenderNeverFinished refusal", err)
	}
	if w.renders != remediationv1alpha1.MaxUnfinishedRenders {
		t.Fatalf("%d renders started, want %d", w.renders, remediationv1alpha1.MaxUnfinishedRenders)
	}
}

// TestUnfinishedRendersAnswerResets checks that a render that answers, whatever the answer, removes the mark, so
// one or two unfinished renders (an ordinary rollout or node drain) never end the request.
func TestUnfinishedRendersAnswerResets(t *testing.T) {
	for name, answer := range map[string]error{
		"pass":          nil,
		"refusal":       &semdiff.Error{Reason: semdiff.ReasonTooLarge},
		"an end":        crashed,
		"context ended": context.Canceled,
	} {
		t.Run(name, func(t *testing.T) {
			w := newRenderWorld(t)
			for range 3 {
				w.dies()
				w.dies()
				_ = w.run(context.Background(), func() error { return answer })
				if m := w.stored().Status.UnfinishedRenders; m != nil {
					t.Fatalf("the mark %+v stays after the render answered", m)
				}
			}
			if err := w.answers(); err != nil {
				t.Fatalf("got %v, want a pass", err)
			}
			if w.renders != 10 {
				t.Fatalf("%d renders started, want 10", w.renders)
			}
		})
	}
}

// TestUnfinishedRendersClearedAfterTheStepEnded checks that a render the step's context ended still removes
// the mark: the executor is alive, so the render did not take it down, and a step deadline that keeps ending
// it must never become RenderNeverFinished.
func TestUnfinishedRendersClearedAfterTheStepEnded(t *testing.T) {
	w := newRenderWorld(t)
	for range remediationv1alpha1.MaxUnfinishedRenders + 1 {
		ctx, cancel := context.WithCancel(context.Background())
		err := w.run(ctx, func() error { cancel(); return context.Canceled })
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v, want the context error", err)
		}
		if got := w.count(); got != 0 {
			t.Fatalf("count %d after a cancelled render, want 0", got)
		}
	}
}

// TestUnfinishedRendersPerPlan checks that a mark for another plan digest counts as zero.
func TestUnfinishedRendersPerPlan(t *testing.T) {
	w := newRenderWorld(t)
	for range remediationv1alpha1.MaxUnfinishedRenders {
		w.dies()
	}
	rr := w.stored()
	rr.Status.PlanDigest = digestB
	if err := w.c.Status().Update(context.Background(), rr); err != nil {
		t.Fatal(err)
	}
	w.dies()
	m := w.stored().Status.UnfinishedRenders
	if m == nil || m.PlanDigest != digestB || m.Count != 1 {
		t.Fatalf("got the mark %+v, want count 1 for the new plan", m)
	}
	if err := w.answers(); err != nil {
		t.Fatalf("got %v, want a pass", err)
	}
}

// TestUnfinishedRendersLostMarkStartsNoRender checks that a status write of the mark that fails is an error for
// a retry and the render does not start: a render without its mark could take the executor down uncounted.
func TestUnfinishedRendersLostMarkStartsNoRender(t *testing.T) {
	w := newRenderWorld(t)
	w.failStatus = func(*remediationv1alpha1.RemediationRequest) bool { return true }
	err := w.answers()
	if err == nil || !strings.Contains(err.Error(), "record the start of the render check") {
		t.Fatalf("got %v, want the failed write for a retry", err)
	}
	if _, refused := errors.AsType[*renderRefusal](err); refused {
		t.Fatalf("got the refusal %v, want an error for a retry", err)
	}
	if w.renders != 0 {
		t.Fatalf("%d renders started, want none", w.renders)
	}
}

// TestUnfinishedRendersDroppedMarkStartsNoRender checks that a status write the API server accepts while it
// drops the mark (a CustomResourceDefinition older than the executor) starts no render: without the stored
// mark a render that ends the process would never be counted.
func TestUnfinishedRendersDroppedMarkStartsNoRender(t *testing.T) {
	w := newRenderWorld(t)
	w.dropMark = true
	err := w.answers()
	if !errors.Is(err, errMarkNotStored) {
		t.Fatalf("got %v, want errMarkNotStored for a retry", err)
	}
	if _, refused := errors.AsType[*renderRefusal](err); refused {
		t.Fatalf("got the refusal %v, want an error for a retry", err)
	}
	if w.renders != 0 {
		t.Fatalf("%d renders started, want none", w.renders)
	}
}

// TestUnfinishedRendersLostClearIsRetried checks that a failed write of the render's end is an error for a
// retry, and the outcome is not used: the refusal is not returned and the mark stays.
func TestUnfinishedRendersLostClearIsRetried(t *testing.T) {
	w := newRenderWorld(t)
	writes := 0
	w.failStatus = func(*remediationv1alpha1.RemediationRequest) bool { writes++; return writes == 2 }
	err := w.run(context.Background(), func() error { return &semdiff.Error{Reason: semdiff.ReasonTooLarge} })
	if err == nil || !strings.Contains(err.Error(), "record the end of the render check") {
		t.Fatalf("got %v, want the failed write for a retry", err)
	}
	if _, refused := errors.AsType[*renderRefusal](err); refused {
		t.Fatalf("got the refusal %v, want an error for a retry", err)
	}
	if got := w.count(); got != 1 {
		t.Fatalf("count %d, want the mark of 1 kept", got)
	}
}

// TestUnfinishedRendersNeedADigest checks that a plan without a digest is refused with PlanIncomplete before
// any render: its render could not be marked.
func TestUnfinishedRendersNeedADigest(t *testing.T) {
	w := newRenderWorld(t)
	rr := w.stored()
	rr.Status.PlanDigest = ""
	if err := w.c.Status().Update(context.Background(), rr); err != nil {
		t.Fatal(err)
	}
	err := w.answers()
	if rf, ok := errors.AsType[*renderRefusal](err); !ok || rf.reason != ReasonPlanIncomplete {
		t.Fatalf("got %v, want the PlanIncomplete refusal", err)
	}
	if w.renders != 0 {
		t.Fatalf("%d renders started, want none", w.renders)
	}
}

// annotate changes an annotation of the stored request, as a person running kubectl annotate does.
func (w *renderWorld) annotate(value string) {
	w.t.Helper()
	rr := w.stored()
	if rr.Annotations == nil {
		rr.Annotations = map[string]string{}
	}
	rr.Annotations["example.com/note"] = value
	if err := w.c.Update(context.Background(), rr); err != nil {
		w.t.Fatal(err)
	}
}

// TestUnfinishedRendersAnnotatedDuringRender checks that a render that answered removes its mark even when the
// request changed while it ran (a person's kubectl annotate, so the removal's write conflicts), and records
// the pass in that write: three such renders in a row must not count 1, 2, 3 and end a healthy request.
func TestUnfinishedRendersAnnotatedDuringRender(t *testing.T) {
	for name, process := range map[string]string{"one live process": newProcessID(), "a process per render": ""} {
		t.Run(name, func(t *testing.T) {
			w := newRenderWorld(t)
			w.process = process
			for i := range remediationv1alpha1.MaxUnfinishedRenders + 1 {
				err := w.runWith(context.Background(), func() error {
					w.annotate(fmt.Sprint(i))
					return nil
				}, func(rr *remediationv1alpha1.RemediationRequest) bool {
					rr.Status.ObservedGeneration = int64(i + 1)
					return true
				})
				if err != nil {
					t.Fatalf("render %d: got %v, want a pass", i+1, err)
				}
				rr := w.stored()
				if m := rr.Status.UnfinishedRenders; m != nil {
					t.Fatalf("render %d: the mark %+v stays after the render answered", i+1, m)
				}
				if rr.Status.ObservedGeneration != int64(i+1) || rr.Annotations["example.com/note"] != fmt.Sprint(i) {
					t.Fatalf("render %d: the pass or the annotation was lost: %+v", i+1, rr)
				}
			}
		})
	}
}

// TestUnfinishedRendersChangedMarkIsNotRemoved checks that a removal after a conflict leaves a mark that is no
// longer the one the render wrote, and returns an error for a retry instead of the render's outcome.
func TestUnfinishedRendersChangedMarkIsNotRemoved(t *testing.T) {
	w := newRenderWorld(t)
	err := w.run(context.Background(), func() error {
		rr := w.stored()
		rr.Status.PlanDigest = digestB
		if err := w.c.Status().Update(context.Background(), rr); err != nil {
			t.Fatal(err)
		}
		return nil
	})
	if !errors.Is(err, errMarkChanged) {
		t.Fatalf("got %v, want errMarkChanged for a retry", err)
	}
	if m := w.stored().Status.UnfinishedRenders; m == nil || m.PlanDigest != digestA {
		t.Fatalf("got the mark %+v, want the render's mark left alone", m)
	}
}

// TestUnfinishedRendersLostRemovalOfALiveProcess checks the second line behind the retried removal: a removal
// that failed for good leaves the mark of a process that is still running, and that process's next render
// clears it instead of counting it, while a mark of a process that is gone still counts.
func TestUnfinishedRendersLostRemovalOfALiveProcess(t *testing.T) {
	w := newRenderWorld(t)
	w.dies() // a process that is gone: it counts
	w.process = newProcessID()
	w.failStatus = func(rr *remediationv1alpha1.RemediationRequest) bool { return rr.Status.UnfinishedRenders == nil }
	if err := w.answers(); err == nil || !strings.Contains(err.Error(), "record the end of the render check") {
		t.Fatalf("got %v, want the lost removal for a retry", err)
	}
	if m := w.stored().Status.UnfinishedRenders; m == nil || m.Count != 2 || m.Process != w.process {
		t.Fatalf("got the mark %+v, want count 2 of the live process", m)
	}
	var seen []int32
	for i := range remediationv1alpha1.MaxUnfinishedRenders + 1 {
		w.failStatus = func(rr *remediationv1alpha1.RemediationRequest) bool {
			// Every removal after a render is lost; the removal of the old mark before one is not.
			return rr.Status.UnfinishedRenders == nil && w.renders > i+2
		}
		err := w.run(context.Background(), func() error {
			seen = append(seen, w.stored().Status.UnfinishedRenders.Count)
			return nil
		})
		if _, refused := errors.AsType[*renderRefusal](err); refused || err == nil {
			t.Fatalf("render %d: got %v, want the lost removal for a retry", i+1, err)
		}
	}
	for i, n := range seen {
		if n != 1 {
			t.Fatalf("render %d ran with count %d, want 1: a render of the live process was counted (%v)", i+1, n, seen)
		}
	}
	w.failStatus = nil
	if err := w.answers(); err != nil {
		t.Fatalf("got %v, want a pass", err)
	}
	if m := w.stored().Status.UnfinishedRenders; m != nil {
		t.Fatalf("the mark %+v stays", m)
	}
}

// TestOnlyOwnMarksChanged checks which updates of a request the controller's event filter drops: only those
// that change status.unfinishedRenders or condition PullRequestNotClosed and nothing a reconcile reads besides.
func TestOnlyOwnMarksChanged(t *testing.T) {
	base := renderEndsRequest("a", "uid-a", digestA)
	base.ResourceVersion = "1"
	marked := base.DeepCopy()
	marked.ResourceVersion = "2"
	marked.Status.UnfinishedRenders = &remediationv1alpha1.UnfinishedRenders{PlanDigest: digestA, Count: 1,
		Process: newProcessID()}
	annotated := marked.DeepCopy()
	annotated.Annotations = map[string]string{"example.com/note": "x"}
	passed := base.DeepCopy()
	passed.Status.ObservedGeneration = 3
	notClosed := base.DeepCopy()
	notClosed.ResourceVersion = "3"
	meta.SetStatusCondition(&notClosed.Status.Conditions, metav1.Condition{
		Type: remediationv1alpha1.ConditionPullRequestNotClosed, Status: metav1.ConditionTrue, Reason: ReasonGitFailed,
		Message: "m"})
	notFound := notClosed.DeepCopy()
	meta.SetStatusCondition(&notFound.Status.Conditions, metav1.Condition{
		Type: remediationv1alpha1.ConditionPullRequestNotClosed, Status: metav1.ConditionTrue, Reason: ReasonGitNotFound,
		Message: "m"})
	notClosedAndPhase := notClosed.DeepCopy()
	notClosedAndPhase.Status.Phase = remediationv1alpha1.PhaseSuperseded
	notClosedAndOther := notClosed.DeepCopy()
	meta.SetStatusCondition(&notClosedAndOther.Status.Conditions, metav1.Condition{
		Type: remediationv1alpha1.ConditionPullRequestUnreadable, Status: metav1.ConditionTrue, Reason: ReasonGitForbidden,
		Message: "m"})
	unreadable := func(from *remediationv1alpha1.RemediationRequest, reason string) *remediationv1alpha1.RemediationRequest {
		rr := from.DeepCopy()
		rr.ResourceVersion = "4"
		meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{
			Type: remediationv1alpha1.ConditionPullRequestUnreadable, Status: metav1.ConditionTrue, Reason: reason,
			Message: "m"})
		return rr
	}
	unreadableFailed := unreadable(base, ReasonGitFailed)
	unreadableNotFound := unreadable(base, ReasonGitNotFound)
	unreadableRefused := unreadable(base, ReasonGitForbidden)
	unreadableAndPhase := unreadableFailed.DeepCopy()
	unreadableAndPhase.Status.Phase = remediationv1alpha1.PhaseSuperseded
	opened := func(from *remediationv1alpha1.RemediationRequest, reason string) *remediationv1alpha1.RemediationRequest {
		rr := from.DeepCopy()
		rr.ResourceVersion = "5"
		meta.RemoveStatusCondition(&rr.Status.Conditions, remediationv1alpha1.ConditionPullRequestNotClosed)
		meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{
			Type: remediationv1alpha1.ConditionPROpened, Status: metav1.ConditionTrue, Reason: reason, Message: "m"})
		return rr
	}
	closedRecord := opened(notClosed, ReasonPullRequestClosed)
	closedAndPhase := closedRecord.DeepCopy()
	closedAndPhase.Status.Phase = remediationv1alpha1.PhaseSuperseded
	delivered := func(from *remediationv1alpha1.RemediationRequest, status metav1.ConditionStatus,
		reason, message string) *remediationv1alpha1.RemediationRequest {
		rr := from.DeepCopy()
		rr.ResourceVersion = "6"
		meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{
			Type: remediationv1alpha1.ConditionDelivered, Status: status, Reason: reason, Message: message})
		return rr
	}
	pending := delivered(base, metav1.ConditionUnknown, ReasonDeliveryPending, "waiting")
	pendingCause := delivered(base, metav1.ConditionUnknown, ReasonDeliveryPending, "waiting; cannot be read: x")
	applied := delivered(base, metav1.ConditionTrue, ReasonMergeCommitApplied, "applied")
	pendingAndPhase := pendingCause.DeepCopy()
	pendingAndPhase.Status.Phase = remediationv1alpha1.PhaseInconclusive
	withApproved := func(from *remediationv1alpha1.RemediationRequest, reason string) *remediationv1alpha1.RemediationRequest {
		rr := from.DeepCopy()
		rr.ResourceVersion = "7"
		meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{
			Type: remediationv1alpha1.ConditionApproved, Status: metav1.ConditionFalse, Reason: reason, Message: reason})
		return rr
	}
	waitingReview := withApproved(base, ReasonWaitingForReview)
	liveNote := withApproved(base, ReasonReviewCheckLiveReadFailed)
	liveNoteAndPhase := liveNote.DeepCopy()
	liveNoteAndPhase.Status.Phase = remediationv1alpha1.PhaseSuperseded
	checkNote := base.DeepCopy()
	checkNote.ResourceVersion = "8"
	meta.SetStatusCondition(&checkNote.Status.Conditions, metav1.Condition{
		Type: remediationv1alpha1.ConditionReviewCheckNotFailed, Status: metav1.ConditionTrue, Reason: ReasonGitFailed,
		Message: "m"})
	checkNoteAndPhase := checkNote.DeepCopy()
	checkNoteAndPhase.Status.Phase = remediationv1alpha1.PhaseSuperseded
	for name, tc := range map[string]struct {
		old, new *remediationv1alpha1.RemediationRequest
		want     bool
	}{
		// #925: a failed live read before a review check write is noted on Approved, and a stopped request's failed
		// check that cannot be written or looked at in ReviewCheckNotFailed, by looks that keep their pace.
		"Approved live read note set":      {waitingReview, liveNote, true},
		"live read note on a new request":  {base, liveNote, true},
		"live read note ended":             {liveNote, waitingReview, true},
		"live read note and a phase":       {waitingReview, liveNoteAndPhase, false},
		"Approved to another reason":       {liveNote, withApproved(base, ReasonReviewCheckRefused), false},
		"ReviewCheckNotFailed set":         {base, checkNote, true},
		"ReviewCheckNotFailed removed":     {checkNote, base, true},
		"ReviewCheckNotFailed and a phase": {base, checkNoteAndPhase, false},
		// #771: deliveryReadFailed names a lasting cause in Delivered Unknown and returns the failure for the error
		// backoff, in both modes; its write must not wake the request before that delay.
		"Delivered pending set":          {base, pendingCause, true},
		"Delivered pending with a cause": {pending, pendingCause, true},
		"Delivered applied":              {pendingCause, applied, false},
		"Delivered pending and a phase":  {pending, pendingAndPhase, false},
		// #797: recordStaleClosed removes PullRequestNotClosed and records the close in one write while the end waits.
		"the close recorded with the removal": {notClosed, closedRecord, true},
		"the close recorded and a phase":      {notClosed, closedAndPhase, false},
		"the close recorded alone":            {base, opened(base, ReasonPullRequestClosed), false},
		"another PROpened with the removal":   {notClosed, opened(notClosed, ReasonChangeRequestMerged), false},
		"the mark":                            {base, marked, true},
		"its removal":                         {marked, base, true},
		"the mark and an annotation":          {base, annotated, false},
		"the removal and a pass":              {marked, passed, false},
		"no change of the mark":               {base, base, false},
		"PullRequestNotClosed set":            {base, notClosed, true},
		"its reason":                          {notClosed, notFound, true},
		"it and a phase":                      {notClosed, notClosedAndPhase, false},
		"it and the unreadable condition":     {base, notClosedAndOther, true},
		"it and the render mark":              {marked, notClosed, true},
		// #797: a read through a new installation that fails sets PullRequestUnreadable with these reasons on
		// every poll; its write, a change of reason and its removal must not wake the request before its delay.
		"PullRequestUnreadable GitFailed set":  {base, unreadableFailed, true},
		"its reason GitNotFound":               {unreadableFailed, unreadableNotFound, true},
		"the unreadable removal":               {unreadableNotFound, base, true},
		"it and the not-closed condition":      {notClosed, unreadable(notClosed, ReasonGitFailed), true},
		"unreadable and a phase":               {unreadableFailed, unreadableAndPhase, false},
		"PullRequestUnreadable with a refusal": {base, unreadableRefused, true},
		"from a refusal to GitFailed":          {unreadableRefused, unreadableFailed, true},
	} {
		if got := onlyOwnMarksChanged(tc.old, tc.new); got != tc.want {
			t.Errorf("%s: got %v, want %v", name, got, tc.want)
		}
		if got := skipOwnMarks.Update(crevent.UpdateEvent{ObjectOld: tc.old, ObjectNew: tc.new}); got == tc.want {
			t.Errorf("%s: the filter let it through: %v", name, got)
		}
	}
}
