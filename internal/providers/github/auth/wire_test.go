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
package auth

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shadewright-labs/shadewright/internal/providers/fake"
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

// wireEnv is a Source talking to a TLS server that answers the token exchange (the first one with a 503 when
// fail is set) and GET /app, and keeps every App JWT it was sent. When hold is set, it answers nothing: it
// reports each request to hold and keeps it until the client goes away.
type wireEnv struct {
	src   *Source
	clk   *clock
	conns atomic.Int32
	mu    sync.Mutex
	jwts  []string
	rt    tlsRewrite
}

func newWireEnv(t *testing.T, fail bool, hold *tracecanary.Arrivals) *wireEnv {
	t.Helper()
	e := &wireEnv{clk: &clock{t: t0}}
	var n atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		e.jwts = append(e.jwts, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		e.mu.Unlock()
		if hold != nil {
			_, _ = io.Copy(io.Discard, r.Body) // so the server sees the client go away
			hold.Arrived()
			<-r.Context().Done()
			return
		}
		if r.URL.Path == "/app" {
			_, _ = io.WriteString(w, `{"id":77}`)
			return
		}
		if fail && n.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, okBody(e.clk.now().Add(time.Hour)))
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			e.conns.Add(1)
		}
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	e.rt = tlsRewrite{target: u, inner: srv.Client().Transport}
	signer, err := fake.NewSigner()
	if err != nil {
		t.Fatal(err)
	}
	e.src, err = newSource(Config{
		ClientID: testClientID, InstallationID: 4242, RepositoryURL: testRepoURL, Signer: signer,
	}, testHooks{transport: e.rt, now: e.clk.now})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *wireEnv) sentJWTs() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.jwts...)
}

// No trace hook in the caller's context sees the App JWT of the token exchange or of GET /app: not across a
// retry, not on a reused connection. A control request built with the caller's context shows the JWT to the
// recorder, so the test can fail.
func TestCallerTraceHooksNeverSeeTheAppJWT(t *testing.T) {
	for _, tc := range []struct {
		name     string
		fail     bool
		refresh  int
		wantErrs int
	}{
		{"exchange", false, 1, 0},
		{"retry after 503", true, 2, 1},
		{"reused connection", false, 3, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newWireEnv(t, tc.fail, nil)
			var rec tracecanary.Recorder
			ctx := rec.Context(context.Background())
			errs := 0
			for range tc.refresh {
				tok, err := e.src.Token(ctx)
				if err != nil {
					errs++
					continue
				}
				if tok.Reveal() != testToken {
					t.Fatal("Token returned another value")
				}
				e.clk.mu.Lock()
				e.clk.t = e.clk.t.Add(2 * time.Hour) // the next call exchanges again
				e.clk.mu.Unlock()
			}
			if errs != tc.wantErrs {
				t.Errorf("%d failed exchanges, want %d", errs, tc.wantErrs)
			}
			if id, err := e.src.AppID(ctx); err != nil || id != 77 {
				t.Fatalf("AppID = %d, %v", id, err)
			}
			jwts := e.sentJWTs()
			if len(jwts) != tc.refresh+1 {
				t.Fatalf("%d requests, want %d", len(jwts), tc.refresh+1)
			}
			if seen := rec.Calls(); len(seen) != 0 {
				t.Errorf("the caller's trace saw the source's requests: %v", seen)
			}
			if tc.refresh > 1 && e.conns.Load() != 1 {
				t.Errorf("%d connections, want 1 reused", e.conns.Load())
			}

			// Control: a request built the old way, with the caller's context and a JWT sent before.
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiBase+"/app", nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+jwts[0])
			resp, err := e.src.client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if len(rec.Seen(jwts[0])) == 0 {
				t.Fatalf("control: the recorder did not see the JWT; calls: %v", rec.Calls())
			}
		})
	}
}

// The caller's cancellation still aborts an exchange in flight, and the caller's deadline still applies, to
// the token exchange and to GET /app, each with a context of its own.
func TestExchangeEndsWithTheCallersContext(t *testing.T) {
	arrivals := tracecanary.NewArrivals()
	e := newWireEnv(t, false, arrivals)
	t.Run("token", func(t *testing.T) {
		tracecanary.Ends(t, arrivals, func(ctx context.Context) error {
			_, err := e.src.Token(ctx)
			return err
		})
	})
	t.Run("app", func(t *testing.T) {
		tracecanary.Ends(t, arrivals, func(ctx context.Context) error {
			_, err := e.src.AppID(ctx)
			return err
		})
	})
}
