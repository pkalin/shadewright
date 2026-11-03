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
// tokenErr fails the token request in the network; status, when set, is the token endpoint's answer.
type fakeEntra struct {
	mu         sync.Mutex
	tokenErr   error
	status     int
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

// realCredential builds the credential New builds, with Entra replaced by entra and no retries inside the
// credential.
func realCredential(t *testing.T, path string, entra *fakeEntra) azcore.TokenCredential {
	t.Helper()
	t.Setenv(envAuthorityHost, "")
	cred, err := newWorkloadCredential(
		workloadIdentity{clientID: testClientID, tenantID: testTenant, tokenFile: path},
		azcore.ClientOptions{Transport: entra, Retry: policy.RetryOptions{MaxRetries: -1}},
	)
	if err != nil {
		t.Fatal(err)
	}
	return cred
}

// TestSignWithTheWorkloadCredential checks that the credential New builds sends the token file's content
// as the client assertion, and that its failures are classed exactly: Entra's answer decides when there is
// one, a token file that cannot be read is final, and a token request that gets no answer after the file was
// read is transient.
func TestSignWithTheWorkloadCredential(t *testing.T) {
	kid := "https://" + testHost + "/keys/github-app"
	dir := t.TempDir()
	network := &url.Error{Op: http.MethodPost, URL: "https://" + entraHost + "/x?sig=" + bodySecret,
		Err: &net.OpError{Op: opDial, Net: netTCP, Err: errors.New("no such host " + bodySecret)}}

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
			retryable: true},
		{name: "Entra refuses", path: writeTokenFile(t, testAssertion), entra: &fakeEntra{status: http.StatusBadRequest}},
		{name: "Entra throttles", path: writeTokenFile(t, testAssertion),
			entra: &fakeEntra{status: http.StatusTooManyRequests}, retryable: true},
		{name: "file missing", path: filepath.Join(dir, "missing"), entra: &fakeEntra{tokenErr: network},
			class: errTokenFileUnreadable},
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
		if tc.class != nil && len(tc.entra.assertions) != 0 {
			t.Errorf("%s: a token request was sent without an assertion", tc.name)
		}
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
	read := &tokenFileRead{}
	if _, err := tf.assertion(context.WithValue(context.Background(), tokenFileReadKey{}, read)); !errors.Is(err, errTokenFileUnreadable) {
		t.Errorf("read after expiry = %v, want the unreadable error", err)
	}
	if !errors.Is(read.err(), errTokenFileUnreadable) {
		t.Errorf("recorded error = %v, want the unreadable error", read.err())
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
