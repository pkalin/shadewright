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
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	azlog "github.com/Azure/azure-sdk-for-go/sdk/azcore/log"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"

	"github.com/pkalin/shadewright/internal/providers"
)

const (
	testToken = "test-access-token-must-never-appear"
	testHost  = "example.vault.azure.net"
	testScope = "https://vault.azure.net/.default"
	// bodySecret stands for anything in a Key Vault response body; it must never reach an error.
	bodySecret = "response-body-text-must-never-appear"
	kidField   = "kid"
	valueField = "value"
)

// fakeCredential hands out a fixed token and records the scopes asked for.
type fakeCredential struct {
	mu     sync.Mutex
	scopes []string
	err    error
}

func (c *fakeCredential) GetToken(_ context.Context, o policy.TokenRequestOptions) (azcore.AccessToken, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.scopes = append(c.scopes, o.Scopes...)
	if c.err != nil {
		return azcore.AccessToken{}, c.err
	}
	return azcore.AccessToken{Token: testToken, ExpiresOn: time.Now().Add(time.Hour)}, nil
}

// redirect sends every request to the test server while leaving the URL the SDK built (and so its host
// checks) unchanged; the original host travels in the Host header.
type redirect struct {
	target *url.URL
	client *http.Client
}

func (r redirect) Do(req *http.Request) (*http.Response, error) {
	out := req.Clone(req.Context())
	out.URL.Scheme = r.target.Scheme
	out.URL.Host = r.target.Host
	out.Host = req.URL.Host
	out.RequestURI = ""
	return r.client.Transport.RoundTrip(out)
}

// seen is one request as the fake Key Vault received it.
type seen struct {
	method, host, path, apiVersion, auth string
	body                                 []byte
}

// fakeVault is a Key Vault keys endpoint. It answers the first unauthenticated request with a challenge,
// then calls respond for every authorized request.
type fakeVault struct {
	// challenge, when set, replaces the WWW-Authenticate header of the challenge response.
	challenge string
	key       *rsa.PrivateKey
	mu        sync.Mutex
	reqs      []seen
	respond   func(n int, w http.ResponseWriter, r *http.Request, body []byte)
}

func (v *fakeVault) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	v.mu.Lock()
	v.reqs = append(v.reqs, seen{
		method: r.Method, host: r.Host, path: r.URL.Path, apiVersion: r.URL.Query().Get("api-version"),
		auth: r.Header.Get("Authorization"), body: body,
	})
	n := 0
	for _, s := range v.reqs {
		if s.auth != "" {
			n++
		}
	}
	v.mu.Unlock()
	if r.Header.Get("Authorization") == "" {
		challenge := `Bearer authorization="https://login.microsoftonline.com/00000000-0000-0000-0000-000000000000", ` +
			`resource="https://vault.azure.net"`
		if v.challenge != "" {
			challenge = v.challenge
		}
		w.Header().Set("WWW-Authenticate", challenge)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	v.respond(n, w, r, body)
}

func (v *fakeVault) authorized() []seen {
	v.mu.Lock()
	defer v.mu.Unlock()
	var out []seen
	for _, s := range v.reqs {
		if s.auth != "" {
			out = append(out, s)
		}
	}
	return out
}

// signOK signs the digest in the request with the test key, as Key Vault would.
func (v *fakeVault) signOK(kid string) func(int, http.ResponseWriter, *http.Request, []byte) {
	return func(_ int, w http.ResponseWriter, _ *http.Request, body []byte) {
		var p struct {
			Alg   string `json:"alg"`
			Value string `json:"value"`
		}
		if err := json.Unmarshal(body, &p); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		digest, err := base64.RawURLEncoding.DecodeString(p.Value)
		if err != nil {
			http.Error(w, "bad value", http.StatusBadRequest)
			return
		}
		sig, err := rsa.SignPKCS1v15(nil, v.key, crypto.SHA256, digest)
		if err != nil {
			http.Error(w, "sign failed", http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{kidField: kid, valueField: base64.RawURLEncoding.EncodeToString(sig)})
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": bodySecret}})
}

var testRSAKey = sync.OnceValue(func() *rsa.PrivateKey {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return k
})

// setup starts a fake vault and returns a Signer for keyID that talks to it.
func setup(t *testing.T, keyID string, opts Options) (*Signer, *fakeVault, *fakeCredential) {
	t.Helper()
	v := &fakeVault{key: testRSAKey()}
	srv := httptest.NewTLSServer(v)
	t.Cleanup(srv.Close)
	target, _ := url.Parse(srv.URL)
	key, err := ParseKeyID(keyID)
	if err != nil {
		t.Fatal(err)
	}
	cred := &fakeCredential{}
	s, err := newSigner(key, cred, opts, clientConfig{
		transport:  redirect{target: target, client: srv.Client()},
		retryDelay: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	return s, v, cred
}

func digestOf(s string) []byte {
	d := sha256.Sum256([]byte(s))
	return d[:]
}

func TestSignRequestShape(t *testing.T) {
	for _, tc := range []struct {
		name, keyID, wantPath string
	}{
		{"pinned version", "https://" + testHost + "/keys/github-app/" + testVersion,
			"/keys/github-app/" + testVersion + "/sign"},
		{"current version", "https://" + testHost + "/keys/github-app", "/keys/github-app/sign"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, v, cred := setup(t, tc.keyID, Options{})
			v.respond = v.signOK("https://" + testHost + "/keys/github-app/" + testVersion)
			digest := digestOf("header.payload")

			if s.Algorithm() != providers.RS256 {
				t.Errorf("Algorithm() = %q", s.Algorithm())
			}
			sig, err := s.Sign(context.Background(), digest)
			if err != nil {
				t.Fatalf("Sign: %v", err)
			}
			if err := rsa.VerifyPKCS1v15(&v.key.PublicKey, crypto.SHA256, digest, sig); err != nil {
				t.Errorf("signature does not verify: %v", err)
			}

			got := v.authorized()
			if len(got) != 1 {
				t.Fatalf("authorized requests = %d, want 1", len(got))
			}
			r := got[0]
			// Only the sign operation, on exactly the configured key, in the configured vault.
			if r.method != http.MethodPost || r.path != tc.wantPath {
				t.Errorf("request = %s %s, want POST %s", r.method, r.path, tc.wantPath)
			}
			if r.host != testHost {
				t.Errorf("Host = %q, want %q", r.host, testHost)
			}
			if r.apiVersion == "" {
				t.Error("no api-version")
			}
			if r.auth != "Bearer "+testToken {
				t.Errorf("Authorization = %q", r.auth)
			}
			var body map[string]string
			if err := json.Unmarshal(r.body, &body); err != nil {
				t.Fatalf("body: %v", err)
			}
			if body["alg"] != "RS256" {
				t.Errorf("alg = %q, want RS256", body["alg"])
			}
			if body["value"] != base64.RawURLEncoding.EncodeToString(digest) {
				t.Errorf("value = %q, want the digest", body["value"])
			}
			if len(body) != 2 {
				t.Errorf("body has extra fields: %v", body)
			}
			if len(cred.scopes) == 0 || cred.scopes[0] != testScope {
				t.Errorf("token scopes = %v, want %s", cred.scopes, testScope)
			}
		})
	}
}

func TestSignRejectsBadDigest(t *testing.T) {
	s, v, _ := setup(t, "https://"+testHost+"/keys/github-app", Options{})
	v.respond = v.signOK("https://" + testHost + "/keys/github-app/" + testVersion)
	for _, d := range [][]byte{nil, make([]byte, 20), make([]byte, 64)} {
		if _, err := s.Sign(context.Background(), d); err == nil {
			t.Errorf("Sign accepted a %d-byte digest", len(d))
		}
	}
	if n := len(v.reqs); n != 0 {
		t.Errorf("requests = %d, want none for an invalid digest", n)
	}
}

func TestSignRetriesThrottling(t *testing.T) {
	s, v, _ := setup(t, "https://"+testHost+"/keys/github-app", Options{MaxRetries: 3})
	ok := v.signOK("https://" + testHost + "/keys/github-app/" + testVersion)
	v.respond = func(n int, w http.ResponseWriter, r *http.Request, body []byte) {
		if n <= 2 {
			writeError(w, http.StatusTooManyRequests, "Throttled")
			return
		}
		ok(n, w, r, body)
	}
	if _, err := s.Sign(context.Background(), digestOf("x")); err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if n := len(v.authorized()); n != 3 {
		t.Errorf("authorized requests = %d, want 3", n)
	}
}

func TestSignRetriesAreBounded(t *testing.T) {
	s, v, _ := setup(t, "https://"+testHost+"/keys/github-app", Options{MaxRetries: 2})
	v.respond = func(_ int, w http.ResponseWriter, _ *http.Request, _ []byte) {
		writeError(w, http.StatusTooManyRequests, "Throttled")
	}
	_, err := s.Sign(context.Background(), digestOf("x"))
	if !providers.IsRetryable(err) {
		t.Fatalf("err = %v, want retryable", err)
	}
	if n := len(v.authorized()); n != 3 {
		t.Errorf("authorized requests = %d, want 3 (one try and two retries)", n)
	}
}

func TestSignKeepsLongRetryAfter(t *testing.T) {
	s, v, _ := setup(t, "https://"+testHost+"/keys/github-app", Options{})
	v.respond = func(_ int, w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.Header().Set("Retry-After", "120")
		writeError(w, http.StatusTooManyRequests, "Throttled")
	}
	_, err := s.Sign(context.Background(), digestOf("x"))
	if d, ok := providers.RetryAfter(err); !ok || d != 120*time.Second {
		t.Errorf("RetryAfter = %v, %v; err = %v", d, ok, err)
	}
	if n := len(v.authorized()); n != 1 {
		t.Errorf("authorized requests = %d, want 1: a delay beyond the cap is left to the caller", n)
	}
}

func TestSignErrorMapping(t *testing.T) {
	for _, tc := range []struct {
		status              int
		code                string
		notFound, retryable bool
	}{
		{http.StatusNotFound, "KeyNotFound", true, false},
		{http.StatusForbidden, "Forbidden", false, false},
		{http.StatusUnauthorized, "Unauthorized", false, false},
		{http.StatusBadRequest, "BadParameter", false, false},
		{http.StatusConflict, "Conflict", false, false},
		{http.StatusServiceUnavailable, "ServiceUnavailable", false, true},
		{http.StatusInternalServerError, "<script>" + bodySecret, false, true},
	} {
		s, v, _ := setup(t, "https://"+testHost+"/keys/github-app", Options{MaxRetries: -1})
		v.respond = func(_ int, w http.ResponseWriter, _ *http.Request, _ []byte) { writeError(w, tc.status, tc.code) }
		_, err := s.Sign(context.Background(), digestOf("x"))
		if err == nil {
			t.Errorf("%d: no error", tc.status)
			continue
		}
		if providers.IsNotFound(err) != tc.notFound || providers.IsRetryable(err) != tc.retryable ||
			providers.IsConflict(err) {
			t.Errorf("%d: misclassified: %v", tc.status, err)
		}
		assertClean(t, err)
		if !strings.Contains(err.Error(), "github-app") {
			t.Errorf("%d: error does not name the key: %v", tc.status, err)
		}
	}
}

func TestSignTimeout(t *testing.T) {
	s, v, _ := setup(t, "https://"+testHost+"/keys/github-app", Options{Timeout: 200 * time.Millisecond})
	v.respond = func(_ int, w http.ResponseWriter, r *http.Request, _ []byte) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
		writeError(w, http.StatusServiceUnavailable, "Late")
	}
	start := time.Now()
	_, err := s.Sign(context.Background(), digestOf("x"))
	if el := time.Since(start); el > 3*time.Second {
		t.Errorf("Sign took %s, want about the 200ms timeout", el)
	}
	if !providers.IsRetryable(err) || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want a retryable deadline error", err)
	}
	assertClean(t, err)
}

func TestSignCallerCancel(t *testing.T) {
	s, v, _ := setup(t, "https://"+testHost+"/keys/github-app", Options{})
	v.respond = v.signOK("https://" + testHost + "/keys/github-app/" + testVersion)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := s.Sign(ctx, digestOf("x"))
	if !errors.Is(err, context.Canceled) || providers.IsRetryable(err) {
		t.Errorf("err = %v, want a non-retryable cancellation", err)
	}
}

func TestSignValidatesResponse(t *testing.T) {
	for name, respond := range map[string]func(int, http.ResponseWriter, *http.Request, []byte){
		"another key": func(n int, w http.ResponseWriter, r *http.Request, b []byte) {
			(&fakeVault{key: testRSAKey()}).signOK("https://"+testHost+"/keys/another/"+testVersion)(n, w, r, b)
		},
		"another vault": func(n int, w http.ResponseWriter, r *http.Request, b []byte) {
			(&fakeVault{key: testRSAKey()}).signOK("https://other.vault.azure.net/keys/github-app/"+testVersion)(n, w, r, b)
		},
		"short signature": func(_ int, w http.ResponseWriter, _ *http.Request, _ []byte) {
			writeJSON(w, http.StatusOK, map[string]string{
				kidField:   "https://" + testHost + "/keys/github-app/" + testVersion,
				valueField: base64.RawURLEncoding.EncodeToString([]byte("short")),
			})
		},
		"missing kid": func(_ int, w http.ResponseWriter, _ *http.Request, _ []byte) {
			writeJSON(w, http.StatusOK, map[string]string{valueField: base64.RawURLEncoding.EncodeToString(make([]byte, 256))})
		},
		"null kid": func(_ int, w http.ResponseWriter, _ *http.Request, _ []byte) {
			writeJSON(w, http.StatusOK, map[string]any{
				kidField: nil, valueField: base64.RawURLEncoding.EncodeToString(make([]byte, 256)),
			})
		},
		"empty kid": func(_ int, w http.ResponseWriter, _ *http.Request, _ []byte) {
			writeJSON(w, http.StatusOK, map[string]string{
				kidField: "", valueField: base64.RawURLEncoding.EncodeToString(make([]byte, 256)),
			})
		},
		"no signature": func(_ int, w http.ResponseWriter, _ *http.Request, _ []byte) {
			writeJSON(w, http.StatusOK, map[string]string{kidField: "https://" + testHost + "/keys/github-app/" + testVersion})
		},
	} {
		s, v, _ := setup(t, "https://"+testHost+"/keys/github-app/"+testVersion, Options{})
		v.respond = respond
		if _, err := s.Sign(context.Background(), digestOf("x")); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

// authFailure builds the error azidentity returns when Entra answers a token request with status.
func authFailure(status int, retryAfter string) error {
	h := http.Header{}
	if retryAfter != "" {
		h.Set("Retry-After", retryAfter)
	}
	req, _ := http.NewRequest(http.MethodPost, "https://login.microsoftonline.com/example/oauth2/v2.0/token", nil)
	return &azidentity.AuthenticationFailedError{RawResponse: &http.Response{
		StatusCode: status,
		Header:     h,
		Request:    req,
		Body:       io.NopCloser(strings.NewReader(bodySecret)),
	}}
}

func TestSignCredentialFailure(t *testing.T) {
	for _, tc := range []struct {
		status     int
		retryAfter string
		retryable  bool
		wantDelay  time.Duration
	}{
		{http.StatusBadRequest, "", false, 0},
		{http.StatusUnauthorized, "", false, 0},
		{http.StatusTooManyRequests, "30", true, 30 * time.Second},
		{http.StatusServiceUnavailable, "5", true, 5 * time.Second},
		{http.StatusInternalServerError, "", true, 0},
	} {
		s, v, cred := setup(t, "https://"+testHost+"/keys/github-app", Options{MaxRetries: -1})
		v.respond = v.signOK("https://" + testHost + "/keys/github-app/" + testVersion)
		cred.err = authFailure(tc.status, tc.retryAfter)
		_, err := s.Sign(context.Background(), digestOf("x"))
		if err == nil || !strings.Contains(err.Error(), "status "+strconv.Itoa(tc.status)) {
			t.Errorf("%d: err = %v, want a token request failure with its status", tc.status, err)
			continue
		}
		if providers.IsRetryable(err) != tc.retryable {
			t.Errorf("%d: retryable = %v, want %v", tc.status, providers.IsRetryable(err), tc.retryable)
		}
		if d, _ := providers.RetryAfter(err); d != tc.wantDelay {
			t.Errorf("%d: RetryAfter = %v, want %v", tc.status, d, tc.wantDelay)
		}
		assertClean(t, err)
		if n := len(v.authorized()); n != 0 {
			t.Errorf("%d: authorized requests = %d, want none without a token", tc.status, n)
		}
	}
}

// TestSDKLogsStayClean captures everything azcore logs while a token request fails and Key Vault returns
// an error, and checks that neither the token nor any response text reaches the log.
func TestSDKLogsStayClean(t *testing.T) {
	var mu sync.Mutex
	var logs strings.Builder
	azlog.SetListener(func(_ azlog.Event, msg string) {
		mu.Lock()
		defer mu.Unlock()
		logs.WriteString(msg + "\n")
	})
	t.Cleanup(func() { azlog.SetListener(nil) })

	s, v, cred := setup(t, "https://"+testHost+"/keys/github-app", Options{MaxRetries: 1})
	v.respond = func(_ int, w http.ResponseWriter, _ *http.Request, _ []byte) {
		writeError(w, http.StatusForbidden, "Forbidden")
	}
	cred.err = authFailure(http.StatusServiceUnavailable, "")
	if _, err := s.Sign(context.Background(), digestOf("x")); err == nil {
		t.Fatal("no error with a failing credential")
	}
	cred.mu.Lock()
	cred.err = nil
	cred.mu.Unlock()
	if _, err := s.Sign(context.Background(), digestOf("x")); err == nil {
		t.Fatal("no error with a 403")
	}

	// Challenges the transport refuses never reach the SDK's parser or its retry log.
	for _, challenge := range []string{
		`Bearer authorization="https://login.microsoftonline.com/x", resource="https://` + bodySecret + `.attacker.example"`,
		`Bearer authorization="` + bodySecret + `", resource="https://vault.azure.net"`,
		`Bearer resource="%` + bodySecret + `"`,
	} {
		s, v, _ := setup(t, "https://"+testHost+"/keys/github-app", Options{MaxRetries: 1})
		v.challenge = challenge
		if _, err := s.Sign(context.Background(), digestOf("x")); !errors.Is(err, errMalformedChallenge) {
			t.Errorf("challenge %q: got %v, want the malformed challenge error", challenge, err)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if logs.Len() == 0 {
		t.Fatal("captured no SDK logs; the test would prove nothing")
	}
	for _, bad := range []string{bodySecret, testToken} {
		if strings.Contains(logs.String(), bad) {
			t.Errorf("SDK log contains %q:\n%s", bad, logs.String())
		}
	}
}

func TestSignMalformedChallenge(t *testing.T) {
	const foreign = "foreign resource"
	for name, challenge := range map[string]string{
		foreign: `Bearer authorization="https://login.microsoftonline.com/x", resource="https://` +
			bodySecret + `.attacker.example"`,
		"no resource":    `Bearer authorization="https://login.microsoftonline.com/` + bodySecret + `"`,
		"garbage claims": `Bearer claims="` + bodySecret + `" resource="https://vault.azure.net/` + bodySecret + `"`,
		// keyvault/internal v1.2.0 indexes strings.Split(authorization, "/")[3]; these would panic in Sign.
		"authorization not a url": `Bearer authorization="not-a-url", resource="https://vault.azure.net"`,
		"authorization no tenant": `Bearer authorization="https://login.microsoftonline.com", resource="https://vault.azure.net"`,
		"authorization one slash": `Bearer authorization="a/b", resource="https://vault.azure.net"`,
	} {
		s, v, _ := setup(t, "https://"+testHost+"/keys/github-app", Options{MaxRetries: -1})
		v.challenge = challenge
		v.respond = v.signOK("https://" + testHost + "/keys/github-app/" + testVersion)
		_, err := s.Sign(context.Background(), digestOf("x"))
		if err == nil {
			// A challenge the SDK tolerates must still not let a token reach another resource.
			continue
		}
		assertClean(t, err)
		if (strings.HasPrefix(name, "authorization") || name == foreign) && !errors.Is(err, errMalformedChallenge) {
			t.Errorf("%s: got %v, want the malformed challenge error", name, err)
		}
		if name == foreign && len(v.authorized()) != 0 {
			t.Errorf("%s: a token was sent after a foreign challenge", name)
		}
	}
}

func TestSafeChallenge(t *testing.T) {
	const vault = `resource="https://vault.azure.net"`
	for header, want := range map[string]bool{
		"":                true,
		`Bearer ` + vault: true,
		`Bearer authorization="https://login.microsoftonline.com/tenant", ` + vault:                            true,
		`Bearer authorization="", ` + vault:                                                                    true,
		`Bearer authorization="https://login.microsoftonline.com/t", scope="https://vault.azure.net/.default"`: true,
		`Bearer authorization="not-a-url", ` + vault:                                                           false,
		`Bearer authorization="https://login.microsoftonline.com/", ` + vault:                                  false,
		`Bearer authorization="http://login.microsoftonline.com/tenant", ` + vault:                             false,
		`Bearer authorization="https:///tenant", ` + vault:                                                     false,
		`Bearer resource="https://attacker.example"`:                                                           false,
		`Bearer resource="https://azure.net.attacker.example"`:                                                 false,
		`Bearer resource="http://vault.azure.net"`:                                                             false,
		`Bearer resource="https://user@vault.azure.net"`:                                                       false,
		`Bearer resource="%zz"`:                                                                                false,
		`Bearer scope="https://attacker.example/.default", ` + vault:                                           false,
	} {
		if got := safeChallenge(header, testHost); got != want {
			t.Errorf("safeChallenge(%q) = %v, want %v", header, got, want)
		}
	}
}

// TestSignRefusesLargeResponses checks that oversized bodies are refused at the transport, before azcore
// buffers them, for success and error responses, with and without a Content-Length, and without retries.
func TestSignRefusesLargeResponses(t *testing.T) {
	big := strings.Repeat("a", maxResponseBody+1)
	kid := "https://" + testHost + "/keys/github-app/" + testVersion
	for name, respond := range map[string]func(int, http.ResponseWriter, *http.Request, []byte){
		"success with length": func(_ int, w http.ResponseWriter, _ *http.Request, _ []byte) {
			writeJSON(w, http.StatusOK, map[string]string{kidField: kid, valueField: big})
		},
		"success chunked": func(_ int, w http.ResponseWriter, _ *http.Request, _ []byte) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"kid":"`+kid+`","value":"`)
			w.(http.Flusher).Flush()
			_, _ = io.WriteString(w, big+`"}`)
		},
		"error with length": func(_ int, w http.ResponseWriter, _ *http.Request, _ []byte) {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": map[string]string{"code": "x", "message": big}})
		},
		"error chunked": func(_ int, w http.ResponseWriter, _ *http.Request, _ []byte) {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.(http.Flusher).Flush()
			_, _ = io.WriteString(w, big)
		},
	} {
		s, v, _ := setup(t, kid, Options{})
		v.respond = respond
		_, err := s.Sign(context.Background(), digestOf("x"))
		if !errors.Is(err, errResponseTooLarge) {
			t.Errorf("%s: got %v, want the response too large error", name, err)
			continue
		}
		assertClean(t, err)
		if n := len(v.authorized()); n != 1 {
			t.Errorf("%s: %d authorized requests, want 1 (no retry)", name, n)
		}
	}
}

func TestSignAcceptsBodyAtLimit(t *testing.T) {
	kid := "https://" + testHost + "/keys/github-app/" + testVersion
	s, v, _ := setup(t, kid, Options{})
	sign := v.signOK(kid)
	v.respond = func(n int, w http.ResponseWriter, r *http.Request, b []byte) {
		rec := httptest.NewRecorder()
		sign(n, rec, r, b)
		body := strings.TrimSuffix(rec.Body.String(), "\n")
		pad := maxResponseBody - len(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, strings.Repeat(" ", pad)+body)
	}
	if _, err := s.Sign(context.Background(), digestOf("x")); err != nil {
		t.Fatalf("a %d-byte response was refused: %v", maxResponseBody, err)
	}
}

func TestNewRejectsKeyIDBeforeAnythingElse(t *testing.T) {
	// No workload identity variables are needed to reject a bad key ID.
	for _, in := range []string{
		"http://" + testHost + "/keys/github-app",
		"https://example.attacker.example/keys/github-app",
	} {
		if _, err := New(in, Options{}); err == nil || !strings.Contains(err.Error(), "key ID") {
			t.Errorf("New(%q) err = %v, want a key ID error", in, err)
		}
	}
}

func TestRetryAfterHeader(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"":       0,
		"7":      7 * time.Second,
		"-3":     0,
		"999999": time.Hour,
		"soon":   0,
	} {
		resp := &http.Response{Header: http.Header{}}
		if in != "" {
			resp.Header.Set("Retry-After", in)
		}
		if got := retryAfter(resp); got != want {
			t.Errorf("retryAfter(%q) = %v, want %v", in, got, want)
		}
	}
	if retryAfter(nil) != 0 {
		t.Error("retryAfter(nil) != 0")
	}
}

// assertClean fails if an error carries the token or Key Vault response text.
func assertClean(t *testing.T, err error) {
	t.Helper()
	for _, bad := range []string{testToken, bodySecret, "Bearer"} {
		if strings.Contains(err.Error(), bad) {
			t.Errorf("error leaks %q: %v", bad, err)
		}
	}
}
