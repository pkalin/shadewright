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
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"

	"github.com/pkalin/shadewright/internal/providers"
)

// unsafeTestChallenge is a Key Vault challenge that safeChallenge refuses: the SDK would panic on it.
const unsafeTestChallenge = `Bearer authorization="not-a-url", resource="https://vault.azure.net"`

// challengeHeader is the canonical name of the header that carries a Key Vault challenge.
const challengeHeader = "Www-Authenticate"

// oversized is a response body one byte over maxResponseBody.
var oversized = strings.Repeat("a", maxResponseBody+1)

// lateAnswer is an answer that guardTransport classifies by itself, before or while it reads the body.
type lateAnswer struct {
	name string
	// make builds the response for req.
	make func(req *http.Request) *http.Response
	// refusal is the final error guardTransport gives this answer when it arrives in time.
	refusal error
}

func lateAnswers() []lateAnswer {
	return []lateAnswer{
		{"401 unsafe challenge", func(req *http.Request) *http.Response {
			return &http.Response{StatusCode: http.StatusUnauthorized, Request: req,
				Header: http.Header{challengeHeader: {unsafeTestChallenge}}, Body: http.NoBody}
		}, errMalformedChallenge},
		{"200 declared oversized", func(req *http.Request) *http.Response {
			return &http.Response{StatusCode: http.StatusOK, Request: req, ContentLength: int64(len(oversized)),
				Header: http.Header{"Content-Length": {strconv.Itoa(len(oversized))}},
				Body:   io.NopCloser(strings.NewReader(oversized))}
		}, errResponseTooLarge},
		{"200 chunked oversized", func(req *http.Request) *http.Response {
			return &http.Response{StatusCode: http.StatusOK, Request: req, ContentLength: -1, Header: http.Header{},
				Body: io.NopCloser(strings.NewReader(oversized))}
		}, errResponseTooLarge},
	}
}

// TestGuardTransportAnswerAfterContextEnd calls guardTransport directly, for Key Vault and for Entra ID,
// with an answer it would refuse by itself (an unsafe challenge, an oversized body) returned after the
// request's context ended (#781): it reports that end, never the refusal, and records it for a token
// request. The unsafe challenge is checked on Key Vault only, so on Entra ID that 401 passes through when
// it arrives in time. No clock decides: the context has ended before the answer is handed over. The same
// answers in time, and an oversized body read across the context's end, keep their final refusal.
func TestGuardTransportAnswerAfterContextEnd(t *testing.T) {
	assertAnswerInTimeIsRefused(t)
	assertOversizedBodyReadAcrossContextEndIsFinal(t)
	ends := map[string]struct {
		ctx  func() (context.Context, context.CancelFunc)
		want error
	}{
		"deadline": {func() (context.Context, context.CancelFunc) {
			return context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		}, context.DeadlineExceeded},
		"cancel": {func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx, cancel
		}, context.Canceled},
	}
	for _, a := range lateAnswers() {
		for endName, end := range ends {
			for _, entra := range []bool{false, true} {
				name := fmt.Sprintf("%s after %s entra=%v", a.name, endName, entra)
				ctx, cancel := end.ctx()
				got, tr, err := guardDo(t, ctx, entra, a.make(nil))
				cancel()
				if got != nil || !errors.Is(err, end.want) || errors.Is(err, a.refusal) {
					t.Errorf("%s: resp %v, err = %v; want no response and %v", name, got, err, end.want)
				}
				if entra {
					cause, _ := entraCause(tr.transportErr)
					if cause != end.want || tr.status != 0 {
						t.Errorf("%s: recorded %+v, class %v; want %v and no status", name, tr, cause, end.want)
					}
				}
			}
		}
	}
}

// assertAnswerInTimeIsRefused is the other side of TestGuardTransportAnswerAfterContextEnd: the same
// answers returned while the context lives keep their final refusal.
func assertAnswerInTimeIsRefused(t *testing.T) {
	t.Helper()
	for _, a := range lateAnswers() {
		for _, entra := range []bool{false, true} {
			if entra && a.refusal == errMalformedChallenge {
				continue // Entra ID sends no Key Vault challenge, so none is checked.
			}
			got, _, err := guardDo(t, context.Background(), entra, a.make(nil))
			var f final
			if got != nil || !errors.Is(err, a.refusal) || !errors.As(err, &f) {
				t.Errorf("%s entra=%v: resp %v, err = %v; want no response and final %v", a.name, entra, got, err, a.refusal)
			}
		}
	}
}

// endingBody ends the request's context on its first read and then reads as data does: a body whose read
// is still running when the context ends.
type endingBody struct {
	end  func()
	data io.Reader
}

func (b *endingBody) Read(p []byte) (int, error) {
	if b.end != nil {
		b.end()
		b.end = nil
	}
	return b.data.Read(p)
}

func (*endingBody) Close() error { return nil }

// assertOversizedBodyReadAcrossContextEndIsFinal covers the case the check after the answer leaves alone:
// the answer arrived while the context lived and its oversized body was read across the context's end. The
// answer is real and its size is known, so it stays the final response too large error (#206).
func assertOversizedBodyReadAcrossContextEndIsFinal(t *testing.T) {
	t.Helper()
	for _, entra := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		resp := &http.Response{StatusCode: http.StatusOK, ContentLength: -1, Header: http.Header{},
			Body: &endingBody{end: cancel, data: strings.NewReader(oversized)}}
		got, _, err := guardDo(t, ctx, entra, resp)
		if ctx.Err() == nil {
			t.Fatalf("entra=%v: the body read did not end the context", entra)
		}
		cancel()
		var f final
		if got != nil || !errors.Is(err, errResponseTooLarge) || !errors.As(err, &f) {
			t.Errorf("entra=%v: resp %v, err = %v; want the final response too large error", entra, got, err)
		}
	}
}

// answerAfterEnd returns, for each authorized request once armed is set, the answer of a, after calling
// end (when set) and waiting for the request's context to end. It never reaches the fake vault.
func answerAfterEnd(armed *atomic.Bool, a lateAnswer, end func()) func(policy.Transporter) policy.Transporter {
	return func(inner policy.Transporter) policy.Transporter {
		return transporterFunc(func(req *http.Request) (*http.Response, error) {
			if !armed.Load() || req.Header.Get("Authorization") == "" {
				return inner.Do(req)
			}
			if end != nil {
				end()
			}
			<-req.Context().Done()
			return a.make(req), nil
		})
	}
}

// TestSignAnswerAfterContextEnd covers, through Sign, an answer that guardTransport would refuse returned
// after the context ended (#781): after the per-try deadline it is the try's retryable deadline, after
// the caller's cancellation a final cancellation, and never the final refusal of the answer. The first Sign
// learns the challenge under a generous try timeout, so the timed try holds only the authorized request,
// and the answer waits for the context's end, so no clock races it.
func TestSignAnswerAfterContextEnd(t *testing.T) {
	kid := "https://" + testHost + "/keys/github-app/" + testVersion
	for _, a := range lateAnswers() {
		var armed atomic.Bool
		s, v := setupWith(t, kid, Options{Timeout: time.Minute, MaxRetries: -1}, &fakeCredential{},
			answerAfterEnd(&armed, a, nil), clientConfig{tryTimeout: 30 * time.Second})
		v.respond = v.signOK(kid)
		if _, err := s.Sign(context.Background(), digestOf("x")); err != nil {
			t.Fatalf("%s: first Sign: %v", a.name, err)
		}
		armed.Store(true)
		_, err := s.Sign(atTryDeadline(), digestOf("x"))
		if !providers.IsRetryable(err) || !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, a.refusal) {
			t.Errorf("%s after the try deadline: err = %v, want a retryable deadline error", a.name, err)
		}
		assertCleanChain(t, a.name+" after the try deadline", err)

		ctx, cancel := context.WithCancel(context.Background())
		armed.Store(false)
		s, v = setupWith(t, kid, Options{Timeout: time.Minute, MaxRetries: -1}, &fakeCredential{},
			answerAfterEnd(&armed, a, cancel), clientConfig{tryTimeout: 30 * time.Second})
		v.respond = v.signOK(kid)
		if _, err := s.Sign(context.Background(), digestOf("x")); err != nil {
			t.Fatalf("%s: first Sign: %v", a.name, err)
		}
		armed.Store(true)
		_, err = s.Sign(ctx, digestOf("x"))
		if !errors.Is(err, context.Canceled) || providers.IsRetryable(err) || errors.Is(err, a.refusal) {
			t.Errorf("%s after a cancel: err = %v, want a non-retryable cancellation", a.name, err)
		}
		cancel()
	}
}
