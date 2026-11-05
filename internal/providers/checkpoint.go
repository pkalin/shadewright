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

// WriteCheckpoint is a check an adapter calls right before each request that writes to the host. A non-nil
// error means the request must not be sent: the adapter returns that error, wrapped, and sends nothing more in
// that call. The kill switch (design section 5.4) is one.
type WriteCheckpoint func(context.Context) error

type writeCheckpointKey struct{}

// WithWriteCheckpoint returns a context that carries check, for the adapter methods called with it. It
// replaces a checkpoint ctx already carries. A nil check returns ctx unchanged.
func WithWriteCheckpoint(ctx context.Context, check WriteCheckpoint) context.Context {
	if check == nil {
		return ctx
	}
	return context.WithValue(ctx, writeCheckpointKey{}, check)
}

// CheckWrite calls the checkpoint ctx carries and returns its error, or nil when ctx carries none.
//
// An adapter calls it immediately before every write request it sends to the host: each attempt, the first and
// every retry after a backoff, of every write a method makes (a method may make several). Reads are not
// checked. A write request already sent is not recalled; the checkpoint only decides whether the next one goes
// out. Since a method may stop between two of its writes, the caller relies on idempotent writes to finish the
// work later (see [GitProvider]).
func CheckWrite(ctx context.Context) error {
	check, _ := ctx.Value(writeCheckpointKey{}).(WriteCheckpoint)
	if check == nil {
		return nil
	}
	return check(ctx)
}
