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
package openaicompatible

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shadewright-labs/shadewright/internal/testutil/tracecanary"
)

// No trace hook in the caller's context sees a request the provider sends, so none sees the API key: not
// across a retry, not on a reused connection. The control request shows the key to the recorder. The
// caller's cancellation and deadline still end a request in flight.
func TestCallerTraceHooksNeverSeeTheKey(t *testing.T) {
	key := strings.Join([]string{"sk", "TRACECANARY", "c3a0"}, "-")
	var n atomic.Int32
	var hold atomic.Bool
	var mu sync.Mutex
	remotes := map[string]bool{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, chatOK)
	}))
	t.Cleanup(srv.Close)
	// Every connection goes to the test server, whose certificate names example.com.
	tr := srv.Client().Transport.(*http.Transport).Clone()
	tr.TLSClientConfig.ServerName = "example.com"
	addr := srv.Listener.Addr().String()
	tr.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
	p, err := newProvider(Config{Endpoint: endpoint, Deployment: testModel, KeyFile: keyFile(t, key)}, testHooks{
		transport: tr,
		sleep:     func(ctx context.Context, _ time.Duration) error { return ctx.Err() },
		jitter:    func(d time.Duration) time.Duration { return d },
	})
	if err != nil {
		t.Fatal(err)
	}
	var rec tracecanary.Recorder
	ctx := rec.Context(context.Background())
	for range 3 {
		if _, err := p.Complete(ctx, request()); err != nil {
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
	tracecanary.Control(t, &rec, ctx, p.client, p.url, "Authorization", "Bearer "+key)

	hold.Store(true)
	tracecanary.Ends(t, func(ctx context.Context) error {
		_, err := p.Complete(ctx, request())
		return err
	})
}
