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
	"maps"
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
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	api "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/gitops/sourcemap"
	"github.com/pkalin/shadewright/internal/policy/freshness"
	"github.com/pkalin/shadewright/internal/providers"
	fakegit "github.com/pkalin/shadewright/internal/providers/fake"
	fx "github.com/pkalin/shadewright/internal/testfixtures"
)

// The generated model of a handoff (#786, fifth decision of 2026-11-18, extended by the second round of the same day):
// two requests, a predecessor whose pull request is open and a replacement taking it over, driven through Reconcile in
// seeded random orders of events, from one of three starts (the intent reserved and nothing pushed, the replacement's
// commit pushed and the takeover not recorded, the takeover recorded). The events: a reconcile of either request
// (either order, repeated), the clock moving on (minutes, past the replacement's deadline, past the deadline and the
// handoff grace, past a day), the Git host failing (503), refusing (403), not finding (404) or rejecting (422) the next
// read of one operation or every read until it answers again, the next status write of one request lost or meeting a
// conflicting write, a restart (a new reconciler, nothing kept in memory), a crash after the next reconcile made n
// writes (status or Git, so any two writes can be cut apart), the kill switch pausing writes or lifting the pause,
// either request deleted, a settings change (the repository leaves the settings, its entry cannot be used or is fixed
// again, its target branch changes), and a person merging the pull request at its head, pushing a commit on top of it,
// force pushing it back to the predecessor's commit (alone, or with a person's commit on it), closing it, reopening it
// or deleting its branch (which closes it); one merge in eight is reported by the host without a merge commit ID on
// every read, so it can only be recorded without a receipt. Nine sequences in ten that draw no merge, close or branch deletion get one
// in their second half, so most reach the end of the pull request.
//
// After every step, and at the end after a settling run (the pause lifted, reads answering, an unusable entry fixed,
// both requests reconciled in both orders while the clock moves on), the properties:
//
//	P1 a merge of a request's commit is recorded once: on the request whose commit it is, with its merge receipt, or,
//	   when that request is the replacement and it is deleted before it recorded the merge, on the predecessor as
//	   MergedWithForeignChange with no receipt (#786, decision 2 of the model result of 2026-11-18), a completed
//	   takeover included (without a receipt when the host reports no merge commit ID); never on both, and, once
//	   reads answer, never on none. A merged head with further commits on
//	   top of the replacement's commit is the documented gap #870, and the predecessor's commit put back by a force
//	   push after the replacement took the pull request over, with the predecessor handed off or gone, is a foreign
//	   change to the replacement: both are left out. A merge is identified at its first read (decision of
//	   2026-11-19 on #880): the request that first stores a record of it (a wait of kind MergeCheck or MergeContent
//	   naming the pull request, or the merge recorded in any form, MergedWithForeignChange included) is the one that
//	   records it. Whatever the merged head, the two requests never hold a record of it at the same time, and a live
//	   request that is not being deleted never loses the record of its first read;
//	P2 the circuit breaker counts that merge once (one success at most), and, once reads answer, no request is a
//	   failure for the breaker that was not one when a person merged a request's own commit, except the
//	   predecessor's MergedWithForeignChange of P1, a failure as on main;
//	P3 the target lock is never free while an open pull request carries a commit of either request, the replacement
//	   never releases it while its commit is on the open pull request, and no request releases it on a merge of its
//	   own commit it does not record;
//	P4 after the abort is stored, the two requests never both write the branch or the pull request, and the
//	   predecessor writes nothing to Git while its intent stands; every write to Git is checked as it is made, against
//	   the abort as stored by then (noted at the status write that stores it); and a request that waits for the other
//	   request's merge record (a status write of it carries MergeRecordPending, stored, lost or meeting a conflict,
//	   until one without it is stored, while that request may still record the merge) writes nothing to Git, not
//	   even an attempt;
//	P5 every status write passes the CRD schema and its CEL rules (transition rules included), and a reconcile that
//	   could act leaves no entry of status.gitWaits that stood past its own end at its start;
//	P6 after the settling run, once the pull request is merged or closed, or its branch or its repository is gone, no
//	   request holds the target lock, but for two named expectations while the repository is not in the settings
//	   (decision of 2026-11-19 on #786): a holder with no verdict stored (a merge recorded with its receipt or a phase
//	   after it; a read whose status write was lost is none) that had stopped, or that waited for the other
//	   request's merge record (MergeRecordPending) when the repository left, keeps the lock and shows condition
//	   PullRequestUnreadable with reason RepositoryUnavailable; both are asserted and counted. A sequence that ends
//	   with the repository removed then puts it back, and a second settling run checks P6, P1 and P2 again: the lock
//	   is free and a merge is recorded once.
//
// Exclusions, each named and recognised by its shape, never by a seed (hmShapes, and the checks that name them): the
// #288 cleanup bound (a request whose cleanup was failing let go of its finalizer at the end of a bound of that
// cleanup, elapsed on the model's clock, with the pull request open), the #870 head (P3), a reopen after a release (a
// request whose lock a verdict released when a person reopened the pull request does not take it back and records
// nothing of it, design 6.5), #877, a request deleted while the repository was not in the settings (its finalizer
// does not wait for the repository, design 6, so its pull request stays open with no holder after the repository is
// back: P3), and #875: a replacement deleted before it recorded the merge of its pull request after its predecessor
// ended HandedOff (the pull request was the replacement's alone, and a request deleted before it records the merge of
// its own pull request records nothing, as any request; the predecessor has ended). With the repository gone, reads
// never answer, so P1 and P2 ask nothing of a merge then. The #288 and #877 shapes excuse only what the request that
// let go leaves (goneShape): the lock free on the open pull request while no live request has a commit on it, and,
// for #288, the record missing of a merge of that request's commit and a lock the other request holds after the
// settling run; a later violation by the request still there is a violation.
//
// A failing seed prints its minimal sequence (steps dropped while the same property still fails) and the property; a
// seed whose only violations an exclusion covers is skipped with the exclusion's name. The default set is the seeds 1
// to 500, 1 to 100 under the race detector or -short; the code was checked against the seeds 1 to 9,000 and then
// 9,001 to 13,000, and in the fifth round of #786 against 1 to 24,000. SHADEWRIGHT_HANDOFF_MODEL_SEED=s runs one seed, SHADEWRIGHT_HANDOFF_MODEL_SEED_RANGE=a-b the seeds a
// to b instead, SHADEWRIGHT_HANDOFF_MODEL_TRACE=1 prints the trace of every seed
// (SHADEWRIGHT_HANDOFF_MODEL_CONDITIONS=1 adds the conditions after each reconcile). The log of every run (go test -v)
// holds the count of failures per property, of the exclusions, and of what the sequences reached.

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
	// hmSettleRounds is how many rounds of the settling run reconcile both requests: enough after the jump past the
	// recovery bound (half way) for a merge reported without a merge commit ID, which waits mergeCommitWait (one more
	// round) before it is recorded, and for the step after that record.
	hmSettleRounds = 12
)

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
	hReopen   hmKind = "person-reopens"
	hBranch   hmKind = "person-deletes-branch"
	hForce    hmKind = "person-force-pushes"
	hSettings hmKind = "settings-change"
)

// The variants of a force push (hForce) and of a settings change (hSettings).
const (
	forceDrop   = "drop-to-predecessor"
	forcePerson = "person-commit-on-predecessor"

	settingsRemoved  = "repository-removed"
	settingsUnusable = "entry-unusable"
	settingsBranch   = "target-branch-changed"
	settingsFixed    = "entry-fixed"
	// settingsRestored puts the plan's repository back in the settings after it left them: the step that follows
	// the settling run of a sequence that ends with the repository removed.
	settingsRestored = "repository-restored"

	// hmOtherBranch is the target branch a settings change names; hmOtherURL the repository that replaces the plan's.
	hmOtherBranch = "release"
	hmOtherURL    = "https://github.com/example/other-fleet"
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
	// NoID: the Git host reports the merged pull request without a merge commit ID, on every read.
	NoID bool
	// Variant is the kind of a force push or a settings change.
	Variant string
}

func (s hmStep) String() string {
	switch s.Kind {
	case hRecPred, hRecRepl, hAnswer, hRestart, hPause, hLift, hPush, hClose, hReopen, hBranch:
		return string(s.Kind)
	case hForce, hSettings:
		return string(s.Kind) + " " + s.Variant
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
		kind := "squash"
		if s.Commit {
			kind = "with a merge commit"
		}
		if s.NoID {
			kind += ", the host reports no merge commit ID"
		}
		return "person-merges (" + kind + ")"
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
	// commits: the predecessor's, the replacement's (empty until pushed; replCommits holds every commit the
	// replacement pushed, replCommit the last), and the ones a person pushed.
	predCommit, replCommit string
	replCommits            map[string]bool
	personCommits          map[string]bool

	// settings is the last settings change in effect ("" for none): the repository removed, its entry unusable, or
	// the target branch changed.
	settings string
	// branchGone: a person deleted the branch of the pull request.
	branchGone bool
	// releasedAtReopen names the requests that did not hold the target lock (a verdict released it, or they were
	// gone) when a person reopened the pull request: the named exclusion "a reopen after a release" (design 6.5), a
	// released request does not take the lock back and records nothing of the reopened pull request.
	releasedAtReopen map[string]bool
	// prEnded: the pull request was merged or closed or its branch gone, as of the last step.
	prEnded bool
	// planURL is the plan's repository, put back by settingsRestored; branchChanged: a settings change named
	// another target branch, which a restored repository keeps.
	planURL       string
	branchChanged bool
	// waitingAtRemoval names the requests that waited for the other request's merge record (condition Approved,
	// reason MergeRecordPending) when the repository left the settings.
	waitingAtRemoval map[string]bool
	// lastRes and lastErr are what the last reconcile of each request returned.
	lastRes map[string]reconcile.Result
	lastErr map[string]error
	// gone288 names the requests that were deleted and let go of their finalizer with the pull request open after
	// their cleanup failed until a bound of it elapsed on the model's clock (CleanupFailing True on the last copy): the
	// end of the cleanup bound (#288).
	gone288 map[string]bool
	// goneMissing names the requests that, being deleted, let go of their finalizer while the repository was not in
	// the settings, with the pull request open (design 6: a repository that left the settings does not block the
	// deletion; #877).
	goneMissing map[string]bool
	lastRepl    *api.RemediationRequest
	lastPred    *api.RemediationRequest
	// reach is what the sequence reached, for the counts; excused counts the violations a named shape excuses;
	// expected counts the named expectations that held (a state the design requires, checked, not excused).
	reach    map[string]bool
	excused  map[string]int
	expected map[string]int

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
	// awaiting names the requests that wait for the other request's merge record: a status write of theirs carried
	// MergeRecordPending (stored, lost or meeting a conflict), and none without it was stored since (P4).
	awaiting map[string]bool

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
	// afterTakeover: the predecessor's commit, merged after the replacement took the pull request over.
	afterTakeover bool
	// noID: the host reports the merge without a merge commit ID, so it is recorded without a receipt.
	noID bool
	// firstRead is the request that first stored a record of the merge (readRecord), "" until one did.
	firstRead string
}

// foreignOnReplacement reports the other record a merge of the predecessor's commit may have once the replacement took
// the pull request over (afterTakeover): on the replacement as MergedWithForeignChange with no receipt, and not on the
// predecessor.
func (m *hmWorld) foreignOnReplacement(reqs map[string]*api.RemediationRequest) bool {
	mg, repl := m.merge, reqs[hmRepl]
	if mg == nil || !mg.afterTakeover || mg.recorded[hmPred] {
		return false
	}
	if repl == nil {
		// Gone since: what it recorded while it lived counts.
		return mg.recorded[hmRepl]
	}
	c := meta.FindStatusCondition(repl.Status.Conditions, api.ConditionApproved)
	return repl.Status.MergeReceipt == nil && c != nil && c.Reason == ReasonMergedWithForeignChange
}

func (m *hmWorld) logf(format string, args ...any) {
	m.trace = append(m.trace, fmt.Sprintf("[%d +%v] ", m.stepNo, m.now.Sub(m.t0))+fmt.Sprintf(format, args...))
}

// hmShapes are the shapes of a sequence that the design does not require a property of, by name, with the properties
// each one excuses and the function that recognises it. A violation of an excused property is counted under its
// shape instead of failing the seed.
var hmShapes = []struct {
	name     string
	excuses  string
	matching func(m *hmWorld, about string) bool
}{
	// A request is deleted while every read or the close fails; its finalizer lets go at the end of the
	// CleanupRefused or CleanupFailing bound, elapsed on the model's clock, with the pull request left open (design 6,
	// "The Git cleanup cannot hold a deletion for good", #288), so no request holds the lock of that open pull
	// request, and a later merge of its commit is recorded by nobody. It excuses only that: the lock free on the open
	// pull request while no live request has a commit on it, the record missing of a merge of the gone request's
	// commit, and a lock the other request still holds after the settling run.
	{name: "#288 cleanup bound", excuses: "P1 P3 P6", matching: func(m *hmWorld, about string) bool {
		return m.goneShape(m.gone288, about, true)
	}},
	// A request is deleted while the repository is not in the settings: nothing can close its pull request, and the
	// deletion does not wait for the repository (design 6, the deletion; #877), so the pull request stays open with
	// no holder after the repository comes back. It excuses only the lock free on that open pull request while no
	// live request has a commit on it.
	{name: "#877 deleted while the repository was missing", excuses: "P3", matching: func(m *hmWorld, about string) bool {
		return m.goneShape(m.goneMissing, about, false)
	}},
	// A person pushed on top of the replacement's commit (#870): the head of the pull request carries the
	// replacement's commit with further commits on it. The merge of such a head is excluded from P1 and P2 by its
	// owner (gap870); P3 asks nothing of the lock of that head.
	{name: "#870 commits on top of the replacement's", excuses: "P3", matching: func(m *hmWorld, _ string) bool {
		return m.ownerOf(m.pr().HeadSHA) == "gap870"
	}},
}

// What a violation is about, for the shapes that excuse only their own (goneShape): the lock free on the open pull
// request, the record missing of a merge, or a lock still held by a request (aboutHolder plus its name). A violation
// about anything else ("") is never excused by them.
const (
	aboutLockFree   = "lock-free"
	aboutUnrecorded = "merge-unrecorded"
	aboutHolder     = "holder:"
)

// goneShape reports whether a violation about about is the one a request of gone (that let go of its finalizer with
// the pull request open) leaves: the lock free while the pull request is open and every request whose commit its head
// carries is gone (or is the predecessor that ended HandedOff) and a request of gone left that pull request open; with all, also the record missing of a merge of the commit of a request in
// gone, and a lock held after the settling run by the other request of one in gone.
func (m *hmWorld) goneShape(gone map[string]bool, about string, all bool) bool {
	other := map[string]string{hmPred: hmRepl, hmRepl: hmPred}
	switch {
	case about == aboutLockFree:
		cr := m.pr()
		if cr.State != providers.ChangeRequestOpen || !m.carries(cr.HeadSHA) {
			return false
		}
		for _, who := range []string{hmPred, hmRepl} {
			if !m.carriesOwn(cr.HeadSHA, who) {
				continue
			}
			if x := m.get(who); x != nil && (who != hmPred || !handedOff(x)) {
				// A live request on the pull request, unless it is the predecessor that ended HandedOff: the pull
				// request was then the replacement's alone.
				return false
			}
		}
		// The model has one pull request, the one the request in gone left open when it let go.
		return len(gone) > 0
	case !all:
		return false
	case about == aboutUnrecorded:
		return m.merge != nil && gone[m.merge.owner]
	case strings.HasPrefix(about, aboutHolder):
		return gone[other[strings.TrimPrefix(about, aboutHolder)]]
	}
	return false
}

// excusedBy is the name of the shape that excuses a violation of p about about now, "" for none.
func (m *hmWorld) excusedBy(p, about string) string {
	for _, sh := range hmShapes {
		if slices.Contains(strings.Fields(sh.excuses), p) && sh.matching(m, about) {
			return sh.name
		}
	}
	return ""
}

// fail notes a violation of property p; one per property. A violation a named shape excuses is counted under it.
func (m *hmWorld) fail(p, format string, args ...any) {
	m.failAbout(p, "", format, args...)
}

// failAbout is fail for a violation about about (aboutLockFree, aboutUnrecorded, aboutHolder plus a name), which only
// the shapes that excuse that may excuse.
func (m *hmWorld) failAbout(p, about, format string, args ...any) {
	if shape := m.excusedBy(p, about); shape != "" {
		if m.excused[shape] == 0 {
			m.logf("EXCUSED (%s) %s: %s", shape, p, fmt.Sprintf(format, args...))
		}
		m.excused[shape]++
		return
	}
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
		personCommits: map[string]bool{}, replCommits: map[string]bool{}, once: map[string]error{},
		lose: map[string]bool{}, conflict: map[string]bool{}, crashAt: -1, writers: map[string]bool{}, awaiting: map[string]bool{},
		gone288: map[string]bool{}, goneMissing: map[string]bool{},
		held: map[string]bool{}, reach: map[string]bool{}, excused: map[string]int{}, releasedAtReopen: map[string]bool{},
		waitingAtRemoval: map[string]bool{}, expected: map[string]int{}, lastErr: map[string]error{},
		lastRes: map[string]reconcile.Result{}}
	base, err := g.BranchHead(ctx, mainBranch)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.CreateBranch(ctx, hmOtherBranch, base); err != nil {
		t.Fatal(err)
	}
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
		m.replCommit, m.replCommits[head] = head, true
	case hmStartTookOver:
		m.replCommit, m.replCommits[head] = head, true
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
		OpenRepository: func(_ context.Context, repo api.GitRepository) (providers.GitProvider, error) {
			if err := m.gitFail("open"); err != nil {
				return nil, err
			}
			if m.settings == settingsUnusable {
				return nil, fmt.Errorf("open %s: %w", repo.URL, providers.ErrInvalidConfig)
			}
			// A provider opened for a target branch answers about the pull requests to that branch.
			return g.ForTarget(string(repo.TargetBranch)), nil
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
	branch := mainBranch
	if s.m.settings == settingsBranch {
		branch = hmOtherBranch
	}
	res.Source.TargetBranch = api.GitBranch(branch)
	head, err := s.m.g.BranchHead(ctx, branch)
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
	if hmWrites[op] && m.awaiting[m.actor] && m.otherMayRecord(m.actor) {
		who := m.actor
		m.mu.Unlock()
		m.fail("P4", "the %s attempted %s while it waits for the other request's merge record", who, op)
		m.mu.Lock()
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

// otherMayRecord reports whether the request of the handoff other than who may still record the merge of the pull
// request as stored now: it exists, is not being deleted, holds the target lock or its intent, and holds no record of
// that merge yet. Only then does who wait for that record (mayStillRecord); a record or a release ends the wait, and
// the next look of who decides again.
func (m *hmWorld) otherMayRecord(who string) bool {
	other := map[string]string{hmPred: hmRepl, hmRepl: hmPred}[who]
	x := &api.RemediationRequest{}
	if err := m.base.Get(context.Background(), m.keys[other], x); err != nil {
		return false
	}
	if m.merge != nil && holdsMergeRecord(x, m.number, m.merge.head) {
		return false
	}
	return mayStillRecord(x)
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
	who := hmPred
	if rr.Name == m.keys[hmRepl].Name {
		who = hmRepl
	}
	awaiting := waitsForRecord(rr)
	m.mu.Lock()
	if awaiting {
		m.awaiting[who] = true
	}
	lose, conflict := m.lose[rr.Name], m.conflict[rr.Name]
	delete(m.lose, rr.Name)
	delete(m.conflict, rr.Name)
	if lose || conflict {
		m.faulted = true
	}
	m.mu.Unlock()
	if lose {
		m.logf("  status write of %s lost", rr.Name)
		m.reach["lost-write"] = true
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
	if !awaiting {
		m.mu.Lock()
		delete(m.awaiting, who)
		m.mu.Unlock()
	}
	// P4 follows the abort from the write that stores it, so a write to Git later in the same reconcile counts.
	if stored.Name == m.keys[hmPred].Name && stored.Status.Handoff != nil && stored.Status.Handoff.To != nil &&
		stored.Status.Handoff.To.Phase == api.HandoffAborted {
		m.mu.Lock()
		if !m.abortStored {
			m.abortStored = true
			m.trace = append(m.trace, fmt.Sprintf("[%d +%v]   the abort is stored", m.stepNo, m.now.Sub(m.t0)))
		}
		m.mu.Unlock()
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
	if repl != nil && repl.DeletionTimestamp.IsZero() && !handoffSettled(repl) {
		// A replacement whose handoff a verdict settled (Aborted) records nothing any more, as a deleted one.
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
	return slices.ContainsFunc(above, func(sha string) bool { return m.replCommits[sha] })
}

// learnReplCommit finds a commit the replacement pushed: a commit on top of the predecessor's that no person pushed.
// After a force push removed its commit, the replacement may push another one.
func (m *hmWorld) learnReplCommit() {
	head, err := m.g.BranchHead(context.Background(), m.branch)
	if err != nil {
		return
	}
	above, ok := m.history(head)
	if !ok {
		return
	}
	for _, sha := range slices.Backward(above) {
		if !m.personCommits[sha] && !m.replCommits[sha] {
			m.replCommit, m.replCommits[sha] = sha, true
			m.logf("  the replacement's commit is %s", short(sha))
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

// readRecord reports whether rr's status stores a record of the merge of pull request number, the first read of it
// included (decision of 2026-11-19 on #880): a wait of kind MergeCheck or MergeContent that names the pull request, or
// the merge recorded in any form on that pull request (recordsMerge: a receipt, mergedAt, Merged, or a merge that did
// not deliver the plan as shown, MergedWithForeignChange among them). PullRequestOpened marked merged alone is no
// record: the settlement of a predecessor that ended HandedOff marks it so. It is the model's own reading of the
// status, written apart from the reconciler's.
func readRecord(rr *api.RemediationRequest, number int64) bool {
	if rr == nil {
		return false
	}
	for _, w := range rr.Status.GitWaits {
		if (w.Kind == api.GitWaitMergeCheck || w.Kind == api.GitWaitMergeContent) && w.PullRequest != nil &&
			w.PullRequest.Number == number {
			return true
		}
	}
	if rr.Status.PullRequest == nil || rr.Status.PullRequest.Number != number {
		return false
	}
	return recordsMerge(rr)
}

// observeFirstRead checks the first read of P1 after every step, for a merge of any head: the first request seen
// holding a record of the merge is noted, the other never holds one at the same time, and the first one, alive and not
// being deleted, keeps it. A request being deleted drops its merge waits (#878) and records nothing, so the other may
// record the merge once that record is gone.
func (m *hmWorld) observeFirstRead(reqs map[string]*api.RemediationRequest) {
	mg := m.merge
	if mg == nil {
		return
	}
	var holding []string
	for _, who := range []string{hmPred, hmRepl} {
		if readRecord(reqs[who], m.number) {
			holding = append(holding, who)
		}
	}
	switch {
	case len(holding) == 2 && mg.firstRead != "":
		other := map[string]string{hmPred: hmRepl, hmRepl: hmPred}[mg.firstRead]
		m.fail("P1", "the %s holds a record of the merge of %s while the %s holds the record of its first read (pred: %s, "+
			"repl: %s)", other, short(mg.head), mg.firstRead, describe(reqs[hmPred]), describe(reqs[hmRepl]))
	case len(holding) == 2:
		m.fail("P1", "both requests stored a record of the merge of %s in one step", short(mg.head))
	case len(holding) == 1 && mg.firstRead == "":
		mg.firstRead = holding[0]
		m.reach["first-read-"+holding[0]] = true
		m.logf("  the %s stored the first read of the merge", holding[0])
	case mg.firstRead != "" && !slices.Contains(holding, mg.firstRead):
		if x := reqs[mg.firstRead]; x != nil && x.DeletionTimestamp.IsZero() {
			m.fail("P1", "the %s lost the record of its first read of the merge of %s (%s)", mg.firstRead,
				short(mg.head), describe(x))
		}
	}
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

// observeMerge checks P1 and P2 after a merge of a request's own commit, on reqs as they are now.
func (m *hmWorld) observeMerge(reqs map[string]*api.RemediationRequest) {
	mg := m.merge
	if mg == nil || (mg.owner != hmPred && mg.owner != hmRepl) {
		return
	}
	other := map[string]string{hmPred: hmRepl, hmRepl: hmPred}[mg.owner]
	if recordsMerge(reqs[other]) && !m.foreignForGone(reqs) && !m.foreignOnReplacement(reqs) {
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

// observe checks the properties that hold after every step, and notes what later checks need.
func (m *hmWorld) observe() {
	p, rr := m.get(hmPred), m.get(hmRepl)
	reqs := map[string]*api.RemediationRequest{hmPred: p, hmRepl: rr}
	cr := m.pr()
	m.mu.Lock()
	m.prEnded = cr.State != providers.ChangeRequestOpen || m.branchGone
	m.mu.Unlock()
	m.note(p, rr)
	// P1 and P2 after a merge of a request's own commit; the first read of P1 after a merge of any head.
	m.observeFirstRead(reqs)
	m.observeMerge(reqs)
	m.observeLock(reqs)
}

// note records what the sequence reached and the shapes later checks excuse.
func (m *hmWorld) note(p, rr *api.RemediationRequest) {
	if p != nil && p.Status.Handoff != nil && p.Status.Handoff.To != nil &&
		p.Status.Handoff.To.Phase == api.HandoffAborted && !m.abortStored {
		m.abortStored = true
		m.logf("  the abort is stored")
	}
	if m.abortStored {
		m.reach["abort-stored"] = true
	}
	if rr != nil {
		for _, w := range rr.Status.GitWaits {
			if w.Kind == api.GitWaitHandoffRecovery {
				m.reach["refusal-to-HandoffRecovery"] = true
			}
		}
	}
	// The #288 shape: a request let go of its finalizer while being deleted with the pull request open, at the end of
	// its cleanup bound: its cleanup was failing (CleanupFailing True on its last copy), and a bound of it had elapsed
	// on the model's clock when it let go (closeRefusedGrace after CleanupRefused, or cleanupFailingBound after
	// CleanupFailing). A request that let go with the pull request open in any other way is not this shape.
	letGo := func(now, last *api.RemediationRequest) bool {
		if now != nil || last == nil || last.DeletionTimestamp.IsZero() {
			return false
		}
		failing := meta.FindStatusCondition(last.Status.Conditions, api.ConditionCleanupFailing)
		if failing == nil || failing.Status != metav1.ConditionTrue {
			return false
		}
		refused := meta.FindStatusCondition(last.Status.Conditions, api.ConditionCleanupRefused)
		return boundLeft(failing, cleanupFailingBound, m.now) <= 0 ||
			(refused != nil && refused.Status == metav1.ConditionTrue && boundLeft(refused, closeRefusedGrace, m.now) <= 0)
	}
	// A request deleted while the repository is not in the settings: its finalizer lets go, the pull request left open
	// (design 6, "A repository that left the settings ... does not block the deletion").
	goneMissing := func(now, last *api.RemediationRequest) bool {
		return now == nil && last != nil && !last.DeletionTimestamp.IsZero() && m.settings == settingsRemoved
	}
	open := m.pr().State == providers.ChangeRequestOpen
	for who, now := range map[string]*api.RemediationRequest{hmPred: p, hmRepl: rr} {
		last := map[string]*api.RemediationRequest{hmPred: m.lastPred, hmRepl: m.lastRepl}[who]
		if open && goneMissing(now, last) && !m.goneMissing[who] {
			m.goneMissing[who] = true
			m.logf("  the %s, deleted while the repository is not in the settings, let go with the pull request open", who)
		}
		if open && letGo(now, last) && !m.gone288[who] {
			m.gone288[who] = true
			m.logf("  the #288 shape: the %s let go at its cleanup bound with the pull request open", who)
		}
	}
	m.lastRepl, m.lastPred = rr, p
}

// observeLock checks P3 after every step.
func (m *hmWorld) observeLock(reqs map[string]*api.RemediationRequest) {
	cr := m.pr()
	// P3. While the repository is not in the settings P3 asks nothing of the lock: a merge that did not deliver the
	// plan as shown releases it (tenth decision of 2026-11-18 on #771), and so does the finalizer of a request
	// deleted then. The release of a replacement that ended before its takeover there was withdrawn (decision of
	// 2026-11-19 on #786); P6 checks that it keeps the lock, and the run after the repository's return checks P3.
	repoGone := m.settings == settingsRemoved
	free := true
	for who, x := range reqs {
		holds := x != nil && ptr.Deref(x.Status.HoldsTarget, false)
		if holds {
			free = false
		}
		if m.held[who] && !holds && x != nil && !repoGone {
			switch {
			case cr.State == providers.ChangeRequestOpen && who == hmRepl && m.carriesOwn(cr.HeadSHA, hmRepl):
				m.fail("P3", "the replacement released the lock while its commit is on the open pull request (%s)",
					describe(x))
			case cr.State == providers.ChangeRequestMerged && m.merge != nil && m.merge.owner == who && !recordsMerge(x) &&
				!m.foreignOnReplacement(reqs) && (!m.merge.afterTakeover || who != hmPred || !handedOff(x)):
				m.fail("P3", "the %s released the lock on a merge of its commit it does not record (%s)", who,
					describe(x))
			}
		}
		m.held[who] = holds
	}
	if free && cr.State == providers.ChangeRequestOpen && m.carries(cr.HeadSHA) && !repoGone &&
		!m.reopenExcuses(cr.HeadSHA) {
		m.failAbout("P3", aboutLockFree, "the lock is free while the open pull request carries a commit of a request (pred: %s, repl: %s)",
			describe(reqs[hmPred]), describe(reqs[hmRepl]))
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
	m.lastRes[who], m.lastErr[who] = res, err
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
		m.reach["deletion"] = true
	case hSettings:
		m.changeSettings(s.Variant)
	case hMerge, hPush, hClose, hForce:
		m.personEvent(s)
	case hReopen:
		m.reopen()
	case hBranch:
		if m.branchGone {
			return
		}
		if err := m.g.DeleteBranch(m.branch); err != nil {
			m.t.Fatal(err)
		}
		m.branchGone = true
		m.reach["branch-deleted"] = true
	}
}

// changeSettings makes a settings change: the plan's repository leaves the settings (an entry for another repository
// takes its place), its entry cannot open a provider, its target branch changes, or an unusable entry is fixed. A
// repository that left the settings and a changed branch stay so; only settingsRestored, after the settling run, puts
// the repository back (with the branch a settings change named, if one did).
func (m *hmWorld) changeSettings(variant string) {
	if variant == settingsRestored {
		m.restoreRepository()
		return
	}
	if m.settings == settingsRemoved || (m.settings == settingsBranch && variant != settingsRemoved) {
		return
	}
	switch variant {
	case settingsFixed:
		if m.settings == settingsUnusable {
			m.settings = ""
		}
		return
	case settingsUnusable:
		m.settings = variant
		m.reach["settings-change"] = true
		return
	}
	ctx := context.Background()
	st := &api.RemediationSettings{}
	if err := m.base.Get(ctx, client.ObjectKey{Name: api.SettingsName}, st); err != nil {
		m.t.Fatal(err)
	}
	switch variant {
	case settingsRemoved:
		m.planURL = st.Spec.Repositories[0].URL
		st.Spec.Repositories[0].URL = hmOtherURL
		for _, who := range []string{hmPred, hmRepl} {
			if x := m.get(who); x != nil && waitsForRecord(x) {
				m.waitingAtRemoval[who] = true
			}
		}
	case settingsBranch:
		st.Spec.Repositories[0].TargetBranch = hmOtherBranch
		m.branchChanged = true
	}
	if err := m.base.Update(ctx, st); err != nil {
		m.t.Fatal(err)
	}
	m.settings = variant
	m.reach["settings-change"] = true
}

// restoreRepository puts the plan's repository back in the settings after it left them (decision of 2026-11-19 on
// #786, point 2): the settling run that follows must free the lock and record a merge once.
func (m *hmWorld) restoreRepository() {
	if m.settings != settingsRemoved {
		return
	}
	ctx := context.Background()
	st := &api.RemediationSettings{}
	if err := m.base.Get(ctx, client.ObjectKey{Name: api.SettingsName}, st); err != nil {
		m.t.Fatal(err)
	}
	st.Spec.Repositories[0].URL = m.planURL
	if err := m.base.Update(ctx, st); err != nil {
		m.t.Fatal(err)
	}
	m.settings = ""
	if m.branchChanged {
		m.settings = settingsBranch
	}
	clear(m.waitingAtRemoval)
	m.reach["repository-restored"] = true
}

// waitsForRecord reports whether x waits for the other request of the handoff to record the merge of its pull request.
func waitsForRecord(x *api.RemediationRequest) bool {
	c := meta.FindStatusCondition(x.Status.Conditions, api.ConditionApproved)
	return c != nil && c.Reason == ReasonMergeRecordPending
}

// reopen is a person reopening the closed pull request. A reopen while no request holds the lock is the named
// exclusion of P3: a request whose lock a verdict released does not take it back (design 6.5).
func (m *hmWorld) reopen() {
	if m.pr().State != providers.ChangeRequestClosed {
		return
	}
	if err := m.g.Reopen(m.number); err != nil {
		m.logf("  the reopen is refused: %v", err)
		return
	}
	m.reach["reopen"] = true
	for _, who := range []string{hmPred, hmRepl} {
		if x := m.get(who); (x == nil || !ptr.Deref(x.Status.HoldsTarget, false)) && !m.releasedAtReopen[who] {
			m.releasedAtReopen[who] = true
			m.logf("  reopened after the %s's lock was released", who)
		}
	}
}

// reopenExcuses reports whether the named exclusion "a reopen after a release" covers a free lock while the open
// pull request has head: every request whose commit head carries had its lock released when a person reopened it.
func (m *hmWorld) reopenExcuses(head string) bool {
	released := func(who string) bool { return m.releasedAtReopen[who] || m.get(who) == nil }
	if !m.reach["reopen"] || !released(hmPred) || (m.carriesOwn(head, hmRepl) && !released(hmRepl)) {
		return false
	}
	m.excused["reopen after a release"]++
	return true
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
		if s.NoID {
			// The host answers every read of the merged pull request without a merge commit ID: the merge can only be
			// recorded without a receipt, after the wait for the merge commit (mergeCommitWait).
			if err := m.g.SetMergeCommit(m.number, ""); err != nil {
				m.t.Fatal(err)
			}
			m.reach["merge-without-commit-id"] = true
		}
		m.merge = &hmMerge{head: cr.HeadSHA, owner: m.ownerOf(cr.HeadSHA), failed: map[string]bool{},
			recorded: map[string]bool{}, noID: s.NoID}
		if pred := m.get(hmPred); m.merge.owner == hmPred && (m.replTookOver() || (pred != nil && handedOff(pred))) {
			// The replacement took the pull request over, and a person put the predecessor's commit back with a force
			// push: the predecessor records that merge as its own while it can (alive, not handed off), or the
			// replacement records it as a foreign change (foreignOnReplacement). One record either way.
			m.merge.afterTakeover = true
		}
		for _, who := range []string{hmPred, hmRepl} {
			if x := m.get(who); x != nil && breakerOutcome(x) == outcomeFailure {
				m.merge.failed[who] = true
			}
		}
		m.logf("  merged head %s, the %s's commit", short(cr.HeadSHA), m.merge.owner)
		m.reach["merge"] = true
		if m.merge.owner == hmPred || m.merge.owner == hmRepl {
			m.reach["own-commit-merge"] = true
		}
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
	case hForce:
		// A force push that removes every commit on top of the predecessor's, and, for forcePerson, puts a person's
		// commit there instead.
		if err := m.g.ForcePush(m.branch, m.predCommit); err != nil {
			m.t.Fatal(err)
		}
		if s.Variant == forcePerson {
			sha, err := m.g.PushFiles(m.branch, map[string][]byte{"person.yaml": []byte(fmt.Sprintf("force: %d\n", m.stepNo))})
			if err != nil {
				m.t.Fatal(err)
			}
			m.personCommits[sha] = true
		}
	}
}

// replTookOver reports whether the replacement exists and recorded the takeover.
func (m *hmWorld) replTookOver() bool {
	rr := m.get(hmRepl)
	return rr != nil && tookOver(rr)
}

// ownerOf is whose commit head is: hmPred, hmRepl, gap870 (further commits on top of the replacement's commit,
// #870) or foreign (a person's commit on top of the predecessor's alone).
func (m *hmWorld) ownerOf(head string) string {
	switch {
	case head == m.predCommit:
		return hmPred
	case m.replCommits[head]:
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
	// An unusable entry is fixed, as reads answer; a repository that left the settings and a changed branch stay.
	m.changeSettings(settingsFixed)
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
	m.liveness()
	mg := m.merge
	if mg == nil || (mg.owner != hmPred && mg.owner != hmRepl) {
		return
	}
	if m.settings == settingsRemoved {
		// The repository left the settings: no read of it can start, so reads never answer, and nothing records a
		// merge made through it.
		return
	}
	reqs := map[string]*api.RemediationRequest{hmPred: m.get(hmPred), hmRepl: m.get(hmRepl)}
	m.settledBreaker(mg, reqs)
	m.settledRecord(mg, reqs)
}

// settledBreaker checks P2 once reads answer: no request is a failure for the breaker that was not one when the merge
// was made.
func (m *hmWorld) settledBreaker(mg *hmMerge, reqs map[string]*api.RemediationRequest) {
	for _, who := range []string{hmPred, hmRepl} {
		if (who == hmPred && m.foreignForGone(reqs)) || (who == hmRepl && m.foreignOnReplacement(reqs)) {
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
}

// settledRecord checks P1 after the settling run: the merge of a request's own commit is recorded once, on the
// request the one owner rule names, with its receipt.
func (m *hmWorld) settledRecord(mg *hmMerge, reqs map[string]*api.RemediationRequest) {
	if m.releasedAtReopen[mg.owner] {
		// A reopen after a release: the request whose lock a verdict released records nothing of the reopened pull
		// request (design 6.5).
		m.excused["reopen after a release"]++
		return
	}
	owner := m.get(mg.owner)
	if mg.afterTakeover && (owner == nil || !owner.DeletionTimestamp.IsZero()) && !mg.recorded[hmPred] {
		// The predecessor went before it recorded the merge of its commit put back after the takeover: the replacement
		// records it as a foreign change.
		if !m.foreignOnReplacement(reqs) && reqs[hmRepl] != nil && reqs[hmRepl].DeletionTimestamp.IsZero() {
			m.failAbout("P1", aboutUnrecorded, "the merge of the predecessor's commit %s after the takeover is recorded on neither request "+
				"(repl: %s)", short(mg.head), describe(reqs[hmRepl]))
		}
		return
	}
	if owner == nil || (mg.owner == hmRepl && !owner.DeletionTimestamp.IsZero() && !mg.recorded[hmRepl]) {
		m.settledGoneOwner(mg, reqs)
		return
	}
	if mg.afterTakeover && handedOff(owner) && !mg.recorded[hmRepl] && (reqs[hmRepl] == nil ||
		!reqs[hmRepl].DeletionTimestamp.IsZero()) {
		// The same shape for the predecessor's commit put back after the takeover: the pull request was the
		// replacement's alone, and it was deleted before it recorded the merge.
		m.excused["#875 replacement deleted before it recorded, predecessor handed off"]++
		return
	}
	if mg.noID {
		// The host never reported a merge commit ID: no receipt can be made, and the merge is recorded on its owner
		// without one (Merged, then Inconclusive with NoMergeReceipt).
		if !recordsMerge(owner) && !m.foreignOnReplacement(reqs) {
			m.failAbout("P1", aboutUnrecorded, "the merge of the %s's commit %s, reported without a merge commit ID, is not recorded on it "+
				"(pred: %s, repl: %s)", mg.owner, short(mg.head), describe(m.get(hmPred)), describe(m.get(hmRepl)))
		}
		return
	}
	if (owner.Status.MergeReceipt == nil || owner.Status.MergeReceipt.HeadSHA != mg.head) && !m.foreignOnReplacement(reqs) {
		m.failAbout("P1", aboutUnrecorded, "the merge of the %s's commit %s is not recorded on it with its receipt (pred: %s, repl: %s)",
			mg.owner, short(mg.head), describe(m.get(hmPred)), describe(m.get(hmRepl)))
	}
}

// settledGoneOwner is settledRecord for a merge whose owner is gone, or is the replacement being deleted before it
// recorded the merge.
func (m *hmWorld) settledGoneOwner(mg *hmMerge, reqs map[string]*api.RemediationRequest) {
	// #786, decision 2 of the model result of 2026-11-18: a deleted replacement records nothing; unless it
	// recorded the merge before it went, the predecessor records it as a foreign change, a predecessor that handed
	// its pull request over (to Done) included: takeover, merge, replacement deleted ends with one record.
	if mg.owner == hmRepl && !mg.recorded[hmRepl] && reqs[hmPred] != nil && handedOff(reqs[hmPred]) {
		// The predecessor ended HandedOff while the replacement lived (step 5 is written by the replacement alone,
		// and at its deletion only once it recorded its merge): the pull request was the replacement's alone, and
		// a request deleted before it records the merge of its own pull request records nothing, as any request
		// (finalize reads it merged and lets go). The predecessor has ended and records nothing either.
		m.excused["#875 replacement deleted before it recorded, predecessor handed off"]++
		return
	}
	if mg.owner == hmRepl && !mg.recorded[hmRepl] && reqs[hmPred] != nil && m.releasedAtReopen[hmPred] {
		// The fallback is the predecessor, and a verdict released its lock before the reopen: it records nothing
		// of the reopened pull request either (design 6.5), so the merge of the deleted replacement's commit
		// stays unrecorded.
		m.excused["reopen after a release"]++
		return
	}
	if mg.owner == hmRepl && !mg.recorded[hmRepl] && reqs[hmPred] != nil && !m.foreignForGone(reqs) {
		m.failAbout("P1", aboutUnrecorded, "the merge of the deleted replacement's commit %s is recorded on neither request (pred: %s)",
			short(mg.head), describe(reqs[hmPred]))
	}
}

// liveness checks P6 after the settling run: once the pull request is merged or closed, or its branch or repository is
// gone, no request holds the target lock. A reopen after a release is outside it by name (reopenedAfterRelease): the
// pull request is then open, and P6 asks nothing of an open one.
func (m *hmWorld) liveness() {
	cr := m.pr()
	if m.merge != nil || cr.State != providers.ChangeRequestOpen {
		m.reach["merge-or-close"] = true
	}
	var why string
	switch {
	case m.settings == settingsRemoved:
		why = "the repository left the settings"
	case m.branchGone:
		why = "the branch is deleted"
	case cr.State == providers.ChangeRequestMerged:
		why = "the pull request is merged"
	case cr.State == providers.ChangeRequestClosed:
		why = "the pull request is closed"
	default:
		return
	}
	for _, who := range []string{hmPred, hmRepl} {
		if x := m.get(who); x != nil && ptr.Deref(x.Status.HoldsTarget, false) {
			if name := m.keptWhileMissing(who, x); name != "" {
				m.expectMissingShown(who, x, name)
				continue
			}
			m.failAbout("P6", aboutHolder+who, "%s, and the %s still holds the target lock after the settling run (%s)", why,
				who, describe(x))
		}
	}
}

// keptWhileMissing names the expectation a holder of the lock meets while the repository is not in the settings, ""
// for none. A holder with no verdict stored keeps the lock, as nothing can read its pull request any more (design 6,
// the table of stops; tenth decision of 2026-11-18 on #771; decision of 2026-11-19 on #786, points 1 and 3): one that
// had stopped (the replacement whose recovery holds the lock included, whose release there the same decision
// withdrew), and one that waited for the other request's merge record when the repository left. A request still at
// work, or one with a verdict stored, has no such expectation.
func (m *hmWorld) keptWhileMissing(who string, x *api.RemediationRequest) string {
	switch {
	case m.settings != settingsRemoved || verdictStored(x):
		return ""
	case m.waitingAtRemoval[who]:
		return "holder waiting for the merge record keeps the lock while the repository is missing"
	case m.stopped(x):
		return "stopped holder keeps the lock while the repository is missing"
	}
	return ""
}

// verdictStored reports whether x's status records a merge that settles its lock without a read: its receipt, or a
// phase after the merge. A verdict counts once it is stored (decision of 2026-11-19 on #786, point 3): a read whose
// status write was lost, conflicted or cut by a crash is none, nor is a wait for the other request's merge record
// (MergeRecordPending stores no merge). A close stored without a merge (closedOnly) is no verdict of the lock either:
// another pull request of the branch may still carry the change, so the lock waits for a read of the branch.
func verdictStored(x *api.RemediationRequest) bool {
	return x.Status.MergeReceipt != nil || slices.Contains(successPhases, x.Status.Phase) || mergedAndEnded(x)
}

// expectMissingShown checks the other half of a named expectation of keptWhileMissing: the holder shows why it keeps
// the lock (condition PullRequestUnreadable, reason RepositoryUnavailable), and counts it.
func (m *hmWorld) expectMissingShown(who string, x *api.RemediationRequest, name string) {
	c := meta.FindStatusCondition(x.Status.Conditions, api.ConditionPullRequestUnreadable)
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != reasonRepositoryUnavailable {
		m.fail("P6", "the repository left the settings and the %s keeps the lock (%s), but does not show condition "+
			"%s with reason %s (%+v)", who, name, api.ConditionPullRequestUnreadable, reasonRepositoryUnavailable, c)
		return
	}
	m.expected[name]++
}

// stopped reports whether x had stopped: ended, or NeedsIntervention.
func (m *hmWorld) stopped(x *api.RemediationRequest) bool {
	return ended(x.Status.Phase) || x.Status.Phase == api.PhaseNeedsIntervention
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
	if m.settings == settingsRemoved {
		// The repository comes back, and the settling run that follows frees the lock and records a merge once
		// (decision of 2026-11-19 on #786, point 2): settle checks P6, P1 and P2 again with reads answering.
		m.stepNo++
		m.apply(hmStep{Kind: hSettings, Variant: settingsRestored})
		m.observe()
		m.settle()
	}
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
		{1, hClose}, {1, hReopen}, {1, hBranch}, {1, hForce}, {1, hSettings}}
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
			s.NoID = rng.IntN(8) == 0
		case hForce:
			s.Variant = []string{forceDrop, forcePerson}[rng.IntN(2)]
		case hSettings:
			s.Variant = []string{settingsRemoved, settingsUnusable, settingsBranch, settingsFixed}[rng.IntN(4)]
		}
		steps = append(steps, s)
	}
	// Most sequences reach a merge or a close: one without a step that ends the pull request gets one, nine times in
	// ten, at a random place in its second half, so that later steps still follow it.
	ends := slices.ContainsFunc(steps, func(s hmStep) bool { return s.Kind == hMerge || s.Kind == hClose || s.Kind == hBranch })
	if !ends && rng.IntN(10) > 0 {
		end := hmStep{Kind: hMerge, Commit: rng.IntN(4) > 0, NoID: rng.IntN(8) == 0}
		if rng.IntN(5) == 0 {
			end = hmStep{Kind: hClose}
		}
		steps[hmSteps/2+rng.IntN(hmSteps-hmSteps/2)] = end
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
		// The reach counts (and the failures per property) are in the log of every run, go test -v shows them.
		t.Logf("handoff model summary: %v", counts)
	})
	for _, seed := range hmSeeds(t) {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			t.Parallel()
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
			for k := range m.reach {
				counts["reach "+k]++
			}
			for k := range m.excused {
				counts["excused "+k]++
			}
			for k := range m.expected {
				counts["expected "+k]++
			}
			mu.Unlock()
			if os.Getenv("SHADEWRIGHT_HANDOFF_MODEL_TRACE") != "" {
				t.Logf("trace:\n  %s", strings.Join(m.trace, "\n  "))
			}
			if p == "" {
				if len(m.excused) > 0 {
					t.Skipf("the design does not require it: shapes %v", slices.Sorted(maps.Keys(m.excused)))
				}
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

// Third round of #786, seed 16384: both requests are deleted while the replacement's commit is on the open pull request
// and the settings entry cannot be used. The predecessor's finalizer must not let go as if the repository were gone: the
// entry may be fixed, so the cleanup fails within the bound of CleanupFailing (reason RepositoryUnusable), keeps the
// finalizer and the lock, and closes the pull request once the entry works again.
func TestHandoffModelDeletionWithAnUnusableEntry(t *testing.T) {
	steps := []hmStep{{Kind: hDelete, Obj: hmRepl}, stRecRepl, {Kind: hSettings, Variant: settingsUnusable},
		{Kind: hDelete, Obj: hmPred}, stRecPred}
	m := newHMWorld(t, requestStatusSchema(t), hmStartPushed)
	for i, s := range steps {
		m.stepNo = i + 1
		m.apply(s)
		m.observe()
	}
	p := m.get(hmPred)
	if p == nil || !controllerutil.ContainsFinalizer(p, FinalizerName) {
		t.Fatalf("the predecessor let go of its finalizer with an unusable entry and its pull request open: %s", describe(p))
	}
	if c := meta.FindStatusCondition(p.Status.Conditions, api.ConditionCleanupFailing); c == nil ||
		c.Reason != reasonRepositoryUnusable {
		t.Fatalf("CleanupFailing = %+v, want reason %s", c, reasonRepositoryUnusable)
	}
	hmDirected(t, hmStartPushed, steps)
}

// Third round of #786, seeds 29586, 30458, 54201 and 9008: the settings name another target branch, then both requests
// are deleted while the replacement's commit is on the open pull request. The finalizer closes that pull request on the
// branch the plan recorded instead of letting go with it open and no request holding its target.
func TestHandoffModelDeletionAfterABranchChange(t *testing.T) {
	m := hmDirected(t, hmStartPushed, []hmStep{{Kind: hSettings, Variant: settingsBranch}, {Kind: hDelete, Obj: hmRepl},
		{Kind: hDelete, Obj: hmPred}})
	if st := m.pr().State; st != providers.ChangeRequestClosed {
		t.Fatalf("the pull request is %s after both requests were deleted, want closed", st)
	}
}

// Third round of #786, seeds 27632 and 41857: after the takeover a person puts the predecessor's commit back and merges
// it, the predecessor records the merge with its receipt and is then deleted, and the replacement looks before the
// predecessor's finalizer runs. The merge is the predecessor's (it holds the receipt), so the replacement ends
// MergedForPredecessor and records no foreign change.
func TestHandoffModelPredecessorMergeWhileItIsDeleted(t *testing.T) {
	hmDirected(t, hmStartIntent, []hmStep{stRecRepl, {Kind: hForce, Variant: forceDrop}, stMerge, stRecPred,
		{Kind: hDelete, Obj: hmPred}, stRecRepl})
}

// Fourth round of #786, seed 90404: after the recorded takeover a person puts the predecessor's commit back and closes
// the pull request, the predecessor aborts its intent and releases the lock, then a person reopens the pull request and
// merges it. The predecessor cannot record that merge any more (a verdict released it), so the replacement, which
// still watches, must not wait for it: after the settling run no request holds the lock, and no request is left
// retrying with an error or still at work on the merged pull request.
func TestHandoffModelReopenAfterThePredecessorReleased(t *testing.T) {
	m := hmDirected(t, hmStartPushed, []hmStep{stRecRepl, stRecPred, {Kind: hForce, Variant: forceDrop},
		{Kind: hClose}, stRecPred, {Kind: hReopen}, stMerge})
	for _, who := range []string{hmPred, hmRepl} {
		x := m.get(who)
		if x == nil {
			continue
		}
		if ptr.Deref(x.Status.HoldsTarget, false) {
			t.Errorf("the %s holds the target lock after the settling run: %s", who, describe(x))
		}
		if err := m.lastErr[who]; err != nil {
			t.Errorf("the last reconcile of the %s failed (a retry no condition shows): %v", who, err)
		}
		if progresses(x) {
			t.Errorf("the %s still waits on the merged pull request: %s", who, describe(x))
		}
	}
}

// Fourth round of #786: a person merges the replacement's commit before the replacement recorded its takeover, and
// the predecessor, whose intent stands, looks first. The replacement records that merge, so the predecessor waits for
// it: it shows Approved False with MergeRecordPending, naming the replacement, and writes nothing to the merged pull
// request; the replacement then records the merge and the predecessor ends.
func TestHandoffModelWaitForTheReplacementsRecordIsShown(t *testing.T) {
	steps := make([]hmStep, 0, 4)
	steps = append(steps, stMerge, stRecPred)
	m := newHMWorld(t, requestStatusSchema(t), hmStartPushed)
	for i, s := range steps {
		m.stepNo = i + 1
		m.apply(s)
		m.observe()
	}
	for _, l := range m.trace {
		if strings.Contains(l, "git write") {
			t.Fatalf("a write to the merged pull request while the replacement's record is awaited: %s", l)
		}
	}
	p := m.get(hmPred)
	if c := meta.FindStatusCondition(p.Status.Conditions, api.ConditionApproved); c == nil ||
		c.Reason != ReasonMergeRecordPending || !strings.Contains(c.Message, string(p.Status.Handoff.To.Name)) {
		t.Fatalf("Approved = %+v, want reason %s naming the replacement (%s)", c, ReasonMergeRecordPending, describe(p))
	}
	hmDirected(t, hmStartPushed, append(steps, stRecRepl, stRecPred))
}

// #880, seed 13030 (shape B): after the takeover a person puts the predecessor's commit back and merges it, and the
// host reports no merge commit ID. The predecessor reads the merge first and stores its wait for the merge commit
// (MergeCheck, naming the pull request and the merged head); the replacement, which then ends the predecessor
// HandedOff, must read that record and leave the merge to it instead of recording it as a foreign change.
func TestHandoffModelFirstReadByThePredecessorWithoutAMergeCommitID(t *testing.T) {
	m := hmDirected(t, hmStartPushed, []hmStep{stRecRepl, {Kind: hForce, Variant: forceDrop},
		{Kind: hMerge, Commit: true, NoID: true}})
	if m.merge == nil || m.merge.firstRead != hmPred {
		t.Fatalf("the first read of the merge is not the predecessor's: %+v", m.merge)
	}
}

// #880, seed 26733 (shape D): a person merges the replacement's commit, the host reports no merge commit ID, and the
// replacement, stopped while the repository is out of the settings, reads it first once the repository is back and
// records it as MergedWithForeignChange, which sets no mergedAt. The predecessor must read that record and not record
// the same merge again.
func TestHandoffModelFirstReadByTheReplacementAsAForeignChange(t *testing.T) {
	m := hmDirected(t, hmStartPushed, []hmStep{{Kind: hMerge, Commit: true, NoID: true},
		{Kind: hFault, Code: 403, Op: opGet}, stRecRepl, stRecRepl, {Kind: hSettings, Variant: settingsRemoved}})
	if m.merge == nil || m.merge.firstRead != hmRepl {
		t.Fatalf("the first read of the merge is not the replacement's: %+v", m.merge)
	}
}
