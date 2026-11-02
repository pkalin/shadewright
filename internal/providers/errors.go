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
	// ErrConflict means the request was based on stale state, for example a commit whose expected base is no
	// longer the head of the branch, or an object that already exists with different content.
	ErrConflict = errors.New("conflict")
	// ErrRetryable means a transient failure: the same request may succeed later.
	ErrRetryable = errors.New("retryable")
	// ErrTooLarge means a file is larger than MaxFileBytes; its content was not downloaded.
	ErrTooLarge = errors.New("too large")
	// ErrInvalidConfig means the settings an adapter is built from cannot be used (wrong provider, missing
	// signer, malformed key ID or URL). It is permanent: the same settings fail the same way on every retry.
	ErrInvalidConfig = errors.New("invalid configuration")
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

// IsConflict reports whether err means the request was based on stale state.
func IsConflict(err error) bool { return errors.Is(err, ErrConflict) }

// IsTooLarge reports whether err means a file exceeds MaxFileBytes.
func IsTooLarge(err error) bool { return errors.Is(err, ErrTooLarge) }

// IsRetryable reports whether err is a transient failure.
func IsRetryable(err error) bool { return errors.Is(err, ErrRetryable) }

// IsInvalidConfig reports whether err means the settings an adapter is built from cannot be used.
func IsInvalidConfig(err error) bool { return errors.Is(err, ErrInvalidConfig) }

// RetryAfter returns the delay requested by the remote system, if err carries one.
func RetryAfter(err error) (time.Duration, bool) {
	var r *RetryableError
	if errors.As(err, &r) && r.RetryAfter > 0 {
		return r.RetryAfter, true
	}
	return 0, false
}
