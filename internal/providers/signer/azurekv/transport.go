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
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
)

// maxResponseBody bounds every Key Vault response body. A sign response holds a key identifier and at most
// a 512-byte signature, an error response a short JSON error; both are far below it.
const maxResponseBody = 64 << 10

// guardTransport sits below every azcore policy, so it sees each response before anything in the SDK
// reads it:
//
//   - It reads at most maxResponseBody+1 bytes of a body and refuses a larger one, so azcore's body
//     download policy never buffers an unbounded response (success or error).
//   - It refuses a 401 challenge that the Key Vault SDK would parse unsafely or reject with an error that
//     quotes it: keyvault/internal v1.2.0 takes the tenant as strings.Split(authorization, "/")[3] without
//     a length check, so authorization="not-a-url" would panic inside Sign, and it copies an unparsable or
//     foreign resource into an error message that azcore's retry policy logs.
//   - It replaces every error of the HTTP client with a fixed one (cleanTransportError), so neither the
//     error chain nor azcore's retry log, which prints transport errors, sees the request URL or the
//     network stack's text.
//
// Both refusals are fixed errors that carry no response text and are not retried.
type guardTransport struct {
	inner policy.Transporter
}

// Do sends the request and checks the response.
func (g guardTransport) Do(req *http.Request) (*http.Response, error) {
	resp, err := g.inner.Do(req)
	if err != nil {
		return nil, cleanTransportError(err)
	}
	if resp == nil {
		return nil, errRequestSDK
	}
	if resp.StatusCode == http.StatusUnauthorized && !safeChallenge(resp.Header.Get("WWW-Authenticate"), req.URL.Host) {
		closeBody(resp)
		return nil, final{errMalformedChallenge}
	}
	if resp.Body == nil || resp.Body == http.NoBody {
		return resp, nil
	}
	if resp.ContentLength > maxResponseBody {
		closeBody(resp)
		return nil, final{errResponseTooLarge}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody+1))
	closeBody(resp)
	if err != nil {
		return nil, cleanTransportError(err)
	}
	if len(body) > maxResponseBody {
		return nil, final{errResponseTooLarge}
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
	return resp, nil
}

// closeBody closes a response body without reading the rest of it.
func closeBody(resp *http.Response) {
	if resp.Body != nil {
		_ = resp.Body.Close()
	}
}

// safeChallenge reports whether the Key Vault SDK can handle a WWW-Authenticate value without panicking
// and without copying response text into an error. It splits the value exactly as keyvault/internal v1.2.0
// does, then requires that a present authorization parameter is an https URL with a tenant segment, and
// that the scope (or resource) is an https URL whose host is a parent domain of vaultHost, which is the
// SDK's own resource check. An empty header or a missing resource is left to the SDK, whose errors for both
// are fixed text.
func safeChallenge(header, vaultHost string) bool {
	vals := map[string]string{}
	for part := range strings.SplitSeq(strings.ReplaceAll(header, "Bearer ", ""), " ") {
		sub := strings.Split(part, "=")
		if len(sub) == 2 {
			vals[sub[0]] = strings.TrimSuffix(strings.ReplaceAll(sub[1], `"`, ""), ",")
		}
	}
	if auth := vals["authorization"]; auth != "" {
		parts := strings.Split(auth, "/")
		if len(parts) < 4 || parts[0] != "https:" || parts[1] != "" || parts[2] == "" || parts[3] == "" {
			return false
		}
	}
	scope := vals["scope"]
	if scope == "" {
		scope = vals["resource"]
	}
	if scope == "" {
		return true
	}
	u, err := url.Parse(scope)
	return err == nil && u.Scheme == schemeHTTPS && u.User == nil && u.Host != "" &&
		strings.HasSuffix(vaultHost, "."+u.Host)
}

// final marks a fixed error as final for azcore's retry policy (errorinfo.NonRetriable), so a refusal is
// not sent again. errors.Is and errors.As still find the fixed error through Unwrap.
type final struct{ fixedError }

// NonRetriable marks the error as final for azcore.
func (final) NonRetriable() {}

// Unwrap returns the fixed error.
func (e final) Unwrap() error { return e.fixedError }

// cleanTransportError replaces an HTTP client error with a fixed one. Not wrapped: a *url.Error quotes the
// request URL, and the network stack's errors name addresses and certificates.
//
//   - Context errors (the try's or the caller's deadline, a cancellation) are kept, so azcore and mapError
//     still tell a timeout from a cancellation.
//   - TLS and certificate failures are errRequestTLS, final: a wrong certificate or a refused handshake
//     does not heal on retry.
//   - Other network failures (DNS, dial, connection reset, the response header timeout) are
//     errRequestNetwork, which azcore retries and Sign returns as retryable.
//   - Anything else is errRequestSDK. azcore retries it as it retried the raw error before; Sign returns
//     it as final.
func cleanTransportError(err error) error {
	switch {
	case errors.Is(err, context.Canceled):
		return context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return context.DeadlineExceeded
	case isTLSError(err):
		return final{errRequestTLS}
	}
	if _, ok := errors.AsType[net.Error](err); ok {
		return errRequestNetwork
	}
	return errRequestSDK
}

// isTLSError reports whether err is a TLS or certificate failure. Certificate and record errors have types.
// Most handshake failures do not: crypto/tls returns an untyped error, or a *net.OpError around an
// unexported alert, whose text starts with "tls: ". So the innermost error's text decides for those. A TLS
// handshake timeout ("net/http: TLS handshake timeout") is not matched and stays a network failure.
func isTLSError(err error) bool {
	if _, ok := errors.AsType[*tls.CertificateVerificationError](err); ok {
		return true
	}
	if _, ok := errors.AsType[x509.UnknownAuthorityError](err); ok {
		return true
	}
	if _, ok := errors.AsType[x509.HostnameError](err); ok {
		return true
	}
	if _, ok := errors.AsType[x509.CertificateInvalidError](err); ok {
		return true
	}
	if _, ok := errors.AsType[tls.RecordHeaderError](err); ok {
		return true
	}
	if _, ok := errors.AsType[tls.AlertError](err); ok {
		return true
	}
	for {
		next := errors.Unwrap(err)
		if next == nil {
			return strings.HasPrefix(err.Error(), "tls: ")
		}
		err = next
	}
}

// defaultTransport is the HTTP client used when tests do not replace it: the standard transport with
// TLS 1.2 or later, bounded connection setup, and no redirects. Key Vault never redirects, and Go's client
// keeps the Authorization header on a redirect to the same host even from https to http, so a redirect is
// returned as a response (and fails as an unexpected status) instead of being followed.
func defaultTransport() policy.Transporter {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	t.TLSHandshakeTimeout = 10 * time.Second
	t.ResponseHeaderTimeout = tryTimeout
	return &http.Client{
		Transport: t,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}
