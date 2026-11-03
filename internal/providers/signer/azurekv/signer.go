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
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/security/keyvault/azkeys"

	"github.com/pkalin/shadewright/internal/providers"
)

var _ providers.Signer = (*Signer)(nil)

// Defaults for [Options].
const (
	DefaultTimeout    = 30 * time.Second
	DefaultMaxRetries = 3

	tryTimeout    = 10 * time.Second
	retryDelay    = 1 * time.Second
	maxRetryDelay = 10 * time.Second
)

// Options tunes a Signer. The zero value uses the defaults.
type Options struct {
	// Timeout bounds one Sign call, retries included. Zero means DefaultTimeout. A shorter deadline on the
	// caller's context wins.
	Timeout time.Duration
	// MaxRetries is the number of retries after the first attempt on throttling (429), request timeouts and
	// transient server errors. Zero means DefaultMaxRetries; a negative value disables retries.
	MaxRetries int
}

// Signer signs RS256 digests with one Key Vault key. It is safe for concurrent use.
type Signer struct {
	key     KeyID
	client  *azkeys.Client
	timeout time.Duration
}

// New returns a Signer for keyID that authenticates with workload identity (the AZURE_CLIENT_ID,
// AZURE_TENANT_ID and AZURE_FEDERATED_TOKEN_FILE variables that the workload identity webhook injects).
// It makes no network call; the first Sign does.
func New(keyID string, opts Options) (*Signer, error) {
	key, err := ParseKeyID(keyID)
	if err != nil {
		return nil, err
	}
	cred, err := azidentity.NewWorkloadIdentityCredential(nil)
	if err != nil {
		// The azidentity error names the variable at fault but is SDK text; the class is enough.
		return nil, errNoWorkloadIdentity
	}
	return newSigner(key, cred, opts, clientConfig{})
}

// clientConfig carries what tests replace: the HTTP transport and the delay between retries.
type clientConfig struct {
	transport  policy.Transporter
	retryDelay time.Duration
}

func newSigner(key KeyID, cred azcore.TokenCredential, opts Options, cc clientConfig) (*Signer, error) {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	retries := opts.MaxRetries
	switch {
	case retries == 0:
		retries = DefaultMaxRetries
	case retries < 0:
		retries = -1
	case retries > 10:
		retries = 10
	}
	delay := retryDelay
	if cc.retryDelay != 0 {
		delay = cc.retryDelay
	}
	co := &azkeys.ClientOptions{
		ClientOptions: azcore.ClientOptions{
			Retry: policy.RetryOptions{
				MaxRetries:    int32(retries),
				TryTimeout:    min(tryTimeout, timeout),
				RetryDelay:    delay,
				MaxRetryDelay: max(maxRetryDelay, delay),
			},
			// Never log request or response bodies. This package never sets an azcore log listener; if a
			// process sets one, it sees only cleaned credential errors (cleanCredential) and cleaned error
			// responses (errorBodyPolicy).
			Logging:         policy.LogOptions{IncludeBody: false},
			PerCallPolicies: []policy.Policy{errorBodyPolicy{}},
		},
	}
	transport := cc.transport
	if transport == nil {
		transport = defaultTransport()
	}
	co.Transport = guardTransport{inner: transport}
	client, err := azkeys.NewClient(key.VaultURL(), cleanCredential{cred}, co)
	if err != nil {
		return nil, fmt.Errorf("key vault client for %s: %w", key, errClientSetup)
	}
	return &Signer{key: key, client: client, timeout: timeout}, nil
}

// KeyID returns the validated key identifier this Signer uses.
func (s *Signer) KeyID() KeyID { return s.key }

// Algorithm reports RS256.
func (s *Signer) Algorithm() providers.SignatureAlgorithm { return providers.RS256 }

// Sign signs a SHA-256 digest with the Key Vault key and returns the PKCS #1 v1.5 signature.
//
// Errors: ErrNotFound when the key or version does not exist; a retryable error, with the delay Key Vault
// asked for when it gave one, on throttling, transient server errors or a timeout; other errors otherwise.
func (s *Signer) Sign(ctx context.Context, digest []byte) ([]byte, error) {
	if len(digest) != sha256.Size {
		return nil, fmt.Errorf("digest is %d bytes, want %d", len(digest), sha256.Size)
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	alg := azkeys.SignatureAlgorithmRS256
	resp, err := s.client.Sign(ctx, s.key.Name, s.key.Version, azkeys.SignParameters{
		Algorithm: &alg,
		Value:     digest,
	}, nil)
	if err != nil {
		return nil, s.mapError(err)
	}
	if resp.KID == nil || *resp.KID == "" {
		return nil, fmt.Errorf("sign with %s: response does not name the key", s.key)
	}
	if !s.key.matches(string(*resp.KID)) {
		return nil, fmt.Errorf("sign with %s: response names another key", s.key)
	}
	sig := resp.Result
	switch len(sig) {
	case 256, 384, 512: // RSA 2048, 3072 and 4096, the sizes Key Vault supports.
	default:
		return nil, fmt.Errorf("sign with %s: signature is %d bytes, not an RSA 2048, 3072 or 4096 signature",
			s.key, len(sig))
	}
	return sig, nil
}

// errorCodePattern limits the Key Vault error code copied into an error to a short identifier, so no
// response text beyond it can reach logs or status.
var errorCodePattern = regexp.MustCompile(`^[A-Za-z0-9.]{1,64}$`)

// mapError turns an SDK error into an error with a providers kind. It never wraps an SDK or network error:
// response and authentication errors include response bodies, and transport errors include URLs. Only the
// providers kind, the retry delay, the status code and a short error code are kept.
func (s *Signer) mapError(err error) error {
	op := "sign with " + s.key.String()

	if re, ok := errors.AsType[*azcore.ResponseError](err); ok {
		code := ""
		if errorCodePattern.MatchString(re.ErrorCode) {
			code = " (" + re.ErrorCode + ")"
		}
		msg := fmt.Errorf("%s: key vault returned status %d%s", op, re.StatusCode, code)
		switch re.StatusCode {
		case http.StatusNotFound:
			return fmt.Errorf("%w: %w", msg, providers.ErrNotFound)
		case http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusInternalServerError,
			http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return &providers.RetryableError{RetryAfter: retryAfter(re.RawResponse), Err: msg}
		case http.StatusUnauthorized, http.StatusForbidden:
			return fmt.Errorf("%w: the identity may not sign with this key", msg)
		default:
			return msg
		}
	}

	if ge, ok := errors.AsType[guardError](err); ok {
		return fmt.Errorf("%s: %w", op, ge)
	}

	if ce, ok := errors.AsType[*credentialError](err); ok {
		msg := fmt.Errorf("%s: %w", op, ce)
		if ce.transient() {
			return &providers.RetryableError{RetryAfter: ce.retryAfter, Err: msg}
		}
		return msg
	}

	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return &providers.RetryableError{Err: fmt.Errorf("%s: %w", op, context.DeadlineExceeded)}
	case errors.Is(err, context.Canceled):
		return fmt.Errorf("%s: %w", op, context.Canceled)
	}
	// Anything else may carry response-controlled text (for example a malformed WWW-Authenticate
	// challenge), request URLs or headers, so it is dropped: only its class survives, as a fixed error.
	if _, ok := errors.AsType[net.Error](err); ok {
		return &providers.RetryableError{Err: fmt.Errorf("%s: %w", op, errRequestNetwork)}
	}
	return fmt.Errorf("%s: %w", op, errRequestSDK)
}

// fixedError is an error whose text is a constant of this package. Errors that leave this package wrap
// only fixed errors, providers sentinels and context errors, never an SDK or network error, so neither
// Error() nor a caller that unwraps the chain and prints every link can reach SDK, URL, header or token
// text.
type fixedError string

func (e fixedError) Error() string { return string(e) }

const (
	// errRequestNetwork is a request that failed in the network stack (dial, TLS, connection reset). It is
	// returned as retryable.
	errRequestNetwork fixedError = "key vault request failed (network)"
	// errRequestSDK is any other failure inside the Azure SDK. It is final.
	errRequestSDK fixedError = "key vault request failed (sdk)"
	// errNoWorkloadIdentity means the workload identity variables are missing or invalid.
	errNoWorkloadIdentity fixedError = "workload identity credential: AZURE_CLIENT_ID, AZURE_TENANT_ID or " +
		"AZURE_FEDERATED_TOKEN_FILE is missing or invalid"
	// errClientSetup means the Key Vault client could not be built from the options.
	errClientSetup fixedError = "the key vault client could not be built"
)

// maxErrorBody bounds how much of an error response is read to find its error code.
const maxErrorBody = 64 << 10

// errorBodyPolicy replaces the body of every error response with one that holds only the Key Vault error
// code, when that code is a short identifier. azcore copies error bodies into ResponseError messages and
// logs them; after this policy there is no response text left to copy.
type errorBodyPolicy struct{}

// Do sends the request and cleans an error response.
func (errorBodyPolicy) Do(req *policy.Request) (*http.Response, error) {
	resp, err := req.Next()
	if err != nil || resp.StatusCode < http.StatusBadRequest {
		return resp, err
	}
	var parsed struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if resp.Body != nil {
		_ = json.NewDecoder(io.LimitReader(resp.Body, maxErrorBody)).Decode(&parsed)
		_ = resp.Body.Close()
	}
	body := "{}"
	if errorCodePattern.MatchString(parsed.Error.Code) {
		body = `{"error":{"code":"` + parsed.Error.Code + `"}}`
	}
	if !errorCodePattern.MatchString(resp.Header.Get("x-ms-error-code")) {
		resp.Header.Del("x-ms-error-code")
	}
	resp.Body = io.NopCloser(strings.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
	return resp, nil
}

// cleanCredential wraps a token credential so that its errors are cleaned before azcore sees them: azcore
// formats credential errors in its own log messages, and an AuthenticationFailedError includes the Entra
// response body. Only the status code and the Retry-After delay survive.
type cleanCredential struct {
	inner azcore.TokenCredential
}

// GetToken returns a token from the wrapped credential, or a cleaned error.
func (c cleanCredential) GetToken(ctx context.Context, o policy.TokenRequestOptions) (azcore.AccessToken, error) {
	tok, err := c.inner.GetToken(ctx, o)
	if err == nil {
		return tok, nil
	}
	ce := &credentialError{}
	if af, ok := errors.AsType[*azidentity.AuthenticationFailedError](err); ok && af.RawResponse != nil {
		ce.status = af.RawResponse.StatusCode
		ce.retryAfter = retryAfter(af.RawResponse)
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		ce.cause = context.DeadlineExceeded
	case errors.Is(err, context.Canceled):
		ce.cause = context.Canceled
	}
	return azcore.AccessToken{}, ce
}

// credentialError is a token request failure with every piece of response text removed.
type credentialError struct {
	status     int
	retryAfter time.Duration
	// cause is a context error, when the failure was a deadline or a cancellation.
	cause error
}

func (e *credentialError) Error() string {
	if e.status == 0 {
		return "workload identity token request failed"
	}
	return fmt.Sprintf("workload identity token request failed with status %d", e.status)
}

func (e *credentialError) Unwrap() error { return e.cause }

// transient reports whether the token request may succeed later.
func (e *credentialError) transient() bool {
	switch e.status {
	case http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusInternalServerError,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return errors.Is(e.cause, context.DeadlineExceeded)
}

// retryAfter reads a Retry-After header in seconds or as an HTTP date, capped at one hour.
func retryAfter(resp *http.Response) time.Duration {
	if resp == nil {
		return 0
	}
	v := resp.Header.Get("Retry-After")
	if v == "" {
		return 0
	}
	var d time.Duration
	if secs, err := strconv.Atoi(v); err == nil {
		d = time.Duration(secs) * time.Second
	} else if t, err := http.ParseTime(v); err == nil {
		d = time.Until(t)
	}
	return min(max(d, 0), time.Hour)
}
