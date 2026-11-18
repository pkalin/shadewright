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
	"github.com/pkalin/shadewright/internal/gitops/sourcemap"
	"github.com/pkalin/shadewright/internal/policy/freshness"
	"github.com/pkalin/shadewright/internal/providers"
	fakegit "github.com/pkalin/shadewright/internal/providers/fake"
)

// The generated test of the decisions of 2026-11-17 and 2026-11-18 on #771: a small model of the world around one
// request, driven through Reconcile with a moving clock over seeded random orders of events. It uses only what the
// request shows (its status, conditions, phase, lock) and what the model injects, so it runs on any build of the
// branch.
//
// The world: the request starts waiting for review, in PolicyChecked before its pull request is recorded (with a
// deadline a few minutes away), or stopped (for the branch change on PlanFresh, on Approved, or with its pull
// request never recorded because that status write was lost, cancelled for its Incident, or stopped for another
// reason). The branch the settings name now may be the longest name the API allows. Its pull request is open,
// closed, merged into the recorded branch, moved to the configured branch (and merged there), deleted (gone from the
// host), or none at all; a merge may be reported without its commit for a while. Between reconciles: one or two Git
// reads fail or run out of the step's time (the pull request reads, the listing, the read of what a merge brought, a
// file, the opening of the repository), or reads work again; the repository leaves the settings or comes back; the
// kill switch pauses writes or lifts the pause; a person cancels the request; the settings name another target
// branch; the next status write is lost, meets a conflicting write, or is applied and reported failed; the cache
// serves an older copy of the request at the next read; the executor restarts (nothing kept in memory). The clock
// moves on by the wait each reconcile returns (a second after an error). Every status write is checked against the
// schema of the CRD, as the API server does: a write it would refuse is a violation.
//
// After each reconcile:
//   - pause: a reconcile that starts while writes are paused calls the Git host nothing (opening the repository
//     included) and leaves the phase of a request whose merge is not recorded as it was: a pause stops the look at a recorded pull request, the settlement,
//     the cancel watch, the deadline's end and the end of every other wait (sixth decision of 2026-11-18 on #771);
//   - lift: the first reconcile after the pause is lifted ends every wait that passed meanwhile (time paused counts
//     against a started bound) and, with reads working and no write made to fail, settles the lock of a stopped holder
//     whose pull request is closed, before any other step;
//   - waits (not while paused): every entry of status.gitWaits has not passed the end of its kind by more than one poll interval: the
//     request deadline (RequestDeadline), the deadline plus the handoff grace (HandoffGrace), five minutes after its
//     start or the merge clock (MergeCheck), five minutes after its start (MergeContent);
//   - clocks (not while paused): every time field of the status that bounds a wait has not passed its end by more than one poll
//     interval (gitWaitClocks): a request in PolicyChecked past its deadline, a merge seen without its commit
//     (mergeCommitAwaitedSince) whose merge is not recorded;
//   - content (not while paused): the reads of what a merge brought that kept failing since a time T end the request
//     (MergeContentUnreadable or the merge recorded) by T plus mergeContentWait plus one poll interval;
//   - errors: no three reconciles in a row return an error while no write is made to fail;
//   - pace: no three reconciles in a row ask to be run again within a second without an error (a wait that ends
//     and is not ended runs hot, and the clock that bounds it never moves);
//   - record: a merge the status records names its pull request;
//   - lock: the lock is never released while a pull request of the request is open, nor on a merge the status does
//     not record, checked at the moment of the release (a merge into a branch the plan's change was not for, seen
//     only after the settings moved, is recorded as released aside), and a lock once released never comes back
//     while the request does not hold it;
//   - ending: a reconcile that starts with a wait past its end (status.gitWaits) calls the Git host nothing before
//     its first status write: the end decides from recorded state alone (fifth decision of 2026-11-18 on #771, point
//     5 as the sixth kept it);
//   - schema: no status write is one the API server refuses.
//
// At the end the world heals (reads work, no write fails; the repository comes back in half of the runs; a pause is
// lifted, after a few more reconciles under it in half of the runs and always under the pause focus) and, after a few
// reconciles, the lock is free, or held by a request whose pull request is open and can still merge; a merge
// recorded with its receipt never holds it, whether the repository came back or not; a merge is recorded; and a
// cancelled request whose pull request is closed or gone holds no lock (settled cancel).
//
// Each seed has a focus (seed modulo the number of focuses, modelFocuses): none, or one that makes an order of the
// fourth or fifth decision of 2026-11-18 on #771 likely (a deadline that passes while the repository cannot be
// opened, a merge reported without its commit while the reads fail, a merge recorded with its receipt and the
// repository gone, the cache serving a copy that held the lock after it was released, a retargeted merge into the
// longest branch name whose reads fail, a cancel with a branch change and a pull request closed, deleted or merged by
// hand, writes paused while the request is stopped or waits for its deadline and a person closes or merges its pull
// request). A focus only weights the start and the events; every event can still happen.
//
// Seeds: modelSeeds in CI (a fixed set, fewer under the race detector or -short); SHADEWRIGHT_MODEL_SEEDS=n runs n
// more seeds from the time, SHADEWRIGHT_MODEL_SEED=s one seed and SHADEWRIGHT_MODEL_SEED_RANGE=a-b the seeds a to b
// instead of the fixed set. A failure prints its seed and the trace; SHADEWRIGHT_MODEL_TRACE=1 prints it always.

const (
	modelSteps = 24

	faultNone    = "none"
	faultContent = "content"
	faultOpen    = "open"
	// faultBetween fails the reads that compare what a merge brought (CheckBetween, CountAhead), after the merge
	// commit's parents were read.
	faultBetween = "between"
	faultTimeout = "+timeout"
	// faultBoth joins two faults of one pass.
	faultBoth = "&"

	// startRunning: the request waits for review, not stopped.
	startRunning = "running"
	// startNoPullRequest: no pull request exists at all (the open's write was lost and it was deleted).
	startNoPullRequest = "no-pull-request-at-all"
	// startPolicyChecked: the request is in PolicyChecked, its deadline minutes away, and its pull request was opened
	// while the write that would record it was lost; the settings name the plan's branch.
	startPolicyChecked = "policy-checked"

	// modelPolicyDeadline is how far the deadline of a request that starts in PolicyChecked lies.
	modelPolicyDeadline = 3 * time.Minute

	focusNone     = "none"
	focusDeadline = "deadline"
	focusCommit   = "commit"
	focusReceipt  = "receipt"
	focusStale    = "stale"
	focusLong     = "long"
	// focusCancel: a person cancels a request whose target branch then changes (or changed), and its pull request is
	// closed, deleted, or moved to the configured branch and merged there.
	focusCancel = "cancel"
	// focusPause: the kill switch pauses writes early and the pause mostly lasts, while the pull request is closed,
	// deleted or merged and a deadline passes.
	focusPause = "pause"
)

var (
	modelStarts = []string{startRunning, stopPlanFresh, stopApproved, stopUnrecord, stopCancelled, stopOther,
		startNoPullRequest, startPolicyChecked}
	modelFaults = []string{faultNone, opGet, opList, faultContent, opReadFile, faultOpen, opGet + faultTimeout,
		faultContent + faultTimeout, faultBetween, opGet + faultBoth + faultContent, faultOpen + faultBoth + opList}
	// longBranch is the longest target branch name the schema allows (api.GitBranch).
	longBranch = strings.Repeat("r", 255)
	// modelFocuses are the focuses of the seeds, in the order of seed modulo their number.
	modelFocuses = []string{focusNone, focusDeadline, focusCommit, focusReceipt, focusStale, focusLong, focusCancel,
		focusPause}
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
	n := 240
	if testing.Short() || raceDetector {
		// Each order of the fourth decision of 2026-11-18 on #771 is found in this range on the build before its fix.
		n = 90
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
	schema := requestStatusSchema(t)
	for _, seed := range modelSeeds(t) {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			t.Parallel()
			m := newModel(t, seed, schema)
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
	schema   *statusSchema
	// configured is the target branch the settings name now; repo is the settings' repository entry as it was
	// before it left them.
	configured string
	repo       *api.GitRepository

	mu sync.Mutex
	// fault is the Git read that fails (or runs out of the step's time), faultNone when reads work; two faults of
	// one pass are joined by faultBoth.
	fault string
	// loseNext and conflictNext make the next status write fail, or meet a write made between its read and it;
	// applyNext applies it and reports it failed.
	loseNext, conflictNext, applyNext bool
	// staleNext makes the cache serve stale at the next read of the request.
	staleNext bool
	stale     *api.RemediationRequest
	// writeFaulted reports that this reconcile met an injected write fault or a stale read.
	writeFaulted bool
	// deleted hides the pull request from every read (gone from the host).
	deleted bool
	// noCommit makes the host report the merge without its merge commit.
	noCommit bool
	number   int64
	merge    string
	// contentFailing is when the reads of what the merge brought began to fail without a success since, from the
	// first failure in a reconcile that ended without an error (a wait whose status write failed is not started:
	// the reconcile returns the error for a retry); zero when they work, or were never made. contentTried is the
	// first failure of the reconcile running.
	contentFailing, contentTried time.Time
	errorsInRow, hotInRow        int
	startAt                      time.Time
	// released: the request's lock was seen released; recorded: the request's merge was seen recorded.
	released, recorded bool
	// releasedAside: at the release the pull request was merged into a branch that delivered nothing.
	releasedAside bool
	// handoff: the request is a replacement recovering a handoff; the pull request is its predecessor's, which
	// holds the lock on it, so the lock properties about the request's own pull request do not apply.
	handoff bool
	// lastingUntil keeps the fault as it is until then (an outage that lasts): fault events wait.
	lastingUntil time.Time
	// history is every copy of the request the API server held after a reconcile, for the stale cache.
	history    []*api.RemediationRequest
	violations []string
	// focus weights the events (modelFocuses).
	focus string
	// paused: the kill switch pauses writes now.
	paused bool
	// calls counts the calls to the Git host (opening the repository included) in the reconcile running; passed is
	// the kind of a Git wait of the request that had passed its end when that reconcile began, and ended whether the
	// first status write of that reconcile was made (property "ending").
	calls  int
	passed string
	ended  bool
	// lifted: the pause was lifted since the last reconcile (property "lift"); liftedAt is when the last pause was
	// lifted: an end that passed during it is due by then (afterPause).
	lifted             bool
	pausedAt, liftedAt time.Time
}

// modelStart is how a run of the model begins: drawn from the seed (drawStart), or given by a scripted test.
type modelStart struct {
	start string
	// long: the branch the settings name now is the longest name the schema allows.
	long bool
	// fresh: a running request whose settings name the plan's branch, so it watches a fresh plan.
	fresh bool
	// open: the repository cannot be opened from the start (a token that cannot be minted), lasting the minutes.
	open    bool
	lasting time.Duration
	// handoff: the request in PolicyChecked is a replacement recovering a handoff (with open).
	handoff bool
	verify  bool
	// focus weights the events of the run (modelFocuses); focusNone when empty.
	focus string
}

// drawStart draws a start from rng for a run with focus.
func drawStart(rng *rand.Rand, focus string) modelStart {
	switch focus {
	case focusDeadline:
		return modelStart{start: startPolicyChecked, open: true, lasting: time.Duration(5+rng.IntN(26)) * time.Minute,
			handoff: rng.IntN(4) == 0, verify: rng.IntN(2) == 0, focus: focus}
	case focusCommit, focusReceipt, focusStale:
		return modelStart{start: startRunning, fresh: true, verify: focus != focusCommit || rng.IntN(2) == 0,
			focus: focus}
	case focusLong:
		starts := []string{startRunning, stopPlanFresh, stopApproved, stopUnrecord}
		return modelStart{start: starts[rng.IntN(len(starts))], long: true, verify: rng.IntN(2) == 0, focus: focus}
	case focusCancel:
		// Waiting for review with its pull request recorded (on a fresh plan, so the branch changes later), or in
		// PolicyChecked with the record of its pull request lost.
		starts := []string{startRunning, startPolicyChecked}
		st := modelStart{start: starts[rng.IntN(len(starts))], verify: rng.IntN(2) == 0, focus: focus}
		st.fresh = st.start == startRunning && rng.IntN(3) > 0
		return st
	case focusPause:
		starts := []string{stopPlanFresh, stopApproved, stopUnrecord, startPolicyChecked, startRunning, stopOther}
		return modelStart{start: starts[rng.IntN(len(starts))], verify: rng.IntN(2) == 0, focus: focus}
	}
	st := modelStart{start: modelStarts[rng.IntN(len(modelStarts))], focus: focusNone}
	st.long = st.start != startPolicyChecked && rng.IntN(3) == 0
	st.fresh = st.start == startRunning && rng.IntN(2) == 0
	if st.start == startPolicyChecked && rng.IntN(2) == 0 {
		st.open, st.lasting = true, time.Duration(rng.IntN(30))*time.Minute
		st.handoff = rng.IntN(2) == 0
	}
	st.verify = rng.IntN(2) == 0
	return st
}

func newModel(t *testing.T, seed uint64, schema *statusSchema) *modelWorld {
	// #nosec G404 -- a seeded generator, so a failing order can be run again from its seed.
	rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	return newModelFrom(t, seed, rng, schema, drawStart(rng, modelFocuses[seed%uint64(len(modelFocuses))]))
}

func newModelFrom(t *testing.T, seed uint64, rng *rand.Rand, schema *statusSchema, st modelStart) *modelWorld {
	m := &modelWorld{t: t, seed: seed, rng: rng, fault: faultNone, schema: schema, configured: freshOtherBranch,
		focus: st.focus}
	start := st.start
	phase := api.PhaseNeedsIntervention
	switch start {
	case startRunning:
		phase = api.PhaseAwaitingApproval
	case stopCancelled:
		phase = api.PhaseCancelled
	case startPolicyChecked:
		phase = api.PhasePolicyChecked
	}
	r, g, rr := settlementWorld(t, phase)
	m.r, m.g, m.key = r, g, client.ObjectKeyFromObject(rr)
	m.template = rr.DeepCopy()
	m.number = rr.Status.PullRequest.Number
	ctx := context.Background()
	if st.long {
		m.setConfigured(longBranch)
	}
	switch start {
	case startRunning:
		if st.fresh {
			m.setConfigured(mainBranch)
			m.freshPlan(rr)
		}
		meta.RemoveStatusCondition(&rr.Status.Conditions, api.ConditionApproved)
		if err := r.Status().Update(ctx, rr); err != nil {
			t.Fatal(err)
		}
	case stopCancelled:
		orderStop(t, r, rr, stopCancelled)
	case startNoPullRequest:
		lostRecord(t, r, rr, api.ConditionPROpened)
		m.deleted = true
	case startPolicyChecked:
		m.setConfigured(mainBranch)
		m.freshPlan(rr)
		rr.Status.PullRequest = nil
		meta.RemoveStatusCondition(&rr.Status.Conditions, api.ConditionApproved)
		rr.Status.Deadline = &metav1.Time{Time: time.Now().Truncate(time.Second).Add(modelPolicyDeadline)}
		if err := r.Status().Update(ctx, rr); err != nil {
			t.Fatal(err)
		}
		if st.open {
			m.fault = faultOpen
			m.lastingUntil = time.Now().Add(st.lasting)
			if st.handoff {
				m.recovering(rr)
			}
		}
	default:
		orderStop(t, r, rr, start)
	}
	r.VerifyOutcomes = st.verify
	m.logf("start %+v", st)
	m.clock = clockOf(r)
	m.startAt = m.clock.at
	m.base = r.Client.(client.WithWatch)
	direct := interceptor.NewClient(m.base, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object,
			opts ...client.SubResourceUpdateOption) error {
			return m.write(ctx, obj, func() error { return c.SubResource(sub).Update(ctx, obj, opts...) })
		},
		SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch,
			opts ...client.SubResourcePatchOption) error {
			return m.write(ctx, obj, func() error { return c.SubResource(sub).Patch(ctx, obj, patch, opts...) })
		},
	})
	// The cache: the request may be served as an older copy at the next read (staleNext).
	cache := interceptor.NewClient(direct, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if rr, ok := obj.(*api.RemediationRequest); ok && key == m.key {
				m.mu.Lock()
				stale := m.stale
				if m.staleNext && stale != nil {
					m.staleNext, m.writeFaulted = false, true
					m.mu.Unlock()
					stale.DeepCopyInto(rr)
					return nil
				}
				m.mu.Unlock()
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
	r.Client, r.APIReader = cache, direct
	r.Source = &modelResolver{m: m}
	r.OpenRepository = func(ctx context.Context, repo api.GitRepository) (providers.GitProvider, error) {
		if err := (&modelGit{m: m}).fail(ctx, faultOpen); err != nil {
			return nil, err
		}
		return &modelGit{GitProvider: g.ForTarget(string(repo.TargetBranch)), m: m}, nil
	}
	// Long enough for every step that does not wait on a slow read, also under the race detector.
	r.CleanupTimeout = 150 * time.Millisecond
	return m
}

// freshPlan makes rr's plan the one made on the head of its target branch, as the source mapping finds it
// (modelResolver), and the pull request carry its marker (request UID and plan digest).
func (m *modelWorld) freshPlan(rr *api.RemediationRequest) {
	ctx := context.Background()
	head, err := m.g.BranchHead(ctx, string(rr.Status.Source.TargetBranch))
	if err != nil {
		m.t.Fatal(err)
	}
	pol := &api.RemediationPolicy{}
	settings := &api.RemediationSettings{}
	if err := m.r.Get(ctx, client.ObjectKey{Name: string(rr.Spec.PolicyRef)}, pol); err != nil {
		m.t.Fatal(err)
	}
	if err := m.r.Get(ctx, client.ObjectKey{Name: api.SettingsName}, settings); err != nil {
		m.t.Fatal(err)
	}
	rr.Status.Source.BaseCommit = head
	if rr.Status.PlanDigest, err = freshness.ComputeDigest(rr, rr.Status.Source, pol, settings); err != nil {
		m.t.Fatal(err)
	}
	if err := m.g.SetBody(m.number, marker(rr)); err != nil {
		m.t.Fatal(err)
	}
}

// recovering makes rr a replacement recovering the handoff of a predecessor ("rr-old") that recorded its intent to
// hand rr its pull request: the recovery reads the branch until it learns whether rr's commit is on it, bounded by
// the deadline and the handoff grace (HandoffGrace).
func (m *modelWorld) recovering(rr *api.RemediationRequest) {
	ctx := context.Background()
	cr, _ := m.pr()
	pred := &api.RemediationRequest{ObjectMeta: metav1.ObjectMeta{Name: rr.Name + "-old", Namespace: rr.Namespace,
		UID: "11111111-2222-4333-8444-555555555555"}, Spec: *rr.Spec.DeepCopy()}
	h := api.Hint{Source: api.HintSourceGitHub, CommentID: 7, Digest: "sha256:" + strings.Repeat("b", 64),
		State: api.HintReserved, Author: api.HintAuthor{ID: 1001, Login: "reviewer"}}
	if err := m.r.Create(ctx, pred); err != nil {
		m.t.Fatal(err)
	}
	pred.Status = *rr.Status.DeepCopy()
	pred.Status.Phase = api.PhaseAwaitingApproval
	pred.Status.HoldsTarget = new(true)
	pred.Status.Hints = []api.Hint{h}
	pred.Status.PullRequest = &api.PullRequestStatus{Number: cr.Number, URL: cr.URL, HeadSHA: cr.HeadSHA}
	pred.Status.Handoff = &api.Handoff{To: &api.OutgoingHandoff{Name: api.Name(rr.Name), UID: api.UID(rr.UID),
		Hint: refOf(&h), ExpectedHead: cr.HeadSHA, Phase: api.HandoffIntent}}
	if err := m.r.Status().Update(ctx, pred); err != nil {
		m.t.Fatal(err)
	}
	rr.Spec.Replaces = &api.RequestReplacement{Name: api.Name(pred.Name), UID: api.UID(pred.UID), Hint: refOf(&h)}
	if err := m.r.Update(ctx, rr); err != nil {
		m.t.Fatal(err)
	}
	rr.Status.Handoff = &api.Handoff{From: &api.IncomingHandoff{Name: api.Name(pred.Name), UID: api.UID(pred.UID),
		Hint: refOf(&h), Phase: api.HandoffReserved}}
	if err := m.r.Status().Update(ctx, rr); err != nil {
		m.t.Fatal(err)
	}
	m.r.ReviewerHints = true
	m.handoff = true
}

// setConfigured makes branch the target branch the settings name now (created on the host when missing).
func (m *modelWorld) setConfigured(branch string) {
	ctx := context.Background()
	if _, err := m.g.BranchHead(ctx, branch); err != nil {
		head, err := m.g.BranchHead(ctx, mainBranch)
		if err != nil {
			m.t.Fatal(err)
		}
		if err := m.g.CreateBranch(ctx, branch, head); err != nil {
			m.t.Fatal(err)
		}
	}
	m.updateSettings(func(s *api.RemediationSettings) {
		for i := range s.Spec.Repositories {
			s.Spec.Repositories[i].TargetBranch = api.GitBranch(branch)
		}
		if m.repo != nil {
			m.repo.TargetBranch = api.GitBranch(branch)
		}
	})
	m.configured = branch
}

func (m *modelWorld) updateSettings(change func(*api.RemediationSettings)) {
	ctx := context.Background()
	s := &api.RemediationSettings{}
	c := m.r.Client
	if m.base != nil {
		c = m.base
	}
	if err := c.Get(ctx, client.ObjectKey{Name: api.SettingsName}, s); err != nil {
		m.t.Fatal(err)
	}
	change(s)
	if err := c.Update(ctx, s); err != nil {
		m.t.Fatal(err)
	}
}

// write makes the status write do, unless the model makes it fail, conflict, or apply and report a failure; a write
// the API server would refuse (the CRD schema) is refused, and noted.
func (m *modelWorld) write(ctx context.Context, obj client.Object, do func() error) error {
	if rr, ok := obj.(*api.RemediationRequest); ok {
		if errs := m.schema.validate(rr); len(errs) > 0 {
			m.fatalf("schema: the API server refuses a status write of %s: %v", rr.Name, errs)
			return apierrors.NewInvalid(api.GroupVersion.WithKind("RemediationRequest").GroupKind(), rr.Name, errs)
		}
	}
	m.mu.Lock()
	if rr, ok := obj.(*api.RemediationRequest); ok && rr.Name == m.key.Name && m.passed != "" && !m.ended {
		m.ended = true
		if m.calls > 0 {
			passed, calls := m.passed, m.calls
			m.mu.Unlock()
			m.fatalf("ending: the %s wait had passed its end and the reconcile called the Git host %d times before "+
				"its first status write", passed, calls)
			m.mu.Lock()
		}
	}
	lose, conflict, apply := m.loseNext, m.conflictNext, m.applyNext
	m.loseNext, m.conflictNext, m.applyNext = false, false, false
	if lose || conflict || apply {
		m.writeFaulted = true
	}
	m.mu.Unlock()
	switch {
	case lose:
		return apierrors.NewServiceUnavailable("lost")
	case apply:
		if err := do(); err != nil {
			return err
		}
		return apierrors.NewTimeoutError("applied, the answer lost", 1)
	case conflict:
		live := &api.RemediationRequest{}
		if err := m.base.Get(ctx, client.ObjectKeyFromObject(obj), live); err != nil {
			return err
		}
		if live.Annotations == nil {
			live.Annotations = map[string]string{}
		}
		live.Annotations["example.test/model"] = strconv.Itoa(len(live.Annotations["example.test/model"]) + 1)
		if err := m.base.Update(ctx, live); err != nil {
			return err
		}
	}
	return do()
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
		if strings.HasPrefix(v, kind+":") {
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
	if m.focus == focusCancel && m.rng.IntN(4) > 0 {
		// The cancel and the branch change come before the executor first looks: a pull request whose record was
		// lost is not found before them.
		m.cancel()
		m.setConfigured(freshOtherBranch)
		m.logf("the settings name target branch %s", freshOtherBranch)
	}
	if !m.deleted && m.focus != focusCancel && (m.focus == focusLong || m.rng.IntN(3) == 0) {
		// A person moves the pull request and merges it before the executor looks (decision of 2026-11-16: every
		// order of the settings change, the move and the merge).
		m.prelude()
	}
	// The copy the API server held before the first reconcile, for the stale cache.
	m.history = append(m.history, m.live())
	for range modelSteps {
		m.event()
		m.step()
	}
	m.mu.Lock()
	m.fault, m.loseNext, m.conflictNext, m.applyNext, m.staleNext, m.noCommit = faultNone, false, false, false, false, false
	m.lastingUntil = time.Time{}
	m.mu.Unlock()
	if m.paused && (m.focus == focusPause || m.rng.IntN(2) == 0) {
		// A pause stops everything (sixth decision of 2026-11-18 on #771): the healed world changes nothing while it
		// lasts, and the reconcile after the lift settles what passed meanwhile.
		m.logf("the pause stays a while")
		for range 3 {
			m.step()
		}
	}
	if m.paused {
		m.pause(false)
	}
	if m.repo != nil && m.rng.IntN(2) == 0 {
		m.restoreRepository()
	}
	m.logf("heal (repository in the settings: %t, writes paused: %t)", m.repo == nil, m.paused)
	for range 8 {
		m.step()
	}
	m.settled()
	m.report()
}

// prelude moves the pull request to the configured branch and merges it there, before the first reconcile.
func (m *modelWorld) prelude() {
	if err := m.g.SetBase(m.number, m.configured); err != nil {
		m.t.Fatal(err)
	}
	sha, err := m.g.Merge(m.number)
	if err != nil {
		m.t.Fatal(err)
	}
	m.merge = sha
	switch {
	case m.focus == focusLong:
		// What the merge into the longest branch name brought cannot be compared, for a while.
		m.fault = []string{faultBetween, faultContent}[m.rng.IntN(2)]
		m.lastingUntil = m.clock.at.Add(time.Duration(m.rng.IntN(10)) * time.Minute)
	case m.rng.IntN(2) == 0 && !time.Now().Before(m.lastingUntil):
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
	if m.focusEvent(cr) {
		return
	}
	switch m.rng.IntN(18) {
	case 0, 1, 8:
		if cr.State != providers.ChangeRequestOpen || m.deleted {
			return
		}
		switch m.rng.IntN(6) {
		case 0:
			_ = m.g.CloseChangeRequest(ctx, m.number)
			m.logf("close the pull request")
		case 1, 4:
			if cr.BaseBranch == mainBranch && m.configured != mainBranch {
				_ = m.g.SetBase(m.number, m.configured)
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
				if m.rng.IntN(2) == 0 && !m.clock.at.Before(m.lastingUntil) {
					// The host fails the reads of what the merge brought (or every read) from the merge on, for a while.
					m.fault = []string{faultContent, faultBetween, opGet, opReadFile}[m.rng.IntN(4)]
					m.lastingUntil = m.clock.at.Add(time.Duration(m.rng.IntN(20)) * time.Minute)
				}
				// The host reports the merge without its commit for a while.
				m.noCommit = m.rng.IntN(3) == 0
				fault, noCommit := m.fault, m.noCommit
				m.mu.Unlock()
				m.logf("merge the pull request into %.12s (reads: %s, merge commit withheld: %t)", cr.BaseBranch,
					fault, noCommit)
			}
		}
	case 2, 3, 4, 9:
		if m.clock.at.Before(m.lastingUntil) {
			return
		}
		m.mu.Lock()
		m.fault = modelFaults[m.rng.IntN(len(modelFaults))]
		if m.rng.IntN(3) == 0 {
			m.lastingUntil = m.clock.at.Add(time.Duration(m.rng.IntN(20)) * time.Minute)
		}
		m.mu.Unlock()
		m.logf("reads: %s (lasting until +%v)", m.fault, max(m.lastingUntil.Sub(m.start()), 0))
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
	case 11:
		if m.repo == nil {
			m.removeRepository()
		} else {
			m.restoreRepository()
		}
	case 12:
		m.mu.Lock()
		m.applyNext = true
		m.mu.Unlock()
		m.logf("apply the next status write and report it failed")
	case 13:
		m.serveStale()
	case 14:
		m.stop()
	case 15:
		m.mu.Lock()
		if m.noCommit {
			m.noCommit = false
			m.logf("the host reports the merge commit")
		}
		m.mu.Unlock()
	case 16:
		if m.clock.at.Before(m.lastingUntil) {
			return
		}
		// Two faults in one pass: a read fails and the next status write is lost.
		m.mu.Lock()
		m.fault, m.loseNext = modelFaults[1+m.rng.IntN(len(modelFaults)-1)], true
		m.mu.Unlock()
		m.logf("reads: %s, and lose the next status write", m.fault)
	case 17:
		m.pause(!m.paused)
	}
}

// pause sets or lifts the kill switch's pause of writes.
func (m *modelWorld) pause(on bool) {
	m.updateSettings(func(s *api.RemediationSettings) { s.Spec.Paused = new(on) })
	switch {
	case m.paused && !on:
		m.lifted, m.liftedAt = true, m.clock.at
	case !m.paused && on && m.clock != nil:
		m.pausedAt = m.clock.at
	}
	m.paused = on
	m.logf("the kill switch: writes paused %t", on)
}

// commitEvent is the event of focusCommit: a merge without its commit, then reads that fail for a while once the
// executor saw it; mostly quiet while that lasts. ok is false when it draws none.
func (m *modelWorld) commitEvent(open bool) (done, ok bool) {
	live := m.live()
	switch {
	case open && m.rng.IntN(2) == 0:
		// Merged without its commit.
		sha, err := m.g.Merge(m.number)
		if err != nil {
			return false, true
		}
		m.mu.Lock()
		m.merge, m.noCommit = sha, true
		m.mu.Unlock()
		m.logf("merge the pull request without its commit")
		return true, true
	case live.Status.MergeCommitAwaitedSince != nil && !mergeRecorded(live) && m.lastingUntil.IsZero():
		// The executor saw the merge without its commit; from now on the reads fail, for a while.
		m.mu.Lock()
		m.fault = []string{opGet, opGet + faultTimeout, faultOpen}[m.rng.IntN(3)]
		m.lastingUntil = m.clock.at.Add(time.Duration(6+m.rng.IntN(20)) * time.Minute)
		m.mu.Unlock()
		m.logf("reads: %s until +%v", m.fault, m.lastingUntil.Sub(m.start()))
		return true, true
	case m.clock.at.Before(m.lastingUntil):
		// Mostly quiet while the outage lasts.
		return m.rng.IntN(4) > 0, true
	}
	return false, false
}

// cancelEvent is the event of focusCancel: the cancel first, then the change of the target branch, then a close,
// a deletion or a merge by hand while the pull request is open; mostly quiet otherwise.
func (m *modelWorld) cancelEvent(cr providers.ChangeRequest, open bool) bool {
	switch {
	case !m.live().Spec.Cancel:
		m.cancel()
		return true
	case m.configured == mainBranch:
		// The plan's branch is main (the start's).
		m.setConfigured(freshOtherBranch)
		m.logf("the settings name target branch %s", freshOtherBranch)
		return true
	case open && m.rng.IntN(2) == 0:
		m.byHand(cr, m.rng.IntN(4))
		return true
	}
	return m.rng.IntN(3) > 0
}

// pauseEvent is the event of focusPause: the pause, then a close, a deletion or a merge by hand while it lasts and
// the pull request is open; the pause mostly lasts.
func (m *modelWorld) pauseEvent(cr providers.ChangeRequest, open bool) bool {
	switch {
	case !m.paused && m.rng.IntN(2) == 0:
		m.pause(true)
		return true
	case m.paused && open && m.rng.IntN(3) == 0:
		m.byHand(cr, m.rng.IntN(4))
		return true
	case m.paused:
		return m.rng.IntN(3) > 0
	}
	return false
}

// focusEvent is the event of the run's focus, when it draws one; false leaves the event to the common draw.
func (m *modelWorld) focusEvent(cr providers.ChangeRequest) bool {
	open := cr.State == providers.ChangeRequestOpen && !m.deleted
	switch m.focus {
	case focusDeadline:
		// Mostly quiet until the deadline and a poll interval after it have passed, while the outage lasts.
		quiet := m.clock.at.Before(m.start().Add(modelPolicyDeadline + 2*DefaultPullRequestPollInterval))
		return quiet && m.rng.IntN(4) > 0
	case focusCommit:
		if done, ok := m.commitEvent(open); ok {
			return done
		}
	case focusReceipt:
		switch {
		case open && m.rng.IntN(2) == 0:
			sha, err := m.g.Merge(m.number)
			if err != nil {
				return false
			}
			m.mu.Lock()
			m.merge = sha
			m.mu.Unlock()
			m.logf("merge the pull request with its commit")
			return true
		case cr.State == providers.ChangeRequestMerged && m.rng.IntN(3) == 0:
			if m.repo == nil {
				m.removeRepository()
			} else {
				m.restart()
			}
			return true
		}
	case focusCancel:
		return m.cancelEvent(cr, open)
	case focusPause:
		return m.pauseEvent(cr, open)
	case focusStale:
		if !m.released {
			// First the lock is released: the pull request is merged and verified (WaitingForDelivery holds the
			// lock), then an executor without verification ends it.
			switch {
			case open && m.rng.IntN(2) == 0:
				sha, err := m.g.Merge(m.number)
				if err != nil {
					return false
				}
				m.mu.Lock()
				m.merge, m.fault = sha, faultNone
				m.mu.Unlock()
				m.logf("merge the pull request with its commit")
				return true
			case cr.State == providers.ChangeRequestMerged && m.r.VerifyOutcomes && m.rng.IntN(3) == 0:
				m.restart()
				m.r.VerifyOutcomes = false
				m.logf("verification off")
				return true
			}
			return m.rng.IntN(2) == 0
		}
		if m.rng.IntN(2) == 0 {
			// Then the cache serves an older copy while a read fails, so a helper that writes the Git waits runs on it.
			m.serveStale()
			if !m.clock.at.Before(m.lastingUntil) {
				m.mu.Lock()
				m.fault = []string{opGet, opList, faultContent, faultOpen}[m.rng.IntN(4)]
				m.mu.Unlock()
				m.logf("reads: %s", m.fault)
			}
			return true
		}
	}
	return false
}

// cancel is a person's cancel of the request (spec.cancel).
func (m *modelWorld) cancel() {
	live := &api.RemediationRequest{}
	if err := m.base.Get(context.Background(), m.key, live); err != nil {
		m.t.Fatal(err)
	}
	live.Spec.Cancel = true
	if err := m.base.Update(context.Background(), live); err != nil {
		m.t.Fatal(err)
	}
	m.logf("a person cancels the request")
}

// byHand is what a person does to the open pull request cr: close it (0), delete it (1), or move it to the configured
// branch and merge it there (2), or merge it where it is (3).
func (m *modelWorld) byHand(cr providers.ChangeRequest, what int) {
	ctx := context.Background()
	switch what {
	case 0:
		_ = m.g.CloseChangeRequest(ctx, m.number)
		m.logf("close the pull request by hand")
	case 1:
		m.mu.Lock()
		m.deleted = true
		m.mu.Unlock()
		m.logf("delete the pull request")
	default:
		if what == 2 && cr.BaseBranch != m.configured {
			_ = m.g.SetBase(m.number, m.configured)
		}
		sha, err := m.g.Merge(m.number)
		if err != nil {
			return
		}
		m.mu.Lock()
		m.merge = sha
		m.mu.Unlock()
		m.logf("merge the pull request into %.12s", m.configured)
	}
}

// serveStale makes the cache serve an older copy of the request at the next read: one that held the lock when the
// lock was seen released, else one that recorded a Git wait when there is one (what the helpers that write waits
// read again), any older one otherwise.
func (m *modelWorld) serveStale() {
	if len(m.history) < 2 {
		return
	}
	older := m.history[:len(m.history)-1]
	var waiting, holding []*api.RemediationRequest
	for _, o := range older {
		if len(o.Status.GitWaits) > 0 {
			waiting = append(waiting, o)
		}
		if holdsLock(o) {
			holding = append(holding, o)
		}
	}
	switch {
	case m.released && len(holding) > 0 && m.rng.IntN(2) == 0:
		older = holding
	case len(waiting) > 0 && m.rng.IntN(3) > 0:
		older = waiting
	}
	m.mu.Lock()
	m.stale, m.staleNext = older[m.rng.IntN(len(older))], true
	m.mu.Unlock()
	m.logf("the cache serves an older copy of the request at the next read (phase %s, waits %d)",
		m.stale.Status.Phase, len(m.stale.Status.GitWaits))
}

// stop stops the request from outside: the kill switch, a person's cancel, or another target branch in the
// settings.
func (m *modelWorld) stop() {
	switch m.rng.IntN(3) {
	case 0:
		m.pause(!m.paused)
	case 1:
		m.cancel()
	default:
		branch := freshOtherBranch
		if m.configured == freshOtherBranch {
			branch = mainBranch
		}
		m.setConfigured(branch)
		m.logf("the settings name target branch %s", branch)
	}
}

// removeRepository takes the repository entry out of the settings.
func (m *modelWorld) removeRepository() {
	m.updateSettings(func(s *api.RemediationSettings) {
		m.repo = s.Spec.Repositories[0].DeepCopy()
		s.Spec.Repositories = s.Spec.Repositories[1:]
	})
	m.logf("the repository leaves the settings")
}

// restoreRepository puts the repository entry back into the settings.
func (m *modelWorld) restoreRepository() {
	repo := m.repo
	m.updateSettings(func(s *api.RemediationSettings) {
		s.Spec.Repositories = append([]api.GitRepository{*repo}, s.Spec.Repositories...)
	})
	m.repo = nil
	m.logf("the repository is back in the settings")
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
		OpenRepository: r.OpenRepository, Now: r.Now, CleanupTimeout: r.CleanupTimeout, ReviewerHints: r.ReviewerHints,
		VerifyOutcomes: m.rng.IntN(2) == 0}
	m.logf("restart (verify outcomes %t)", m.r.VerifyOutcomes)
}

// step runs one reconcile, checks the properties, and moves the clock on by its wait.
func (m *modelWorld) step() {
	ctx := context.Background()
	passed := ""
	before := m.live()
	m.mu.Lock()
	staleRead := m.staleNext
	m.mu.Unlock()
	if staleRead {
		// The reconcile reads an older copy: it cannot end a wait it does not see.
		before.Status.GitWaits = nil
	}
	for _, w := range before.Status.GitWaits {
		if end, ok := modelWaitEnd(before, w); ok && !m.clock.at.Before(end) {
			passed = string(w.Kind)
		}
	}
	m.mu.Lock()
	m.writeFaulted, m.calls, m.passed, m.ended = false, 0, passed, false
	m.mu.Unlock()
	paused, lifted, startedAt := m.paused, m.lifted, m.clock.at
	m.lifted = false
	res, err := m.r.Reconcile(ctx, reconcile.Request{NamespacedName: m.key})
	m.mu.Lock()
	m.passed = ""
	m.mu.Unlock()
	live := &api.RemediationRequest{}
	if gerr := m.base.Get(ctx, m.key, live); gerr != nil {
		m.t.Fatal(gerr)
	}
	m.history = append(m.history, live.DeepCopy())
	m.mu.Lock()
	faulted := m.writeFaulted
	if err == nil && !m.contentTried.IsZero() && m.contentFailing.IsZero() {
		m.contentFailing = m.contentTried
	}
	m.contentTried = time.Time{}
	m.mu.Unlock()
	if m.second.Name != "" {
		m.stepSecond(ctx, live)
	}
	retry := meta.FindStatusCondition(live.Status.Conditions, api.ConditionGitReadRetry)
	approved := meta.FindStatusCondition(live.Status.Conditions, api.ConditionApproved)
	kinds := make([]string, 0, len(live.Status.GitWaits))
	for _, w := range live.Status.GitWaits {
		kinds = append(kinds, string(w.Kind))
	}
	m.logf("reconcile: phase %s, holds %t, wait %v, err %v, retry %s, waits %v, approved %s%s", live.Status.Phase,
		holdsLock(live), res.RequeueAfter, err, reasonOf(retry), kinds, reasonOf(approved), conditionsOf(live))
	m.pauseProperties(before, live, paused, lifted && !staleRead, startedAt, faulted)
	m.properties(live, res, err, faulted, paused)
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
func (m *modelWorld) stepSecond(ctx context.Context, first *api.RemediationRequest) {
	m.mu.Lock()
	m.writeFaulted = false
	m.mu.Unlock()
	// The second request's source tracks the branch the settings name now, so its policy check can admit it once
	// the target is free; its own reconciler, as the first one's resolver answers for the first's plan, and with no
	// Git host, as the limits tests run admission (limitsWorld): the model checks its lock, not its pull request.
	res := resolved()
	res.Source.TargetBranch = api.GitBranch(m.configured)
	// The fake client does not serve the field selectors the lock's list uses; apiServerReader does.
	r := &RemediationRequestReconciler{Client: m.base, APIReader: &apiServerReader{Reader: m.base}, Scheme: m.r.Scheme,
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
	m.logf("reconcile the second: phase %s, holds %t, err %v", live.Status.Phase, holdsLock(live), err)
	if holdsLock(live) && holdsLock(first) {
		m.fatalf("two holders: the second request holds the target while the first does")
	}
}

// conditionsOf names the conditions of rr other than GitReadRetry and Approved, with SHADEWRIGHT_MODEL_TRACE=2.
func conditionsOf(rr *api.RemediationRequest) string {
	if os.Getenv("SHADEWRIGHT_MODEL_TRACE") != "2" {
		return ""
	}
	var out []string
	for _, c := range rr.Status.Conditions {
		if c.Type != api.ConditionGitReadRetry && c.Type != api.ConditionApproved {
			out = append(out, c.Type+"="+string(c.Status)+"/"+c.Reason)
		}
	}
	return fmt.Sprintf(", conditions %v", out)
}

func reasonOf(c *metav1.Condition) string {
	if c == nil {
		return "-"
	}
	return c.Reason
}

// modelWaitEnd is when the model expects an entry of kind k of rr to end, from what the model knows of the kind
// (not from the code under test); ok is false for a Paced entry, which has no end.
func modelWaitEnd(rr *api.RemediationRequest, w api.GitWait) (time.Time, bool) {
	switch w.Kind {
	case api.GitWaitRequestDeadline:
		if rr.Status.Deadline != nil {
			return rr.Status.Deadline.Time, true
		}
		return w.Since.Time, true
	case api.GitWaitHandoffGrace:
		if rr.Status.Deadline != nil {
			return rr.Status.Deadline.Add(handoffRecoveryGrace), true
		}
		return w.Since.Add(handoffRecoveryGrace), true
	case api.GitWaitMergeCheck:
		since := w.Since.Time
		if c := rr.Status.MergeCommitAwaitedSince; c != nil && c.Before(&w.Since) {
			since = c.Time
		}
		return since.Add(mergeCommitWait), true
	case api.GitWaitMergeContent:
		return w.Since.Add(mergeContentWait), true
	}
	return time.Time{}, false
}

// properties checks the model's properties after a reconcile (see the top of this file).
func (m *modelWorld) properties(live *api.RemediationRequest, res reconcile.Result, err error, faulted, paused bool) {
	now := m.clock.at
	poll := m.r.pullRequestPollInterval()
	if paused {
		// A pause holds every wait and every clock (property "pause"); the reconcile after the lift answers for them
		// (property "lift").
		m.paceProperties(live, res, err, faulted)
		m.lockProperties(live)
		return
	}
	for _, w := range live.Status.GitWaits {
		if end, ok := modelWaitEnd(live, w); ok && now.After(m.afterPause(end).Add(poll)) {
			m.fatalf("waits: the %s wait passed its end %v ago and stands (phase %s)", w.Kind, now.Sub(end),
				live.Status.Phase)
		}
	}
	// A request a person cancelled is bounded by the cancel's own bound and then watched with CancelPending
	// (cancelFailed, TestEveryGitReadRetrySiteKeepsItsBound), not by its deadline.
	if d := live.Status.Deadline; d != nil && live.Status.Phase == api.PhasePolicyChecked && staleMark(live) == nil &&
		intent(live) == nil && !recoveringHandoff(live) && !live.Spec.Cancel && !faulted &&
		now.After(m.afterPause(d.Time).Add(poll)) {
		m.fatalf("clocks: the request is in PolicyChecked %v after its deadline", now.Sub(d.Time))
	}
	if since := live.Status.MergeCommitAwaitedSince; since != nil && progresses(live) && !mergeRecorded(live) &&
		now.After(m.afterPause(since.Add(mergeCommitWait)).Add(poll)) {
		m.fatalf("clocks: the merge seen at +%v is not recorded %v later (phase %s)", since.Sub(m.start()),
			now.Sub(since.Time), live.Status.Phase)
	}
	m.mu.Lock()
	since := m.contentFailing
	m.mu.Unlock()
	if !since.IsZero() && now.After(m.afterPause(since.Add(mergeContentWait)).Add(poll)) && !mergeRecorded(live) {
		m.fatalf("content: the reads of what the merge brought have failed since +%v and the request has not ended",
			since.Sub(m.start()))
	}
	m.paceProperties(live, res, err, faulted)
	m.lockProperties(live)
}

// afterPause is end, or the time the last pause was lifted when end was not overdue by a poll interval when the pause
// began and passed before the lift: a pause holds every end, and time paused counts against the bound, so such an
// end is due at the lift.
func (m *modelWorld) afterPause(end time.Time) time.Time {
	if end.Before(m.liftedAt) && !end.Add(m.r.pullRequestPollInterval()).Before(m.pausedAt) {
		return m.liftedAt
	}
	return end
}

// pauseProperties checks the properties of the pause (see the top of this file): while paused, no call to the Git
// host and no phase change; at the first reconcile after the lift (lifted), no wait past its end at the start of that
// reconcile stands, and a stopped holder whose pull request is closed holds no lock, unless a read or a write was made
// to fail.
func (m *modelWorld) pauseProperties(before, live *api.RemediationRequest, paused, lifted bool, startedAt time.Time,
	faulted bool) {
	m.mu.Lock()
	calls, fault := m.calls, m.fault
	m.mu.Unlock()
	if paused {
		if calls > 0 {
			m.fatalf("pause call: writes are paused and the reconcile called the Git host %d times (phase %s)", calls,
				live.Status.Phase)
		}
		// The phases after a recorded merge go on during a pause as on main: they read and write only the request's
		// own status (afterMerge), and the rollout's window does not count the time paused.
		if live.Status.Phase != before.Status.Phase && !mergeRecorded(before) {
			m.fatalf("pause phase: writes are paused and the phase changed from %s to %s", before.Status.Phase,
				live.Status.Phase)
		}
		return
	}
	if !lifted || faulted {
		return
	}
	for _, w := range live.Status.GitWaits {
		if end, ok := modelWaitEnd(live, w); ok && !startedAt.Before(end) {
			m.fatalf("lift wait: the %s wait passed its end during the pause and stands after the first reconcile "+
				"after the lift (phase %s)", w.Kind, live.Status.Phase)
		}
	}
	cr, found := m.pr()
	if fault == faultNone && m.repo == nil && !m.handoff && found && !m.deleted &&
		cr.State == providers.ChangeRequestClosed && holdsLock(live) && !progresses(live) &&
		!heldForVerification(live, m.r.VerifyOutcomes) {
		m.fatalf("lift lock: the pull request is closed and the stopped holder keeps the lock after the first "+
			"reconcile after the lift (phase %s)", live.Status.Phase)
	}
}

// paceProperties: no three errors in a row with no write made to fail, and no three hot requeues in a row.
func (m *modelWorld) paceProperties(live *api.RemediationRequest, res reconcile.Result, err error, faulted bool) {
	if err != nil && !faulted {
		m.errorsInRow++
		if m.errorsInRow >= 3 {
			m.fatalf("errors: three reconciles in a row returned an error with no write made to fail: %v", err)
		}
	} else {
		m.errorsInRow = 0
	}
	if err == nil && res.RequeueAfter > 0 && res.RequeueAfter < time.Second {
		m.hotInRow++
		if m.hotInRow >= 3 {
			m.fatalf("pace: three reconciles in a row asked to run again within a second (phase %s, waits %d)",
				live.Status.Phase, len(live.Status.GitWaits))
		}
	} else {
		m.hotInRow = 0
	}
}

// lockProperties: a recorded merge names its pull request and stays recorded; the lock is never released while the
// pull request is open or on a merge the status does not record, and never comes back.
func (m *modelWorld) lockProperties(live *api.RemediationRequest) {
	if mergeRecorded(live) && live.Status.PullRequest == nil {
		m.fatalf("record: the merge is recorded without the pull request it is about (phase %s)", live.Status.Phase)
	}
	if mergeRecorded(live) {
		m.recorded = true
	} else if m.recorded {
		m.fatalf("record lost: the merge was recorded and the status no longer records it (phase %s)", live.Status.Phase)
	}
	if !holdsLock(live) && !m.handoff && !m.released {
		// Checked at the release: a merge into a branch that delivered nothing then (a third base) stays one when the
		// settings name that branch later.
		cr, found := m.pr()
		switch {
		case found && cr.State == providers.ChangeRequestOpen && m.delivers(live, cr) && !m.deleted:
			m.fatalf("lock open: the lock was released while the pull request is open")
		case found && cr.State == providers.ChangeRequestMerged && m.delivers(live, cr) && !mergeRecorded(live):
			m.fatalf("lock merge: the lock was released on a merge the status does not record")
		}
		if !progresses(live) {
			m.released = true
			// A pull request merged into a branch that delivered nothing at the release (a third base) is not the
			// request's merge, whatever the settings name later.
			m.releasedAside = found && cr.State == providers.ChangeRequestMerged && !m.delivers(live, cr)
		}
	} else if m.released && holdsLock(live) {
		m.fatalf("lock back: the lock was released and is held again (phase %s)", live.Status.Phase)
	}
}

// delivers reports whether cr was merged into a branch that delivers the request's change: the branch its plan was
// made for, or the one the settings name now. A merge into a branch the settings named for a while and no longer
// name delivers nothing.
func (m *modelWorld) delivers(live *api.RemediationRequest, cr providers.ChangeRequest) bool {
	return cr.BaseBranch == m.configured || live.Status.Source != nil && cr.BaseBranch == string(live.Status.Source.TargetBranch)
}

// settled: after the world healed, the lock is free, or held by a request whose pull request is open; a merge
// recorded with its receipt never holds it.
func (m *modelWorld) settled() {
	live := &api.RemediationRequest{}
	if err := m.base.Get(context.Background(), m.key, live); err != nil {
		m.t.Fatal(err)
	}
	cr, found := m.pr()
	open := found && cr.State == providers.ChangeRequestOpen && !m.deleted && m.delivers(live, cr)
	switch {
	case live.Status.MergeReceipt != nil && holdsLock(live) && !progresses(live) &&
		!heldForVerification(live, m.r.VerifyOutcomes):
		m.fatalf("settled receipt: the merge is recorded with its receipt and the lock is held: phase %s",
			live.Status.Phase)
	case m.repo != nil, m.handoff:
		// The repository left the settings: no read can settle anything else. Or the pull request is the
		// predecessor's, which holds it.
	case live.Spec.Cancel && holdsLock(live) && (m.deleted || found && cr.State == providers.ChangeRequestClosed):
		// A cancel whose pull request is closed or gone settles like any other stop, also after a change of the
		// target branch (fifth decision of 2026-11-18 on #771).
		m.fatalf("settled cancel: the request is cancelled, its pull request is %s, and the lock is held: phase %s",
			map[bool]string{true: "deleted", false: foundClosed}[m.deleted], live.Status.Phase)
	case holdsLock(live) && !open && !progresses(live) && !heldForVerification(live, m.r.VerifyOutcomes):
		m.fatalf("settled lock: the lock is held after the world healed, and the pull request cannot merge (%s): phase %s",
			cr.State, live.Status.Phase)
	case found && cr.State == providers.ChangeRequestMerged && m.delivers(live, cr) && !mergeRecorded(live) && !m.deleted &&
		!m.releasedAside:
		m.fatalf("settled merge: the merge is not recorded after the world healed: phase %s", live.Status.Phase)
	}
}

// modelResolver maps the request's target to the file on the target branch the settings name now, at its head.
type modelResolver struct{ m *modelWorld }

func (s *modelResolver) Resolve(ctx context.Context, _ sourcemap.Input) (sourcemap.Result, error) {
	res := resolved()
	res.Source.TargetBranch = api.GitBranch(s.m.configured)
	head, err := s.m.g.BranchHead(ctx, s.m.configured)
	if err != nil {
		return sourcemap.Result{}, err
	}
	res.Source.BaseCommit = head
	return res, nil
}

// modelGit is the Git host of the model: the request's pull request hidden once deleted, its merge commit withheld
// while the model says so, and the read the model says failing or running out of the step's time. Reads of what a
// merge brought are the Parents read of the merge commit; the model notes when they began to fail and when one
// worked.
type modelGit struct {
	providers.GitProvider
	m *modelWorld
}

var errModelOutage = &providers.RetryableError{Err: errors.New("outage")}

// fail is the failure of op for the model now: nil, an outage, or the step's time run out.
func (g *modelGit) fail(ctx context.Context, op string) error {
	g.m.mu.Lock()
	g.m.calls++
	fault := g.m.fault
	g.m.mu.Unlock()
	for f := range strings.SplitSeq(fault, faultBoth) {
		name, timeout := strings.CutSuffix(f, faultTimeout)
		if name != op {
			continue
		}
		if deadline, bounded := ctx.Deadline(); timeout && bounded && time.Until(deadline) < 2*time.Second {
			// The step's time (CleanupTimeout) runs out while the host does not answer.
			<-ctx.Done()
			return freshness.WrapProvider("read "+op, ctx.Err())
		}
		// A read with no short deadline of its own fails as the host's client gives up.
		return errModelOutage
	}
	return nil
}

func (g *modelGit) hidden(n int64) bool {
	g.m.mu.Lock()
	defer g.m.mu.Unlock()
	return g.m.deleted && n == g.m.number
}

// shown is cr as the host reports it: without its merge commit while the model withholds it.
func (g *modelGit) shown(cr providers.ChangeRequest) providers.ChangeRequest {
	g.m.mu.Lock()
	defer g.m.mu.Unlock()
	if g.m.noCommit && cr.Number == g.m.number && cr.State == providers.ChangeRequestMerged {
		cr.MergeCommit = ""
	}
	return cr
}

func (g *modelGit) GetChangeRequest(ctx context.Context, n int64) (providers.ChangeRequest, error) {
	if err := g.fail(ctx, opGet); err != nil {
		return providers.ChangeRequest{}, err
	}
	if g.hidden(n) {
		return providers.ChangeRequest{}, fmt.Errorf("pull request %d: %w", n, providers.ErrNotFound)
	}
	cr, err := g.GitProvider.GetChangeRequest(ctx, n)
	return g.shown(cr), err
}

func (g *modelGit) ListChangeRequests(ctx context.Context, head string, openOnly bool) ([]providers.ChangeRequest, error) {
	if err := g.fail(ctx, opList); err != nil {
		return nil, err
	}
	all, err := g.GitProvider.ListChangeRequests(ctx, head, openOnly)
	for i := range all {
		all[i] = g.shown(all[i])
	}
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
	return g.shown(cr), err
}

func (g *modelGit) ReadFile(ctx context.Context, sha, path string, limit int) (providers.File, error) {
	if err := g.fail(ctx, opReadFile); err != nil {
		return providers.File{}, err
	}
	return g.GitProvider.ReadFile(ctx, sha, path, limit)
}

// CheckBetween and CountAhead are reads of what a merge brought too (retargetedContent); they fail with the
// content fault.
func (g *modelGit) CheckBetween(ctx context.Context, from, to, path string) (providers.BranchState, error) {
	if err := g.contentFail(ctx); err != nil {
		return 0, err
	}
	return g.GitProvider.CheckBetween(ctx, from, to, path)
}

func (g *modelGit) CountAhead(ctx context.Context, base, head string) (int, error) {
	if err := g.contentFail(ctx); err != nil {
		return 0, err
	}
	return g.GitProvider.CountAhead(ctx, base, head)
}

// contentFail is the content fault of a read of what a merge brought, noted for the content property.
func (g *modelGit) contentFail(ctx context.Context) error {
	g.m.mu.Lock()
	merged := g.m.merge != ""
	g.m.mu.Unlock()
	if !merged {
		g.m.mu.Lock()
		g.m.calls++
		g.m.mu.Unlock()
		return nil
	}
	err := g.fail(ctx, faultBetween)
	if err != nil {
		g.m.mu.Lock()
		if g.m.contentTried.IsZero() {
			g.m.contentTried = g.m.clock.at
		}
		g.m.mu.Unlock()
	}
	return err
}

func (g *modelGit) Parents(ctx context.Context, sha string) ([]string, error) {
	g.m.mu.Lock()
	content := sha == g.m.merge && g.m.merge != ""
	g.m.mu.Unlock()
	if !content {
		g.m.mu.Lock()
		g.m.calls++
		g.m.mu.Unlock()
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

// live is the request as the API server holds it.
func (m *modelWorld) live() *api.RemediationRequest {
	live := &api.RemediationRequest{}
	if err := m.base.Get(context.Background(), m.key, live); err != nil {
		m.t.Fatal(err)
	}
	return live
}
