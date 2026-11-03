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
package sourcemap

import (
	"errors"
	"time"

	"github.com/pkalin/shadewright/internal/providers"
)

// ErrTransient matches every error [Resolver.Resolve] returns except a [RefusedError]: a failure that may
// succeed on retry.
var ErrTransient = errors.New("sourcemap: transient failure")

// TransientError is a failed read of the cluster or the repository. It deliberately does not wrap the
// underlying error, whose text may hold a repository URL with credentials; it keeps only what a caller
// needs to retry.
type TransientError struct {
	// Op says what failed, built only from validated names, for example "read head of branch main".
	Op string
	// RetryAfter is the delay the remote system asked for; zero when it gave none.
	RetryAfter time.Duration
}

func (e *TransientError) Error() string { return "sourcemap: " + e.Op + " failed" }

// Is reports whether target is ErrTransient.
func (e *TransientError) Is(target error) bool { return target == ErrTransient }

// RefusedError is a read the Git host refused for good: [providers.ErrForbidden] (a permission the App
// lacks, a rule of the repository) or [providers.ErrRejected]. Retrying cannot help, so it does not match
// [ErrTransient]; like TransientError it keeps no provider text, only the class.
type RefusedError struct {
	// Op says what failed, built only from validated names.
	Op string
	// Class is providers.ErrForbidden or providers.ErrRejected.
	Class error
}

func (e *RefusedError) Error() string { return "sourcemap: " + e.Op + " refused: " + e.Class.Error() }

// Is reports whether target is the class of the refusal.
func (e *RefusedError) Is(target error) bool { return target == e.Class }

// transient replaces err by a TransientError for op, dropping its text. A final refusal of the Git host
// keeps its class as a [RefusedError] instead, so the caller stops rather than retrying until a deadline.
func transient(op string, err error) error {
	switch {
	case providers.IsRetryable(err):
		// A rate limit is never a refusal, whatever else the error matches.
	case providers.IsForbidden(err):
		return &RefusedError{Op: op, Class: providers.ErrForbidden}
	case providers.IsRejected(err):
		return &RefusedError{Op: op, Class: providers.ErrRejected}
	}
	d, _ := providers.RetryAfter(err)
	return &TransientError{Op: op, RetryAfter: d}
}
