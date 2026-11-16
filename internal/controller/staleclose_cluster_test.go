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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/policy/freshness"
	"github.com/pkalin/shadewright/internal/providers"
	fakegit "github.com/pkalin/shadewright/internal/providers/fake"
)

// failChainRead is a client whose reads of the RemediationRequest named name fail with err: a read of the
// supersede chain that ownsMarker makes.
type failChainRead struct {
	client.Client
	name string
	err  error
}

func (c *failChainRead) Get(ctx context.Context, key client.ObjectKey, obj client.Object,
	opts ...client.GetOption) error {
	if _, ok := obj.(*api.RemediationRequest); ok && key.Name == c.name {
		return c.err
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

// reinstalledChainWorld is a replacement ("new") that took its pull request over from "old", which took it over from
// "root" (handoffAppendedWorld with a root): both rewrites of the body were refused, so the body still carries the
// root's marker and ownsMarker reads "old" to accept it (#591). The plan is stale and the settings reach its
// repository only through a new installation of the GitHub App (#290), so the cleanup goes through it. Reads of
// "old" fail with readErr.
func reinstalledChainWorld(t *testing.T, readErr error) (*RemediationRequestReconciler, *fakegit.Git,
	*api.RemediationRequest, *record.FakeRecorder) {
	t.Helper()
	ctx := context.Background()
	r, g, p, rr, cr, head := handoffAppendedWorld(t, func(*api.RemediationRequest) {})
	if err := r.recordHandoff(ctx, rr, p, cr, head); err != nil {
		t.Fatal(err)
	}
	rr.Status.Handoff.From.PullRequestUpdate = api.HandoffStepRefused
	rr.Status.Phase = api.PhaseAwaitingApproval
	meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{Type: api.ConditionPlanFresh,
		Status: metav1.ConditionFalse, Reason: freshness.ReasonPlanChanged, Message: "the plan is stale"})
	if err := r.Status().Update(ctx, rr); err != nil {
		t.Fatal(err)
	}
	s := &api.RemediationSettings{}
	if err := r.Get(ctx, client.ObjectKey{Name: api.SettingsName}, s); err != nil {
		t.Fatal(err)
	}
	s.Spec.Repositories[0].GitHub.InstallationID = 2
	if err := r.Update(ctx, s); err != nil {
		t.Fatal(err)
	}
	if staleMark(rr) == nil || !inheritsMarker(rr) {
		t.Fatal("the world needs a stale mark and an inherited marker")
	}
	if _, reinstalled, ok := cleanupRepository(s, rr, true); !ok || !reinstalled {
		t.Fatal("the world needs a cleanup through a new installation")
	}
	r.Client = &failChainRead{Client: r.Client, name: oldName, err: readErr}
	events := record.NewFakeRecorder(10)
	r.Recorder = events
	return r, g, rr, events
}

// TestReinstalledCleanupKeepsAChainReadOutOfNotClosed (#812): a failed read of the supersede chain in ownsMarker is
// a failure on the Kubernetes side, not of the Git host: a cleanup through a new installation retries it as before,
// with no PullRequestNotClosed and no Event. A failure of the Git host at the same step still sets the condition.
func TestReinstalledCleanupKeepsAChainReadOutOfNotClosed(t *testing.T) {
	stale := &freshness.StaleError{Reason: freshness.ReasonPlanChanged, Message: "the plan changed"}
	t.Run("a failed read of the chain", func(t *testing.T) {
		readErr := apierrors.NewServiceUnavailable("the server is unavailable")
		r, g, rr, events := reinstalledChainWorld(t, readErr)
		closes := 0
		g.SetFail(func(op string) error {
			if op == opClose {
				closes++
			}
			return nil
		})
		ctx := context.Background()
		err := r.cleanUpStale(ctx, rr, stale)
		if !errors.Is(err, readErr) || !isUnknownChange(err) {
			t.Fatalf("cleanUpStale = %v, want the failed read for a retry", err)
		}
		if _, ok := errors.AsType[*reinstalledError](err); ok {
			t.Errorf("cleanUpStale = %v, want no failure of the new installation", err)
		}
		if closes != 0 {
			t.Errorf("%d closes, want none", closes)
		}
		got := &api.RemediationRequest{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(rr), got); err != nil {
			t.Fatal(err)
		}
		if c := notClosed(got); c != nil {
			t.Errorf("PullRequestNotClosed = %s: %s, want none", c.Reason, c.Message)
		}
		if got.Status.Phase != api.PhaseAwaitingApproval {
			t.Errorf("phase = %s, want AwaitingApproval", got.Status.Phase)
		}
		for len(events.Events) > 0 {
			if e := <-events.Events; strings.Contains(e, EventPullRequestNotClosed) {
				t.Errorf("Event %q, want none", e)
			}
		}
	})
	t.Run("a failed read of the Git host", func(t *testing.T) {
		r, g, rr, events := reinstalledChainWorld(t, nil)
		g.SetFail(func(op string) error {
			if op == opGet {
				return fmt.Errorf("server error: %w", providers.ErrRetryable)
			}
			return nil
		})
		ctx := context.Background()
		if err := r.cleanUpStale(ctx, rr, stale); err == nil {
			t.Fatal("cleanUpStale = nil, want the failed read for a retry")
		}
		got := &api.RemediationRequest{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(rr), got); err != nil {
			t.Fatal(err)
		}
		c := notClosed(got)
		if c == nil || c.Reason != ReasonGitFailed {
			t.Fatalf("PullRequestNotClosed = %+v, want reason %s", c, ReasonGitFailed)
		}
		if len(events.Events) != 1 {
			t.Errorf("%d Events, want one", len(events.Events))
		}
	})
}

// TestReinstalledLockLookKeepsAChainReadOutOfUnreadable (#812): the look of the target lock before the end of a
// stale plan through a new installation (releaseReinstalled) reads the supersede chain in ownsMarker too. A failed
// read there is a failure on the Kubernetes side: it is retried, with no PullRequestUnreadable GitFailed and no
// Warning Event.
func TestReinstalledLockLookKeepsAChainReadOutOfUnreadable(t *testing.T) {
	readErr := apierrors.NewServiceUnavailable("the server is unavailable")
	r, _, rr, events := reinstalledChainWorld(t, readErr)
	ctx := context.Background()
	rr.Status.HoldsTarget = new(true)
	if err := r.Status().Update(ctx, rr); err != nil {
		t.Fatal(err)
	}
	err := r.releaseReinstalled(ctx, rr)
	if !errors.Is(err, errStaleLockPending) || !errors.Is(err, readErr) {
		t.Fatalf("releaseReinstalled = %v, want the failed read tried again", err)
	}
	got := &api.RemediationRequest{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(rr), got); err != nil {
		t.Fatal(err)
	}
	if c := meta.FindStatusCondition(got.Status.Conditions, api.ConditionPullRequestUnreadable); c != nil {
		t.Errorf("PullRequestUnreadable = %s: %s, want none", c.Reason, c.Message)
	}
	if !ptr.Deref(got.Status.HoldsTarget, false) {
		t.Error("the lock was released, want it held")
	}
	for len(events.Events) > 0 {
		if e := <-events.Events; strings.HasPrefix(e, "Warning") {
			t.Errorf("Event %q, want no Warning", e)
		}
	}
}
