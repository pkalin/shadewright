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
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/cloud"
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
	wi, err := workloadIdentityFromEnv()
	if err != nil {
		return nil, err
	}
	cred, err := azidentity.NewWorkloadIdentityCredential(&azidentity.WorkloadIdentityCredentialOptions{
		ClientID:      wi.clientID,
		TenantID:      wi.tenantID,
		TokenFilePath: wi.tokenFile,
	})
	if err != nil {
		// Not wrapped: the azidentity error is SDK text that can quote the configured values. Every other
		// setting it checks was checked above, so what is left is the tenant ID's format, which is left to
		// azidentity instead of copying its rule.
		return nil, fmt.Errorf("%w: azidentity refused %s", errNoWorkloadIdentity, envTenantID)
	}
	return newSigner(key, cred, opts, clientConfig{})
}

// Workload identity variables, as the workload identity webhook injects them.
const (
	envClientID      = "AZURE_CLIENT_ID"
	envTenantID      = "AZURE_TENANT_ID"
	envTokenFile     = "AZURE_FEDERATED_TOKEN_FILE"
	envAuthorityHost = "AZURE_AUTHORITY_HOST"
)

// authorityHosts are the Entra ID hosts AZURE_AUTHORITY_HOST may name: the public, US Government and China
// clouds, as azcore's cloud configurations list them.
var authorityHosts = func() map[string]bool {
	hosts := map[string]bool{}
	for _, c := range []cloud.Configuration{cloud.AzurePublic, cloud.AzureGovernment, cloud.AzureChina} {
		if u, err := url.Parse(c.ActiveDirectoryAuthorityHost); err == nil {
			hosts[u.Host] = true
		}
	}
	return hosts
}()

// knownAuthorityHost reports whether v is https://<a host in authorityHosts>, with at most a trailing
// slash and no user, query or fragment, so a token request cannot be sent anywhere else.
func knownAuthorityHost(v string) bool {
	u, err := url.Parse(v)
	return err == nil && u.Scheme == schemeHTTPS && authorityHosts[u.Host] && u.User == nil &&
		(u.Path == "" || u.Path == "/") && u.RawQuery == "" && !u.ForceQuery && u.Fragment == "" && u.Opaque == ""
}

// workloadIdentity is the configuration read from the workload identity variables.
type workloadIdentity struct {
	clientID, tenantID, tokenFile string
}

// workloadIdentityFromEnv reads and checks the workload identity variables. An error names the variable at
// fault, never its value.
func workloadIdentityFromEnv() (workloadIdentity, error) {
	wi := workloadIdentity{
		clientID:  os.Getenv(envClientID),
		tenantID:  os.Getenv(envTenantID),
		tokenFile: os.Getenv(envTokenFile),
	}
	for _, v := range []struct{ name, value string }{
		{envClientID, wi.clientID}, {envTenantID, wi.tenantID}, {envTokenFile, wi.tokenFile},
	} {
		if v.value == "" {
			return workloadIdentity{}, fmt.Errorf("%w: %s is not set", errNoWorkloadIdentity, v.name)
		}
	}
	if host := os.Getenv(envAuthorityHost); host != "" && !knownAuthorityHost(host) {
		return workloadIdentity{}, fmt.Errorf("%w: %s is not a known Entra ID authority host", errNoWorkloadIdentity,
			envAuthorityHost)
	}
	return wi, nil
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
			// process sets one, it sees only cleaned credential errors (cleanCredential), fixed transport
			// errors (guardTransport) and cleaned error responses (errorBodyPolicy).
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
		// Not wrapped: the azkeys error is SDK text, and the options are built here, so the class is enough.
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

// mapError turns an SDK error into an error with a providers kind. It never wraps an SDK error: response
// and authentication errors include response bodies, and the SDK's own errors can quote URLs and headers.
// It keeps the providers kind and, as fixed text, the status code, short error code and retry delay of a
// response, or the message of a credential error, a guardTransport error or a context error.
func (s *Signer) mapError(err error) error {
	op := "sign with " + s.key.String()

	if re, ok := errors.AsType[*azcore.ResponseError](err); ok {
		code := ""
		if errorCodePattern.MatchString(re.ErrorCode) {
			code = " (" + re.ErrorCode + ")"
		}
		// Not wrapped: a ResponseError quotes the request URL and the response body.
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
	// guardTransport has already replaced every transport error with a fixed error or a context error.
	if fe, ok := errors.AsType[fixedError](err); ok {
		msg := fmt.Errorf("%s: %w", op, fe)
		if fe.retryable() {
			return &providers.RetryableError{Err: msg}
		}
		return msg
	}
	// Not wrapped: anything else comes from SDK code (for example the challenge policy) and may quote
	// response text, request URLs or headers, so only its class survives.
	return fmt.Errorf("%s: %w", op, errRequestSDK)
}

// fixedError is an error whose text is a constant of this package. Errors that leave this package wrap
// only fixed errors, credential errors, providers sentinels and context errors, never an SDK or network
// error, so neither Error() nor a caller that unwraps the chain and prints every link can reach SDK, URL,
// header or token text. guardTransport marks the final ones for azcore with [final].
type fixedError string

func (e fixedError) Error() string { return string(e) }

// retryable reports whether Sign returns the error as retryable. guardTransport gives azcore the same
// answer: it wraps every fixed error for which this is false in [final].
func (e fixedError) retryable() bool { return e == errRequestNetwork || e == errBodyRead }

const (
	// errRequestNetwork is a request that failed in the network stack (DNS, dial, timeout, connection
	// reset, a TLS alert, a connection closed before the response). azcore retries it, and Sign returns it
	// as retryable.
	errRequestNetwork fixedError = "key vault request failed (network)"
	// errBodyRead is a response body that broke off (truncated, reset). azcore retries it, and Sign returns
	// it as retryable.
	errBodyRead fixedError = "key vault response body could not be read"
	// errRequestTLS is a certificate the client refused (unknown authority, hostname mismatch, an invalid
	// certificate). No retry fixes it, so it is final.
	errRequestTLS fixedError = "key vault request failed (tls)"
	// errRequestSDK is any other failure inside the Azure SDK or the HTTP client (for example a scheme or
	// proxy error). It is final.
	errRequestSDK fixedError = "key vault request failed (sdk)"
	// errMalformedChallenge is a 401 challenge that guardTransport refused. It is final.
	errMalformedChallenge fixedError = "key vault sent a malformed authentication challenge"
	// errResponseTooLarge is a response body larger than maxResponseBody. It is final.
	errResponseTooLarge fixedError = "key vault response is larger than 64 KiB"
	// errNoWorkloadIdentity means the workload identity variables are missing or invalid. New wraps it with
	// the name of the variable at fault, never its value.
	errNoWorkloadIdentity fixedError = "workload identity is not configured"
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
	// Not wrapped: an AuthenticationFailedError prints the Entra response body, and azcore logs credential
	// errors. Only the status, the Retry-After delay and a context error are kept.
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
