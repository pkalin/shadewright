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
	"io"
	"net/http"
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
