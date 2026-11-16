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
	"net"
	"reflect"
	"slices"
	"sync"
	"syscall"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	api "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/providers"
	fakegit "github.com/pkalin/shadewright/internal/providers/fake"
)

// The wait of a recovery sorted by where an error comes from (#786, decision of 2026-11-16: sort by where the error
// comes from, and let nothing unknown end a waiting recovery). One table crosses every call of the recovery to the
// Kubernetes API and to the Git host, in each of its paths, with each kind of failure, and checks the outcome the
// rule gives. The calls of each path are listed by hand: a call the recovery makes that the list lacks, or a listed
// call it no longer makes, fails the test, so a new call site gets its row.

// failureKind is a kind of failure the table injects at one call.
type failureKind string

const (
	kindStatus       failureKind = "status error"
	kindForbidden    failureKind = "refusal"
	kindConflict     failureKind = "conflict"
	kindTransport    failureKind = "transport error"
	kindStepDeadline failureKind = "step deadline"
	kindCancel       failureKind = "caller's cancellation"
	kindJoined       failureKind = "joined errors"
)

var failureKinds = []failureKind{kindStatus, kindForbidden, kindConflict, kindTransport, kindStepDeadline, kindCancel,
	kindJoined}

// waitStepTimeout is the recovery's own deadline in the table: long enough for a whole look at the fakes, short
// enough for the rows that run into it.
const waitStepTimeout = 300 * time.Millisecond

// transportError is a failure to reach a server at all: no status, no class of the Git host.
func transportError() error {
	return &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
}

// injected is the failure of kind at a call to the Kubernetes API (cluster) or the Git host. The step deadline waits
// for ctx to end; the caller's cancellation cancels the reconcile's context first.
func injected(ctx context.Context, cluster bool, kind failureKind, cancel context.CancelFunc) error {
	gr := api.GroupVersion.WithResource(resourceRemediationRequests).GroupResource()
	switch kind {
	case kindStatus:
		if cluster {
			return apierrors.NewServiceUnavailable("the API server is overloaded")
		}
		return &providers.RetryableError{Err: errors.New("status 503")}
	case kindForbidden:
		if cluster {
			return apierrors.NewForbidden(gr, oldName, errors.New("RBAC: access denied"))
		}
		return fmt.Errorf("status 403: %w", providers.ErrForbidden)
	case kindConflict:
		if cluster {
			return apierrors.NewConflict(gr, oldName, errors.New("it changed since it was read"))
		}
		return fmt.Errorf("status 409: %w", providers.ErrConflict)
	case kindTransport:
		return transportError()
	case kindStepDeadline:
		if _, ok := ctx.Deadline(); !ok {
			// A call outside the step's own deadline (the recovery's write after the step): a client-side timeout.
			return context.DeadlineExceeded
		}
		<-ctx.Done()
		return ctx.Err()
	case kindCancel:
		cancel()
		return context.Canceled
	case kindJoined:
		if cluster {
			return errors.Join(transportError(), apierrors.NewConflict(gr, oldName, errors.New("changed")))
		}
		return errors.Join(transportError(), &providers.RetryableError{Err: errors.New("status 502")})
	}
	panic("unknown kind " + kind)
}

// callLog records the calls of one look, each as "<side> <call>#<n>" (n counts the same call), and injects the
// failure at the call named target.
type callLog struct {
	mu     sync.Mutex
	calls  []string
	seen   map[string]int
	target string
	kind   failureKind
	cancel context.CancelFunc
}

func (l *callLog) call(ctx context.Context, cluster bool, name string) error {
	side := "git"
	if cluster {
		side = "k8s"
	}
	l.mu.Lock()
	if l.seen == nil {
		l.seen = map[string]int{}
	}
	key := side + " " + name
	l.seen[key]++
	key = fmt.Sprintf("%s#%d", key, l.seen[key])
	l.calls = append(l.calls, key)
	hit := key == l.target
	l.mu.Unlock()
	if !hit {
		return nil
	}
	return injected(ctx, cluster, l.kind, l.cancel)
}

func (l *callLog) recorded() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.calls)
}

// objName is the kind and name of obj, for a call key.
func objName(obj client.Object) string {
	return reflect.TypeOf(obj).Elem().Name() + "/" + obj.GetName()
}

// logClient wraps c so that every call to the Kubernetes API goes through l.
func logClient(c client.WithWatch, l *callLog) client.WithWatch {
	return interceptor.NewClient(c, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			obj.SetName(key.Name)
			if err := l.call(ctx, true, "get "+objName(obj)); err != nil {
				return err
			}
			return c.Get(ctx, key, obj, opts...)
		},
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if err := l.call(ctx, true, "list "+reflect.TypeOf(list).Elem().Name()); err != nil {
				return err
			}
			return c.List(ctx, list, opts...)
		},
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if err := l.call(ctx, true, "create "+objName(obj)); err != nil {
				return err
			}
			return c.Create(ctx, obj, opts...)
		},
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if err := l.call(ctx, true, "update "+objName(obj)); err != nil {
				return err
			}
			return c.Update(ctx, obj, opts...)
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if err := l.call(ctx, true, "patch "+objName(obj)); err != nil {
				return err
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if err := l.call(ctx, true, "delete "+objName(obj)); err != nil {
				return err
			}
			return c.Delete(ctx, obj, opts...)
		},
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if err := l.call(ctx, true, "update "+sub+" "+objName(obj)); err != nil {
				return err
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
		SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			if err := l.call(ctx, true, "patch "+sub+" "+objName(obj)); err != nil {
				return err
			}
			return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
		},
	})
}

// logGit is a Git host whose every call goes through l before it reaches the fake. It implements every method of
// providers.GitProvider itself, embedding nothing, so a method added to the interface breaks the build here until
// it gets its line.
type logGit struct {
	l *callLog
	g providers.GitProvider
}

var _ providers.GitProvider = (*logGit)(nil)

func (w *logGit) Capabilities() providers.GitCapabilities { return w.g.Capabilities() }

func (w *logGit) ReadFile(ctx context.Context, commit, path string, limit int) (providers.File, error) {
	if err := w.l.call(ctx, false, "ReadFile"); err != nil {
		return providers.File{}, err
	}
	return w.g.ReadFile(ctx, commit, path, limit)
}

func (w *logGit) ListDir(ctx context.Context, commit, path string) ([]providers.DirEntry, error) {
	if err := w.l.call(ctx, false, "ListDir"); err != nil {
		return nil, err
	}
	return w.g.ListDir(ctx, commit, path)
}

func (w *logGit) BranchHead(ctx context.Context, branch string) (string, error) {
	if err := w.l.call(ctx, false, "BranchHead"); err != nil {
		return "", err
	}
	return w.g.BranchHead(ctx, branch)
}

func (w *logGit) CreateBranch(ctx context.Context, branch, fromCommit string) error {
	if err := w.l.call(ctx, false, "CreateBranch"); err != nil {
		return err
	}
	return w.g.CreateBranch(ctx, branch, fromCommit)
}

func (w *logGit) CommitFile(ctx context.Context, branch, expectedHead string, change providers.FileChange) (string, error) {
	if err := w.l.call(ctx, false, "CommitFile"); err != nil {
		return "", err
	}
	return w.g.CommitFile(ctx, branch, expectedHead, change)
}

func (w *logGit) AppendCommit(ctx context.Context, branch, expectedHead string, change providers.FileChange) (string, error) {
	if err := w.l.call(ctx, false, "AppendCommit"); err != nil {
		return "", err
	}
	return w.g.AppendCommit(ctx, branch, expectedHead, change)
}

func (w *logGit) CheckBranch(ctx context.Context, commit, path string) (providers.BranchState, error) {
	if err := w.l.call(ctx, false, "CheckBranch"); err != nil {
		return 0, err
	}
	return w.g.CheckBranch(ctx, commit, path)
}

func (w *logGit) CheckChain(ctx context.Context, commit, path string, commits int) (providers.BranchState, error) {
	if err := w.l.call(ctx, false, "CheckChain"); err != nil {
		return 0, err
	}
	return w.g.CheckChain(ctx, commit, path, commits)
}

func (w *logGit) Parents(ctx context.Context, commit string) ([]string, error) {
	if err := w.l.call(ctx, false, "Parents"); err != nil {
		return nil, err
	}
	return w.g.Parents(ctx, commit)
}

func (w *logGit) CheckCommit(ctx context.Context, commit, path string) (providers.BranchState, string, error) {
	if err := w.l.call(ctx, false, "CheckCommit"); err != nil {
		return 0, "", err
	}
	return w.g.CheckCommit(ctx, commit, path)
}

func (w *logGit) CheckMerged(ctx context.Context, head, mergeCommit, path string) (providers.BranchState, string, error) {
	if err := w.l.call(ctx, false, "CheckMerged"); err != nil {
		return 0, "", err
	}
	return w.g.CheckMerged(ctx, head, mergeCommit, path)
}

func (w *logGit) CommentChangeRequest(ctx context.Context, number int64, marker, body string) error {
	if err := w.l.call(ctx, false, "CommentChangeRequest"); err != nil {
		return err
	}
	return w.g.CommentChangeRequest(ctx, number, marker, body)
}

func (w *logGit) OpenChangeRequest(ctx context.Context, req providers.NewChangeRequest) (providers.ChangeRequest, error) {
	if err := w.l.call(ctx, false, "OpenChangeRequest"); err != nil {
		return providers.ChangeRequest{}, err
	}
	return w.g.OpenChangeRequest(ctx, req)
}

func (w *logGit) FindChangeRequest(ctx context.Context, headBranch string) (providers.ChangeRequest, error) {
	if err := w.l.call(ctx, false, "FindChangeRequest"); err != nil {
		return providers.ChangeRequest{}, err
	}
	return w.g.FindChangeRequest(ctx, headBranch)
}

func (w *logGit) ListChangeRequests(ctx context.Context, headBranch string, openOnly bool) ([]providers.ChangeRequest, error) {
	if err := w.l.call(ctx, false, fmt.Sprintf("ListChangeRequests(open=%v)", openOnly)); err != nil {
		return nil, err
	}
	return w.g.ListChangeRequests(ctx, headBranch, openOnly)
}

func (w *logGit) GetChangeRequest(ctx context.Context, number int64) (providers.ChangeRequest, error) {
	if err := w.l.call(ctx, false, "GetChangeRequest"); err != nil {
		return providers.ChangeRequest{}, err
	}
	return w.g.GetChangeRequest(ctx, number)
}

func (w *logGit) EnableAutoMerge(ctx context.Context, number int64, expectedHeadSHA string) error {
	if err := w.l.call(ctx, false, "EnableAutoMerge"); err != nil {
		return err
	}
	return w.g.EnableAutoMerge(ctx, number, expectedHeadSHA)
}

func (w *logGit) CloseChangeRequest(ctx context.Context, number int64) error {
	if err := w.l.call(ctx, false, "CloseChangeRequest"); err != nil {
		return err
	}
	return w.g.CloseChangeRequest(ctx, number)
}

func (w *logGit) UpdateChangeRequest(ctx context.Context, number int64, title, body string) error {
	if err := w.l.call(ctx, false, "UpdateChangeRequest"); err != nil {
		return err
	}
	return w.g.UpdateChangeRequest(ctx, number, title, body)
}

func (w *logGit) CreateCheckRun(ctx context.Context, head string, run providers.CheckRun) error {
	if err := w.l.call(ctx, false, "CreateCheckRun"); err != nil {
		return err
	}
	return w.g.CreateCheckRun(ctx, head, run)
}

func (w *logGit) ReviewCheckRun(ctx context.Context, head string) (providers.CheckRunStatus, bool, error) {
	if err := w.l.call(ctx, false, "ReviewCheckRun"); err != nil {
		return "", false, err
	}
	return w.g.ReviewCheckRun(ctx, head)
}

// recoveryPath is one path of the recovery from a look that works: arrange sets it up on a recovery that waits on a
// refused read of the branch, calls lists every call that look makes, and ended tells the look that worked.
type recoveryPath struct {
	name    string
	arrange func(t *testing.T, r *RemediationRequestReconciler, g *fakegit.Git, p *api.RemediationRequest,
		cr providers.ChangeRequest)
	calls []string
	ended func(rr *api.RemediationRequest) bool
}

// resourceRemediationRequests is the resource of a RemediationRequest, for the API errors the table injects.
const resourceRemediationRequests = "remediationrequests"

// The calls of the Git host that more than one path makes.
const (
	callOpen       = "git open#1"
	callBranchHead = "git BranchHead#1"
	callParents    = "git Parents#1"
	callCheckChain = "git CheckChain#1"
	callReadFile1  = "git ReadFile#1"
	callReadFile2  = "git ReadFile#2"
	callGetChange1 = "git GetChangeRequest#1"
)

var recoveryPaths = []recoveryPath{
	{
		name: "takeover of the open pull request",
		arrange: func(*testing.T, *RemediationRequestReconciler, *fakegit.Git, *api.RemediationRequest, providers.ChangeRequest) {
		},
		calls: []string{
			"k8s get RemediationRequest/" + oldName + "#1",
			"k8s get RemediationSettings/" + api.SettingsName + "#1",
			callOpen,
			callBranchHead,
			callParents,
			callCheckChain,
			callReadFile1,
			callReadFile2,
			callGetChange1,
			"k8s get RemediationSettings/" + api.SettingsName + "#2",
			"k8s update status RemediationRequest/" + newName + "#1",
		},
		ended: func(rr *api.RemediationRequest) bool { return rr.Status.Phase == api.PhasePRCreated },
	},
	{
		name: "pull request closed around the handoff",
		arrange: func(t *testing.T, _ *RemediationRequestReconciler, g *fakegit.Git, _ *api.RemediationRequest,
			cr providers.ChangeRequest) {
			if err := g.CloseChangeRequest(context.Background(), cr.Number); err != nil {
				t.Fatal(err)
			}
		},
		calls: []string{
			"k8s get RemediationRequest/" + oldName + "#1",
			"k8s get RemediationSettings/" + api.SettingsName + "#1",
			callOpen,
			callBranchHead,
			callParents,
			callCheckChain,
			callReadFile1,
			callReadFile2,
			callGetChange1,
			"git ListChangeRequests(open=true)#1",
			"k8s get RemediationSettings/" + api.SettingsName + "#2",
			"k8s update status RemediationRequest/" + newName + "#1",
		},
		ended: func(rr *api.RemediationRequest) bool { return rr.Status.Phase == api.PhaseCancelled },
	},
	{
		name: "branch answers 404 and the pull request is closed",
		arrange: func(t *testing.T, _ *RemediationRequestReconciler, g *fakegit.Git, _ *api.RemediationRequest,
			cr providers.ChangeRequest) {
			if err := g.CloseChangeRequest(context.Background(), cr.Number); err != nil {
				t.Fatal(err)
			}
			g.SetFail(func(op string) error {
				if op == opBranchHead {
					return fmt.Errorf("status 404: %w", providers.ErrNotFound)
				}
				return nil
			})
		},
		calls: []string{
			"k8s get RemediationRequest/" + oldName + "#1",
			"k8s get RemediationSettings/" + api.SettingsName + "#1",
			callOpen,
			callBranchHead,
			callGetChange1,
			"git ListChangeRequests(open=true)#1",
			callParents,
			callCheckChain,
			callReadFile1,
			callReadFile2,
			"git GetChangeRequest#2",
			"git ListChangeRequests(open=true)#2",
			"k8s get RemediationSettings/" + api.SettingsName + "#2",
			"k8s update status RemediationRequest/" + newName + "#1",
		},
		ended: func(rr *api.RemediationRequest) bool { return rr.Status.Phase == api.PhaseCancelled },
	},
	{
		name: "reviewer hints turned off",
		arrange: func(_ *testing.T, r *RemediationRequestReconciler, _ *fakegit.Git, _ *api.RemediationRequest,
			_ providers.ChangeRequest) {
			r.ReviewerHints = false
		},
		calls: []string{
			"k8s get RemediationSettings/" + api.SettingsName + "#1",
			"k8s update status RemediationRequest/" + newName + "#1",
		},
		ended: func(rr *api.RemediationRequest) bool { return rr.Status.Phase == api.PhaseCancelled },
	},
	{
		name: "predecessor gone: nothing to recover",
		arrange: func(t *testing.T, r *RemediationRequestReconciler, _ *fakegit.Git, p *api.RemediationRequest,
			_ providers.ChangeRequest) {
			if err := r.Delete(context.Background(), p); err != nil {
				t.Fatal(err)
			}
		},
		calls: []string{
			"k8s get RemediationRequest/" + oldName + "#1",
			"k8s get RemediationRequest/" + newName + "#1",
			"k8s patch status RemediationRequest/" + newName + "#1",
		},
		ended: func(rr *api.RemediationRequest) bool {
			return rr.Status.Phase == api.PhasePolicyChecked && watchedRecovery(rr) == nil
		},
	},
}

// waitingWorld is a replacement whose recovery waits on a refused read of the branch past the deadline and its
// grace, set up on path, with every later call of r to the Kubernetes API and to the Git host going through l. It
// also returns the client without l, for the test's own reads.
func waitingWorld(t *testing.T, path recoveryPath, l *callLog) (*RemediationRequestReconciler, client.Client,
	*api.RemediationRequest) {
	t.Helper()
	r, g, p, rr, cr, _ := handoffAppendedWorld(t, nil)
	ctx := context.Background()
	refuseBranchHead(g)
	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(rr)}); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(rr), rr); err != nil {
		t.Fatal(err)
	}
	if c := watchedRecovery(rr); c == nil || c.Reason != ReasonGitForbidden {
		t.Fatalf("the recovery does not wait on the refusal: %+v", c)
	}
	g.SetFail(nil)
	pastDeadline(t, r, rr, handoffRecoveryGrace+time.Hour)
	path.arrange(t, r, g, p, cr)
	if err := r.Get(ctx, client.ObjectKeyFromObject(rr), rr); err != nil {
		t.Fatal(err)
	}
	base := r.Client
	c := logClient(r.Client.(client.WithWatch), l)
	r.Client, r.APIReader = c, c
	r.WriteTimeout = waitStepTimeout
	r.OpenRepository = func(ctx context.Context, _ api.GitRepository) (providers.GitProvider, error) {
		if err := l.call(ctx, false, "open"); err != nil {
			return nil, err
		}
		return &logGit{l: l, g: g}, nil
	}
	return r, base, rr
}

// waitOutcome is what the rule gives for a failure of kind at a call to the Kubernetes API (cluster) or to the Git
// host, in a recovery that waits: wait is true when the request keeps waiting with the poll interval and no error,
// false when the failure goes to the error backoff; keep is true when the condition must stay as it was. The
// request never ends on a failure.
func waitOutcome(cluster bool, kind failureKind) (wait, keep bool) {
	switch {
	case kind == kindCancel:
		// The end of the reconcile's own context is returned as it is.
		return false, true
	case cluster:
		// Marked where it is returned, whatever it is: the error backoff, and the condition, which names a read of
		// the Git host, stays.
		return false, true
	case kind == kindForbidden:
		// A final refusal of the Git host: watched, with the refusal's reason.
		return true, false
	case kind == kindStatus, kind == kindStepDeadline, kind == kindJoined:
		// A failure of the Git host worth retrying (a 5xx, a joined one with a 5xx), or a read past the step's own
		// deadline: the wait and its pacing.
		return true, false
	}
	// Any other failure of the Git host, a conflict or a transport error that carries no class: the error backoff.
	return false, false
}

// Every call of the recovery, crossed with every kind of failure, while the recovery waits on a refusal: the request
// never ends, and the outcome is the one the rule gives (waitOutcome).
func TestRecoveryWaitSortsEachFailureByItsSource(t *testing.T) {
	for _, path := range recoveryPaths {
		t.Run(path.name, func(t *testing.T) {
			// The look that works makes exactly the listed calls, and ends as the path says.
			l := &callLog{}
			r, base, rr := waitingWorld(t, path, l)
			ctx := context.Background()
			if _, _, err := r.recoverHandoff(ctx, rr); err != nil {
				t.Fatalf("recoverHandoff without a failure: %v", err)
			}
			if got := l.recorded(); !slices.Equal(got, path.calls) {
				t.Fatalf("calls of the recovery = %q, want %q: give each new call its row", got, path.calls)
			}
			if err := base.Get(ctx, client.ObjectKeyFromObject(rr), rr); err != nil {
				t.Fatal(err)
			}
			if !path.ended(rr) {
				t.Fatalf("the look that works did not end as the path says: phase %q, conditions %+v",
					rr.Status.Phase, rr.Status.Conditions)
			}

			for _, call := range path.calls {
				for _, kind := range failureKinds {
					t.Run(call+"/"+string(kind), func(t *testing.T) {
						recoveryWaitRow(t, path, call, kind)
					})
				}
			}
		})
	}
}

func recoveryWaitRow(t *testing.T, path recoveryPath, call string, kind failureKind) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l := &callLog{target: call, kind: kind, cancel: cancel}
	r, base, rr := waitingWorld(t, path, l)
	was := watchedRecovery(rr).DeepCopy()
	cluster := call[:3] == "k8s"
	res, _, err := r.recoverHandoff(ctx, rr)
	if !slices.Contains(l.recorded(), call) {
		t.Fatalf("the call %s was not made: calls %q", call, l.recorded())
	}
	got := &api.RemediationRequest{}
	if err := base.Get(context.Background(), client.ObjectKeyFromObject(rr), got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != api.PhasePolicyChecked || got.Status.Handoff.From.Phase != api.HandoffReserved {
		t.Fatalf("a failure ended a waiting recovery: phase %q, handoff %q, conditions %+v (err %v)",
			got.Status.Phase, got.Status.Handoff.From.Phase, got.Status.Conditions, err)
	}
	c := watchedRecovery(got)
	if c == nil {
		t.Fatalf("the recovery no longer waits: conditions %+v (err %v)", got.Status.Conditions, err)
	}
	wait, keep := waitOutcome(cluster, kind)
	switch {
	case wait && (err != nil || res.RequeueAfter != DefaultPullRequestPollInterval):
		t.Errorf("recoverHandoff = %+v, %v; want the wait with the poll interval", res, err)
	case !wait && err == nil:
		t.Errorf("recoverHandoff = %+v, nil; want the error back for the error backoff", res)
	}
	if keep && (c.Reason != was.Reason || c.Message != was.Message) {
		t.Errorf("PullRequestUnreadable = %s %q, want it kept as %s %q", c.Reason, c.Message, was.Reason, was.Message)
	}
	if kind == kindForbidden && !cluster && c.Reason != ReasonGitForbidden {
		t.Errorf("PullRequestUnreadable = %s, want %s for a refusal", c.Reason, ReasonGitForbidden)
	}
	if !keep && kind != kindForbidden && c.Reason != ReasonGitForbidden && c.Reason != ReasonRecoveryReadFailed {
		t.Errorf("PullRequestUnreadable = %s, want the read failing now named", c.Reason)
	}
}
