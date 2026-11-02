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
	"errors"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strconv"
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
		return nil, fmt.Errorf("workload identity credential: %w", err)
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
			// Never log request or response bodies, even if a log listener is configured elsewhere.
			Logging: policy.LogOptions{IncludeBody: false},
		},
	}
	if cc.transport != nil {
		co.Transport = cc.transport
	}
	client, err := azkeys.NewClient(key.VaultURL(), cred, co)
	if err != nil {
		return nil, fmt.Errorf("key vault client for %s: %w", key, err)
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
	if resp.KID != nil && !s.key.matches(string(*resp.KID)) {
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

// mapError turns an SDK error into an error with a providers kind. It never wraps the SDK's response or
// authentication errors, whose messages include response bodies; only the status code and a short error
// code are kept.
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

	if af, ok := errors.AsType[*azidentity.AuthenticationFailedError](err); ok {
		status := 0
		if af.RawResponse != nil {
			status = af.RawResponse.StatusCode
		}
		return fmt.Errorf("%s: workload identity token request failed with status %d", op, status)
	}

	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return &providers.RetryableError{Err: fmt.Errorf("%s: %w", op, context.DeadlineExceeded)}
	case errors.Is(err, context.Canceled):
		return fmt.Errorf("%s: %w", op, context.Canceled)
	}
	// Network errors (DNS, TLS, connection reset) name only the URL, which holds no secret, and are
	// transient as far as the caller can tell.
	if _, ok := errors.AsType[net.Error](err); ok {
		return &providers.RetryableError{Err: fmt.Errorf("%s: %w", op, err)}
	}
	return fmt.Errorf("%s: %w", op, err)
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
