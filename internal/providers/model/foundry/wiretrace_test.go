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
package foundry

import (
	"context"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/shadewright-labs/shadewright/internal/testutil/tracecanary"
)

// No trace hook in the caller's context sees a request the provider sends, so none sees the access token: not
// across a retry, not on a reused connection. The control request shows the token to the recorder. The
// caller's cancellation and deadline still end a request in flight.
func TestCallerTraceHooksNeverSeeTheToken(t *testing.T) {
	var n atomic.Int32
	var hold atomic.Bool
	var mu sync.Mutex
	remotes := map[string]bool{}
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		remotes[r.RemoteAddr] = true
		mu.Unlock()
		_, _ = io.Copy(io.Discard, r.Body)
		if hold.Load() {
			<-r.Context().Done()
			return
		}
		if n.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		reply(w, completionJSON("stop", `{"role":"assistant","content":"{\"cause\":\"oom\"}","refusal":null}`))
	}, nil)
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
	tracecanary.Control(t, &rec, ctx, h.p.client, h.p.url, "Authorization", "Bearer "+goodToken)

	hold.Store(true)
	tracecanary.Ends(t, func(ctx context.Context) error {
		_, err := h.p.Complete(ctx, structuredRequest())
		return err
	})
}
