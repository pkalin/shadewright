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
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/gitops/semdiff"
	fakegit "github.com/pkalin/shadewright/internal/providers/fake"
)

func renderEndsRequest(name, uid, digest string) *remediationv1alpha1.RemediationRequest {
	rr := &remediationv1alpha1.RemediationRequest{ObjectMeta: metav1.ObjectMeta{
		Namespace: "shadewright-system", Name: name, UID: types.UID(uid)}}
	rr.Status.PlanDigest = digest
	return rr
}

// crashed is an end of the render process that is neither an answer nor a limit refusal.
var crashed = &semdiff.RenderEnded{Cause: "crashed before it answered", Err: errors.New("exit status 2")}

// noteEnds notes n crashes of the render process for rr and fails the test if any of them is a refusal.
func noteEnds(t *testing.T, c *renderEnds, rr *remediationv1alpha1.RemediationRequest, n int) {
	t.Helper()
	for i := range n {
		if err := c.note(rr, crashed); !errors.Is(err, crashed) {
			t.Fatalf("end %d: got %v, want the end for a retry", i+1, err)
		}
	}
}

// TestRenderEndsRefuseAfterFive checks that the render check refuses a change whose render process ended
// without an answer maxRenderEnds times in a row, with TooLarge and a message naming the count and the last
// cause (#614, ADR-0035): without the count those ends are retried for as long as the pull request waits.
func TestRenderEndsRefuseAfterFive(t *testing.T) {
	var c renderEnds
	rr := renderEndsRequest("a", "uid-a", "digest-1")
	noteEnds(t, &c, rr, maxRenderEnds-1)
	err := c.note(rr, crashed)
	rf, ok := errors.AsType[*renderRefusal](err)
	if !ok || rf.reason != string(semdiff.ReasonTooLarge) {
		t.Fatalf("got %v, want a TooLarge refusal", err)
	}
	want := fmt.Sprintf("ended without an answer %d times in a row; the last time it crashed before it answered", maxRenderEnds)
	if !strings.Contains(rf.message, want) {
		t.Fatalf("got the message %q, want it to say %q", rf.message, want)
	}
	// The refusal starts the count again.
	noteEnds(t, &c, rr, maxRenderEnds-1)
}

// TestRenderEndsAnswerClears checks that an answer clears the count: four ends, then an answer (a result or a
// refusal of the check), then four more ends are all retried.
func TestRenderEndsAnswerClears(t *testing.T) {
	for name, answer := range map[string]error{
		"result":  nil,
		"refusal": &semdiff.Error{Reason: semdiff.ReasonTooLarge, Detail: "the render needs more than 128 MiB of memory"},
	} {
		t.Run(name, func(t *testing.T) {
			var c renderEnds
			rr := renderEndsRequest("a", "uid-a", "digest-1")
			noteEnds(t, &c, rr, maxRenderEnds-1)
			if err := c.note(rr, answer); err != answer { //nolint:errorlint // the same value is passed through
				t.Fatalf("got %v, want the answer %v", err, answer)
			}
			noteEnds(t, &c, rr, maxRenderEnds-1)
		})
	}
}

// TestRenderEndsCancelDoesNotCount checks that a check whose context ended neither counts nor clears: ten
// cancelled checks are retried, and they do not hide the ends before them.
func TestRenderEndsCancelDoesNotCount(t *testing.T) {
	var c renderEnds
	rr := renderEndsRequest("a", "uid-a", "digest-1")
	for range 10 {
		for _, cancelled := range []error{context.Canceled, context.DeadlineExceeded} {
			if err := c.note(rr, cancelled); !errors.Is(err, cancelled) {
				t.Fatalf("got %v, want %v for a retry", err, cancelled)
			}
		}
	}
	noteEnds(t, &c, rr, maxRenderEnds-1)
	_ = c.note(rr, context.DeadlineExceeded)
	_ = c.note(rr, errors.New("read the build: unavailable"))
	if _, ok := errors.AsType[*renderRefusal](c.note(rr, crashed)); !ok {
		t.Fatal("the fifth end after a cancelled check and a failed read was not refused")
	}
}

// TestRenderEndsPerPlan checks that the count belongs to one request and one plan: another plan digest,
// another request under the same name (another UID) and another name each start from zero, and forget drops
// the count.
func TestRenderEndsPerPlan(t *testing.T) {
	var c renderEnds
	a1 := renderEndsRequest("a", "uid-a", "digest-1")
	noteEnds(t, &c, a1, maxRenderEnds-1)
	noteEnds(t, &c, renderEndsRequest("b", "uid-b", "digest-1"), maxRenderEnds-1)
	noteEnds(t, &c, renderEndsRequest("a", "uid-a", "digest-2"), maxRenderEnds-1)
	noteEnds(t, &c, renderEndsRequest("a", "uid-a2", "digest-2"), maxRenderEnds-1)
	noteEnds(t, &c, a1, maxRenderEnds-1)
	c.forget(client.ObjectKeyFromObject(a1))
	noteEnds(t, &c, a1, maxRenderEnds-1)
}

// TestRenderEndsBounded checks that renderEnds keeps at most maxRenderEndEntries counts.
func TestRenderEndsBounded(t *testing.T) {
	var c renderEnds
	for i := range maxRenderEndEntries + 10 {
		_ = c.note(renderEndsRequest(fmt.Sprint("r", i), fmt.Sprint("uid", i), "d"), crashed)
	}
	if len(c.m) > maxRenderEndEntries {
		t.Fatalf("got %d counts, want at most %d", len(c.m), maxRenderEndEntries)
	}
}

// TestRenderEndsForgotten checks that the reconciler drops a request's count when it forgets the request (it
// ended, is being deleted or is gone).
func TestRenderEndsForgotten(t *testing.T) {
	r := &RemediationRequestReconciler{}
	rr := renderEndsRequest("a", "uid-a", "digest-1")
	noteEnds(t, &r.renderEnds, rr, 1)
	r.forget(client.ObjectKeyFromObject(rr))
	if n := len(r.renderEnds.m); n != 0 {
		t.Fatalf("got %d counts after forget, want none", n)
	}
}

// checkRenderEnds runs checkRender for rr with a render check that returns the next of outcomes on each call,
// against a build directory deploy in a fake repository, and returns what each run returned.
func checkRenderEnds(t *testing.T, rr *remediationv1alpha1.RemediationRequest, outcomes ...error) []error {
	t.Helper()
	ctx := context.Background()
	git := fakegit.NewGit("main", map[string][]byte{
		"deploy/kustomization.yaml": []byte("resources:\n- app.yaml\n"),
		"deploy/app.yaml":           []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: app\n"),
	})
	head, err := git.BranchHead(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	rr.Status.Source = &remediationv1alpha1.ResolvedSource{Path: "deploy/app.yaml", BuildPath: "deploy"}
	calls := 0
	r := &RemediationRequestReconciler{renderCheck: func(context.Context, semdiff.Input) (semdiff.Result, error) {
		calls++
		return semdiff.Result{}, outcomes[calls-1]
	}}
	got := make([]error, 0, len(outcomes))
	for range outcomes {
		got = append(got, r.checkRender(ctx, git, head, []byte("changed"), rr))
	}
	if calls != len(outcomes) {
		t.Fatalf("the render check ran %d times, want %d", calls, len(outcomes))
	}
	return got
}

// TestCheckRenderCountsRenderEnds checks the count where checkRender wires it (#614, ADR-0035): four ends of
// the render process and then an answer are retried and pass; five ends in a row, with a context error between
// them that does not count, end as the TooLarge refusal with the fixed message.
func TestCheckRenderCountsRenderEnds(t *testing.T) {
	retried := func(t *testing.T, i int, err error) {
		t.Helper()
		if _, refused := errors.AsType[*renderRefusal](err); refused || !errors.Is(err, crashed) {
			t.Fatalf("run %d: got %v, want the end for a retry", i+1, err)
		}
	}
	t.Run("passes after four ends", func(t *testing.T) {
		got := checkRenderEnds(t, renderEndsRequest("a", "uid-a", "digest-1"), crashed, crashed, crashed, crashed, nil)
		for i, err := range got[:4] {
			retried(t, i, err)
		}
		if got[4] != nil {
			t.Fatalf("run 5: got %v, want a pass", got[4])
		}
	})
	t.Run("refused at the fifth end in a row", func(t *testing.T) {
		got := checkRenderEnds(t, renderEndsRequest("a", "uid-a", "digest-1"),
			crashed, crashed, context.Canceled, crashed, crashed, crashed)
		for i, err := range got[:5] {
			if i == 2 {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("run 3: got %v, want the context error", err)
				}
				continue
			}
			retried(t, i, err)
		}
		rf, ok := errors.AsType[*renderRefusal](got[5])
		if !ok || rf.reason != string(semdiff.ReasonTooLarge) {
			t.Fatalf("run 6: got %v, want a TooLarge refusal", got[5])
		}
		want := fmt.Sprintf("The render process ended without an answer %d times in a row; the last time it crashed before it answered", maxRenderEnds)
		if rf.message != want {
			t.Fatalf("got the message %q, want %q", rf.message, want)
		}
	})
}
