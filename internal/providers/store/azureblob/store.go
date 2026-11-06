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

package azureblob

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"

	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/providers/retryafter"
	"github.com/pkalin/shadewright/internal/redact"
)

// MaxRecordBytes bounds the data of one record. A record holds counts, names and a few redacted sentences.
const MaxRecordBytes = 1 << 20

const (
	// putTimeout bounds one Put, every try and wait included; the caller's deadline wins when it is earlier.
	putTimeout = 30 * time.Second
	// tryTimeout bounds one try: the token and the request.
	tryTimeout = 10 * time.Second
	// maxTries is the most requests one Put sends.
	maxTries = 3
	// firstBackoff and maxBackoff bound the wait between two tries, before jitter.
	firstBackoff = 500 * time.Millisecond
	maxBackoff   = 4 * time.Second
	// maxResponseBody bounds what is read of a response body, which is drained and never parsed or quoted.
	maxResponseBody = 64 << 10
	// apiVersion is the Blob Storage REST API version sent with every request.
	apiVersion = "2023-11-03"
	// storageScope is the only token audience the adapter asks for.
	storageScope = "https://storage.azure.com/.default"
	// blobHostSuffix is the Blob Storage host of the Azure public cloud.
	blobHostSuffix = ".blob.core.windows.net"
)

var (
	// accountPattern is Azure's rule for storage account names.
	accountPattern = regexp.MustCompile(`^[a-z0-9]{3,24}$`)
	// containerPattern is Azure's rule for container names: 3 to 63 lower-case letters, digits and single
	// hyphens, starting and ending with a letter or digit.
	containerPattern = regexp.MustCompile(`^[a-z0-9](-?[a-z0-9])+$`)
	// idPattern bounds a record ID, so that the blob name is one path segment with nothing to escape.
	idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,199}$`)
	// errorCodePattern is what an x-ms-error-code header must look like to be kept in an error.
	errorCodePattern = regexp.MustCompile(`^[A-Za-z]{1,64}$`)
)

// fixedError is an error whose text is a constant, so nothing from a request, a response or a record can reach
// a message.
type fixedError string

func (e fixedError) Error() string { return string(e) }

// Fixed causes. Each is wrapped with its kind from package providers where it is returned.
const (
	errBadAccount      fixedError = "azureblob: the storage account name is not 3 to 24 lower-case letters and digits"
	errBadContainer    fixedError = "azureblob: the container name is not a valid Blob Storage container name"
	errNoIdentity      fixedError = "azureblob: the Azure workload identity is not configured"
	errBadID           fixedError = "azureblob: the record ID is not a lower-case name of at most 200 letters, digits and hyphens"
	errEmpty           fixedError = "azureblob: the record has no data"
	errTooLarge        fixedError = "azureblob: the record is larger than 1 MiB"
	errNotJSON         fixedError = "azureblob: the record is not valid JSON"
	errRedactable      fixedError = "azureblob: the record still holds data the redactor removes; nothing was sent"
	errExists          fixedError = "azureblob: the analysis record already exists; it was not overwritten"
	errNoAnswer        fixedError = "azureblob: no answer from Blob Storage"
	errTLS             fixedError = "azureblob: the certificate of Blob Storage was refused"
	errEntraNoAnswer   fixedError = "azureblob: the token request got no usable answer from Entra ID"
	errEntraRefused    fixedError = "azureblob: Entra ID refused the token request"
	errEntraUnusable   fixedError = "azureblob: no access could be obtained from the workload identity"
	errRefused         fixedError = "azureblob: Blob Storage refused the analyzer's identity"
	errNoContainer     fixedError = "azureblob: Blob Storage did not find the container"
	errRejected        fixedError = "azureblob: Blob Storage rejected the request"
	errServer          fixedError = "azureblob: Blob Storage answered with a transient error"
	errCanceled        fixedError = "azureblob: the write was canceled"
	errDeadline        fixedError = "azureblob: the write ran out of time"
	errUnexpectedReply fixedError = "azureblob: Blob Storage answered with an unexpected status"
)

// Config names where records go. Both values come from the operator's configuration, never from cluster data.
type Config struct {
	// Account is the storage account name: 3 to 24 lower-case letters and digits.
	Account string
	// Container is the container name.
	Container string
}

// Options are the optional parts of a [Store].
type Options struct {
	// Credential returns the Entra ID tokens. Nil means the pod's workload identity (azidentity's
	// WorkloadIdentityCredential, from AZURE_CLIENT_ID, AZURE_TENANT_ID and AZURE_FEDERATED_TOKEN_FILE).
	Credential azcore.TokenCredential
	// Redactor runs the last check of a record's data. Nil means the built-in rules.
	Redactor *redact.Redactor
}

// Store writes analysis records to one Blob Storage container, create-only. It is safe for concurrent use.
type Store struct {
	container *url.URL
	cred      azcore.TokenCredential
	redactor  *redact.Redactor
	client    *http.Client
	// tryTimeout bounds one try; tests shorten it.
	tryTimeout time.Duration
	// sleep waits between tries; tests replace it.
	sleep func(ctx context.Context, d time.Duration) error
	now   func() time.Time
}

var _ providers.AnalysisStore = (*Store)(nil)

// New validates cfg and returns a store. It makes no network call. A wrong setting, or no workload identity
// when opts has no credential, matches [providers.ErrInvalidConfig] and names no value.
func New(cfg Config, opts Options) (*Store, error) {
	if !accountPattern.MatchString(cfg.Account) {
		return nil, fmt.Errorf("%w: %w", errBadAccount, providers.ErrInvalidConfig)
	}
	if len(cfg.Container) > 63 || !containerPattern.MatchString(cfg.Container) {
		return nil, fmt.Errorf("%w: %w", errBadContainer, providers.ErrInvalidConfig)
	}
	cred := opts.Credential
	if cred == nil {
		wi, err := azidentity.NewWorkloadIdentityCredential(nil)
		if err != nil {
			// Not wrapped: SDK text can quote the environment's values.
			return nil, fmt.Errorf("%w: %w", errNoIdentity, providers.ErrInvalidConfig)
		}
		cred = wi
	}
	red := opts.Redactor
	if red == nil {
		var err error
		if red, err = redact.New(); err != nil {
			return nil, fmt.Errorf("azureblob: the redactor: %w", err)
		}
	}
	u := &url.URL{Scheme: "https", Host: cfg.Account + blobHostSuffix}
	return newStore(u.JoinPath(cfg.Container), cred, red, nil), nil
}

// newStore returns a store for the container URL; rt replaces the HTTP transport in tests.
func newStore(container *url.URL, cred azcore.TokenCredential, red *redact.Redactor, rt http.RoundTripper) *Store {
	if rt == nil {
		rt = http.DefaultTransport
	}
	return &Store{
		container: container, cred: cred, redactor: red,
		client: &http.Client{
			Transport: rt,
			// Never follow a redirect: it could carry the token and the record to another host.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		tryTimeout: tryTimeout, sleep: sleepCtx, now: time.Now,
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// check refuses a record before anything is sent: a bad ID, no data, data too large or not JSON, or data the
// redactor would still change (whole occurrences of the ID, the fingerprint and rec.Verified are kept).
func (s *Store) check(rec providers.AnalysisRecord) error {
	switch {
	case !idPattern.MatchString(rec.ID):
		return fmt.Errorf("%w: %w", errBadID, providers.ErrRejected)
	case len(rec.Data) == 0:
		return fmt.Errorf("%w: %w", errEmpty, providers.ErrRejected)
	case len(rec.Data) > MaxRecordBytes:
		return fmt.Errorf("%w: %w", errTooLarge, providers.ErrTooLarge)
	case !json.Valid(rec.Data):
		return fmt.Errorf("%w: %w", errNotJSON, providers.ErrRejected)
	}
	keep := append([]string{rec.ID, rec.Fingerprint}, rec.Verified...)
	if s.redactor.StringKeeping(string(rec.Data), keep...) != string(rec.Data) {
		return fmt.Errorf("%w: %w", errRedactable, providers.ErrRejected)
	}
	return nil
}

// Put writes rec as "<ID>.json" with If-None-Match: *, and returns the blob's URL. A blob that exists is
// never replaced: Put returns an error matching [providers.ErrConflict], also when an earlier try of the same
// Put may have written it, because the identity cannot read the blob to compare.
func (s *Store) Put(ctx context.Context, rec providers.AnalysisRecord) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", ctxError(err)
	}
	if err := s.check(rec); err != nil {
		return "", err
	}
	blob := s.container.JoinPath(rec.ID + ".json")
	ctx, cancel := context.WithTimeout(ctx, putTimeout)
	defer cancel()
	var last error
	for try := 1; ; try++ {
		var retryAfter time.Duration
		retryAfter, last = s.try(ctx, blob, rec.Data)
		if last == nil {
			return blob.String(), nil
		}
		if !providers.IsRetryable(last) || try == maxTries {
			return "", last
		}
		wait := backoff(try)
		if retryAfter > 0 {
			wait = retryAfter
		}
		if dl, ok := ctx.Deadline(); ok && s.now().Add(wait).After(dl) {
			// No time left for another try: the last error is the answer.
			return "", last
		}
		if err := s.sleep(ctx, wait); err != nil {
			return "", ctxError(err)
		}
	}
}

// backoff is the wait after the given try: exponential from firstBackoff, capped, with full jitter.
func backoff(try int) time.Duration {
	d := min(firstBackoff<<(try-1), maxBackoff)
	return d/2 + rand.N(d/2+1) // #nosec G404 -- jitter, not a secret
}

// try sends one request and classifies the answer. It returns the Retry-After delay of a retryable answer.
func (s *Store) try(ctx context.Context, blob *url.URL, data []byte) (time.Duration, error) {
	tctx, cancel := context.WithTimeout(ctx, s.tryTimeout)
	defer cancel()
	tok, err := s.cred.GetToken(tctx, policy.TokenRequestOptions{Scopes: []string{storageScope}})
	if err != nil {
		return 0, s.tokenError(ctx, err)
	}
	req, err := http.NewRequestWithContext(tctx, http.MethodPut, blob.String(), bytes.NewReader(data))
	if err != nil {
		return 0, fmt.Errorf("%w: %w", errRejected, providers.ErrRejected)
	}
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	req.Header.Set("x-ms-version", apiVersion)
	req.Header.Set("x-ms-blob-type", "BlockBlob")
	req.Header.Set("If-None-Match", "*")
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, s.transportError(ctx, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBody))
		_ = resp.Body.Close()
	}()
	return classify(resp, s.now())
}

// statusError is an answer of Blob Storage that is not a success: the fixed cause, the status code and the
// short error code, if it had a well-formed one.
type statusError struct {
	cause  fixedError
	kind   error
	status int
	code   string
}

func (e *statusError) Error() string {
	msg := string(e.cause) + " (" + strconv.Itoa(e.status)
	if e.code != "" {
		msg += " " + e.code
	}
	return msg + ")"
}

func (e *statusError) Unwrap() []error { return []error{e.cause, e.kind} }

// classify maps a response to nil (created) or an error of the right kind, with its Retry-After delay.
func classify(resp *http.Response, now time.Time) (time.Duration, error) {
	if resp.StatusCode == http.StatusCreated {
		return 0, nil
	}
	code := resp.Header.Get("x-ms-error-code")
	if !errorCodePattern.MatchString(code) {
		code = ""
	}
	e := &statusError{status: resp.StatusCode, code: code}
	switch resp.StatusCode {
	case http.StatusConflict, http.StatusPreconditionFailed:
		e.cause, e.kind = errExists, providers.ErrConflict
	case http.StatusUnauthorized, http.StatusForbidden:
		e.cause, e.kind = errRefused, providers.ErrForbidden
	case http.StatusNotFound:
		e.cause, e.kind = errNoContainer, providers.ErrNotFound
	case http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusInternalServerError,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		after := retryafter.Parse(resp.Header.Get("Retry-After"), now, putTimeout)
		e.cause, e.kind = errServer, providers.ErrRetryable
		return after, &providers.RetryableError{RetryAfter: after, Err: e}
	default:
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			// A success other than 201 is not what Put Blob answers; nothing says the blob was created.
			e.cause, e.kind = errUnexpectedReply, providers.ErrRejected
		} else {
			e.cause, e.kind = errRejected, providers.ErrRejected
		}
	}
	return 0, e
}

// transportError classifies a request that got no response. The SDK or network error is dropped: it can quote
// the URL.
func (s *Store) transportError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctxError(ctx.Err())
	}
	var certErr *tls.CertificateVerificationError
	var unknownAuth x509.UnknownAuthorityError
	var hostErr x509.HostnameError
	var invalid x509.CertificateInvalidError
	if errors.As(err, &certErr) || errors.As(err, &unknownAuth) || errors.As(err, &hostErr) || errors.As(err, &invalid) {
		return fmt.Errorf("%w: %w", errTLS, providers.ErrForbidden)
	}
	// The try's own timeout, a reset or a DNS failure: worth another try.
	return &providers.RetryableError{Err: errNoAnswer}
}

// tokenError classifies a failed token request. The azidentity error is dropped: it can quote the
// environment's values and Entra ID's answer.
func (s *Store) tokenError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctxError(ctx.Err())
	}
	var failed *azidentity.AuthenticationFailedError
	if errors.As(err, &failed) && failed.RawResponse != nil {
		st := failed.RawResponse.StatusCode
		if st == http.StatusTooManyRequests || st >= 500 {
			return &providers.RetryableError{Err: errEntraNoAnswer}
		}
		return fmt.Errorf("%w: %w", errEntraRefused, providers.ErrForbidden)
	}
	// No answer at all, the try's own timeout, or a failure azidentity does not class (such as a token file that
	// cannot be read): retried within the bounds, then final.
	return &providers.RetryableError{Err: errEntraUnusable}
}

// ctxError keeps the context's class (canceled or deadline) with fixed text.
func ctxError(err error) error {
	if errors.Is(err, context.Canceled) {
		return fmt.Errorf("%w: %w", errCanceled, context.Canceled)
	}
	return fmt.Errorf("%w: %w", errDeadline, context.DeadlineExceeded)
}
