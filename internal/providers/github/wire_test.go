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
package github

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shadewright-labs/shadewright/internal/testutil/tracecanary"
)

// tlsRewrite sends requests for api.github.com to a TLS test server, through a real transport so that the
// trace hooks of the request's context run.
type tlsRewrite struct {
	target *url.URL
	inner  http.RoundTripper
}

func (r tlsRewrite) RoundTrip(req *http.Request) (*http.Response, error) {
	c := req.Clone(req.Context())
	c.URL.Scheme, c.URL.Host = r.target.Scheme, r.target.Host
	return r.inner.RoundTrip(c)
}

// wireServer is a TLS server that answers GET .../git/ref/heads/main, the first one with a 503 when fail is
// set, and counts the connections it accepts.
func wireServer(t *testing.T, fail bool, block chan struct{}) (*atomic.Int32, tlsRewrite) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", "ref_main.json"))
	if err != nil {
		t.Fatal(err)
	}
	var n atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if block != nil {
			select {
			case <-block:
			case <-r.Context().Done():
			}
			return
		}
		if fail && n.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write(body)
	}))
	var conns atomic.Int32
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			conns.Add(1)
		}
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	return &conns, tlsRewrite{target: u, inner: srv.Client().Transport}
}

func wireProvider(t *testing.T, rt http.RoundTripper, tok string) *Provider {
	t.Helper()
	p, err := newProvider(Config{RepositoryURL: testRepoURL, TargetBranch: target}, testHooks{
		transport: rt,
		token:     func(context.Context) (string, error) { return tok, nil },
		appID:     func(context.Context) (int64, error) { return testAppID, nil },
		sleep:     func(context.Context, time.Duration) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// canaryToken is built at run time so no token-shaped literal sits in the source.
func canaryToken() string { return strings.Join([]string{"ghs", "TRACECANARY", "5d81"}, "_") }

// No trace hook in the caller's context sees a request the client sends: not across a retry, not on a
// reused connection. A control request built with the caller's context shows the token to the recorder, so
// the test can fail.
func TestCallerTraceHooksNeverSeeTheToken(t *testing.T) {
	tok := canaryToken()
	for _, tc := range []struct {
		name  string
		fail  bool
		calls int
	}{
		{"one request", false, 1},
		{"retry after 503", true, 1},
		{"reused connection", false, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conns, rt := wireServer(t, tc.fail, nil)
			p := wireProvider(t, rt, tok)
			var rec tracecanary.Recorder
			ctx := rec.Context(context.Background())
			for range tc.calls {
				sha, err := p.BranchHead(ctx, target)
				if err != nil || sha == "" {
					t.Fatalf("BranchHead = %q, %v", sha, err)
				}
			}
			if seen := rec.Calls(); len(seen) != 0 {
				t.Errorf("the caller's trace saw the client's requests: %v", seen)
			}
			if tc.calls > 1 && conns.Load() != 1 {
				t.Errorf("%d connections, want 1 reused", conns.Load())
			}

			// Control: the same request built the old way, with the caller's context.
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiBase+repoPath+"/git/ref/heads/main", nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+tok)
			resp, err := p.client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if len(rec.Seen(tok)) == 0 {
				t.Fatalf("control: the recorder did not see the token; calls: %v", rec.Calls())
			}
		})
	}
}

// The caller's cancellation still aborts a request in flight, and the caller's deadline still applies.
func TestWireRequestEndsWithTheCallersContext(t *testing.T) {
	for _, tc := range []struct {
		name string
		ctx  func() (context.Context, context.CancelFunc)
		want error
	}{
		{"cancel", func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(context.Background())
			time.AfterFunc(200*time.Millisecond, cancel)
			return ctx, cancel
		}, context.Canceled},
		{"deadline", func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), 200*time.Millisecond)
		}, context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			block := make(chan struct{})
			defer close(block)
			_, rt := wireServer(t, false, block)
			p := wireProvider(t, rt, canaryToken())
			ctx, cancel := tc.ctx()
			defer cancel()
			start := time.Now()
			_, err := p.BranchHead(ctx, target)
			if err == nil || !errors.Is(err, tc.want) {
				t.Errorf("BranchHead error = %v, want %v", err, tc.want)
			}
			if d := time.Since(start); d > 10*time.Second {
				t.Errorf("the request ran %v after its context ended", d)
			}
		})
	}
}
