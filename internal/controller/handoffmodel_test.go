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
	"math/rand/v2"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	api "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/gitops/sourcemap"
	"github.com/pkalin/shadewright/internal/policy/freshness"
	"github.com/pkalin/shadewright/internal/providers"
	fakegit "github.com/pkalin/shadewright/internal/providers/fake"
	fx "github.com/pkalin/shadewright/internal/testfixtures"
)

// The generated model of a handoff (#786, fifth decision of 2026-11-18): two requests, a predecessor whose pull request
// is open and a replacement taking it over, driven through Reconcile in seeded random orders of events, from one of
// three starts (the intent reserved and nothing pushed, the replacement's commit pushed and the takeover not recorded,
// the takeover recorded). The events: a reconcile of either request (either order, repeated), the clock moving on
// (minutes, past the replacement's deadline, past the deadline and the handoff grace, past a day), the Git host
// failing (503), refusing (403), not finding (404) or rejecting (422) the next read of one operation or every read
// until it answers again, the next status write of one request lost or meeting a conflicting write, a restart (a new
// reconciler, nothing kept in memory), a crash after the next reconcile made n writes (status or Git, so any two
// writes can be cut apart), the kill switch pausing writes or lifting the pause, either request deleted, and a person
// merging the pull request at its head, pushing a commit on top of it, or closing it.
//
// After every step, and at the end after a settling run (the pause lifted, reads answering, both requests reconciled
// in both orders while the clock moves on), the properties:
//
//	P1 a merge of a request's commit is recorded once: on the request whose commit it is, with its merge receipt, or,
//	   when that request is the replacement and it is deleted before it recorded the merge, on the predecessor as
//	   MergedWithForeignChange with no receipt (#786, decision 2 of the model result of 2026-11-18); never on both,
//	   and, once reads answer, never on none (a merged head with further commits on top of the replacement's commit
//	   is the documented gap #870: excluded and counted apart);
//	P2 the circuit breaker counts that merge once (one success at most), and, once reads answer, no request is a
//	   failure for the breaker that was not one when a person merged a request's own commit, except the
//	   predecessor's MergedWithForeignChange of P1, a failure as on main;
//	P3 the target lock is never free while an open pull request carries a commit of either request, the replacement
//	   never releases it while its commit is on the open pull request, and no request releases it on a merge of its
//	   own commit it does not record;
//	P4 after the abort is stored, the two requests never both write the branch or the pull request, and the
//	   predecessor writes nothing to Git while its intent stands;
//	P5 every status write passes the CRD schema and its CEL rules (transition rules included), and a reconcile that
//	   could act leaves no entry of status.gitWaits that stood past its own end at its start.
//
// A failing seed prints its minimal sequence (steps dropped while the same property still fails) and the property.
// The default set is the seeds 1 to 500 (about ten seconds), 1 to 100 under the race detector or -short; the code was
// checked against the seeds 1 to 5,000. SHADEWRIGHT_HANDOFF_MODEL_SEED=s runs one seed,
// SHADEWRIGHT_HANDOFF_MODEL_SEED_RANGE=a-b the seeds a to b instead, SHADEWRIGHT_HANDOFF_MODEL_TRACE=1
// prints the trace of every seed (SHADEWRIGHT_HANDOFF_MODEL_CONDITIONS=1 adds the conditions after each reconcile), and
// SHADEWRIGHT_HANDOFF_MODEL_SUMMARY=1 logs the count of failures per property.

const (
	// hmPred and hmRepl name the two requests in the model; hmPerson is the actor of a person's change.
	hmPred   = "pred"
	hmRepl   = "repl"
	hmPerson = "person"
	hmGone   = "gone"

	hmStartIntent   = "intent-reserved"
	hmStartPushed   = "pushed-not-recorded"
	hmStartTookOver = "takeover-recorded"

	// hmDeadline is how far the replacement's deadline lies from the start.
	hmDeadline = 20 * time.Minute
	// hmSteps is how many steps a generated sequence has.
	hmSteps = 28
	// hmSettleRounds is how many rounds of the settling run reconcile both requests.
	hmSettleRounds = 10
)

// handoffModelSkips are the seeds whose first failing property the design does not require, with the sentence that
// lets it happen. They are outside the default set and skip when a range names them.
var handoffModelSkips = map[uint64]string{
	// A replacement that took the pull request over is deleted while every read or the close fails; its finalizer
	// lets go at the end of the CleanupRefused or CleanupFailing bound with the pull request left open (design 6,
	// "The Git cleanup cannot hold a deletion for good", #288), so no request holds the lock of that open pull
	// request. P3 asks more than that sentence.
	4017: "P3: a deleted replacement's cleanup ran out of its bound (#288), the pull request is left open",
	4726: "P3: a deleted replacement's cleanup ran out of its bound (#288), the pull request is left open",
}

type hmKind string

const (
	hRecPred  hmKind = "reconcile-predecessor"
	hRecRepl  hmKind = "reconcile-replacement"
	hClock    hmKind = "clock"
	hFault    hmKind = "git-fails"
	hAnswer   hmKind = "git-answers"
	hLose     hmKind = "lose-next-write"
	hConflict hmKind = "conflict-next-write"
	hRestart  hmKind = "restart"
	hCrash    hmKind = "crash-after-writes"
	hPause    hmKind = "pause"
	hLift     hmKind = "lift-pause"
	hDelete   hmKind = "delete"
	hMerge    hmKind = "person-merges"
	hPush     hmKind = "person-pushes"
	hClose    hmKind = "person-closes"
)

const (
	clockSmall    = "minutes"
	clockDeadline = "past-deadline"
	clockGrace    = "past-deadline+15m"
	clockDay      = "past-24h"
)

// hmStep is one step of a sequence.
type hmStep struct {
	Kind   hmKind
	Obj    string // hmPred or hmRepl
	Op     string // the Git operation of a fault, "*" for every read
	Code   int    // 503, 403, 404 or 422
	Sticky bool   // the fault lasts until the Git host answers again
	Clock  string
	Mins   int
	N      int  // writes before a crash
	Commit bool // a merge with a merge commit
}

func (s hmStep) String() string {
	switch s.Kind {
	case hRecPred, hRecRepl, hAnswer, hRestart, hPause, hLift, hPush, hClose:
		return string(s.Kind)
	case hClock:
		if s.Clock == clockSmall {
			return fmt.Sprintf("clock+%dm", s.Mins)
		}
		return "clock " + s.Clock
	case hFault:
		if s.Sticky {
			return fmt.Sprintf("git-fails %d on every read until it answers", s.Code)
		}
		return fmt.Sprintf("git-fails %d on the next %s", s.Code, s.Op)
	case hLose, hConflict, hDelete:
		return string(s.Kind) + " " + s.Obj
	case hCrash:
		return fmt.Sprintf("crash after %d writes of the next reconcile", s.N)
	case hMerge:
		if s.Commit {
			return "person-merges (merge commit)"
		}
		return "person-merges (squash)"
	}
	return string(s.Kind)
}

// hmReads are the reads a fault may name.
var hmReads = []string{"open", opGet, "FindChangeRequest", opList, opBranchHead, "CheckBranch", "CheckCommit",
	"CheckMerged", "Parents", "IsAncestor", opReadFile, hmOpListComments, "ListReviews", "ReviewCheckRun", "CanWrite",
	"CheckChain"}

// hmWrites are the calls that write the branch or the pull request.
var hmWrites = map[string]bool{opCommit: true, "AppendCommit": true, opCreateBranch: true, opOpen: true,
	"UpdateChangeRequest": true, opClose: true, opComment: true, hmOpAutoMerge: true, "CreateCheckRun": true}

const (
	hmOpListComments = "ListComments"
	hmOpAutoMerge    = "EnableAutoMerge"
)

var errHMCrash = errors.New("the executor crashed")

func hmFaultErr(code int, op string) error {
	switch code {
	case 403:
		return fmt.Errorf("%s: status 403: %w", op, providers.ErrForbidden)
	case 404:
		return fmt.Errorf("%s: status 404: %w", op, providers.ErrNotFound)
	case 422:
		return fmt.Errorf("%s: status 422: %w", op, providers.ErrRejected)
	}
	return &providers.RetryableError{Err: fmt.Errorf("%s: status 503", op)}
}

// hmWorld is one run of the model.
type hmWorld struct {
	t      *testing.T
	schema *statusSchema
	start  string
	r      *RemediationRequestReconciler
	g      *fakegit.Git
	base   client.WithWatch
	cl     client.Client
	now    time.Time
	t0     time.Time
	dl     time.Time
	keys   map[string]client.ObjectKey
	number int64
	branch string
	// commits: the predecessor's, the replacement's (empty until pushed), and the ones a person pushed.
	predCommit, replCommit string
	personCommits          map[string]bool

	mu          sync.Mutex
	actor       string
	once        map[string]error
	sticky      error
	lose        map[string]bool
	conflict    map[string]bool
	crashAt     int // writes left before the crash, -1 for none
	crashed     bool
	faulted     bool // a status write of this reconcile was made to fail
	paused      bool
	abortStored bool
	writers     map[string]bool

	merge *hmMerge
	held  map[string]bool

	stepNo     int
	violations []string
	trace      []string
}

// hmMerge is the merge a person made.
type hmMerge struct {
	head  string
	owner string // hmPred, hmRepl, "gap870" or "foreign"
	// failed is which requests the breaker counted as a failure when the merge was made.
	failed map[string]bool
	// recorded is which requests have recorded the merge.
	recorded map[string]bool
}

func (m *hmWorld) logf(format string, args ...any) {
	m.trace = append(m.trace, fmt.Sprintf("[%d +%v] ", m.stepNo, m.now.Sub(m.t0))+fmt.Sprintf(format, args...))
}

// fail notes a violation of property p; one per property.
func (m *hmWorld) fail(p, format string, args ...any) {
	for _, v := range m.violations {
		if strings.HasPrefix(v, p+":") {
			return
		}
	}
	v := p + ": " + fmt.Sprintf(format, args...)
	m.violations = append(m.violations, v)
	m.logf("VIOLATION %s", v)
}

func newHMWorld(t *testing.T, schema *statusSchema, start string) *hmWorld {
	t.Helper()
	r, g, p, rr, cr, head := hmFixture(t)
	ctx := context.Background()
	m := &hmWorld{t: t, schema: schema, start: start, g: g, base: r.Client.(client.WithWatch),
		keys:   map[string]client.ObjectKey{hmPred: client.ObjectKeyFromObject(p), hmRepl: client.ObjectKeyFromObject(rr)},
		number: cr.Number, branch: cr.HeadBranch, predCommit: p.Status.PullRequest.HeadSHA,
		personCommits: map[string]bool{}, once: map[string]error{}, lose: map[string]bool{},
		conflict: map[string]bool{}, crashAt: -1, writers: map[string]bool{}, held: map[string]bool{}}
	m.now = reconcilerClock
	m.t0, m.dl = m.now, m.now.Add(hmDeadline)
	r.Now = func() time.Time { return m.now }
	rr.Status.Deadline = &metav1.Time{Time: m.dl}
	if err := r.Status().Update(ctx, rr); err != nil {
		t.Fatal(err)
	}
	switch start {
	case hmStartIntent:
		if err := g.ForcePush(m.branch, m.predCommit); err != nil {
			t.Fatal(err)
		}
	case hmStartPushed:
		m.replCommit = head
	case hmStartTookOver:
		m.replCommit = head
		if err := r.recordHandoff(ctx, rr, p, cr, head); err != nil {
			t.Fatal(err)
		}
		rr.Status.Phase = api.PhaseAwaitingApproval
		if err := r.Status().Update(ctx, rr); err != nil {
			t.Fatal(err)
		}
		if err := r.supersedePredecessor(ctx, rr); err != nil {
			t.Fatal(err)
		}
	}
	m.cl = interceptor.NewClient(m.base, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object,
			opts ...client.SubResourceUpdateOption) error {
			return m.statusWrite(ctx, obj, func() error { return c.SubResource(sub).Update(ctx, obj, opts...) })
		},
		SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch,
			opts ...client.SubResourcePatchOption) error {
			return m.statusWrite(ctx, obj, func() error { return c.SubResource(sub).Patch(ctx, obj, patch, opts...) })
		},
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if err := m.countWrite(); err != nil {
				return err
			}
			return c.Update(ctx, obj, opts...)
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch,
			opts ...client.PatchOption) error {
			if err := m.countWrite(); err != nil {
				return err
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	})
	g.SetFail(m.gitFail)
	g.SetAfter(m.gitAfter)
	m.restart()
	m.observe()
	m.logf("start %s", start)
	return m
}

// The UIDs of the model's requests: the schema wants UUIDs.
const (
	hmPolicy  = "handoff-model"
	hmPredUID = "0ad0ff00-0000-4000-8000-000000000001"
	hmReplUID = "0ad0ff00-0000-4000-8000-000000000002"
)

// hmFixture is handoffAppendedWorld with a status every write of which the CRD schema accepts: UUIDs, a full source, a
// hint with its text and times, and the target lock held by both requests, as a chain of a handoff holds it.
func hmFixture(t *testing.T) (*RemediationRequestReconciler, *fakegit.Git, *api.RemediationRequest,
	*api.RemediationRequest, providers.ChangeRequest, string) {
	t.Helper()
	ctx := context.Background()
	pol := fx.RemediationPolicy(hmPolicy)
	pol.UID = "0ad0ff00-0000-4000-8000-0000000000f0"
	pol.Spec.Actions = []api.PolicyAction{fx.MemoryLimitPolicyAction()}
	pol.Spec.Actions[0].SetContainerMemoryLimit.MaxIncreasePercent = 100
	pol.Spec.RequestNamespaces = []api.Name{requestNamespace}
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: fx.TargetNamespace, Name: appName, UID: "0ad0ff00-0000-4000-8000-0000000000d0"},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: workerContainer, Resources: corev1.ResourceRequirements{
				Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")}}}}}}},
	}
	p := fx.MemoryLimitRequest(requestNamespace, oldName)
	p.UID = hmPredUID
	p.Finalizers = []string{FinalizerName}
	p.Spec.PolicyRef = hmPolicy
	p.Spec.TargetRef.Name = dep.Name
	p.Spec.TargetRef.UID = api.UID(dep.UID)
	name, ns := p.Spec.TargetRef.Name, p.Spec.TargetRef.Namespace
	g := fakegit.NewGit(mainBranch, map[string][]byte{sourcePath: deploymentManifest(name, ns, workerContainer, "256Mi")})
	base, err := g.BranchHead(ctx, mainBranch)
	if err != nil {
		t.Fatal(err)
	}
	p.Status.Phase = api.PhaseAwaitingApproval
	src := *resolved().Source
	src.TargetBranch, src.BaseCommit = mainBranch, base
	p.Status.Source = &src
	s := fx.RemediationSettings()
	s.Spec.Repositories[0].TargetBranch = mainBranch
	p.Status.PlanRepository = freshness.RepositoryOf(s.Spec.Repositories[0])
	p.Status.PolicyUID, p.Status.PolicyRevision = api.UID(pol.UID), freshness.RevisionOf(pol)
	if p.Status.PlanDigest, err = freshness.ComputeDigest(p, &src, pol, s); err != nil {
		t.Fatal(err)
	}
	p.Status.HoldsTarget = new(true)
	h := api.Hint{Source: api.HintSourceGitHub, CommentID: 7, Digest: "sha256:" + strings.Repeat("b", 64),
		State: api.HintReserved, Author: api.HintAuthor{ID: 1001, Login: "reviewer"}, Text: "raise the limit",
		UpdatedAt: metav1.NewTime(reconcilerClock), AcceptedAt: metav1.NewTime(reconcilerClock)}
	p.Status.Hints = []api.Hint{h}
	rr := p.DeepCopy()
	rr.Name, rr.UID = newName, hmReplUID
	rr.Spec.ChainRoot = chainRootOf(p)
	rr.Spec.Replaces = &api.RequestReplacement{Name: api.Name(p.Name), UID: api.UID(p.UID), Hint: refOf(&h)}
	rr.Spec.Action.SetContainerMemoryLimit.Desired = resource.MustParse("512Mi")
	rr.Status.Phase, rr.Status.Hints = api.PhasePolicyChecked, nil
	if rr.Status.PlanDigest, err = freshness.ComputeDigest(rr, &src, pol, s); err != nil {
		t.Fatal(err)
	}
	rr.Status.Handoff = &api.Handoff{From: &api.IncomingHandoff{Name: api.Name(p.Name), UID: api.UID(p.UID),
		Hint: refOf(&h), Phase: api.HandoffReserved}}
	branch := "shadewright/" + string(chainRootOf(p))
	if err := g.CreateBranch(ctx, branch, base); err != nil {
		t.Fatal(err)
	}
	old, err := g.CommitFile(ctx, branch, base, providers.FileChange{Path: sourcePath, Content: deploymentManifest(
		name, ns, workerContainer, "384Mi"), Message: oldName})
	if err != nil {
		t.Fatal(err)
	}
	cr, err := g.OpenChangeRequest(ctx, providers.NewChangeRequest{HeadBranch: branch, BaseBranch: mainBranch,
		Title: oldName, Body: marker(p)})
	if err != nil {
		t.Fatal(err)
	}
	p.Status.PullRequest = &api.PullRequestStatus{Number: cr.Number, URL: cr.URL, HeadSHA: old}
	p.Status.Handoff = &api.Handoff{To: &api.OutgoingHandoff{Name: api.Name(rr.Name), UID: api.UID(rr.UID),
		Hint: refOf(&h), ExpectedHead: old, Phase: api.HandoffIntent}}
	content, ok, err := handoffContent(ctx, g, rr, p, old)
	if err != nil || !ok {
		t.Fatalf("handoff content: %v, %v", ok, err)
	}
	head, err := g.AppendCommit(ctx, branch, old, providers.FileChange{Path: sourcePath, Content: content, Message: newName})
	if err != nil {
		t.Fatal(err)
	}
	if cr, err = g.FindChangeRequest(ctx, branch); err != nil {
		t.Fatal(err)
	}
	c := clientfake.NewClientBuilder().WithScheme(deadlineScheme(t)).WithStatusSubresource(p, rr).
		WithObjects(s, pol, dep, p, rr).Build()
	if err := clientfake.AddIndex(c, &api.RemediationRequest{}, clearedField, func(obj client.Object) []string {
		return []string{obj.(*api.RemediationRequest).Status.BreakerClearedBy}
	}); err != nil {
		t.Fatal(err)
	}
	for _, o := range []*api.RemediationRequest{p, rr} {
		if err := c.Get(ctx, client.ObjectKeyFromObject(o), o); err != nil {
			t.Fatal(err)
		}
	}
	r := &RemediationRequestReconciler{Client: c, APIReader: c, ReviewerHints: true,
		OpenRepository: func(context.Context, api.GitRepository) (providers.GitProvider, error) { return g, nil }}
	fixClocks(r, g)
	return r, g, p, rr, cr, head
}

// restart is a new executor process: a new reconciler, nothing kept in memory.
func (m *hmWorld) restart() {
	g := m.g
	// The API server answers the != field selectors of the limits, which the fake client refuses (apiServerReader).
	m.r = &RemediationRequestReconciler{Client: m.cl, APIReader: &apiServerReader{Reader: m.cl}, ReviewerHints: true,
		Scheme: m.base.Scheme(), Now: func() time.Time { return m.now }, Source: &hmResolver{m: m},
		OpenRepository: func(context.Context, api.GitRepository) (providers.GitProvider, error) {
			if err := m.gitFail("open"); err != nil {
				return nil, err
			}
			return g, nil
		}}
	g.SetFeedbackClock(func() time.Time { return hostClock })
}

// hmResolver maps the target to the head of the target branch now.
type hmResolver struct{ m *hmWorld }

func (s *hmResolver) Resolve(ctx context.Context, _ sourcemap.Input) (sourcemap.Result, error) {
	if err := s.m.gitFail(opBranchHead); err != nil {
		return sourcemap.Result{}, err
	}
	res := resolved()
	res.Source.TargetBranch = mainBranch
	head, err := s.m.g.BranchHead(ctx, mainBranch)
	if err != nil {
		return sourcemap.Result{}, err
	}
	res.Source.BaseCommit = head
	return res, nil
}

// countWrite counts a write of the reconcile against a crash: past it, every write fails.
func (m *hmWorld) countWrite() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.crashed {
		return errHMCrash
	}
	if m.crashAt == 0 {
		m.crashed = true
		return errHMCrash
	}
	if m.crashAt > 0 {
		m.crashAt--
	}
	return nil
}

func (m *hmWorld) gitFail(op string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.actor == hmPerson || m.actor == "" {
		return nil
	}
	if m.crashed {
		return errHMCrash
	}
	if hmWrites[op] {
		if m.crashAt == 0 {
			m.crashed = true
			return errHMCrash
		}
		return nil
	}
	if err, ok := m.once[op]; ok {
		delete(m.once, op)
		return err
	}
	if m.sticky != nil {
		return m.sticky
	}
	return nil
}

func (m *hmWorld) gitAfter(op string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !hmWrites[op] || m.actor == hmPerson || m.actor == "" {
		return nil
	}
	who := m.actor
	m.trace = append(m.trace, fmt.Sprintf("[%d +%v]   git write %s by %s", m.stepNo, m.now.Sub(m.t0), op, who))
	if m.crashAt > 0 {
		m.crashAt--
	}
	if who == hmPred {
		// The intent as stored when the write is made: the predecessor may store its abort and then write.
		p := &api.RemediationRequest{}
		if err := m.base.Get(context.Background(), m.keys[hmPred], p); err == nil && intent(p) != nil {
			m.mu.Unlock()
			m.fail("P4", "the predecessor wrote %s while its intent stood", op)
			m.mu.Lock()
		}
	}
	if m.abortStored {
		m.writers[who] = true
		if len(m.writers) == 2 {
			m.mu.Unlock()
			m.fail("P4", "both requests wrote the branch or the pull request after the abort was stored (last: %s by %s)",
				op, who)
			m.mu.Lock()
		}
	}
	return nil
}

// statusWrite is a status write as the API server makes it: refused by the schema or the CEL rules, and lost or
// meeting a conflicting write when the model says so.
func (m *hmWorld) statusWrite(ctx context.Context, obj client.Object, do func() error) error {
	rr, ok := obj.(*api.RemediationRequest)
	if !ok {
		return do()
	}
	if err := m.countWrite(); err != nil {
		return err
	}
	if errs := m.schema.validate(rr); len(errs) > 0 {
		m.fail("P5", "the API server refuses a status write of %s: %v", rr.Name, errs)
		return apierrors.NewInvalid(api.GroupVersion.WithKind("RemediationRequest").GroupKind(), rr.Name, errs)
	}
	m.mu.Lock()
	lose, conflict := m.lose[rr.Name], m.conflict[rr.Name]
	delete(m.lose, rr.Name)
	delete(m.conflict, rr.Name)
	if lose || conflict {
		m.faulted = true
	}
	m.mu.Unlock()
	if lose {
		m.logf("  status write of %s lost", rr.Name)
		return apierrors.NewServiceUnavailable("lost")
	}
	if conflict {
		live := &api.RemediationRequest{}
		if err := m.base.Get(ctx, client.ObjectKeyFromObject(obj), live); err != nil {
			return err
		}
		if live.Annotations == nil {
			live.Annotations = map[string]string{}
		}
		live.Annotations["example.test/handoff-model"] = strconv.Itoa(len(live.Annotations) + m.stepNo)
		if err := m.base.Update(ctx, live); err != nil {
			return err
		}
		m.logf("  status write of %s meets a conflicting write", rr.Name)
	}
	old := &api.RemediationRequest{}
	if err := m.base.Get(ctx, client.ObjectKeyFromObject(rr), old); err != nil {
		return do()
	}
	if err := do(); err != nil {
		return err
	}
	stored := &api.RemediationRequest{}
	if err := m.base.Get(ctx, client.ObjectKeyFromObject(rr), stored); err != nil {
		return err
	}
	if errs := m.schema.validateUpdate(old, stored); len(errs) > 0 {
		m.fail("P5", "the CEL rules refuse a status write of %s: %v", rr.Name, errs)
		old.ResourceVersion = stored.ResourceVersion
		if err := m.base.Status().Update(ctx, old); err != nil {
			return err
		}
		return apierrors.NewInvalid(api.GroupVersion.WithKind("RemediationRequest").GroupKind(), rr.Name, errs)
	}
	return nil
}

// get reads who as stored, nil when gone.
func (m *hmWorld) get(who string) *api.RemediationRequest {
	rr := &api.RemediationRequest{}
	switch err := m.base.Get(context.Background(), m.keys[who], rr); {
	case apierrors.IsNotFound(err):
		return nil
	case err != nil:
		m.t.Fatal(err)
	}
	return rr
}

func (m *hmWorld) pr() providers.ChangeRequest {
	for _, cr := range m.g.ChangeRequests() {
		if cr.Number == m.number {
			return cr
		}
	}
	m.t.Fatalf("pull request #%d is gone", m.number)
	return providers.ChangeRequest{}
}

// history is the commits from sha back to the predecessor's commit, newest first; ok is false when the predecessor's
// commit is not under sha.
func (m *hmWorld) history(sha string) ([]string, bool) {
	var out []string
	for range 64 {
		if sha == m.predCommit {
			return out, true
		}
		out = append(out, sha)
		parents, err := m.g.Parents(context.Background(), sha)
		if err != nil || len(parents) == 0 {
			return out, false
		}
		sha = parents[0]
	}
	return out, false
}

// foreignForGone reports the one case where a merge of the replacement's commit is recorded on the predecessor
// (#786, decision 2 of the model result of 2026-11-18): the replacement is deleted (gone or being deleted), it did not
// record the merge, and the predecessor records it as MergedWithForeignChange with no receipt.
func (m *hmWorld) foreignForGone(reqs map[string]*api.RemediationRequest) bool {
	mg, repl, p := m.merge, reqs[hmRepl], reqs[hmPred]
	if mg == nil || mg.owner != hmRepl || mg.recorded[hmRepl] || p == nil {
		return false
	}
	if repl != nil && repl.DeletionTimestamp.IsZero() {
		return false
	}
	c := meta.FindStatusCondition(p.Status.Conditions, api.ConditionApproved)
	return p.Status.MergeReceipt == nil && c != nil && c.Reason == ReasonMergedWithForeignChange
}

// carries reports whether head carries a commit of either request.
func (m *hmWorld) carries(head string) bool {
	_, ok := m.history(head)
	return ok
}

// carriesOwn reports whether head carries the commit of who.
func (m *hmWorld) carriesOwn(head, who string) bool {
	above, ok := m.history(head)
	if !ok {
		return false
	}
	if who == hmPred {
		return true
	}
	return m.replCommit != "" && slices.Contains(above, m.replCommit)
}

// learnReplCommit finds the replacement's commit after it pushed: the commit on top of the predecessor's that no
// person pushed.
func (m *hmWorld) learnReplCommit() {
	if m.replCommit != "" {
		return
	}
	head, err := m.g.BranchHead(context.Background(), m.branch)
	if err != nil {
		return
	}
	above, ok := m.history(head)
	if !ok {
		return
	}
	for _, sha := range slices.Backward(above) {
		if !m.personCommits[sha] {
			m.replCommit = sha
			m.logf("  the replacement's commit is %s", short(sha))
			return
		}
	}
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

// recordsMerge reports whether rr records a merge of its pull request in any form.
func recordsMerge(rr *api.RemediationRequest) bool {
	if rr == nil {
		return false
	}
	if rr.Status.MergeReceipt != nil || (rr.Status.PullRequest != nil && rr.Status.PullRequest.MergedAt != nil) ||
		rr.Status.Phase == api.PhaseMerged {
		return true
	}
	for _, c := range rr.Status.Conditions {
		if uncountedMergeReasons[c.Reason] {
			return true
		}
	}
	return false
}

func describe(rr *api.RemediationRequest) string {
	if rr == nil {
		return hmGone
	}
	s := fmt.Sprintf("%s holds=%v", rr.Status.Phase, ptr.Deref(rr.Status.HoldsTarget, false))
	if c := meta.FindStatusCondition(rr.Status.Conditions, api.ConditionPROpened); c != nil && c.Status == metav1.ConditionFalse {
		s += " PROpened=" + c.Reason
	}
	if c := meta.FindStatusCondition(rr.Status.Conditions, api.ConditionApproved); c != nil && c.Status == metav1.ConditionFalse {
		s += " Approved=" + c.Reason
	}
	if ho := rr.Status.Handoff; ho != nil {
		if ho.From != nil {
			s += " from=" + string(ho.From.Phase)
		}
		if ho.To != nil {
			s += " to=" + string(ho.To.Phase)
		}
	}
	if rr.Status.MergeReceipt != nil {
		s += " receipt=" + short(rr.Status.MergeReceipt.HeadSHA)
	}
	for _, w := range rr.Status.GitWaits {
		s += " wait=" + string(w.Kind)
	}
	if !rr.DeletionTimestamp.IsZero() {
		s += " deleting"
	}
	return s
}

// observe checks the properties that hold after every step, and notes what later checks need.
func (m *hmWorld) observe() {
	p, rr := m.get(hmPred), m.get(hmRepl)
	reqs := map[string]*api.RemediationRequest{hmPred: p, hmRepl: rr}
	if p != nil && p.Status.Handoff != nil && p.Status.Handoff.To != nil &&
		p.Status.Handoff.To.Phase == api.HandoffAborted && !m.abortStored {
		m.abortStored = true
		m.logf("  the abort is stored")
	}
	cr := m.pr()
	// P1 and P2 after a merge of a request's own commit.
	if mg := m.merge; mg != nil && (mg.owner == hmPred || mg.owner == hmRepl) {
		other := map[string]string{hmPred: hmRepl, hmRepl: hmPred}[mg.owner]
		if recordsMerge(reqs[other]) && !m.foreignForGone(reqs) {
			m.fail("P1", "the merge of the %s's commit is recorded on the %s (%s)", mg.owner, other, describe(reqs[other]))
		}
		for who, x := range reqs {
			if recordsMerge(x) {
				mg.recorded[who] = true
			}
		}
		if len(mg.recorded) > 1 {
			m.fail("P1", "the merge of the %s's commit is recorded on both requests", mg.owner)
		}
		successes := 0
		for _, x := range reqs {
			if x != nil && breakerOutcome(x) == outcomeSuccess {
				successes++
			}
		}
		if successes > 1 {
			m.fail("P2", "the breaker counts the merge as %d successes", successes)
		}
	}
	// P3.
	free := true
	for who, x := range reqs {
		holds := x != nil && ptr.Deref(x.Status.HoldsTarget, false)
		if holds {
			free = false
		}
		if m.held[who] && !holds && x != nil {
			switch {
			case cr.State == providers.ChangeRequestOpen && who == hmRepl && m.carriesOwn(cr.HeadSHA, hmRepl):
				m.fail("P3", "the replacement released the lock while its commit is on the open pull request (%s)",
					describe(x))
			case cr.State == providers.ChangeRequestMerged && m.merge != nil && m.merge.owner == who && !recordsMerge(x):
				m.fail("P3", "the %s released the lock on a merge of its commit it does not record (%s)", who,
					describe(x))
			}
		}
		m.held[who] = holds
	}
	if free && cr.State == providers.ChangeRequestOpen && m.carries(cr.HeadSHA) {
		m.fail("P3", "the lock is free while the open pull request carries a commit of a request (pred: %s, repl: %s)",
			describe(p), describe(rr))
	}
}

// reconcile runs one reconcile of who and checks what it left.
func (m *hmWorld) reconcile(who string) {
	before := m.get(who)
	if before == nil {
		m.logf("%s is gone", who)
		return
	}
	ctx := context.Background()
	m.mu.Lock()
	m.actor, m.faulted = who, false
	m.mu.Unlock()
	res, err := m.r.Reconcile(ctx, reconcile.Request{NamespacedName: m.keys[who]})
	m.mu.Lock()
	m.actor = ""
	crashed, faulted := m.crashed, m.faulted
	m.crashed, m.crashAt = false, -1
	m.mu.Unlock()
	after := m.get(who)
	m.logf("  -> %s: %s (requeue %v, err %v)", who, describe(after), res.RequeueAfter, err)
	if os.Getenv("SHADEWRIGHT_HANDOFF_MODEL_CONDITIONS") != "" && after != nil {
		for _, c := range after.Status.Conditions {
			m.logf("       %s=%s %s: %s", c.Type, c.Status, c.Reason, c.Message)
		}
	}
	if who == hmRepl {
		m.learnReplCommit()
	}
	if crashed {
		m.logf("  the executor crashed and restarts")
		m.restart()
		return
	}
	// P5: a reconcile that could act ends every wait that stood past its own end at its start.
	if after != nil && err == nil && !faulted && !m.paused {
		for _, w := range before.Status.GitWaits {
			if w.Ends == nil || w.Ends.After(m.now) {
				continue
			}
			for _, a := range after.Status.GitWaits {
				if a.Kind == w.Kind && a.Since.Equal(&w.Since) {
					m.fail("P5", "the %s wait of the %s stands past its own end %v", w.Kind, who, w.Ends.Time)
				}
			}
		}
	}
}

// apply makes one step happen.
func (m *hmWorld) apply(s hmStep) {
	ctx := context.Background()
	m.logf("%s", s)
	switch s.Kind {
	case hRecPred:
		m.reconcile(hmPred)
	case hRecRepl:
		m.reconcile(hmRepl)
	case hClock:
		switch s.Clock {
		case clockSmall:
			m.now = m.now.Add(time.Duration(s.Mins) * time.Minute)
		case clockDeadline:
			m.now = latest(m.now.Add(time.Minute), m.dl.Add(time.Minute))
		case clockGrace:
			m.now = latest(m.now.Add(time.Minute), m.dl.Add(handoffRecoveryGrace+time.Minute))
		case clockDay:
			m.now = m.now.Add(recoveryWaitBound + time.Hour)
		}
	case hFault:
		m.mu.Lock()
		if s.Sticky {
			m.sticky = hmFaultErr(s.Code, "every read")
		} else {
			m.once[s.Op] = hmFaultErr(s.Code, s.Op)
		}
		m.mu.Unlock()
	case hAnswer:
		m.mu.Lock()
		m.sticky, m.once = nil, map[string]error{}
		m.mu.Unlock()
	case hLose:
		m.lose[m.keys[s.Obj].Name] = true
	case hConflict:
		m.conflict[m.keys[s.Obj].Name] = true
	case hRestart:
		m.restart()
	case hCrash:
		m.crashAt = s.N
	case hPause, hLift:
		m.paused = s.Kind == hPause
		s := &api.RemediationSettings{}
		if err := m.base.Get(ctx, client.ObjectKey{Name: api.SettingsName}, s); err != nil {
			m.t.Fatal(err)
		}
		s.Spec.Paused = new(m.paused)
		if err := m.base.Update(ctx, s); err != nil {
			m.t.Fatal(err)
		}
	case hDelete:
		rr := m.get(s.Obj)
		if rr == nil || !rr.DeletionTimestamp.IsZero() {
			return
		}
		if err := m.base.Delete(ctx, rr); err != nil && !apierrors.IsNotFound(err) {
			m.t.Fatal(err)
		}
	case hMerge, hPush, hClose:
		m.personEvent(s)
	}
}

// personEvent is a person merging, pushing to or closing the open pull request.
func (m *hmWorld) personEvent(s hmStep) {
	ctx := context.Background()
	cr := m.pr()
	if cr.State != providers.ChangeRequestOpen {
		return
	}
	m.mu.Lock()
	m.actor = hmPerson
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.actor = ""
		m.mu.Unlock()
	}()
	switch s.Kind {
	case hMerge:
		var err error
		if s.Commit {
			_, err = m.g.MergeWithCommit(m.number)
		} else {
			_, err = m.g.Merge(m.number)
		}
		if err != nil {
			m.logf("  the merge is refused: %v", err)
			return
		}
		m.merge = &hmMerge{head: cr.HeadSHA, owner: m.ownerOf(cr.HeadSHA), failed: map[string]bool{},
			recorded: map[string]bool{}}
		for _, who := range []string{hmPred, hmRepl} {
			if x := m.get(who); x != nil && breakerOutcome(x) == outcomeFailure {
				m.merge.failed[who] = true
			}
		}
		m.logf("  merged head %s, the %s's commit", short(cr.HeadSHA), m.merge.owner)
	case hPush:
		sha, err := m.g.PushFiles(m.branch, map[string][]byte{"person.yaml": []byte(fmt.Sprintf("step: %d\n", m.stepNo))})
		if err != nil {
			m.t.Fatal(err)
		}
		m.personCommits[sha] = true
	case hClose:
		if err := m.g.CloseChangeRequest(ctx, m.number); err != nil {
			m.t.Fatal(err)
		}
	}
}

// ownerOf is whose commit head is: hmPred, hmRepl, gap870 (further commits on top of the replacement's commit,
// #870) or foreign (a person's commit on top of the predecessor's alone).
func (m *hmWorld) ownerOf(head string) string {
	switch {
	case head == m.predCommit:
		return hmPred
	case m.replCommit != "" && head == m.replCommit:
		return hmRepl
	case m.carriesOwn(head, hmRepl):
		return "gap870"
	}
	return "foreign"
}

func latest(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// settle lifts the pause, lets reads answer and reconciles both requests in both orders while the clock moves on, then
// checks what must hold once reads answer.
func (m *hmWorld) settle() {
	m.logf("settle")
	m.mu.Lock()
	m.sticky, m.once, m.lose, m.conflict, m.crashAt = nil, map[string]error{}, map[string]bool{}, map[string]bool{}, -1
	m.mu.Unlock()
	if m.paused {
		m.apply(hmStep{Kind: hLift})
	}
	for i := range hmSettleRounds {
		order := []string{hmPred, hmRepl}
		if i%2 == 1 {
			order = []string{hmRepl, hmPred}
		}
		for _, who := range order {
			m.reconcile(who)
			m.observe()
		}
		m.now = m.now.Add(11 * time.Minute)
		if i == hmSettleRounds/2 {
			m.now = m.now.Add(recoveryWaitBound)
		}
	}
	mg := m.merge
	if mg == nil || (mg.owner != hmPred && mg.owner != hmRepl) {
		return
	}
	// P2 once reads answer: no request is a failure for the breaker that was not one when the merge was made.
	reqs := map[string]*api.RemediationRequest{hmPred: m.get(hmPred), hmRepl: m.get(hmRepl)}
	for _, who := range []string{hmPred, hmRepl} {
		if who == hmPred && m.foreignForGone(reqs) {
			// #786, decision 2 of the model result of 2026-11-18: MergedWithForeignChange is a failure for the
			// breaker, as on main.
			continue
		}
		if x := reqs[who]; who == hmRepl && x != nil && !x.DeletionTimestamp.IsZero() && !mg.recorded[hmRepl] {
			// #786, decision 2 of the model result of 2026-11-18: a replacement being deleted records nothing, so its
			// end stays as it was.
			continue
		}
		if x := reqs[who]; x != nil && breakerOutcome(x) == outcomeFailure && !mg.failed[who] {
			m.fail("P2", "the %s ended a failure for the breaker after the merge of the %s's commit (%s)", who,
				mg.owner, describe(x))
		}
	}
	owner := m.get(mg.owner)
	if owner == nil || (mg.owner == hmRepl && !owner.DeletionTimestamp.IsZero() && !mg.recorded[hmRepl]) {
		// #786, decision 2 of the model result of 2026-11-18: a deleted replacement records nothing; unless it
		// recorded the merge before it went, the predecessor records it as a foreign change. A predecessor that handed its pull request over (to Done) has
		// none left to record, as for any request deleted after its merge.
		if mg.owner == hmRepl && !mg.recorded[hmRepl] && reqs[hmPred] != nil && !handedOff(reqs[hmPred]) &&
			!m.foreignForGone(reqs) {
			m.fail("P1", "the merge of the deleted replacement's commit %s is recorded on neither request (pred: %s)",
				short(mg.head), describe(reqs[hmPred]))
		}
		return
	}
	if owner.Status.MergeReceipt == nil || owner.Status.MergeReceipt.HeadSHA != mg.head {
		m.fail("P1", "the merge of the %s's commit %s is not recorded on it with its receipt (pred: %s, repl: %s)",
			mg.owner, short(mg.head), describe(m.get(hmPred)), describe(m.get(hmRepl)))
	}
}

// hmRun runs start and steps and returns the world, with its violations.
func hmRun(t *testing.T, schema *statusSchema, start string, steps []hmStep) *hmWorld {
	t.Helper()
	m := newHMWorld(t, schema, start)
	for i, s := range steps {
		m.stepNo = i + 1
		m.apply(s)
		m.observe()
	}
	m.stepNo = len(steps) + 1
	m.settle()
	return m
}

// property is the property of the first violation, "" for none.
func (m *hmWorld) property() string {
	if len(m.violations) == 0 {
		return ""
	}
	p, _, _ := strings.Cut(m.violations[0], ":")
	return p
}

// hmOutcome is what a merge was, for the counts.
func (m *hmWorld) mergeOwner() string {
	if m.merge == nil {
		return "none"
	}
	return m.merge.owner
}

// hmGenerate draws a start and a sequence from seed. The seed's focus (seed modulo 4) weights the draw towards the
// orders of the decision: a refusal and the end of the watch, a lost takeover write and a 5xx past the deadline, a
// crash and a deleted predecessor, or none.
func hmGenerate(seed uint64) (string, []hmStep) {
	// #nosec G404 -- a seeded generator, so a failing order can be run again from its seed.
	rng := rand.New(rand.NewPCG(seed, seed^0x2545f4914f6cdd1d))
	focus := seed % 4
	starts := []string{hmStartIntent, hmStartPushed, hmStartTookOver}
	start := starts[rng.IntN(len(starts))]
	if focus != 0 && rng.IntN(3) > 0 {
		start = hmStartPushed
	}
	type weighted struct {
		w    int
		kind hmKind
	}
	weights := []weighted{{6, hRecPred}, {6, hRecRepl}, {4, hClock}, {3, hFault}, {2, hAnswer}, {1, hLose},
		{1, hConflict}, {1, hRestart}, {1, hCrash}, {1, hPause}, {1, hLift}, {1, hDelete}, {2, hMerge}, {1, hPush},
		{1, hClose}}
	switch focus {
	case 1:
		weights = append(weights, weighted{3, hFault}, weighted{2, hClock}, weighted{2, hMerge})
	case 2:
		weights = append(weights, weighted{2, hLose}, weighted{2, hClock}, weighted{2, hFault})
	case 3:
		weights = append(weights, weighted{2, hCrash}, weighted{2, hDelete}, weighted{3, hClock})
	}
	total := 0
	for _, w := range weights {
		total += w.w
	}
	objs := []string{hmPred, hmRepl}
	steps := make([]hmStep, 0, hmSteps)
	for range hmSteps {
		n := rng.IntN(total)
		var k hmKind
		for _, w := range weights {
			if n < w.w {
				k = w.kind
				break
			}
			n -= w.w
		}
		s := hmStep{Kind: k}
		switch k {
		case hClock:
			s.Clock = []string{clockSmall, clockSmall, clockDeadline, clockGrace, clockDay}[rng.IntN(5)]
			s.Mins = 1 + rng.IntN(5)
		case hFault:
			s.Code = []int{503, 403, 404, 422}[rng.IntN(4)]
			if focus == 2 && rng.IntN(2) == 0 {
				s.Code = 503
			}
			if focus == 1 && rng.IntN(2) == 0 {
				s.Code = 403
			}
			s.Sticky = rng.IntN(3) == 0
			s.Op = hmReads[rng.IntN(len(hmReads))]
		case hLose, hConflict:
			s.Obj = objs[rng.IntN(2)]
		case hDelete:
			s.Obj = objs[rng.IntN(2)]
			if focus == 3 {
				s.Obj = hmPred
			}
		case hCrash:
			s.N = rng.IntN(3)
		case hMerge:
			s.Commit = rng.IntN(4) > 0
		}
		steps = append(steps, s)
	}
	return start, steps
}

// hmShrink drops steps while the run still breaks property p, as far as one pass goes.
func hmShrink(t *testing.T, schema *statusSchema, start string, steps []hmStep, p string) []hmStep {
	t.Helper()
	cur := slices.Clone(steps)
	for i := range slices.Backward(cur) {
		try := slices.Delete(slices.Clone(cur), i, i+1)
		if hmRun(t, schema, start, try).property() == p {
			cur = try
		}
	}
	return cur
}

func formatSteps(steps []hmStep) string {
	out := make([]string, len(steps))
	for i, s := range steps {
		out[i] = fmt.Sprintf("%2d. %s", i+1, s)
	}
	return strings.Join(out, "\n    ")
}

// hmSeeds is the seed set: 1 to n, or what the environment names.
func hmSeeds(t *testing.T) []uint64 {
	if s := os.Getenv("SHADEWRIGHT_HANDOFF_MODEL_SEED"); s != "" {
		seed, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		return []uint64{seed}
	}
	from, to := uint64(1), uint64(500)
	if testing.Short() || raceDetector {
		to = 100
	}
	if s := os.Getenv("SHADEWRIGHT_HANDOFF_MODEL_SEED_RANGE"); s != "" {
		a, b, _ := strings.Cut(s, "-")
		x, errA := strconv.ParseUint(a, 10, 64)
		y, errB := strconv.ParseUint(b, 10, 64)
		if errA != nil || errB != nil || y < x {
			t.Fatalf("SHADEWRIGHT_HANDOFF_MODEL_SEED_RANGE=%q; want from-to", s)
		}
		from, to = x, y
	}
	seeds := make([]uint64, 0, to-from+1)
	for s := from; s <= to; s++ {
		seeds = append(seeds, s)
	}
	return seeds
}

func TestHandoffModel(t *testing.T) {
	schema := requestStatusSchema(t)
	var mu sync.Mutex
	counts := map[string]int{}
	t.Cleanup(func() {
		if os.Getenv("SHADEWRIGHT_HANDOFF_MODEL_SUMMARY") != "" {
			t.Logf("handoff model summary: %v", counts)
		}
	})
	for _, seed := range hmSeeds(t) {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			t.Parallel()
			if why, ok := handoffModelSkips[seed]; ok {
				t.Skipf("the design does not require it: %s", why)
			}
			start, steps := hmGenerate(seed)
			m := hmRun(t, schema, start, steps)
			p := m.property()
			mu.Lock()
			counts["seeds"]++
			counts["merge="+m.mergeOwner()]++
			for _, v := range m.violations {
				k, _, _ := strings.Cut(v, ":")
				counts[k]++
			}
			mu.Unlock()
			if os.Getenv("SHADEWRIGHT_HANDOFF_MODEL_TRACE") != "" {
				t.Logf("trace:\n  %s", strings.Join(m.trace, "\n  "))
			}
			if p == "" {
				return
			}
			minimal := hmShrink(t, schema, start, steps, p)
			small := hmRun(t, schema, start, minimal)
			t.Fatalf("seed %d (SHADEWRIGHT_HANDOFF_MODEL_SEED=%d) breaks %s\n  start %s\n  minimal sequence:\n    %s\n"+
				"  violations: %s\n  violations of the full sequence: %s\n  trace of the minimal sequence:\n  %s",
				seed, seed, p, start, formatSteps(minimal), strings.Join(small.violations, "; "),
				strings.Join(m.violations, "; "), strings.Join(small.trace, "\n  "))
		})
	}
}

// hmDirected runs a scripted sequence and fails with the violations.
func hmDirected(t *testing.T, start string, steps []hmStep) *hmWorld {
	t.Helper()
	m := hmRun(t, requestStatusSchema(t), start, steps)
	if len(m.violations) > 0 || os.Getenv("SHADEWRIGHT_HANDOFF_MODEL_TRACE") != "" {
		t.Logf("trace:\n  %s", strings.Join(m.trace, "\n  "))
	}
	if len(m.violations) > 0 {
		t.Fatalf("violations: %s", strings.Join(m.violations, "; "))
	}
	return m
}

var (
	stRecPred = hmStep{Kind: hRecPred}
	stRecRepl = hmStep{Kind: hRecRepl}
	stAnswer  = hmStep{Kind: hAnswer}
	stMerge   = hmStep{Kind: hMerge, Commit: true}
)

func stSticky(code int) hmStep { return hmStep{Kind: hFault, Code: code, Sticky: true} }
func stClock(c string) hmStep  { return hmStep{Kind: hClock, Clock: c} }

// The replacement in a world where the intent is reserved and nothing is pushed takes the pull request over, and a merge
// of its commit is recorded on it alone.
func TestHandoffModelIntentTakeover(t *testing.T) {
	m := hmDirected(t, hmStartIntent, []hmStep{stRecRepl, stRecRepl, stRecPred, stRecRepl, stMerge, stRecPred,
		stRecRepl})
	if m.replCommit == "" {
		t.Fatal("the replacement pushed no commit from the intent")
	}
}

// (a) The watch runs to its end with every read refused, reads answer again, a person merges the pull request with the
// replacement's commit, and the predecessor, still AwaitingApproval, looks first: the merge must be recorded on the
// replacement alone, and the breaker must not count a failure for it.
func TestHandoffModelMergeAfterTheWatchPredecessorFirst(t *testing.T) {
	hmDirected(t, hmStartPushed, []hmStep{stSticky(403), stRecRepl, stClock(clockDay), stRecRepl,
		stAnswer, stMerge, stRecPred, stRecRepl})
}

// (b) The replacement's takeover write is lost, a 503 after the deadline records HandoffGrace, after the grace the
// predecessor looks first, the replacement then ends, and a person merges its commit: the merge must be recorded on
// the replacement.
func TestHandoffModelMergeAfterTheGrace(t *testing.T) {
	hmDirected(t, hmStartPushed, []hmStep{{Kind: hLose, Obj: hmRepl}, stRecRepl, stClock(clockDeadline),
		stSticky(503), stRecRepl, stClock(clockGrace), stRecPred, stRecRepl, stAnswer, stMerge, stRecPred, stRecRepl})
}

// (c) The replacement pushed and restarted before it recorded the takeover, a 5xx past the deadline records
// HandoffGrace, the grace passes, the predecessor is deleted (its finalizer waits for the replacement), and a person
// merges the replacement's commit before the replacement looks again: the merge must be recorded on the replacement.
func TestHandoffModelMergeAfterThePredecessorIsDeleted(t *testing.T) {
	hmDirected(t, hmStartPushed,
		[]hmStep{{Kind: hRestart}, stClock(clockDeadline), stSticky(503), stRecRepl, stAnswer, stClock(clockGrace),
			{Kind: hDelete, Obj: hmPred}, stRecPred, stMerge, stRecRepl, stRecPred})
}
