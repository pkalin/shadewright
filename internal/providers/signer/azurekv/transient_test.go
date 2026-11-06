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

package azurekv

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"

	"github.com/pkalin/shadewright/internal/providers"
)

// failingBody fails every read with err, as a truncated or reset response body does.
type failingBody struct{ err error }

func (b failingBody) Read([]byte) (int, error) { return 0, b.err }
func (failingBody) Close() error               { return nil }

// breakBodies replaces the body of the first n authorized responses with one that fails with err.
type breakBodies struct {
	inner policy.Transporter
	n     int32
	err   error
	seen  atomic.Int32
}

func (b *breakBodies) Do(req *http.Request) (*http.Response, error) {
	resp, err := b.inner.Do(req)
	if err != nil || req.Header.Get("Authorization") == "" || b.seen.Add(1) > b.n {
		return resp, err
	}
	_ = resp.Body.Close()
	resp.Body = failingBody{err: b.err}
	resp.ContentLength = -1
	resp.Header.Del("Content-Length")
	return resp, nil
}

// TestSignBodyReadFailureIsRetryable covers a response body that breaks off: Sign returns a fixed, retryable
// error when no retry is left, and a retry with backoff succeeds when one is.
func TestSignBodyReadFailureIsRetryable(t *testing.T) {
	kid := "https://" + testHost + "/keys/github-app/" + testVersion
	for name, readErr := range map[string]error{
		"unexpected EOF": io.ErrUnexpectedEOF,
		"other":          errors.New("tls: " + sdkText),
	} {
		var bb *breakBodies
		wrap := func(inner policy.Transporter) policy.Transporter {
			bb = &breakBodies{inner: inner, n: 1, err: readErr}
			return bb
		}
		s, v := setupWith(t, kid, Options{MaxRetries: -1}, &fakeCredential{}, wrap, clientConfig{})
		v.respond = v.signOK(kid)
		_, err := s.Sign(context.Background(), digestOf("x"))
		if !providers.IsRetryable(err) || !errors.Is(err, errBodyRead) {
			t.Errorf("%s: err = %v, want the retryable body read error", name, err)
		}
		assertCleanChain(t, name, err)

		s, v = setupWith(t, kid, Options{MaxRetries: 1}, &fakeCredential{}, wrap, clientConfig{})
		v.respond = v.signOK(kid)
		if _, err := s.Sign(context.Background(), digestOf("x")); err != nil {
			t.Errorf("%s with a retry left: %v", name, err)
		}
		if got := bb.seen.Load(); got != 2 {
			t.Errorf("%s: %d authorized requests, want 2 (one retry)", name, got)
		}
	}
}

// lateBodyReader reads each request body once more after it returned the response, as net/http's transport
// may (it copies the body in its own goroutine and can still read it, if only for the final EOF, after the
// response came back). wait blocks until every such read is done.
type lateBodyReader struct {
	inner policy.Transporter
	wg    sync.WaitGroup
}

func (l *lateBodyReader) Do(req *http.Request) (*http.Response, error) {
	resp, err := l.inner.Do(req)
	if body := req.Body; body != nil {
		l.wg.Go(func() {
			time.Sleep(5 * time.Millisecond)
			_, _ = io.Copy(io.Discard, body)
		})
	}
	return resp, err
}

// TestRetryGivesTheTransportItsOwnBody checks that azcore rewinding the request body for a retry never
// touches the body a previous attempt handed to the HTTP transport: run with -race, the late read of the
// first attempt's body and the rewind for the second are unsynchronized when they share one reader.
func TestRetryGivesTheTransportItsOwnBody(t *testing.T) {
	kid := "https://" + testHost + "/keys/github-app/" + testVersion
	var late *lateBodyReader
	wrap := func(inner policy.Transporter) policy.Transporter {
		late = &lateBodyReader{inner: &breakBodies{inner: inner, n: 1, err: io.ErrUnexpectedEOF}}
		return late
	}
	s, v := setupWith(t, kid, Options{MaxRetries: 1}, &fakeCredential{}, wrap, clientConfig{})
	v.respond = v.signOK(kid)
	if _, err := s.Sign(context.Background(), digestOf("x")); err != nil {
		t.Fatalf("Sign with a retry left: %v", err)
	}
	late.wg.Wait()
}

// TestSignBodyReadKeepsCancellation checks that a body read that fails because the caller gave up is still
// a non-retryable cancellation, not a transient failure.
func TestSignBodyReadKeepsCancellation(t *testing.T) {
	kid := "https://" + testHost + "/keys/github-app/" + testVersion
	ctx, cancel := context.WithCancel(context.Background())
	wrap := func(inner policy.Transporter) policy.Transporter {
		return transporterFunc(func(req *http.Request) (*http.Response, error) {
			resp, err := inner.Do(req)
			if err == nil && req.Header.Get("Authorization") != "" {
				cancel()
				_ = resp.Body.Close()
				resp.Body = failingBody{err: context.Canceled}
			}
			return resp, err
		})
	}
	s, v := setupWith(t, kid, Options{}, &fakeCredential{}, wrap, clientConfig{})
	v.respond = v.signOK(kid)
	_, err := s.Sign(ctx, digestOf("x"))
	if !errors.Is(err, context.Canceled) || providers.IsRetryable(err) {
		t.Errorf("err = %v, want a non-retryable cancellation", err)
	}
}

type transporterFunc func(*http.Request) (*http.Response, error)

func (f transporterFunc) Do(req *http.Request) (*http.Response, error) { return f(req) }

// blockingCredential waits for its context to end and then fails the way the pinned azidentity does on a
// timeout: with an error that does not wrap the context error.
type blockingCredential struct{}

func (blockingCredential) GetToken(ctx context.Context, _ policy.TokenRequestOptions) (azcore.AccessToken, error) {
	<-ctx.Done()
	return azcore.AccessToken{}, errors.New("token request failed: " + sdkText)
}

// TestSignCredentialTryTimeout covers a token request that hits the per-try deadline while the Sign
// deadline is still running: it is transient, and a caller that cancels still gets a cancellation.
func TestSignCredentialTryTimeout(t *testing.T) {
	kid := "https://" + testHost + "/keys/github-app"
	s, v := setupWith(t, kid, Options{Timeout: 5 * time.Second, MaxRetries: -1}, blockingCredential{}, nil,
		clientConfig{tryTimeout: 100 * time.Millisecond})
	v.respond = v.signOK(kid + "/" + testVersion)
	start := time.Now()
	_, err := s.Sign(context.Background(), digestOf("x"))
	if el := time.Since(start); el > 3*time.Second {
		t.Errorf("Sign took %s, want about the 100ms try timeout", el)
	}
	if !providers.IsRetryable(err) || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want a retryable deadline error", err)
	}
	assertCleanChain(t, "try timeout", err)

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	_, err = s.Sign(ctx, digestOf("x"))
	if !errors.Is(err, context.Canceled) || providers.IsRetryable(err) {
		t.Errorf("caller cancel: err = %v, want a non-retryable cancellation", err)
	}
}

// cutBody returns a cut JSON body and a clean EOF once ctx ends, as an HTTP/1.1 response does when the
// client's close_notify reaches the server before the socket closes and the server ends a chunked response.
type cutBody struct {
	ctx  context.Context
	data io.Reader
}

func (b *cutBody) Read(p []byte) (int, error) {
	<-b.ctx.Done()
	return b.data.Read(p)
}

func (*cutBody) Close() error { return nil }

// cutAuthorized replaces the body of each authorized response with a cutBody holding data, sent chunked,
// and then calls onAuthorized when it is set.
func cutAuthorized(data string, onAuthorized func()) func(policy.Transporter) policy.Transporter {
	return func(inner policy.Transporter) policy.Transporter {
		return transporterFunc(func(req *http.Request) (*http.Response, error) {
			resp, err := inner.Do(req)
			if err != nil || req.Header.Get("Authorization") == "" {
				return resp, err
			}
			_ = resp.Body.Close()
			resp.Body = &cutBody{ctx: req.Context(), data: strings.NewReader(data)}
			resp.ContentLength = -1
			resp.Header.Del("Content-Length")
			if onAuthorized != nil {
				onAuthorized()
			}
			return resp, nil
		})
	}
}

// TestSignTryTimeoutWithCleanlyEndedBody covers a per-try deadline after which the body read still ends
// without an error but with a cut body: it is the try's retryable deadline error, not a final SDK error
// from decoding the cut JSON. A caller that cancels in the same place still gets a non-retryable
// cancellation.
func TestSignTryTimeoutWithCleanlyEndedBody(t *testing.T) {
	kid := "https://" + testHost + "/keys/github-app/" + testVersion
	cutJSON := `{"kid":"` + kid + `","value":"`

	s, v := setupWith(t, kid, Options{Timeout: 30 * time.Second, MaxRetries: -1}, &fakeCredential{},
		cutAuthorized(cutJSON, nil), clientConfig{tryTimeout: 50 * time.Millisecond})
	v.respond = v.signOK(kid)
	_, err := s.Sign(context.Background(), digestOf("x"))
	if !providers.IsRetryable(err) || !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, errRequestSDK) {
		t.Errorf("try timeout: err = %v, want a retryable deadline error", err)
	}
	assertCleanChain(t, "try timeout with a cleanly ended body", err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, v = setupWith(t, kid, Options{Timeout: 30 * time.Second, MaxRetries: -1}, &fakeCredential{},
		cutAuthorized(cutJSON, cancel), clientConfig{tryTimeout: 30 * time.Second})
	v.respond = v.signOK(kid)
	_, err = s.Sign(ctx, digestOf("x"))
	if !errors.Is(err, context.Canceled) || providers.IsRetryable(err) {
		t.Errorf("caller cancel: err = %v, want a non-retryable cancellation", err)
	}
}

// TestSignOversizedBodyAtTryDeadlineIsFinal covers an oversized chunked body whose read ends as the per-try
// deadline passes: it stays the final response too large error, not a retryable timeout.
func TestSignOversizedBodyAtTryDeadlineIsFinal(t *testing.T) {
	kid := "https://" + testHost + "/keys/github-app/" + testVersion
	s, v := setupWith(t, kid, Options{Timeout: 30 * time.Second, MaxRetries: 2}, &fakeCredential{},
		cutAuthorized(strings.Repeat("a", maxResponseBody+1), nil), clientConfig{tryTimeout: 50 * time.Millisecond})
	v.respond = v.signOK(kid)
	_, err := s.Sign(context.Background(), digestOf("x"))
	if !errors.Is(err, errResponseTooLarge) || providers.IsRetryable(err) {
		t.Errorf("err = %v, want the final response too large error", err)
	}
	assertCleanChain(t, "oversized body at the try deadline", err)
	if n := len(v.authorized()); n != 1 {
		t.Errorf("%d authorized requests, want 1 (no retry)", n)
	}
}

// TestGuardTransportCancelledWithCleanBody calls guardTransport directly with a request whose context was
// cancelled and a body that ends cleanly: it returns the cancellation, which is final, and never the body.
func TestGuardTransportCancelledWithCleanBody(t *testing.T) {
	for _, entra := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+testHost+"/keys/k/sign", nil)
		if err != nil {
			t.Fatal(err)
		}
		g := guardTransport{entra: entra, inner: transporterFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Request: req, ContentLength: -1,
				Body: io.NopCloser(strings.NewReader(`{"kid":"cut`))}, nil
		})}
		resp, err := g.do(req)
		if resp != nil {
			t.Errorf("entra=%v: got a response, want none", entra)
		}
		if !errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("entra=%v: err = %v, want context.Canceled", entra, err)
		}
		if _, retry := entraCause(err); retry {
			t.Errorf("entra=%v: err = %v is classed retryable, want final", entra, err)
		}
	}
}

// TestSignTryTimeoutDuringBodyRead covers a per-try deadline that fires while the response body is being
// read and the Sign deadline still runs: the try's deadline error is kept (retryable), not reported as a
// body that broke off.
func TestSignTryTimeoutDuringBodyRead(t *testing.T) {
	kid := "https://" + testHost + "/keys/github-app/" + testVersion
	s, v := setupWith(t, kid, Options{Timeout: 5 * time.Second, MaxRetries: -1}, &fakeCredential{}, nil,
		clientConfig{tryTimeout: 200 * time.Millisecond})
	v.respond = func(_ int, w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"kid":"`+kid+`","value":"`)
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}
	start := time.Now()
	_, err := s.Sign(context.Background(), digestOf("x"))
	if el := time.Since(start); el > 3*time.Second {
		t.Errorf("Sign took %s, want about the 200ms try timeout", el)
	}
	if !providers.IsRetryable(err) || !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, errBodyRead) {
		t.Errorf("err = %v, want a retryable deadline error", err)
	}
	assertCleanChain(t, "try timeout during body read", err)
}

// emptyAfter returns, for each authorized request once armed is set, a response with status and no body
// after calling end (when set) and waiting for the request's context to end: an answer that the HTTP client
// hands back after the try or the caller gave up, as from a server that sees the client leave and returns
// from its handler.
func emptyAfter(armed *atomic.Bool, status int, end func()) func(policy.Transporter) policy.Transporter {
	return func(inner policy.Transporter) policy.Transporter {
		return transporterFunc(func(req *http.Request) (*http.Response, error) {
			if !armed.Load() || req.Header.Get("Authorization") == "" {
				return inner.Do(req)
			}
			if end != nil {
				end()
			}
			<-req.Context().Done()
			return &http.Response{StatusCode: status, Header: http.Header{"Content-Length": {"0"}},
				Body: http.NoBody, Request: req}, nil
		})
	}
}

// TestSignEmptyAnswerAfterDeadline covers a Key Vault response without a body that the HTTP client returns
// after the per-try deadline (#447, the race #438 found in the model client): it is the try's retryable
// deadline, not a final "response does not name the key" (an empty 200) or a final status. When the caller
// cancelled instead, it is a final cancellation. The first Sign learns the challenge under a generous try
// timeout, so the timed try holds only the authorized request.
func TestSignEmptyAnswerAfterDeadline(t *testing.T) {
	kid := "https://" + testHost + "/keys/github-app/" + testVersion
	for _, status := range []int{http.StatusOK, http.StatusBadRequest, http.StatusNotFound} {
		var armed atomic.Bool
		s, v := setupWith(t, kid, Options{Timeout: time.Minute, MaxRetries: -1}, &fakeCredential{},
			emptyAfter(&armed, status, nil), clientConfig{tryTimeout: 30 * time.Second})
		v.respond = v.signOK(kid)
		if _, err := s.Sign(context.Background(), digestOf("x")); err != nil {
			t.Fatalf("%d: first Sign: %v", status, err)
		}
		armed.Store(true)
		_, err := s.Sign(atTryDeadline(), digestOf("x"))
		if !providers.IsRetryable(err) || !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("%d after the try deadline: err = %v, want a retryable deadline error", status, err)
		}
		assertCleanChain(t, "empty answer after the try deadline", err)

		ctx, cancel := context.WithCancel(context.Background())
		s, v = setupWith(t, kid, Options{Timeout: time.Minute, MaxRetries: -1}, &fakeCredential{},
			emptyAfter(&armed, status, cancel), clientConfig{tryTimeout: 30 * time.Second})
		v.respond = v.signOK(kid)
		_, err = s.Sign(ctx, digestOf("x"))
		if !errors.Is(err, context.Canceled) || providers.IsRetryable(err) {
			t.Errorf("%d after a cancel: err = %v, want a non-retryable cancellation", status, err)
		}
		cancel()
	}
}

// guardDo calls guardTransport directly, for Key Vault or Entra ID, with a request whose context is ctx and
// an inner transport that returns resp. It returns the result and what was recorded for the token request.
func guardDo(t *testing.T, ctx context.Context, entra bool, resp *http.Response) (*http.Response, *tokenRequest, error) {
	t.Helper()
	tr := &tokenRequest{}
	req, err := http.NewRequestWithContext(context.WithValue(ctx, tokenRequestKey{}, tr), http.MethodPost,
		"https://"+testHost+"/keys/k/sign", nil)
	if err != nil {
		t.Fatal(err)
	}
	g := guardTransport{entra: entra, inner: transporterFunc(func(req *http.Request) (*http.Response, error) {
		resp.Request = req
		return resp, nil
	})}
	got, err := g.Do(req)
	return got, tr, err
}

// safeTestChallenge is a Key Vault challenge that safeChallenge accepts for testHost.
const safeTestChallenge = `Bearer authorization="https://login.microsoftonline.com/tenant", resource="https://vault.azure.net"`

// TestGuardTransportEmptyAnswerAfterDeadline calls guardTransport directly, for Key Vault and for Entra
// ID, with a response without a body (http.NoBody or a nil Body) of several statuses, including a 401 with
// a challenge the Key Vault check accepts. Returned after the request's deadline, it becomes the deadline
// and, for a token request, is recorded as such, so the credential classes it as a transient deadline and
// not by the status. Returned in time, it passes through unchanged and its status is recorded.
func TestGuardTransportEmptyAnswerAfterDeadline(t *testing.T) {
	cases := []struct {
		name   string
		status int
		header http.Header
		body   io.ReadCloser
	}{
		{"400 NoBody", http.StatusBadRequest, http.Header{}, http.NoBody},
		{"200 nil body", http.StatusOK, http.Header{}, nil},
		{"404 NoBody", http.StatusNotFound, http.Header{}, http.NoBody},
		{"401 safe challenge", http.StatusUnauthorized, http.Header{"Www-Authenticate": {safeTestChallenge}}, http.NoBody},
	}
	for _, c := range cases {
		for _, entra := range []bool{false, true} {
			for _, late := range []bool{true, false} {
				name := fmt.Sprintf("%s entra=%v late=%v", c.name, entra, late)
				ctx, cancel := context.WithCancel(context.Background())
				if late {
					ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
				}
				resp := &http.Response{StatusCode: c.status, Header: c.header, Body: c.body}
				got, tr, err := guardDo(t, ctx, entra, resp)
				cancel()
				if late {
					if got != nil || !errors.Is(err, context.DeadlineExceeded) {
						t.Errorf("%s: resp %v, err = %v; want no response and context.DeadlineExceeded", name, got, err)
					}
					if entra {
						if cause, retry := entraCause(tr.transportErr); cause != context.DeadlineExceeded || !retry || tr.status != 0 {
							t.Errorf("%s: recorded %+v, class %v, %v; want a transient deadline and no status", name, tr, cause, retry)
						}
					}
					continue
				}
				if err != nil || got != resp || got.StatusCode != c.status || got.Body != c.body {
					t.Errorf("%s: resp %+v, err = %v; want the response unchanged", name, got, err)
				}
				if entra && (tr.status != c.status || tr.transportErr != nil) {
					t.Errorf("%s: recorded %+v; want status %d and no error", name, tr, c.status)
				}
			}
		}
	}
}

// TestGuardTransportEmptyAnswerAfterCancel calls guardTransport directly, for Key Vault and for Entra ID,
// with a response without a body returned after the caller cancelled: it returns the cancellation, which
// azcore does not retry, and for a token request records it, so the credential classes it as a final
// cancellation and not by the status (an empty 200 would otherwise be "a successful answer that cannot be
// used", which is transient).
func TestGuardTransportEmptyAnswerAfterCancel(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusServiceUnavailable} {
		for _, entra := range []bool{false, true} {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			got, tr, err := guardDo(t, ctx, entra, &http.Response{StatusCode: status, Header: http.Header{}, Body: http.NoBody})
			if got != nil || !errors.Is(err, context.Canceled) || providers.IsRetryable(err) {
				t.Errorf("%d entra=%v: resp %v, err = %v; want no response and a final context.Canceled", status, entra, got, err)
			}
			if entra {
				if cause, retry := entraCause(tr.transportErr); cause != context.Canceled || retry || tr.status != 0 {
					t.Errorf("%d: recorded %+v, class %v, %v; want a final cancellation and no status", status, tr, cause, retry)
				}
			}
		}
	}
}
