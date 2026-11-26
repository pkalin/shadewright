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
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shadewright-labs/shadewright/internal/providers"
)

const (
	testClientID = "00000000-0000-0000-0000-000000000001"
	testTenantID = "00000000-0000-0000-0000-000000000002"
	assertionJWT = "eyJhbGciOiJSUzI1NiJ9.ASSERTION-SECRET.c2ln"
)

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func goodEnv(t *testing.T) map[string]string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte(assertionJWT+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return map[string]string{envClientID: testClientID, envTenantID: testTenantID, envTokenFile: file}
}

func TestNewWorkloadIdentityChecksTheEnvironment(t *testing.T) {
	w, err := newWorkloadIdentity(envOf(goodEnv(t)), nil)
	if err != nil {
		t.Fatalf("valid environment refused: %v", err)
	}
	if w.tokenURL != "https://login.microsoftonline.com/"+testTenantID+"/oauth2/v2.0/token" {
		t.Errorf("token URL = %q", w.tokenURL)
	}
	for name, mod := range map[string]func(map[string]string){
		"no client ID":       func(m map[string]string) { delete(m, envClientID) },
		"client ID not GUID": func(m map[string]string) { m[envClientID] = "x/../y" },
		"no tenant":          func(m map[string]string) { delete(m, envTenantID) },
		"tenant path":        func(m map[string]string) { m[envTenantID] = "a/b" },
		"no token file":      func(m map[string]string) { delete(m, envTokenFile) },
		"unknown authority":  func(m map[string]string) { m[envAuthorityHost] = "https://login.example.com/" },
		"plain http":         func(m map[string]string) { m[envAuthorityHost] = "http://login.microsoftonline.com/" },
		"authority path":     func(m map[string]string) { m[envAuthorityHost] = "https://login.microsoftonline.com/x" },
	} {
		env := goodEnv(t)
		mod(env)
		_, err := newWorkloadIdentity(envOf(env), nil)
		if !providers.IsInvalidConfig(err) {
			t.Errorf("%s: err = %v, want invalid config", name, err)
		}
	}
	env := goodEnv(t)
	env[envAuthorityHost] = "https://login.microsoftonline.us/"
	if _, err := newWorkloadIdentity(envOf(env), nil); err != nil {
		t.Errorf("US Government authority refused: %v", err)
	}
}

// entraHarness is a WorkloadIdentity whose token URL points at an httptest TLS server.
func entraHarness(t *testing.T, handler http.HandlerFunc) (*WorkloadIdentity, *atomic.Int32, map[string]string) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	env := goodEnv(t)
	w, err := newWorkloadIdentity(envOf(env), srv.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	w.tokenURL = srv.URL + "/" + testTenantID + "/oauth2/v2.0/token"
	return w, &n, env
}

func TestWorkloadIdentityExchangesAndCaches(t *testing.T) {
	var form url.Values
	w, n, _ := entraHarness(t, func(rw http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		form = r.PostForm
		rw.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(rw, `{"token_type":"Bearer","expires_in":3600,"access_token":%q}`, goodToken)
	})
	now := time.Now()
	w.now = func() time.Time { return now }

	tok, err := w.Token(context.Background())
	if err != nil || tok != goodToken {
		t.Fatalf("Token = %v", err)
	}
	for k, want := range map[string]string{
		"grant_type":            "client_credentials",
		"client_id":             testClientID,
		"client_assertion_type": "urn:ietf:params:oauth:client-assertion-type:jwt-bearer",
		"client_assertion":      assertionJWT,
		"scope":                 "https://cognitiveservices.azure.com/.default",
	} {
		if form.Get(k) != want {
			t.Errorf("form %s = %q, want %q", k, form.Get(k), want)
		}
	}
	if _, err := w.Token(context.Background()); err != nil || n.Load() != 1 {
		t.Errorf("cached token not reused: requests %d, err %v", n.Load(), err)
	}
	now = now.Add(56 * time.Minute) // within five minutes of expiry
	if _, err := w.Token(context.Background()); err != nil || n.Load() != 2 {
		t.Errorf("token not refreshed before expiry: requests %d, err %v", n.Load(), err)
	}
}

func TestWorkloadIdentityErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		status    int
		body      string
		retryable bool
		forbidden bool
	}{
		"refused":         {status: http.StatusBadRequest, body: `{"error_description":"` + bodyMark + `"}`, forbidden: true},
		"unauthorized":    {status: http.StatusUnauthorized, body: bodyMark, forbidden: true},
		"request timeout": {status: http.StatusRequestTimeout, body: bodyMark, retryable: true},
		// The assertion is not within its valid time range: a rotated token file can fix it.
		"assertion expired": {status: http.StatusBadRequest, retryable: true,
			body: `{"error":"invalid_client","error_description":"AADSTS700024: ` + bodyMark + `","error_codes":[700024]}`},
		"assertion time range": {status: http.StatusBadRequest, retryable: true,
			body: `{"error":"invalid_grant","error_description":"` + bodyMark + `","error_codes":[500133]}`},
		"entra transient": {status: http.StatusBadRequest, retryable: true,
			body: `{"error":"temporarily_unavailable","error_codes":[50033]}`},
		"no federation": {status: http.StatusBadRequest, forbidden: true,
			body: `{"error":"invalid_client","error_description":"` + bodyMark + `","error_codes":[700213]}`},
		"code on a 401": {status: http.StatusUnauthorized, forbidden: true, body: `{"error_codes":[700024]}`},
		"oversized refusal": {status: http.StatusBadRequest, forbidden: true,
			body: `{"error_codes":[700024],"pad":"` + strings.Repeat("a", maxTokenResponse) + `"}`},
		"throttled":       {status: http.StatusTooManyRequests, body: bodyMark, retryable: true},
		"unavailable":     {status: http.StatusServiceUnavailable, body: bodyMark, retryable: true},
		"not JSON":        {status: http.StatusOK, body: bodyMark, retryable: true},
		"no token":        {status: http.StatusOK, body: `{"token_type":"Bearer","expires_in":3600}`, retryable: true},
		"wrong type":      {status: http.StatusOK, body: `{"token_type":"pop","expires_in":3600,"access_token":"` + bodyMark + `"}`, retryable: true},
		"no expiry":       {status: http.StatusOK, body: `{"token_type":"Bearer","access_token":"` + bodyMark + `"}`, retryable: true},
		"oversized reply": {status: http.StatusOK, body: strings.Repeat("a", maxTokenResponse+1), retryable: true},
	} {
		t.Run(name, func(t *testing.T) {
			w, _, _ := entraHarness(t, func(rw http.ResponseWriter, r *http.Request) {
				rw.WriteHeader(tc.status)
				_, _ = io.WriteString(rw, tc.body)
			})
			_, err := w.Token(context.Background())
			if err == nil || providers.IsRetryable(err) != tc.retryable || providers.IsForbidden(err) != tc.forbidden {
				t.Fatalf("err = %v", err)
			}
			if s := fmt.Sprintf("%+v", err); strings.Contains(s, bodyMark) || strings.Contains(s, "ASSERTION-SECRET") {
				t.Errorf("error leaks: %v", err)
			}
		})
	}
}

func TestWorkloadIdentityTokenFile(t *testing.T) {
	w, n, env := entraHarness(t, func(rw http.ResponseWriter, r *http.Request) {})
	for name, prepare := range map[string]func(path string){
		"missing":    func(p string) { _ = os.Remove(p) },
		"empty file": func(p string) { _ = os.WriteFile(p, []byte(" \n"), 0o600) },
		"too large":  func(p string) { _ = os.WriteFile(p, []byte(strings.Repeat("a", maxTokenFile+1)), 0o600) },
		"directory":  func(p string) { _ = os.Remove(p); _ = os.Mkdir(p, 0o700) },
	} {
		t.Run(name, func(t *testing.T) {
			path := env[envTokenFile]
			_ = os.RemoveAll(path)
			_ = os.WriteFile(path, []byte(assertionJWT), 0o600)
			prepare(path)
			_, err := w.Token(context.Background())
			if !providers.IsRetryable(err) || strings.Contains(err.Error(), path) {
				t.Fatalf("err = %v, want retryable without the path", err)
			}
		})
	}
	if n.Load() != 0 {
		t.Errorf("sent %d token requests without an assertion", n.Load())
	}
}

func TestWorkloadIdentityCancellation(t *testing.T) {
	started, gone := make(chan struct{}), make(chan struct{})
	w, _, _ := entraHarness(t, func(rw http.ResponseWriter, r *http.Request) {
		close(started)
		_, _ = io.Copy(io.Discard, r.Body) // the server sees the client go away only after the body is read
		<-r.Context().Done()
		close(gone)
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-started; cancel() }()
	_, err := w.Token(ctx)
	if !errors.Is(err, context.Canceled) || providers.IsRetryable(err) {
		t.Fatalf("err = %v, want canceled", err)
	}
	// With no caller left, the exchange itself is canceled rather than left to its timeout.
	select {
	case <-gone:
	case <-time.After(5 * time.Second):
		t.Fatal("the token request went on after its only caller left")
	}
}

// A token for another audience is refused and not cached, so the next call asks again.
func TestWorkloadIdentityRefusesAnotherAudience(t *testing.T) {
	w, n, _ := entraHarness(t, func(rw http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(rw, `{"token_type":"Bearer","expires_in":3600,"access_token":%q}`, fakeJWT("https://management.azure.com"))
	})
	for range 2 {
		if _, err := w.Token(context.Background()); !providers.IsForbidden(err) {
			t.Fatalf("err = %v, want forbidden", err)
		}
	}
	if n.Load() != 2 {
		t.Errorf("sent %d token requests, want 2 (nothing cached)", n.Load())
	}
}

// A caller waiting for another caller's exchange gives up when its own context ends.
func TestWorkloadIdentityWaitRespectsContext(t *testing.T) {
	release := make(chan struct{})
	w, _, _ := entraHarness(t, func(http.ResponseWriter, *http.Request) { <-release })
	defer close(release)
	go func() { _, _ = w.Token(context.Background()) }() // another exchange in progress
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := w.Token(ctx)
	if !errors.Is(err, context.DeadlineExceeded) || !providers.IsRetryable(err) {
		t.Fatalf("err = %v, want a retryable deadline", err)
	}
	cctx, ccancel := context.WithCancel(context.Background())
	ccancel()
	if _, err := w.Token(cctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want canceled", err)
	}
}

// Callers share one exchange, and a canceled caller does not fail the exchange the others wait for.
func TestWorkloadIdentitySharesOneExchange(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	w, n, _ := entraHarness(t, func(rw http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(started) })
		<-release
		_, _ = fmt.Fprintf(rw, `{"token_type":"Bearer","expires_in":3600,"access_token":%q}`, goodToken)
	})
	first, cancelFirst := context.WithCancel(context.Background())
	firstErr := make(chan error, 1)
	go func() { _, err := w.Token(first); firstErr <- err }()
	<-started
	type result struct {
		tok string
		err error
	}
	waiters := make(chan result, 3)
	for range 3 {
		go func() { tok, err := w.Token(context.Background()); waiters <- result{tok, err} }()
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		w.mu.Lock()
		joined := w.inflight != nil && w.inflight.waiters == 4
		w.mu.Unlock()
		if joined {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the waiters did not join the exchange")
		}
	}
	cancelFirst()
	if err := <-firstErr; !errors.Is(err, context.Canceled) {
		t.Fatalf("first caller: err = %v, want canceled", err)
	}
	close(release)
	for range 3 {
		if r := <-waiters; r.err != nil || r.tok != goodToken {
			t.Fatalf("waiter: err = %v", r.err)
		}
	}
	if n.Load() != 1 {
		t.Errorf("sent %d token requests, want 1 shared exchange", n.Load())
	}
}

// A token whose lifetime is at or below the refresh margin is still reused for half its lifetime, not
// exchanged on every call.
func TestWorkloadIdentityCachesShortLivedTokens(t *testing.T) {
	for _, expiresIn := range []int{60, 300, 2} {
		t.Run(fmt.Sprint(expiresIn), func(t *testing.T) {
			w, n, _ := entraHarness(t, func(rw http.ResponseWriter, r *http.Request) {
				_, _ = fmt.Fprintf(rw, `{"token_type":"Bearer","expires_in":%d,"access_token":%q}`, expiresIn, goodToken)
			})
			now := time.Now()
			w.now = func() time.Time { return now }
			for range 3 {
				if _, err := w.Token(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			if n.Load() != 1 {
				t.Fatalf("sent %d token requests for 3 calls, want 1", n.Load())
			}
			now = now.Add(time.Duration(expiresIn) * time.Second / 2)
			if _, err := w.Token(context.Background()); err != nil || n.Load() != 2 {
				t.Errorf("not refreshed at half its lifetime: requests %d, err %v", n.Load(), err)
			}
		})
	}
}

// lateEntra answers only once the exchange's context has ended, as net/http can return an answer that raced
// the end of the context (#767). It ends the caller's context itself, so the only waiter leaves and the
// exchange is canceled; the test needs no clock. call is the exchange it served.
type lateEntra struct {
	w    **WorkloadIdentity
	end  context.CancelFunc
	code int
	call chan *exchangeCall
}

func (l lateEntra) RoundTrip(r *http.Request) (*http.Response, error) {
	w := *l.w
	w.mu.Lock()
	l.call <- w.inflight
	w.mu.Unlock()
	l.end()
	<-r.Context().Done()
	body := fmt.Sprintf(`{"token_type":"Bearer","expires_in":3600,"access_token":%q}`, goodToken)
	return &http.Response{StatusCode: l.code, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
}

// An exchange whose context ended classifies nothing of the answer that raced that end, and caches no token
// of it (#779).
func TestWorkloadIdentityAnswerAfterContextEnd(t *testing.T) {
	for _, code := range []int{http.StatusOK, http.StatusUnauthorized} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var w *WorkloadIdentity
			calls := make(chan *exchangeCall, 1)
			w, err := newWorkloadIdentity(envOf(goodEnv(t)), lateEntra{w: &w, end: cancel, code: code, call: calls})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := w.Token(ctx); !errors.Is(err, context.Canceled) {
				t.Fatalf("Token = %v, want the caller's cancellation", err)
			}
			call := <-calls
			<-call.done
			if !errors.Is(call.err, context.Canceled) || errors.Is(call.err, providers.ErrForbidden) {
				t.Errorf("exchange err = %v, want its cancellation", call.err)
			}
			w.mu.Lock()
			cached := w.cachedToken()
			w.mu.Unlock()
			if cached != "" {
				t.Error("a token taken after the context ended was cached")
			}
		})
	}
}

// rtFunc is a RoundTripper made of a function, so a test can serve any response and body.
type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// closeGate is a response body whose Close waits, so a test can hold an exchange between its last context
// check and its write to the cache.
type closeGate struct {
	io.Reader
	entered, release chan struct{}
	enterOnce        sync.Once
	releaseOnce      sync.Once
}

func (c *closeGate) Close() error {
	c.enterOnce.Do(func() { close(c.entered) })
	<-c.release
	return nil
}

func (c *closeGate) unblock() {
	c.releaseOnce.Do(func() { close(c.release) })
}

const testWaitBound = 30 * time.Second

func waitSignal(t *testing.T, ch <-chan struct{}, msg string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(testWaitBound):
		t.Fatal(msg)
	}
}

func waitErr(t *testing.T, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(testWaitBound):
		t.Fatal("first caller never returned")
		return nil
	}
}

func entraTokenBody(tok string) string {
	return fmt.Sprintf(`{"token_type":"Bearer","expires_in":3600,"access_token":%q}`, tok)
}

// An exchange that was abandoned and replaced by a newer successful exchange must not overwrite the cache,
// while still finishing with its own result.
func TestWorkloadIdentitySupersededExchangeDoesNotWriteTheCache(t *testing.T) {
	stale := fakeJWT([]string{cognitiveServicesAudience})
	if stale == goodToken {
		t.Fatal("test setup: stale and replacement tokens must differ")
	}
	gate := &closeGate{
		Reader:  strings.NewReader(entraTokenBody(stale)),
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	defer gate.unblock()

	var n atomic.Int32
	var w *WorkloadIdentity
	first := make(chan *exchangeCall, 1)
	w, err := newWorkloadIdentity(envOf(goodEnv(t)), rtFunc(func(r *http.Request) (*http.Response, error) {
		if n.Add(1) == 1 {
			w.mu.Lock()
			first <- w.inflight
			w.mu.Unlock()
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: gate, Request: r}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{},
			Body:       io.NopCloser(strings.NewReader(entraTokenBody(goodToken))),
			Request:    r,
		}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}

	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	errA := make(chan error, 1)
	go func() { _, err := w.Token(ctxA); errA <- err }()
	waitSignal(t, gate.entered, "the first exchange never reached the end of its response")

	cancelA() // the only waiter leaves
	if err := waitErr(t, errA); !errors.Is(err, context.Canceled) {
		t.Fatalf("first caller: err = %v, want canceled", err)
	}
	if tok, err := w.Token(context.Background()); err != nil || tok != goodToken {
		t.Fatalf("second exchange: err = %v, got replacement token = %v", err, tok == goodToken)
	}

	gate.unblock()
	call := <-first
	waitSignal(t, call.done, "the first exchange never finished")
	if tok, err := call.result(); err != nil || tok != stale {
		t.Errorf("first exchange result: err = %v, got own token = %v; want own token and nil error", err, tok == stale)
	}
	if tok, err := w.Token(context.Background()); err != nil || tok != goodToken || n.Load() != 2 {
		t.Errorf("cache check: err = %v, got second token = %v, got stale token = %v, exchanges = %d; want second token from 2 exchanges",
			err, tok == goodToken, tok == stale, n.Load())
	}
}

// An abandoned exchange that succeeds with no newer exchange still caches its valid token, so the next Token
// call reuses it without making a second exchange.
func TestWorkloadIdentityAbandonedExchangeCachesWhenAlone(t *testing.T) {
	firstTok := fakeJWT([]string{cognitiveServicesAudience})
	if firstTok == goodToken {
		t.Fatal("test setup: tokens must differ")
	}
	gate := &closeGate{
		Reader:  strings.NewReader(entraTokenBody(firstTok)),
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	defer gate.unblock()

	var n atomic.Int32
	var w *WorkloadIdentity
	first := make(chan *exchangeCall, 1)
	w, err := newWorkloadIdentity(envOf(goodEnv(t)), rtFunc(func(r *http.Request) (*http.Response, error) {
		if n.Add(1) == 1 {
			w.mu.Lock()
			first <- w.inflight
			w.mu.Unlock()
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: gate, Request: r}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{},
			Body:       io.NopCloser(strings.NewReader(entraTokenBody(goodToken))),
			Request:    r,
		}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}

	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	errA := make(chan error, 1)
	go func() { _, err := w.Token(ctxA); errA <- err }()
	waitSignal(t, gate.entered, "the first exchange never reached the end of its response")

	cancelA() // the only waiter leaves, and no newer caller arrives
	if err := waitErr(t, errA); !errors.Is(err, context.Canceled) {
		t.Fatalf("first caller: err = %v, want canceled", err)
	}

	gate.unblock()
	call := <-first
	waitSignal(t, call.done, "the first exchange never finished")
	if tok, err := call.result(); err != nil || tok != firstTok {
		t.Errorf("first exchange result: err = %v, got own token = %v; want own token and nil error", err, tok == firstTok)
	}
	if tok, err := w.Token(context.Background()); err != nil || tok != firstTok || n.Load() != 1 {
		t.Errorf("cache check: err = %v, got first token = %v, exchanges = %d; want first token from 1 exchange",
			err, tok == firstTok, n.Load())
	}
}

// An abandoned exchange that succeeds after a newer exchange failed still caches its valid token.
func TestWorkloadIdentityAbandonedExchangeCachesAfterReplacementFails(t *testing.T) {
	firstTok := fakeJWT([]string{cognitiveServicesAudience})
	if firstTok == goodToken {
		t.Fatal("test setup: tokens must differ")
	}
	gate := &closeGate{
		Reader:  strings.NewReader(entraTokenBody(firstTok)),
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	defer gate.unblock()

	var n atomic.Int32
	var w *WorkloadIdentity
	first := make(chan *exchangeCall, 1)
	w, err := newWorkloadIdentity(envOf(goodEnv(t)), rtFunc(func(r *http.Request) (*http.Response, error) {
		switch n.Add(1) {
		case 1:
			w.mu.Lock()
			first <- w.inflight
			w.mu.Unlock()
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: gate, Request: r}, nil
		case 2:
			return &http.Response{
				StatusCode: http.StatusServiceUnavailable,
				Header:     http.Header{},
				Body:       io.NopCloser(strings.NewReader("unavailable")),
				Request:    r,
			}, nil
		default:
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{},
				Body:       io.NopCloser(strings.NewReader(entraTokenBody(goodToken))),
				Request:    r,
			}, nil
		}
	}))
	if err != nil {
		t.Fatal(err)
	}

	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	errA := make(chan error, 1)
	go func() { _, err := w.Token(ctxA); errA <- err }()
	waitSignal(t, gate.entered, "the first exchange never reached the end of its response")

	cancelA() // the only waiter leaves
	if err := waitErr(t, errA); !errors.Is(err, context.Canceled) {
		t.Fatalf("first caller: err = %v, want canceled", err)
	}
	if _, err := w.Token(context.Background()); !providers.IsRetryable(err) {
		t.Fatalf("second exchange: err = %v, want retryable failure", err)
	}

	gate.unblock()
	call := <-first
	waitSignal(t, call.done, "the first exchange never finished")
	if tok, err := call.result(); err != nil || tok != firstTok {
		t.Errorf("first exchange result: err = %v, got own token = %v; want own token and nil error", err, tok == firstTok)
	}
	if tok, err := w.Token(context.Background()); err != nil || tok != firstTok || n.Load() != 2 {
		t.Errorf("cache check: err = %v, got first token = %v, exchanges = %d; want first token from 2 exchanges",
			err, tok == firstTok, n.Load())
	}
}

// A live waiter of an exchange always gets that exchange's own result, even when an older abandoned exchange
// finishes and writes the cache while the newer exchange is still in flight.
func TestWorkloadIdentityLiveWaiterGetsOwnCallResult(t *testing.T) {
	firstTok := fakeJWT([]string{cognitiveServicesAudience})
	if firstTok == goodToken {
		t.Fatal("test setup: tokens must differ")
	}
	for _, tc := range []struct {
		name      string
		secondOK  bool
		wantCache string
	}{
		{name: "replacement succeeds", secondOK: true, wantCache: goodToken},
		{name: "replacement fails", secondOK: false, wantCache: firstTok},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gate1 := &closeGate{
				Reader:  strings.NewReader(entraTokenBody(firstTok)),
				entered: make(chan struct{}),
				release: make(chan struct{}),
			}
			defer gate1.unblock()
			call2Started := make(chan struct{})
			call2Release := make(chan struct{})
			var release2Once sync.Once
			unblock2 := func() { release2Once.Do(func() { close(call2Release) }) }
			defer unblock2()

			var n atomic.Int32
			var w *WorkloadIdentity
			first := make(chan *exchangeCall, 1)
			w, err := newWorkloadIdentity(envOf(goodEnv(t)), rtFunc(func(r *http.Request) (*http.Response, error) {
				switch n.Add(1) {
				case 1:
					w.mu.Lock()
					first <- w.inflight
					w.mu.Unlock()
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: gate1, Request: r}, nil
				case 2:
					close(call2Started)
					<-call2Release
					if tc.secondOK {
						return &http.Response{
							StatusCode: http.StatusOK,
							Header:     http.Header{},
							Body:       io.NopCloser(strings.NewReader(entraTokenBody(goodToken))),
							Request:    r,
						}, nil
					}
					return &http.Response{
						StatusCode: http.StatusServiceUnavailable,
						Header:     http.Header{},
						Body:       io.NopCloser(strings.NewReader("unavailable")),
						Request:    r,
					}, nil
				default:
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     http.Header{},
						Body:       io.NopCloser(strings.NewReader(entraTokenBody(goodToken))),
						Request:    r,
					}, nil
				}
			}))
			if err != nil {
				t.Fatal(err)
			}

			ctxA, cancelA := context.WithCancel(context.Background())
			defer cancelA()
			errA := make(chan error, 1)
			go func() { _, err := w.Token(ctxA); errA <- err }()
			waitSignal(t, gate1.entered, "the first exchange never reached the end of its response")

			cancelA()
			if err := waitErr(t, errA); !errors.Is(err, context.Canceled) {
				t.Fatalf("first caller: err = %v, want canceled", err)
			}

			type result struct {
				tok string
				err error
			}
			resB := make(chan result, 1)
			go func() {
				tok, err := w.Token(context.Background())
				resB <- result{tok, err}
			}()
			waitSignal(t, call2Started, "the second exchange never started")

			gate1.unblock()
			call1 := <-first
			waitSignal(t, call1.done, "the first exchange never finished")
			if tok, err := call1.result(); err != nil || tok != firstTok {
				t.Errorf("first exchange result: err = %v, got own token = %v; want own token and nil error", err, tok == firstTok)
			}
			w.mu.Lock()
			stillInflight := w.inflight != nil && w.inflight != call1
			w.mu.Unlock()
			if !stillInflight {
				t.Error("older exchange cleared inflight while the newer exchange was still running")
			}

			unblock2()
			var r result
			select {
			case r = <-resB:
			case <-time.After(testWaitBound):
				t.Fatal("second caller never returned")
			}
			if tc.secondOK {
				if r.err != nil || r.tok != goodToken {
					t.Errorf("live waiter: err = %v, got second token = %v; want second token", r.err, r.tok == goodToken)
				}
			} else {
				if r.tok != "" || !providers.IsRetryable(r.err) {
					t.Errorf("live waiter: err = %v, empty token = %v; want retryable error and no token", r.err, r.tok == "")
				}
			}
			if tok, err := w.Token(context.Background()); err != nil || tok != tc.wantCache || n.Load() != 2 {
				t.Errorf("cache check: err = %v, got expected token = %v, exchanges = %d; want expected token from 2 exchanges",
					err, tok == tc.wantCache, n.Load())
			}
		})
	}
}
