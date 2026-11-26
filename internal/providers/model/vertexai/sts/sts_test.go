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

package sts

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/shadewright-labs/shadewright/internal/providers"
	"github.com/shadewright-labs/shadewright/internal/providers/model/openaicompatible"
	"github.com/shadewright-labs/shadewright/internal/providers/model/openaiwire"
)

const (
	testAudience   = "//iam.googleapis.com/projects/123456789012/locations/global/workloadIdentityPools/aks-lab/providers/aks-oidc"
	testProjection = "https://iam.googleapis.com/projects/123456789012/locations/global/workloadIdentityPools/aks-lab/providers/aks-oidc"
	testSubject    = "system:serviceaccount:shadewright-system:shadewright-analyzer"
	// issuedMark is the access token the test STS returns.
	issuedMark = "opaque-ISSUED-MARK-0123456789"
	// claimMark is a claim value of every projected token, to catch a decoded claim in an error.
	claimMark = "PROJECTED-CLAIM-MARK"
	// bodyMark is in every refusal body, to catch a body in an error.
	bodyMark   = "BODY-MARK-quoted-request"
	testHeader = `{"alg":"RS256","kid":"k1"}`
)

// fixtureExp is the exp of the shared projected token: far enough ahead of the run that a test's clock,
// real or fake, is inside it.
var fixtureExp = time.Now().AddDate(20, 0, 0).Unix()

// encodeJWT builds a compact JWT with the test header around payload.
func encodeJWT(payload string) string {
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(testHeader)) + "." + enc([]byte(payload)) + "." + enc([]byte("signature"))
}

// claimsJSON is the payload of a projected token for sub and aud (a string or a list).
func claimsJSON(sub string, aud any) string {
	b, err := json.Marshal(map[string]any{
		"iss": "https://oidc.example", "sub": sub, "aud": aud, "exp": fixtureExp, "mark": claimMark,
	})
	if err != nil {
		panic(err)
	}
	return string(b)
}

// claimsWithExp is claimsJSON with the given exp claim; a nil exp leaves the claim out.
func claimsWithExp(exp any) string {
	m := map[string]any{"iss": "https://oidc.example", "sub": testSubject, "aud": []string{testProjection}, "mark": claimMark}
	if exp != nil {
		m["exp"] = exp
	}
	b, err := json.Marshal(m)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func goodJWT() string {
	return encodeJWT(claimsJSON(testSubject, []string{testProjection}))
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func testConfig(t *testing.T) Config {
	t.Helper()
	file := filepath.Join(t.TempDir(), "token")
	writeFile(t, file, goodJWT()+"\n")
	return Config{Audience: testAudience, TokenFile: file, Subject: testSubject, ProjectionAudience: testProjection}
}

func stsAnswer(expiresIn int64) string {
	return fmt.Sprintf(`{"access_token":%q,"issued_token_type":%q,"token_type":"Bearer","expires_in":%d}`,
		issuedMark, typeAccessToken, expiresIn)
}

// harness is a Source whose token URL points at an httptest TLS server; n counts the requests it got.
func harness(t *testing.T, handler http.HandlerFunc) (*Source, *atomic.Int32, Config) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	cfg := testConfig(t)
	s, err := newSource(cfg, srv.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	s.tokenURL = srv.URL + "/v1/token"
	return s, &n, cfg
}

// assertNoLeak fails when any rendering of err holds the projected token, a decoded claim, the access token,
// the file path or a response body.
func assertNoLeak(t *testing.T, err error, cfg Config) {
	t.Helper()
	if err == nil {
		return
	}
	jwt := goodJWT()
	payload := strings.Split(jwt, ".")[1]
	for _, s := range []string{err.Error(), fmt.Sprintf("%+v", err), fmt.Sprintf("%#v", err), fmt.Sprintf("%q", err)} {
		for _, mark := range []string{jwt, payload, claimMark, issuedMark, bodyMark, cfg.TokenFile, filepath.Dir(cfg.TokenFile)} {
			if strings.Contains(s, mark) {
				t.Errorf("error leaks %q: %s", mark, s)
			}
		}
	}
}

func TestNewRefusesInvalidConfig(t *testing.T) {
	ranges := make([]netip.Prefix, maxInClusterRanges+1)
	for i := range ranges {
		ranges[i] = netip.PrefixFrom(netip.AddrFrom4([4]byte{100, 64, byte(i), 0}), 24)
	}
	for name, mod := range map[string]func(*Config){
		"audience as https":       func(c *Config) { c.Audience = "https:" + testAudience },
		"audience trailing slash": func(c *Config) { c.Audience = testAudience + "/" },
		"no audience":             func(c *Config) { c.Audience = "" },
		"relative token file":     func(c *Config) { c.TokenFile = "token" },
		"unclean token file":      func(c *Config) { c.TokenFile = "/var/run/../token" },
		"token file with slash":   func(c *Config) { c.TokenFile = "/var/run/token/" },
		"root as token file":      func(c *Config) { c.TokenFile = "/" },
		"no token file":           func(c *Config) { c.TokenFile = "" },
		"NUL in token file":       func(c *Config) { c.TokenFile = "/var/run/to\x00ken" },
		"long token file":         func(c *Config) { c.TokenFile = "/" + strings.Repeat("a", maxPath) },
		"subject too long":        func(c *Config) { c.Subject = subjectPrefix + strings.Repeat("n", 63) + ":" + strings.Repeat("s", 42) },
		"subject without prefix":  func(c *Config) { c.Subject = "shadewright-system:shadewright-analyzer" },
		"no subject":              func(c *Config) { c.Subject = "" },
		"no projection audience":  func(c *Config) { c.ProjectionAudience = "" },
		"Azure projection":        func(c *Config) { c.ProjectionAudience = AzureExchangeAudience },
		"Azure projection, case":  func(c *Config) { c.ProjectionAudience = "API://azureadtokenexchange/" },
		"space in projection":     func(c *Config) { c.ProjectionAudience = "a b" },
		"range of every address":  func(c *Config) { c.InClusterRanges = []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")} },
		"range with host bits": func(c *Config) {
			c.InClusterRanges = []netip.Prefix{netip.PrefixFrom(netip.MustParseAddr("100.64.1.1"), 16)}
		},
		"range embedding IPv4": func(c *Config) { c.InClusterRanges = []netip.Prefix{netip.MustParsePrefix("::ffff:100.64.0.0/112")} },
		"zero range":           func(c *Config) { c.InClusterRanges = []netip.Prefix{{}} },
		"too many ranges":      func(c *Config) { c.InClusterRanges = ranges },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testConfig(t)
			mod(&cfg)
			_, err := New(cfg)
			if !providers.IsInvalidConfig(err) {
				t.Fatalf("err = %v, want invalid config", err)
			}
			for _, v := range []string{cfg.Audience, cfg.TokenFile, cfg.Subject, cfg.ProjectionAudience} {
				if len(v) > 8 && strings.Contains(err.Error(), v) {
					t.Errorf("error quotes a configured value: %v", err)
				}
			}
		})
	}
}

// countingTransport counts requests and answers none.
type countingTransport struct{ n atomic.Int32 }

func (c *countingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	c.n.Add(1)
	return nil, errors.New("no network in this test")
}

// New reads no file and sends nothing: a token file that does not exist yet is accepted.
func TestNewReadsNothing(t *testing.T) {
	cfg := testConfig(t)
	cfg.TokenFile = filepath.Join(t.TempDir(), "not-yet")
	cfg.InClusterRanges = []netip.Prefix{netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("2001:db8::/32")}
	rt := &countingTransport{}
	s, err := newSource(cfg, rt)
	if err != nil {
		t.Fatalf("valid config refused: %v", err)
	}
	if s.tokenURL != TokenURL || rt.n.Load() != 0 {
		t.Errorf("token URL %q, %d requests", s.tokenURL, rt.n.Load())
	}
	if _, err := New(cfg); err != nil {
		t.Errorf("New: %v", err)
	}
}

type part struct {
	s  string
	ok bool
}

// The STS audience rule, over a corpus built from valid and invalid parts: only the combination of valid
// parts is accepted.
func TestAudienceCorpus(t *testing.T) {
	numbers := []part{{"1", true}, {"123456789012", true}, {"1" + strings.Repeat("0", 19), true},
		{"1" + strings.Repeat("0", 20), false}, {"0", false}, {"012", false}, {"", false}, {"1a", false}, {"-1", false}}
	ids := []part{{"pool", true}, {"my-pool-1", true}, {strings.Repeat("a", 32), true}, {"abc", false},
		{strings.Repeat("a", 33), false}, {"Pool", false}, {"po_l", false}, {"po.l", false}, {"po l", false}, {"", false}}
	locations := []part{{"global", true}, {"eu", false}, {"Global", false}}
	prefixes := []part{{"//iam.googleapis.com/projects/", true}, {"https://iam.googleapis.com/projects/", false},
		{"//iam.googleapis.com.example/projects/", false}, {"//IAM.googleapis.com/projects/", false},
		{"/iam.googleapis.com/projects/", false}, {"//iamxgoogleapis.com/projects/", false}}
	suffixes := []part{{"", true}, {"/", false}, {"\n", false}, {"/x", false}}
	base := testConfig(t)
	accepted := 0
	for _, pre := range prefixes {
		for _, num := range numbers {
			for _, loc := range locations {
				for _, pool := range ids {
					for _, prov := range ids {
						for _, suf := range suffixes {
							cfg := base
							cfg.Audience = pre.s + num.s + "/locations/" + loc.s + "/workloadIdentityPools/" + pool.s +
								"/providers/" + prov.s + suf.s
							want := pre.ok && num.ok && loc.ok && pool.ok && prov.ok && suf.ok
							if err := validate(cfg); (err == nil) != want {
								t.Fatalf("%q: err = %v, want accepted %v", cfg.Audience, err, want)
							}
							if want {
								accepted++
							}
						}
					}
				}
			}
		}
	}
	if accepted != 3*3*3 {
		t.Errorf("accepted %d audiences, want %d", accepted, 3*3*3)
	}
}

// The subject rule, over a corpus built from valid and invalid parts: a subject is accepted only when every
// part is valid and the whole is at most 127 bytes.
func TestSubjectCorpus(t *testing.T) {
	prefixes := []part{{subjectPrefix, true}, {"system:serviceaccounts:", false}, {"System:serviceaccount:", false},
		{"", false}, {" " + subjectPrefix, false}}
	namespaces := []part{{"ns", true}, {"a", true}, {"shadewright-system", true}, {strings.Repeat("n", 63), true},
		{strings.Repeat("n", 64), false}, {"Ns", false}, {"-ns", false}, {"ns-", false}, {"n_s", false}, {"n.s", false}, {"", false}}
	accounts := []part{{"sa", true}, {"a.b", true}, {"shadewright-analyzer", true}, {strings.Repeat("s", 41), true},
		{strings.Repeat("s", 42), true}, {strings.Repeat("s", 106), true}, {"a..b", false}, {".a", false}, {"a.", false},
		{"Sa", false}, {"", false}, {"s a", false}, {"sa:x", false}, {"sa\n", false}, {"-sa", false}}
	base := testConfig(t)
	var longest, refusedForLength int
	for _, pre := range prefixes {
		for _, ns := range namespaces {
			for _, sa := range accounts {
				cfg := base
				cfg.Subject = pre.s + ns.s + ":" + sa.s
				fits := len(cfg.Subject) <= MaxSubjectBytes
				want := pre.ok && ns.ok && sa.ok && fits
				if err := validate(cfg); (err == nil) != want {
					t.Fatalf("%q: err = %v, want accepted %v", cfg.Subject, err, want)
				}
				if want {
					longest = max(longest, len(cfg.Subject))
				}
				if pre.ok && ns.ok && sa.ok && !fits {
					refusedForLength++
				}
			}
		}
	}
	if longest != MaxSubjectBytes || refusedForLength == 0 {
		t.Errorf("corpus does not reach the length limit: longest accepted %d, refused for length %d", longest, refusedForLength)
	}
}

// The projection audience rule: every byte value, the length limit and the spellings of the Azure audience.
func TestProjectionAudienceCorpus(t *testing.T) {
	base := testConfig(t)
	check := func(aud string, want bool) {
		t.Helper()
		cfg := base
		cfg.ProjectionAudience = aud
		if err := validate(cfg); (err == nil) != want {
			t.Errorf("%q: err = %v, want accepted %v", aud, err, want)
		}
	}
	for b := range 256 {
		check("aud"+string([]byte{byte(b)})+"x", b >= 0x21 && b <= 0x7e)
	}
	check("", false)
	check(strings.Repeat("a", maxProjectionAudience), true)
	check(strings.Repeat("a", maxProjectionAudience+1), false)
	for _, azure := range []string{AzureExchangeAudience, strings.ToUpper(AzureExchangeAudience),
		AzureExchangeAudience + "/", AzureExchangeAudience + "//"} {
		check(azure, false)
	}
	check(AzureExchangeAudience+"2", true)
	check("x"+AzureExchangeAudience, true)
}

// The in-cluster range rule agrees with the compatible adapter's, the rule of the installation flag, over
// generated prefixes; an empty list and 32 ranges are accepted, 33 are not.
func TestInClusterRangesMatchTheCompatibleRule(t *testing.T) {
	addrs := []string{"10.0.0.0", "10.1.2.3", "100.64.0.0", "8.8.8.0", "0.0.0.0", "2001:db8::", "2001:db8::1",
		"::ffff:10.0.0.0", "64:ff9b::", "2002::", "fd00::", "::", "fe80::"}
	agreed := 0
	for _, a := range addrs {
		addr := netip.MustParseAddr(a)
		for bits := 0; bits <= addr.BitLen(); bits++ {
			p := netip.PrefixFrom(addr, bits)
			want := openaicompatible.ValidateInClusterRanges([]netip.Prefix{p}) == nil
			if got := validInClusterRanges([]netip.Prefix{p}); got != want {
				t.Errorf("%s: accepted %v, the compatible rule %v", p, got, want)
			}
			agreed++
		}
	}
	if validInClusterRanges([]netip.Prefix{{}}) || !validInClusterRanges(nil) {
		t.Error("zero prefix accepted or empty list refused")
	}
	many := make([]netip.Prefix, maxInClusterRanges+1)
	for i := range many {
		many[i] = netip.PrefixFrom(netip.AddrFrom4([4]byte{100, 64, byte(i), 0}), 24)
	}
	if !validInClusterRanges(many[:maxInClusterRanges]) || validInClusterRanges(many) {
		t.Error("the count limit is not 32")
	}
	if (openaicompatible.ValidateInClusterRanges(many) == nil) != validInClusterRanges(many) ||
		maxInClusterRanges != openaicompatible.MaxInClusterRanges {
		t.Error("the count limit differs from the compatible rule")
	}
	if agreed < 500 {
		t.Errorf("only %d prefixes compared", agreed)
	}
}

func TestExchangeSendsTheGrantAndCaches(t *testing.T) {
	var form url.Values
	var method, path, contentType string
	s, n, _ := harness(t, func(rw http.ResponseWriter, r *http.Request) {
		method, path, contentType = r.Method, r.URL.Path, r.Header.Get("Content-Type")
		_ = r.ParseForm()
		form = r.PostForm
		rw.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(rw, stsAnswer(3600))
	})
	now := time.Now()
	s.now = func() time.Time { return now }
	tok, err := s.Token(context.Background())
	if err != nil || tok != issuedMark {
		t.Fatalf("Token = %q, %v", tok, err)
	}
	if method != http.MethodPost || path != "/v1/token" || contentType != "application/x-www-form-urlencoded" {
		t.Errorf("request %s %s %s", method, path, contentType)
	}
	want := map[string]string{ // #nosec G101 -- the RFC 8693 names and a synthetic token
		"grant_type":           "urn:ietf:params:oauth:grant-type:token-exchange",
		"audience":             testAudience,
		"scope":                "https://www.googleapis.com/auth/cloud-platform",
		"requested_token_type": "urn:ietf:params:oauth:token-type:access_token",
		"subject_token_type":   "urn:ietf:params:oauth:token-type:jwt",
		"subject_token":        goodJWT(),
	}
	for k, v := range want {
		if form.Get(k) != v {
			t.Errorf("form %s = %q, want %q", k, form.Get(k), v)
		}
	}
	if len(form) != len(want) {
		t.Errorf("form has %d fields, want %d", len(form), len(want))
	}
	if _, err := s.Token(context.Background()); err != nil || n.Load() != 1 {
		t.Errorf("cached token not reused: requests %d, err %v", n.Load(), err)
	}
	now = now.Add(55*time.Minute - time.Second)
	if _, err := s.Token(context.Background()); err != nil || n.Load() != 1 {
		t.Errorf("token replaced early: requests %d, err %v", n.Load(), err)
	}
	now = now.Add(time.Second) // five minutes before expiry
	if _, err := s.Token(context.Background()); err != nil || n.Load() != 2 {
		t.Errorf("token not refreshed five minutes before expiry: requests %d, err %v", n.Load(), err)
	}
}

// recordingTransport records the URL of each request and answers with a token.
type recordingTransport struct {
	mu   sync.Mutex
	urls []string
}

func (r *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	r.urls = append(r.urls, req.Method+" "+req.URL.String())
	r.mu.Unlock()
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{},
		Body: io.NopCloser(strings.NewReader(stsAnswer(3600))), Request: req}, nil
}

func TestPostsOnlyToGoogleSTS(t *testing.T) {
	rt := &recordingTransport{}
	s, err := newSource(testConfig(t), rt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(rt.urls) != 1 || rt.urls[0] != "POST https://sts.googleapis.com/v1/token" {
		t.Errorf("requests %q", rt.urls)
	}
}

func TestClientRefusesProxiesRedirectsAndPrivateAddresses(t *testing.T) {
	cfg := testConfig(t)
	cfg.InClusterRanges = []netip.Prefix{netip.MustParsePrefix("8.8.8.0/24")}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	tr, ok := s.client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport %T", s.client.Transport)
	}
	if tr.Proxy != nil || tr.TLSClientConfig == nil || tr.TLSClientConfig.MinVersion < tls.VersionTLS12 {
		t.Error("the transport uses a proxy or allows TLS before 1.2")
	}
	if tr.ResponseHeaderTimeout <= 0 || tr.ResponseHeaderTimeout > exchangeTimeout {
		t.Errorf("header timeout %v", tr.ResponseHeaderTimeout)
	}
	if s.client.CheckRedirect == nil || !errors.Is(s.client.CheckRedirect(nil, nil), http.ErrUseLastResponse) {
		t.Error("the client follows redirects")
	}
	// The dialer's control runs before connecting, so these dials reach no network. 8.8.8.8 is public, and
	// refused only because the installation declared it in-cluster.
	for _, addr := range []string{"127.0.0.1:443", "[::1]:443", "10.0.0.1:443", "169.254.169.254:80", "[fe80::1]:443", "8.8.8.8:443"} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		conn, err := tr.DialContext(ctx, "tcp", addr)
		cancel()
		if conn != nil {
			_ = conn.Close()
		}
		if !errors.Is(err, openaiwire.ErrDialRefused) {
			t.Errorf("dial %s: err = %v, want refused", addr, err)
		}
	}

	// End to end: the real client sends nothing to a loopback server. The server's certificate is trusted, so
	// the dial guard is what stops the request: with the guard removed the same client reaches the handler.
	var n atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { n.Add(1) }))
	defer srv.Close()
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	tr.TLSClientConfig.RootCAs = pool
	s.tokenURL = srv.URL + "/v1/token"

	unguarded := tr.Clone()
	unguarded.DialContext = nil
	probe, err := newSource(cfg, unguarded)
	if err != nil {
		t.Fatal(err)
	}
	probe.tokenURL = s.tokenURL
	if _, err := probe.Token(context.Background()); n.Load() != 1 {
		t.Fatalf("control: the unguarded client sent %d requests (err %v), so this test cannot show the guard", n.Load(), err)
	}
	n.Store(0)

	_, err = s.Token(context.Background())
	if !providers.IsRetryable(err) || n.Load() != 0 {
		t.Errorf("err = %v, %d requests reached a loopback address", err, n.Load())
	}
	assertNoLeak(t, err, cfg)
}

func TestRedirectIsNotFollowed(t *testing.T) {
	var elsewhere atomic.Int32
	s, _, cfg := harness(t, func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/elsewhere" {
			elsewhere.Add(1)
			_, _ = io.WriteString(rw, stsAnswer(3600))
			return
		}
		http.Redirect(rw, r, "/elsewhere", http.StatusTemporaryRedirect)
	})
	_, err := s.Token(context.Background())
	if !providers.IsForbidden(err) || elsewhere.Load() != 0 {
		t.Fatalf("err = %v, redirect followed %d times", err, elsewhere.Load())
	}
	assertNoLeak(t, err, cfg)
}

// A 3xx whose Location cannot be parsed fails inside net/http before CheckRedirect runs; it is still a
// redirect, refused and not retried.
func TestMalformedRedirectIsRefused(t *testing.T) {
	for _, loc := range []string{"%", "http://[::1"} {
		t.Run(loc, func(t *testing.T) {
			s, _, cfg := harness(t, func(rw http.ResponseWriter, r *http.Request) {
				rw.Header().Set("Location", loc)
				rw.WriteHeader(http.StatusFound)
				_, _ = io.WriteString(rw, bodyMark)
			})
			_, err := s.Token(context.Background())
			if !providers.IsForbidden(err) || providers.IsRetryable(err) {
				t.Fatalf("err = %v, want forbidden", err)
			}
			assertNoLeak(t, err, cfg)
		})
	}
}

func TestStatusClasses(t *testing.T) {
	answer := func(tok string, issued any, typ string, exp any) string {
		b, _ := json.Marshal(map[string]any{"access_token": tok, "issued_token_type": issued, "token_type": typ, "expires_in": exp})
		return string(b)
	}
	for name, tc := range map[string]struct {
		status    int
		body      string
		retryable bool
		forbidden bool
	}{
		"bad request":       {status: http.StatusBadRequest, body: `{"error":"invalid_grant","error_description":"` + bodyMark + `"}`, forbidden: true},
		"unauthorized":      {status: http.StatusUnauthorized, body: bodyMark, forbidden: true},
		"forbidden":         {status: http.StatusForbidden, body: bodyMark, forbidden: true},
		"not found":         {status: http.StatusNotFound, body: bodyMark, forbidden: true},
		"request timeout":   {status: http.StatusRequestTimeout, body: bodyMark, retryable: true},
		"throttled":         {status: http.StatusTooManyRequests, body: bodyMark, retryable: true},
		"server error":      {status: http.StatusInternalServerError, body: bodyMark, retryable: true},
		"unavailable":       {status: http.StatusServiceUnavailable, body: bodyMark, retryable: true},
		"status 600":        {status: 600, body: bodyMark, forbidden: true},
		"status 999":        {status: 999, body: bodyMark, forbidden: true},
		"not JSON":          {status: http.StatusOK, body: bodyMark, retryable: true},
		"no token":          {status: http.StatusOK, body: `{"token_type":"Bearer","expires_in":3600}`, retryable: true},
		"empty token":       {status: http.StatusOK, body: answer("", typeAccessToken, "Bearer", 3600), retryable: true},
		"space in token":    {status: http.StatusOK, body: answer(issuedMark+" x", typeAccessToken, "Bearer", 3600), retryable: true},
		"newline in token":  {status: http.StatusOK, body: answer(issuedMark+"\r\nX: y", typeAccessToken, "Bearer", 3600), retryable: true},
		"oversized token":   {status: http.StatusOK, body: answer(issuedMark+strings.Repeat("a", maxAccessToken), typeAccessToken, "Bearer", 3600), retryable: true},
		"wrong type":        {status: http.StatusOK, body: answer(issuedMark, typeAccessToken, "pop", 3600), retryable: true},
		"wrong issued type": {status: http.StatusOK, body: answer(issuedMark, typeJWT, "Bearer", 3600), retryable: true},
		"empty issued type": {status: http.StatusOK, body: answer(issuedMark, "", "Bearer", 3600), retryable: true},
		"null issued type":  {status: http.StatusOK, body: answer(issuedMark, nil, "Bearer", 3600), retryable: true},
		"no expiry":         {status: http.StatusOK, body: answer(issuedMark, typeAccessToken, "Bearer", 0), retryable: true},
		"negative expiry":   {status: http.StatusOK, body: answer(issuedMark, typeAccessToken, "Bearer", -5), retryable: true},
		"expiry as text":    {status: http.StatusOK, body: answer(issuedMark, typeAccessToken, "Bearer", "3600"), retryable: true},
	} {
		t.Run(name, func(t *testing.T) {
			s, n, cfg := harness(t, func(rw http.ResponseWriter, r *http.Request) {
				rw.WriteHeader(tc.status)
				_, _ = io.WriteString(rw, tc.body)
			})
			for range 2 {
				_, err := s.Token(context.Background())
				if err == nil || providers.IsRetryable(err) != tc.retryable || providers.IsForbidden(err) != tc.forbidden {
					t.Fatalf("err = %v", err)
				}
				assertNoLeak(t, err, cfg)
			}
			if n.Load() != 2 {
				t.Errorf("sent %d requests for 2 calls: a refused answer was cached", n.Load())
			}
		})
	}
}

// The access token rule: every byte value and the length limit.
func TestAccessTokenCorpus(t *testing.T) {
	for b := range 256 {
		tok := "ya" + string([]byte{byte(b)}) + "z"
		if got, want := printable(tok, maxAccessToken), b >= 0x21 && b <= 0x7e; got != want {
			t.Errorf("byte %#x: accepted %v, want %v", b, got, want)
		}
	}
	for n, want := range map[int]bool{0: false, 1: true, maxAccessToken: true, maxAccessToken + 1: false} {
		if printable(strings.Repeat("a", n), maxAccessToken) != want {
			t.Errorf("length %d: want accepted %v", n, want)
		}
	}
}

func TestTokenFileProblemsAreRetryable(t *testing.T) {
	for name, prepare := range map[string]func(t *testing.T, path string){
		"missing":    func(t *testing.T, p string) { _ = os.Remove(p) },
		"empty":      func(t *testing.T, p string) { writeFile(t, p, "") },
		"whitespace": func(t *testing.T, p string) { writeFile(t, p, " \n\t") },
		"too large":  func(t *testing.T, p string) { writeFile(t, p, strings.Repeat("a", maxTokenFile+1)) },
		"directory":  func(t *testing.T, p string) { _ = os.Remove(p); _ = os.Mkdir(p, 0o700) },
		// Opened without O_NONBLOCK, a pipe with no writer would hold the read until the deadline.
		"named pipe": func(t *testing.T, p string) {
			_ = os.Remove(p)
			if err := syscall.Mkfifo(p, 0o600); err != nil {
				t.Skipf("mkfifo: %v", err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, n, cfg := harness(t, func(rw http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(rw, stsAnswer(3600)) })
			prepare(t, cfg.TokenFile)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err := s.Token(ctx)
			if !providers.IsRetryable(err) || errors.Is(err, context.DeadlineExceeded) || providers.IsForbidden(err) {
				t.Fatalf("err = %v, want a retryable file error", err)
			}
			assertNoLeak(t, err, cfg)
			if n.Load() != 0 {
				t.Errorf("sent %d requests without a token", n.Load())
			}
			// Once the kubelet has written the file, the next call succeeds.
			_ = os.RemoveAll(cfg.TokenFile)
			writeFile(t, cfg.TokenFile, goodJWT())
			if tok, err := s.Token(context.Background()); err != nil || tok != issuedMark {
				t.Errorf("after the rotation: %v", err)
			}
		})
	}
}

// A token of another ServiceAccount, for the Azure audience or for more than one audience is refused before
// anything is sent.
func TestAssertionRefusedLocally(t *testing.T) {
	good := claimsJSON(testSubject, []string{testProjection})
	for name, tc := range map[string]struct{ header, payload string }{
		"another ServiceAccount": {payload: claimsJSON("system:serviceaccount:shadewright-system:other", []string{testProjection})},
		"another namespace":      {payload: claimsJSON("system:serviceaccount:default:shadewright-analyzer", testProjection)},
		"Azure audience":         {payload: claimsJSON(testSubject, []string{AzureExchangeAudience})},
		"Azure and Google":       {payload: claimsJSON(testSubject, []string{testProjection, AzureExchangeAudience})},
		"another audience":       {payload: claimsJSON(testSubject, "https://kubernetes.default.svc")},
		"two audiences":          {payload: claimsJSON(testSubject, []string{testProjection, "https://kubernetes.default.svc"})},
		"header not JSON":        {header: "not json", payload: good},
		"header an array":        {header: `["RS256"]`, payload: good},
		"header a string":        {header: `"RS256"`, payload: good},
		"header null":            {header: " null ", payload: good},
		"header with trailing":   {header: `{"alg":"RS256"} {}`, payload: good},
	} {
		header, payload := tc.header, tc.payload
		t.Run(name, func(t *testing.T) {
			s, n, cfg := harness(t, func(rw http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(rw, stsAnswer(3600)) })
			jwt := encodeJWT(payload)
			if header != "" {
				enc := base64.RawURLEncoding.EncodeToString
				jwt = enc([]byte(header)) + "." + enc([]byte(payload)) + "." + enc([]byte("signature"))
			}
			writeFile(t, cfg.TokenFile, jwt)
			_, err := s.Token(context.Background())
			if !providers.IsForbidden(err) || providers.IsRetryable(err) {
				t.Fatalf("err = %v, want forbidden", err)
			}
			if strings.Contains(fmt.Sprintf("%+v", err), jwt) || strings.Contains(err.Error(), strings.Split(jwt, ".")[1]) {
				t.Errorf("error leaks the token: %v", err)
			}
			assertNoLeak(t, err, cfg)
			if n.Load() != 0 {
				t.Errorf("sent %d requests", n.Load())
			}
		})
	}
}

// checkAssertion over a corpus of token shapes crossed with payloads, each valid or not by construction: only
// a valid shape with a valid payload is accepted, and nothing else is.
func TestAssertionCorpus(t *testing.T) {
	enc := base64.RawURLEncoding.EncodeToString
	header, sig := enc([]byte(testHeader)), enc([]byte("signature"))
	type shape struct {
		name string
		make func(payload string) string
		ok   bool
	}
	shapes := []shape{
		{"compact", func(p string) string { return header + "." + enc([]byte(p)) + "." + sig }, true},
		{"two parts", func(p string) string { return header + "." + enc([]byte(p)) }, false},
		{"four parts", func(p string) string { return header + "." + enc([]byte(p)) + "." + sig + "." + sig }, false},
		{"empty header", func(p string) string { return "." + enc([]byte(p)) + "." + sig }, false},
		{"empty signature", func(p string) string { return header + "." + enc([]byte(p)) + "." }, false},
		{"padded payload", func(p string) string {
			return header + "." + base64.URLEncoding.EncodeToString([]byte(p+"x")) + "=." + sig
		}, false},
		{"plus in header", func(p string) string { return header + "+." + enc([]byte(p)) + "." + sig }, false},
		{"slash in signature", func(p string) string { return header + "." + enc([]byte(p)) + "." + sig + "/" }, false},
		{"newline in payload", func(p string) string { return header + ".\n" + enc([]byte(p)) + "." + sig }, false},
		{"leading space", func(p string) string { return " " + header + "." + enc([]byte(p)) + "." + sig }, false},
		{"header of one character", func(p string) string { return "A." + enc([]byte(p)) + "." + sig }, false},
		{"raw payload", func(p string) string { return header + "." + p + "." + sig }, false},
	}
	type payload struct {
		name string
		json string
		ok   bool
	}
	sub, aud := `"`+testSubject+`"`, `"`+testProjection+`"`
	payloads := []payload{
		{"string audience", `{"sub":` + sub + `,"aud":` + aud + `}`, true},
		{"list audience", `{"aud":[` + aud + `],"sub":` + sub + `}`, true},
		{"more claims", claimsJSON(testSubject, []string{testProjection}), true},
		{"whitespace", " {\n\"sub\" : " + sub + " ,\t\"aud\":[ " + aud + " ] }\n", true},
		{"escaped name", `{"s\u0075b":` + sub + `,"aud":` + aud + `}`, true},
		{"other case name besides", `{"SUB":"system:serviceaccount:x:y","sub":` + sub + `,"aud":` + aud + `}`, true},
		{"other case name alone", `{"Sub":` + sub + `,"aud":` + aud + `}`, false},
		{"other ServiceAccount", `{"sub":"system:serviceaccount:shadewright-system:other","aud":` + aud + `}`, false},
		{"subject prefix", `{"sub":"` + testSubject[:len(testSubject)-1] + `","aud":` + aud + `}`, false},
		{"subject longer", `{"sub":"` + testSubject + `x","aud":` + aud + `}`, false},
		{"subject upper case", `{"sub":"` + strings.ToUpper(testSubject) + `","aud":` + aud + `}`, false},
		{"subject space", `{"sub":"` + testSubject + ` ","aud":` + aud + `}`, false},
		{"no subject", `{"aud":` + aud + `}`, false},
		{"subject number", `{"sub":1,"aud":` + aud + `}`, false},
		{"subject null", `{"sub":null,"aud":` + aud + `}`, false},
		{"subject list", `{"sub":[` + sub + `],"aud":` + aud + `}`, false},
		{"no audience", `{"sub":` + sub + `}`, false},
		{"empty audience list", `{"sub":` + sub + `,"aud":[]}`, false},
		{"other audience", `{"sub":` + sub + `,"aud":"https://kubernetes.default.svc"}`, false},
		{"audience with slash", `{"sub":` + sub + `,"aud":"` + testProjection + `/"}`, false},
		{"two audiences", `{"sub":` + sub + `,"aud":[` + aud + `,"https://kubernetes.default.svc"]}`, false},
		{"same audience twice", `{"sub":` + sub + `,"aud":[` + aud + `,` + aud + `]}`, false},
		{"Azure audience", `{"sub":` + sub + `,"aud":"` + AzureExchangeAudience + `"}`, false},
		{"Azure after Google", `{"sub":` + sub + `,"aud":[` + aud + `,"` + AzureExchangeAudience + `"]}`, false},
		{"audience number", `{"sub":` + sub + `,"aud":1}`, false},
		{"audience list of number", `{"sub":` + sub + `,"aud":[1]}`, false},
		{"audience null", `{"sub":` + sub + `,"aud":null}`, false},
		{"repeated subject, good first", `{"sub":` + sub + `,"aud":` + aud + `,"sub":"system:serviceaccount:a:b"}`, false},
		{"repeated subject, good last", `{"sub":"system:serviceaccount:a:b","aud":` + aud + `,"sub":` + sub + `}`, false},
		{"repeated audience", `{"sub":` + sub + `,"aud":"x","aud":` + aud + `}`, false},
		{"repeated other claim", `{"iss":"a","sub":` + sub + `,"aud":` + aud + `,"iss":"b"}`, false},
		{"trailing object", `{"sub":` + sub + `,"aud":` + aud + `}{}`, false},
		{"trailing text", `{"sub":` + sub + `,"aud":` + aud + `}x`, false},
		{"unterminated", `{"sub":` + sub + `,"aud":` + aud, false},
		{"list", `[` + sub + `]`, false},
		{"string", sub, false},
		{"null", `null`, false},
		{"empty", ``, false},
	}
	s, err := newSource(testConfig(t), &countingTransport{})
	if err != nil {
		t.Fatal(err)
	}
	accepted := 0
	for _, sh := range shapes {
		for _, p := range payloads {
			tok := sh.make(p.json)
			err := s.checkAssertion(tok)
			want := sh.ok && p.ok
			if (err == nil) != want || (err != nil && !providers.IsForbidden(err)) {
				t.Errorf("%s, %s: err = %v, want accepted %v", sh.name, p.name, err, want)
			}
			if err != nil && strings.Contains(err.Error(), testSubject) {
				t.Errorf("%s, %s: error quotes a claim: %v", sh.name, p.name, err)
			}
			if want {
				accepted++
			}
		}
	}
	if accepted != 6 {
		t.Errorf("accepted %d tokens, want 6", accepted)
	}
}

func TestCancellationStopsTheExchange(t *testing.T) {
	started, gone := make(chan struct{}), make(chan struct{})
	s, _, _ := harness(t, func(rw http.ResponseWriter, r *http.Request) {
		close(started)
		_, _ = io.Copy(io.Discard, r.Body) // the server sees the client go away only after the body is read
		<-r.Context().Done()
		close(gone)
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-started; cancel() }()
	_, err := s.Token(ctx)
	if !errors.Is(err, context.Canceled) || providers.IsRetryable(err) {
		t.Fatalf("err = %v, want canceled", err)
	}
	select {
	case <-gone:
	case <-time.After(5 * time.Second):
		t.Fatal("the token request went on after its only caller left")
	}
}

// A caller waiting for another caller's exchange gives up when its own context ends.
func TestWaitRespectsTheCallersContext(t *testing.T) {
	release := make(chan struct{})
	s, _, _ := harness(t, func(http.ResponseWriter, *http.Request) { <-release })
	defer close(release)
	go func() { _, _ = s.Token(context.Background()) }() // another exchange in progress
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := s.Token(ctx); !errors.Is(err, context.DeadlineExceeded) || !providers.IsRetryable(err) {
		t.Fatalf("err = %v, want a retryable deadline", err)
	}
	cctx, ccancel := context.WithCancel(context.Background())
	ccancel()
	if _, err := s.Token(cctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want canceled", err)
	}
}

// Callers share one exchange, and a canceled caller does not fail the exchange the others wait for.
func TestCallersShareOneExchange(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	s, n, _ := harness(t, func(rw http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(started) })
		<-release
		_, _ = io.WriteString(rw, stsAnswer(3600))
	})
	first, cancelFirst := context.WithCancel(context.Background())
	firstErr := make(chan error, 1)
	go func() { _, err := s.Token(first); firstErr <- err }()
	select {
	case <-started:
	case err := <-firstErr:
		t.Fatalf("the exchange never reached the server: %v", err)
	}
	type result struct {
		tok string
		err error
	}
	waiters := make(chan result, 3)
	for range 3 {
		go func() { tok, err := s.Token(context.Background()); waiters <- result{tok, err} }()
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		s.mu.Lock()
		joined := s.inflight != nil && s.inflight.waiters == 4
		s.mu.Unlock()
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
		if r := <-waiters; r.err != nil || r.tok != issuedMark {
			t.Fatalf("waiter: err = %v", r.err)
		}
	}
	if n.Load() != 1 {
		t.Errorf("sent %d token requests, want 1 shared exchange", n.Load())
	}
}

// A token is replaced min(5m, lifetime/2) before it expires, and a stated lifetime counts as at most 24 hours.
func TestRefreshMargin(t *testing.T) {
	for _, tc := range []struct {
		expiresIn int64
		refresh   time.Duration // after the exchange
	}{
		{2, time.Second},
		{60, 30 * time.Second},
		{300, 150 * time.Second},
		{3600, 55 * time.Minute},
		{100000, 24*time.Hour - 5*time.Minute},
	} {
		t.Run(fmt.Sprint(tc.expiresIn), func(t *testing.T) {
			s, n := countingSource(t, tc.expiresIn, goodJWT())
			// A fake clock fixed in 2030: the cache must not depend on the date the test runs.
			start := time.Unix(1900000001, 0)
			now := start
			s.now = func() time.Time { return now }
			for range 3 {
				if _, err := s.Token(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			now = start.Add(tc.refresh - time.Nanosecond)
			if _, err := s.Token(context.Background()); err != nil || n.Load() != 1 {
				t.Fatalf("replaced before %v: requests %d, err %v", tc.refresh, n.Load(), err)
			}
			now = start.Add(tc.refresh)
			if _, err := s.Token(context.Background()); err != nil || n.Load() != 2 {
				t.Errorf("not replaced at %v: requests %d, err %v", tc.refresh, n.Load(), err)
			}
		})
	}
}

// lateSTS answers only once the exchange's context has ended, as net/http can return an answer that raced
// the end of the context. It ends the caller's context itself, so the only waiter leaves and the exchange is
// canceled. call is the exchange it served.
type lateSTS struct {
	s    **Source
	end  context.CancelFunc
	code int
	call chan *exchangeCall
}

func (l lateSTS) RoundTrip(r *http.Request) (*http.Response, error) {
	s := *l.s
	s.mu.Lock()
	l.call <- s.inflight
	s.mu.Unlock()
	l.end()
	<-r.Context().Done()
	return &http.Response{StatusCode: l.code, Header: http.Header{},
		Body: io.NopCloser(strings.NewReader(stsAnswer(3600))), Request: r}, nil
}

// An exchange whose context ended classifies nothing of the answer that raced that end, and caches no token
// of it.
func TestAnswerAfterContextEndIsDiscarded(t *testing.T) {
	for _, code := range []int{http.StatusOK, http.StatusUnauthorized, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var s *Source
			calls := make(chan *exchangeCall, 1)
			s, err := newSource(testConfig(t), lateSTS{s: &s, end: cancel, code: code, call: calls})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Token(ctx); !errors.Is(err, context.Canceled) {
				t.Fatalf("Token = %v, want the caller's cancellation", err)
			}
			call := <-calls
			<-call.done
			if !errors.Is(call.err, context.Canceled) || providers.IsForbidden(call.err) || providers.IsRetryable(call.err) || call.token() != "" {
				t.Errorf("exchange err = %v, want its cancellation", call.err)
			}
			s.mu.Lock()
			cached := s.cachedToken()
			s.mu.Unlock()
			if cached != "" {
				t.Error("a token taken after the context ended was cached")
			}
		})
	}
}

// No error of any failure, in any format, and nothing written to the standard loggers holds the projected
// token, a claim, the access token, the file path or a response body.
func TestNothingLeaksToErrorsOrLogs(t *testing.T) {
	var logs bytes.Buffer
	prevOut, prevFlags, prevSlog := log.Writer(), log.Flags(), slog.Default()
	log.SetOutput(&logs)
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { log.SetOutput(prevOut); log.SetFlags(prevFlags); slog.SetDefault(prevSlog) })

	var status atomic.Int64
	var valid atomic.Bool
	s, n, cfg := harness(t, func(rw http.ResponseWriter, r *http.Request) {
		echo := r.PostFormValue("subject_token")
		if valid.Load() {
			_, _ = io.WriteString(rw, stsAnswer(3600))
			return
		}
		rw.WriteHeader(int(status.Load()))
		_, _ = io.WriteString(rw, bodyMark+" "+echo+" "+issuedMark)
	})
	for i, code := range []int{400, 401, 403, 404, 408, 429, 500, 502, 200} {
		status.Store(int64(code))
		_, err := s.Token(context.Background())
		if err == nil {
			t.Fatalf("status %d: no error", code)
		}
		if int(n.Load()) != i+1 {
			t.Fatalf("status %d: %d requests reached the server, want %d", code, n.Load(), i+1)
		}
		assertNoLeak(t, err, cfg)
	}
	writeFile(t, cfg.TokenFile, encodeJWT(claimsJSON(testSubject, AzureExchangeAudience)))
	_, err := s.Token(context.Background())
	assertNoLeak(t, err, cfg)
	_ = os.Remove(cfg.TokenFile)
	_, err = s.Token(context.Background())
	assertNoLeak(t, err, cfg)

	// A valid answer: the token is returned and cached, and nothing is logged.
	writeFile(t, cfg.TokenFile, goodJWT())
	valid.Store(true)
	before := n.Load()
	for range 2 {
		if tok, err := s.Token(context.Background()); err != nil || tok != issuedMark {
			t.Fatalf("valid answer: %q, %v", tok, err)
		}
	}
	if n.Load() != before+1 {
		t.Errorf("valid answer: %d requests, want 1", n.Load()-before)
	}

	if logs.Len() != 0 {
		t.Errorf("the token source wrote to a log: %d bytes", logs.Len())
	}
}

// rtFunc is a RoundTripper made of a function, so a test can serve any response and body.
type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func okResponse(r *http.Request, status int, body io.ReadCloser) (*http.Response, error) {
	return &http.Response{StatusCode: status, Header: http.Header{}, Body: body, Request: r}, nil
}

func stsAnswerFor(token string, expiresIn int64) string {
	return fmt.Sprintf(`{"access_token":%q,"issued_token_type":%q,"token_type":"Bearer","expires_in":%d}`,
		token, typeAccessToken, expiresIn)
}

// pipeListener is an in-memory listener, so the HTTP/2 test needs no loopback socket.
type pipeListener struct {
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
}

type pipeAddr struct{}

func (pipeAddr) Network() string { return "pipe" }
func (pipeAddr) String() string  { return "pipe" }

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *pipeListener) Close() error   { l.once.Do(func() { close(l.done) }); return nil }
func (l *pipeListener) Addr() net.Addr { return pipeAddr{} }

func (l *pipeListener) dial(ctx context.Context, _, _ string) (net.Conn, error) {
	client, server := net.Pipe()
	select {
	case l.conns <- server:
		return client, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// The client must not speak HTTP/2: Go's HTTP/2 frame diagnostics (GODEBUG=http2debug) print the bytes of
// DATA frames, which would hold the projected token and the access token. The server here offers HTTP/2;
// the control client shows the test can see it being negotiated.
func TestClientNegotiatesHTTP1Only(t *testing.T) {
	lis := &pipeListener{conns: make(chan net.Conn), done: make(chan struct{})}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(rw, "ok")
	}))
	srv.Listener = lis
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()
	const target = "https://example.com/" // the certificate of httptest names example.com

	get := func(tr *http.Transport) *http.Response {
		t.Helper()
		defer tr.CloseIdleConnections()
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, target, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := (&http.Client{Transport: tr}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return resp
	}
	trusted := srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs

	control := srv.Client().Transport.(*http.Transport).Clone()
	control.DialContext = lis.dial
	if resp := get(control); resp.ProtoMajor != 2 {
		t.Fatalf("control client negotiated %s, want HTTP/2: the server does not offer it", resp.Proto)
	}

	s, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	guarded, ok := s.client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport %T", s.client.Transport)
	}
	// The same transport settings, except that the dial guard is replaced and the test server's
	// certificate is trusted.
	tr := guarded.Clone()
	tr.DialContext = lis.dial
	tr.TLSClientConfig.RootCAs = trusted
	resp := get(tr)
	if resp.Proto != "HTTP/1.1" || resp.TLS == nil || resp.TLS.NegotiatedProtocol == "h2" {
		t.Errorf("protocol %q, want HTTP/1.1 and no h2 in ALPN", resp.Proto)
	}
}

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

func waitErr(t *testing.T, ch <-chan error, msg string) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(testWaitBound):
		t.Fatal(msg)
		return nil
	}
}

// An exchange that was abandoned and replaced by a newer successful exchange must not overwrite the cache,
// while still finishing with its own result.
func TestSupersededExchangeDoesNotWriteTheCache(t *testing.T) {
	const stale = "opaque-STALE-TOKEN-0123456789"
	gate := &closeGate{Reader: strings.NewReader(stsAnswerFor(stale, 3600)), entered: make(chan struct{}), release: make(chan struct{})}
	defer gate.unblock()
	var n atomic.Int32
	var s *Source
	first := make(chan *exchangeCall, 1)
	s, err := newSource(testConfig(t), rtFunc(func(r *http.Request) (*http.Response, error) {
		if n.Add(1) == 1 {
			s.mu.Lock()
			first <- s.inflight
			s.mu.Unlock()
			return okResponse(r, http.StatusOK, gate)
		}
		return okResponse(r, http.StatusOK, io.NopCloser(strings.NewReader(stsAnswer(3600))))
	}))
	if err != nil {
		t.Fatal(err)
	}
	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	errA := make(chan error, 1)
	go func() { _, err := s.Token(ctxA); errA <- err }()
	waitSignal(t, gate.entered, "the first exchange never reached the end of its response")
	cancelA() // the only waiter leaves
	if err := waitErr(t, errA, "first caller never returned"); !errors.Is(err, context.Canceled) {
		t.Fatalf("first caller: err = %v, want canceled", err)
	}
	if tok, err := s.Token(context.Background()); err != nil || tok != issuedMark {
		t.Fatalf("second exchange: %q, %v", tok, err)
	}
	gate.unblock()
	call := <-first
	waitSignal(t, call.done, "the first exchange never finished")
	if tok, err := call.result(); err != nil || tok != stale {
		t.Errorf("first exchange result: err = %v, got own token = %v; want own token and nil error", err, tok == stale)
	}
	if tok, err := s.Token(context.Background()); err != nil || tok != issuedMark || n.Load() != 2 {
		t.Errorf("cache holds %q (err %v) after %d exchanges, want the second exchange's token from 2", tok, err, n.Load())
	}
}

// An abandoned exchange that succeeds with no newer exchange still caches its valid token, so the next Token
// call reuses it without making a second exchange.
func TestAbandonedExchangeCachesWhenAlone(t *testing.T) {
	const firstTok = "opaque-FIRST-TOKEN-0123456789"
	gate := &closeGate{Reader: strings.NewReader(stsAnswerFor(firstTok, 3600)), entered: make(chan struct{}), release: make(chan struct{})}
	defer gate.unblock()
	var n atomic.Int32
	var s *Source
	first := make(chan *exchangeCall, 1)
	s, err := newSource(testConfig(t), rtFunc(func(r *http.Request) (*http.Response, error) {
		if n.Add(1) == 1 {
			s.mu.Lock()
			first <- s.inflight
			s.mu.Unlock()
			return okResponse(r, http.StatusOK, gate)
		}
		return okResponse(r, http.StatusOK, io.NopCloser(strings.NewReader(stsAnswer(3600))))
	}))
	if err != nil {
		t.Fatal(err)
	}
	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	errA := make(chan error, 1)
	go func() { _, err := s.Token(ctxA); errA <- err }()
	waitSignal(t, gate.entered, "the first exchange never reached the end of its response")
	cancelA() // the only waiter leaves, and no newer caller arrives
	if err := waitErr(t, errA, "first caller never returned"); !errors.Is(err, context.Canceled) {
		t.Fatalf("first caller: err = %v, want canceled", err)
	}
	gate.unblock()
	call := <-first
	waitSignal(t, call.done, "the first exchange never finished")
	if tok, err := call.result(); err != nil || tok != firstTok {
		t.Errorf("first exchange result: err = %v, got own token = %v; want own token and nil error", err, tok == firstTok)
	}
	if tok, err := s.Token(context.Background()); err != nil || tok != firstTok || n.Load() != 1 {
		t.Errorf("cache holds %q (err %v) after %d exchanges, want the first exchange's token from 1", tok, err, n.Load())
	}
}

// An abandoned exchange that succeeds after a newer exchange failed still caches its valid token.
func TestAbandonedExchangeCachesAfterReplacementFails(t *testing.T) {
	const firstTok = "opaque-FIRST-TOKEN-0123456789"
	gate := &closeGate{Reader: strings.NewReader(stsAnswerFor(firstTok, 3600)), entered: make(chan struct{}), release: make(chan struct{})}
	defer gate.unblock()
	var n atomic.Int32
	var s *Source
	first := make(chan *exchangeCall, 1)
	s, err := newSource(testConfig(t), rtFunc(func(r *http.Request) (*http.Response, error) {
		switch n.Add(1) {
		case 1:
			s.mu.Lock()
			first <- s.inflight
			s.mu.Unlock()
			return okResponse(r, http.StatusOK, gate)
		case 2:
			return okResponse(r, http.StatusServiceUnavailable, io.NopCloser(strings.NewReader("unavailable")))
		default:
			return okResponse(r, http.StatusOK, io.NopCloser(strings.NewReader(stsAnswer(3600))))
		}
	}))
	if err != nil {
		t.Fatal(err)
	}
	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	errA := make(chan error, 1)
	go func() { _, err := s.Token(ctxA); errA <- err }()
	waitSignal(t, gate.entered, "the first exchange never reached the end of its response")
	cancelA() // the only waiter leaves
	if err := waitErr(t, errA, "first caller never returned"); !errors.Is(err, context.Canceled) {
		t.Fatalf("first caller: err = %v, want canceled", err)
	}
	if _, err := s.Token(context.Background()); !providers.IsRetryable(err) {
		t.Fatalf("second exchange: err = %v, want retryable failure", err)
	}
	gate.unblock()
	call := <-first
	waitSignal(t, call.done, "the first exchange never finished")
	if tok, err := call.result(); err != nil || tok != firstTok {
		t.Errorf("first exchange result: err = %v, got own token = %v; want own token and nil error", err, tok == firstTok)
	}
	if tok, err := s.Token(context.Background()); err != nil || tok != firstTok || n.Load() != 2 {
		t.Errorf("cache holds %q (err %v) after %d exchanges, want the first exchange's token from 2", tok, err, n.Load())
	}
}

// A live waiter of an exchange always gets that exchange's own result, even when an older abandoned exchange
// finishes and writes the cache while the newer exchange is still in flight.
func TestLiveWaiterGetsOwnCallResult(t *testing.T) {
	const firstTok = "opaque-FIRST-TOKEN-0123456789"
	for _, tc := range []struct {
		name      string
		secondOK  bool
		wantCache string
	}{
		{name: "replacement succeeds", secondOK: true, wantCache: issuedMark},
		{name: "replacement fails", secondOK: false, wantCache: firstTok},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gate1 := &closeGate{Reader: strings.NewReader(stsAnswerFor(firstTok, 3600)), entered: make(chan struct{}), release: make(chan struct{})}
			defer gate1.unblock()
			call2Started := make(chan struct{})
			call2Release := make(chan struct{})
			var release2Once sync.Once
			unblock2 := func() { release2Once.Do(func() { close(call2Release) }) }
			defer unblock2()

			var n atomic.Int32
			var s *Source
			first := make(chan *exchangeCall, 1)
			s, err := newSource(testConfig(t), rtFunc(func(r *http.Request) (*http.Response, error) {
				switch n.Add(1) {
				case 1:
					s.mu.Lock()
					first <- s.inflight
					s.mu.Unlock()
					return okResponse(r, http.StatusOK, gate1)
				case 2:
					close(call2Started)
					<-call2Release
					if tc.secondOK {
						return okResponse(r, http.StatusOK, io.NopCloser(strings.NewReader(stsAnswer(3600))))
					}
					return okResponse(r, http.StatusServiceUnavailable, io.NopCloser(strings.NewReader("unavailable")))
				default:
					return okResponse(r, http.StatusOK, io.NopCloser(strings.NewReader(stsAnswer(3600))))
				}
			}))
			if err != nil {
				t.Fatal(err)
			}

			ctxA, cancelA := context.WithCancel(context.Background())
			defer cancelA()
			errA := make(chan error, 1)
			go func() { _, err := s.Token(ctxA); errA <- err }()
			waitSignal(t, gate1.entered, "the first exchange never reached the end of its response")
			cancelA()
			if err := waitErr(t, errA, "first caller never returned"); !errors.Is(err, context.Canceled) {
				t.Fatalf("first caller: err = %v, want canceled", err)
			}

			type result struct {
				tok string
				err error
			}
			resB := make(chan result, 1)
			go func() {
				tok, err := s.Token(context.Background())
				resB <- result{tok, err}
			}()
			waitSignal(t, call2Started, "the second exchange never started")

			gate1.unblock()
			call1 := <-first
			waitSignal(t, call1.done, "the first exchange never finished")
			if tok, err := call1.result(); err != nil || tok != firstTok {
				t.Errorf("first exchange result: err = %v, got own token = %v; want own token and nil error", err, tok == firstTok)
			}
			s.mu.Lock()
			stillInflight := s.inflight != nil && s.inflight != call1
			s.mu.Unlock()
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
				if r.err != nil || r.tok != issuedMark {
					t.Errorf("live waiter: err = %v, got second token = %v; want second token", r.err, r.tok == issuedMark)
				}
			} else {
				if r.tok != "" || !providers.IsRetryable(r.err) {
					t.Errorf("live waiter: err = %v, empty token = %v; want retryable error and no token", r.err, r.tok == "")
				}
			}
			if tok, err := s.Token(context.Background()); err != nil || tok != tc.wantCache || n.Load() != 2 {
				t.Errorf("cache check: err = %v, got expected token = %v, exchanges = %d; want expected token from 2 exchanges",
					err, tok == tc.wantCache, n.Load())
			}
		})
	}
}

// countingSource is a Source whose STS answers every request with a token of the given lifetime, reading the
// given projected token; n counts the requests.
func countingSource(t *testing.T, expiresIn int64, projected string) (*Source, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	cfg := testConfig(t)
	writeFile(t, cfg.TokenFile, projected)
	s, err := newSource(cfg, rtFunc(func(r *http.Request) (*http.Response, error) {
		n.Add(1)
		return okResponse(r, http.StatusOK, io.NopCloser(strings.NewReader(stsAnswer(expiresIn))))
	}))
	if err != nil {
		t.Fatal(err)
	}
	return s, &n
}

func jwtExpiringAt(exp any) string { return encodeJWT(claimsWithExp(exp)) }

// The cached token is dropped before the projected token it came from expires: at its exp minus the
// refresh margin, which is the shorter of five minutes and half the time left. A projection without a
// readable exp caps the cache at a short fixed lifetime.
func TestCacheNeverOutlivesTheProjection(t *testing.T) {
	start := time.Now().Truncate(time.Second)
	in := func(d time.Duration) any { return start.Add(d).Unix() }
	for name, tc := range map[string]struct {
		exp       any
		expiresIn int64
		refresh   time.Duration // after the exchange
	}{
		"30 seconds left":     {in(30 * time.Second), 3600, 15 * time.Second},
		"10 minutes left":     {in(10 * time.Minute), 3600, 5 * time.Minute},
		"fraction of seconds": {float64(start.Unix()) + 60.5, 3600, 30 * time.Second}, // read as 60s: never later
		"projection outlasts": {in(24 * time.Hour), 3600, 55 * time.Minute},
		"STS outlasts":        {in(time.Hour), 7200, 55 * time.Minute},
		"no exp":              {nil, 3600, unknownProjectionLifetime},
		"exp as text":         {"soon", 3600, unknownProjectionLifetime},
		"exp null":            {json.RawMessage("null"), 3600, unknownProjectionLifetime},
		"exp as list":         {[]int{1}, 3600, unknownProjectionLifetime},
	} {
		t.Run(name, func(t *testing.T) {
			s, n := countingSource(t, tc.expiresIn, jwtExpiringAt(tc.exp))
			now := start
			s.now = func() time.Time { return now }
			for range 3 {
				if _, err := s.Token(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			now = start.Add(tc.refresh - time.Nanosecond)
			if _, err := s.Token(context.Background()); err != nil || n.Load() != 1 {
				t.Fatalf("replaced before %v: requests %d, err %v", tc.refresh, n.Load(), err)
			}
			now = start.Add(tc.refresh)
			if _, err := s.Token(context.Background()); err != nil || n.Load() != 2 {
				t.Errorf("not replaced at %v: requests %d, err %v", tc.refresh, n.Load(), err)
			}
		})
	}
	if unknownProjectionLifetime <= 0 || unknownProjectionLifetime > 10*time.Minute {
		t.Errorf("the fixed lifetime %v is not short", unknownProjectionLifetime)
	}
}

// A projected token that has already expired still gets its exchange answered by STS, but the answer is
// not kept for the next caller.
func TestAnswerForAnExpiredProjectionIsNotCached(t *testing.T) {
	start := time.Now().Truncate(time.Second)
	s, n := countingSource(t, 3600, jwtExpiringAt(start.Add(-time.Minute).Unix()))
	s.now = func() time.Time { return start }
	for i := 1; i <= 3; i++ {
		if _, err := s.Token(context.Background()); err != nil || int(n.Load()) != i {
			t.Fatalf("call %d: requests %d, err %v, want one exchange per call", i, n.Load(), err)
		}
	}
}

// A token that expired, or is inside its refresh margin, by the time the answer is read is not returned.
func TestTokenAlreadyStaleOnArrivalIsRefused(t *testing.T) {
	for name, tc := range map[string]struct {
		expiresIn int64
		transit   time.Duration
		refused   bool
	}{
		"expired in transit":      {1, 2 * time.Second, true},
		"inside the margin":       {60, 40 * time.Second, true},
		"exactly at the margin":   {60, 30 * time.Second, true},
		"just outside the margin": {60, 30*time.Second - time.Millisecond, false},
		"quick answer":            {60, time.Millisecond, false},
	} {
		t.Run(name, func(t *testing.T) {
			base := time.Now()
			var elapsed atomic.Int64
			s, err := newSource(testConfig(t), rtFunc(func(r *http.Request) (*http.Response, error) {
				elapsed.Store(int64(tc.transit))
				return okResponse(r, http.StatusOK, io.NopCloser(strings.NewReader(stsAnswer(tc.expiresIn))))
			}))
			if err != nil {
				t.Fatal(err)
			}
			s.now = func() time.Time { return base.Add(time.Duration(elapsed.Load())) }
			tok, err := s.Token(context.Background())
			if tc.refused {
				if tok != "" || !providers.IsRetryable(err) || providers.IsForbidden(err) {
					t.Fatalf("Token = %q, %v, want an empty token and a retryable error", tok, err)
				}
				assertNoLeak(t, err, Config{TokenFile: "/unused"})
				s.mu.Lock()
				cached := s.cachedToken()
				s.mu.Unlock()
				if cached != "" {
					t.Error("a stale token was cached")
				}
				return
			}
			if err != nil || tok != issuedMark {
				t.Fatalf("Token = %q, %v", tok, err)
			}
		})
	}
}

// stallBody blocks its reader until ctx ends, like a response body that stops sending.
type stallBody struct {
	ctx     context.Context
	reading chan struct{}
	once    sync.Once
}

func (b *stallBody) Read([]byte) (int, error) {
	b.once.Do(func() { close(b.reading) })
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

func (b *stallBody) Close() error { return nil }

// A deadline or a cancellation that ends the read of a refusal body is reported as that end, not as the
// refusal.
func TestEndDuringRefusalBodyIsNotTheRefusal(t *testing.T) {
	for _, code := range []int{http.StatusOK, http.StatusForbidden, http.StatusBadRequest, http.StatusServiceUnavailable} {
		t.Run(fmt.Sprint(code, " deadline"), func(t *testing.T) {
			s, err := newSource(testConfig(t), rtFunc(func(r *http.Request) (*http.Response, error) {
				return okResponse(r, code, &stallBody{ctx: r.Context(), reading: make(chan struct{})})
			}))
			if err != nil {
				t.Fatal(err)
			}
			s.timeout = 50 * time.Millisecond
			tok, err := s.Token(context.Background())
			if tok != "" || !providers.IsRetryable(err) || !errors.Is(err, context.DeadlineExceeded) || providers.IsForbidden(err) {
				t.Fatalf("Token = %q, %v, want an empty token and the retryable deadline error", tok, err)
			}
			assertNoLeak(t, err, Config{TokenFile: "/unused"})
		})
		t.Run(fmt.Sprint(code, " cancellation"), func(t *testing.T) {
			var s *Source
			reading := make(chan struct{})
			calls := make(chan *exchangeCall, 1)
			s, err := newSource(testConfig(t), rtFunc(func(r *http.Request) (*http.Response, error) {
				s.mu.Lock()
				calls <- s.inflight
				s.mu.Unlock()
				return okResponse(r, code, &stallBody{ctx: r.Context(), reading: reading})
			}))
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go func() { <-reading; cancel() }()
			if _, err := s.Token(ctx); !errors.Is(err, context.Canceled) {
				t.Fatalf("Token = %v, want canceled", err)
			}
			call := <-calls
			<-call.done
			if !errors.Is(call.err, context.Canceled) || providers.IsForbidden(call.err) || call.token() != "" {
				t.Errorf("exchange err = %v, want its cancellation", call.err)
			}
		})
	}
}

// limitedBody serves a valid token answer followed by whitespace, which JSON allows after the value, and
// fails the test when it is read after the limit and one more byte: the read limit is what stops it.
type limitedBody struct {
	t     *testing.T
	data  []byte // what is left to serve
	total int
}

func (b *limitedBody) Read(p []byte) (int, error) {
	if b.total > maxTokenResponse {
		b.t.Errorf("read past %d bytes: the response is not bounded", maxTokenResponse+1)
		return 0, io.EOF
	}
	if len(b.data) == 0 {
		return 0, io.EOF
	}
	n := copy(p, b.data)
	b.data = b.data[n:]
	b.total += n
	return n, nil
}

func (b *limitedBody) Close() error { return nil }

func paddedAnswer(size int) []byte {
	head := stsAnswer(3600)
	return append([]byte(head), bytes.Repeat([]byte(" "), size-len(head))...)
}

func TestOversizedAnswerIsStoppedByTheReadLimit(t *testing.T) {
	serve := func(t *testing.T, size int) (string, error) {
		t.Helper()
		s, err := newSource(testConfig(t), rtFunc(func(r *http.Request) (*http.Response, error) {
			return okResponse(r, http.StatusOK, &limitedBody{t: t, data: paddedAnswer(size)})
		}))
		if err != nil {
			t.Fatal(err)
		}
		return s.Token(context.Background())
	}
	// The padded answer is valid JSON, so only the limit can refuse it.
	if tok, err := serve(t, 4*maxTokenResponse); tok != "" || !providers.IsRetryable(err) {
		t.Errorf("Token = %q, %v, want an empty token and a retryable error", tok, err)
	}
	if tok, err := serve(t, maxTokenResponse+1); tok != "" || !providers.IsRetryable(err) {
		t.Errorf("one byte over the limit: Token = %q, %v", tok, err)
	}
	if tok, err := serve(t, maxTokenResponse); err != nil || tok != issuedMark {
		t.Errorf("an answer of exactly the limit: Token = %q, %v", tok, err)
	}
}

// The cache follows the exp this package read with the exact name, not one that encoding/json would take
// for another spelling: a later EXP, valid or not, must not lengthen the life of the cached token.
func TestExpiryIsReadByExactNameOnly(t *testing.T) {
	start := time.Now().Truncate(time.Second)
	short := start.Add(30 * time.Second).Unix()
	for name, tc := range map[string]struct {
		payload string
		refresh time.Duration
	}{
		"exp then EXP":   {fmt.Sprintf(`{"sub":%q,"aud":[%q],"exp":%d,"EXP":%d}`, testSubject, testProjection, short, start.Add(2*time.Hour).Unix()), 15 * time.Second},
		"EXP not a time": {fmt.Sprintf(`{"sub":%q,"aud":[%q],"exp":%d,"EXP":"x"}`, testSubject, testProjection, short), 15 * time.Second},
		"lone Exp":       {fmt.Sprintf(`{"sub":%q,"aud":[%q],"Exp":%d}`, testSubject, testProjection, start.Add(2*time.Hour).Unix()), unknownProjectionLifetime},
	} {
		t.Run(name, func(t *testing.T) {
			s, n := countingSource(t, 3600, encodeJWT(tc.payload))
			now := start
			s.now = func() time.Time { return now }
			if _, err := s.Token(context.Background()); err != nil {
				t.Fatal(err)
			}
			now = start.Add(tc.refresh - time.Nanosecond)
			if _, err := s.Token(context.Background()); err != nil || n.Load() != 1 {
				t.Fatalf("replaced early: requests %d, err %v", n.Load(), err)
			}
			now = start.Add(tc.refresh)
			if _, err := s.Token(context.Background()); err != nil || n.Load() != 2 {
				t.Errorf("not replaced at %v: requests %d, err %v", tc.refresh, n.Load(), err)
			}
		})
	}
}

// A projection that expires while the answer is on the way gives no token, whatever lifetime STS names.
func TestTokenIsRefusedWhenTheProjectionExpiredInTransit(t *testing.T) {
	base := time.Now().Truncate(time.Second)
	var elapsed atomic.Int64
	cfg := testConfig(t)
	writeFile(t, cfg.TokenFile, jwtExpiringAt(base.Add(10*time.Second).Unix()))
	s, err := newSource(cfg, rtFunc(func(r *http.Request) (*http.Response, error) {
		elapsed.Store(int64(15 * time.Second))
		return okResponse(r, http.StatusOK, io.NopCloser(strings.NewReader(stsAnswer(3600))))
	}))
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return base.Add(time.Duration(elapsed.Load())) }
	tok, err := s.Token(context.Background())
	if tok != "" || !providers.IsRetryable(err) {
		t.Fatalf("Token = %q, %v, want an empty token and a retryable error", tok, err)
	}
	assertNoLeak(t, err, cfg)
	s.mu.Lock()
	cached := s.cachedToken()
	s.mu.Unlock()
	if cached != "" {
		t.Error("a token for an expired projection was cached")
	}
}

// Formatting a Source never shows the cached token or the file path, in any verb, and does not read the
// fields a refresh writes.
func TestSourceFormatsAsAPlaceholder(t *testing.T) {
	s, _, cfg := harness(t, func(rw http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(rw, stsAnswer(3600)) })
	if _, err := s.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	logger.Info("source", "source", s)
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%d", "%x"} {
		out := fmt.Sprintf(verb, s) + " " + logs.String()
		for _, secret := range []string{issuedMark, cfg.TokenFile} {
			if strings.Contains(out, secret) {
				t.Errorf("%s shows %q", verb, secret)
			}
		}
	}
}

// The token file is read at each exchange: a kubelet rotation (the ..data symlink swapped to a new
// directory) is what the next exchange sends.
func TestRotatedTokenFileIsUsedByTheNextExchange(t *testing.T) {
	dir := t.TempDir()
	for _, d := range []string{"v1", "v2"} {
		if err := os.Mkdir(filepath.Join(dir, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	jwt1 := encodeJWT(claimsWithExp(time.Now().Add(24 * time.Hour).Unix()))
	jwt2 := encodeJWT(strings.Replace(claimsWithExp(time.Now().Add(24*time.Hour).Unix()), claimMark, "SECOND", 1))
	if jwt1 == jwt2 {
		t.Fatal("the two projected tokens are equal")
	}
	writeFile(t, filepath.Join(dir, "v1", "token"), jwt1)
	writeFile(t, filepath.Join(dir, "v2", "token"), jwt2)
	swap := func(to string) {
		tmp := filepath.Join(dir, "..data_tmp")
		_ = os.Remove(tmp)
		if err := os.Symlink(to, tmp); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, filepath.Join(dir, "..data")); err != nil {
			t.Fatal(err)
		}
	}
	swap("v1")
	if err := os.Symlink(filepath.Join("..data", "token"), filepath.Join(dir, "token")); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var sent []string
	cfg := testConfig(t)
	cfg.TokenFile = filepath.Join(dir, "token")
	s, err := newSource(cfg, rtFunc(func(r *http.Request) (*http.Response, error) {
		_ = r.ParseForm()
		mu.Lock()
		sent = append(sent, r.PostForm.Get("subject_token"))
		mu.Unlock()
		return okResponse(r, http.StatusOK, io.NopCloser(strings.NewReader(stsAnswer(3600))))
	}))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	s.now = func() time.Time { return now }
	if _, err := s.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	swap("v2")
	now = now.Add(time.Minute)
	if _, err := s.Token(context.Background()); err != nil || len(sent) != 1 {
		t.Fatalf("the cache was not served inside its life: %d exchanges, err %v", len(sent), err)
	}
	now = now.Add(time.Hour)
	if _, err := s.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 2 || sent[0] != jwt1 || sent[1] != jwt2 {
		t.Errorf("exchanges sent the projected tokens %v, want the first then the rotated one", len(sent))
	}
}

// A token that expires while the exchange is still closing its response body is not handed to anyone, and a
// caller that joins during that time gets a retryable error, not a token that expired before it joined.
func TestTokenExpiredAtPublishIsNotHandedOut(t *testing.T) {
	gate := &closeGate{Reader: strings.NewReader(stsAnswer(1)), entered: make(chan struct{}), release: make(chan struct{})}
	base := time.Now()
	var elapsed atomic.Int64
	s, err := newSource(testConfig(t), rtFunc(func(r *http.Request) (*http.Response, error) {
		return okResponse(r, http.StatusOK, gate)
	}))
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return base.Add(time.Duration(elapsed.Load())) }
	type result struct {
		tok string
		err error
	}
	first, second := make(chan result, 1), make(chan result, 1)
	go func() { tok, err := s.Token(context.Background()); first <- result{tok, err} }()
	select {
	case <-gate.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the exchange never reached the end of its response")
	}
	elapsed.Store(int64(2 * time.Second)) // the one-second token is now expired
	go func() { tok, err := s.Token(context.Background()); second <- result{tok, err} }()
	for { // wait until the second caller has joined the exchange
		s.mu.Lock()
		joined := s.inflight != nil && s.inflight.waiters == 2
		s.mu.Unlock()
		if joined {
			break
		}
		time.Sleep(time.Millisecond)
	}
	close(gate.release)
	for name, ch := range map[string]chan result{"first": first, "second": second} {
		select {
		case r := <-ch:
			if r.tok != "" || !providers.IsRetryable(r.err) || providers.IsForbidden(r.err) {
				t.Errorf("%s caller: %q, %v, want an empty token and a retryable error", name, r.tok, r.err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s caller never returned", name)
		}
	}
	s.mu.Lock()
	cached := s.cachedToken()
	s.mu.Unlock()
	if cached != "" {
		t.Error("an expired token was cached")
	}
}

// The exchange deadline covers reading the token file: a read that stalls, as a regular file on a stalled
// filesystem can, does not hold every caller past it.
func TestStalledTokenFileReadEndsAtTheDeadline(t *testing.T) {
	s, n, cfg := harness(t, func(rw http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(rw, stsAnswer(3600)) })
	s.timeout = 50 * time.Millisecond
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	s.readFile = func(path string) (string, error) {
		entered <- struct{}{}
		<-gate
		return readTokenFile(path)
	}
	done := make(chan error, 1)
	go func() { _, err := s.Token(context.Background()); done <- err }()
	select {
	case err := <-done:
		if !providers.IsRetryable(err) || !errors.Is(err, context.DeadlineExceeded) || providers.IsForbidden(err) {
			t.Errorf("err = %v, want a retryable deadline", err)
		}
		assertNoLeak(t, err, cfg)
	case <-time.After(5 * time.Second):
		t.Error("the caller is still blocked on the stalled read")
	}
	close(gate)
	<-entered
	if n.Load() != 0 {
		t.Errorf("sent %d requests for a read that ended after the deadline", n.Load())
	}
}

// A stalled read keeps its slot until it returns: retries while it is stuck start no further reads, so
// goroutines and open files do not pile up, and the slot is free again once the read ends.
func TestStalledTokenFileReadsAreNotStackedByRetries(t *testing.T) {
	s, _, _ := harness(t, func(rw http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(rw, stsAnswer(3600)) })
	s.timeout = 20 * time.Millisecond
	gate := make(chan struct{})
	var reads atomic.Int32
	s.readFile = func(path string) (string, error) {
		if reads.Add(1) == 1 {
			<-gate
		}
		return readTokenFile(path)
	}
	for i := range 5 {
		_, err := s.Token(context.Background())
		if !providers.IsRetryable(err) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("attempt %d: err = %v, want a retryable deadline", i, err)
		}
	}
	if got := reads.Load(); got != 1 {
		t.Errorf("%d reads started while the first was stuck, want 1", got)
	}
	close(gate)
	var tok string
	var err error
	for range 200 {
		if tok, err = s.Token(context.Background()); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil || tok == "" {
		t.Fatalf("no token after the read returned: %v", err)
	}
}

// HTTP/1.1 does not by itself keep bytes out of the standard log: when a server sends more than its
// Content-Length declares, net/http logs the start of the extra bytes as an unsolicited response. The answer
// of STS holds the access token, so the production transport must not leave a connection to be read like that.
func TestBytesPastTheDeclaredLengthAreNotLogged(t *testing.T) {
	const marker = "SYNTHETIC-TOKEN-MARK-0123456789"
	var logs syncBuffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(prevOut); log.SetFlags(prevFlags) })

	srv := httptest.NewTLSServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		conn, buf, err := rw.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		answer := stsAnswer(3600)
		// The header declares the length of the answer, and the marker follows it.
		_, _ = fmt.Fprintf(buf, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s%s",
			len(answer), answer, marker)
		_ = buf.Flush()
		time.Sleep(500 * time.Millisecond) // keep the connection open, as a server that reuses it would
	}))
	defer srv.Close()

	s, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	// The production transport, with the dial guard replaced and the fixture's certificate trusted.
	tr := s.client.Transport.(*http.Transport).Clone()
	tr.DialContext = (&net.Dialer{}).DialContext
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	tr.TLSClientConfig.RootCAs = pool
	s.client.Transport = tr
	s.tokenURL = srv.URL + "/v1/token"
	if tok, err := s.Token(context.Background()); err != nil || tok != issuedMark {
		t.Fatalf("Token = %q, %v", tok, err)
	}
	time.Sleep(600 * time.Millisecond) // the transport reads an idle connection in the background
	if strings.Contains(logs.String(), marker) || strings.Contains(logs.String(), "Unsolicited") {
		t.Errorf("the standard log holds bytes the server sent past its declared length: %q", logs.String())
	}
}

// syncBuffer is a bytes.Buffer safe for the transport's goroutines to log into.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
