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
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"

	"github.com/pkalin/shadewright/internal/providers"
)

const (
	// testTenant is the tenant of the fake vault's challenge, so the credential accepts it.
	testTenant   = "00000000-0000-0000-0000-000000000000"
	testClientID = "00000000-0000-0000-0000-0000000000c1"
	// testAssertion stands for the projected service account token.
	testAssertion   = "header.payload.signature-of-the-service-account-token"
	entraHost       = "login.microsoftonline.com"
	netTCP          = "tcp"
	tokenTypeBearer = "Bearer"
)

// fakeEntra answers the OpenID configuration and the token requests of a client assertion credential.
// tokenErr fails the token request in the transport; status, when set, is the token endpoint's answer;
// badAnswer makes it a 200 whose body does not parse; cutAnswer makes it a 200 whose body is cut and ends
// cleanly once the request's context ends.
type fakeEntra struct {
	mu         sync.Mutex
	tokenErr   error
	status     int
	badAnswer  bool
	cutAnswer  bool
	assertions []string
}

func (e *fakeEntra) Do(req *http.Request) (*http.Response, error) {
	if req.URL.Host != entraHost {
		return nil, errors.New("unexpected host " + req.URL.Host)
	}
	base := "https://" + entraHost + "/" + testTenant
	switch {
	case strings.HasSuffix(req.URL.Path, "/v2.0/.well-known/openid-configuration"):
		return jsonResponse(req, http.StatusOK, map[string]string{
			"authorization_endpoint": base + "/oauth2/v2.0/authorize",
			"token_endpoint":         base + "/oauth2/v2.0/token",
			"issuer":                 base + "/v2.0",
		}), nil
	case strings.HasSuffix(req.URL.Path, "/oauth2/v2.0/token"):
		body, _ := io.ReadAll(req.Body)
		form, _ := url.ParseQuery(string(body))
		e.mu.Lock()
		defer e.mu.Unlock()
		e.assertions = append(e.assertions, form.Get("client_assertion"))
		if e.tokenErr != nil {
			return nil, e.tokenErr
		}
		if e.badAnswer {
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Request: req,
				Body: io.NopCloser(strings.NewReader("{" + bodySecret))}, nil
		}
		if e.cutAnswer {
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Request: req, ContentLength: -1,
				Body: &cutBody{ctx: req.Context(), data: strings.NewReader(`{"token_type":"Bearer","access_token":"`)}}, nil
		}
		if e.status != 0 {
			return jsonResponse(req, e.status, struct {
				Code string `json:"error"`
				Desc string `json:"error_description"`
			}{"invalid_client", bodySecret}), nil
		}
		return jsonResponse(req, http.StatusOK, map[string]any{
			"token_type": tokenTypeBearer, "expires_in": 3600, "access_token": testToken,
		}), nil
	}
	return nil, errors.New("unexpected path " + req.URL.Path)
}

func jsonResponse(req *http.Request, status int, v any) *http.Response {
	b, _ := json.Marshal(v)
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(string(b))),
		Request:    req,
	}
}

// writeTokenFile writes content as the projected service account token and returns its path.
func writeTokenFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "azure-identity-token")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// realCredential builds the credential New builds, with the HTTP client below guardTransport replaced by
// entra and no retries inside the credential.
func realCredential(t *testing.T, path string, entra *fakeEntra) azcore.TokenCredential {
	t.Helper()
	t.Setenv(envAuthorityHost, "")
	cred, err := newWorkloadCredential(
		workloadIdentity{clientID: testClientID, tenantID: testTenant, tokenFile: path},
		azcore.ClientOptions{Retry: policy.RetryOptions{MaxRetries: -1}}, entra,
	)
	if err != nil {
		t.Fatal(err)
	}
	return cred
}

// TestSignWithTheWorkloadCredential checks that the credential New builds sends the token file's content
// as the client assertion, and that its failures are classed exactly: Entra's answer decides when there is
// one, a token file that cannot be read is final, a token request that gets no answer is transient, a
// certificate of Entra ID the client refuses is final, and a 200 that cannot be used is transient.
func TestSignWithTheWorkloadCredential(t *testing.T) {
	kid := "https://" + testHost + "/keys/github-app"
	dir := t.TempDir()
	network := &url.Error{Op: http.MethodPost, URL: "https://" + entraHost + "/x?sig=" + bodySecret,
		Err: &net.OpError{Op: opDial, Net: netTCP, Err: errors.New("no such host " + bodySecret)}}
	certificate := &url.Error{Op: http.MethodPost, URL: "https://" + entraHost + "/x?sig=" + bodySecret,
		Err: &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}}

	for _, tc := range []struct {
		name      string
		path      string
		entra     *fakeEntra
		ok        bool
		retryable bool
		class     error
	}{
		{name: "token issued", path: writeTokenFile(t, testAssertion), entra: &fakeEntra{}, ok: true},
		{name: "no answer from Entra", path: writeTokenFile(t, testAssertion), entra: &fakeEntra{tokenErr: network},
			retryable: true, class: errEntraNetwork},
		{name: "idle connection closed", path: writeTokenFile(t, testAssertion),
			entra: &fakeEntra{tokenErr: &url.Error{Op: http.MethodPost, URL: "https://" + entraHost + "/x",
				Err: errors.New("http: server closed idle connection")}},
			retryable: true, class: errEntraNetwork},
		{name: "http2 stream reset", path: writeTokenFile(t, testAssertion),
			entra:     &fakeEntra{tokenErr: errors.New("stream error: stream ID 3; INTERNAL_ERROR; received from peer")},
			retryable: true, class: errEntraNetwork},
		{name: "certificate refused", path: writeTokenFile(t, testAssertion), entra: &fakeEntra{tokenErr: certificate},
			class: errEntraTLS},
		{name: "answer does not parse", path: writeTokenFile(t, testAssertion), entra: &fakeEntra{badAnswer: true},
			retryable: true, class: errEntraAnswer},
		{name: "Entra refuses", path: writeTokenFile(t, testAssertion), entra: &fakeEntra{status: http.StatusBadRequest}},
		{name: "Entra throttles", path: writeTokenFile(t, testAssertion),
			entra: &fakeEntra{status: http.StatusTooManyRequests}, retryable: true},
		{name: "file missing", path: filepath.Join(dir, "missing"), entra: &fakeEntra{tokenErr: network},
			class: errTokenFileMissing},
		{name: "file is a directory", path: dir, entra: &fakeEntra{tokenErr: network}, class: errTokenFileNotRegular},
		{name: "file too large", path: writeTokenFile(t, strings.Repeat("a", maxTokenFile+1)),
			entra: &fakeEntra{tokenErr: network}, class: errTokenFileTooLarge},
		{name: "file empty", path: writeTokenFile(t, ""), entra: &fakeEntra{tokenErr: network}, class: errTokenFileEmpty},
	} {
		s, v := setupWith(t, kid, Options{MaxRetries: -1}, realCredential(t, tc.path, tc.entra), nil, clientConfig{})
		v.respond = v.signOK(kid + "/" + testVersion)
		_, err := s.Sign(context.Background(), digestOf("x"))
		if tc.ok {
			if err != nil {
				t.Errorf("%s: %v", tc.name, err)
			} else if got := tc.entra.assertions; len(got) != 1 || got[0] != testAssertion {
				t.Errorf("%s: client assertions %q, want the token file's content once", tc.name, got)
			}
			continue
		}
		assertCleanChain(t, tc.name, err)
		if providers.IsRetryable(err) != tc.retryable {
			t.Errorf("%s: retryable = %v, want %v: %v", tc.name, providers.IsRetryable(err), tc.retryable, err)
		}
		if tc.class != nil && !errors.Is(err, tc.class) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.class)
		}
		if tc.class != nil && strings.Contains(tc.class.Error(), "token file") && len(tc.entra.assertions) != 0 {
			t.Errorf("%s: a token request was sent without an assertion", tc.name)
		}
	}
}

// TestTokenRequestTryTimeoutWithCleanlyEndedBody covers a token request whose answer is cut by the per-try
// deadline of the Key Vault request and still ends cleanly: guardTransport on the credential's client
// returns the deadline, so the failure is a retryable timeout, not an answer that could not be used or a
// client failure.
func TestTokenRequestTryTimeoutWithCleanlyEndedBody(t *testing.T) {
	kid := "https://" + testHost + "/keys/github-app"
	entra := &fakeEntra{cutAnswer: true}
	s, v := setupWith(t, kid, Options{Timeout: 30 * time.Second, MaxRetries: -1},
		realCredential(t, writeTokenFile(t, testAssertion), entra), nil, clientConfig{tryTimeout: 50 * time.Millisecond})
	v.respond = v.signOK(kid + "/" + testVersion)
	_, err := s.Sign(context.Background(), digestOf("x"))
	if !providers.IsRetryable(err) || !errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, errEntraAnswer) || errors.Is(err, errEntraClient) {
		t.Errorf("err = %v, want a retryable deadline error", err)
	}
	assertCleanChain(t, "token request cut at the try deadline", err)
	if len(entra.assertions) != 1 {
		t.Errorf("%d token requests, want 1", len(entra.assertions))
	}
}

// TestTokenFileIsReused checks that the token file is read once and reused, as WorkloadIdentityCredential
// does, so a token file removed after a good read does not fail the next token request.
func TestTokenFileIsReused(t *testing.T) {
	path := writeTokenFile(t, testAssertion)
	tf := &tokenFile{path: path}
	if _, err := tf.assertion(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	got, err := tf.assertion(context.Background())
	if err != nil || got != testAssertion {
		t.Errorf("second read = %q, %v; want the reused token", got, err)
	}
	tf.expiresAt = time.Time{}
	tr := &tokenRequest{}
	ctx := context.WithValue(context.Background(), tokenRequestKey{}, tr)
	if _, err := tf.assertion(ctx); !errors.Is(err, errTokenFileMissing) {
		t.Errorf("read after expiry = %v, want the missing error", err)
	}
	if !tr.read || !errors.Is(tr.readErr, errTokenFileMissing) {
		t.Errorf("recorded = %+v, want the missing error", tr)
	}

	// A later good read in the same token request replaces the failure.
	if err := os.WriteFile(path, []byte(testAssertion), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := tf.assertion(ctx); err != nil {
		t.Fatal(err)
	}
	if !tr.read || tr.readErr != nil {
		t.Errorf("recorded = %+v after a good read, want no error", tr)
	}
}

// TestTokenFileMissingForAMoment checks that a token file that appears within the retry delay, as during
// the kubelet's swap of the ..data link, is read, and that a caller who gives up during the wait gets its
// context error, recorded in place of an earlier exchange.
func TestTokenFileMissingForAMoment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "azure-identity-token")
	// The file appears during the wait before the one retry. Writing it from the wait itself, instead of from a
	// timer racing the retry delay, keeps the test independent of the scheduler (it failed now and then under
	// -race on a loaded machine).
	wait := tokenFileRetryWait
	t.Cleanup(func() { tokenFileRetryWait = wait })
	tokenFileRetryWait = func(d time.Duration) <-chan time.Time {
		if err := os.WriteFile(path, []byte(testAssertion), 0o600); err != nil {
			t.Error(err)
		}
		return wait(d)
	}
	tr := &tokenRequest{}
	got, err := (&tokenFile{path: path}).assertion(context.WithValue(context.Background(), tokenRequestKey{}, tr))
	if err != nil || got != testAssertion || tr.readErr != nil {
		t.Errorf("assertion = %q, %v, recorded %+v; want the token", got, err, tr)
	}

	// An earlier successful exchange must not decide the class once the deadline ends the wait.
	tr.recordExchange(http.StatusOK, nil)
	ctx, cancel := context.WithTimeout(context.WithValue(context.Background(), tokenRequestKey{}, tr),
		tokenFileRetryDelay/5)
	defer cancel()
	_, err = (&tokenFile{path: filepath.Join(t.TempDir(), "missing")}).assertion(ctx)
	if !errors.Is(err, context.DeadlineExceeded) || tr.read || !errors.Is(tr.transportErr, context.DeadlineExceeded) {
		t.Errorf("wait cut by the deadline: err = %v, recorded %+v; want the deadline recorded", err, tr)
	}
	if cause, retry := entraCause(tr.transportErr); cause != context.DeadlineExceeded || !retry {
		t.Errorf("class = %v, %v; want a transient deadline", cause, retry)
	}
}

// TestCredentialWeCannotSeeIntoIsFinal checks that a token failure without a response from a credential
// this package did not build (no file read and no exchange recorded) is final: it cannot be told apart from
// a configuration error.
func TestCredentialWeCannotSeeIntoIsFinal(t *testing.T) {
	kid := "https://" + testHost + "/keys/github-app"
	cred := &fakeCredential{err: &azidentity.AuthenticationFailedError{}}
	s, v := setupWith(t, kid, Options{MaxRetries: -1}, cred, nil, clientConfig{})
	v.respond = v.signOK(kid + "/" + testVersion)
	_, err := s.Sign(context.Background(), digestOf("x"))
	if err == nil || providers.IsRetryable(err) {
		t.Errorf("err = %v, want a final token request failure", err)
	}
	assertCleanChain(t, "foreign credential", err)
}

// answerThenCancel cancels the caller's context and then returns Entra's answer with status.
type answerThenCancel struct {
	cancel context.CancelFunc
	status int
}

func (a answerThenCancel) GetToken(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
	a.cancel()
	return azcore.AccessToken{}, authFailure(a.status, "")
}

// TestCredentialCancelBeatsAnswer checks that a caller who cancelled gets a final cancellation from the
// credential even when Entra answered, also with a status that would be retryable.
func TestCredentialCancelBeatsAnswer(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusServiceUnavailable} {
		ctx, cancel := context.WithCancel(context.Background())
		_, err := cleanCredential{inner: answerThenCancel{cancel: cancel, status: status}}.GetToken(ctx,
			policy.TokenRequestOptions{Scopes: []string{testScope}})
		ce, ok := errors.AsType[*credentialError](err)
		if !ok || !errors.Is(err, context.Canceled) || ce.transient() {
			t.Errorf("%d: err = %v, want a final cancellation", status, err)
		}
	}
}

// lateRefusal waits for the context to end and then returns Entra's refusal, as when a 400 arrives just as
// the deadline passes.
type lateRefusal struct{ status int }

func (l lateRefusal) GetToken(ctx context.Context, _ policy.TokenRequestOptions) (azcore.AccessToken, error) {
	<-ctx.Done()
	return azcore.AccessToken{}, authFailure(l.status, "")
}

// TestSignCredentialAnswerBeatsDeadline checks that Entra's answer decides the class even when the
// deadline has passed by the time it arrives: a refusal stays final, throttling stays retryable.
func TestSignCredentialAnswerBeatsDeadline(t *testing.T) {
	kid := "https://" + testHost + "/keys/github-app"
	for status, retryable := range map[int]bool{
		http.StatusBadRequest:      false,
		http.StatusUnauthorized:    false,
		http.StatusTooManyRequests: true,
	} {
		s, v := setupWith(t, kid, Options{Timeout: 5 * time.Second, MaxRetries: -1}, lateRefusal{status: status}, nil,
			clientConfig{tryTimeout: 50 * time.Millisecond})
		v.respond = v.signOK(kid + "/" + testVersion)
		_, err := s.Sign(context.Background(), digestOf("x"))
		if providers.IsRetryable(err) != retryable || errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("%d: err = %v, want retryable %v and no deadline error", status, err, retryable)
		}
		assertCleanChain(t, http.StatusText(status), err)
	}
}
