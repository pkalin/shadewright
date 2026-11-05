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
	"context"
	"errors"
)

// WriteCheckpoint is a check an adapter calls right before each request that writes to the host. A non-nil
// error means the request must not be sent. The kill switch (design section 5.4) is one.
type WriteCheckpoint func(context.Context) error

// ErrWriteStopped is what CheckWrite returns when the checkpoint refused with an error that matches none of
// the classes it was registered with.
var ErrWriteStopped = errors.New("the write was stopped by the caller's checkpoint")

type writeCheckpointKey struct{}

type writeCheckpoint struct {
	check   WriteCheckpoint
	classes []error
}

// WithWriteCheckpoint returns a context that carries check, for the adapter methods called with it, replacing
// a checkpoint ctx already carries. classes are the sentinel errors the caller needs to tell apart (for the
// kill switch: a pause, or settings that cannot be read); see [CheckWrite]. A nil check leaves ctx unchanged,
// so an inherited checkpoint stays in force: a checkpoint cannot be removed this way.
func WithWriteCheckpoint(ctx context.Context, check WriteCheckpoint, classes ...error) context.Context {
	if check == nil {
		return ctx
	}
	return context.WithValue(ctx, writeCheckpointKey{}, writeCheckpoint{check: check, classes: classes})
}

// CheckWrite calls the checkpoint ctx carries. It returns nil when ctx carries none or the checkpoint allows
// the write. When the checkpoint refuses, it returns the first of its classes the refusal matches
// ([errors.Is]), or [ErrWriteStopped]: never the checkpoint's own error, so no text of it (an API server error,
// a URL) reaches an adapter's errors, which carry fixed text only.
//
// Every adapter must call it right before each write request it sends to the host: each attempt, the first and
// every retry after a backoff, of every write a method makes (a method may make several), after any work that
// may take time (a token refresh, encoding the body), so nothing slow sits between the check and the send.
// Reads are not checked. When it refuses, the adapter sends nothing more in that call and returns the error,
// wrapped. A write request already sent is not recalled; since a method may stop between two of its writes,
// the caller relies on idempotent writes to finish the work later (see [GitProvider]).
func CheckWrite(ctx context.Context) error {
	cp, _ := ctx.Value(writeCheckpointKey{}).(writeCheckpoint)
	if cp.check == nil {
		return nil
	}
	err := cp.check(ctx)
	if err == nil {
		return nil
	}
	for _, class := range cp.classes {
		if errors.Is(err, class) {
			return class
		}
	}
	return ErrWriteStopped
}
