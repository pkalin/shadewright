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
	"bytes"
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/providers/fake"
)

const (
	retryAfterHeader = "Retry-After"
	rlRemaining      = "X-RateLimit-Remaining"
	rlReset          = "X-RateLimit-Reset"
	testRepoURL      = "https://github.com/acme/fleet"
	testClientID     = "Iv23liTESTCLIENT"
	testToken        = "ghs_SECRETINSTALLATIONTOKEN0123456789"
)

var t0 = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// rewrite sends requests for the pinned host to a test server and fails the test for any other host.
type rewrite struct {
	t      *testing.T
	target *url.URL
}

func (r rewrite) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != "https" || req.URL.Host != "api.github.com" {
		r.t.Errorf("request to %s://%s, want https://api.github.com", req.URL.Scheme, req.URL.Host)
	}
	c := req.Clone(req.Context())
	c.URL.Scheme, c.URL.Host = r.target.Scheme, r.target.Host
	return http.DefaultTransport.RoundTrip(c)
}

type env struct {
	src    *Source
	signer *fake.Signer
	clk    *clock
	srv    *httptest.Server
}

func setup(t *testing.T, h http.HandlerFunc) *env {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	signer, err := fake.NewSigner()
	if err != nil {
		t.Fatal(err)
	}
	clk := &clock{t: t0}
	src, err := newSource(Config{
		ClientID: testClientID, InstallationID: 4242, RepositoryURL: testRepoURL, Signer: signer,
	}, testHooks{transport: rewrite{t, u}, now: clk.now})
	if err != nil {
		t.Fatal(err)
	}
	return &env{src: src, signer: signer, clk: clk, srv: srv}
}

func okBody(exp time.Time) string {
	return fmt.Sprintf(`{"token":%q,"expires_at":%q,"repositories":[{"full_name":"acme/fleet"}]}`,
		testToken, exp.UTC().Format(time.RFC3339))
}

func created(exp func() time.Time) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, okBody(exp()))
	}
}

func TestRequestClaimsScopeAndSignature(t *testing.T) {
	var hits atomic.Int32
	var gotJWT, gotPath, gotMethod string
	var gotBody []byte
	var hdr http.Header
	e := setup(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		gotJWT = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		gotPath, gotMethod, hdr = r.URL.Path, r.Method, r.Header.Clone()
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, okBody(t0.Add(time.Hour)))
	})
	tok, err := e.src.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if tok.Reveal() != testToken {
		t.Fatal("wrong token")
	}
	if gotMethod != http.MethodPost || gotPath != "/app/installations/4242/access_tokens" {
		t.Fatalf("request %s %s", gotMethod, gotPath)
	}
	if hdr.Get("X-GitHub-Api-Version") == "" || !strings.Contains(hdr.Get("Accept"), "github") {
		t.Fatalf("missing GitHub headers: %v", hdr)
	}

	// Scoping: exactly one repository (by name) and exactly the design 8.4 permissions.
	var body struct {
		Repositories []string          `json:"repositories"`
		Permissions  map[string]string `json:"permissions"`
	}
	if err := json.Unmarshal(gotBody, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Repositories) != 1 || body.Repositories[0] != "fleet" {
		t.Fatalf("repositories = %v", body.Repositories)
	}
	want := map[string]string{"contents": permWrite, "pull_requests": permWrite, "issues": permWrite, "metadata": "read"}
	if len(body.Permissions) != len(want) {
		t.Fatalf("permissions = %v", body.Permissions)
	}
	for k, v := range want {
		if body.Permissions[k] != v {
			t.Fatalf("permission %s = %q, want %q", k, body.Permissions[k], v)
		}
	}

	// JWT: RS256, base64url without padding, claims, and a signature over the SHA-256 digest.
	parts := strings.Split(gotJWT, ".")
	if len(parts) != 3 {
		t.Fatalf("JWT has %d parts", len(parts))
	}
	for _, p := range parts {
		if strings.ContainsAny(p, "=+/") {
			t.Fatalf("JWT part is not unpadded base64url: %q", p)
		}
	}
	dec := func(s string) []byte {
		b, err := base64.RawURLEncoding.DecodeString(s)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	var h jwtHeader
	if err := json.Unmarshal(dec(parts[0]), &h); err != nil || h.Alg != "RS256" || h.Typ != "JWT" {
		t.Fatalf("header %+v %v", h, err)
	}
	var c jwtClaims
	if err := json.Unmarshal(dec(parts[1]), &c); err != nil {
		t.Fatal(err)
	}
	if c.Iss != testClientID {
		t.Fatalf("iss = %q", c.Iss)
	}
	if c.Iat != t0.Add(-60*time.Second).Unix() {
		t.Fatalf("iat = %d, want now-60s", c.Iat)
	}
	if c.Exp-c.Iat > 600 || c.Exp <= t0.Unix() {
		t.Fatalf("exp = %d, iat = %d: lifetime must be positive and at most 10 minutes", c.Exp, c.Iat)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(e.signer.Public(), crypto.SHA256, digest[:], dec(parts[2])); err != nil {
		t.Fatalf("signature does not verify: %v", err)
	}
}

func TestCacheAndRefresh(t *testing.T) {
	var hits atomic.Int32
	var e *env
	e = setup(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		created(func() time.Time { return e.clk.now().Add(time.Hour) })(w, r)
	})
	ctx := context.Background()
	for range 3 {
		if _, err := e.src.Token(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if hits.Load() != 1 {
		t.Fatalf("hits = %d, want 1 (cached)", hits.Load())
	}
	e.clk.advance(54 * time.Minute) // 6 minutes left, outside the 5 minute margin
	if _, err := e.src.Token(ctx); err != nil || hits.Load() != 1 {
		t.Fatalf("hits = %d err = %v, want cached", hits.Load(), err)
	}
	e.clk.advance(2 * time.Minute) // 4 minutes left, inside the margin
	if _, err := e.src.Token(ctx); err != nil || hits.Load() != 2 {
		t.Fatalf("hits = %d err = %v, want refreshed", hits.Load(), err)
	}
}

func TestSingleFlight(t *testing.T) {
	var hits atomic.Int32
	release := make(chan struct{})
	e := setup(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		<-release
		created(func() time.Time { return t0.Add(time.Hour) })(w, r)
	})
	const n = 25
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for range n {
		wg.Go(func() {
			tok, err := e.src.Token(context.Background())
			if err == nil && tok.Reveal() != testToken {
				err = errors.New("wrong token")
			}
			errs <- err
		})
	}
	time.Sleep(100 * time.Millisecond) // let every caller reach the lock while the first is in flight
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if hits.Load() != 1 {
		t.Fatalf("hits = %d, want 1 shared refresh", hits.Load())
	}
}

func TestFailedRefreshDoesNotPoisonCache(t *testing.T) {
	var fail atomic.Bool
	var hits atomic.Int32
	var e *env
	e = setup(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if fail.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		created(func() time.Time { return e.clk.now().Add(time.Hour) })(w, r)
	})
	ctx := context.Background()
	if _, err := e.src.Token(ctx); err != nil {
		t.Fatal(err)
	}
	e.clk.advance(56 * time.Minute)
	fail.Store(true)
	_, err := e.src.Token(ctx)
	if !providers.IsRetryable(err) {
		t.Fatalf("err = %v, want retryable", err)
	}
	// The failure is not cached: the next call asks again, and a success replaces the token.
	fail.Store(false)
	tok, err := e.src.Token(ctx)
	if err != nil || tok.Reveal() != testToken || hits.Load() != 3 {
		t.Fatalf("tok err = %v, hits = %d", err, hits.Load())
	}
	e.clk.advance(10 * time.Minute)
	if _, err := e.src.Token(ctx); err != nil || hits.Load() != 3 {
		t.Fatalf("new token not cached: err = %v hits = %d", err, hits.Load())
	}
}

func TestWaiterRetriesAfterLeaderCancelled(t *testing.T) {
	var hits atomic.Int32
	entered := make(chan struct{}, 4)
	e := setup(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body) // lets the server notice a client that went away
		if hits.Add(1) == 1 {
			entered <- struct{}{}
			<-r.Context().Done()
			return
		}
		created(func() time.Time { return t0.Add(time.Hour) })(w, r)
	})
	leaderCtx, cancel := context.WithCancel(context.Background())
	leaderErr := make(chan error, 1)
	go func() { _, err := e.src.Token(leaderCtx); leaderErr <- err }()
	<-entered
	waiterErr := make(chan error, 1)
	go func() { _, err := e.src.Token(context.Background()); waiterErr <- err }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	if err := <-leaderErr; !errors.Is(err, context.Canceled) {
		t.Fatalf("leader err = %v", err)
	}
	if err := <-waiterErr; err != nil {
		t.Fatalf("waiter err = %v, want its own refresh to succeed", err)
	}
}

func TestRedirectNotFollowed(t *testing.T) {
	var other atomic.Int32
	e := setup(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/elsewhere" {
			other.Add(1)
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, okBody(t0.Add(time.Hour)))
			return
		}
		http.Redirect(w, r, "/elsewhere", http.StatusTemporaryRedirect)
	})
	_, err := e.src.Token(context.Background())
	if err == nil || !strings.Contains(err.Error(), "status 307") {
		t.Fatalf("err = %v, want status 307", err)
	}
	if other.Load() != 0 {
		t.Fatal("redirect was followed (the JWT would have been sent again)")
	}
}

func TestOversizedBodyRefused(t *testing.T) {
	for name, declare := range map[string]bool{"chunked": false, "content-length": true} {
		t.Run(name, func(t *testing.T) {
			e := setup(t, func(w http.ResponseWriter, _ *http.Request) {
				big := okBody(t0.Add(time.Hour))
				big = big[:len(big)-1] + `,"pad":"` + strings.Repeat("A", 70<<10) + `"}`
				if declare {
					w.Header().Set("Content-Length", fmt.Sprint(len(big)))
				}
				w.WriteHeader(http.StatusCreated)
				_, _ = io.WriteString(w, big)
				if f, ok := w.(http.Flusher); ok && !declare {
					f.Flush()
				}
			})
			tok, err := e.src.Token(context.Background())
			if err == nil || !strings.Contains(err.Error(), "larger than 64 KiB") || tok.Reveal() != "" {
				t.Fatalf("tok = %v err = %v, want refusal", tok.Reveal() != "", err)
			}
		})
	}
}

func TestStatusMapping(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		header    map[string]string
		retryable bool
		notFound  bool
		after     time.Duration
	}{
		{name: "401", status: 401},
		{name: "403", status: 403},
		{name: "403 rate limited", status: 403, header: map[string]string{retryAfterHeader: "30"}, retryable: true, after: 30 * time.Second},
		{name: "403 quota exhausted", status: 403, header: map[string]string{rlRemaining: "0"}, retryable: true},
		{name: "404", status: 404, notFound: true},
		{name: "422", status: 422},
		{name: "429", status: 429, header: map[string]string{retryAfterHeader: "7"}, retryable: true, after: 7 * time.Second},
		{name: "500", status: 500, retryable: true},
		{name: "502", status: 502, retryable: true},
		{name: "503", status: 503, header: map[string]string{retryAfterHeader: "2"}, retryable: true, after: 2 * time.Second},
		{name: "504", status: 504, retryable: true},
		{name: "501", status: 501, retryable: true},
		{name: "599", status: 599, retryable: true},
		{name: "reset only", status: 403, header: map[string]string{rlRemaining: "0", rlReset: strconv.FormatInt(t0.Add(90*time.Second).Unix(), 10)}, retryable: true, after: 90 * time.Second},
		{name: "reset capped", status: 403, header: map[string]string{rlRemaining: "0", rlReset: strconv.FormatInt(t0.Add(5*time.Hour).Unix(), 10)}, retryable: true, after: time.Hour},
		{name: "reset past", status: 403, header: map[string]string{rlRemaining: "0", rlReset: strconv.FormatInt(t0.Add(-time.Minute).Unix(), 10)}, retryable: true},
		{name: "reset garbage", status: 403, header: map[string]string{rlRemaining: "0", rlReset: "soon"}, retryable: true},
		{name: "retry-after wins", status: 403, header: map[string]string{retryAfterHeader: "5", rlRemaining: "0", rlReset: strconv.FormatInt(t0.Add(90*time.Second).Unix(), 10)}, retryable: true, after: 5 * time.Second},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := setup(t, func(w http.ResponseWriter, _ *http.Request) {
				for k, v := range c.header {
					w.Header().Set(k, v)
				}
				w.WriteHeader(c.status)
				_, _ = io.WriteString(w, `{"message":"SECRET-RESPONSE-TEXT `+testToken+`"}`)
			})
			_, err := e.src.Token(context.Background())
			if err == nil {
				t.Fatal("want an error")
			}
			if providers.IsRetryable(err) != c.retryable || providers.IsNotFound(err) != c.notFound {
				t.Fatalf("retryable = %v notFound = %v for %v", providers.IsRetryable(err), providers.IsNotFound(err), err)
			}
			if d, _ := providers.RetryAfter(err); d != c.after {
				t.Fatalf("retry after = %v, want %v", d, c.after)
			}
			if !strings.Contains(err.Error(), fmt.Sprintf("status %d", c.status)) {
				t.Fatalf("error lacks the status code: %v", err)
			}
			if strings.Contains(err.Error(), "SECRET-RESPONSE-TEXT") || strings.Contains(err.Error(), testToken) {
				t.Fatalf("error contains response text: %v", err)
			}
		})
	}
}

func TestNetworkAndTimeoutAreRetryable(t *testing.T) {
	e := setup(t, func(http.ResponseWriter, *http.Request) {})
	e.srv.Close()
	_, err := e.src.Token(context.Background())
	if !providers.IsRetryable(err) {
		t.Fatalf("network error = %v, want retryable", err)
	}
	if strings.Contains(err.Error(), "127.0.0.1") || strings.Contains(err.Error(), "api.github.com") {
		t.Fatalf("network error leaks the transport text: %v", err)
	}

	slow := setup(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	})
	slow.src.timeout = 50 * time.Millisecond
	_, err = slow.src.Token(context.Background())
	if !providers.IsRetryable(err) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout = %v, want retryable deadline", err)
	}
}

func TestBadResponsesRefused(t *testing.T) {
	cases := map[string]string{
		"not json":         `nope`,
		"no token":         `{"expires_at":"2026-11-02T13:00:00Z","repositories":[{"full_name":"acme/fleet"}]}`,
		"token with space": `{"token":"a b","expires_at":"2026-11-02T13:00:00Z","repositories":[{"full_name":"acme/fleet"}]}`,
		"bad expiry":       `{"token":"abc","expires_at":"soon","repositories":[{"full_name":"acme/fleet"}]}`,
		"expires too soon": `{"token":"abc","expires_at":"2026-11-02T12:00:30Z","repositories":[{"full_name":"acme/fleet"}]}`,
		"inside margin":    `{"token":"abc","expires_at":"2026-11-02T12:04:00Z","repositories":[{"full_name":"acme/fleet"}]}`,
		"no repositories":  `{"token":"abc","expires_at":"2026-11-02T13:00:00Z"}`,
		"other repository": `{"token":"abc","expires_at":"2026-11-02T13:00:00Z","repositories":[{"full_name":"acme/other"}]}`,
		"two repositories": `{"token":"abc","expires_at":"2026-11-02T13:00:00Z","repositories":[{"full_name":"acme/fleet"},{"full_name":"acme/other"}]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			e := setup(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusCreated)
				_, _ = io.WriteString(w, body)
			})
			if tok, err := e.src.Token(context.Background()); err == nil || tok.Reveal() != "" {
				t.Fatal("want a refusal")
			}
		})
	}
}

func TestSignerFailureSendsNothing(t *testing.T) {
	var hits atomic.Int32
	e := setup(t, func(http.ResponseWriter, *http.Request) { hits.Add(1) })
	e.signer.Fail = func(string) error { return &providers.RetryableError{Err: errors.New("vault busy")} }
	_, err := e.src.Token(context.Background())
	if !providers.IsRetryable(err) || hits.Load() != 0 {
		t.Fatalf("err = %v hits = %d", err, hits.Load())
	}
}

type fixedSigner struct{ alg providers.SignatureAlgorithm }

func (s fixedSigner) Algorithm() providers.SignatureAlgorithm    { return s.alg }
func (fixedSigner) Sign(context.Context, []byte) ([]byte, error) { return []byte("x"), nil }

func TestNewValidatesBeforeAnyRequest(t *testing.T) {
	signer, _ := fake.NewSigner()
	ok := Config{ClientID: testClientID, InstallationID: 1, RepositoryURL: testRepoURL, Signer: signer}
	if _, err := New(ok); err != nil {
		t.Fatal(err)
	}
	bad := map[string]func(*Config){
		"empty client id":    func(c *Config) { c.ClientID = "" },
		"client id chars":    func(c *Config) { c.ClientID = "a b" },
		"client id slash":    func(c *Config) { c.ClientID = "a/b" },
		"client id long":     func(c *Config) { c.ClientID = strings.Repeat("a", 65) },
		"zero installation":  func(c *Config) { c.InstallationID = 0 },
		"negative install":   func(c *Config) { c.InstallationID = -5 },
		"no signer":          func(c *Config) { c.Signer = nil },
		"non RS256 signer":   func(c *Config) { c.Signer = fixedSigner{alg: "ES256"} },
		"bad repository URL": func(c *Config) { c.RepositoryURL = "https://example.com/acme/fleet" },
	}
	for name, mutate := range bad {
		t.Run(name, func(t *testing.T) {
			c := ok
			mutate(&c)
			if _, err := New(c); err == nil {
				t.Fatal("want an error")
			}
		})
	}
}

func TestParseRepositoryURL(t *testing.T) {
	good := map[string]Repository{
		"https://github.com/acme/fleet":        {"acme", "fleet"},
		"https://github.com/a/b.c_d-e":         {"a", "b.c_d-e"},
		"https://github.com/Acme-Corp/Fleet.1": {"Acme-Corp", "Fleet.1"},
	}
	for in, want := range good {
		got, err := ParseRepositoryURL(in)
		if err != nil || got != want {
			t.Errorf("%s: %v %v", in, got, err)
		}
	}
	for _, in := range []string{
		"", "github.com/acme/fleet", "http://github.com/acme/fleet", "https://api.github.com/acme/fleet",
		"https://github.com.evil.com/acme/fleet", "https://github.com@evil.com/acme/fleet",
		"https://user@github.com/acme/fleet", "https://user:pw@github.com/acme/fleet",
		"https://github.com:443/acme/fleet", "https://GitHub.com/acme/fleet",
		"https://github.com/acme/fleet.git", "https://github.com/acme/fleet/", "https://github.com/acme",
		"https://github.com/acme/fleet/extra", "https://github.com/acme/fleet?x=1",
		"https://github.com/acme/fleet#frag", "https://github.com//fleet", "https://github.com/acme/",
		"https://github.com/acme/..", "https://github.com/acme/.", "https://github.com/-acme/fleet",
		"https://github.com/acme/fl eet", "https://github.com/acme/fleet\n", "https://github.com/acme/fl%65et",
		"https://github.com/ac_me/fleet", "https://github.com/" + strings.Repeat("a", 40) + "/fleet",
	} {
		if _, err := ParseRepositoryURL(in); err == nil {
			t.Errorf("%q accepted", in)
		}
	}
}

func TestSecretsNeverInLogsOrErrors(t *testing.T) {
	var jwts []string
	e := setup(t, func(w http.ResponseWriter, r *http.Request) {
		jwts = append(jwts, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, okBody(t0.Add(time.Hour)))
	})

	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, nil))
	var all []string
	record := func(tok Token, err error) {
		log.Info("Installation token", "token", tok, "error", err)
		log.Info("Installation token as value", "token", any(tok))
		all = append(all,
			fmt.Sprintf("%v|%+v|%#v|%s|%q|%x", tok, tok, tok, tok, tok, tok),
			fmt.Sprintf("%v|%+v|%#v", err, err, err))
		if b, jerr := json.Marshal(struct{ T Token }{tok}); jerr == nil {
			all = append(all, string(b))
		}
	}
	tok, err := e.src.Token(context.Background())
	record(tok, err)
	// A hostile or buggy server that echoes credentials back in every error response.
	for _, status := range []int{401, 403, 404, 429, 500} {
		fresh := setup(t, func(w http.ResponseWriter, r *http.Request) {
			j := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			jwts = append(jwts, j)
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `{"message":"bad `+j+` `+testToken+`"}`)
		})
		tok, err := fresh.src.Token(context.Background())
		record(tok, err)
	}
	if len(jwts) < 6 {
		t.Fatalf("captured %d JWTs, want one per request", len(jwts))
	}
	all = append(all, logs.String())
	for _, s := range all {
		if strings.Contains(s, testToken) || strings.Contains(s, "SECRETINSTALLATION") {
			t.Fatalf("token in output: %.200s", s)
		}
		for _, jwt := range jwts {
			if jwt == "" || strings.Contains(s, jwt) {
				t.Fatalf("JWT in output (or none captured): %.200s", s)
			}
		}
	}
	if !strings.Contains(logs.String(), "redacted") {
		t.Fatal("log capture did not record the token placeholder")
	}
}

type countingBody struct {
	io.ReadCloser
	n *atomic.Int64
}

func (c countingBody) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	c.n.Add(int64(n))
	return n, err
}

type countingTransport struct {
	inner http.RoundTripper
	n     *atomic.Int64
}

func (c countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := c.inner.RoundTrip(r)
	if resp != nil {
		resp.Body = countingBody{resp.Body, c.n}
	}
	return resp, err
}

// TestBodyReadIsBounded proves the client stops reading at the limit instead of buffering a huge body.
func TestBodyReadIsBounded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		chunk := bytes.Repeat([]byte("A"), 32<<10)
		for range 256 { // 8 MiB, chunked
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	signer, _ := fake.NewSigner()
	var n atomic.Int64
	src, err := newSource(Config{ClientID: testClientID, InstallationID: 1, RepositoryURL: testRepoURL, Signer: signer},
		testHooks{transport: countingTransport{rewrite{t, u}, &n}, now: (&clock{t: t0}).now})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := src.Token(context.Background()); err == nil {
		t.Fatal("want a refusal")
	}
	if got := n.Load(); got > maxResponseBody+1 {
		t.Fatalf("read %d bytes, limit is %d", got, maxResponseBody+1)
	}
}

func TestSourceRedactsItself(t *testing.T) {
	e := setup(t, created(func() time.Time { return t0.Add(time.Hour) }))
	if _, err := e.src.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	slog.New(slog.NewJSONHandler(&logs, nil)).Info("src", "source", e.src, "v", any(e.src))
	slog.New(slog.NewTextHandler(&logs, nil)).Info("src", "source", e.src)
	out := []string{
		logs.String(),
		fmt.Sprintf("%v|%+v|%#v|%s|%q|%x|%d", e.src, e.src, e.src, e.src, e.src, e.src, e.src),
		fmt.Sprint(struct{ S *Source }{e.src}),
		fmt.Sprintf("%+v|%#v", struct{ S *Source }{e.src}, struct{ S *Source }{e.src}),
	}
	for _, s := range out {
		if strings.Contains(s, testToken) || strings.Contains(s, "SECRETINSTALLATION") {
			t.Fatalf("token in output: %.200s", s)
		}
	}
	if !strings.Contains(logs.String(), "redacted") {
		t.Fatal("log output lacks the placeholder")
	}
}

func TestTransportErrorDropsRawCause(t *testing.T) {
	var jwt string
	e := setup(t, func(w http.ResponseWriter, r *http.Request) {
		jwt = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	})
	e.srv.Close()
	e.src.client.Transport = failing{func(r *http.Request) error {
		jwt = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		return errors.New("boom " + jwt)
	}}
	_, err := e.src.Token(context.Background())
	if !providers.IsRetryable(err) {
		t.Fatalf("err = %v, want retryable", err)
	}
	if _, ok := errors.AsType[*url.Error](err); ok {
		t.Fatal("chain still holds a *url.Error")
	}
	for cur := err; cur != nil; cur = errors.Unwrap(cur) {
		if strings.Contains(cur.Error(), jwt) || jwt == "" {
			t.Fatalf("chain element leaks the JWT: %v", cur)
		}
	}
}

type failing struct{ f func(*http.Request) error }

func (f failing) RoundTrip(r *http.Request) (*http.Response, error) { return nil, f.f(r) }

func TestWaiterSurvivesLeaderDeadline(t *testing.T) {
	var hits atomic.Int32
	entered := make(chan struct{}, 4)
	e := setup(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if hits.Add(1) == 1 {
			entered <- struct{}{}
			<-r.Context().Done()
			return
		}
		created(func() time.Time { return t0.Add(time.Hour) })(w, r)
	})
	leaderCtx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	leaderErr := make(chan error, 1)
	go func() { _, err := e.src.Token(leaderCtx); leaderErr <- err }()
	<-entered
	waiterErr := make(chan error, 1)
	go func() { _, err := e.src.Token(context.Background()); waiterErr <- err }()
	if err := <-leaderErr; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("leader err = %v", err)
	}
	if err := <-waiterErr; err != nil {
		t.Fatalf("waiter err = %v, want its own refresh to succeed", err)
	}
}

func TestOwnTimeoutReachesWaiters(t *testing.T) {
	entered := make(chan struct{}, 4)
	e := setup(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		entered <- struct{}{}
		<-r.Context().Done()
	})
	e.src.timeout = 300 * time.Millisecond
	leaderErr := make(chan error, 1)
	go func() { _, err := e.src.Token(context.Background()); leaderErr <- err }()
	<-entered
	_, err := e.src.Token(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) || !providers.IsRetryable(err) {
		t.Fatalf("waiter err = %v, want the refresh timeout", err)
	}
	if err := <-leaderErr; err == nil {
		t.Fatal("leader should have timed out")
	}
}

func TestBodyReadFailureIsRetryable(t *testing.T) {
	t.Run("truncated", func(t *testing.T) {
		e := setup(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", "500")
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"token":"SECRET-PARTIAL`)
			w.(http.Flusher).Flush()
			panic(http.ErrAbortHandler)
		})
		_, err := e.src.Token(context.Background())
		if !providers.IsRetryable(err) || strings.Contains(err.Error(), "SECRET-PARTIAL") {
			t.Fatalf("err = %v, want retryable without response text", err)
		}
	})
	t.Run("stall until deadline", func(t *testing.T) {
		e := setup(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", "500")
			w.WriteHeader(http.StatusCreated)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		})
		e.src.timeout = 100 * time.Millisecond
		_, err := e.src.Token(context.Background())
		if !providers.IsRetryable(err) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want retryable deadline", err)
		}
	})
}

// fmt and slog walk an unexported field without calling its methods, so a Token inside another struct must
// not print its value either.
func TestTokenInPrivateFieldStaysHidden(t *testing.T) {
	type holder struct {
		name string
		tok  Token
	}
	h := holder{name: "x", tok: newToken(testToken)}
	var logs bytes.Buffer
	slog.New(slog.NewTextHandler(&logs, nil)).Info("h", "holder", h, "ptr", &h)
	slog.New(slog.NewJSONHandler(&logs, nil)).Info("h", "holder", h, "ptr", &h)
	out := []string{logs.String(), fmt.Sprintf("%v|%+v|%#v|%v|%+v|%#v", h, h, h, &h, &h, &h)}
	for _, s := range out {
		if strings.Contains(s, testToken) {
			t.Fatalf("token in output: %.200s", s)
		}
	}
	if h.tok.Reveal() != testToken || (Token{}).Reveal() != "" {
		t.Fatal("Reveal does not return the value")
	}
}

// A signer error can carry text from the key store's response. Only its class reaches the caller.
func TestSignerErrorTextIsDropped(t *testing.T) {
	const echoed = "SECRETFROMVAULT"
	cases := map[string]struct {
		err   error
		check func(error) bool
	}{
		"plain":     {errors.New("vault said " + echoed), func(err error) bool { return !providers.IsRetryable(err) }},
		"retryable": {&providers.RetryableError{RetryAfter: 7 * time.Second, Err: errors.New(echoed)}, func(err error) bool { d, ok := providers.RetryAfter(err); return ok && d == 7*time.Second }},
		"not found": {fmt.Errorf("%s: %w", echoed, providers.ErrNotFound), providers.IsNotFound},
		"canceled":  {fmt.Errorf("%s: %w", echoed, context.Canceled), func(err error) bool { return errors.Is(err, context.Canceled) }},
		"deadline":  {fmt.Errorf("%s: %w", echoed, context.DeadlineExceeded), func(err error) bool { return providers.IsRetryable(err) && errors.Is(err, context.DeadlineExceeded) }},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			e := setup(t, func(http.ResponseWriter, *http.Request) { t.Error("request sent") })
			e.signer.Fail = func(string) error { return c.err }
			_, err := e.src.Token(context.Background())
			if err == nil || !c.check(err) {
				t.Fatalf("class lost: %v", err)
			}
			if s := fmt.Sprintf("%v|%+v|%#v", err, err, err); strings.Contains(s, echoed) {
				t.Fatalf("signer text in error: %s", s)
			}
			for u := err; u != nil; u = errors.Unwrap(u) {
				if strings.Contains(u.Error(), echoed) {
					t.Fatalf("signer text in chain: %v", u)
				}
			}
		})
	}
}
