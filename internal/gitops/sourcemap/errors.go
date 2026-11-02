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

// ErrTransient matches every error [Resolver.Resolve] returns: a failure that may succeed on retry.
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

// transient replaces err by a TransientError for op, dropping its text.
func transient(op string, err error) error {
	d, _ := providers.RetryAfter(err)
	return &TransientError{Op: op, RetryAfter: d}
}
