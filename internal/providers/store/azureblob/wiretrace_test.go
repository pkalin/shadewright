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
package azureblob

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/shadewright-labs/shadewright/internal/testutil/tracecanary"
)

// No trace hook in the caller's context sees a request the store sends, so none sees the access token: not
// across a retry, not on a reused connection. The control request shows the token to the recorder. The
// caller's cancellation and deadline still end a request in flight.
func TestCallerTraceHooksNeverSeeTheToken(t *testing.T) {
	var hold atomic.Bool
	arrivals := tracecanary.NewArrivals()
	var mu sync.Mutex
	remotes := map[string]bool{}
	created := status(http.StatusCreated)
	answer := func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		remotes[r.RemoteAddr] = true
		mu.Unlock()
		if hold.Load() {
			arrivals.Arrived()
			<-r.Context().Done()
			return
		}
		created(w, r)
	}
	s := newServer(t, status(http.StatusInternalServerError), answer)
	st, _ := testStore(t, s)
	var rec tracecanary.Recorder
	ctx := rec.Context(context.Background())
	for range 3 {
		if _, err := st.Put(ctx, testRecord()); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	if got := len(s.requests()); got != 4 {
		t.Errorf("%d requests, want 4 (one retried)", got)
	}
	if len(remotes) != 1 {
		t.Errorf("%d connections, want 1 reused", len(remotes))
	}
	if calls := rec.Calls(); len(calls) != 0 {
		t.Errorf("the caller's trace saw the store's requests: %v", calls)
	}
	tracecanary.Control(t, &rec, ctx, st.client, st.container.String(), "Authorization", "Bearer "+testToken)

	hold.Store(true)
	tracecanary.Ends(t, arrivals, func(ctx context.Context) error {
		_, err := st.Put(ctx, testRecord())
		return err
	})
}
