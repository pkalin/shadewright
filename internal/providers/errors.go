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
	// ErrWithheld means redaction withheld the answer whole: its work ran out of its budget, or the answer was
	// longer than redaction reads (package redact, budget.go). Nothing of the answer is returned; a caller shows it
	// as a source that failed.
	ErrWithheld = errors.New("withheld by redaction")
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
	// ErrUnauthorized means the host rejected the identity's credentials (HTTP 401): a GitHub App whose private key
	// is wrong or rotated without the cluster's copy, or an App that was deleted, so the JWT or the installation
	// token it signed is refused. It is not ErrForbidden and not retryable: it holds until a person fixes the App
	// or its key, and callers that class refusals keep treating it as a plain failure.
	ErrUnauthorized = errors.New("unauthorized")
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
	// ErrCredentialUnavailable means the adapter could not use the static credential the cluster operator
	// provides as a file: it could not be read, was empty or is malformed. The adapter sent nothing with it.
	// A file that could not be read or was empty comes inside a [RetryableError] (a mounted Secret may be between
	// two versions); a malformed one is permanent until a person fixes the file.
	ErrCredentialUnavailable = errors.New("the credential file could not be used")
	// ErrNotSent marks the error of a call that sent no request at all, on any attempt, so nothing of it can
	// have been billed. Only an adapter that can tell this for certain marks an error with it ([NotSent]).
	ErrNotSent = errors.New("no request was sent")
	// ErrNotProcessed marks the error of a call whose every attempt the host refused before processing it, a
	// refusal the host documents as not billed, so nothing of it was billed. It is a fact about one host, read
	// from the status of the HTTP response the adapter received, never from a body or model output: only an
	// adapter whose host documents the refusal as free marks an error with it ([NotProcessed]).
	ErrNotProcessed = errors.New("the host refused the request before processing it")
)

// notProcessedError keeps the text and the chain of err and adds ErrNotProcessed to it.
type notProcessedError struct{ err error }

func (e *notProcessedError) Error() string { return e.err.Error() }

// Unwrap returns the underlying error.
func (e *notProcessedError) Unwrap() error { return e.err }

// Is reports whether target is ErrNotProcessed.
func (e *notProcessedError) Is(target error) bool { return target == ErrNotProcessed }

// NotProcessed marks err as the error of a call that the host refused before processing it on every attempt,
// keeping its text and its chain. Nil stays nil.
func NotProcessed(err error) error {
	if err == nil {
		return nil
	}
	return &notProcessedError{err: err}
}

// notSentError keeps the text and the chain of err and adds ErrNotSent to it.
type notSentError struct{ err error }

func (e *notSentError) Error() string { return e.err.Error() }

// Unwrap returns the underlying error.
func (e *notSentError) Unwrap() error { return e.err }

// Is reports whether target is ErrNotSent.
func (e *notSentError) Is(target error) bool { return target == ErrNotSent }

// NotSent marks err as the error of a call that sent no request, keeping its text and its chain. Nil stays nil.
func NotSent(err error) error {
	if err == nil {
		return nil
	}
	return &notSentError{err: err}
}

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

// IsUnauthorized reports whether err is, or wraps, ErrUnauthorized.
func IsUnauthorized(err error) bool { return errors.Is(err, ErrUnauthorized) }

// IsRejected reports whether err means the host refused the request as invalid.
func IsRejected(err error) bool { return errors.Is(err, ErrRejected) }

// IsInvalidEntry reports whether err means a directory listing held an entry that cannot be one.
func IsInvalidEntry(err error) bool { return errors.Is(err, ErrInvalidEntry) }

// IsMalformedOutput reports whether err means a model's reply broke the contract of the request.
func IsMalformedOutput(err error) bool { return errors.Is(err, ErrMalformedOutput) }

// IsUntrustedAnswer reports whether err is or wraps ErrUntrustedAnswer.
func IsUntrustedAnswer(err error) bool { return errors.Is(err, ErrUntrustedAnswer) }

// IsCredentialUnavailable reports whether err means the adapter's credential file could not be used.
func IsCredentialUnavailable(err error) bool { return errors.Is(err, ErrCredentialUnavailable) }

// IsNotSent reports whether err is marked as the error of a call that sent no request.
func IsNotSent(err error) bool { return errors.Is(err, ErrNotSent) }

// IsNotProcessed reports whether err is marked as the error of a call the host refused before processing it.
func IsNotProcessed(err error) bool { return errors.Is(err, ErrNotProcessed) }

// RefusedUnbilled reports whether a call that ended with err and the Completion c cost nothing because its host
// refused it before processing: err is marked [ErrNotProcessed] and no attempt was billed without a usage
// (c.UsageUnknown). It is the one place that rule is written; the adapter that marks, the analysis loop that
// charges and the evaluation harness that counts all ask it.
func RefusedUnbilled(c Completion, err error) bool { return IsNotProcessed(err) && !c.UsageUnknown }

// RetryAfter returns the delay requested by the remote system, if err carries one.
func RetryAfter(err error) (time.Duration, bool) {
	var r *RetryableError
	if errors.As(err, &r) && r.RetryAfter > 0 {
		return r.RetryAfter, true
	}
	return 0, false
}
