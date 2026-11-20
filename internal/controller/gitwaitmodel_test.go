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
	"k8s.io/apimachinery/pkg/types"
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
// serves an older copy of the request at the next read through it (a reconcile starts from the API server's copy, so
// this reaches only the later reads of the cache), and the second request, when there is one, is reconciled between
// the next reconcile's read of the request and its first read of the Git host; the executor restarts (nothing kept in memory); the executor
// is down for longer than a wait (no reconcile at all, then a restart: down). The clock
// moves on by the wait each reconcile returns (a second after an error). Every status write is checked against the
// schema of the CRD, as the API server does: a write it would refuse is a violation.
//
// After each reconcile:
//   - pause: a reconcile that starts while writes are paused calls the Git host nothing (opening the repository
//     included) and leaves the phase of a request whose merge is not recorded as it was: a pause stops the look at a
//     recorded pull request, the settlement by a read, the cancel watch, the deadline's end and the end of every other
//     wait (sixth decision of 2026-11-18 on #771); a release on a merge receipt reads nothing and goes on (seventh
//     decision, point 1);
//   - lift: the first reconcile after the pause is lifted, with no write made to fail, ends every wait that passed
//     meanwhile (time paused counts against every wait; MergeCheck and MergeContent end there after their last
//     read), and, with reads working and no write made to fail, a stopped holder whose pull request is closed holds
//     no lock within three reconciles after it stopped (liftSettleSteps);
//   - requeue: a reconcile that leaves a stopped request holding its lock, without an error, asks to run again at a
//     time (seventh decision, point 3);
//   - waits (not while paused): every entry of status.gitWaits has not passed the end of its kind by more than one
//     poll interval: the request deadline (RequestDeadline), the deadline plus the handoff grace (HandoffGrace),
//     recoveryWaitBound after the first refusal (HandoffRecovery, #786), five minutes after its start (MergeCheck,
//     MergeContent); an end that fell in a pause or while the executor was down is due when that gap ended
//     (afterGap);
//   - own end (not while paused): a reconcile that could act (no write made to fail, no error, no stale read) leaves
//     no RequestDeadline, HandoffGrace or HandoffRecovery entry that stood at its start past the end that entry
//     records: no entry ends at another entry's end (third decision of 2026-11-18 on #786, point 1);
//   - last read (not while paused): a reconcile that starts with a MergeCheck or MergeContent wait past its end and
//     returns no error, with no write made to fail, ends it only on evidence about its last read (lastReadProperty):
//     a failed read ends it (never a retry), a repository gone from the settings ends it, a pause met inside the
//     reconcile keeps it, and a kept wait comes back between a second and the poll interval; answers that disagree
//     end it too, as a failure (ninth decision of 2026-11-18 on #771, point 1, the tenth, point 1, and the eleventh,
//     point 1);
//   - clocks (not while paused): every time field of the status that bounds a wait has not passed its end by more than one poll
//     interval (gitWaitClocks): a request in PolicyChecked past its deadline, a merge seen without its commit
//     (mergeCommitAwaitedSince, due as a read wait) whose merge is not recorded;
//   - content (not while paused): the reads of what a merge brought that kept failing since a time T end the request
//     (MergeContentUnreadable or the merge recorded) by T plus mergeContentWait plus one poll interval, or when the
//     gap it fell in ended (readWaitDue);
//   - errors: no three reconciles in a row return an error while no write is made to fail;
//   - pace: no reconcile asks to be run again within a second without an error (a wait that ends and is not ended
//     runs hot, and the clock that bounds it never moves);
//   - record: a merge the status records names its pull request;
//   - lock recorded: a request whose status records its merge (Merged, Succeeded, Inconclusive, Failed with its
//     receipt, or its pull request marked merged) and that no verification holds holds no lock after a reconcile
//     with no write made to fail, paused or not (ninth decision of 2026-11-18 on #771, point 5), except a merge that
//     did not deliver the plan as shown (MergedWithForeignChange, MergeContentUnreadable, MergeNotCheckable) or
//     whose wait for a read is open: that lock waits for a read of the branch (tenth decision, point 3, and the
//     eleventh, point 2);
//   - lock: the lock is never released while a pull request of the request is open, nor on a merge the status does
//     not record, checked at the moment of the release (a merge into a branch the plan's change was not for, seen
//     only after the settings moved, is recorded as released aside), and a lock once released never comes back
//     while the request does not hold it;
//   - ending: a reconcile that starts with a RequestDeadline, HandoffGrace or HandoffRecovery wait past its end
//     calls the Git host nothing before its first status write: the end decides from recorded state alone (fifth
//     decision of 2026-11-18 on #771, point 5 as the sixth kept it);
//   - stop check kept: once a reconcile leaves a stopped request whose own pull request is open with the last review
//     check of its head failed, that check stays the last one on the head while the request stays stopped and the
//     pull request open, also when a reconcile works from an older copy of the request, whether its live read works
//     or fails: without the live read only a pass is taken down (decisions of 2026-11-20 on #925, the last after the
//     fifth pass). The model fails the live read of the request in half of the reconciles that read an older copy
//     (liveFails); it draws no approval, so no passing check, and directed tests hold that case
//     (TestFailedLiveReadWritesOnlyOverAPass, TestFailedLiveReadStillTakesThePassDown);
//   - schema: no status write is one the API server refuses.
//
// At the end the world heals (reads work, no write fails; the repository comes back in half of the runs; a pause is
// lifted, after a few more reconciles under it in half of the runs and always under the pause focus) and, after a few
// reconciles, the lock is free, or held by a request whose pull request is open and can still merge; a merge
// recorded with its receipt never holds it, whether the repository came back or not; a merge is recorded; and a
// cancelled request whose pull request is closed or gone holds no lock (settled cancel). With reviewer hints on, a
// request that stopped while its own pull request is open, with its repository in the settings, has its review check
// failed (settled check) or, where the settings permit no write (another target branch named), says so with condition
// ReviewCheckNotFailed (settled note, #925); a request whose own pull request is not open (closed, merged or gone,
// on any base), while no other open pull request from its branch keeps the lock, carries no such condition (note
// cleared). After every reconcile, no request whose failed check is
// recorded as Stale, and still shown failed last on its head, carries it (note cleared), nor one whose lock is released or whose merge is recorded (note
// ended); and a request that stops because its pull request is read gone (ChangeRequestGone), with reviewer hints on,
// leaves the failed review check last on its recorded head, or condition ReviewCheckNotFailed, in that reconcile (gone
// check).
//
// Each seed has a focus (seed modulo the number of focuses, modelFocuses): none, or one that makes an order of the
// fourth or fifth decision of 2026-11-18 on #771 likely (a deadline that passes while the repository cannot be
// opened, a merge reported without its commit while the reads fail, a merge recorded with its receipt and the
// repository gone, the cache serving a copy that held the lock after it was released, a retargeted merge into the
// longest branch name whose reads fail, a cancel with a branch change and a pull request closed, deleted or merged by
// hand, writes paused while the request is stopped or waits for its deadline and a person closes or merges its pull
// request, or, in half of the pause runs, a pause that comes and goes at every event, faster than a wait, while a merge
// waits for its commit or for reads of its content, with a status write lost or applied and reported failed at the
// pause or the lift). Under the commit and pause focuses the executor is also down, now and then, for longer than a
// wait. A focus only weights the start and the events; every event can still happen.
//
// Seeds: modelSeeds in CI (a fixed set, fewer under the race detector or -short); SHADEWRIGHT_MODEL_SEEDS=n runs n
// more seeds from the time, SHADEWRIGHT_MODEL_SEED=s one seed and SHADEWRIGHT_MODEL_SEED_RANGE=a-b the seeds a to b
// instead of the fixed set. A failure prints its seed and the trace; SHADEWRIGHT_MODEL_TRACE=1 prints it always.

const (
	modelSteps = 24

	// CleanupTimeout of the model: long, but for a step that meets a read that times out (cleanupTimeout). The long
	// one stays above the 2 s that modelGit.fail takes for a short deadline.
	modelLongTimeout  = 30 * time.Second
	modelShortTimeout = 150 * time.Millisecond

	faultNone    = "none"
	faultContent = "content"
	faultOpen    = "open"
	// faultBetween fails the reads that compare what a merge brought (CheckBetween, CountAhead), after the merge
	// commit's parents were read.
	faultBetween = "between"
	faultTimeout = "+timeout"
	// faultBoth joins two faults of one pass.
	faultBoth = "&"
	// opGet404 answers the read of the pull request by its number with a 404 that the listing of its branch does not
	// confirm: answers that disagree, not a failed read (tenth decision of 2026-11-18 on #771).
	opGet404 = "get404"

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
	// focusLastRead: a request stopped for a change of its target branch whose pull request a person moves to the
	// branch the settings name now and merges there, with or without its commit, while the reads of what it brought
	// fail; past the end of its wait for a read, the answers disagree, a pause comes and goes inside one reconcile, or
	// the host reports the commit (tenth decision of 2026-11-18 on #771).
	focusLastRead = "lastread"
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
		focusPause, focusLastRead}
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
	t.Cleanup(func() {
		modelMoves.mu.Lock()
		defer modelMoves.mu.Unlock()
		t.Logf("moves to another base and back: %d, in %d sequences; deleted while moved: %d; closed or merged while "+
			"moved before a lost write was adopted: %d", modelMoves.moves, modelMoves.sequences,
			modelMoves.movedBeforeDelete, modelMoves.endedWhileMoved)
		modelGoneStops.mu.Lock()
		defer modelGoneStops.mu.Unlock()
		t.Logf("stops for a pull request read gone with reviewer hints on, checked: %d", modelGoneStops.count)
	})
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
	// passWrites lets that many status writes through before loseNext, conflictNext or applyNext takes the next one,
	// so a fault can meet the second write of a reconcile.
	passWrites int
	// staleNext makes the cache serve stale at the next read of the request.
	staleNext bool
	// interleaveNext makes the second request change between the next reconcile's read of the request and its
	// decisions: it is reconciled before that reconcile's first read of the Git host (decision of 2026-11-19 on #786,
	// point 4: the reconcile reads the request from the API server, so the cache's older copy no longer starts one).
	interleaveNext bool
	// staleStart is what the next read of the request from the API server returns: a copy the request changes from
	// before the reconcile that read it decides (scripted sequences only).
	staleStart *api.RemediationRequest
	stale      *api.RemediationRequest
	// liveFails makes every live read of the request (APIReader) but the reconcile's first fail, in the reconcile
	// that follows a stale copy of the cache; liveRng draws it, apart from rng, so the orders the seeds drew before it
	// are kept. liveReads counts the live reads of the request in that reconcile.
	liveFails bool
	liveReads int
	liveRng   *rand.Rand
	// writeFaulted reports that this reconcile met an injected write fault or a stale read.
	writeFaulted bool
	// deleted hides the pull request from every read (gone from the host); gone are earlier pull requests of the
	// request deleted before it opened the one the model follows now (modelGit.OpenChangeRequest).
	deleted bool
	gone    []int64
	// movedFrom is the base the pull request had before a person moved it to the third base, and moves how often
	// it was moved there or back in this sequence (moveEvent).
	movedFrom string
	moves     int
	// second says a pull request was opened next to the request's own while that one was open (modelGit
	// OpenChangeRequest); empty while none was.
	secondPR string
	// unmarked is a pull request a person opened from the request's branch to the third base without the request's
	// marker (unmarkedEvent, #925); 0 while there is none.
	unmarked int64
	// reopenedAfterRelease: a person reopened the request's pull request after its lock was released. A released
	// request does not look at its pull request any more, so the lock does not follow the reopen and a merge after it
	// is not recorded: a known limit while point 3 of #925 waits for a decision, left out of the properties
	// "settled merge" and "lock back".
	reopenedAfterRelease bool
	// noCommit makes the host report the merge without its merge commit.
	noCommit bool
	number   int64
	merge    string
	// contentFailing is when the reads of what the merge brought began to fail without a success since, from the
	// first failure in a reconcile that ended without an error (a wait whose status write failed is not started:
	// the reconcile returns the error for a retry); zero when they work, or were never made. contentTried is the
	// first failure of the reconcile running.
	contentFailing, contentTried time.Time
	errorsInRow                  int
	startAt                      time.Time
	// released: the request's lock was seen released; recorded: the request's merge was seen recorded.
	released, recorded bool
	// releasedAside: at the release the pull request was merged into a branch that delivered nothing.
	releasedAside bool
	// handoff: the request is a replacement recovering a handoff; the pull request is its predecessor's, which
	// holds the lock on it, so the lock properties about the request's own pull request do not apply.
	handoff bool
	// refusedAt: the first reconcile, not paused, of a replacement recovering a handoff that ran with its repository
	// gone from the settings or with the settings naming another branch than its plan's (planBranch): the recovery
	// waits on that refusal from then on (#786), and its HandoffRecovery wait ends recoveryWaitBound after it.
	refusedAt  time.Time
	planBranch string
	// lost: the request started in PolicyChecked with the write recording its own pull request lost (and no handoff),
	// so only its marker finds that pull request.
	lost bool
	// lastingUntil keeps the fault as it is until then (an outage that lasts): fault events wait.
	lastingUntil time.Time
	// checkFails: the Git host fails every write of a check run (CreateCheckRun) with a 5xx (checkEvent): a write
	// made to fail (writeFaulted), no failed read.
	checkFails bool
	// goneStopSeen says goneCheckProperty saw this sequence's stop for a pull request read gone (ChangeRequestGone).
	goneStopSeen bool
	// stopFailedHead is the head whose last review check a reconcile left failed while the request stopped with its
	// own pull request open on that head (property "stop check kept"); empty otherwise.
	stopFailedHead string
	// history is every copy of the request the API server held after a reconcile, for the stale cache.
	history    []*api.RemediationRequest
	violations []string
	// focus weights the events (modelFocuses).
	focus string
	// flicker: the next reconcile that makes a last read meets a pause at its first read of the settings after the
	// check at its start, lifted at the next read (flickerReader).
	flicker bool
	// flickered: the flicker was consumed by the reconcile just checked.
	flickered bool
	// paused: the kill switch pauses writes now. flap: under focusPause, the pause comes and goes at every event,
	// faster than a wait, while a merge waits for its commit or its content.
	paused, flap bool
	// calls counts the calls to the Git host (opening the repository included) in the reconcile running; passed is
	// the kind of a Git wait of the request that had passed its end when that reconcile began, and ended whether the
	// first status write of that reconcile was made (property "ending").
	calls int
	// failedCalls counts the calls to the Git host the model made fail in the reconcile running (property "last
	// read": the evidence that a last read failed).
	failedCalls int
	// lastFailedCalls is failedCalls of the last reconcile checked (readsSettle).
	lastFailedCalls int
	// lastErred: the last reconcile checked returned an error or had a write made to fail (readsSettle).
	lastErred bool
	passed    string
	ended     bool
	// lifted: the pause was lifted since the last reconcile (property "lift"); liftedAt is when the last pause was
	// lifted: an end that passed during it is due by then (afterGap).
	lifted             bool
	pausedAt, liftedAt time.Time
	// pauses are the pauses lifted so far, in order; downs the times the executor was down (down), in order;
	// disagree the times the answers about the pull request disagreed (opGet404), from disagreeFrom while they do.
	pauses, downs, disagree []pauseSpan
	disagreeFrom            time.Time
	// afterLift counts the reconciles since the last lift with reads working and no write made to fail; -1 when
	// none is counted (no lift yet, or a fault since).
	afterLift int
}

// pauseSpan is a pause from its start to its lift, or a time the executor was down.
type pauseSpan struct{ from, to time.Time }

// liftSettleSteps is how many reconciles after a lift, with reads working, a stopped holder whose pull request is
// closed may keep the lock, counted from the first reconcile that starts with it stopped (the end the lift lets
// through, an expiry or a confirmation that the pull request is gone, is not counted): the close, then the look that
// settles it.
const liftSettleSteps = 3

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
	// flap: under focusPause, the pause comes and goes at every event (modelWorld.flap).
	flap bool
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
	case focusLastRead:
		return modelStart{start: stopPlanFresh, verify: rng.IntN(2) == 0, focus: focus}
	case focusPause:
		if rng.IntN(2) == 0 {
			// The pause comes and goes faster than a wait, while a merge of a running request waits (flap).
			return modelStart{start: startRunning, fresh: rng.IntN(2) == 0, verify: rng.IntN(2) == 0, focus: focus,
				flap: true}
		}
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
		focus: st.focus, afterLift: -1, flap: st.flap}
	// #nosec G404 -- a seeded generator, so a failing order can be run again from its seed.
	m.liveRng = rand.New(rand.NewPCG(seed, seed^0x6a09e667f3bcc909))
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
	// Reviewer hints in half of the runs: the review check of a request that stops while its pull request is open is
	// failed (property "settled check", #925).
	r.ReviewerHints = rng.IntN(2) == 0
	// A third base, neither the plan's target branch nor a configured one, that a person may move the pull request to
	// (moveEvent, #892).
	if head, err := g.BranchHead(context.Background(), mainBranch); err != nil {
		t.Fatal(err)
	} else if err := g.CreateBranch(context.Background(), movedBranch, head); err != nil {
		t.Fatal(err)
	}
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
			if st.handoff {
				m.recovering(rr)
			}
		}
	default:
		orderStop(t, r, rr, start)
	}
	// In half of the runs with reviewer hints on, the Git host fails the write of check runs from the start until
	// checkEvent or the heal: a stopped request says so with condition ReviewCheckNotFailed (GitFailed) until a later
	// look writes the failed check (#925).
	m.checkFails = r.ReviewerHints && rng.IntN(2) == 0
	m.lost = start == startPolicyChecked && !m.handoff
	r.VerifyOutcomes = st.verify
	m.logf("start %+v", st)
	m.clock = clockOf(r)
	m.startAt = m.clock.at
	if st.start == startPolicyChecked && st.open {
		m.lastingUntil = m.clock.at.Add(st.lasting)
	}
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
	r.Client, r.APIReader = cache, &flickerReader{WithWatch: direct, m: m}
	r.Source = &modelResolver{m: m}
	r.OpenRepository = func(ctx context.Context, repo api.GitRepository) (providers.GitProvider, error) {
		if err := (&modelGit{m: m}).fail(ctx, faultOpen); err != nil {
			return nil, err
		}
		return &modelGit{GitProvider: g.ForTarget(string(repo.TargetBranch)), m: m}, nil
	}
	r.CleanupTimeout = modelLongTimeout
	return m
}

// cleanupTimeout is the time a step may take (CleanupTimeout) for the fault now. It is a real deadline inside a
// model that runs on a fake clock, so it is long (a read that is slow on a loaded machine must not end the step)
// unless a read is to time out: then it is short, and the faulted read waits for it (modelGit.fail).
func (m *modelWorld) cleanupTimeout() time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	if strings.Contains(m.fault, faultTimeout) {
		return modelShortTimeout
	}
	return modelLongTimeout
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
		State: api.HintReserved, Author: api.HintAuthor{ID: 1001, Login: "reviewer"}, Text: "raise the limit",
		UpdatedAt: metav1.NewTime(reconcilerClock), AcceptedAt: metav1.NewTime(reconcilerClock)}
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
	rr.Spec.ChainRoot = chainRootOf(pred)
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
	m.planBranch = m.configured
}

// setConfigured makes branch the target branch the settings name now (created on the host when missing). The count of
// reconciles that settle a stopped holder after a lift (afterLift) starts again: until the settings name the plan's
// branch again, a cancel only reads (the close waits), so the close and the look that settles it may come only now
// (seed 199179).
func (m *modelWorld) setConfigured(branch string) {
	if m.afterLift > 0 {
		m.afterLift = 0
	}
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
		// The CRD's CEL rules, transition rules included, on what the write stores against what was stored before it:
		// an entry of status.gitWaits whose start or end moved, or a handoff moved back, is refused as the API server
		// refuses it. A write the store refuses (a conflict on the resourceVersion, which the API server checks before
		// the rules) is not checked.
		write := do
		do = func() error {
			old := &api.RemediationRequest{}
			if err := m.base.Get(ctx, client.ObjectKeyFromObject(rr), old); err != nil {
				return write()
			}
			if err := write(); err != nil {
				return err
			}
			stored := &api.RemediationRequest{}
			if err := m.base.Get(ctx, client.ObjectKeyFromObject(rr), stored); err != nil {
				return err
			}
			if errs := m.schema.validateUpdate(old, stored); len(errs) > 0 {
				m.fatalf("rules: the API server refuses a status write of %s: %v", rr.Name, errs)
				old.ResourceVersion = stored.ResourceVersion
				if err := m.base.Status().Update(ctx, old); err != nil {
					return err
				}
				return apierrors.NewInvalid(api.GroupVersion.WithKind("RemediationRequest").GroupKind(), rr.Name, errs)
			}
			return nil
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
	if m.passWrites > 0 && (m.loseNext || m.conflictNext || m.applyNext) {
		m.passWrites--
		m.mu.Unlock()
		return do()
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
	} else if m.lost && m.rng.IntN(3) == 0 {
		m.lostThenMoved()
	}
	// The copy the API server held before the first reconcile, for the stale cache.
	m.history = append(m.history, m.live())
	for range modelSteps {
		m.event()
		m.step()
	}
	m.mu.Lock()
	m.fault, m.loseNext, m.conflictNext, m.applyNext, m.staleNext, m.noCommit = faultNone, false, false, false, false, false
	m.liveFails, m.liveReads = false, 0
	m.interleaveNext = false
	m.lastingUntil, m.passWrites, m.checkFails = time.Time{}, 0, false
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

// lostThenMoved is the order lost write, move, reconcile (#892, decisions of 2026-11-19): a person moves the pull
// request whose record was lost to the third base before the executor first looks, and may close it or merge it
// there ("close while moved", "merge while moved"). The request's Incident stands, so nothing but the look for its
// own pull request, on every base and in every state, keeps the request from opening another one to the target
// branch (modelGit.OpenChangeRequest notes one).
func (m *modelWorld) lostThenMoved() {
	ref := m.template.Spec.IncidentRef
	inc := &api.Incident{ObjectMeta: metav1.ObjectMeta{Namespace: m.key.Namespace, Name: ref.Name, UID: types.UID(ref.UID)}}
	if err := m.base.Create(context.Background(), inc); err != nil && !apierrors.IsAlreadyExists(err) {
		m.t.Fatal(err)
	}
	cr, _ := m.pr()
	m.moveEvent(cr)
	switch m.rng.IntN(3) {
	case 0:
		if err := m.g.Close(m.number); err != nil {
			m.t.Fatal(err)
		}
		modelMoves.mu.Lock()
		modelMoves.endedWhileMoved++
		modelMoves.mu.Unlock()
		m.logf("close the pull request while moved")
	case 1:
		sha, err := m.g.Merge(m.number)
		if err != nil {
			m.t.Fatal(err)
		}
		m.merge = sha
		modelMoves.mu.Lock()
		modelMoves.endedWhileMoved++
		modelMoves.mu.Unlock()
		m.logf("merge the pull request while moved")
	}
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
	case m.rng.IntN(2) == 0 && !m.clock.at.Before(m.lastingUntil):
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

// pullRequestEvent closes, moves, deletes or merges an open pull request.
func (m *modelWorld) pullRequestEvent(cr providers.ChangeRequest) {
	ctx := context.Background()
	if cr.State != providers.ChangeRequestOpen || m.deleted {
		return
	}
	switch m.rng.IntN(10) {
	case 6, 7, 8, 9:
		m.moveEvent(cr)
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
		if cr.BaseBranch == movedBranch {
			modelMoves.mu.Lock()
			modelMoves.movedBeforeDelete++
			modelMoves.mu.Unlock()
		}
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
}

// modelMoves counts, over every sequence of the run, the moves of the pull request to the third base and back
// (moveEvent), and the sequences with at least one, for the run's log (TestGitWaitModel).
var modelMoves struct {
	mu                sync.Mutex
	moves, sequences  int
	movedBeforeDelete int
	// endedWhileMoved counts the pull requests whose record was lost, closed or merged on the third base before the
	// executor first looked (lostThenMoved).
	endedWhileMoved int
}

// moveEvent is a person who moves the open pull request cr to the third base (movedBranch), or back to the base it
// had before (#892): the read by its number then answers it open on another base (changeElsewhere) on whatever path
// the request is, a stop, a cancel, a pause or a deletion of the pull request that may follow.
func (m *modelWorld) moveEvent(cr providers.ChangeRequest) {
	to := movedBranch
	if cr.BaseBranch == movedBranch {
		to = m.movedFrom
	} else {
		m.movedFrom = cr.BaseBranch
	}
	if err := m.g.SetBase(m.number, to); err != nil {
		m.t.Fatal(err)
	}
	modelMoves.mu.Lock()
	modelMoves.moves++
	if m.moves == 0 {
		modelMoves.sequences++
	}
	modelMoves.mu.Unlock()
	m.moves++
	m.logf("move the pull request from %.12s to %.12s", cr.BaseBranch, to)
}

// unmarkedEvent is a person who opens a pull request from the request's branch to the third base without the
// request's marker, or closes the one opened before (#925): open, it has the branch head and can be moved to the
// target branch and merged, so it keeps the lock as the request's own does (property "lock unmarked").
func (m *modelWorld) unmarkedEvent() {
	if m.unmarked != 0 {
		if err := m.g.Close(m.unmarked); err != nil {
			m.t.Fatal(err)
		}
		m.logf("close the unmarked pull request #%d", m.unmarked)
		m.unmarked = 0
		return
	}
	n, err := m.g.AddOpenChangeRequest(freshness.BranchName(m.template), movedBranch, "a person's change")
	if err != nil {
		return
	}
	m.unmarked = n
	m.logf("open #%d from the request's branch to %.12s without the marker", n, movedBranch)
}

// unmarkedOpen reports whether the pull request of unmarkedEvent is open.
func (m *modelWorld) unmarkedOpen() bool {
	if m.unmarked == 0 {
		return false
	}
	for _, cr := range m.g.ChangeRequests() {
		if cr.Number == m.unmarked {
			return cr.State == providers.ChangeRequestOpen
		}
	}
	return false
}

// reopenEvent is a person who reopens the request's closed pull request (#925), on whatever base it is.
func (m *modelWorld) reopenEvent(cr providers.ChangeRequest) {
	if cr.State != providers.ChangeRequestClosed || m.deleted {
		return
	}
	if err := m.g.Reopen(m.number); err != nil {
		return
	}
	if m.released {
		m.reopenedAfterRelease = true
	}
	m.restartSettlement()
	m.logf("reopen the pull request on %.12s", cr.BaseBranch)
}

// retargetEvent is a person who changes the base of the request's open pull request to any of the three bases: the
// recorded target branch, the one the settings name now, or the third base (#925), also from the third base to the
// configured branch, which moveEvent never draws.
func (m *modelWorld) retargetEvent(cr providers.ChangeRequest) {
	if cr.State != providers.ChangeRequestOpen || m.deleted {
		return
	}
	to := []string{mainBranch, m.configured, movedBranch}[m.rng.IntN(3)]
	if to == cr.BaseBranch {
		return
	}
	if to == movedBranch {
		m.movedFrom = cr.BaseBranch
	}
	if err := m.g.SetBase(m.number, to); err != nil {
		m.t.Fatal(err)
	}
	m.restartSettlement()
	m.logf("retarget the pull request from %.12s to %.12s", cr.BaseBranch, to)
}

// restartSettlement starts the count of property "lift lock" again, as at a lift, after a change of the world the
// settlement decides on: a reopened or retargeted pull request (an expired request closes a reopened one again) or
// the repository's return to the settings (the close of an expired request's cleanup once more) takes reconciles of
// its own (model seeds 10135, 138015 and 171640).
func (m *modelWorld) restartSettlement() {
	if m.afterLift > 0 {
		m.afterLift = 0
	}
}

// moveBackEvent is a person who moves the request's open pull request back from the third base to the base it had,
// once the request has stopped (#925, ADR-0043 limit (e)): the stopped request keeps the lock while it is open.
func (m *modelWorld) moveBackEvent(cr providers.ChangeRequest) {
	if cr.State != providers.ChangeRequestOpen || m.deleted || cr.BaseBranch != movedBranch || progresses(m.live()) {
		return
	}
	m.moveEvent(cr)
	m.logf("(moved back after the request stopped)")
}

// checkEvent switches the Git host's failure of check run writes on or off (checkFails, #925).
func (m *modelWorld) checkEvent() {
	m.mu.Lock()
	m.checkFails = !m.checkFails
	on := m.checkFails
	m.mu.Unlock()
	m.logf("the Git host fails check run writes: %t", on)
}

// event changes the world at random before a reconcile.
func (m *modelWorld) event() {
	cr, _ := m.pr()
	if m.focusEvent(cr) {
		return
	}
	switch m.rng.IntN(24) {
	case 0, 1, 8:
		m.pullRequestEvent(cr)
	case 19:
		m.unmarkedEvent()
	case 20:
		m.reopenEvent(cr)
	case 21:
		m.retargetEvent(cr)
	case 22:
		m.moveBackEvent(cr)
	case 23:
		m.checkEvent()
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
	case 18:
		m.down()
	}
}

// down stops the executor for longer than a wait, between six minutes and an hour: no reconcile at all, then a new
// process (restart). The first reconcile after it makes the last read of a read wait that passed meanwhile.
func (m *modelWorld) down() {
	from := m.clock.at
	m.clock.advance(time.Duration(6+m.rng.IntN(55)) * time.Minute)
	m.downs = append(m.downs, pauseSpan{from: from, to: m.clock.at})
	m.logf("the executor was down for %v", m.clock.at.Sub(from))
	m.restart()
}

// pause sets or lifts the kill switch's pause of writes.
func (m *modelWorld) pause(on bool) {
	m.updateSettings(func(s *api.RemediationSettings) { s.Spec.Paused = new(on) })
	switch {
	case m.paused && !on:
		m.lifted, m.liftedAt = true, m.clock.at
		m.pauses = append(m.pauses, pauseSpan{from: m.pausedAt, to: m.clock.at})
	case !m.paused && on && m.clock != nil:
		// No read is made while paused, and no wait ends: one that passed is due at the lift (afterGap).
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
	case gitWait(live, waitMergeCheck) != nil && m.rng.IntN(4) == 0:
		// The executor is down for longer than the wait while the merge waits for its commit; the host may report
		// it meanwhile.
		m.down()
		if m.rng.IntN(2) == 0 {
			m.mu.Lock()
			m.noCommit, m.fault, m.lastingUntil = false, faultNone, time.Time{}
			m.mu.Unlock()
			m.logf("the host reports the merge commit, and reads work")
		}
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
		m.byHand(cr, m.rng.IntN(5))
		return true
	}
	return m.rng.IntN(3) > 0
}

// pauseEvent is the event of focusPause: the pause, then a close, a deletion or a merge by hand while it lasts and
// the pull request is open; the pause mostly lasts.
func (m *modelWorld) pauseEvent(cr providers.ChangeRequest, open bool) bool {
	if m.flap {
		return m.flapEvent(cr, open)
	}
	switch {
	case !m.paused && m.rng.IntN(2) == 0:
		m.pause(true)
		return true
	case m.paused && open && m.rng.IntN(3) == 0:
		m.byHand(cr, m.rng.IntN(5))
		return true
	case m.paused:
		return m.rng.IntN(3) > 0
	}
	return false
}

// flapEvent is the event of a flapping pause (flap): first a merge, without its commit into the plan's branch, or
// into the branch the settings name now with the reads of what it brought failing; once the executor saw the merge
// without its commit, reads that fail for a while; then the pause set or lifted at every event, faster than a wait,
// and in a third of them a status write of the reconcile that follows lost or applied and reported failed (the first
// or the second write).
func (m *modelWorld) flapEvent(cr providers.ChangeRequest, open bool) bool {
	live := m.live()
	switch {
	case open && !m.paused:
		retarget := cr.BaseBranch == mainBranch && m.configured != mainBranch
		if retarget {
			_ = m.g.SetBase(m.number, m.configured)
		}
		sha, err := m.g.Merge(m.number)
		if err != nil {
			return false
		}
		m.mu.Lock()
		m.merge, m.noCommit = sha, !retarget
		if retarget {
			m.fault = []string{faultContent, faultBetween}[m.rng.IntN(2)]
			m.lastingUntil = m.clock.at.Add(time.Duration(20+m.rng.IntN(20)) * time.Minute)
		}
		m.mu.Unlock()
		m.logf("merge the pull request into %.12s (merge commit withheld: %t, reads: %s)", m.configured, !retarget, m.fault)
		return true
	case live.Status.MergeCommitAwaitedSince != nil && !mergeRecorded(live) && m.lastingUntil.IsZero():
		m.mu.Lock()
		m.fault = []string{opGet, opGet + faultTimeout}[m.rng.IntN(2)]
		m.lastingUntil = m.clock.at.Add(time.Duration(20+m.rng.IntN(20)) * time.Minute)
		m.mu.Unlock()
		m.logf("reads: %s until +%v", m.fault, m.lastingUntil.Sub(m.start()))
		return true
	case !m.paused && len(live.Status.GitWaits) > 0 && m.rng.IntN(6) == 0:
		// The executor is down for longer than the wait while it stands.
		m.down()
		return true
	}
	m.pause(!m.paused)
	if m.rng.IntN(3) == 0 {
		m.mu.Lock()
		m.passWrites = m.rng.IntN(2)
		if m.rng.IntN(2) == 0 {
			m.loseNext = true
		} else {
			m.applyNext = true
		}
		pass := m.passWrites
		m.mu.Unlock()
		m.logf("lose (or apply and report failed) status write %d of the next reconcile", pass+1)
	}
	return true
}

// focusEvent is the event of the run's focus, when it draws one; false leaves the event to the common draw. Under any
// focus, a stopped request with an open wait for a read may meet answers that disagree for a while (disagreeEvent).
func (m *modelWorld) focusEvent(cr providers.ChangeRequest) bool {
	open := cr.State == providers.ChangeRequestOpen && !m.deleted
	if m.disagreeEvent() {
		return true
	}
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
	case focusLastRead:
		return m.lastReadEvent(cr, open)
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

// lastReadEvent is the event of focusLastRead: the open pull request moved to the configured branch and merged there
// (half of the time without its commit, else with the reads of what it brought failing for a while); then, once a
// wait for a read stands, a pause that comes and goes inside the next reconcile (flicker), the host reporting the
// commit, or reads that work again; mostly quiet otherwise, so the waits pass their end.
func (m *modelWorld) lastReadEvent(cr providers.ChangeRequest, open bool) bool {
	live := m.live()
	reading := gitWait(live, waitMergeCheck) != nil || gitWait(live, waitMergeContent) != nil
	switch {
	case open && cr.BaseBranch != m.configured && m.rng.IntN(2) == 0:
		if err := m.g.SetBase(m.number, m.configured); err != nil {
			return false
		}
		sha, err := m.g.Merge(m.number)
		if err != nil {
			return false
		}
		m.mu.Lock()
		m.merge, m.noCommit = sha, m.rng.IntN(2) == 0
		if !m.noCommit {
			m.fault = faultBetween
			m.lastingUntil = m.clock.at.Add(time.Duration(3+m.rng.IntN(10)) * time.Minute)
		}
		noCommit := m.noCommit
		m.mu.Unlock()
		m.logf("move the pull request to the configured branch and merge it there (merge commit withheld: %t)",
			noCommit)
		return true
	case reading && m.rng.IntN(3) == 0:
		m.flicker = true
		m.logf("a pause comes and goes inside the next reconcile")
		return true
	case reading && m.rng.IntN(4) == 0:
		m.mu.Lock()
		m.noCommit = false
		m.mu.Unlock()
		m.logf("the host reports the merge commit")
		return true
	}
	return m.rng.IntN(3) > 0
}

// flickerReader serves the settings paused at the first read made after the reconcile marked its last reads
// (markLastRead) while the model's flicker is set, and as they are at every other read: a pause set after the check at
// the start of the reconcile and lifted before the end of the wait reads the settings again.
type flickerReader struct {
	client.WithWatch
	m *modelWorld
}

func (f *flickerReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if rr, ok := obj.(*api.RemediationRequest); ok && key == f.m.key {
		f.m.mu.Lock()
		stale := f.m.staleStart
		f.m.staleStart = nil
		// The reconcile's first read of the request starts it (getLive); a failed live read is drawn for the reads
		// after it, such as the one right before the merge gate writes the check.
		fails := f.m.liveFails && f.m.liveReads > 0
		f.m.liveReads++
		if fails {
			f.m.writeFaulted = true
		}
		f.m.mu.Unlock()
		if fails {
			return apierrors.NewServiceUnavailable("the API server is not available")
		}
		if stale != nil {
			// The request read here changes into what the API server holds before the reconcile decides.
			stale.DeepCopyInto(rr)
			return nil
		}
	}
	if err := f.WithWatch.Get(ctx, key, obj, opts...); err != nil {
		return err
	}
	s, ok := obj.(*api.RemediationSettings)
	if !ok || len(f.m.r.gitRetries().lastReads(f.m.key)) == 0 {
		return nil
	}
	f.m.mu.Lock()
	defer f.m.mu.Unlock()
	if f.m.flicker {
		f.m.flicker = false
		s.Spec.Paused = new(true)
	}
	return nil
}

// disagreeEvent: the read of the pull request by its number answers 404 while its branch still lists it (opGet404),
// for a while or, in a third of the draws, for ever (until the world heals), when the request is stopped with an open
// wait for a read. Before the end of the wait the answers that disagree keep it; past its end they show the read was
// made, and the wait ends as a failure (eleventh decision of 2026-11-18 on #771, point 1). The answer the running path
// gets is left out: it returns an error there, before this branch as on main.
func (m *modelWorld) disagreeEvent() bool {
	live := m.live()
	reading := gitWait(live, waitMergeCheck) != nil || gitWait(live, waitMergeContent) != nil
	if !reading || progresses(live) || m.clock.at.Before(m.lastingUntil) || m.rng.IntN(5) > 0 {
		return false
	}
	m.mu.Lock()
	m.fault = opGet404
	m.lastingUntil = m.clock.at.Add(time.Duration(6+m.rng.IntN(20)) * time.Minute)
	if m.rng.IntN(3) == 0 {
		// For ever: longer than any run, until the world heals.
		m.lastingUntil = m.clock.at.Add(72 * time.Hour)
	}
	m.mu.Unlock()
	m.logf("reads: the pull request answers 404 by its number while its branch lists it, until +%v",
		m.lastingUntil.Sub(m.start()))
	return true
}

// noteDisagreement keeps the spans of answers that disagree (opGet404) up to date at the start of a reconcile, for
// the trace and for noEvidenceProperty.
func (m *modelWorld) noteDisagreement() {
	m.mu.Lock()
	on := slices.Contains(strings.Split(m.fault, faultBoth), opGet404)
	m.mu.Unlock()
	switch {
	case on && m.disagreeFrom.IsZero():
		m.disagreeFrom = m.clock.at
	case !on && !m.disagreeFrom.IsZero():
		m.disagree = append(m.disagree, pauseSpan{from: m.disagreeFrom, to: m.clock.at})
		m.disagreeFrom = time.Time{}
	}
}

// disagreeing reports whether answers disagree now (opGet404).
func (m *modelWorld) disagreeing() bool { return !m.disagreeFrom.IsZero() }

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
// branch and merge it there (2), or merge it where it is (3), or move it to the third base or back (4, moveEvent).
func (m *modelWorld) byHand(cr providers.ChangeRequest, what int) {
	ctx := context.Background()
	switch what {
	case 4:
		m.moveEvent(cr)
	case 0:
		_ = m.g.CloseChangeRequest(ctx, m.number)
		m.logf("close the pull request by hand")
	case 1:
		m.mu.Lock()
		m.deleted = true
		m.mu.Unlock()
		if cr.BaseBranch == movedBranch {
			modelMoves.mu.Lock()
			modelMoves.movedBeforeDelete++
			modelMoves.mu.Unlock()
		}
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
	m.liveFails, m.liveReads = m.liveRng.IntN(2) == 0, 0
	m.interleaveNext = m.second.Name != ""
	m.mu.Unlock()
	m.logf("the cache serves an older copy of the request at the next read (phase %s, waits %d, live read fails %t)",
		m.stale.Status.Phase, len(m.stale.Status.GitWaits), m.liveFails)
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
	m.restartSettlement()
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
	m.noteDisagreement()
	passed := ""
	before := m.live()
	m.mu.Lock()
	armed := m.staleNext
	m.mu.Unlock()
	var readPassed []api.GitWaitKind
	for _, w := range before.Status.GitWaits {
		if end, ok := m.waitEnd(before, w); ok && !m.clock.at.Before(end) {
			if w.Kind == api.GitWaitMergeCheck || w.Kind == api.GitWaitMergeContent {
				// Ended after one more read (property "last read"), not from recorded state alone.
				readPassed = append(readPassed, w.Kind)
				continue
			}
			passed = string(w.Kind)
		}
	}
	m.mu.Lock()
	m.writeFaulted, m.calls, m.failedCalls, m.passed, m.ended = false, 0, 0, passed, false
	m.mu.Unlock()
	if m.handoff && !m.paused && (m.repo != nil || m.configured != m.planBranch) && m.refusedAt.IsZero() {
		m.refusedAt = m.clock.at
	}
	paused, lifted, startedAt := m.paused, m.lifted, m.clock.at
	m.lifted = false
	m.mu.Lock()
	flick := m.flicker
	m.mu.Unlock()
	m.r.CleanupTimeout = m.cleanupTimeout()
	res, err := m.r.Reconcile(ctx, reconcile.Request{NamespacedName: m.key})
	m.mu.Lock()
	m.flickered = flick && !m.flicker
	// The reconcile starts from the API server's copy; the cache's older copy reaches only a later read of it (the
	// look at what is left to clear), and the properties that judge what the reconcile ended skip such a reconcile.
	staleRead := armed && !m.staleNext
	m.interleaveNext = false
	m.liveFails, m.liveReads = false, 0
	m.mu.Unlock()
	m.mu.Lock()
	m.passed = ""
	failedCalls := m.failedCalls
	m.lastFailedCalls = failedCalls
	m.lastErred = err != nil || m.writeFaulted
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
	m.pauseProperties(before, live, paused, lifted && !staleRead, startedAt, faulted || err != nil)
	if !paused && !staleRead {
		m.lastReadProperty(readPassed, live, res, err, faulted, failedCalls)
		m.ownEndProperty(before, live, startedAt, err, faulted)
	}
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
		CleanupTimeout: m.cleanupTimeout()}
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

// waitEnd is when the model expects an entry of kind k of rr to end at the latest, from what the model knows of the
// kind and of the pauses (not from the code under test); ok is false for a Paced entry, which has no end.
func (m *modelWorld) waitEnd(rr *api.RemediationRequest, w api.GitWait) (time.Time, bool) {
	switch w.Kind {
	case api.GitWaitRequestDeadline:
		if rr.Status.Deadline != nil {
			return rr.Status.Deadline.Time, true
		}
		return w.Since.Time, true
	case api.GitWaitHandoffGrace:
		// Each wait of the recovery ends at its own end (third decision of 2026-11-18 on #786).
		if rr.Status.Deadline != nil {
			return rr.Status.Deadline.Add(handoffRecoveryGrace), true
		}
		return w.Since.Add(handoffRecoveryGrace), true
	case api.GitWaitHandoffRecovery:
		// recoveryWaitBound after the first refusal (#786).
		if !m.refusedAt.IsZero() {
			return m.refusedAt.Add(recoveryWaitBound), true
		}
		return w.Since.Add(recoveryWaitBound), true
	case api.GitWaitMergeCheck, api.GitWaitMergeContent:
		// From its own start (never from status.mergeCommitAwaitedSince).
		return w.Since.Add(mergeCommitWait), true
	}
	return time.Time{}, false
}

// ownEndProperty: a reconcile not paused that started with a RequestDeadline, HandoffGrace or HandoffRecovery entry
// past the end that entry records (before, at startedAt) and that could act (no error, no write made to fail) leaves
// no such entry standing (live): every entry ends at its own end, never at one computed from another entry (third
// decision of 2026-11-18 on #786, point 1).
func (m *modelWorld) ownEndProperty(before, live *api.RemediationRequest, startedAt time.Time, err error, faulted bool) {
	if err != nil || faulted {
		return
	}
	for _, w := range before.Status.GitWaits {
		switch w.Kind {
		case api.GitWaitRequestDeadline, api.GitWaitHandoffGrace, api.GitWaitHandoffRecovery:
		default:
			continue
		}
		if w.Ends == nil || startedAt.Before(w.Ends.Time) {
			continue
		}
		for _, l := range live.Status.GitWaits {
			if l.Kind == w.Kind && l.Since.Equal(&w.Since) {
				m.fatalf("own end: the %s entry stands %v past the end it records (%v)", w.Kind,
					startedAt.Sub(w.Ends.Time), w.Ends.Time)
			}
		}
	}
}

// properties checks the model's properties after a reconcile (see the top of this file).
func (m *modelWorld) properties(live *api.RemediationRequest, res reconcile.Result, err error, faulted, paused bool) {
	now := m.clock.at
	poll := m.r.pullRequestPollInterval()
	m.requeueProperty(live, res, err, faulted)
	if paused {
		// A pause holds every wait and every clock (property "pause"); the reconcile after the lift answers for them
		// (property "lift").
		m.paceProperties(live, res, err, faulted)
		m.lockProperties(live)
		m.recordedProperty(live, faulted)
		m.noteProperty(live)
		m.stopCheckProperty(live)
		return
	}
	for _, w := range live.Status.GitWaits {
		end, ok := m.waitEnd(live, w)
		due := m.afterGap(end)
		if w.Kind == api.GitWaitMergeCheck || w.Kind == api.GitWaitMergeContent {
			if !m.readsSettle(live, m.lastFailedCalls) {
				// No evidence about the last read: the wait stands (property "last read").
				continue
			}
			due = m.readGap(end)
		}
		if ok && now.After(due.Add(poll)) {
			m.fatalf("waits: the %s wait passed its end %v ago and stands (phase %s)", w.Kind, now.Sub(end),
				live.Status.Phase)
		}
	}
	// A request a person cancelled is bounded by the cancel's own bound and then watched with CancelPending
	// (cancelFailed, TestEveryGitReadRetrySiteKeepsItsBound), not by its deadline.
	if d := live.Status.Deadline; d != nil && live.Status.Phase == api.PhasePolicyChecked && staleMark(live) == nil &&
		intent(live) == nil && !recoveringHandoff(live) && !live.Spec.Cancel && !faulted &&
		now.After(m.afterGap(d.Time).Add(poll)) {
		m.fatalf("clocks: the request is in PolicyChecked %v after its deadline", now.Sub(d.Time))
	}
	if since := live.Status.MergeCommitAwaitedSince; since != nil && progresses(live) && !mergeRecorded(live) &&
		m.readsSettle(live, m.lastFailedCalls) &&
		now.After(m.readWaitDue(since.Time).Add(poll)) {
		m.fatalf("clocks: the merge seen at +%v is not recorded %v later (phase %s)", since.Sub(m.start()),
			now.Sub(since.Time), live.Status.Phase)
	}
	m.mu.Lock()
	since := m.contentFailing
	m.mu.Unlock()
	if !since.IsZero() && now.After(m.readWaitDue(since).Add(poll)) && !mergeRecorded(live) &&
		m.readsSettle(live, m.lastFailedCalls) {
		m.fatalf("content: the reads of what the merge brought have failed since +%v and the request has not ended",
			since.Sub(m.start()))
	}
	m.paceProperties(live, res, err, faulted)
	m.lockProperties(live)
	m.recordedProperty(live, faulted)
	m.noteProperty(live)
	m.stopCheckProperty(live)
	m.goneCheckProperty(live)
}

// modelGoneStops counts, over every seed of a run, the stops for a pull request read gone with reviewer hints on that
// goneCheckProperty checked, so a run shows the property was reached.
var modelGoneStops struct {
	mu    sync.Mutex
	count int
}

// goneCheckProperty: a request that stops because its pull request is read gone (ChangeRequestGone) with reviewer
// hints on fails the review check on its recorded head in the same reconcile, or says why not with condition
// ReviewCheckNotFailed (gone check, decision of 2026-11-20 on #925 extending the replacement of point 2 to the stop,
// ADR-0045): otherwise another pull request from its branch could merge that commit on a pass written before the
// stop. The model reaches no new installation of the App, so the exception for it is the directed tests'.
func (m *modelWorld) goneCheckProperty(live *api.RemediationRequest) {
	if m.goneStopSeen || live.Status.Phase != api.PhaseNeedsIntervention || !slices.ContainsFunc(live.Status.Conditions,
		func(c metav1.Condition) bool { return c.Reason == ReasonChangeRequestGone }) {
		return
	}
	m.goneStopSeen = true
	pr := live.Status.PullRequest
	if !m.r.ReviewerHints || pr == nil || handedOff(live) || intent(live) != nil ||
		meta.FindStatusCondition(live.Status.Conditions, api.ConditionReviewCheckNotFailed) != nil {
		return
	}
	modelGoneStops.mu.Lock()
	modelGoneStops.count++
	modelGoneStops.mu.Unlock()
	if last := lastReviewCheck(m.g.CheckRuns(pr.HeadSHA)); last == nil || last.Status != providers.CheckRunFailure {
		m.fatalf("gone check: the request stopped with ChangeRequestGone and the last review check of its recorded "+
			"head %.12s is not failed, and condition ReviewCheckNotFailed does not say so: %+v", pr.HeadSHA,
			m.g.CheckRuns(pr.HeadSHA))
	}
}

// stopCheckProperty: after a stop, the last review check written for the head is the failed one, or the pull request
// is no longer open (stop check kept, decision of 2026-11-20 on #925). Once a reconcile leaves a stopped request whose
// own pull request is open with the last review check of its head failed, no later reconcile leaves another state
// last on that head while the request stays stopped and that pull request open with that head: a reconcile working
// from an older copy of the request (the stale cache) must not write a pending or passing check over the failed one,
// whether its live read works or fails: without it only a pass is taken down (decision of 2026-11-20 on #925, after
// the fifth pass). The model fails that live read in half of the stale-copy reconciles (liveFails), but it does not
// catch a wrong rule for a failed read: with the condition reverted, seeds 1 to 20,000 pass. The directed tests
// TestFailedLiveReadWritesOnlyOverAPass, TestReviewCheckFromAStaleCopyLeavesTheStopsCheck and
// TestFailedLiveReadLeavesTheStopsCheckWhileAnotherPullRequestHolds are what prove it. Paused or not, with any fault: nothing the model injects writes a check run.
func (m *modelWorld) stopCheckProperty(live *api.RemediationRequest) {
	pr := live.Status.PullRequest
	cr, found := m.pr()
	if pr == nil || progresses(live) || !found || m.deleted || cr.State != providers.ChangeRequestOpen ||
		cr.Number != pr.Number || cr.HeadSHA != pr.HeadSHA {
		m.stopFailedHead = ""
		return
	}
	last := lastReviewCheck(m.g.CheckRuns(cr.HeadSHA))
	switch {
	case last != nil && last.Status == providers.CheckRunFailure:
		m.stopFailedHead = cr.HeadSHA
	case m.stopFailedHead == cr.HeadSHA:
		m.fatalf("stop check kept: the request stopped (phase %s) with its pull request open on %.12s and the failed "+
			"review check written, and a later check run on the head is not failed: %+v", live.Status.Phase,
			cr.BaseBranch, m.g.CheckRuns(cr.HeadSHA))
		m.stopFailedHead = ""
	}
}

// failedLast reports whether the last review check run on the head live records is failed.
func (m *modelWorld) failedLast(live *api.RemediationRequest) bool {
	last := lastReviewCheck(m.g.CheckRuns(live.Status.PullRequest.HeadSHA))
	return last != nil && last.Status == providers.CheckRunFailure
}

// lastReviewCheck is the last check run named providers.CheckRunName in runs, oldest first, or nil.
func lastReviewCheck(runs []providers.CheckRun) *providers.CheckRun {
	var last *providers.CheckRun
	for i := range runs {
		if runs[i].Name == providers.CheckRunName {
			last = &runs[i]
		}
	}
	return last
}

// noteProperty: condition ReviewCheckNotFailed says the failed review check is tried again at the next look of a
// stopped holder, so no request carries it after its lock is released or once its merge is recorded (note ended,
// #925): the write that ends the looks removes it; nor once its failed check is recorded as Stale (note cleared):
// the write that records it removes it, unless the Git host no longer shows that failed check last on the head: a
// later look writes it again and says so while that write fails (decision of 2026-11-20 on #925).
func (m *modelWorld) noteProperty(live *api.RemediationRequest) {
	c := meta.FindStatusCondition(live.Status.Conditions, api.ConditionReviewCheckNotFailed)
	switch {
	case c == nil:
	case !holdsLock(live):
		m.fatalf("note ended: the lock is released (phase %s) and condition ReviewCheckNotFailed is still set: %s",
			live.Status.Phase, conditionsOf(live))
	case mergeRecorded(live):
		m.fatalf("note ended: the merge is recorded (phase %s) and condition ReviewCheckNotFailed is still set: %s",
			live.Status.Phase, conditionsOf(live))
	case staleRecorded(live) && m.failedLast(live):
		m.fatalf("note cleared: the failed review check is recorded, and condition ReviewCheckNotFailed is still "+
			"set: %s", conditionsOf(live))
	}
}

// recordedProperty: a request whose status records a merge that delivered the plan, in Merged, Succeeded,
// Inconclusive or Failed with its receipt, or with its recorded pull request marked merged (PullRequestOpened
// ChangeRequestMerged, outside Superseded), and that no verification holds, holds no lock after a reconcile with no
// write made to fail, under a pause too: the write that records the merge or ends the verification releases it, as on
// main. A merge that did not deliver the plan as shown (MergedWithForeignChange, MergeContentUnreadable,
// MergeNotCheckable), or whose wait for a read is still open, is left to the read of the branch (tenth decision of
// 2026-11-18 on #771, point 3, and the eleventh, point 2), which the end of the run checks (settled).
func (m *modelWorld) recordedProperty(live *api.RemediationRequest, faulted bool) {
	opened := meta.FindStatusCondition(live.Status.Conditions, api.ConditionPROpened)
	approved := meta.FindStatusCondition(live.Status.Conditions, api.ConditionApproved)
	undelivered := live.Status.Phase == api.PhaseNeedsIntervention && approved != nil &&
		(approved.Reason == ReasonMergedWithForeignChange || approved.Reason == ReasonMergeContentUnreadable ||
			approved.Reason == ReasonMergeNotCheckable)
	reading := slices.ContainsFunc(live.Status.GitWaits, func(w api.GitWait) bool {
		return w.Kind == api.GitWaitMergeCheck || w.Kind == api.GitWaitMergeContent
	})
	merged := live.Status.Phase == api.PhaseMerged || live.Status.Phase == api.PhaseSucceeded ||
		live.Status.Phase == api.PhaseInconclusive ||
		(live.Status.Phase == api.PhaseFailed && live.Status.MergeReceipt != nil) ||
		(live.Status.PullRequest != nil && opened != nil && opened.Reason == ReasonChangeRequestMerged &&
			live.Status.Phase != api.PhaseSuperseded && !progresses(live) && !undelivered && !reading)
	if !faulted && !m.handoff && merged && holdsLock(live) && !heldForVerification(live, m.r.VerifyOutcomes) {
		m.fatalf("lock recorded: the merge is recorded (phase %s, receipt %t) and the lock is held", live.Status.Phase,
			live.Status.MergeReceipt != nil)
	}
}

// requeueProperty: a reconcile that leaves the lock held by a stopped request (one that is not held for its
// verification) returns an error or a time to run again (seventh decision of 2026-11-18 on #771, point 3).
func (m *modelWorld) requeueProperty(live *api.RemediationRequest, res reconcile.Result, err error, faulted bool) {
	if err == nil && res.RequeueAfter <= 0 && !faulted && holdsLock(live) && !progresses(live) &&
		!heldForVerification(live, m.r.VerifyOutcomes) {
		m.fatalf("requeue: the reconcile leaves the lock held by the stopped request with no time to run again "+
			"(phase %s)", live.Status.Phase)
	}
}

// readWaitDue is when a read wait (the merge check, the content wait) started at start is due at the latest: start
// plus its five minutes, or, when that falls in a pause or a time the executor was down, the end of that gap
// (afterGap): the first reconcile that can read makes the last read, and nothing is stretched (ninth decision of
// 2026-11-18 on #771, point 1).
func (m *modelWorld) readWaitDue(start time.Time) time.Time {
	// The merge check and the content wait have the same bound (mergeContentWait is mergeCommitWait).
	return m.readGap(start.Add(mergeCommitWait))
}

// readGap is afterGap for the end of a wait for a read. Answers that disagree (opGet404, or a pull request missing by
// its number that its branch does not confirm gone) are no gap: past the end they end the wait (eleventh decision of
// 2026-11-18 on #771, point 1).
func (m *modelWorld) readGap(end time.Time) time.Time {
	return m.afterGap(end)
}

// readsSettle reports whether the reconcile just checked could settle a wait for a read past its end: a read failed
// (failedCalls) or the repository is gone (evidence that ends it), or the reads answer (fault none, or answers that
// disagree, which past the end of the wait end it as well: eleventh decision of 2026-11-18 on #771, point 1). Otherwise
// the wait may stand, and the time bounds of the read waits are not checked for it.
func (m *modelWorld) readsSettle(_ *api.RemediationRequest, failedCalls int) bool {
	if m.lastErred || m.flickered {
		// A reconcile that returned an error, or had a write made to fail, ends nothing (property "errors"); one that
		// met a pause inside it keeps its waits for a read.
		return false
	}
	m.mu.Lock()
	fault := m.fault
	m.mu.Unlock()
	return failedCalls > 0 || m.repo != nil || fault == faultNone || fault == opGet404
}

// afterGap is end, or the end of a pause or of a time the executor was down that end fell in (from a poll interval
// before its start, before a reconcile could end it): a pause holds every end, and no reconcile runs while the
// executor is down, so such an end is due when the gap ends. Gaps that follow each other are followed.
func (m *modelWorld) afterGap(end time.Time) time.Time {
	return m.gapEnd(end, slices.Concat(m.pauses, m.downs))
}

// gapEnd is end, or the end of the gap of gaps that end fell in, following gaps that follow each other.
func (m *modelWorld) gapEnd(end time.Time, gaps []pauseSpan) time.Time {
	poll := m.r.pullRequestPollInterval()
	slices.SortFunc(gaps, func(a, b pauseSpan) int { return a.from.Compare(b.from) })
	for _, g := range gaps {
		if end.Before(g.to) && !end.Add(poll).Before(g.from) {
			end = g.to
		}
	}
	return end
}

// lastReadProperty: a reconcile that is not paused and starts with read waits past their end (passed) makes their
// reads once more, and ends a wait only on evidence about that read (tenth decision of 2026-11-18 on #771, point 1).
// With no error and no write made to fail:
//   - a wait ended as a failure of its last read ("the read after the wait failed") had a read of the Git host fail
//     in that reconcile (failedCalls), and one ended because no read could start had the repository gone from the
//     settings: the model checks the read the wait waits for, not that some call was made;
//   - a read failed, the repository is gone, or the answers about the pull request disagreed (disagreed): none of
//     the waits stands after it (a failed last read ends the wait, never a retry; eleventh decision of 2026-11-18 on
//     #771, point 1). A wait that stands comes back between a second and the poll interval.
func (m *modelWorld) lastReadProperty(passed []api.GitWaitKind, live *api.RemediationRequest, res reconcile.Result,
	err error, faulted bool, failedCalls int) {
	if len(passed) == 0 || err != nil || faulted || m.handoff {
		return
	}
	// A pause inside the reconcile (flicker) outweighs any evidence: the wait is kept.
	disagreed := m.disagreed(live)
	evidence := (failedCalls > 0 || m.repo != nil || disagreed) && !m.flickered
	if !evidence {
		m.noEvidenceProperty(passed, live)
	}
	how := "could not start"
	switch {
	case failedCalls > 0:
		how = "was a failed read"
	case disagreed && m.repo == nil:
		how = "met answers that disagree"
	}
	for _, k := range passed {
		for _, w := range live.Status.GitWaits {
			if w.Kind != k {
				continue
			}
			if evidence {
				m.fatalf("last read: the %s wait had passed its end, its last read %s, and the wait stands after it "+
					"(phase %s)", k, how, live.Status.Phase)
			}
			if res.RequeueAfter < time.Second || res.RequeueAfter > m.r.pullRequestPollInterval() {
				m.fatalf("last read pace: the %s wait stands past its end and the request comes back after %v; want "+
					"between a second and the poll interval", k, res.RequeueAfter)
			}
		}
	}
	m.endMessageProperty(passed, live, failedCalls)
}

// noEvidenceProperty is the part of lastReadProperty for a reconcile with no evidence about the last read of the
// passed waits: when it met a pause inside it, it recorded no merge without a receipt (the end of a MergeCheck wait).
func (m *modelWorld) noEvidenceProperty(passed []api.GitWaitKind, live *api.RemediationRequest) {
	if len(m.history) < 2 {
		return
	}
	before := m.history[len(m.history)-2]
	if m.flickered && !mergeRecorded(before) && mergeRecorded(live) && live.Status.MergeReceipt == nil {
		m.fatalf("last read evidence: the %v wait ended with no evidence about its last read (a pause inside the "+
			"reconcile)", passed)
	}
}

// disagreed reports whether the reconcile just checked met answers that disagree (opGet404): the 404 by number
// disagrees with the listing only for a pull request read by its recorded number, still on a branch the code reads.
func (m *modelWorld) disagreed(live *api.RemediationRequest) bool {
	if len(m.history) < 2 {
		return false
	}
	before := m.history[len(m.history)-2]
	cr, found := m.pr()
	return m.disagreeing() && found && !m.deleted && m.delivers(live, cr) && before.Status.PullRequest != nil
}

// endMessageProperty is the part of lastReadProperty for the message of an end: it names the evidence the reconcile
// had.
func (m *modelWorld) endMessageProperty(passed []api.GitWaitKind, live *api.RemediationRequest, failedCalls int) {
	for _, t := range []string{api.ConditionApproved, api.ConditionPlanFresh} {
		c := meta.FindStatusCondition(live.Status.Conditions, t)
		switch {
		case c == nil || m.endedBefore(c):
		case strings.Contains(c.Message, "the read after the wait failed") && failedCalls == 0:
			m.fatalf("last read failed: the %v wait ended on a failed last read and no read of the Git host failed "+
				"(%s %s)", passed, t, c.Reason)
		case strings.Contains(c.Message, "no read of the Git host could") && m.repo == nil:
			m.fatalf("last read start: the %v wait ended as a read that could not start with the repository in the "+
				"settings (%s %s)", passed, t, c.Reason)
		}
	}
}

// endedBefore reports whether condition c of the request stood as it is before the reconcile being checked: an end
// of an earlier reconcile, not this one's.
func (m *modelWorld) endedBefore(c *metav1.Condition) bool {
	if len(m.history) < 2 {
		return false
	}
	prev := meta.FindStatusCondition(m.history[len(m.history)-2].Status.Conditions, c.Type)
	return prev != nil && prev.Message == c.Message
}

// pauseProperties checks the properties of the pause (see the top of this file): while paused, no call to the Git
// host and no phase change; at the first reconcile after the lift (lifted), no wait past its end at the start of that
// reconcile stands; and liftSettleSteps reconciles after the lift, counted from the reconcile that starts with the
// request stopped, a stopped holder whose pull request is closed holds no lock, unless a read or a write was made to fail since the lift (seventh decision of 2026-11-18 on #771, point 3:
// settlement may take more than one reconcile, each with a time to run again, property "requeue").
func (m *modelWorld) pauseProperties(before, live *api.RemediationRequest, paused, lifted bool, startedAt time.Time,
	faulted bool) {
	m.mu.Lock()
	calls, fault := m.calls, m.fault
	m.mu.Unlock()
	if paused {
		m.afterLift = -1
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
	if lifted {
		m.afterLift = 0
	}
	if m.afterLift < 0 || faulted || fault != faultNone {
		// Only reconciles with reads working and no write made to fail count towards the settlement after a lift.
		m.afterLift = -1
		return
	}
	if !progresses(before) {
		// Counted from the stop, not from the lift: the end the lift lets through (an expiry, a confirmation that the
		// pull request is gone) may take reconciles of its own, and only then does the settlement start (seventh
		// decision of 2026-11-18 on #771, point 3: settlement may take more than one reconcile, each with a time to run
		// again).
		m.afterLift++
	}
	cr, found := m.pr()
	if m.afterLift >= liftSettleSteps && m.repo == nil && !m.handoff && found && !m.deleted &&
		cr.State == providers.ChangeRequestClosed && holdsLock(live) && !progresses(live) &&
		!heldForVerification(live, m.r.VerifyOutcomes) && !m.unmarkedOpen() {
		m.fatalf("lift lock: the pull request is closed and the stopped holder keeps the lock %d reconciles "+
			"after the lift (phase %s)", m.afterLift, live.Status.Phase)
	}
	if !lifted {
		return
	}
	if faulted {
		// A status write made to fail (or the error it returned): the wait is ended at the next reconcile.
		return
	}
	for _, w := range live.Status.GitWaits {
		if w.Kind == api.GitWaitMergeCheck || w.Kind == api.GitWaitMergeContent {
			// A wait for a read ends at the lift only on evidence about its last read (property "last read", and
			// property "waits" bounds it).
			continue
		}
		if end, ok := m.waitEnd(live, w); ok && !startedAt.Before(end) {
			m.fatalf("lift wait: the %s wait passed its end during the pause and stands after the first reconcile "+
				"after the lift (phase %s)", w.Kind, live.Status.Phase)
		}
	}
}

// paceProperties: no three errors in a row with no write made to fail, and no reconcile that asks to run again within
// a second (the floor of every time to run again).
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
		m.fatalf("pace: a reconcile asked to run again within a second (%v, phase %s, waits %d)",
			res.RequeueAfter, live.Status.Phase, len(live.Status.GitWaits))
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
		case found && cr.State == providers.ChangeRequestOpen && !m.deleted:
			// Open on any base, a third one included: it can still merge (decision of 2026-11-19 on #892).
			m.fatalf("lock open: the lock was released while the pull request is open (on %.12s)", cr.BaseBranch)
		case found && cr.State == providers.ChangeRequestMerged && m.delivers(live, cr) && !mergeRecorded(live):
			m.fatalf("lock merge: the lock was released on a merge the status does not record")
		case m.unmarkedOpen() && !progresses(live) && !mergeRecorded(live):
			// Any open pull request from the request's branch keeps the lock, on any base, marked or not (#925), as
			// one open on the target branch does: a release on a recorded merge reads nothing, there as here.
			m.fatalf("lock unmarked: the lock was released while #%d from the request's branch is open on %.12s",
				m.unmarked, movedBranch)
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

// settled: after the world healed, the lock is free, or held by a request whose own pull request is open on any base; a merge
// recorded with its receipt never holds it.
func (m *modelWorld) settled() {
	live := &api.RemediationRequest{}
	if err := m.base.Get(context.Background(), m.key, live); err != nil {
		m.t.Fatal(err)
	}
	m.mu.Lock()
	second := m.secondPR
	m.mu.Unlock()
	if second != "" {
		m.fatalf("second pull request: %s", second)
	}
	cr, found := m.pr()
	// The request's own pull request open on any base, a third one included, can still merge and keeps the lock
	// (decision of 2026-11-19 on #892); a closed or merged one, or none, does not.
	open := found && cr.State == providers.ChangeRequestOpen && !m.deleted
	// So does any open pull request from its branch without its marker (#925).
	open = open || m.unmarkedOpen()
	switch {
	case live.Status.MergeReceipt != nil && holdsLock(live) && !progresses(live) &&
		!heldForVerification(live, m.r.VerifyOutcomes):
		m.fatalf("settled receipt: the merge is recorded with its receipt and the lock is held: phase %s",
			live.Status.Phase)
	case m.repo != nil, m.handoff:
		// The repository left the settings: no read can settle anything else. Or the pull request is the
		// predecessor's, which holds it.
	case live.Spec.Cancel && holdsLock(live) && !m.unmarkedOpen() &&
		(m.deleted || found && cr.State == providers.ChangeRequestClosed):
		// A cancel whose pull request is closed or gone settles like any other stop, also after a change of the
		// target branch (fifth decision of 2026-11-18 on #771).
		m.fatalf("settled cancel: the request is cancelled, its pull request is %s, and the lock is held: phase %s",
			map[bool]string{true: "deleted", false: foundClosed}[m.deleted], live.Status.Phase)
	case holdsLock(live) && !open && !progresses(live) && !heldForVerification(live, m.r.VerifyOutcomes):
		m.fatalf("settled lock: the lock is held after the world healed, and the pull request cannot merge (%s): phase %s",
			cr.State, live.Status.Phase)
	case m.uncheckedStop(live, cr, open):
		m.fatalf("settled check: the request stopped (phase %s) while its pull request is open on %.12s, and the "+
			"last review check of its head is not failed: %+v", live.Status.Phase, cr.BaseBranch,
			m.g.CheckRuns(cr.HeadSHA))
	case m.settledNote(live, cr, found, open):
		// Said by settledNote.
	case found && cr.State == providers.ChangeRequestMerged && m.delivers(live, cr) && !mergeRecorded(live) && !m.deleted &&
		!m.releasedAside && !m.reopenedAfterRelease:
		m.fatalf("settled merge: the merge is not recorded after the world healed: phase %s", live.Status.Phase)
	}
}

// uncheckedStop reports whether live, after the world healed, stopped while its own pull request cr is open (open)
// on any base, with reviewer hints on and its repository entry naming its target branch, and the last review check
// run of the head is not a failure: a person could move the pull request back to the target branch, or leave it
// there, and merge it on a check written before the stop (#925, ADR-0043 limit (e)).
func (m *modelWorld) uncheckedStop(live *api.RemediationRequest, cr providers.ChangeRequest, open bool) bool {
	pr := live.Status.PullRequest
	if !m.r.ReviewerHints || m.handoff || m.repo != nil || m.paused || pr == nil || progresses(live) ||
		!holdsLock(live) || !open || m.deleted || cr.Number != pr.Number || cr.State != providers.ChangeRequestOpen ||
		cr.HeadSHA != pr.HeadSHA || live.Status.Source == nil || string(live.Status.Source.TargetBranch) != m.configured ||
		handedOff(live) || intent(live) != nil {
		return false
	}
	last := lastReviewCheck(m.g.CheckRuns(cr.HeadSHA))
	return last == nil || last.Status != providers.CheckRunFailure
}

// settledNote checks the properties "settled note" and "note cleared" (see the top of this file) after the world
// healed, and reports whether one failed.
func (m *modelWorld) settledNote(live *api.RemediationRequest, cr providers.ChangeRequest, found, open bool) bool {
	pr := live.Status.PullRequest
	switch {
	case m.unsaidStop(live, cr, open):
		m.fatalf("settled note: the request stopped (phase %s) while its pull request is open on %.12s, the last "+
			"review check of its head is not failed (%+v), and condition ReviewCheckNotFailed does not say so: %s",
			live.Status.Phase, cr.BaseBranch, m.g.CheckRuns(cr.HeadSHA), conditionsOf(live))
		return true
	case meta.FindStatusCondition(live.Status.Conditions, api.ConditionReviewCheckNotFailed) == nil:
	case m.repo == nil && (!found || m.deleted || pr == nil || cr.Number != pr.Number ||
		cr.State != providers.ChangeRequestOpen) && !m.unmarkedOpen():
		// With the repository in the settings the looks read the pull request: one that is not open ends them,
		// unless another open pull request from the branch keeps the lock (third fix round of #925).
		m.fatalf("note cleared: the request's pull request is not open (found %t, gone %t, #%d %s), and condition "+
			"ReviewCheckNotFailed is still set: %s", found, m.deleted, cr.Number, cr.State, conditionsOf(live))
		return true
	}
	return false
}

// unsaidStop reports whether live, after the world healed, stopped while its own pull request cr is open on any
// base, with reviewer hints on and its repository in the settings, the last review check run of the head is not a
// failure, and condition ReviewCheckNotFailed is not True: the request must say so where a person looks when the
// failed check could not be written (#925). Only the settings that name another target branch keep the write from
// happening after the world healed; uncheckedStop covers the rest.
func (m *modelWorld) unsaidStop(live *api.RemediationRequest, cr providers.ChangeRequest, open bool) bool {
	pr := live.Status.PullRequest
	if !m.r.ReviewerHints || m.handoff || m.repo != nil || m.paused || pr == nil || progresses(live) ||
		!holdsLock(live) || !open || m.deleted || cr.Number != pr.Number || cr.State != providers.ChangeRequestOpen ||
		cr.HeadSHA != pr.HeadSHA || handedOff(live) || intent(live) != nil {
		return false
	}
	last := lastReviewCheck(m.g.CheckRuns(cr.HeadSHA))
	if last != nil && last.Status == providers.CheckRunFailure {
		return false
	}
	c := meta.FindStatusCondition(live.Status.Conditions, api.ConditionReviewCheckNotFailed)
	return c == nil || c.Status != metav1.ConditionTrue
}

// staleRecorded reports whether live records the review check of its pull request's head as Stale.
func staleRecorded(live *api.RemediationRequest) bool {
	pr := live.Status.PullRequest
	return pr != nil && pr.ReviewCheck != nil && pr.ReviewCheck.Head == pr.HeadSHA &&
		pr.ReviewCheck.State == api.ReviewCheckStale
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
	interleave := g.m.interleaveNext
	g.m.interleaveNext = false
	g.m.mu.Unlock()
	if interleave {
		// The other request changes between what this reconcile read and what it decides next.
		g.m.logf("before %s, the second request is reconciled", op)
		first := &api.RemediationRequest{}
		g.m.stepSecond(context.WithoutCancel(ctx), first)
	}
	g.m.mu.Lock()
	g.m.calls++
	fault := g.m.fault
	g.m.mu.Unlock()
	for f := range strings.SplitSeq(fault, faultBoth) {
		name, timeout := strings.CutSuffix(f, faultTimeout)
		if name != op {
			continue
		}
		g.m.mu.Lock()
		g.m.failedCalls++
		g.m.mu.Unlock()
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

// answers404 reports whether the model answers the read of a pull request by its number with a 404 now (opGet404).
func (g *modelGit) answers404() bool {
	g.m.mu.Lock()
	defer g.m.mu.Unlock()
	return slices.Contains(strings.Split(g.m.fault, faultBoth), opGet404)
}

func (g *modelGit) hidden(n int64) bool {
	g.m.mu.Lock()
	defer g.m.mu.Unlock()
	return g.m.deleted && n == g.m.number || slices.Contains(g.m.gone, n)
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
	if g.hidden(n) || g.answers404() {
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

// OpenChangeRequest notes a pull request opened next to the request's own while the host still has that one, on any
// base and in any state: a request that lost the write recording it finds it again by its marker and never opens
// another, also when a person closed or merged it on another base (invariant 5, #892, decisions of 2026-11-19).
func (g *modelGit) OpenChangeRequest(ctx context.Context, req providers.NewChangeRequest) (providers.ChangeRequest, error) {
	own, found := g.m.pr()
	out, err := g.GitProvider.OpenChangeRequest(ctx, req)
	if err != nil || out.Number == own.Number {
		return out, err
	}
	if found && !g.hidden(own.Number) {
		g.m.mu.Lock()
		if g.m.secondPR == "" {
			g.m.secondPR = fmt.Sprintf("#%d opened to %.12s while #%d is %s on %.12s", out.Number, req.BaseBranch,
				own.Number, own.State, own.BaseBranch)
		}
		g.m.mu.Unlock()
		return out, err
	}
	// Its own was deleted, so no look can find it: the new pull request is the request's from now on, and the model's
	// properties are about it.
	g.m.mu.Lock()
	if g.m.deleted {
		// Gone from the host: it stays hidden from every read after the model follows the new one.
		g.m.gone = append(g.m.gone, own.Number)
	}
	g.m.number, g.m.deleted, g.m.merge, g.m.noCommit = out.Number, false, "", false
	g.m.mu.Unlock()
	g.m.logf("the request opened #%d to %.12s; #%d is %s on %.12s", out.Number, req.BaseBranch, own.Number,
		own.State, own.BaseBranch)
	return out, err
}

// ListChangeRequestsAnyBase is a listing too: it fails with the listing's fault and hides a deleted pull request.
func (g *modelGit) ListChangeRequestsAnyBase(ctx context.Context, head string, openOnly bool) (
	[]providers.ChangeRequest, error) {
	if err := g.fail(ctx, opList); err != nil {
		return nil, err
	}
	all, err := g.GitProvider.ListChangeRequestsAnyBase(ctx, head, openOnly)
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

// CreateCheckRun fails while the model says so (checkFails).
func (g *modelGit) CreateCheckRun(ctx context.Context, sha string, run providers.CheckRun) error {
	g.m.mu.Lock()
	failing := g.m.checkFails
	if failing {
		// A write made to fail: a step that returns it for the backoff is no error of its own (property "errors").
		g.m.writeFaulted = true
	}
	g.m.mu.Unlock()
	if failing {
		return errModelOutage
	}
	return g.GitProvider.CreateCheckRun(ctx, sha, run)
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
