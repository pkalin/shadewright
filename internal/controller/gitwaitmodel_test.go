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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	api "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/policy/freshness"
	"github.com/pkalin/shadewright/internal/providers"
	fakegit "github.com/pkalin/shadewright/internal/providers/fake"
)

// The generated test of the third decision of 2026-11-17 on #771: a small model of the world around one request
// whose plan's target branch changed, driven through Reconcile with a moving clock over seeded random orders of
// events. It uses only what the request shows (conditions, phase, lock) and what the model injects, so it runs on
// any build of the branch.
//
// The world: the request starts waiting for review or stopped (for the branch change on PlanFresh, on Approved, or
// with its pull request never recorded because that status write was lost, cancelled for its Incident, or stopped
// for another reason). Its pull request is open, closed, merged into the recorded branch, moved to the configured
// branch (and merged there), deleted (gone from the host), or none at all. Between reconciles: a Git read fails
// or runs out of the step's time (the pull request reads, the listing, the read of what a merge brought, a file),
// or reads work again; the next status write is lost or meets a conflicting write; the executor restarts (nothing
// kept in memory). The clock moves on by the wait each reconcile returns (a second after an error).
//
// After each reconcile:
//   - bound: a wait condition GitReadRetry shows has not passed its end by more than one poll interval, and the
//     reads of what a merge brought that kept failing since a time T end the request (MergeContentUnreadable or
//     the merge recorded) by T plus mergeContentWait plus one poll interval, whatever else fails meanwhile;
//   - errors: no three reconciles in a row return an error while no status write is made to fail;
//   - lock: the lock is never released while a pull request of the request is open, nor on a merge the status
//     does not record.
//
// At the end the world heals (reads work, no write fails) and, after a few reconciles, the lock is free, or held
// by a request whose pull request is open and can still merge.
//
// Seeds: modelSeeds in CI (a fixed set, fewer under the race detector or -short); SHADEWRIGHT_MODEL_SEEDS=n runs n
// more seeds from the time, SHADEWRIGHT_MODEL_SEED=s one seed and SHADEWRIGHT_MODEL_SEED_RANGE=a-b the seeds a to b
// instead of the fixed set. A failure prints its seed and the trace; SHADEWRIGHT_MODEL_TRACE=1 prints it always.

const (
	modelSteps = 24

	faultNone    = "none"
	faultContent = "content"
	faultTimeout = "+timeout"

	// startRunning: the request waits for review, not stopped.
	startRunning = "running"
	// startNoPullRequest: no pull request exists at all (the open's write was lost and it was deleted).
	startNoPullRequest = "no-pull-request-at-all"
)

var (
	modelStarts = []string{startRunning, stopPlanFresh, stopApproved, stopUnrecord, stopCancelled, stopOther, startNoPullRequest}
	modelFaults = []string{faultNone, opGet, opList, faultContent, opReadFile, opGet + faultTimeout,
		faultContent + faultTimeout}
)

// modelSeeds is the fixed seed set: 1 to n.
func modelSeeds(t *testing.T) []uint64 {
	if s := os.Getenv("SHADEWRIGHT_MODEL_SEED"); s != "" {
		seed, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		return []uint64{seed}
	}
	n := 80
	if testing.Short() || raceDetector {
		n = 24
	}
	seeds := make([]uint64, 0, n)
	for i := range n {
		seeds = append(seeds, uint64(i+1))
	}
	if s := os.Getenv("SHADEWRIGHT_MODEL_SEED_RANGE"); s != "" {
		from, to, _ := strings.Cut(s, "-")
		a, errA := strconv.ParseUint(from, 10, 64)
		b, errB := strconv.ParseUint(to, 10, 64)
		if errA != nil || errB != nil || b < a {
			t.Fatalf("SHADEWRIGHT_MODEL_SEED_RANGE=%q; want from-to", s)
		}
		seeds = seeds[:0]
		for seed := a; seed <= b; seed++ {
			seeds = append(seeds, seed)
		}
	}
	if s := os.Getenv("SHADEWRIGHT_MODEL_SEEDS"); s != "" {
		more, err := strconv.Atoi(s)
		if err != nil {
			t.Fatal(err)
		}
		base := uint64(time.Now().UnixNano())
		for i := range more {
			seeds = append(seeds, base+uint64(i))
		}
	}
	return seeds
}

func TestGitWaitModel(t *testing.T) {
	for _, seed := range modelSeeds(t) {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			t.Parallel()
			m := newModel(t, seed)
			m.run()
		})
	}
}

// modelWorld is one run of the model.
type modelWorld struct {
	t    *testing.T
	seed uint64
	rng  *rand.Rand
	r    *RemediationRequestReconciler
	g    *fakegit.Git
	base client.WithWatch
	key  client.ObjectKey
	// second is a second request on the same target, created during the run; zero until then.
	second client.ObjectKey
	clock  *testClock
	trace  []string
	// template is the request as it started, for the second request's spec.
	template *api.RemediationRequest

	mu sync.Mutex
	// fault is the Git read that fails (or runs out of the step's time), faultNone when reads work.
	fault string
	// loseNext and conflictNext make the next status write fail, or meet a write made between its read and it.
	loseNext, conflictNext bool
	// writeFaulted reports that this reconcile met an injected write fault.
	writeFaulted bool
	// deleted hides the pull request from every read (gone from the host).
	deleted bool
	number  int64
	merge   string
	// contentFailing is when the reads of what the merge brought began to fail without a success since, from the
	// first failure in a reconcile that ended without an error (a wait whose status write failed is not started:
	// the reconcile returns the error for a retry); zero when they work, or were never made. contentTried is the
	// first failure of the reconcile running.
	contentFailing, contentTried time.Time
	errorsInRow                  int
	startAt                      time.Time
	violations                   []string
}

func newModel(t *testing.T, seed uint64) *modelWorld {
	// #nosec G404 -- a seeded generator, so a failing order can be run again from its seed.
	m := &modelWorld{t: t, seed: seed, rng: rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)), fault: faultNone}
	start := modelStarts[m.rng.IntN(len(modelStarts))]
	phase := api.PhaseNeedsIntervention
	switch start {
	case startRunning:
		phase = api.PhaseAwaitingApproval
	case stopCancelled:
		phase = api.PhaseCancelled
	}
	r, g, rr := settlementWorld(t, phase)
	m.r, m.g, m.key = r, g, client.ObjectKeyFromObject(rr)
	m.template = rr.DeepCopy()
	m.number = rr.Status.PullRequest.Number
	ctx := context.Background()
	switch start {
	case startRunning:
		meta.RemoveStatusCondition(&rr.Status.Conditions, api.ConditionApproved)
		if err := r.Status().Update(ctx, rr); err != nil {
			t.Fatal(err)
		}
	case stopCancelled:
		orderStop(t, r, rr, stopCancelled)
	case startNoPullRequest:
		lostRecord(t, r, rr, api.ConditionPROpened)
		m.deleted = true
	default:
		orderStop(t, r, rr, start)
	}
	m.logf("start %s", start)
	m.clock = clockOf(r)
	m.startAt = m.clock.at
	m.base = r.Client.(client.WithWatch)
	c := interceptor.NewClient(m.base, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object,
			opts ...client.SubResourceUpdateOption) error {
			if err := m.writeFault(ctx, obj); err != nil {
				return err
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
		SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch,
			opts ...client.SubResourcePatchOption) error {
			if err := m.writeFault(ctx, obj); err != nil {
				return err
			}
			return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
		},
	})
	r.Client, r.APIReader = c, c
	r.OpenRepository = func(_ context.Context, repo api.GitRepository) (providers.GitProvider, error) {
		return &modelGit{GitProvider: g.ForTarget(string(repo.TargetBranch)), m: m}, nil
	}
	// Long enough for every step that does not wait on a slow read, also under the race detector.
	r.CleanupTimeout = 150 * time.Millisecond
	return m
}

// writeFault fails the status write, or makes a write between its read and it, when the model says so.
func (m *modelWorld) writeFault(ctx context.Context, obj client.Object) error {
	m.mu.Lock()
	lose, conflict := m.loseNext, m.conflictNext
	m.loseNext, m.conflictNext = false, false
	if lose || conflict {
		m.writeFaulted = true
	}
	m.mu.Unlock()
	if lose {
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
		live.Annotations["example.test/model"] = strconv.Itoa(len(live.Annotations["example.test/model"]) + 1)
		return m.base.Update(ctx, live)
	}
	return nil
}

func (m *modelWorld) logf(format string, args ...any) {
	m.trace = append(m.trace, fmt.Sprintf("[+%v] ", m.elapsed())+fmt.Sprintf(format, args...))
}

func (m *modelWorld) elapsed() time.Duration {
	if m.clock == nil {
		return 0
	}
	return m.clock.at.Sub(m.start())
}

// start is the clock's time when the run began.
func (m *modelWorld) start() time.Time { return m.startAt }

// fatalf notes a violated property; the run goes on, so one order can show more than one, and fails at its end
// with every violation (the first of each kind) and the trace.
func (m *modelWorld) fatalf(format string, args ...any) {
	what := fmt.Sprintf(format, args...)
	kind, _, _ := strings.Cut(what, ":")
	for _, v := range m.violations {
		if strings.HasPrefix(v, kind) {
			return
		}
	}
	m.violations = append(m.violations, what)
	m.logf("VIOLATION %s", what)
}

// report fails the run with its violations, its seed and its trace.
func (m *modelWorld) report() {
	m.t.Helper()
	if len(m.violations) == 0 {
		if os.Getenv("SHADEWRIGHT_MODEL_TRACE") != "" {
			m.t.Logf("seed %d trace:\n  %s", m.seed, strings.Join(m.trace, "\n  "))
		}
		return
	}
	m.t.Fatalf("seed %d (SHADEWRIGHT_MODEL_SEED=%d):\n  %s\ntrace:\n  %s", m.seed, m.seed,
		strings.Join(m.violations, "\n  "), strings.Join(m.trace, "\n  "))
}

func (m *modelWorld) run() {
	if !m.deleted && m.rng.IntN(3) == 0 {
		// A person moves the pull request and merges it before the executor looks (decision of 2026-11-16: every
		// order of the settings change, the move and the merge).
		m.prelude()
	}
	for range modelSteps {
		m.event()
		m.step(true)
	}
	m.mu.Lock()
	m.fault, m.loseNext, m.conflictNext = faultNone, false, false
	m.mu.Unlock()
	m.logf("heal")
	for range 8 {
		m.step(true)
	}
	m.settled()
	m.report()
}

// prelude moves the pull request to the configured branch and merges it there, before the first reconcile.
func (m *modelWorld) prelude() {
	if err := m.g.SetBase(m.number, freshOtherBranch); err != nil {
		m.t.Fatal(err)
	}
	sha, err := m.g.Merge(m.number)
	if err != nil {
		m.t.Fatal(err)
	}
	m.merge = sha
	if m.rng.IntN(2) == 0 {
		m.fault = faultContent
	}
	m.logf("move the pull request to the configured branch and merge it there (reads: %s)", m.fault)
}

// pr is the request's pull request as the host holds it, and whether it is hidden (deleted).
func (m *modelWorld) pr() (providers.ChangeRequest, bool) {
	for _, cr := range m.g.ChangeRequests() {
		if cr.Number == m.number {
			return cr, true
		}
	}
	return providers.ChangeRequest{}, false
}

// event changes the world at random before a reconcile.
func (m *modelWorld) event() {
	ctx := context.Background()
	cr, _ := m.pr()
	switch m.rng.IntN(11) {
	case 0, 1, 8:
		if cr.State != providers.ChangeRequestOpen || m.deleted {
			return
		}
		switch m.rng.IntN(6) {
		case 0:
			_ = m.g.CloseChangeRequest(ctx, m.number)
			m.logf("close the pull request")
		case 1, 4:
			if cr.BaseBranch == mainBranch {
				_ = m.g.SetBase(m.number, freshOtherBranch)
				m.logf("move the pull request to the configured branch")
			}
		case 2:
			m.mu.Lock()
			m.deleted = true
			m.mu.Unlock()
			m.logf("delete the pull request")
		default:
			sha, err := m.g.Merge(m.number)
			if err == nil {
				m.mu.Lock()
				m.merge = sha
				if m.rng.IntN(2) == 0 {
					// The host fails the reads of what the merge brought from the merge on.
					m.fault = faultContent
				}
				fault := m.fault
				m.mu.Unlock()
				m.logf("merge the pull request into %s (reads: %s)", cr.BaseBranch, fault)
			}
		}
	case 2, 3, 4, 9:
		m.mu.Lock()
		m.fault = modelFaults[m.rng.IntN(len(modelFaults))]
		m.mu.Unlock()
		m.logf("reads: %s", m.fault)
	case 5:
		m.mu.Lock()
		m.loseNext = true
		m.mu.Unlock()
		m.logf("lose the next status write")
	case 6:
		m.mu.Lock()
		m.conflictNext = true
		m.mu.Unlock()
		m.logf("conflict the next status write")
	case 7:
		m.restart()
	case 10:
		m.addSecond()
	}
}

// addSecond creates a second request for the same target, proposed after the first: it must wait for the lock
// (Queued with TargetLocked) while the first holds it, and is reconciled after the first from then on.
func (m *modelWorld) addSecond() {
	if m.second.Name != "" {
		return
	}
	ctx := context.Background()
	// A policy of its own that allows its file: changing the first request's policy would make its plan stale.
	pol := &api.RemediationPolicy{}
	if err := m.base.Get(ctx, client.ObjectKey{Name: string(m.template.Spec.PolicyRef)}, pol); err != nil {
		m.t.Fatal(err)
	}
	second := &api.RemediationPolicy{ObjectMeta: metav1.ObjectMeta{Name: pol.Name + "-second"}, Spec: *pol.Spec.DeepCopy()}
	second.Spec.RequestNamespaces = []api.Name{api.Name(m.key.Namespace)}
	second.Spec.GitOps.AllowedPaths = []api.GitPathPattern{appsGlob}
	second.Spec.Actions[0].SetContainerMemoryLimit.MaxIncreasePercent = 100
	if err := m.base.Create(ctx, second); err != nil {
		m.t.Fatal(err)
	}
	o := &api.RemediationRequest{ObjectMeta: metav1.ObjectMeta{Name: m.key.Name + "-second", Namespace: m.key.Namespace},
		Spec: *m.template.Spec.DeepCopy()}
	o.Spec.PolicyRef = api.Name(second.Name)
	if err := m.base.Create(ctx, o); err != nil {
		m.t.Fatal(err)
	}
	m.second = client.ObjectKeyFromObject(o)
	m.logf("a second request on the target")
}

// restart is a new executor process: nothing kept in memory, the same API server, Git host and clock.
func (m *modelWorld) restart() {
	r := m.r
	m.r = &RemediationRequestReconciler{Client: r.Client, APIReader: r.APIReader, Scheme: r.Scheme, Source: r.Source,
		OpenRepository: r.OpenRepository, Now: r.Now, CleanupTimeout: r.CleanupTimeout}
	m.logf("restart")
}

// step runs one reconcile, checks the properties, and moves the clock on by its wait.
func (m *modelWorld) step(check bool) {
	ctx := context.Background()
	m.mu.Lock()
	m.writeFaulted = false
	m.mu.Unlock()
	res, err := m.r.Reconcile(ctx, reconcile.Request{NamespacedName: m.key})
	live := &api.RemediationRequest{}
	if gerr := m.base.Get(ctx, m.key, live); gerr != nil {
		m.t.Fatal(gerr)
	}
	m.mu.Lock()
	faulted := m.writeFaulted
	if err == nil && !m.contentTried.IsZero() && m.contentFailing.IsZero() {
		m.contentFailing = m.contentTried
	}
	m.contentTried = time.Time{}
	m.mu.Unlock()
	if m.second.Name != "" {
		m.stepSecond(ctx, live, check)
	}
	retry := meta.FindStatusCondition(live.Status.Conditions, api.ConditionGitReadRetry)
	approved := meta.FindStatusCondition(live.Status.Conditions, api.ConditionApproved)
	m.logf("reconcile: phase %s, holds %t, wait %v, err %v, retry %s, approved %s", live.Status.Phase,
		holdsLock(live), res.RequeueAfter, err, reasonOf(retry), reasonOf(approved))
	if check {
		m.properties(live, res, err, faulted)
	}
	switch {
	case err != nil:
		m.clock.advance(time.Second)
	case res.RequeueAfter > 0:
		m.clock.advance(res.RequeueAfter)
	default:
		// No wait: the request is looked at again on a change; the model looks after a full recheck's period.
		m.clock.advance(heldRecheckInterval)
	}
}

// stepSecond reconciles the second request, and checks that it never holds the target while the first does.
func (m *modelWorld) stepSecond(ctx context.Context, first *api.RemediationRequest, check bool) {
	m.mu.Lock()
	m.writeFaulted = false
	m.mu.Unlock()
	// The second request's source tracks the branch the settings name now, so its policy check can admit it once
	// the target is free; its own reconciler, as the first one's resolver answers for the first's plan, and with no
	// Git host, as the limits tests run admission (limitsWorld): the model checks its lock, not its pull request.
	res := resolved()
	res.Source.TargetBranch = freshOtherBranch
	// The fake client does not serve the field selectors the lock's list uses; apiServerReader does.
	r := &RemediationRequestReconciler{Client: m.r.Client, APIReader: &apiServerReader{Reader: m.r.APIReader}, Scheme: m.r.Scheme,
		Source: &stubResolver{result: res}, Now: m.r.Now,
		CleanupTimeout: m.r.CleanupTimeout}
	_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: m.second})
	live := &api.RemediationRequest{}
	if gerr := m.base.Get(ctx, m.second, live); gerr != nil {
		m.t.Fatal(gerr)
	}
	if gerr := m.base.Get(ctx, m.key, first); gerr != nil {
		m.t.Fatal(gerr)
	}
	m.logf("reconcile the second: phase %s, holds %t, err %v, conditions %v", live.Status.Phase, holdsLock(live), err,
		live.Status.Conditions)
	if check && holdsLock(live) && holdsLock(first) {
		m.fatalf("two holders: the second request holds the target while the first does")
	}
}

func reasonOf(c *metav1.Condition) string {
	if c == nil {
		return "-"
	}
	return c.Reason
}

func (m *modelWorld) properties(live *api.RemediationRequest, res reconcile.Result, err error, faulted bool) {
	now := m.clock.at
	poll := m.r.pullRequestPollInterval()
	if retry := meta.FindStatusCondition(live.Status.Conditions, api.ConditionGitReadRetry); retry != nil &&
		retry.Status == "True" {
		if end, ok := waitKindOf(retry.Reason).end(live, now); ok && waitKindOf(retry.Reason) != waitNone &&
			now.After(end.Add(poll)) {
			m.fatalf("bound: the wait %s passed its end %v ago and stands", retry.Reason, now.Sub(end))
		}
	}
	m.mu.Lock()
	since := m.contentFailing
	m.mu.Unlock()
	if !since.IsZero() && now.After(since.Add(mergeContentWait+poll)) && !mergeRecorded(live) {
		m.fatalf("content bound: the reads of what the merge brought have failed since +%v and the request has not ended",
			since.Sub(m.start()))
	}
	if err != nil && !faulted {
		m.errorsInRow++
		if m.errorsInRow >= 3 {
			m.fatalf("errors: three reconciles in a row returned an error with no write made to fail: %v", err)
		}
	} else {
		m.errorsInRow = 0
	}
	if !holdsLock(live) {
		cr, found := m.pr()
		switch {
		case found && cr.State == providers.ChangeRequestOpen && !m.deleted:
			m.fatalf("lock open: the lock was released while the pull request is open")
		case found && cr.State == providers.ChangeRequestMerged && !mergeRecorded(live):
			m.fatalf("lock merge: the lock was released on a merge the status does not record")
		}
	}
	_ = res
}

// settled: after the world healed, the lock is free, or held by a request whose pull request is open.
func (m *modelWorld) settled() {
	live := &api.RemediationRequest{}
	if err := m.base.Get(context.Background(), m.key, live); err != nil {
		m.t.Fatal(err)
	}
	cr, found := m.pr()
	open := found && cr.State == providers.ChangeRequestOpen && !m.deleted
	switch {
	case holdsLock(live) && !open && !progresses(live):
		m.fatalf("settled lock: the lock is held after the world healed, and the pull request cannot merge (%s): phase %s",
			cr.State, live.Status.Phase)
	case found && cr.State == providers.ChangeRequestMerged && !mergeRecorded(live):
		m.fatalf("settled merge: the merge is not recorded after the world healed: phase %s", live.Status.Phase)
	}
}

// modelGit is the Git host of the model: the request's pull request hidden once deleted, and the read the model
// says failing or running out of the step's time. Reads of what a merge brought are the Parents read of the merge
// commit; the model notes when they began to fail and when one worked.
type modelGit struct {
	providers.GitProvider
	m *modelWorld
}

var errModelOutage = &providers.RetryableError{Err: errors.New("outage")}

// fail is the failure of op for the model now: nil, an outage, or the step's time run out.
func (g *modelGit) fail(ctx context.Context, op string) error {
	g.m.mu.Lock()
	fault := g.m.fault
	g.m.mu.Unlock()
	name, timeout := strings.CutSuffix(fault, faultTimeout)
	if name != op {
		return nil
	}
	if deadline, bounded := ctx.Deadline(); timeout && bounded && time.Until(deadline) < 2*time.Second {
		// The step's time (CleanupTimeout) runs out while the host does not answer.
		<-ctx.Done()
		return freshness.WrapProvider("read "+op, ctx.Err())
	}
	// A read with no short deadline of its own fails as the host's client gives up.
	return errModelOutage
}

func (g *modelGit) hidden(n int64) bool {
	g.m.mu.Lock()
	defer g.m.mu.Unlock()
	return g.m.deleted && n == g.m.number
}

func (g *modelGit) GetChangeRequest(ctx context.Context, n int64) (providers.ChangeRequest, error) {
	if err := g.fail(ctx, opGet); err != nil {
		return providers.ChangeRequest{}, err
	}
	if g.hidden(n) {
		return providers.ChangeRequest{}, fmt.Errorf("pull request %d: %w", n, providers.ErrNotFound)
	}
	return g.GitProvider.GetChangeRequest(ctx, n)
}

func (g *modelGit) ListChangeRequests(ctx context.Context, head string, openOnly bool) ([]providers.ChangeRequest, error) {
	if err := g.fail(ctx, opList); err != nil {
		return nil, err
	}
	all, err := g.GitProvider.ListChangeRequests(ctx, head, openOnly)
	return slices.DeleteFunc(all, func(cr providers.ChangeRequest) bool { return g.hidden(cr.Number) }), err
}

func (g *modelGit) FindChangeRequest(ctx context.Context, head string) (providers.ChangeRequest, error) {
	if err := g.fail(ctx, opFind); err != nil {
		return providers.ChangeRequest{}, err
	}
	cr, err := g.GitProvider.FindChangeRequest(ctx, head)
	if err == nil && g.hidden(cr.Number) {
		return providers.ChangeRequest{}, fmt.Errorf("branch %s: %w", head, providers.ErrNotFound)
	}
	return cr, err
}

func (g *modelGit) ReadFile(ctx context.Context, sha, path string, limit int) (providers.File, error) {
	if err := g.fail(ctx, opReadFile); err != nil {
		return providers.File{}, err
	}
	return g.GitProvider.ReadFile(ctx, sha, path, limit)
}

func (g *modelGit) Parents(ctx context.Context, sha string) ([]string, error) {
	g.m.mu.Lock()
	content := sha == g.m.merge && g.m.merge != ""
	g.m.mu.Unlock()
	if !content {
		return g.GitProvider.Parents(ctx, sha)
	}
	if err := g.fail(ctx, faultContent); err != nil {
		g.m.mu.Lock()
		if g.m.contentTried.IsZero() {
			g.m.contentTried = g.m.clock.at
		}
		g.m.mu.Unlock()
		return nil, err
	}
	g.m.mu.Lock()
	g.m.contentFailing, g.m.contentTried = time.Time{}, time.Time{}
	g.m.mu.Unlock()
	return g.GitProvider.Parents(ctx, sha)
}
