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
	"time"
)

// WireContext returns the context for an HTTP request that carries a credential header. It ends when ctx
// ends, with ctx's error, and has ctx's deadline, but it carries none of ctx's values. net/http reads values
// from a request's context: a net/http/httptrace.ClientTrace there receives every header the client writes,
// Authorization included (WroteHeaderField), and httptrace.WithClientTrace adds to the hooks already in a
// context rather than replacing them. So a hook that a caller or an instrumentation library put into ctx
// would see the credential. Build only the request with this context; keep ctx for everything before it
// (credential lookup, the write checkpoint, logging) and for classifying the request's errors.
//
// It starts no goroutine: a context that net/http derives from it (a request timeout, the transport's own
// cancellation) registers with ctx through [context.AfterFunc] instead of watching it from a goroutine of
// its own.
func WireContext(ctx context.Context) context.Context { return wireContext{parent: ctx} }

type wireContext struct{ parent context.Context }

func (c wireContext) Deadline() (time.Time, bool) { return c.parent.Deadline() }
func (c wireContext) Done() <-chan struct{}       { return c.parent.Done() }
func (c wireContext) Err() error                  { return c.parent.Err() }
func (wireContext) Value(any) any                 { return nil }

// AfterFunc arranges for f to run when the parent ends. context.WithCancel, WithDeadline and the others
// call it (through an interface the context package does not export) to follow a parent they cannot find
// through its values, instead of starting a goroutine per derived context.
func (c wireContext) AfterFunc(f func()) func() bool { return context.AfterFunc(c.parent, f) }

// String names the context without its parent, for a context printed in a test failure.
func (wireContext) String() string { return "providers.WireContext" }
