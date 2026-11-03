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
//   - On the Key Vault client, it refuses a 401 challenge that the Key Vault SDK would parse unsafely or
//     reject with an error that quotes it: keyvault/internal v1.2.0 takes the tenant as
//     strings.Split(authorization, "/")[3] without a length check, so authorization="not-a-url" would panic
//     inside Sign, and it copies an unparsable or foreign resource into an error message that azcore's
//     retry policy logs.
//   - It replaces every error of the HTTP client, and every body read error, with a fixed one or a context
//     error (cleanTransportError, cleanBodyError), so neither the error chain nor azcore's retry log, which
//     prints transport errors, sees the request URL or the network stack's text.
//   - On the credential's client (entra), it records the outcome of each exchange for the token request
//     in the request's context (tokenRequest), because azidentity passes on only an error's text.
//
// Every error it returns carries no response, URL or network text. Each fixed error gets the same retry
// decision from azcore and from Sign: the final ones (both refusals, a refused certificate, any other
// client failure) are wrapped in [final], so azcore does not send them again; network failures and a body
// that breaks off are left to azcore's retries and returned by Sign as retryable.
type guardTransport struct {
	inner policy.Transporter
	// entra is set on the credential's client: Entra ID sends no Key Vault challenge, so none is checked.
	entra bool
}

// Do sends the request, checks the response and records the outcome for a token request.
func (g guardTransport) Do(req *http.Request) (*http.Response, error) {
	resp, err := g.do(req)
	if r := tokenRequestFrom(req.Context()); r != nil && g.entra {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		r.recordExchange(status, err)
	}
	return resp, err
}

func (g guardTransport) do(req *http.Request) (*http.Response, error) {
	resp, err := g.inner.Do(req)
	if err != nil {
		return nil, cleanTransportError(err)
	}
	if resp == nil {
		return nil, final{errRequestSDK}
	}
	if !g.entra && resp.StatusCode == http.StatusUnauthorized && !safeChallenge(resp.Header.Get("WWW-Authenticate"), req.URL.Host) {
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
	// A read that ended after the try or the caller gave up returns the context error, whether or not
	// the read itself failed: the HTTP client's read error does not match it, and a read can also end
	// cleanly with a cut body (closing a TLS connection sends close_notify before the socket closes, and a
	// server that sees it may end a chunked response early). azcore reports a parent context error
	// itself but a per-try deadline only as returned here, so a cut body would otherwise reach the JSON
	// decoder and come back as a final SDK error instead of a retryable timeout.
	if ctxErr := req.Context().Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if err != nil {
		return nil, cleanBodyError(err)
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

// cleanTransportError replaces an HTTP client error with a fixed one or a context error. Not wrapped: a
// *url.Error quotes the request URL, and the network stack's errors name addresses and certificates.
//
//   - Context errors (the try's or the caller's deadline, a cancellation) are kept, so azcore and mapError
//     still tell a timeout from a cancellation.
//   - A certificate the client refused is final{errRequestTLS}: it does not heal on retry.
//   - A known local error is final{errRequestSDK}: http.ErrSchemeMismatch (an https request answered in
//     plain HTTP). An unsupported scheme cannot happen, because the key ID and the authority host are
//     checked to be https, and a proxy refusal has no type to tell it from a network failure.
//   - Anything else is errRequestNetwork, retried by azcore and retryable from Sign. net/http has no type
//     for many transient failures (an idle connection the server closed while a POST was written, HTTP/2
//     GOAWAY, RST_STREAM or a lost client connection, a handshake that broke off), so an unknown error is
//     taken as transient: the retries are bounded, and a wrong guess costs one more try.
func cleanTransportError(err error) error {
	switch {
	case errors.Is(err, context.Canceled):
		return context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return context.DeadlineExceeded
	case isCertificateError(err):
		return final{errRequestTLS}
	case errors.Is(err, http.ErrSchemeMismatch):
		return final{errRequestSDK}
	}
	return errRequestNetwork
}

// cleanBodyError replaces a response body read error. Not wrapped, for the reasons of cleanTransportError.
// Context errors are kept; anything else (a truncated body, a reset connection or HTTP/2 stream) is
// errBodyRead, retried by azcore and retryable from Sign.
func cleanBodyError(err error) error {
	switch {
	case errors.Is(err, context.Canceled):
		return context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return context.DeadlineExceeded
	}
	return errBodyRead
}

// isCertificateError reports whether err is a certificate the client refused. Only certificate verification
// is final: TLS alerts (handshake_failure, internal_error) can come from a server that is restarting or
// overloaded, so they stay network failures.
func isCertificateError(err error) bool {
	if _, ok := errors.AsType[*tls.CertificateVerificationError](err); ok {
		return true
	}
	if _, ok := errors.AsType[x509.UnknownAuthorityError](err); ok {
		return true
	}
	if _, ok := errors.AsType[x509.HostnameError](err); ok {
		return true
	}
	_, ok := errors.AsType[x509.CertificateInvalidError](err)
	return ok
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
