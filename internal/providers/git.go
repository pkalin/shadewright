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

package providers

import "context"

// GitCapabilities says what a Git hosting adapter supports, so the core adapts instead of assuming one product.
type GitCapabilities struct {
	// AutoMerge is true when the host can merge a change request by itself once its rules pass.
	AutoMerge bool
	// RequiredReviews is true when the host can enforce required reviews on the target branch.
	RequiredReviews bool
}

// File is the content of one file at one commit.
type File struct {
	Path   string
	Commit string
	Data   []byte
}

// FileChange replaces the content of one existing file in one commit.
type FileChange struct {
	Path    string
	Content []byte
	Message string
}

// ChangeRequestState is the lifecycle state of a change request.
type ChangeRequestState string

// Change request states.
const (
	ChangeRequestOpen   ChangeRequestState = "Open"
	ChangeRequestClosed ChangeRequestState = "Closed"
	ChangeRequestMerged ChangeRequestState = "Merged"
)

// NewChangeRequest is what is needed to open a change request (a pull request or merge request).
type NewChangeRequest struct {
	// HeadBranch is the branch with the change, for example shadewright/<request-uid>.
	HeadBranch string
	// BaseBranch is the branch the change targets.
	BaseBranch string
	Title      string
	// Body describes the change; it carries the marker that ties it to a RemediationRequest.
	Body string
}

// ChangeRequest is a change request as the host reports it.
type ChangeRequest struct {
	Number     int64
	URL        string
	HeadBranch string
	BaseBranch string
	// HeadSHA is the commit at the head of the change request.
	HeadSHA string
	State   ChangeRequestState
	Title   string
	Body    string
	// MergeCommit is set when State is Merged.
	MergeCommit string
}

// MaxFileBytes is the largest file GitProvider.ReadFile returns. Shadewright edits small manifests; a
// larger file is refused with ErrTooLarge before its content is downloaded.
const MaxFileBytes = 1 << 20

// ReadLimit returns the byte limit a GitProvider.ReadFile call applies for the limit it was given: limit
// itself when it is between 0 and MaxFileBytes, 0 for a negative one and MaxFileBytes for a larger one. The
// clamp only ever narrows a read, so a mistaken limit can refuse a file but never let a larger one through.
func ReadLimit(limit int) int {
	return min(max(limit, 0), MaxFileBytes)
}

// MaxDirEntries is the most entries GitProvider.ListDir accepts in one directory, counted before symbolic
// links and submodules are skipped. A larger directory is refused with ErrTooLarge, so one listing stays small
// whatever the repository holds.
const MaxDirEntries = 1000

// DirEntryType is the kind of a directory entry GitProvider.ListDir returns.
type DirEntryType string

// Directory entry types. Symbolic links and submodules are never listed.
const (
	// DirEntryFile is a regular file, executable or not.
	DirEntryFile DirEntryType = "File"
	// DirEntryDir is a directory (a Git tree).
	DirEntryDir DirEntryType = "Dir"
)

// DirEntry is one entry of a directory at one commit: its name, one path segment that never holds a slash,
// and its type.
type DirEntry struct {
	Name string
	Type DirEntryType
}

// Lengths of a full Git object ID in lowercase hexadecimal.
const (
	SHA1HexLen   = 40
	SHA256HexLen = 64
)

// IsObjectID reports whether s is a full Git object ID: SHA1HexLen or SHA256HexLen lowercase hexadecimal
// characters. Content addressed by one never changes; a branch, a tag or an abbreviated ID can name other
// content later.
func IsObjectID(s string) bool {
	if len(s) != SHA1HexLen && len(s) != SHA256HexLen {
		return false
	}
	for i := range len(s) {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// MaxChainCommits is the most commits of its own a Shadewright branch carries over the target branch: one of
// the first request of a supersede chain and one per hint the chain consumed (ADR-0028 sections 4 and 6, at
// most three). GitProvider.CheckChain accepts no longer chain.
const MaxChainCommits = 4

// CheckRunName is the name of the check run the executor creates on the heads of its pull requests (ADR-0028
// section 5). The ruleset of the GitOps repository requires it with the Shadewright App as its source.
const CheckRunName = "shadewright/review"

// CheckRunStatus is the status of a check run.
type CheckRunStatus string

// Check run statuses.
const (
	// CheckRunInProgress: the check waits, here for an approval of the head (ADR-0028 section 5).
	CheckRunInProgress CheckRunStatus = "InProgress"
	// CheckRunSuccess: the check completed with conclusion success, here after an approval of the head by someone
	// who wrote no hint in the pull request. A check run of the same name created later on the same commit
	// supersedes an earlier one, so completing it is creating a completed one.
	CheckRunSuccess CheckRunStatus = "Success"
	// CheckRunFailure: the check completed with conclusion failure, here when the merge gate could not read every
	// review of the pull request and fails closed on the head.
	CheckRunFailure CheckRunStatus = "Failure"
)

// MaxCheckRunTextBytes is the most bytes of a check run's title or summary.
const MaxCheckRunTextBytes = 4 << 10

// CheckRun is a check run to create on a commit. Title and Summary are fixed text the caller builds from typed
// fields, never text read from the cluster or the code host.
type CheckRun struct {
	// Name is the check's name; CheckRunName is the only one Shadewright creates.
	Name    string
	Status  CheckRunStatus
	Title   string
	Summary string
}

// MaxCommentMarkerBytes is the longest marker GitProvider.CommentChangeRequest accepts.
const MaxCommentMarkerBytes = 256

// BranchState is what the head of a Shadewright branch carries over the target branch ([GitProvider.CheckBranch]).
// The zero value is no answer.
type BranchState int

const (
	// BranchAtTarget: the commit is the head of the target branch or an ancestor of it; the branch carries
	// nothing of its own yet.
	BranchAtTarget BranchState = iota + 1
	// BranchOneChange: one commit on top of a commit of the target branch that changes at most the one file.
	BranchOneChange
	// BranchForeign: anything else, such as a commit someone else pushed, a merge or a change to another path.
	BranchForeign
)

// GitProvider is one repository on a Git host. An instance is bound to exactly one repository at
// construction, so no method can reach another repository.
//
// Writes are idempotent where the operation allows it, so a reconcile that restarts after a crash finds the
// work it already did instead of repeating it.
//
// Every implementation must call [CheckWrite] with the method's context right before each write request it
// sends to the host and before each retry of one, as the last step before the send, and must not send that
// request, or any later one in the call, when it refuses. Reads are not checked. A write method may so stop
// part way: it returns the refusal wrapped, and the caller finishes the work later through the idempotent
// writes. This is how the kill switch reaches every request a method makes.
//
// Any method may also return ErrForbidden (the host refused the identity: a missing permission or a rule of
// the repository) or ErrRejected (the host refused the request as invalid and the state does not explain the
// refusal). Both are permanent; a rate limit or another transient failure is a RetryableError instead.
type GitProvider interface {
	// Capabilities reports what this host supports.
	Capabilities() GitCapabilities

	// ReadFile returns the content of a regular file at a commit. ErrNotFound when the commit or path does
	// not exist, or when the path is not a regular file: a directory, a submodule or a symbolic link, which is
	// never followed. Only when the implementation read the tree at that commit and the path is not a regular
	// file in it is the error ErrPathNotInTree (which wraps ErrNotFound); a missing commit or a host that
	// answers not found for another reason gives a plain ErrNotFound. ErrTooLarge when the file is larger than
	// limit bytes, clamped by [ReadLimit] to between 0 and MaxFileBytes: implementations check the size the
	// host reports before downloading the content, and stop reading a stream after limit+1 bytes, so a large
	// file in the repository cannot exhaust memory and a caller with a byte budget downloads no file past it.
	// Callers without a budget of their own pass MaxFileBytes. A limit of 0 still reads an empty file, and the
	// not-found errors do not depend on the limit.
	ReadFile(ctx context.Context, commit, path string, limit int) (File, error)

	// ListDir returns the regular files and directories directly in one directory at a commit, sorted by
	// name; it does not recurse. Symbolic links and submodules are skipped, never followed. path is "." for
	// the repository root, otherwise a clean relative path. ErrNotFound when the commit does not exist, as
	// for ReadFile; ErrPathNotInTree (which wraps ErrNotFound) only when the implementation read the tree at
	// that commit and path is not a directory in it. ErrTooLarge when the directory holds more than
	// MaxDirEntries entries, symbolic links and submodules included; implementations also bound the size of
	// the host's answer, so a large directory cannot exhaust memory. A name is returned as Git stores it (it
	// may be hidden or hold characters no path rule accepts; callers validate what they use), except that an
	// entry no tree can hold (an empty, "." or ".." name, a slash or NUL byte, invalid UTF-8, an unknown type)
	// fails the listing with ErrInvalidEntry.
	ListDir(ctx context.Context, commit, path string) ([]DirEntry, error)

	// BranchHead returns the commit at the head of a branch. ErrNotFound when the branch does not exist.
	BranchHead(ctx context.Context, branch string) (string, error)

	// CreateBranch creates a branch at a commit. Creating a branch that already exists at that commit
	// succeeds; one that exists at another commit returns ErrConflict.
	CreateBranch(ctx context.Context, branch, fromCommit string) error

	// CommitFile commits one file change on a branch whose head must be expectedHead, and returns the new
	// commit. ErrConflict when the head has moved; it never force-pushes. ErrNotFound when the file does not
	// exist at expectedHead (only existing files are changed). expectedHead must be the head of the target
	// branch or an ancestor of it: a Shadewright branch carries at most one commit of its own, made on a commit
	// of the target branch, so a branch that carries anything else is refused with an error and never extended.
	CommitFile(ctx context.Context, branch, expectedHead string, change FileChange) (string, error)

	// AppendCommit commits one file change on a Shadewright branch that already carries commits of its own
	// (the supersede handoff, ADR-0028 section 4), and returns the new commit: its one parent is expectedHead,
	// which must be the branch head and must not be the head of the target branch or an ancestor of it, and its
	// tree is the tree of expectedHead with the content of the file at change.Path replaced (same mode).
	// ErrConflict when the head has moved; it never force-pushes. ErrNotFound when the file does not exist at
	// expectedHead. Whether the branch carries only the planned change is the caller's check (CheckChain),
	// made before.
	AppendCommit(ctx context.Context, branch, expectedHead string, change FileChange) (string, error)

	// CheckBranch reports what commit, the head of a Shadewright branch, carries over the target branch, as
	// seen in the trees: BranchAtTarget when it is the head of the target branch or an ancestor of it,
	// BranchOneChange when it has exactly one parent, that parent is the head of the target branch or an
	// ancestor of it, and the two trees differ at most in the content of the regular file at path (same mode),
	// and BranchForeign for anything else. It only reads. Errors are failures to find out, never an answer.
	CheckBranch(ctx context.Context, commit, path string) (BranchState, error)

	// CheckChain is CheckBranch for a branch that may carry up to commits commits of its own (the supersede
	// chain of ADR-0028 section 4): BranchAtTarget as CheckBranch; BranchOneChange when commit and each of its
	// ancestors down to the first one whose parent is the head of the target branch or an ancestor of it, at
	// most commits of them in all, has exactly one parent and differs from it at most in the content of the
	// regular file at path (same mode); BranchForeign for anything else. CheckChain(ctx, c, p, 1) answers as
	// CheckBranch(ctx, c, p). commits outside 1 to MaxChainCommits is an error. It only reads.
	CheckChain(ctx context.Context, commit, path string, commits int) (BranchState, error)

	// Parents returns the parents of a commit, in order. ErrNotFound when the commit does not exist. It only
	// reads.
	Parents(ctx context.Context, commit string) ([]string, error)

	// CheckCommit reports what commit carries over its own parent, whatever the target branch holds:
	// BranchOneChange with that parent when commit has exactly one parent and the two trees differ at most in
	// the content of the regular file at path (same mode), BranchForeign otherwise. It never answers
	// BranchAtTarget, so a commit that a merge took into the target branch is still judged (the supersede
	// handoff of ADR-0028 section 4 recognising its own commit after a merge commit). It only reads. Errors are
	// failures to find out, never an answer.
	CheckCommit(ctx context.Context, commit, path string) (BranchState, string, error)

	// CheckMerged reports what head, the head of a change request that mergeCommit merged into the target
	// branch, carried over the target branch as it was before the merge: BranchOneChange with head's parent
	// when head has exactly one parent, that parent is the first parent of mergeCommit or an ancestor of it,
	// and the two trees differ at most in the content of the regular file at path (same mode); BranchForeign
	// otherwise, also when mergeCommit is not on the target branch or is head itself. It judges head against
	// its own parent, so a merge commit that took head into the target branch does not change the answer. It
	// only reads. Errors are failures to find out, never an answer.
	CheckMerged(ctx context.Context, head, mergeCommit, path string) (BranchState, string, error)

	// CommentChangeRequest adds one comment with body to an open change request from a Shadewright branch of
	// this repository to the target branch, unless a comment that the provider's own identity posted holds
	// marker as a whole line already, which succeeds, so a retry after a lost answer or a lost status write
	// does not post it twice; a marker in anyone else's comment does not count. A post that may have gone
	// through is never repeated without looking for the marker again. When the provider cannot read all the
	// comments to look for it, it posts nothing and returns ErrTooManyComments. body must hold marker as one of
	// its lines, of at most MaxCommentMarkerBytes. Any other change request is refused. It is a write.
	CommentChangeRequest(ctx context.Context, number int64, marker, body string) error

	// OpenChangeRequest opens a change request. When an open change request for the same head branch
	// already exists, it returns that one unchanged.
	OpenChangeRequest(ctx context.Context, req NewChangeRequest) (ChangeRequest, error)

	// FindChangeRequest returns the most recent change request for a head branch, in any state.
	// ErrNotFound when there is none.
	FindChangeRequest(ctx context.Context, headBranch string) (ChangeRequest, error)

	// EnableAutoMerge asks the host to merge the change request once its rules pass, only if its head is
	// still expectedHeadSHA (ErrConflict otherwise). Hosts without AutoMerge return an error.
	EnableAutoMerge(ctx context.Context, number int64, expectedHeadSHA string) error

	// CloseChangeRequest closes a change request without merging. Closing a closed one succeeds; closing a
	// merged one returns ErrConflict.
	CloseChangeRequest(ctx context.Context, number int64) error

	// UpdateChangeRequest replaces the title and the body of an open change request from a Shadewright branch
	// of this repository to the target branch. A closed or merged one is ErrConflict; any other change request
	// is refused. A title or body the host would refuse (over its limit, not UTF-8) is ErrRejected. Writing the
	// same title and body again succeeds. It is a write.
	UpdateChangeRequest(ctx context.Context, number int64, title, body string) error

	// CreateCheckRun creates the check run run on commit head, a full object ID. Each call creates one, so
	// the caller records that it did; the latest run of a name on a commit is the one the host counts. It is a
	// write.
	CreateCheckRun(ctx context.Context, head string, run CheckRun) error
}
