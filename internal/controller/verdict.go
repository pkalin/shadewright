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

import "github.com/pkalin/shadewright/internal/controller/lockverdict"

// verdictTable is the one place that decides what each caller does with the verdict of a read of a request's pull
// request (changeOf, #925): one row per changeState, which is a state of the pull request (open, merged, closed,
// gone, or a failed read) together with its base relation (the target branch the read was made for, or another
// base a person moved it to), and one column per decision of a caller (verdictRow). No code of this package compares
// a changeState outside this table and the readers that build a verdict (TestVerdictsDecidedOnlyInTheTable): a
// caller asks v.rule() and acts on its column. A verdict added to the const block without a row, or a column added
// without a value in every row, fails TestVerdictTableDecidesEveryVerdict, which reads both from the source.
//
// The zero value of every column is the one that keeps the lock, writes nothing and retries, so a state with no row
// (decideVerdict) is handled as a failed read.
var verdictTable = map[changeState]verdictRow{
	changeUnknown: {
		name: "unknown", failed: true, stopException: stopExceptNone, heldRecord: heldRecordNone,
		lockFound: lockverdict.FoundOther, keepsLockMoved: false, lookConfirmGone: false, lookYields: true,
		lookWins: false, ownOpenOnTarget: false, adoptLooksElsewhere: false, lockLooksElsewhere: false, fromOtherBases: otherBaseFailed,
		yieldsToClosed: false, handed: handedRetry, recover: recoverRetry, mergedInto: false,
		retargetCancel: retargetCancelNone, failCheck: failCheckRetry, stoppedCheck: false, cancelLook: cancelLookNone, adopt: adoptRetry,
		refuseOpened: refuseClose, mergedUnrecorded: mergedUnrecordedRetry, watch: watchRetry,
		closeRecorded: closeRecordedRetry, closeUnrecorded: closeUnrecordedRetry, stopOnOtherBase: stopOnOtherRetry,
		finalize: finalizeRetry,
	},
	changeOpen: {
		name: "open", failed: false, stopException: stopExceptOpen, heldRecord: heldRecordFound,
		lockFound: lockverdict.FoundOther, keepsLockMoved: false, lookConfirmGone: false, lookYields: false,
		lookWins: false, ownOpenOnTarget: true, adoptLooksElsewhere: false, lockLooksElsewhere: false, fromOtherBases: otherBaseNone,
		yieldsToClosed: false, handed: handedIt, recover: recoverRetry, mergedInto: false,
		retargetCancel: retargetCancelNone, failCheck: failCheckWrite, stoppedCheck: true, cancelLook: cancelLookNone, adopt: adoptIt,
		refuseOpened: refuseClose, mergedUnrecorded: mergedUnrecordedNothing, watch: watchGoOn,
		closeRecorded: closeRecordedClose, closeUnrecorded: closeUnrecordedGoOn, stopOnOtherBase: stopOnOtherNothing,
		finalize: finalizeClose,
	},
	changeMerged: {
		name: "merged", failed: false, stopException: stopExceptMerged, heldRecord: heldRecordFound,
		lockFound: lockverdict.FoundMerged, keepsLockMoved: false, lookConfirmGone: false, lookYields: false,
		lookWins: true, ownOpenOnTarget: false, adoptLooksElsewhere: false, lockLooksElsewhere: true, fromOtherBases: otherBaseNone,
		yieldsToClosed: false, handed: handedIt, recover: recoverFromPullRequest, mergedInto: true,
		retargetCancel: retargetCancelNone, failCheck: failCheckWrite, stoppedCheck: false, cancelLook: cancelLookMerged, adopt: adoptIt,
		refuseOpened: refuseEnded, mergedUnrecorded: mergedUnrecordedMerged, watch: watchGoOn,
		closeRecorded: closeRecordedClose, closeUnrecorded: closeUnrecordedGoOn, stopOnOtherBase: stopOnOtherNothing,
		finalize: finalizeNothing,
	},
	changeClosed: {
		name: "closed", failed: false, stopException: stopExceptNone, heldRecord: heldRecordFound,
		lockFound: lockverdict.FoundClosed, keepsLockMoved: false, lookConfirmGone: false, lookYields: false,
		lookWins: false, ownOpenOnTarget: false, adoptLooksElsewhere: true, lockLooksElsewhere: true, fromOtherBases: otherBaseClosed,
		yieldsToClosed: false, handed: handedIt, recover: recoverFromPullRequest, mergedInto: false,
		retargetCancel: retargetCancelClosed, failCheck: failCheckWrite, stoppedCheck: false, cancelLook: cancelLookEnd, adopt: adoptIt,
		refuseOpened: refuseEnded, mergedUnrecorded: mergedUnrecordedNothing, watch: watchGoOn,
		closeRecorded: closeRecordedClose, closeUnrecorded: closeUnrecordedLookElsewhere,
		stopOnOtherBase: stopOnOtherNothing, finalize: finalizeNothing,
	},
	changeGone: {
		name: "gone", failed: false, stopException: stopExceptNone, heldRecord: heldRecordNone,
		lockFound: lockverdict.FoundGone, keepsLockMoved: false, lookConfirmGone: true, lookYields: true,
		lookWins: false, ownOpenOnTarget: false, adoptLooksElsewhere: true, lockLooksElsewhere: true, fromOtherBases: otherBaseNone,
		yieldsToClosed: true, handed: handedGone, recover: recoverNothing, mergedInto: false,
		retargetCancel: retargetCancelClosed, failCheck: failCheckNotOursReinstalled, stoppedCheck: false, cancelLook: cancelLookEnd,
		adopt: adoptNone, refuseOpened: refuseClose, mergedUnrecorded: mergedUnrecordedNothing, watch: watchGone,
		closeRecorded: closeRecordedNotOurs, closeUnrecorded: closeUnrecordedLookElsewhere,
		stopOnOtherBase: stopOnOtherNothing, finalize: finalizeNothing,
	},
	changeElsewhere: {
		name: "open on another base", failed: false, stopException: stopExceptNone, heldRecord: heldRecordMoved,
		lockFound: lockverdict.FoundOther, keepsLockMoved: true, lookConfirmGone: false, lookYields: false,
		lookWins: false, ownOpenOnTarget: false, adoptLooksElsewhere: false, lockLooksElsewhere: false, fromOtherBases: otherBaseOpen,
		yieldsToClosed: false, handed: handedMoved, recover: recoverRetry, mergedInto: false,
		retargetCancel: retargetCancelMoved, failCheck: failCheckWrite, stoppedCheck: true, cancelLook: cancelLookMoved,
		adopt: adoptElsewhere, refuseOpened: refuseMoved, mergedUnrecorded: mergedUnrecordedNothing, watch: watchMoved,
		closeRecorded: closeRecordedMoved, closeUnrecorded: closeUnrecordedGoOn, stopOnOtherBase: stopOnOtherMoved,
		finalize: finalizeCloseAnyBase,
	},
	changeMergedElsewhere: {
		name: "merged into another base", failed: false, stopException: stopExceptNone, heldRecord: heldRecordNone,
		lockFound: lockverdict.FoundGone, keepsLockMoved: false, lookConfirmGone: false, lookYields: false,
		lookWins: false, ownOpenOnTarget: false, adoptLooksElsewhere: false, lockLooksElsewhere: true, fromOtherBases: otherBaseMerged,
		yieldsToClosed: false, handed: handedGone, recover: recoverNothing, mergedInto: false,
		retargetCancel: retargetCancelMergedElsewhere, failCheck: failCheckNotOurs, stoppedCheck: false,
		cancelLook: cancelLookMergedElsewhere, adopt: adoptMergedElsewhere, refuseOpened: refuseMergedElsewhere,
		mergedUnrecorded: mergedUnrecordedNothing, watch: watchMergedElsewhere, closeRecorded: closeRecordedNotOurs,
		closeUnrecorded: closeUnrecordedGoOn, stopOnOtherBase: stopOnOtherNothing, finalize: finalizeNothing,
	},
	changeClosedElsewhere: {
		name: "closed on another base", failed: false, stopException: stopExceptNone, heldRecord: heldRecordFound,
		lockFound: lockverdict.FoundClosed, keepsLockMoved: false, lookConfirmGone: false, lookYields: false,
		lookWins: false, ownOpenOnTarget: false, adoptLooksElsewhere: true, lockLooksElsewhere: true, fromOtherBases: otherBaseClosed,
		yieldsToClosed: false, handed: handedIt, recover: recoverFromPullRequest, mergedInto: false,
		retargetCancel: retargetCancelClosed, failCheck: failCheckWrite, stoppedCheck: false, cancelLook: cancelLookEnd, adopt: adoptIt,
		refuseOpened: refuseEnded, mergedUnrecorded: mergedUnrecordedNothing, watch: watchGoOn,
		closeRecorded: closeRecordedNothing, closeUnrecorded: closeUnrecordedLookElsewhere,
		stopOnOtherBase: stopOnOtherNothing, finalize: finalizeNothing,
	},
}

// verdictRow is what every caller does with one verdict: a column per decision. Every row names every column
// (TestVerdictTableDecidesEveryVerdict), so a new decision is made for each verdict on purpose.
type verdictRow struct {
	// name is the verdict in logs and messages (changeState.String).
	name string
	// failed: the read failed, conflicted with another read, or answered about another pull request. It ends
	// nothing, releases nothing and removes no finalizer: every caller retries with backoff.
	failed bool

	// stopException is what settleStopException (and settleCancelled after it) does for a stopped holder: its own
	// pull request open, or merged, on a target branch (the recorded one or the one the settings name now).
	stopException stopException
	// heldRecord is what checkHeld records of the pull request a look found (recordFound): nothing, the pull request
	// (found closed or open on a target branch), or the pull request open on a base a person moved it to.
	heldRecord heldRecord
	// lockFound is what a stopped holder's read proves for the release of the lock (settleByRead).
	lockFound lockverdict.Found
	// keepsLockMoved: the request's own pull request is open on a base a person moved it to (#892). It can still
	// merge there, so it keeps the lock (lookHeld puts it among the open ones, settleByRead gives no verdict) and
	// TargetHeld names it (movedMessage).
	keepsLockMoved bool
	// lookConfirmGone: after a change of the target branch, the configured branch did not show the pull request, so
	// the read on the recorded branch confirms a missing one as before #771 (lookHeld, q.recordedBranch).
	lookConfirmGone bool
	// lookYields: a verdict of one of lookHeld's reads that the read of the other target branch replaces.
	lookYields bool
	// lookWins: a verdict that replaces the one of the other target branch's read (the request's own merge).
	lookWins bool
	// ownOpenOnTarget: the verdict's pull request is the request's own, open on a target branch: it is not another
	// pull request that holds the lock (lookHeld), and the finalizer closed it already (closeAllOwn).
	ownOpenOnTarget bool
	// lockLooksElsewhere: a verdict on which a stopped holder's lock may be released, so the release waits for the
	// listing of every base, and every open pull request from the request's branch there, marked or not, keeps the
	// lock (openElsewhere, #925): "gone" and "closed" are concluded only after every base was looked at.
	lockLooksElsewhere bool
	// adoptLooksElsewhere: with no pull request recorded, the target branches show none of the request's own open or
	// merged, so the request's own is taken from the listing of every base (openElsewhere): by the adoption, and by
	// the lock, which then records it.
	adoptLooksElsewhere bool
	// fromOtherBases is what openElsewhere takes from the listing of every base.
	fromOtherBases otherBase
	// yieldsToClosed: a verdict that one closed on another base replaces (openElsewhere): a closed one is evidence of
	// the request's change, gone is none.
	yieldsToClosed bool

	// handed is what findHanded answers about a predecessor's pull request at a handoff.
	handed handed
	// recover is recoverStep's rule after a 404 for the branch.
	recover recoverOutcome

	// mergedInto: merged into the target branch the read was made for (watchBranchChanged, retargetedMergeCounts).
	mergedInto bool
	// retargetCancel is what a cancelled request whose target branch changed does with the read on the recorded
	// branch (watchBranchChanged).
	retargetCancel retargetCancel

	// failCheck is what failCheckStep does before it writes a failed review check. The check run belongs to the head
	// commit, so it is written also for a pull request open on a base a person moved it to (#925): moved back, it
	// cannot merge on a check written before the request stopped.
	failCheck failCheck
	// stoppedCheck: the request's own pull request is open, on the target branch or on a base a person moved it to,
	// so a stopped request fails its review check (failStoppedCheck, #925).
	stoppedCheck bool
	// cancelLook is what the read-only look of a cancel after a branch change does (cancelLook).
	cancelLook cancelLook

	// adopt is pullRequestStep's rule on the pull requests of the branch.
	adopt adoptOutcome
	// refuseOpened is what refuseOpened does before it closes a pull request whose head failed a check.
	refuseOpened refuseOpened
	// mergedUnrecorded is what mergedUnrecorded does after a close met a conflict.
	mergedUnrecorded mergedUnrecorded
	// watch is what the watch of a recorded pull request does (hint look).
	watch watch

	// closeRecorded is what closeFound does with the recorded pull request of a request that is not deleted.
	closeRecorded closeRecorded
	// closeUnrecorded is what closeUnrecorded does with the listing of the branch.
	closeUnrecorded closeUnrecorded
	// stopOnOtherBase is what a stop of a request with no pull request recorded does with the listing of every base
	// (closeUnrecordedElsewhere).
	stopOnOtherBase stopOnOtherBase
	// finalize is closeAllOwn's rule for the recorded pull request.
	finalize finalizeOutcome
}

// decideVerdict is the row of verdict s; a state with no row is handled as a failed read.
func decideVerdict(s changeState) verdictRow {
	if row, ok := verdictTable[s]; ok {
		return row
	}
	return verdictTable[changeUnknown]
}

// rule is the row of v's verdict (decideVerdict).
func (v changeVerdict) rule() verdictRow { return decideVerdict(v.state) }

// failed reports whether v is a failed read (changeUnknown): see verdictRow.failed.
func (v changeVerdict) failed() bool { return v.rule().failed }

func (s changeState) String() string { return decideVerdict(s).name }

// stopException is verdictRow.stopException.
type stopException int

const (
	stopExceptNone stopException = iota
	stopExceptOpen
	stopExceptMerged
)

// heldRecord is verdictRow.heldRecord.
type heldRecord int

const (
	heldRecordNone heldRecord = iota
	heldRecordFound
	heldRecordMoved
)

// otherBase is verdictRow.fromOtherBases.
type otherBase int

const (
	// otherBaseFailed keeps the lock and opens nothing: the listing failed.
	otherBaseFailed otherBase = iota
	otherBaseNone
	// otherBaseOpen: one with the marker is open there; it joins the open ones and becomes the verdict.
	otherBaseOpen
	// otherBaseMerged: one with the marker was merged there; it becomes the verdict.
	otherBaseMerged
	// otherBaseClosed: one with the marker was closed there; it replaces a verdict that yieldsToClosed.
	otherBaseClosed
)

// handed is verdictRow.handed.
type handed int

const (
	handedRetry handed = iota
	handedGone
	handedMoved
	handedIt
)

// retargetCancel is verdictRow.retargetCancel.
type retargetCancel int

const (
	retargetCancelNone retargetCancel = iota
	retargetCancelMoved
	retargetCancelMergedElsewhere
	retargetCancelClosed
)

// failCheck is verdictRow.failCheck.
type failCheck int

const (
	failCheckRetry failCheck = iota
	failCheckNotOurs
	failCheckNotOursReinstalled
	failCheckWrite
)

// cancelLook is verdictRow.cancelLook.
type cancelLook int

const (
	cancelLookNone cancelLook = iota
	cancelLookMerged
	cancelLookEnd
	cancelLookMergedElsewhere
	cancelLookMoved
)

// refuseOpened is verdictRow.refuseOpened.
type refuseOpened int

const (
	refuseClose refuseOpened = iota
	refuseEnded
	refuseMoved
	refuseMergedElsewhere
)

// mergedUnrecorded is verdictRow.mergedUnrecorded.
type mergedUnrecorded int

const (
	mergedUnrecordedRetry mergedUnrecorded = iota
	mergedUnrecordedMerged
	mergedUnrecordedNothing
)

// watch is verdictRow.watch.
type watch int

const (
	watchRetry watch = iota
	watchGone
	watchMoved
	watchMergedElsewhere
	watchGoOn
)

// closeRecorded is verdictRow.closeRecorded.
type closeRecorded int

const (
	closeRecordedRetry closeRecorded = iota
	closeRecordedNotOurs
	closeRecordedMoved
	closeRecordedNothing
	closeRecordedClose
)

// closeUnrecorded is verdictRow.closeUnrecorded.
type closeUnrecorded int

const (
	closeUnrecordedRetry closeUnrecorded = iota
	closeUnrecordedLookElsewhere
	closeUnrecordedGoOn
)

// stopOnOtherBase is verdictRow.stopOnOtherBase.
type stopOnOtherBase int

const (
	stopOnOtherRetry stopOnOtherBase = iota
	stopOnOtherMoved
	stopOnOtherNothing
)
