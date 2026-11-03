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
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"regexp"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/pkalin/shadewright/internal/providers"
)

// failingTransport fails every request with err, as a broken connection or an SDK failure would.
type failingTransport struct{ err error }

func (f failingTransport) Do(*http.Request) (*http.Response, error) { return nil, f.err }

// sdkText stands for text an SDK or the network stack puts in an error: response data, a request URL with
// a query, a header and a token. None of it may stay anywhere in the chain Sign or New returns.
const sdkText = "GET https://" + testHost + "/keys/github-app?sig=" + testToken + " Bearer " + bodySecret

// chain returns err and every error it wraps, following both Unwrap() error and Unwrap() []error.
func chain(err error) []error {
	var out []error
	queue := []error{err}
	for len(queue) > 0 {
		e := queue[0]
		queue = queue[1:]
		if e == nil {
			continue
		}
		out = append(out, e)
		switch u := e.(type) {
		case interface{ Unwrap() error }:
			queue = append(queue, u.Unwrap())
		case interface{ Unwrap() []error }:
			queue = append(queue, u.Unwrap()...)
		}
	}
	return out
}

// allowedChainPackages are the only packages whose error types may appear in a chain that leaves this
// package: its own fixed errors, the providers kinds, the wrappers of fmt and errors, and context errors.
// It catches a typed error from anywhere else (the Azure SDK, net, net/url, net/http, crypto/tls,
// crypto/x509, os, syscall). It cannot catch SDK text carried by an errors.New or fmt.Errorf value, since
// those types are allowed: the text checks in assertCleanChain are what catch that.
var allowedChainPackages = map[string]bool{
	reflect.TypeFor[fixedError]().PkgPath():               true,
	reflect.TypeFor[providers.RetryableError]().PkgPath(): true,
	"fmt":     true,
	"errors":  true,
	"context": true,
}

// allowedChainType reports whether e has a type from allowedChainPackages.
func allowedChainType(e error) bool {
	typ := reflect.TypeOf(e)
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	return allowedChainPackages[typ.PkgPath()]
}

// configuredKey matches the key ID that errors name on purpose ("sign with <key ID>"). It is configuration,
// not SDK text, so it is removed before the URL check.
var configuredKey = regexp.MustCompile(`https://` + regexp.QuoteMeta(testHost) + `/keys/[a-z-]+(/[0-9a-f]{32})?`)

// leakText is text no error may carry: the test token and response text, URL parts (a scheme, a query, a
// user) and the names of headers that hold tokens or response data.
var leakText = []string{
	testToken, bodySecret, "Bearer", "sig=", "https://", "http://", "?", "@",
	"Authorization", "WWW-Authenticate", "Content-Type", "x-ms-",
}

// assertCleanChain fails if any error in the chain of err, printed with %v or %+v, carries SDK, URL,
// header or token text, or has a type outside allowedChainPackages.
func assertCleanChain(t *testing.T, name string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: no error", name)
	}
	for i, e := range chain(err) {
		text := e.Error() + "\n" + fmt.Sprintf("%v", e) + "\n" + fmt.Sprintf("%+v", e)
		text = configuredKey.ReplaceAllString(text, "<key>")
		for _, bad := range leakText {
			if strings.Contains(text, bad) {
				t.Errorf("%s: chain[%d] (%T) leaks %q: %v", name, i, e, bad, e)
				break
			}
		}
		if !allowedChainType(e) {
			t.Errorf("%s: chain[%d] is a %T, which a caller could unwrap and print", name, i, e)
		}
	}
}

// TestAllowedChainType checks that the allow-list catches the error types of the network stack, TLS, the
// file system and the HTTP client, and accepts the ones this package wraps.
func TestAllowedChainType(t *testing.T) {
	for _, e := range []error{
		&url.Error{Op: http.MethodPost, URL: "https://" + testHost, Err: errors.New("x")},
		&net.OpError{Op: opDial, Err: errors.New("x")},
		&net.DNSError{Err: "no such host", Name: testHost},
		x509.UnknownAuthorityError{},
		x509.HostnameError{Host: testHost},
		&tls.CertificateVerificationError{Err: errors.New("x")},
		tls.RecordHeaderError{Msg: "x"},
		&fs.PathError{Op: "open", Path: "/var/run/secrets/token", Err: syscall.ENOENT},
		syscall.ECONNRESET,
		&http.MaxBytesError{Limit: 1},
	} {
		if allowedChainType(e) {
			t.Errorf("%T passes the allow-list", e)
		}
	}
	for _, e := range []error{
		errRequestNetwork, final{errRequestTLS}, &credentialError{}, &providers.RetryableError{},
		fmt.Errorf("x: %w", errRequestSDK), errors.Join(errRequestSDK), errors.New("x"),
		context.Canceled, context.DeadlineExceeded,
	} {
		if !allowedChainType(e) {
			t.Errorf("%T is refused by the allow-list", e)
		}
	}
}

// TestSignErrorChainHasNoSDKText walks the whole chain of every Sign failure class and checks that no
// SDK, URL, header or token text survives anywhere in it, while the provider kinds still match.
func TestSignErrorChainHasNoSDKText(t *testing.T) {
	key, err := ParseKeyID("https://" + testHost + "/keys/github-app")
	if err != nil {
		t.Fatal(err)
	}
	urlErr := func(inner error) error {
		return &url.Error{Op: http.MethodPost, URL: "https://" + testHost + "/keys/github-app?sig=" + testToken, Err: inner}
	}
	for _, tc := range []struct {
		name      string
		err       error
		retryable bool
		class     error
	}{
		{"network", urlErr(&net.OpError{Op: "read", Net: "tcp", Err: errors.New(sdkText)}), true, errRequestNetwork},
		{"dns", urlErr(&net.DNSError{Err: sdkText, Name: testHost, IsNotFound: true}), true, errRequestNetwork},
		{"tls", urlErr(&tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}), false, errRequestTLS},
		{"sdk", errors.New(sdkText), false, errRequestSDK},
	} {
		s, err := newSigner(key, &fakeCredential{}, Options{MaxRetries: -1}, clientConfig{
			transport:  failingTransport{err: tc.err},
			retryDelay: time.Millisecond,
		})
		if err != nil {
			t.Fatal(err)
		}
		_, err = s.Sign(context.Background(), digestOf("x"))
		assertCleanChain(t, tc.name, err)
		if providers.IsRetryable(err) != tc.retryable {
			t.Errorf("%s: retryable = %v, want %v: %v", tc.name, providers.IsRetryable(err), tc.retryable, err)
		}
		if !errors.Is(err, tc.class) {
			t.Errorf("%s: err = %v, want it to match its class %v", tc.name, err, tc.class)
		}
	}

	// Response errors and token request failures go through the SDK's own error types.
	s, v, _ := setup(t, "https://"+testHost+"/keys/github-app", Options{MaxRetries: -1})
	v.respond = func(_ int, w http.ResponseWriter, _ *http.Request, _ []byte) {
		writeError(w, http.StatusServiceUnavailable, "ServiceUnavailable")
	}
	_, err = s.Sign(context.Background(), digestOf("x"))
	assertCleanChain(t, "response", err)
	if !providers.IsRetryable(err) {
		t.Errorf("response: err = %v, want retryable", err)
	}

	s, v, cred := setup(t, "https://"+testHost+"/keys/github-app", Options{MaxRetries: -1})
	v.respond = v.signOK("https://" + testHost + "/keys/github-app/" + testVersion)
	cred.err = authFailure(http.StatusServiceUnavailable, "")
	_, err = s.Sign(context.Background(), digestOf("x"))
	assertCleanChain(t, "credential", err)
	if !providers.IsRetryable(err) {
		t.Errorf("credential: err = %v, want retryable", err)
	}

	// The guard's refusals: a 401 challenge it does not trust, and a response over the size limit.
	s, v, _ = setup(t, "https://"+testHost+"/keys/github-app", Options{MaxRetries: -1})
	v.challenge = `Bearer authorization="` + bodySecret + `", resource="https://vault.azure.net"`
	_, err = s.Sign(context.Background(), digestOf("x"))
	assertCleanChain(t, "malformed challenge", err)
	if !errors.Is(err, errMalformedChallenge) || providers.IsRetryable(err) {
		t.Errorf("malformed challenge: err = %v, want the final malformed challenge error", err)
	}

	s, v, _ = setup(t, "https://"+testHost+"/keys/github-app", Options{MaxRetries: -1})
	v.respond = func(_ int, w http.ResponseWriter, _ *http.Request, _ []byte) {
		writeJSON(w, http.StatusOK, map[string]string{kidField: bodySecret, valueField: strings.Repeat("a", maxResponseBody)})
	}
	_, err = s.Sign(context.Background(), digestOf("x"))
	assertCleanChain(t, "oversized response", err)
	if !errors.Is(err, errResponseTooLarge) || providers.IsRetryable(err) {
		t.Errorf("oversized response: err = %v, want the final response too large error", err)
	}
}

// clientTo sends every request to target through client.Do, so errors come back as the HTTP client returns
// them in production: a *url.Error around the network or TLS error.
type clientTo struct {
	target *url.URL
	client *http.Client
}

func (c clientTo) Do(req *http.Request) (*http.Response, error) {
	out := req.Clone(req.Context())
	out.URL.Scheme = c.target.Scheme
	out.URL.Host = c.target.Host
	out.Host = req.URL.Host
	return c.client.Do(out)
}

// TestTransportErrorClasses checks the class of real HTTP client failures: a refused certificate is final, a
// TLS alert from the server and a refused connection are retryable, a client error such as an https request
// to a plain HTTP server is final, and none leaves text in the chain.
func TestTransportErrorClasses(t *testing.T) {
	kid := "https://" + testHost + "/keys/github-app"
	key, err := ParseKeyID(kid)
	if err != nil {
		t.Fatal(err)
	}
	vault := &fakeVault{key: testRSAKey()}
	vault.respond = vault.signOK(kid + "/" + testVersion)

	trusted := httpsServer(t, vault, nil)
	tls13 := httpsServer(t, vault, &tls.Config{MinVersion: tls.VersionTLS13})
	plain := httptest.NewServer(vault)
	plain.Config.ErrorLog = log.New(io.Discard, "", 0)
	t.Cleanup(plain.Close)
	plainURL, _ := url.Parse(plain.URL)
	plainURL.Scheme = "https"
	closed := closedAddress(t)

	trustingClient := func(srv *serverInfo, cfg func(*tls.Config)) *http.Client {
		tr := srv.client.Transport.(*http.Transport).Clone()
		cfg(tr.TLSClientConfig)
		return &http.Client{Transport: tr}
	}

	for _, tc := range []struct {
		name      string
		transport clientTo
		retryable bool
		class     error
	}{
		{"unknown authority", clientTo{trusted.url, &http.Client{}}, false, errRequestTLS},
		{"hostname mismatch", clientTo{trusted.url, trustingClient(trusted, func(c *tls.Config) {
			c.ServerName = "other.example"
		})}, false, errRequestTLS},
		{"alert from the server", clientTo{tls13.url, trustingClient(tls13, func(c *tls.Config) {
			c.MaxVersion = tls.VersionTLS12
		})}, true, errRequestNetwork},
		{"https to a plain server", clientTo{plainURL, &http.Client{}}, false, errRequestSDK},
		{"connection refused", clientTo{closed, &http.Client{}}, true, errRequestNetwork},
	} {
		s, err := newSigner(key, &fakeCredential{}, Options{MaxRetries: -1}, clientConfig{
			transport:  tc.transport,
			retryDelay: time.Millisecond,
		})
		if err != nil {
			t.Fatal(err)
		}
		_, err = s.Sign(context.Background(), digestOf("x"))
		assertCleanChain(t, tc.name, err)
		if providers.IsRetryable(err) != tc.retryable || !errors.Is(err, tc.class) {
			t.Errorf("%s: err = %v (retryable %v), want %v with retryable %v",
				tc.name, err, providers.IsRetryable(err), tc.class, tc.retryable)
		}
	}
}

// brokenBody fails every read with err, as a truncated or reset response body does.
type brokenBody struct{ err error }

func (b brokenBody) Read([]byte) (int, error) { return 0, b.err }
func (brokenBody) Close() error               { return nil }

// scriptedTransport answers every request with do and counts the requests.
type scriptedTransport struct {
	do func(*http.Request) (*http.Response, error)
	n  atomic.Int32
}

func (s *scriptedTransport) Do(req *http.Request) (*http.Response, error) {
	s.n.Add(1)
	return s.do(req)
}

// TestRetryDecisionsAgree checks, for every class guardTransport returns, that azcore and Sign make the same
// retry decision: a class azcore retries is retryable from Sign, and a class Sign reports as final is sent
// once.
func TestRetryDecisionsAgree(t *testing.T) {
	key, err := ParseKeyID("https://" + testHost + "/keys/github-app")
	if err != nil {
		t.Fatal(err)
	}
	fail := func(inner error) func(*http.Request) (*http.Response, error) {
		return func(*http.Request) (*http.Response, error) {
			return nil, &url.Error{Op: http.MethodPost, URL: "https://" + testHost + "/keys?sig=" + bodySecret, Err: inner}
		}
	}
	respond := func(status int, h http.Header, body io.ReadCloser, length int64) func(*http.Request) (*http.Response, error) {
		return func(req *http.Request) (*http.Response, error) {
			if h == nil {
				h = http.Header{}
			}
			return &http.Response{StatusCode: status, Header: h.Clone(), Body: body, ContentLength: length, Request: req}, nil
		}
	}
	for _, tc := range []struct {
		name      string
		do        func(*http.Request) (*http.Response, error)
		retryable bool
		class     error
	}{
		{"dial", fail(&net.OpError{Op: opDial, Err: syscall.ECONNREFUSED}), true, errRequestNetwork},
		{"dns", fail(&net.DNSError{Err: "no such host", Name: testHost}), true, errRequestNetwork},
		{"closed before the response", fail(io.EOF), true, errRequestNetwork},
		{"tls alert", fail(&net.OpError{Op: "remote error", Err: errors.New("tls: internal error")}), true,
			errRequestNetwork},
		{"certificate", fail(x509.HostnameError{Host: testHost}), false, errRequestTLS},
		{"scheme mismatch", fail(http.ErrSchemeMismatch), false, errRequestSDK},
		{"proxy refused", fail(errors.New("proxyconnect: " + bodySecret)), false, errRequestSDK},
		{"no response", func(*http.Request) (*http.Response, error) { return nil, nil }, false, errRequestSDK},
		{"truncated body", respond(http.StatusOK, nil, brokenBody{io.ErrUnexpectedEOF}, -1), true, errBodyRead},
		{"reset stream", respond(http.StatusOK, nil, brokenBody{errors.New("stream error: " + bodySecret)}, -1), true,
			errBodyRead},
		{"too large", respond(http.StatusOK, nil, io.NopCloser(strings.NewReader("{}")), maxResponseBody+1), false,
			errResponseTooLarge},
		{"malformed challenge", respond(http.StatusUnauthorized,
			http.Header{"Www-Authenticate": []string{`Bearer authorization="x"`}}, http.NoBody, 0), false,
			errMalformedChallenge},
	} {
		st := &scriptedTransport{do: tc.do}
		s, err := newSigner(key, &fakeCredential{}, Options{MaxRetries: 2}, clientConfig{transport: st, retryDelay: time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		_, err = s.Sign(context.Background(), digestOf("x"))
		assertCleanChain(t, tc.name, err)
		want := int32(1)
		if tc.retryable {
			want = 3
		}
		if got := st.n.Load(); got != want || providers.IsRetryable(err) != tc.retryable || !errors.Is(err, tc.class) {
			t.Errorf("%s: %d requests, err = %v (retryable %v); want %d requests, %v with retryable %v",
				tc.name, got, err, providers.IsRetryable(err), want, tc.class, tc.retryable)
		}
	}
}

// TestNewNamesTheMissingVariable checks that New names the workload identity variable at fault, and only
// that one, without its value.
func TestNewNamesTheMissingVariable(t *testing.T) {
	const secretish = "value-must-never-appear"
	valid := map[string]string{
		envClientID:      "00000000-0000-0000-0000-000000000001",
		envTenantID:      "00000000-0000-0000-0000-000000000002",
		envTokenFile:     "/var/run/secrets/azure/tokens/azure-identity-token",
		envAuthorityHost: "",
	}
	const notAuthority = envAuthorityHost + " is not a known Entra ID authority host"
	for _, tc := range []struct {
		name, value, want string
	}{
		{envClientID, "", envClientID + " is not set"},
		{envTenantID, "", envTenantID + " is not set"},
		{envTokenFile, "", envTokenFile + " is not set"},
		{envTenantID, secretish + "/../x", "azidentity refused " + envTenantID},
		{envAuthorityHost, "http://login.microsoftonline.com/", notAuthority},
		{envAuthorityHost, "https://" + secretish + ".example/", notAuthority},
		{envAuthorityHost, "https://login.microsoftonline.com.example/", notAuthority},
		{envAuthorityHost, "https://login.microsoftonline.com/" + secretish, notAuthority},
		{envAuthorityHost, "https://" + secretish + "@login.microsoftonline.com/", notAuthority},
		{envAuthorityHost, "https://login.microsoftonline.com/?" + secretish, notAuthority},
		{envAuthorityHost, "https://login.microsoftonline.com:8443/", notAuthority},
	} {
		for k, v := range valid {
			t.Setenv(k, v)
		}
		t.Setenv(tc.name, tc.value)
		_, err := New("https://"+testHost+"/keys/github-app", Options{})
		assertCleanChain(t, tc.name, err)
		if !errors.Is(err, errNoWorkloadIdentity) || !strings.HasSuffix(err.Error(), tc.want) {
			t.Errorf("%s=%q: err = %v, want the workload identity error ending in %q", tc.name, tc.value, err, tc.want)
		}
		if strings.Contains(err.Error(), secretish) {
			t.Errorf("%s: the error quotes the value: %v", tc.name, err)
		}
		for other := range valid {
			if other != tc.name && strings.Contains(err.Error(), other) {
				t.Errorf("%s: the error also names %s: %v", tc.name, other, err)
			}
		}
	}

	for _, host := range []string{"", "https://login.microsoftonline.com/", "https://login.microsoftonline.us",
		"https://login.chinacloudapi.cn/"} {
		for k, v := range valid {
			t.Setenv(k, v)
		}
		t.Setenv(envAuthorityHost, host)
		if _, err := New("https://"+testHost+"/keys/github-app", Options{}); err != nil {
			t.Errorf("valid variables with authority host %q: %v", host, err)
		}
	}
}

// TestNewErrorChainHasNoSDKText checks that a missing workload identity setup fails with fixed text and no
// azidentity error in the chain.
func TestNewErrorChainHasNoSDKText(t *testing.T) {
	for _, name := range []string{envClientID, envTenantID, envTokenFile} {
		t.Setenv(name, "")
	}
	_, err := New("https://"+testHost+"/keys/github-app", Options{})
	assertCleanChain(t, "new", err)
	if !errors.Is(err, errNoWorkloadIdentity) {
		t.Errorf("err = %v, want the workload identity error", err)
	}
}

// serverInfo is a started test server: its URL and a client that trusts its certificate.
type serverInfo struct {
	url    *url.URL
	client *http.Client
}

// httpsServer starts a TLS test server for h, with cfg as its TLS configuration when given.
func httpsServer(t *testing.T, h http.Handler, cfg *tls.Config) *serverInfo {
	t.Helper()
	srv := httptest.NewUnstartedServer(h)
	srv.TLS = cfg
	srv.Config.ErrorLog = log.New(io.Discard, "", 0) // the refused handshakes are expected
	srv.StartTLS()
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	return &serverInfo{url: u, client: srv.Client()}
}

// opDial is the operation of a *net.OpError for a failed connection.
const opDial = "dial"

// closedAddress returns an https URL on a local port nothing listens on.
func closedAddress(t *testing.T) *url.URL {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return &url.URL{Scheme: "https", Host: addr}
}

// TestMain keeps environment variables of the machine running the tests out of New.
func TestMain(m *testing.M) {
	for _, name := range []string{envClientID, envTenantID, envTokenFile, envAuthorityHost} {
		_ = os.Unsetenv(name)
	}
	os.Exit(m.Run())
}
