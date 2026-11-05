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
	"sync/atomic"
	"testing"
	"time"

	"github.com/pkalin/shadewright/internal/providers"
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
	started := make(chan struct{})
	w, _, _ := entraHarness(t, func(rw http.ResponseWriter, r *http.Request) {
		close(started)
		_, _ = io.Copy(io.Discard, r.Body) // the server sees the client go away only after the body is read
		<-r.Context().Done()
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-started; cancel() }()
	_, err := w.Token(ctx)
	if !errors.Is(err, context.Canceled) || providers.IsRetryable(err) {
		t.Fatalf("err = %v, want canceled", err)
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
	w, _, _ := entraHarness(t, func(http.ResponseWriter, *http.Request) {})
	w.lock <- struct{}{} // another exchange in progress
	defer func() { <-w.lock }()
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
