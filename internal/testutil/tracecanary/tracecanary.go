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

// Package tracecanary records every hook of a net/http/httptrace.ClientTrace, so a test can check that a
// client which sends a credential header does not hand it to trace hooks that came with the caller's context
// (WroteHeaderField receives every header value, Authorization included). It is for tests only.
package tracecanary

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptrace"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// Recorder holds what the hooks of its trace received, each call as its hook name and its arguments printed
// with %+v. It is safe for concurrent use: net/http calls some hooks from its own goroutines.
type Recorder struct {
	mu    sync.Mutex
	calls []string
}

// Trace returns a ClientTrace with every hook set, found by reflection so that a hook added to net/http later
// is recorded too. A hook that returns a value returns its zero value.
func (r *Recorder) Trace() *httptrace.ClientTrace {
	trace := &httptrace.ClientTrace{}
	v := reflect.ValueOf(trace).Elem()
	for i := range v.NumField() {
		f, name := v.Field(i), v.Type().Field(i).Name
		if f.Kind() != reflect.Func || !f.CanSet() {
			continue
		}
		ft := f.Type()
		f.Set(reflect.MakeFunc(ft, func(args []reflect.Value) []reflect.Value {
			parts := make([]string, 0, len(args))
			for _, a := range args {
				parts = append(parts, fmt.Sprintf("%+v", a.Interface()))
			}
			r.mu.Lock()
			r.calls = append(r.calls, name+"("+strings.Join(parts, ", ")+")")
			r.mu.Unlock()
			out := make([]reflect.Value, ft.NumOut())
			for j := range out {
				out[j] = reflect.Zero(ft.Out(j))
			}
			return out
		}))
	}
	return trace
}

// Context returns parent with the recorder's trace added.
func (r *Recorder) Context(parent context.Context) context.Context {
	return httptrace.WithClientTrace(parent, r.Trace())
}

// Calls returns the recorded calls.
func (r *Recorder) Calls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

// Reset forgets the recorded calls.
func (r *Recorder) Reset() {
	r.mu.Lock()
	r.calls = nil
	r.mu.Unlock()
}

// Seen returns the recorded calls that contain any of the canaries.
func (r *Recorder) Seen(canaries ...string) []string {
	var out []string
	for _, c := range r.Calls() {
		for _, k := range canaries {
			if k != "" && strings.Contains(c, k) {
				out = append(out, c)
				break
			}
		}
	}
	return out
}

// Control sends one GET to url through client, built the way a client must not build its requests: with ctx,
// which carries rec's trace, and with header set to value. It fails t when rec did not see value, so a test
// that finds no hook called by the client under test cannot pass because the recorder saw nothing at all.
func Control(t testing.TB, rec *Recorder, ctx context.Context, client *http.Client, url, header, value string) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(header, value)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("control request: %v", err)
	}
	_ = resp.Body.Close()
	if len(rec.Seen(value)) == 0 {
		t.Fatalf("control: the recorder did not see the %s header; calls: %v", header, rec.Calls())
	}
}

// Arrivals tells [Ends] that the test server received a request. A handler that holds a request calls
// Arrived before it waits.
type Arrivals struct{ ch chan struct{} }

// NewArrivals returns an empty Arrivals.
func NewArrivals() *Arrivals { return &Arrivals{ch: make(chan struct{}, 64)} }

// Arrived records that the server received a request. It never blocks.
func (a *Arrivals) Arrived() {
	select {
	case a.ch <- struct{}{}:
	default:
	}
}

// drain forgets the arrivals recorded so far.
func (a *Arrivals) drain() {
	for {
		select {
		case <-a.ch:
		default:
			return
		}
	}
}

// mode is how a context ends in [Ends].
type mode string

const (
	cancelMode   mode = "cancel"
	deadlineMode mode = "deadline"
)

const (
	// endsDeadline is the deadline of the deadline case, counted from the start of the call. The request must
	// reach the server before it passes.
	endsDeadline = 2 * time.Second
	// endsWait bounds every wait of Ends, so a hung call fails the test instead of blocking it.
	endsWait = 10 * time.Second
)

// Ends checks that call, whose request the server holds until the client goes away, ends with its context:
// once with a context canceled after the server reported the request to a, once with a context whose
// deadline passes while the server holds it. Each case gets a fresh context. The call must return an error
// that matches context.Canceled or context.DeadlineExceeded respectively; a call that fails before the server
// has its request, or with another error, fails the test.
func Ends(t *testing.T, a *Arrivals, call func(ctx context.Context) error) {
	t.Helper()
	for _, m := range []mode{cancelMode, deadlineMode} {
		t.Run(string(m), func(t *testing.T) {
			if err := ends(a, m, call); err != nil {
				t.Error(err)
			}
		})
	}
}

// ends runs one case of [Ends] and returns what was wrong with it, or nil.
func ends(a *Arrivals, m mode, call func(ctx context.Context) error) error {
	a.drain()
	want := context.Canceled
	ctx, cancel := context.WithCancel(context.Background())
	if m == deadlineMode {
		want = context.DeadlineExceeded
		ctx, cancel = context.WithTimeout(context.Background(), endsDeadline)
	}
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- call(ctx) }()
	select {
	case err := <-done:
		return fmt.Errorf("the call returned before the server received its request: %w", err)
	case <-a.ch:
	case <-time.After(endsWait):
		cancel()
		return fmt.Errorf("the server never received the request; the call then returned: %w", <-done)
	}
	if m == cancelMode {
		cancel()
	} else if ctx.Err() != nil {
		return fmt.Errorf("the deadline passed before the server received the request (%v)", endsDeadline)
	}
	select {
	case err := <-done:
		if !errors.Is(err, want) {
			return fmt.Errorf("the call returned %w, want an error that matches %w", err, want)
		}
		return nil
	case <-time.After(endsWait):
		return fmt.Errorf("the call did not return within %v after its context ended", endsWait)
	}
}
