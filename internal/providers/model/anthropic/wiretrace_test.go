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
package anthropic

import (
	"context"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/shadewright-labs/shadewright/internal/testutil/tracecanary"
)

// No trace hook in the caller's context sees a request the provider sends, so none sees the API key: not
// across a retry, not on a reused connection. The control request shows the key to the recorder. The
// caller's cancellation and deadline still end a request in flight.
func TestCallerTraceHooksNeverSeeTheKey(t *testing.T) {
	var n atomic.Int32
	var hold atomic.Bool
	arrivals := tracecanary.NewArrivals()
	var mu sync.Mutex
	remotes := map[string]bool{}
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		remotes[r.RemoteAddr] = true
		mu.Unlock()
		if hold.Load() {
			_, _ = io.Copy(io.Discard, r.Body) // so the server sees the client go away
			arrivals.Arrived()
			<-r.Context().Done()
			return
		}
		if n.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		reply(w, messageJSON(testModel, stopEndTurn, `[{"type":"text","text":"{\"cause\":\"oom\"}"}]`))
	})
	var rec tracecanary.Recorder
	ctx := rec.Context(context.Background())
	for range 3 {
		if _, err := h.p.Complete(ctx, structuredRequest()); err != nil {
			t.Fatalf("Complete: %v", err)
		}
	}
	if n.Load() != 4 {
		t.Errorf("%d requests, want 4 (one retried)", n.Load())
	}
	if len(remotes) != 1 {
		t.Errorf("%d connections, want 1 reused", len(remotes))
	}
	if calls := rec.Calls(); len(calls) != 0 {
		t.Errorf("the caller's trace saw the provider's requests: %v", calls)
	}
	tracecanary.Control(t, &rec, ctx, h.p.client, messagesURL, "x-api-key", keyMark)

	hold.Store(true)
	tracecanary.Ends(t, arrivals, func(ctx context.Context) error {
		_, err := h.p.Complete(ctx, structuredRequest())
		return err
	})
}
