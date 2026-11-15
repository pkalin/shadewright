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
	"runtime"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/gitops/semdiff"
	fakegit "github.com/pkalin/shadewright/internal/providers/fake"
)

// renderTestFile is the target file of the render tests' build directory deploy.
const renderTestFile = "deploy/app.yaml"

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
	failStatus func() bool
}

func newRenderWorld(t *testing.T) *renderWorld {
	t.Helper()
	w := &renderWorld{t: t, git: fakegit.NewGit("main", map[string][]byte{
		"deploy/kustomization.yaml": []byte("resources:\n- app.yaml\n"),
		renderTestFile:              []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: app\n"),
	})}
	head, err := w.git.BranchHead(context.Background(), "main")
	if err != nil {
		t.Fatal(err)
	}
	w.head = head
	rr := renderEndsRequest("a", "uid-a", digestA)
	rr.Status.Source = &remediationv1alpha1.ResolvedSource{Path: renderTestFile, BuildPath: "deploy"}
	w.key = client.ObjectKeyFromObject(rr)
	w.c = clientfake.NewClientBuilder().WithScheme(deadlineScheme(t)).WithStatusSubresource(rr).WithObjects(rr).
		WithInterceptorFuncs(interceptor.Funcs{SubResourceUpdate: func(ctx context.Context, c client.Client,
			sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if err := ctx.Err(); err != nil {
				return err // as a real API client does
			}
			if w.failStatus != nil && w.failStatus() {
				return errors.New("etcdserver: request timed out")
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
	r := &RemediationRequestReconciler{Client: w.c, renderCheck: func(context.Context, semdiff.Input) (semdiff.Result, error) {
		w.renders++
		return semdiff.Result{}, check()
	}}
	return r.checkRender(ctx, w.git, w.head, []byte("changed"), w.stored(), nil)
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

// count returns the stored count of unfinished renders for the stored plan digest.
func (w *renderWorld) count() int32 {
	return unfinishedRenders(w.stored())
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
	w.failStatus = func() bool { return true }
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

// TestUnfinishedRendersLostClearIsRetried checks that a failed write of the render's end is an error for a
// retry, and the outcome is not used: the refusal is not returned and the mark stays.
func TestUnfinishedRendersLostClearIsRetried(t *testing.T) {
	w := newRenderWorld(t)
	writes := 0
	w.failStatus = func() bool { writes++; return writes == 2 }
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
