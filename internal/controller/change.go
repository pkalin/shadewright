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
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/gitops/sourcemap"
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
//     request of the branch confirms, on two looks at least confirmWait apart (goneLooks), an answer about
//     the recorded number itself that is from another branch or to another base branch, a recorded one without
//     this request's marker when the caller asked for it, or, with no number to read, a branch that has no pull
//     request with this request's marker.
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
	// own is every pull request of the branch with this request's marker, newest first (listing of the branch,
	// or the listing that checks a 404, for errOwnListed).
	own []providers.ChangeRequest
	// foreign is true when the listing of the branch has a pull request without this request's marker.
	foreign bool
	// open is every open pull request of the branch to the target branch, whoever opened it (the listing of the
	// branch, or openOnBranch).
	open []providers.ChangeRequest
	// ownListed is true for changeUnknown with errOwnListed when the verdict carries every listing of the branch
	// that the call read (own, and open with openOnBranch). A verdict made unknown again by a later failure (a
	// listing, a status write) is a new one without it, so closeAllOwn never acts on a listing it does not have.
	ownListed bool
}

// errUnconfirmedNotFound means the Git host answered 404 for the pull request number, but the request's branch
// still lists a pull request with that number: the 404 was a passing fault, and the read is retried.
var errUnconfirmedNotFound = errors.New("the Git host answered 404 for the pull request, which its branch still lists")

// errConflictingReads means the read by number found the pull request closed, merged or missing while the
// listing of the open pull requests of the branch has it: one of them lags, and nothing is decided on either.
var errConflictingReads = errors.New("the pull request was read as not open, but the branch lists it open")

// errOwnListed means the pull request number is not this request's (a 404 its branch confirms, or an answer that
// rules it out) while a listing of the branch has a pull request with this request's marker: the branch still
// shows a change of this request, so the number is not taken for gone, and the read is retried. Its error is an
// *ownListedError, which names both numbers; condition PullRequestUnreadable shows it (reason
// RecordedMissingOtherListed). Only the finalizer acts on it (closeAllOwn).
var errOwnListed = errors.New("the Git host has no pull request with that number for this request, but the " +
	"branch lists a pull request of this request")

// ownListedError is errOwnListed for pull request n, with listed, the first pull request with this request's
// marker that a listing of the branch showed.
type ownListedError struct{ n, listed int64 }

func (e *ownListedError) Error() string {
	return fmt.Sprintf("%s: #%d (the branch lists #%d)", errOwnListed.Error(), e.n, e.listed)
}

func (e *ownListedError) Is(target error) bool { return target == errOwnListed }

// errGoneUnconfirmed means the Git host answered 404 for the pull request number and the listing of the branch
// has none with that number either, but not yet on two looks at least confirmWait apart (goneLooks): both
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
// to the target branch has none with that number and none with this request's marker, and only on a look at
// least confirmWait after the first that found the same (settleGone, goneLooks); before that, or when the
// listing has the number or a pull request of this request, or fails, the verdict is unknown.
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
//
// Every call settles this process's record of a first look at a missing number once, from what its reads showed
// (settleGone), and keeps condition PullRequestUnreadable in line with the read (noteRead): set while the host
// refuses it, removed by the next read that works.
func (r *RemediationRequestReconciler) changeOf(ctx context.Context, git providers.GitProvider,
	rr *remediationv1alpha1.RemediationRequest, q changeQuery) changeVerdict {
	v, n := r.readChange(ctx, git, rr, q)
	return r.noteRead(ctx, rr, n, v)
}

// readChange is changeOf without the condition; it also returns the number it read (0 for the listing).
func (r *RemediationRequestReconciler) readChange(ctx context.Context, git providers.GitProvider,
	rr *remediationv1alpha1.RemediationRequest, q changeQuery) (changeVerdict, int64) {
	src := rr.Status.Source
	if src == nil {
		return unknownChange(errors.New("no source recorded")), 0
	}
	n := q.number
	if n == 0 && rr.Status.PullRequest != nil {
		n = rr.Status.PullRequest.Number
	}
	if n == 0 {
		return r.settleGone(ctx, rr, 0, false, r.changeOnBranch(ctx, git, rr)), 0
	}
	return r.readByNumber(ctx, git, rr, q, n), n
}

// readByNumber is readChange for a number.
func (r *RemediationRequestReconciler) readByNumber(ctx context.Context, git providers.GitProvider,
	rr *remediationv1alpha1.RemediationRequest, q changeQuery, n int64) changeVerdict {
	v, absent := r.changeByNumber(ctx, git, rr, n, string(rr.Status.Source.TargetBranch))
	switch {
	case v.state == changeUnknown && errors.Is(v.err, errOwnListed) && q.openOnBranch:
		// The finalizer closes what the branch lists of this request (closeAllOwn): it needs the open ones too.
		v = r.checkOpenListing(ctx, git, rr, q, n, v, &absent)
	case v.state == changeUnknown:
	case absent:
		v = r.checkOpenListing(ctx, git, rr, q, n, v, &absent)
	default:
		if q.marker && v.state != changeGone {
			switch ours, err := r.ownsMarker(ctx, v.pr.Body, rr); {
			case err != nil:
				v = unknownChange(err)
			case !ours:
				v.state, v.gone = changeGone, fmt.Sprintf("pull request #%d lacks this request's marker", n)
			}
		}
		if v.state != changeUnknown {
			v = r.checkOpenListing(ctx, git, rr, q, n, v, &absent)
		}
	}
	return r.settleGone(ctx, rr, n, absent, v)
}

// checkOpenListing checks v, an answer about pull request n, against the listing of the open pull requests of the
// branch, when the caller asked for it (q.openOnBranch) or v is closed (one extra read, on closed only):
//   - a pull request read as closed, merged or missing (or as not this request's from another branch or base)
//     that the listing has open is unknown (conflicting reads), so a stale closed answer must not end, release or
//     cancel anything while the pull request is open. One read open but without this request's marker (gone for
//     that alone) does not conflict with the listing: both say it is open;
//   - for gone, another listed pull request with this request's marker is a read that shows a change of this
//     request, so n is not gone (errOwnListed): no read of the call answers gone while one shows such a change.
//
// Any outcome but gone from a missing number clears *absent, so settleGone records no first look on it.
// With q.openOnBranch the listing is v.open, also on errOwnListed from the read by number.
func (r *RemediationRequestReconciler) checkOpenListing(ctx context.Context, git providers.GitProvider,
	rr *remediationv1alpha1.RemediationRequest, q changeQuery, n int64, v changeVerdict, absent *bool) changeVerdict {
	if !q.openOnBranch && v.state != changeClosed {
		return v
	}
	open, err := git.ListChangeRequests(ctx, freshness.BranchName(rr), true)
	if err != nil {
		// A failed read shows nothing: no first look at a missing number is recorded on it.
		*absent = false
		return unknownChange(freshness.WrapProvider("list the open change requests of the branch", err))
	}
	listed := slices.ContainsFunc(open, func(cr providers.ChangeRequest) bool { return cr.Number == n })
	readOpen := v.pr.Number == n && v.pr.State == providers.ChangeRequestOpen
	switch {
	case v.state == changeUnknown:
	case listed && !readOpen:
		*absent = false
		return unknownChange(fmt.Errorf("%w: #%d", errConflictingReads, n))
	case v.state == changeGone:
		for _, o := range open {
			if o.Number == n {
				// Read open by its number without this request's marker: that read decides about n.
				continue
			}
			switch ours, err := r.ownsMarker(ctx, o.Body, rr); {
			case err != nil:
				*absent = false
				return unknownChange(err)
			case ours:
				*absent = false
				v = unknownChange(&ownListedError{n: n, listed: o.Number})
				v.open, v.ownListed = open, true
				return v
			}
		}
	}
	if q.openOnBranch {
		v.open = open
	}
	return v
}

// changeByNumber is changeOf's read by number, without the marker, the open listing and the first look. absent
// is true when the host has no pull request n and the listing of the branch has neither n nor a pull request of
// this request (settleGone).
func (r *RemediationRequestReconciler) changeByNumber(ctx context.Context, git providers.GitProvider,
	rr *remediationv1alpha1.RemediationRequest, n int64, target string) (changeVerdict, bool) {
	cr, err := git.GetChangeRequest(ctx, n)
	switch {
	case providers.IsNotFound(err):
		all, lerr := git.ListChangeRequests(ctx, freshness.BranchName(rr), false)
		if lerr != nil {
			// The 404 is left out of the chain: a caller must never read it as an answer.
			return unknownChange(fmt.Errorf("the Git host answered 404 for pull request #%d, and %w", n,
				freshness.WrapProvider("list the change requests of the branch", lerr))), false
		}
		var own []providers.ChangeRequest
		for _, o := range all {
			switch ours, err := r.ownsMarker(ctx, o.Body, rr); {
			case err != nil:
				return unknownChange(err), false
			case ours:
				own = append(own, o)
			}
		}
		switch {
		case slices.ContainsFunc(all, func(cr providers.ChangeRequest) bool { return cr.Number == n }):
			return unknownChange(fmt.Errorf("%w: #%d", errUnconfirmedNotFound, n)), false
		case len(own) > 0:
			v := unknownChange(&ownListedError{n: n, listed: own[0].Number})
			v.own, v.ownListed = own, true
			return v, false
		}
		return changeVerdict{state: changeGone, gone: fmt.Sprintf(
			"the Git host has no pull request #%d of Shadewright to the target branch", n)}, true
	case providers.IsUntrustedAnswer(err):
		return unknownChange(fmt.Errorf("the Git host answered with another pull request than #%d; it is read "+
			"again: %w", n, freshness.WrapProvider("find the change request", err))), false
	case err != nil:
		return unknownChange(freshness.WrapProvider("find the change request", err)), false
	case cr.Number != n:
		return unknownChange(fmt.Errorf("the Git host answered with another pull request than #%d; it is read "+
			"again: %w", n, providers.ErrUntrustedAnswer)), false
	case cr.HeadBranch != freshness.BranchName(rr):
		return changeVerdict{state: changeGone, gone: fmt.Sprintf(
			"pull request #%d is from another branch than the request's", n)}, false
	case cr.BaseBranch != target:
		return changeVerdict{state: changeGone, gone: fmt.Sprintf(
			"pull request #%d targets another branch than the request's target branch", n)}, false
	}
	state, ok := stateOf(cr)
	if !ok {
		return unknownChange(fmt.Errorf("pull request #%d has a state the executor does not know: %q", n, cr.State)), false
	}
	return changeVerdict{state: state, pr: cr}, false
}

// settleGone is the one place that decides the confirmation wait of a pull request missing by its number (#688),
// on the same terms as a merge conflict (conflicted). It runs once per changeOf call, after its reads, with n, the
// number read (0 for the listing of the branch), and absent, true only when this call's reads found n missing: a
// 404 that neither the listing of every pull request of the branch nor the open listing contradicts, with no
// pull request of this request listed. The first look is evidence about the Git host as this process saw it, so
// it lives in this process's memory (goneLooks), never in status:
//   - a look that finds n missing with no first look kept for this request and n (or one for another number, of
//     an earlier request with the same name, older than goneLookMaxAge, or ahead of the clock) keeps one with the
//     time on this process's clock and answers unknown (errGoneUnconfirmed, with the time left as the delay to
//     ask for); one that finds it missing at least confirmWait after the first look answers gone;
//   - any other outcome (a read that showed n or a pull request of this request, conflicting reads, a failed read,
//     any verdict but gone) drops the first look, so the next 404 waits again.
//
// So gone is never answered by a call in which a read showed a pull request of this request, and never on a time
// from another clock or from stored state. After a restart or a change of leader the wait starts again.
func (r *RemediationRequestReconciler) settleGone(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	n int64, absent bool, v changeVerdict) changeVerdict {
	if !absent || v.state != changeGone {
		r.goneLooks.forget(client.ObjectKeyFromObject(rr))
		return v
	}
	now := r.now()
	if first, ok := r.goneLooks.first(rr, n); ok {
		switch age := now.Sub(first); {
		case age >= confirmWait && age < goneLookMaxAge:
			r.goneLooks.forget(client.ObjectKeyFromObject(rr))
			return v
		case age >= 0 && age < confirmWait:
			return unknownChange(goneWait(n, confirmWait-age))
		}
	}
	r.goneLooks.note(rr, n, now)
	logf.FromContext(ctx).Info("The Git host has no pull request with the number of RemediationRequest; waiting to "+
		"see it missing again", "remediationRequest", client.ObjectKeyFromObject(rr), "pullRequest", n,
		"wait", confirmWait)
	return unknownChange(goneWait(n, confirmWait))
}

// goneLookMaxAge bounds how long a first look at a missing pull request is kept: one older than this is no
// evidence about the host now, and the wait starts again from the next look.
const goneLookMaxAge = 30 * time.Minute

// goneLooks keeps, per request, the first look of this process that found a pull request number missing
// (settleGone): the number and the time on this process's clock, bound to the request's UID. It is memory only,
// like mergeReadings: after a restart or a change of leader every wait starts again. An entry is dropped by any
// other outcome of a read of the request's pull requests, when the request ends (setPhase) and when it is gone.
type goneLooks struct {
	m sync.Map
	// via, when set, is the memory used instead of m: tests that model one process over several reconciler values
	// share one. It is never set outside tests.
	via *goneLooks
}

// goneLook is one entry of goneLooks.
type goneLook struct {
	uid    types.UID
	number int64
	at     time.Time
}

func (g *goneLooks) store() *sync.Map {
	if g.via != nil {
		return &g.via.m
	}
	return &g.m
}

// note keeps the first look at n missing for rr, made at at, in place of any other.
func (g *goneLooks) note(rr *remediationv1alpha1.RemediationRequest, n int64, at time.Time) {
	g.store().Store(client.ObjectKeyFromObject(rr), goneLook{uid: rr.UID, number: n, at: at})
}

// first returns the time of the first look kept for rr and n. An entry of an earlier request with the same name
// (another UID) or for another number is not returned.
func (g *goneLooks) first(rr *remediationv1alpha1.RemediationRequest, n int64) (time.Time, bool) {
	v, ok := g.store().Load(client.ObjectKeyFromObject(rr))
	if !ok {
		return time.Time{}, false
	}
	kept := v.(goneLook)
	if kept.uid != rr.UID || kept.number != n {
		return time.Time{}, false
	}
	return kept.at, true
}

// forget drops the first look kept for the request with key.
func (g *goneLooks) forget(key types.NamespacedName) {
	g.store().Delete(key)
}

// goneWait is the error of a pull request missing on a look that does not confirm it yet: errGoneUnconfirmed,
// with the time left as the delay a caller that reads Retry-After waits (waitOrFail), at least stepDelay.
func goneWait(n int64, left time.Duration) error {
	return errors.Join(fmt.Errorf("%w: #%d", errGoneUnconfirmed, n), &freshness.ProviderError{
		Op: "confirm that the pull request is gone", RetryAfter: max(left, stepDelay)})
}

// restoreStatus puts back before, the status of rr a caller saved at resourceVersion rv, when it gives up a step
// and returns its error for a retry. What changeOf wrote since then (condition PullRequestUnreadable) is kept: a
// write since the save moved the resourceVersion, and rr holds what was written, so a later status write built
// from rr never undoes it.
func restoreStatus(rr *remediationv1alpha1.RemediationRequest, before *remediationv1alpha1.RemediationRequestStatus,
	rv string) {
	if rr.ResourceVersion != rv {
		putUnreadable(&before.Conditions,
			meta.FindStatusCondition(rr.Status.Conditions, remediationv1alpha1.ConditionPullRequestUnreadable))
	}
	rr.Status = *before
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

const (
	// EventPullRequestUnreadable is the Warning Event recorded when condition PullRequestUnreadable is set, or
	// its reason changes.
	EventPullRequestUnreadable = "PullRequestUnreadable"
	// EventPullRequestReadable is the Normal Event recorded when condition PullRequestUnreadable is removed.
	EventPullRequestReadable = "PullRequestReadable"
)

// noteRead keeps condition PullRequestUnreadable in line with the read behind v (#688), so a person sees a read
// the Git host refuses on every path, also while it is retried in a phase without a deadline or on a stopped
// request that keeps its lock:
//   - a read refused for good (refusal: 403 or 422) sets it, with the refusal's reason and a fixed message that
//     names the read, never the host's answer;
//   - a recorded number the host has no pull request of this request for, while a listing of the branch shows
//     another one with this request's marker (errOwnListed), sets it with reason RecordedMissingOtherListed and a
//     message that names both numbers and what a person can do: that request is retried until a person acts;
//   - a read that worked (any verdict but unknown, or unknown from answers the host gave: a 404 the branch does
//     not confirm, contradicting reads, a first look that finds the number missing, an answer about another pull
//     request) removes it;
//   - any other failure (a server error, a timeout) leaves it, except a reason about the repository of the plan
//     (repositoryReasons), which the repository just opened disproves.
//
// Retrying is the caller's as before; noteRead changes no verdict except that a write of the condition that fails
// answers unknown, so nothing is decided on a status that may not be there.
func (r *RemediationRequestReconciler) noteRead(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	n int64, v changeVerdict) changeVerdict {
	cur := meta.FindStatusCondition(rr.Status.Conditions, remediationv1alpha1.ConditionPullRequestUnreadable)
	var want *metav1.Condition
	ownListed, isOwnListed := errors.AsType[*ownListedError](v.err)
	switch reason, _, refused := refusal(v.err); {
	case v.state == changeUnknown && refused:
		want = refusedRead(rr, reason, v.err, n)
	case v.state == changeUnknown && isOwnListed:
		want = ownListedRead(rr, ownListed)
	case v.state != changeUnknown, errors.Is(v.err, errUnconfirmedNotFound), errors.Is(v.err, errConflictingReads),
		errors.Is(v.err, errGoneUnconfirmed), providers.IsUntrustedAnswer(v.err):
	case cur != nil && repositoryReasons[cur.Reason]:
	default:
		return v
	}
	if err := r.setUnreadable(ctx, rr, want); err != nil {
		if v.state == changeUnknown {
			return unknownChange(errors.Join(v.err, err))
		}
		return unknownChange(err)
	}
	return v
}

// refusedRead is condition PullRequestUnreadable for a read of the request's pull requests that the Git host
// refused (reason GitForbidden or GitRejected). It names the read by its fixed operation text and the number.
func refusedRead(rr *remediationv1alpha1.RemediationRequest, reason string, err error, n int64) *metav1.Condition {
	what := "read the pull requests of the request"
	if pe, ok := errors.AsType[*freshness.ProviderError](err); ok {
		what = pe.Op
	}
	if n > 0 {
		what = fmt.Sprintf("%s (pull request #%d)", what, n)
	}
	return &metav1.Condition{
		Type:   remediationv1alpha1.ConditionPullRequestUnreadable,
		Status: metav1.ConditionTrue,
		Reason: reason,
		Message: "The Git host refused (" + reason + ") to " + what + "; it is read again, and nothing is " +
			"decided on it until a read works",
		ObservedGeneration: rr.Generation,
	}
}

// ownListedRead is condition PullRequestUnreadable for errOwnListed: the recorded pull request is missing (or
// not this request's) and the branch lists another one with this request's marker. The message names both numbers
// and says what ends it; it holds no text of the Git host.
func ownListedRead(rr *remediationv1alpha1.RemediationRequest, e *ownListedError) *metav1.Condition {
	return &metav1.Condition{
		Type:   remediationv1alpha1.ConditionPullRequestUnreadable,
		Status: metav1.ConditionTrue,
		Reason: ReasonRecordedMissingOtherListed,
		Message: fmt.Sprintf("The Git host has no pull request #%d of this request, the one it recorded, but the "+
			"request's branch lists pull request #%d with this request's marker, so the request cannot tell whether "+
			"its change is gone; it is read again, and nothing is decided on it. Delete the request, which closes "+
			"the open pull requests of the branch with its marker, or, when #%d is not this request's change, "+
			"remove this request's marker from its description; closing or merging #%d alone does not end this",
			e.n, e.listed, e.listed, e.listed),
		ObservedGeneration: rr.Generation,
	}
}

// repositoryReasons are the reasons of condition PullRequestUnreadable that say why the repository of the plan
// cannot be opened (repositoryRead); a read of the repository disproves each of them.
var repositoryReasons = map[string]bool{
	reasonRepositoryUnavailable: true, reasonTargetBranchChanged: true, string(sourcemap.ReasonRepositoryUnusable): true,
}

// repositoryGoneRead is condition PullRequestUnreadable for a request whose plan's repository is not in
// RemediationSettings any more, so none of its pull requests can be read.
func repositoryGoneRead(rr *remediationv1alpha1.RemediationRequest) *metav1.Condition {
	return &metav1.Condition{
		Type:   remediationv1alpha1.ConditionPullRequestUnreadable,
		Status: metav1.ConditionTrue,
		Reason: reasonRepositoryUnavailable,
		Message: "The repository of the plan is not in RemediationSettings any more, so the request's pull " +
			"requests cannot be read; the target stays locked until it is configured again or the request is deleted",
		ObservedGeneration: rr.Generation,
	}
}

// repositoryRead is condition PullRequestUnreadable for err, the failure to open the repository of the plan, when
// it says that the request's pull requests cannot be read there as the settings stand now: the repository left
// the settings (RepositoryUnavailable), its entry names another target branch (TargetBranchChanged), or its entry
// cannot be used (RepositoryUnusable). Any other failure gives nil.
func repositoryRead(rr *remediationv1alpha1.RemediationRequest, err error) *metav1.Condition {
	var reason, message string
	switch {
	case errors.Is(err, errRepositoryGone):
		return repositoryGoneRead(rr)
	case errors.Is(err, errTargetBranchChanged):
		reason, message = reasonTargetBranchChanged, "The repository of the plan names another target branch in "+
			"RemediationSettings now, so the request's pull requests, which target the branch the plan was made "+
			"for, are not read; the target stays locked until that branch is configured again or the request is "+
			"deleted"
	case errors.Is(err, freshness.ErrRepositoryUnusable):
		reason, message = string(sourcemap.ReasonRepositoryUnusable), "The entry of the repository of the plan in "+
			"RemediationSettings cannot be used, so the request's pull requests cannot be read; the target stays "+
			"locked until the entry is fixed or the request is deleted"
	default:
		return nil
	}
	return &metav1.Condition{
		Type: remediationv1alpha1.ConditionPullRequestUnreadable, Status: metav1.ConditionTrue,
		Reason: reason, Message: message, ObservedGeneration: rr.Generation,
	}
}

// putUnreadable sets condition PullRequestUnreadable to want in conds (nil removes it) and reports whether that
// changed it.
func putUnreadable(conds *[]metav1.Condition, want *metav1.Condition) bool {
	if want == nil {
		return meta.RemoveStatusCondition(conds, remediationv1alpha1.ConditionPullRequestUnreadable)
	}
	return meta.SetStatusCondition(conds, *want)
}

// unreadableEvent records the Event of a change of condition PullRequestUnreadable from was to want: a Warning when
// it is set or its reason changes, a Normal one when it is removed, none otherwise (one per transition, never per
// retry). Without a Recorder it records none.
func (r *RemediationRequestReconciler) unreadableEvent(rr *remediationv1alpha1.RemediationRequest,
	was, want *metav1.Condition) {
	if r.Recorder == nil {
		return
	}
	switch {
	case want != nil && (was == nil || was.Reason != want.Reason):
		r.Recorder.Event(rr, corev1.EventTypeWarning, EventPullRequestUnreadable, want.Message)
	case want == nil && was != nil:
		r.Recorder.Event(rr, corev1.EventTypeNormal, EventPullRequestReadable,
			"The pull requests of the request can be read again")
	}
}

// setUnreadable writes condition PullRequestUnreadable as want (nil removes it), alone, when it changes. The
// patch is made against the request as the API server has it, at the resourceVersion of rr, so it never carries
// a caller's unsaved status; a request changed since rr was read is a conflict, returned for a retry. rr gets the
// condition and the new resourceVersion.
func (r *RemediationRequestReconciler) setUnreadable(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	want *metav1.Condition) error {
	was := meta.FindStatusCondition(rr.Status.Conditions, remediationv1alpha1.ConditionPullRequestUnreadable)
	if was != nil {
		was = was.DeepCopy()
	}
	if probe := slices.Clone(rr.Status.Conditions); !putUnreadable(&probe, want) {
		return nil
	}
	live := &remediationv1alpha1.RemediationRequest{}
	if err := r.reader().Get(ctx, client.ObjectKeyFromObject(rr), live); err != nil {
		return fmt.Errorf("read RemediationRequest %s/%s to record whether its pull requests can be read: %w",
			rr.Namespace, rr.Name, err)
	}
	if live.UID != rr.UID || live.ResourceVersion != rr.ResourceVersion {
		return fmt.Errorf("record whether the pull requests of RemediationRequest %s/%s can be read: %w",
			rr.Namespace, rr.Name, apierrors.NewConflict(remediationv1alpha1.GroupVersion.WithResource(
				"remediationrequests").GroupResource(), rr.Name, errors.New("it changed since it was read")))
	}
	base := live.DeepCopy()
	putUnreadable(&live.Status.Conditions, want)
	if err := r.Status().Patch(ctx, live, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("record whether the pull requests of RemediationRequest %s/%s can be read: %w",
			rr.Namespace, rr.Name, err)
	}
	rr.ResourceVersion = live.ResourceVersion
	if c := meta.FindStatusCondition(live.Status.Conditions, remediationv1alpha1.ConditionPullRequestUnreadable); c != nil {
		meta.SetStatusCondition(&rr.Status.Conditions, *c)
	} else {
		meta.RemoveStatusCondition(&rr.Status.Conditions, remediationv1alpha1.ConditionPullRequestUnreadable)
	}
	r.unreadableEvent(rr, was, want)
	return nil
}
