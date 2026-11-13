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
	"slices"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/policy/freshness"
	"github.com/pkalin/shadewright/internal/providers"
)

// changeState is the closed set of answers to "what is the state of this request's change" (#688). Every caller
// in this package that reads a pull request of a request asks changeOf, and no other function reads one, so the
// rule per answer is the same everywhere:
//   - changeUnknown: a read failed, conflicted with another read, or answered about another pull request. It is
//     retried with backoff and ends nothing, releases nothing and removes no finalizer.
//   - changeOpen, changeMerged, changeClosed: the pull request was read in that state.
//   - changeGone: the pull request is confirmed not to be this request's: a 404 that the listing of every pull
//     request of the branch confirms, on two looks at least confirmWait apart (status.goneSeen), an answer about the recorded number itself that is from another branch or to
//     another base branch, a recorded one without this request's marker when the caller asked for it, or, with no
//     number to read, a branch that has no pull request with this request's marker.
type changeState int

const (
	changeUnknown changeState = iota
	changeOpen
	changeMerged
	changeClosed
	changeGone
)

// changeStates lists every changeState, for the tests that check each caller has a rule for each.
var changeStates = []changeState{changeOpen, changeMerged, changeClosed, changeGone, changeUnknown}

func (s changeState) String() string {
	switch s {
	case changeOpen:
		return "open"
	case changeMerged:
		return "merged"
	case changeClosed:
		return "closed"
	case changeGone:
		return "gone"
	}
	return "unknown"
}

// changeQuery says what changeOf reads.
type changeQuery struct {
	// number is the pull request to read by its number. Zero reads the recorded one, or, when none is recorded,
	// every pull request of the request's branch.
	number int64
	// marker makes a pull request read by its number without this request's marker (ownsMarker) gone.
	marker bool
	// openOnBranch also lists the open pull requests of the branch when a number is read (the lock and the
	// finalizer need them). The listing of the branch has them anyway.
	openOnBranch bool
}

// changeVerdict is changeOf's answer.
type changeVerdict struct {
	state changeState
	// pr is the pull request the state is about: the one read by its number, or, from the listing of the
	// branch, the first one with this request's marker that is open, else merged, else closed (newest first). It
	// is also set for a pull request read by number that is gone only for the lack of the marker.
	pr providers.ChangeRequest
	// gone says why, for changeGone.
	gone string
	// err is the failed read, for changeUnknown; never nil then.
	err error
	// own is every pull request of the branch with this request's marker, newest first (listing of the branch).
	own []providers.ChangeRequest
	// foreign is true when the listing of the branch has a pull request without this request's marker.
	foreign bool
	// open is every open pull request of the branch to the target branch, whoever opened it (the listing of the
	// branch, or openOnBranch).
	open []providers.ChangeRequest
	// absent is true for changeGone from a 404 that the listing of the branch confirms, before confirmGone.
	absent bool
}

// errUnconfirmedNotFound means the Git host answered 404 for the pull request number, but the request's branch
// still lists a pull request with that number: the 404 was a passing fault, and the read is retried.
var errUnconfirmedNotFound = errors.New("the Git host answered 404 for the pull request, which its branch still lists")

// errConflictingReads means the read by number found the pull request closed, merged or missing while the
// listing of the open pull requests of the branch has it: one of them lags, and nothing is decided on either.
var errConflictingReads = errors.New("the pull request was read as not open, but the branch lists it open")

// errGoneUnconfirmed means the Git host answered 404 for the pull request number and the listing of the branch
// has none with that number either, but not yet on two looks at least confirmWait apart (status.goneSeen): both
// reads may come from a replica that lags, so the pull request is not taken for gone yet.
var errGoneUnconfirmed = errors.New("the Git host has no pull request with that number; a later look must find the same")

// unknownChangeError is the error of a changeUnknown verdict. It keeps the read's error in the chain (its class
// and Retry-After decide the backoff), and marks it as a failed read, so no caller takes a class such as
// ErrConflict for an answer (a merge): see isUnknownChange.
type unknownChangeError struct{ err error }

func (e *unknownChangeError) Error() string { return e.err.Error() }
func (e *unknownChangeError) Unwrap() error { return e.err }

// isUnknownChange reports whether err is or wraps the error of a changeUnknown verdict.
func isUnknownChange(err error) bool {
	_, ok := errors.AsType[*unknownChangeError](err)
	return ok
}

func unknownChange(err error) changeVerdict {
	if !isUnknownChange(err) {
		err = &unknownChangeError{err: err}
	}
	return changeVerdict{state: changeUnknown, err: err}
}

// changeOf is the one function in this package that reads a request's pull requests from the Git host, in the
// repository git is bound to (the plan's), and answers with a closed verdict (changeState).
//
// With a number (q.number, or the recorded one), it reads that pull request by its number and binds the answer to
// it (ADR-0030 section 1): the number asked for, the request's branch, its target branch. A newer pull request on
// the same branch never stands in for it. A 404 is gone only when the listing of every pull request of the branch
// to the target branch has none with that number, and only on a look at least confirmWait after the first that
// found the same (confirmGone, status.goneSeen); before that, or when the listing has the number or fails, the
// verdict is unknown.
// With q.openOnBranch it also lists the open pull requests of the branch, and a pull request read as closed,
// merged or missing that the listing has open is unknown (conflicting reads): the lock and the finalizer then
// hold and retry rather than trust either read. A pull request read as closed is checked against that listing
// for every caller, so a stale closed answer never ends a request or a handoff while the pull request is open.
//
// Without a number (none recorded) it reads every pull request of the branch to the target branch, in any state
// (ListChangeRequests), since the newest may hide an older open or merged one, and sorts them by this request's
// marker (own, foreign). The verdict's pr is the first own one that is open, else merged, else closed; each
// caller reads own and open for what it needs. No own one is gone. A listing that fails, a 404 included, is
// unknown, never an empty branch.
func (r *RemediationRequestReconciler) changeOf(ctx context.Context, git providers.GitProvider,
	rr *remediationv1alpha1.RemediationRequest, q changeQuery) changeVerdict {
	src := rr.Status.Source
	if src == nil {
		return unknownChange(errors.New("no source recorded"))
	}
	n := q.number
	if n == 0 && rr.Status.PullRequest != nil {
		n = rr.Status.PullRequest.Number
	}
	if n == 0 {
		return r.changeOnBranch(ctx, git, rr)
	}
	v := r.confirmGone(ctx, rr, n, r.changeByNumber(ctx, git, rr, n, string(src.TargetBranch)))
	if v.state == changeUnknown {
		return v
	}
	// A pull request read as closed is checked against the open listing of the branch even when the caller did
	// not ask for it: a stale closed answer must not end, release or cancel anything while the pull request is
	// open (one extra read, on closed only).
	if q.openOnBranch || v.state == changeClosed {
		open, err := git.ListChangeRequests(ctx, freshness.BranchName(rr), true)
		if err != nil {
			return unknownChange(freshness.WrapProvider("list the open change requests of the branch", err))
		}
		listed := slices.ContainsFunc(open, func(cr providers.ChangeRequest) bool { return cr.Number == n })
		if listed && v.state != changeOpen {
			return unknownChange(fmt.Errorf("%w: #%d", errConflictingReads, n))
		}
		if q.openOnBranch {
			v.open = open
		}
	}
	if q.marker && v.state != changeGone {
		switch ours, err := r.ownsMarker(ctx, v.pr.Body, rr); {
		case err != nil:
			return unknownChange(err)
		case !ours:
			v.state, v.gone = changeGone, fmt.Sprintf("pull request #%d lacks this request's marker", n)
		}
	}
	return v
}

// changeByNumber is changeOf's read by number, without the marker and the open listing.
func (r *RemediationRequestReconciler) changeByNumber(ctx context.Context, git providers.GitProvider,
	rr *remediationv1alpha1.RemediationRequest, n int64, target string) changeVerdict {
	cr, err := git.GetChangeRequest(ctx, n)
	switch {
	case providers.IsNotFound(err):
		all, lerr := git.ListChangeRequests(ctx, freshness.BranchName(rr), false)
		switch {
		case lerr != nil:
			// The 404 is left out of the chain: a caller must never read it as an answer.
			return unknownChange(fmt.Errorf("the Git host answered 404 for pull request #%d, and %w", n,
				freshness.WrapProvider("list the change requests of the branch", lerr)))
		case slices.ContainsFunc(all, func(cr providers.ChangeRequest) bool { return cr.Number == n }):
			return unknownChange(fmt.Errorf("%w: #%d", errUnconfirmedNotFound, n))
		}
		return changeVerdict{state: changeGone, absent: true, gone: fmt.Sprintf(
			"the Git host has no pull request #%d of Shadewright to the target branch", n)}
	case providers.IsUntrustedAnswer(err):
		return unknownChange(fmt.Errorf("the Git host answered with another pull request than #%d; it is read "+
			"again: %w", n, freshness.WrapProvider("find the change request", err)))
	case err != nil:
		return unknownChange(freshness.WrapProvider("find the change request", err))
	case cr.Number != n:
		return unknownChange(fmt.Errorf("the Git host answered with another pull request than #%d; it is read "+
			"again: %w", n, providers.ErrUntrustedAnswer))
	case cr.HeadBranch != freshness.BranchName(rr):
		return changeVerdict{state: changeGone, gone: fmt.Sprintf(
			"pull request #%d is from another branch than the request's", n)}
	case cr.BaseBranch != target:
		return changeVerdict{state: changeGone, gone: fmt.Sprintf(
			"pull request #%d targets another branch than the request's target branch", n)}
	}
	state, ok := stateOf(cr)
	if !ok {
		return unknownChange(fmt.Errorf("pull request #%d has a state the executor does not know: %q", n, cr.State))
	}
	return changeVerdict{state: state, pr: cr}
}

// confirmGone is the confirmation wait of a pull request missing by its number (#688), on the same terms as a merge
// conflict (conflicted): the first look that finds it missing (v.absent) records status.goneSeen with the time on
// the executor's clock and the number, and answers unknown (errGoneUnconfirmed, with the time left as the delay
// to ask for); a later look that finds it missing again at least confirmWait after the mark answers gone. A mark
// for another number, or one that lies more than mergeCommitClockSlack ahead of the clock (it went back), is
// recorded anew. Any other answer about the number removes the mark; a failed read, which answers nothing, leaves
// it. The mark is written
// here, the one place, with a patch of status.goneSeen alone on a copy of rr, so the caller's unsaved changes to
// rr stay as they are; rr gets the new field and resourceVersion. A write that fails answers unknown with its
// error, so nothing is decided on a mark that may not be there.
func (r *RemediationRequestReconciler) confirmGone(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	n int64, v changeVerdict) changeVerdict {
	seen := rr.Status.GoneSeen
	ours := seen != nil && seen.Number == n
	if !v.absent {
		// A failed read is no answer and leaves the mark; an answer about the number that is not its absence (the
		// pull request in any state, from another branch or base, or listed on the branch) removes it.
		answered := v.state != changeUnknown || errors.Is(v.err, errUnconfirmedNotFound)
		if ours && answered {
			if err := r.writeGoneSeen(ctx, rr, nil); err != nil {
				return unknownChange(err)
			}
		}
		return v
	}
	if ours {
		left, ok := confirmWaitRemaining(seen.At.Time, r.now())
		if ok && left == 0 {
			v.absent = false
			return v
		}
		if ok {
			return unknownChange(goneWait(n, left))
		}
	}
	mark := &remediationv1alpha1.GoneSeen{At: metav1.NewTime(r.now().Truncate(time.Second)), Number: n}
	if err := r.writeGoneSeen(ctx, rr, mark); err != nil {
		return unknownChange(err)
	}
	logf.FromContext(ctx).Info("The Git host has no pull request with the number of RemediationRequest; waiting to "+
		"see it missing again", "remediationRequest", client.ObjectKeyFromObject(rr), "pullRequest", n,
		"wait", confirmWait)
	return unknownChange(goneWait(n, confirmWait))
}

// goneWait is the error of a pull request missing on a look that does not confirm it yet: errGoneUnconfirmed,
// with the time left as the delay a caller that reads Retry-After waits (waitOrFail), at least stepDelay.
func goneWait(n int64, left time.Duration) error {
	return errors.Join(fmt.Errorf("%w: #%d", errGoneUnconfirmed, n), &freshness.ProviderError{
		Op: "confirm that the pull request is gone", RetryAfter: max(left, stepDelay)})
}

// writeGoneSeen writes status.goneSeen as mark (nil removes it), alone, with the resourceVersion of rr: a
// concurrent change is a conflict, returned for a retry.
func (r *RemediationRequestReconciler) writeGoneSeen(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	mark *remediationv1alpha1.GoneSeen) error {
	cp := rr.DeepCopy()
	base := cp.DeepCopy()
	cp.Status.GoneSeen = mark
	if err := r.Status().Patch(ctx, cp, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("record whether the pull request of RemediationRequest %s/%s is missing: %w",
			rr.Namespace, rr.Name, err)
	}
	rr.ResourceVersion = cp.ResourceVersion
	rr.Status.GoneSeen = cp.Status.GoneSeen
	return nil
}

// changeOnBranch is changeOf without a number: the listing of every pull request of the branch.
func (r *RemediationRequestReconciler) changeOnBranch(ctx context.Context, git providers.GitProvider,
	rr *remediationv1alpha1.RemediationRequest) changeVerdict {
	all, err := git.ListChangeRequests(ctx, freshness.BranchName(rr), false)
	if err != nil {
		return unknownChange(freshness.WrapProvider("list the change requests of the branch", err))
	}
	v := changeVerdict{state: changeGone, gone: "the branch has no pull request with this request's marker"}
	for _, cr := range all {
		if _, ok := stateOf(cr); !ok {
			return unknownChange(fmt.Errorf("pull request #%d has a state the executor does not know: %q",
				cr.Number, cr.State))
		}
		if cr.State == providers.ChangeRequestOpen {
			v.open = append(v.open, cr)
		}
		switch ours, err := r.ownsMarker(ctx, cr.Body, rr); {
		case err != nil:
			return unknownChange(err)
		case ours:
			v.own = append(v.own, cr)
		default:
			v.foreign = true
		}
	}
	for _, want := range []changeState{changeOpen, changeMerged, changeClosed} {
		if i := slices.IndexFunc(v.own, func(cr providers.ChangeRequest) bool {
			s, _ := stateOf(cr)
			return s == want
		}); i >= 0 {
			v.state, v.pr, v.gone = want, v.own[i], ""
			break
		}
	}
	return v
}

// stateOf maps the host's state of a pull request to a changeState; ok is false for a state it does not know.
func stateOf(cr providers.ChangeRequest) (changeState, bool) {
	switch cr.State {
	case providers.ChangeRequestOpen:
		return changeOpen, true
	case providers.ChangeRequestMerged:
		return changeMerged, true
	case providers.ChangeRequestClosed:
		return changeClosed, true
	}
	return changeUnknown, false
}

// ownMerged returns the newest merged pull request with this request's marker from the listing of the branch.
func (v changeVerdict) ownMerged() *providers.ChangeRequest {
	if i := slices.IndexFunc(v.own, func(cr providers.ChangeRequest) bool {
		return cr.State == providers.ChangeRequestMerged
	}); i >= 0 {
		return &v.own[i]
	}
	return nil
}
