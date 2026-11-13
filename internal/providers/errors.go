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

import (
	"errors"
	"fmt"
	"time"
)

// Sentinel errors. Adapters wrap them (fmt.Errorf("...: %w", ErrNotFound)) so callers can use errors.Is.
var (
	// ErrNotFound means the object asked for does not exist.
	ErrNotFound = errors.New("not found")
	// ErrPathNotInTree means the adapter read the tree at the commit asked for and the path is not a regular
	// file in it: absent, a directory, a submodule or a symbolic link. It wraps ErrNotFound, so IsNotFound
	// holds too. Unlike a plain ErrNotFound, which an adapter also returns for a commit the host does not have
	// (yet) or a 404 from a host that is failing, it never changes for the same commit and path.
	ErrPathNotInTree = fmt.Errorf("path not in tree: %w", ErrNotFound)
	// ErrConflict means the request was based on stale state, for example a commit whose expected base is no
	// longer the head of the branch, or an object that already exists with different content.
	ErrConflict = errors.New("conflict")
	// ErrRetryable means a transient failure: the same request may succeed later.
	ErrRetryable = errors.New("retryable")
	// ErrTooLarge means a file is larger than MaxFileBytes, and its content was not downloaded, or a result is
	// larger than the adapter's limit, and was refused while it was read.
	ErrTooLarge = errors.New("too large")
	// ErrInvalidConfig means the settings an adapter is built from cannot be used (wrong provider, missing
	// signer, malformed key ID or URL). It is permanent: the same settings fail the same way on every retry.
	ErrInvalidConfig = errors.New("invalid configuration")
	// ErrForbidden means the host refused the request for the identity: a permission the App or token lacks,
	// or a rule of the repository (branch protection, a ruleset). It is permanent until a person changes the
	// permission or the rule; a rate limit is never ErrForbidden but a [RetryableError].
	ErrForbidden = errors.New("forbidden")
	// ErrRejected means the host refused the request itself as invalid (validation failed, for example a
	// branch name or pull request it does not accept) and the adapter could not resolve the refusal by reading
	// the state. It is permanent: the same request fails the same way.
	ErrRejected = errors.New("rejected")
	// ErrMalformedOutput means a model answered, but its reply breaks the contract of the request: it was cut
	// off at the output limit, filtered or refused, is not the JSON object a response schema asked for, or
	// calls a tool the request did not offer. The call was billed, so the [ModelProvider] returns the reply's
	// Usage with it. It is not retryable as such: whether to ask once more is the caller's decision (ADR-0023).
	ErrMalformedOutput = errors.New("malformed model output")
	// ErrInvalidEntry means a directory listing holds an entry that cannot be one: a name that is empty, "." or
	// "..", holds a slash or a NUL byte or is not valid UTF-8, or a type that is neither a file, a directory, a
	// symbolic link nor a submodule. It is permanent for that commit: the tree does not change.
	ErrInvalidEntry = errors.New("invalid directory entry")
	// ErrTooManyComments means a change request holds more comments than the adapter reads to look for a
	// comment marker, so it cannot tell whether its own comment is there already: it posts nothing (fail
	// closed). It lasts while the conversation does: a retry fails the same way.
	ErrTooManyComments = errors.New("too many comments to look for a marker")
	// ErrUntrustedAnswer means the host answered, but with another object than the one asked for (a pull request
	// with another number), so nothing in the answer can be used. It is not retried: the caller ends what it was
	// doing instead of asking again without an end.
	ErrUntrustedAnswer = errors.New("untrusted answer")
)

// RetryableError is a transient failure with an optional delay requested by the remote system, for example
// from a Retry-After header. It matches ErrRetryable with errors.Is.
type RetryableError struct {
	// RetryAfter is the delay the remote system asked for; zero when it gave none.
	RetryAfter time.Duration
	// Err is the underlying error.
	Err error
}

func (e *RetryableError) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("retryable (after %s): %v", e.RetryAfter, e.Err)
	}
	return fmt.Sprintf("retryable: %v", e.Err)
}

// Unwrap returns the underlying error.
func (e *RetryableError) Unwrap() error { return e.Err }

// Is reports whether target is ErrRetryable.
func (e *RetryableError) Is(target error) bool { return target == ErrRetryable }

// IsNotFound reports whether err means the object does not exist.
func IsNotFound(err error) bool { return errors.Is(err, ErrNotFound) }

// IsPathNotInTree reports whether err means the path is not a regular file in the tree of the commit, as read
// from the host. Such an answer can be kept for that commit and path.
func IsPathNotInTree(err error) bool { return errors.Is(err, ErrPathNotInTree) }

// IsConflict reports whether err means the request was based on stale state.
func IsConflict(err error) bool { return errors.Is(err, ErrConflict) }

// IsTooLarge reports whether err means a file exceeds MaxFileBytes or a result exceeds an adapter's limit.
func IsTooLarge(err error) bool { return errors.Is(err, ErrTooLarge) }

// IsRetryable reports whether err is a transient failure.
func IsRetryable(err error) bool { return errors.Is(err, ErrRetryable) }

// IsInvalidConfig reports whether err means the settings an adapter is built from cannot be used.
func IsInvalidConfig(err error) bool { return errors.Is(err, ErrInvalidConfig) }

// IsForbidden reports whether err means the host refused the identity (a missing permission or a rule).
func IsForbidden(err error) bool { return errors.Is(err, ErrForbidden) }

// IsRejected reports whether err means the host refused the request as invalid.
func IsRejected(err error) bool { return errors.Is(err, ErrRejected) }

// IsInvalidEntry reports whether err means a directory listing held an entry that cannot be one.
func IsInvalidEntry(err error) bool { return errors.Is(err, ErrInvalidEntry) }

// IsMalformedOutput reports whether err means a model's reply broke the contract of the request.
func IsMalformedOutput(err error) bool { return errors.Is(err, ErrMalformedOutput) }

// IsUntrustedAnswer reports whether err is or wraps ErrUntrustedAnswer.
func IsUntrustedAnswer(err error) bool { return errors.Is(err, ErrUntrustedAnswer) }

// RetryAfter returns the delay requested by the remote system, if err carries one.
func RetryAfter(err error) (time.Duration, bool) {
	var r *RetryableError
	if errors.As(err, &r) && r.RetryAfter > 0 {
		return r.RetryAfter, true
	}
	return 0, false
}
