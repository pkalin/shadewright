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
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/shadewright-labs/shadewright/internal/testutil/tracecanary"
)

type wireKey struct{}

// The wire context keeps the caller's end and deadline and none of its values.
func TestWireContextKeepsTheEndAndHidesValues(t *testing.T) {
	deadline := time.Now().Add(time.Hour)
	for _, tc := range []struct {
		name   string
		parent func() (context.Context, context.CancelFunc)
		end    error
	}{
		{"cancel", func() (context.Context, context.CancelFunc) { return context.WithCancel(context.Background()) }, context.Canceled},
		{"deadline", func() (context.Context, context.CancelFunc) {
			return context.WithDeadline(context.Background(), deadline)
		}, context.Canceled},
		{"passed deadline", func() (context.Context, context.CancelFunc) {
			return context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		}, context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent, cancel := tc.parent()
			defer cancel()
			var rec tracecanary.Recorder
			parent = context.WithValue(rec.Context(parent), wireKey{}, "v")
			w := WireContext(parent)
			if w.Value(wireKey{}) != nil || w.Value(nil) != nil {
				t.Error("a value of the caller's context is visible")
			}
			pd, pok := parent.Deadline()
			wd, wok := w.Deadline()
			if pd != wd || pok != wok {
				t.Errorf("Deadline = %v %v, want %v %v", wd, wok, pd, pok)
			}
			// A context derived from it, as net/http derives one, ends with the parent.
			child, stop := context.WithCancel(w)
			defer stop()
			cancel()
			for _, c := range []context.Context{w, child} {
				select {
				case <-c.Done():
				case <-time.After(5 * time.Second):
					t.Fatal("the wire context did not end with its parent")
				}
				if !errors.Is(c.Err(), tc.end) {
					t.Errorf("Err = %v, want %v", c.Err(), tc.end)
				}
			}
		})
	}
	if w := WireContext(context.Background()); w.Done() != nil || w.Err() != nil {
		t.Error("a wire context of a context that never ends ends")
	}
}

// A context derived from the wire context starts no goroutine of its own, and requests made with it leave
// none behind.
func TestWireContextStartsNoGoroutine(t *testing.T) {
	parent := t.Context()
	before := runtime.NumGoroutine()
	stops := make([]context.CancelFunc, 0, 1000)
	for range 1000 {
		_, stop := context.WithTimeout(WireContext(parent), time.Hour)
		stops = append(stops, stop)
	}
	if n := runtime.NumGoroutine() - before; n > 50 {
		t.Errorf("1000 derived contexts started %d goroutines", n)
	}
	for _, s := range stops {
		s()
	}

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()
	client := srv.Client()
	client.Timeout = time.Minute // net/http derives a context with a deadline for each request
	do := func() {
		req, err := http.NewRequestWithContext(WireContext(parent), http.MethodGet, srv.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	do() // the connection and its goroutines
	before = runtime.NumGoroutine()
	for range 1000 {
		do()
	}
	time.Sleep(100 * time.Millisecond)
	if n := runtime.NumGoroutine() - before; n > 10 {
		t.Errorf("1000 requests left %d goroutines behind", n)
	}
}

// A request built with the wire context reaches none of the caller's trace hooks; the same request built with
// the caller's context hands its Authorization header to them, so the recorder works.
func TestWireContextHidesTraceHooks(t *testing.T) {
	canary := strings.Join([]string{"Bearer", "CANARY", "wire"}, "-")
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer srv.Close()
	var rec tracecanary.Recorder
	ctx := rec.Context(context.Background())
	send := func(ctx context.Context) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", canary)
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	send(ctx)
	if len(rec.Seen(canary)) == 0 {
		t.Fatalf("control: the trace did not see the header; calls: %v", rec.Calls())
	}
	rec.Reset()
	send(WireContext(ctx))
	if calls := rec.Calls(); len(calls) != 0 {
		t.Errorf("the trace saw a request built with the wire context: %v", calls)
	}
}
