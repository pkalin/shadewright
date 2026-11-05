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
	// MaxFileBytes: implementations check the size the host reports before downloading the content, and stop
	// reading a stream after MaxFileBytes+1 bytes, so a large file in the repository cannot exhaust memory.
	ReadFile(ctx context.Context, commit, path string) (File, error)

	// BranchHead returns the commit at the head of a branch. ErrNotFound when the branch does not exist.
	BranchHead(ctx context.Context, branch string) (string, error)

	// CreateBranch creates a branch at a commit. Creating a branch that already exists at that commit
	// succeeds; one that exists at another commit returns ErrConflict.
	CreateBranch(ctx context.Context, branch, fromCommit string) error

	// CommitFile commits one file change on a branch whose head must be expectedHead, and returns the new
	// commit. ErrConflict when the head has moved; it never force-pushes. ErrNotFound when the file does not
	// exist at expectedHead (only existing files are changed).
	CommitFile(ctx context.Context, branch, expectedHead string, change FileChange) (string, error)

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
}
