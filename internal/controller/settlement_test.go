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
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	api "github.com/shadewright-labs/shadewright/api/v1alpha1"
	"github.com/shadewright-labs/shadewright/internal/gitops/sourcemap"
	"github.com/shadewright-labs/shadewright/internal/policy/freshness"
	"github.com/shadewright-labs/shadewright/internal/providers"
	fakegit "github.com/shadewright-labs/shadewright/internal/providers/fake"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	pathWatch       = "watch"
	pathRecorded    = "watch-recorded"
	pathCancel      = "cancel"
	pathCancelled   = "cancelled"
	pathExpired     = "expired"
	pathStale       = "stale"
	pathExpiry      = "expiry"
	pathCreate      = "create"
	pathHandoff     = "handoff"
	pathHeld        = "held"
	pathDelete      = "delete"
	pathReview      = "review"
	clockStart      = "clock-start"
	clockClear      = "clock-clear"
	retryMark       = "retry"
	racedAnnotation = "after-the-read"
)

// whyBranchChanged is the message of a stop for a branch change in these tests.
const whyBranchChanged = "branch changed"

func settlementWorld(t *testing.T, phase api.RemediationPhase) (*RemediationRequestReconciler, *fakegit.Git, *api.RemediationRequest) {
	t.Helper()
	ctx := context.Background()
	c, rr := plannedWorld(t)
	rr.UID = "5e771e00-0000-4000-8000-000000000001"
	if err := c.Update(ctx, rr); err != nil {
		t.Fatal(err)
	}
	rr.Status.Phase = phase
	rr.Status.HoldsTarget = new(true)
	g := fakegit.NewGit(mainBranch, map[string][]byte{sourcePath: deploymentManifest(appName, rr.Spec.TargetRef.Namespace, workerContainer, "256Mi")})
	base, _ := g.BranchHead(ctx, mainBranch)
	if err := g.CreateBranch(ctx, freshOtherBranch, base); err != nil {
		t.Fatal(err)
	}
	if err := g.CreateBranch(ctx, freshness.BranchName(rr), base); err != nil {
		t.Fatal(err)
	}
	data, _ := g.ReadFile(ctx, base, sourcePath, providers.MaxFileBytes)
	content, _, err := planned(data.Data, rr)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.Push(freshness.BranchName(rr), sourcePath, content); err != nil {
		t.Fatal(err)
	}
	cr, err := g.OpenChangeRequest(ctx, providers.NewChangeRequest{HeadBranch: freshness.BranchName(rr), BaseBranch: mainBranch, Body: marker(rr)})
	if err != nil {
		t.Fatal(err)
	}
	rr.Status.PullRequest = &api.PullRequestStatus{Number: cr.Number, URL: cr.URL, HeadSHA: cr.HeadSHA}
	meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{Type: api.ConditionApproved, Status: metav1.ConditionFalse, Reason: reasonTargetBranchChanged, Message: whyBranchChanged})
	if err := c.Status().Update(ctx, rr); err != nil {
		t.Fatal(err)
	}
	settings := &api.RemediationSettings{}
	if err := c.Get(ctx, client.ObjectKey{Name: api.SettingsName}, settings); err != nil {
		t.Fatal(err)
	}
	settings.Spec.Repositories[0].TargetBranch = freshOtherBranch
	if err := c.Update(ctx, settings); err != nil {
		t.Fatal(err)
	}
	// The fake client does not serve the field selectors the limits list with; apiServerReader does.
	r := &RemediationRequestReconciler{Client: c, APIReader: &apiServerReader{Reader: c}, Scheme: c.Scheme(), Source: &stubResolver{result: resolved()}, OpenRepository: func(_ context.Context, repo api.GitRepository) (providers.GitProvider, error) {
		return g.ForTarget(string(repo.TargetBranch)), nil
	}}
	return r, g, rr
}

func TestSettlementRetargetedClosedWithAnotherOpen(t *testing.T) {
	r, g, rr := settlementWorld(t, api.PhaseNeedsIntervention)
	ctx := context.Background()
	n := rr.Status.PullRequest.Number
	if err := g.SetBase(n, freshOtherBranch); err != nil {
		t.Fatal(err)
	}
	if err := g.Close(n); err != nil {
		t.Fatal(err)
	}
	other, err := g.OpenChangeRequest(ctx, providers.NewChangeRequest{HeadBranch: freshness.BranchName(rr), BaseBranch: freshOtherBranch, Body: marker(rr)})
	if err != nil {
		t.Fatal(err)
	}
	res, err := r.checkHeld(ctx, rr)
	held := meta.FindStatusCondition(rr.Status.Conditions, api.ConditionTargetHeld)
	if err != nil || !*rr.Status.HoldsTarget || held == nil || !strings.Contains(held.Message, "#2") || other.Number != 2 || res.RequeueAfter == 0 {
		t.Fatalf("holds=%v condition=%+v result=%+v err=%v", *rr.Status.HoldsTarget, held, res, err)
	}
}

func TestSettlementCancelledReadsBothBranches(t *testing.T) {
	for _, branch := range []string{mainBranch, freshOtherBranch} {
		t.Run(branch, func(t *testing.T) {
			r, g, rr := settlementWorld(t, api.PhaseCancelled)
			ctx := context.Background()
			meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{Type: api.ConditionApproved, Status: metav1.ConditionFalse, Reason: api.ReasonIncidentGone, Message: whyIncidentGone})
			if err := r.Status().Update(ctx, rr); err != nil {
				t.Fatal(err)
			}
			if err := g.SetBase(rr.Status.PullRequest.Number, branch); err != nil {
				t.Fatal(err)
			}
			if err := g.Close(rr.Status.PullRequest.Number); err != nil {
				t.Fatal(err)
			}
			_, err := r.checkHeld(ctx, rr)
			if err != nil || *rr.Status.HoldsTarget {
				t.Fatalf("holds=%v err=%v", *rr.Status.HoldsTarget, err)
			}
		})
	}
}

func TestSettlementContentClockClears(t *testing.T) {
	testSettlementContentClock(t, opParents)
}

func testSettlementContentClock(t *testing.T, operation string) {
	t.Helper()
	r, g, rr := settlementWorld(t, api.PhaseNeedsIntervention)
	ctx := context.Background()
	if err := g.SetBase(rr.Status.PullRequest.Number, freshOtherBranch); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Merge(rr.Status.PullRequest.Number); err != nil {
		t.Fatal(err)
	}
	cr, err := g.ForTarget(freshOtherBranch).GetChangeRequest(ctx, rr.Status.PullRequest.Number)
	if err != nil {
		t.Fatal(err)
	}
	reached := false
	g.SetFail(func(op string) error { reached = reached || op == operation; return nil })
	if ref, err := retargetedContent(ctx, g, rr, cr, freshOtherBranch); err != nil || ref != nil {
		t.Fatalf("content baseline: %v %v", ref, err)
	}
	if !reached {
		t.Skip("method not used by the content read")
	}
	now := time.Now().Truncate(time.Second)
	r.Now = func() time.Time { return now }
	g.SetFail(func(op string) error {
		if op == operation {
			return errors.New("outage")
		}
		return nil
	})
	// A failed content read starts its wait where it is retried (retryGitRead), not in the read.
	failContent := func() {
		t.Helper()
		_, err := r.checkRetargeted(ctx, g, rr, cr, freshOtherBranch)
		if err == nil {
			t.Fatal("wanted a failed content read")
		}
		if _, err := r.waitOrFail(ctx, rr, err, unboundedRead(noBoundHeldLock)); err != nil {
			t.Fatal(err)
		}
	}
	failContent()
	g.SetFail(nil)
	if ref, err := r.checkRetargeted(ctx, g, rr, cr, freshOtherBranch); err != nil || ref != nil {
		t.Fatalf("recovered: %v %v", ref, err)
	}
	if contentWaitSince(rr) != nil {
		t.Fatal("successful content read kept its failure clock")
	}
	now = now.Add(2 * mergeContentWait)
	g.SetFail(func(op string) error {
		if op == operation {
			return errors.New("outage")
		}
		return nil
	})
	failContent()
	if contentWaitSince(rr) == nil || !contentWaitSince(rr).Equal(&metav1.Time{Time: now}) {
		t.Fatal("new failure reused old clock")
	}
}

// readFailureWorld sets up each reconcile entry, using the shared fake provider.
func readFailureWorld(t *testing.T, path string) (*RemediationRequestReconciler, *fakegit.Git, *api.RemediationRequest) {
	t.Helper()
	if path == pathHandoff {
		r, g, _, rr, _, _ := handoffAppendedWorld(t, nil)
		return r, g, rr
	}
	phase := api.PhaseNeedsIntervention
	switch path {
	case pathWatch, pathRecorded, pathCancel, pathReview, pathDelete:
		phase = api.PhaseAwaitingApproval
	case pathCancelled:
		phase = api.PhaseCancelled
	case pathExpired:
		phase = api.PhaseExpired
	case pathStale, pathExpiry, pathCreate:
		phase = api.PhasePolicyChecked
	}
	r, g, rr := settlementWorld(t, phase)
	ctx := context.Background()
	if path == pathCancelled {
		meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{Type: api.ConditionApproved, Status: metav1.ConditionFalse, Reason: api.ReasonIncidentGone, Message: whyIncidentGone})
	}
	switch path {
	case pathRecorded, pathCancel, pathStale, pathExpiry, pathCreate, pathReview, pathDelete:
		settings := &api.RemediationSettings{}
		if err := r.Get(ctx, client.ObjectKey{Name: api.SettingsName}, settings); err != nil {
			t.Fatal(err)
		}
		settings.Spec.Repositories[0].TargetBranch = mainBranch
		if err := r.Update(ctx, settings); err != nil {
			t.Fatal(err)
		}
	default:
		if err := g.SetBase(rr.Status.PullRequest.Number, freshOtherBranch); err != nil {
			t.Fatal(err)
		}
	}
	switch path {
	case pathWatch, pathHeld, pathRecorded:
		if _, err := g.Merge(rr.Status.PullRequest.Number); err != nil {
			t.Fatal(err)
		}
	case pathCancel:
		rr.Spec.Cancel = true
		if err := r.Update(ctx, rr); err != nil {
			t.Fatal(err)
		}
	case pathStale:
		pol := &api.RemediationPolicy{ObjectMeta: metav1.ObjectMeta{Name: string(rr.Spec.PolicyRef)}}
		if err := r.Delete(ctx, pol); err != nil {
			t.Fatal(err)
		}
	case pathExpiry:
		rr.Status.Deadline = &metav1.Time{Time: time.Now().Add(-time.Hour)}
	}
	if err := r.Status().Update(ctx, rr); err != nil {
		t.Fatal(err)
	}
	head, err := g.BranchHead(ctx, mainBranch)
	if err != nil {
		t.Fatal(err)
	}
	if path == pathReview {
		r.ReviewerHints = true
	}
	if path == pathDelete {
		if err := clientfake.AddIndex(r.Client, &api.RemediationRequest{}, clearedField, func(obj client.Object) []string {
			return []string{obj.(*api.RemediationRequest).Status.BreakerClearedBy}
		}); err != nil {
			t.Fatal(err)
		}
		rr.Finalizers = append(rr.Finalizers, FinalizerName)
		if err := r.Update(ctx, rr); err != nil {
			t.Fatal(err)
		}
		if err := r.Delete(ctx, rr); err != nil {
			t.Fatal(err)
		}
	}
	resolvedSource := resolved()
	resolvedSource.Source.BaseCommit = head
	r.Source = &stubResolver{result: resolvedSource}
	return r, g, rr
}

// Enumerate the interface, record the successful path, then fail each call of every reached read separately.
// Repeated calls (both branch listings and both content files) are independent cases.
func TestSettlementGitReadFailures(t *testing.T) {
	for _, path := range []string{pathWatch, pathHeld, pathRecorded, pathCancelled, pathCancel, pathStale, pathExpired, pathExpiry, pathHandoff, pathCreate, pathReview, pathDelete} {
		r, g, rr := readFailureWorld(t, path)
		calls := map[string]int{}
		g.SetFail(func(op string) error { calls[op]++; return nil })
		if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(rr)}); err != nil {
			t.Fatalf("baseline %s: %v", path, err)
		}
		for m := range reflect.TypeFor[providers.GitProvider]().Methods() {
			if !gitReads[m.Name] || m.Type.Out(m.Type.NumOut()-1) != reflect.TypeFor[error]() {
				continue
			}
			if path == pathWatch {
				t.Run("recovered-content/"+m.Name, func(t *testing.T) {
					t.Parallel()
					testSettlementContentClock(t, m.Name)
				})
			}
			if calls[m.Name] == 0 {
				t.Run(path+"/"+m.Name, func(t *testing.T) { t.Skip("method not used by this path") })
				continue
			}
			for nth := 1; nth <= calls[m.Name]; nth++ {
				t.Run(fmt.Sprintf("%s/%s/%d", path, m.Name, nth), func(t *testing.T) {
					// Each row builds its own world (#961).
					t.Parallel()
					r, g, rr := readFailureWorld(t, path)
					ctx := context.Background()
					count := 0
					g.SetFail(func(op string) error {
						if op == m.Name {
							count++
							if count == nth {
								return errors.New("injected read failure")
							}
						}
						return nil
					})
					res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(rr)})
					if count < nth {
						t.Fatalf("path no longer reached call %d of %s", nth, m.Name)
					}
					if getErr := r.Get(ctx, client.ObjectKeyFromObject(rr), rr); getErr != nil {
						t.Fatal(getErr)
					}
					if path == pathExpiry && err == nil && rr.Status.Phase == api.PhaseExpired {
						// Past the deadline the cleanup's failed read ends the request Expired, never a retry of the
						// expiry (fourth decision of 2026-11-18 on #771); the lock stays for checkHeld.
						if !holdsLock(rr) {
							t.Fatal("an expiry whose cleanup could not read the Git host released the target lock")
						}
						return
					}
					c := meta.FindStatusCondition(rr.Status.Conditions, api.ConditionGitReadRetry)
					// The condition names the read: by its method, or by the fixed text of the step that made it (a
					// provider error keeps no chain), never the catch-all.
					if err != nil || res.RequeueAfter < r.pullRequestPollInterval() || c == nil || !strings.HasPrefix(c.Message, "Retrying Git read: ") ||
						strings.HasSuffix(c.Message, ": read the Git host") || c.LastTransitionTime.IsZero() {
						t.Fatalf("retry=%+v err=%v condition=%+v", res, err, c)
					}
				})
			}
		}
	}
}

func TestSettlementRetryMarksDoNotWakeTheWatch(t *testing.T) {
	r, _, rr := settlementWorld(t, api.PhaseNeedsIntervention)
	ctx := context.Background()
	for _, write := range []func() error{
		func() error {
			_, err := r.retryGitRead(ctx, rr, readFailure(opParents, errors.New("outage")), unboundedRead(noBoundHeldLock))
			return err
		},
		func() error { return startContentWait(ctx, r, rr) },
		func() error {
			return r.startMergeWait(ctx, rr, providers.ChangeRequest{Number: rr.Status.PullRequest.Number,
				URL: rr.Status.PullRequest.URL, HeadSHA: rr.Status.PullRequest.HeadSHA,
				State: providers.ChangeRequestMerged}, "wait for the merge commit")
		},
		func() error { return r.clearContentWait(ctx, rr) },
		func() error { return r.dropGitWaits(ctx, rr, func(api.GitWait) bool { return true }) },
	} {
		before := rr.DeepCopy()
		if err := write(); err != nil {
			t.Fatal(err)
		}
		if !onlyOwnMarksChanged(before, rr) {
			t.Fatal("retry-only status write would shorten the returned wait")
		}
	}
}

func TestSettlementReadRecoveryClearsRetry(t *testing.T) {
	r, g, rr := settlementWorld(t, api.PhaseNeedsIntervention)
	ctx := context.Background()
	request := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(rr)}
	fail := true
	g.SetFail(func(op string) error {
		if fail && op == opGet {
			return errors.New("outage")
		}
		return nil
	})
	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(ctx, request.NamespacedName, rr); err != nil {
		t.Fatal(err)
	}
	if meta.FindStatusCondition(rr.Status.Conditions, api.ConditionGitReadRetry) == nil {
		t.Fatal("retry missing")
	}
	fail = false
	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(ctx, request.NamespacedName, rr); err != nil {
		t.Fatal(err)
	}
	if meta.FindStatusCondition(rr.Status.Conditions, api.ConditionGitReadRetry) != nil {
		t.Fatal("successful read left the retry condition")
	}
}

func TestSettlementRetryWritesSurviveConflict(t *testing.T) {
	for _, operation := range []string{clockStart, clockClear, retryMark} {
		t.Run(operation, func(t *testing.T) {
			r, _, rr := settlementWorld(t, api.PhaseNeedsIntervention)
			ctx := context.Background()
			if operation == clockClear {
				if err := startContentWait(ctx, r, rr); err != nil {
					t.Fatal(err)
				}
			}
			raced := false
			base := r.Client
			r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
				SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
					if !raced {
						raced = true
						live := &api.RemediationRequest{}
						if err := base.Get(ctx, client.ObjectKeyFromObject(rr), live); err != nil {
							return err
						}
						live.Annotations = map[string]string{"example.test/touched": racedAnnotation}
						if err := base.Update(ctx, live); err != nil {
							return err
						}
					}
					return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
				},
			})
			var err error
			switch operation {
			case clockStart:
				err = startContentWait(ctx, r, rr)
			case clockClear:
				err = r.clearContentWait(ctx, rr)
			default:
				_, err = r.retryGitRead(ctx, rr, readFailure(opParents, errors.New("outage")), unboundedRead(noBoundHeldLock))
			}
			if err != nil || !raced {
				t.Fatalf("raced=%v error=%v", raced, err)
			}
			live := &api.RemediationRequest{}
			if err := base.Get(ctx, client.ObjectKeyFromObject(rr), live); err != nil {
				t.Fatal(err)
			}
			if live.Annotations["example.test/touched"] != racedAnnotation {
				t.Fatal("concurrent annotation lost")
			}
			if operation == clockClear && contentWaitSince(live) != nil {
				t.Fatal("clear lost")
			}
			if operation == clockStart && contentWaitSince(live) == nil {
				t.Fatal("start lost")
			}
			if operation == retryMark && meta.FindStatusCondition(live.Status.Conditions, api.ConditionGitReadRetry) == nil {
				t.Fatal("retry lost")
			}
		})
	}
}

func TestSettlementClusterFailuresAreNotGitFailures(t *testing.T) {
	r, g, rr := settlementWorld(t, api.PhaseNeedsIntervention)
	ctx := context.Background()
	if err := g.SetBase(rr.Status.PullRequest.Number, freshOtherBranch); err != nil {
		t.Fatal(err)
	}
	if err := g.Close(rr.Status.PullRequest.Number); err != nil {
		t.Fatal(err)
	}
	if _, err := g.OpenChangeRequest(ctx, providers.NewChangeRequest{HeadBranch: freshness.BranchName(rr), BaseBranch: freshOtherBranch, Body: marker(rr)}); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("Kubernetes list unavailable")
	base := r.Client
	r.APIReader = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error { return failure }})
	_, err := r.checkHeld(ctx, rr)
	if !errors.Is(err, failure) {
		t.Fatalf("Kubernetes error swallowed: %v", err)
	}
	if meta.FindStatusCondition(rr.Status.Conditions, api.ConditionGitReadRetry) != nil || meta.FindStatusCondition(rr.Status.Conditions, api.ConditionPullRequestUnreadable) != nil {
		t.Fatal("Kubernetes failure reported as Git")
	}
}

// Incident cancellation has the same branch lookup with and without a recorded PR number.
func TestSettlementCancelledChangedBranchOutcomes(t *testing.T) {
	for _, branch := range []string{mainBranch, freshOtherBranch} {
		for _, state := range []providers.ChangeRequestState{providers.ChangeRequestOpen, providers.ChangeRequestMerged} {
			for _, recorded := range []bool{true, false} {
				t.Run(fmt.Sprintf("%s/%s/recorded=%v", branch, state, recorded), func(t *testing.T) {
					r, g, rr := settlementWorld(t, api.PhaseCancelled)
					ctx := context.Background()
					number := rr.Status.PullRequest.Number
					meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{Type: api.ConditionApproved, Status: metav1.ConditionFalse, Reason: api.ReasonIncidentGone, Message: whyIncidentGone})
					if !recorded {
						rr.Status.PullRequest = nil
					}
					if err := r.Status().Update(ctx, rr); err != nil {
						t.Fatal(err)
					}
					if err := g.SetBase(number, branch); err != nil {
						t.Fatal(err)
					}
					if state == providers.ChangeRequestMerged {
						if _, err := g.Merge(number); err != nil {
							t.Fatal(err)
						}
					}
					res, err := r.checkHeld(ctx, rr)
					if err != nil {
						t.Fatal(err)
					}
					if state == providers.ChangeRequestOpen {
						if !*rr.Status.HoldsTarget || res.RequeueAfter == 0 || g.ChangeRequests()[0].State != providers.ChangeRequestOpen {
							t.Fatalf("open change lost lock or was closed: %+v", rr.Status)
						}
					} else {
						if rr.Status.MergeReceipt == nil || rr.Status.MergeReceipt.TargetBranch != api.GitBranch(branch) {
							t.Fatalf("merge not followed: %+v", rr.Status)
						}
						c := meta.FindStatusCondition(rr.Status.Conditions, api.ConditionPlanFresh)
						if c == nil || c.Reason != api.ReasonIncidentGone {
							t.Fatalf("cancel reason lost: %+v", c)
						}
					}
				})
			}
		}
	}
}

func TestEveryGitReadReportsItsBoundaryFailure(t *testing.T) {
	for m := range reflect.TypeFor[providers.GitProvider]().Methods() {
		if !gitReads[m.Name] || m.Type.Out(m.Type.NumOut()-1) != reflect.TypeFor[error]() {
			continue
		}
		t.Run(m.Name, func(t *testing.T) {
			g := fakegit.NewGit(mainBranch, nil)
			g.SetFail(func(string) error { return errors.New("private provider error") })
			ctx := context.Background()
			wrapped := &gatedGit{git: g, allow: func(context.Context) error { return nil }}
			args := []reflect.Value{}
			for in := range m.Type.Ins() {
				if in == reflect.TypeFor[context.Context]() {
					args = append(args, reflect.ValueOf(ctx))
				} else {
					args = append(args, reflect.Zero(in))
				}
			}
			out := reflect.ValueOf(wrapped).MethodByName(m.Name).Call(args)
			err, _ := reflect.TypeAssert[error](out[len(out)-1])
			if !gitReadFailure(err) || !strings.Contains(err.Error(), m.Name) {
				t.Fatalf("read bypassed retry boundary: %v", err)
			}
		})
	}
}

func TestSettlementCancelledRetargetedForeignHead(t *testing.T) {
	r, g, rr := settlementWorld(t, api.PhaseCancelled)
	ctx := context.Background()
	meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{Type: api.ConditionApproved, Status: metav1.ConditionFalse, Reason: api.ReasonIncidentGone, Message: whyIncidentGone})
	if err := r.Status().Update(ctx, rr); err != nil {
		t.Fatal(err)
	}
	if err := g.SetBase(rr.Status.PullRequest.Number, freshOtherBranch); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Push(freshness.BranchName(rr), sourcePath, []byte("unapproved: change\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Merge(rr.Status.PullRequest.Number); err != nil {
		t.Fatal(err)
	}
	if _, err := r.checkHeld(ctx, rr); err != nil {
		t.Fatal(err)
	}
	if rr.Status.MergeReceipt != nil {
		t.Fatal("foreign head received a merge receipt")
	}
	c := meta.FindStatusCondition(rr.Status.Conditions, api.ConditionApproved)
	if c == nil || c.Reason != ReasonMergedWithForeignChange || !strings.Contains(c.Message, freshOtherBranch) {
		t.Fatalf("foreign merge lost its actual branch: %+v", c)
	}
}

func TestSettlementSourceMappingFailureOrigin(t *testing.T) {
	for _, tc := range []struct {
		op  string
		git bool
	}{
		{"read apps/orders/deployment.yaml", true}, {"list directory apps/orders", true},
		{"open repository gitops", true}, {"get Deployment default/orders", false},
		{"Deployment default/orders changed while the source was mapped", false},
	} {
		t.Run(tc.op, func(t *testing.T) {
			err := &sourcemap.TransientError{Op: tc.op}
			if gitReadFailure(err) != tc.git {
				t.Fatal("source mapping failure was attributed to the wrong service")
			}
		})
	}
}

func TestSettlementGitWritesKeepTheirErrorPath(t *testing.T) {
	for _, operation := range []string{"open the pull request", "create the branch", "commit the change", "close the pull request"} {
		t.Run(operation, func(t *testing.T) {
			r, _, rr := settlementWorld(t, api.PhasePolicyChecked)
			failure := freshness.WrapProvider(operation, errors.New("write failed"))
			result, err := r.waitOrFail(context.Background(), rr, failure, requestBound(rr))
			if err == nil || result.RequeueAfter != 0 || meta.FindStatusCondition(rr.Status.Conditions, api.ConditionGitReadRetry) != nil {
				t.Fatalf("write was labelled as a read: %+v %v", result, err)
			}
		})
	}
}

// TestReconcileReadsForTheRetryClearOnlyWhenShown checks that a reconcile reads the request again to remove
// GitReadRetry only when the request it read showed the condition: every reconcile of a request without it made
// that extra read before, which slowed each reconcile by an API round trip.
func TestReconcileReadsForTheRetryClearOnlyWhenShown(t *testing.T) {
	for _, shown := range []bool{false, true} {
		t.Run(fmt.Sprintf("shown=%v", shown), func(t *testing.T) {
			r, _, rr := settlementWorld(t, api.PhaseNeedsIntervention)
			ctx := context.Background()
			if shown {
				meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{Type: api.ConditionGitReadRetry,
					Status: metav1.ConditionTrue, Reason: api.ReasonGitReadFailed, Message: "an earlier read failed"})
				if err := r.Status().Update(ctx, rr); err != nil {
					t.Fatal(err)
				}
			}
			clearReads := 0
			base := r.Client.(client.WithWatch)
			r.Client = interceptor.NewClient(base, interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch,
				key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*api.RemediationRequest); ok && calledFrom("clearGitReadRetry") {
					clearReads++
				}
				return c.Get(ctx, key, obj, opts...)
			}})
			request := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(rr)}
			if _, err := r.Reconcile(ctx, request); err != nil {
				t.Fatal(err)
			}
			want := 0
			if shown {
				want = 1
			}
			if clearReads != want {
				t.Fatalf("reads of the request to clear the retry = %d, want %d", clearReads, want)
			}
			if err := base.Get(ctx, request.NamespacedName, rr); err != nil {
				t.Fatal(err)
			}
			if meta.FindStatusCondition(rr.Status.Conditions, api.ConditionGitReadRetry) != nil {
				t.Fatal("a reconcile without a failed read left the retry condition")
			}
		})
	}
}

// calledFrom reports whether a function whose name ends in name is on the caller's stack.
func calledFrom(name string) bool {
	pcs := make([]uintptr, 64)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(2, pcs)])
	for {
		f, more := frames.Next()
		if strings.HasSuffix(f.Function, "."+name) {
			return true
		}
		if !more {
			return false
		}
	}
}

// #827: the pull request was opened but the status write that recorded it was lost, and the request then stopped
// for a branch change. The read that settles the lock finds the pull request merged, on the configured branch or on
// the recorded one: the merge is recorded, with its receipt, before the lock goes.
func TestSettlementBranchChangeAdoptsUnrecordedMerge(t *testing.T) {
	for _, branch := range []string{mainBranch, freshOtherBranch} {
		t.Run(branch, func(t *testing.T) {
			r, g, rr := settlementWorld(t, api.PhaseNeedsIntervention)
			ctx := context.Background()
			number := rr.Status.PullRequest.Number
			rr.Status.PullRequest = nil
			meta.RemoveStatusCondition(&rr.Status.Conditions, api.ConditionApproved)
			meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{Type: api.ConditionPlanFresh,
				Status: metav1.ConditionFalse, Reason: reasonTargetBranchChanged, Message: whyBranchChanged})
			if err := r.Status().Update(ctx, rr); err != nil {
				t.Fatal(err)
			}
			if err := g.SetBase(number, branch); err != nil {
				t.Fatal(err)
			}
			if _, err := g.Merge(number); err != nil {
				t.Fatal(err)
			}
			for range 3 {
				if _, err := r.checkHeld(ctx, rr); err != nil {
					t.Fatal(err)
				}
				if rr.Status.MergeReceipt == nil && !*rr.Status.HoldsTarget {
					t.Fatalf("the lock went before the merge was recorded: phase %s", rr.Status.Phase)
				}
			}
			if rr.Status.MergeReceipt == nil || rr.Status.MergeReceipt.TargetBranch != api.GitBranch(branch) {
				t.Fatalf("merge not recorded: phase %s, receipt %+v", rr.Status.Phase, rr.Status.MergeReceipt)
			}
		})
	}
}

// laggingGit answers every read of a pull request by its number with a 404 and lists no pull request of the
// branch: the host has not caught up with a pull request that may well be open or merged.
type laggingGit struct{ providers.GitProvider }

func (laggingGit) GetChangeRequest(context.Context, int64) (providers.ChangeRequest, error) {
	return providers.ChangeRequest{}, fmt.Errorf("pull request: %w", providers.ErrNotFound)
}

func (laggingGit) ListChangeRequests(context.Context, string, bool) ([]providers.ChangeRequest, error) {
	return nil, nil
}

// #827: after a branch change, one 404 on both branches that the listing of the branch does not confirm yet is no
// proof that the pull request is gone: the read on the recorded branch keeps its first look (settleGone) and the
// lock stays until a look confirmWait later finds it missing again.
func TestSettlementBranchChangeUnconfirmedGoneKeepsLock(t *testing.T) {
	r, g, rr := settlementWorld(t, api.PhaseNeedsIntervention)
	ctx := context.Background()
	r.OpenRepository = func(_ context.Context, repo api.GitRepository) (providers.GitProvider, error) {
		return laggingGit{g.ForTarget(string(repo.TargetBranch))}, nil
	}
	if _, err := r.checkHeld(ctx, rr); err != nil {
		t.Fatal(err)
	}
	if !*rr.Status.HoldsTarget {
		t.Fatal("one unconfirmed 404 on both branches released the lock")
	}
}

// #827: the request is gone from the API server (its finalizer removed) while the cache still shows it with
// condition GitReadRetry: clearing the condition has nothing left to clear, and is no error for the backoff.
func TestGitReadRetryClearIgnoresARequestGone(t *testing.T) {
	r, _, rr := settlementWorld(t, api.PhaseNeedsIntervention)
	ctx := context.Background()
	meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{Type: api.ConditionGitReadRetry,
		Status: metav1.ConditionTrue, Reason: api.ReasonGitReadFailed, Message: "Retrying Git read: x"})
	if err := r.Status().Update(ctx, rr); err != nil {
		t.Fatal(err)
	}
	r.APIReader = clientfake.NewClientBuilder().WithScheme(r.Scheme).Build()
	if _, err := r.clearGitReadRetry(ctx, client.ObjectKeyFromObject(rr), true); err != nil {
		t.Fatalf("clearGitReadRetry() = %v; want nil for a request gone from the API server", err)
	}
}

// #827: the same adoption with a head that is not the planned change alone (a person pushed to the branch) merged
// into the recorded branch: no receipt, MergedWithForeignChange.
func TestSettlementBranchChangeAdoptedForeignHead(t *testing.T) {
	r, g, rr := settlementWorld(t, api.PhaseNeedsIntervention)
	ctx := context.Background()
	number := rr.Status.PullRequest.Number
	rr.Status.PullRequest = nil
	meta.RemoveStatusCondition(&rr.Status.Conditions, api.ConditionApproved)
	meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{Type: api.ConditionPlanFresh,
		Status: metav1.ConditionFalse, Reason: reasonTargetBranchChanged, Message: whyBranchChanged})
	if err := r.Status().Update(ctx, rr); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Push(freshness.BranchName(rr), sourcePath, []byte("unapproved: change\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Merge(number); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := r.checkHeld(ctx, rr); err != nil {
			t.Fatal(err)
		}
	}
	if rr.Status.MergeReceipt != nil {
		t.Fatal("a foreign head adopted after a lost write received a merge receipt")
	}
	if c := meta.FindStatusCondition(rr.Status.Conditions, api.ConditionApproved); c == nil ||
		c.Reason != ReasonMergedWithForeignChange {
		t.Fatalf("Approved = %+v; want MergedWithForeignChange", c)
	}
}

// The eleventh decision of 2026-11-18 on #771, point 2: a request that ended MergeNotCheckable on its retargeted
// merge did not show what the merge delivered. Another pull request of its branch is open on the branch the settings
// name now: the lock is kept, reconcile after reconcile, and named in TargetHeld. Once that pull request is closed, a
// read of the branch releases it.
func TestSettlementMergeNotCheckableWithAnotherOpen(t *testing.T) {
	r, g, rr := settlementWorld(t, api.PhaseNeedsIntervention)
	ctx := context.Background()
	n := rr.Status.PullRequest.Number
	if err := g.SetBase(n, freshOtherBranch); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Merge(n); err != nil {
		t.Fatal(err)
	}
	meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{Type: api.ConditionPROpened,
		Status: metav1.ConditionFalse, Reason: ReasonChangeRequestMerged, Message: "merged by a person"})
	meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{Type: api.ConditionApproved,
		Status: metav1.ConditionFalse, Reason: ReasonMergeNotCheckable, Message: "not checkable"})
	if err := r.Status().Update(ctx, rr); err != nil {
		t.Fatal(err)
	}
	other, err := g.OpenChangeRequest(ctx, providers.NewChangeRequest{HeadBranch: freshness.BranchName(rr),
		BaseBranch: freshOtherBranch, Body: marker(rr)})
	if err != nil {
		t.Fatal(err)
	}
	key := client.ObjectKeyFromObject(rr)
	live := &api.RemediationRequest{}
	for i := range 3 {
		res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		if err != nil {
			t.Fatalf("reconcile %d: %v", i+1, err)
		}
		if err := r.Get(ctx, key, live); err != nil {
			t.Fatal(err)
		}
		held := meta.FindStatusCondition(live.Status.Conditions, api.ConditionTargetHeld)
		switch {
		case live.Status.HoldsTarget == nil || !*live.Status.HoldsTarget:
			t.Fatalf("reconcile %d: the lock was released while #%d is open on the branch (phase %s)", i+1,
				other.Number, live.Status.Phase)
		case held == nil || !strings.Contains(held.Message, fmt.Sprintf("#%d", other.Number)):
			t.Fatalf("reconcile %d: TargetHeld %+v does not name #%d", i+1, held, other.Number)
		case res.RequeueAfter <= 0:
			t.Fatalf("reconcile %d: the kept lock asks for no time to run again", i+1)
		}
	}
	if err := g.Close(other.Number); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(ctx, key, live); err != nil {
		t.Fatal(err)
	}
	if live.Status.HoldsTarget != nil && *live.Status.HoldsTarget {
		t.Fatalf("the lock is held after the other pull request was closed (phase %s)", live.Status.Phase)
	}
}

// The eleventh decision of 2026-11-18 on #771, point 3: a settings object that is missing is not "the repository is
// gone from the settings". A request that ended on a merge that did not deliver the plan as shown keeps its lock while
// RemediationSettings does not exist, says so in PullRequestUnreadable and comes back at a time; only a repository
// absent from settings that exist releases it without a read of the branch.
func TestSettlementUndeliveredKeepsTheLockWithoutSettings(t *testing.T) {
	for _, gone := range []string{"settings object missing", "repository removed"} {
		t.Run(gone, func(t *testing.T) {
			r, g, rr := settlementWorld(t, api.PhaseNeedsIntervention)
			ctx := context.Background()
			n := rr.Status.PullRequest.Number
			if err := g.SetBase(n, freshOtherBranch); err != nil {
				t.Fatal(err)
			}
			if _, err := g.Merge(n); err != nil {
				t.Fatal(err)
			}
			meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{Type: api.ConditionApproved,
				Status: metav1.ConditionFalse, Reason: ReasonMergeContentUnreadable, Message: "unreadable"})
			if err := r.Status().Update(ctx, rr); err != nil {
				t.Fatal(err)
			}
			settings := &api.RemediationSettings{}
			if err := r.Get(ctx, client.ObjectKey{Name: api.SettingsName}, settings); err != nil {
				t.Fatal(err)
			}
			if gone == "settings object missing" {
				if err := r.Delete(ctx, settings); err != nil {
					t.Fatal(err)
				}
			} else {
				settings.Spec.Repositories[0].URL += "-elsewhere"
				if err := r.Update(ctx, settings); err != nil {
					t.Fatal(err)
				}
			}
			key := client.ObjectKeyFromObject(rr)
			live := &api.RemediationRequest{}
			res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			if err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			if err := r.Get(ctx, key, live); err != nil {
				t.Fatal(err)
			}
			holds := live.Status.HoldsTarget != nil && *live.Status.HoldsTarget
			unreadable := meta.FindStatusCondition(live.Status.Conditions, api.ConditionPullRequestUnreadable)
			if gone == "repository removed" {
				if holds {
					t.Fatalf("the repository left settings that exist and the lock is held (phase %s)", live.Status.Phase)
				}
				return
			}
			switch {
			case !holds:
				t.Fatalf("the lock was released while RemediationSettings does not exist (phase %s)", live.Status.Phase)
			case unreadable == nil || unreadable.Reason != ReasonSettingsNotFound:
				t.Fatalf("PullRequestUnreadable %+v; want reason %s", unreadable, ReasonSettingsNotFound)
			case res.RequeueAfter <= 0:
				t.Fatalf("the kept lock asks for no time to run again")
			}
		})
	}
}
