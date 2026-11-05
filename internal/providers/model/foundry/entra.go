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

package foundry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/pkalin/shadewright/internal/providers"
)

// Workload identity variables, as the workload identity webhook injects them.
const (
	envClientID      = "AZURE_CLIENT_ID"
	envTenantID      = "AZURE_TENANT_ID"
	envTokenFile     = "AZURE_FEDERATED_TOKEN_FILE"
	envAuthorityHost = "AZURE_AUTHORITY_HOST"
)

const (
	defaultAuthorityHost = "https://login.microsoftonline.com/"
	// maxTokenFile bounds the federated token file; a projected service account token is a few KiB.
	maxTokenFile = 64 << 10
	// maxTokenResponse bounds the body of a token response.
	maxTokenResponse = 64 << 10
	// tokenRefreshMargin is the most time before its expiry that a cached access token is replaced (see
	// refreshMargin).
	tokenRefreshMargin = 5 * time.Minute
	// tokenRequestTimeout bounds one token request.
	tokenRequestTimeout = 30 * time.Second
)

// authorityHosts are the Entra ID hosts AZURE_AUTHORITY_HOST may name: the public, US Government and China
// clouds.
var authorityHosts = map[string]bool{
	"login.microsoftonline.com": true,
	"login.microsoftonline.us":  true,
	"login.chinacloudapi.cn":    true,
}

var (
	guidPattern   = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	tenantPattern = regexp.MustCompile(`^([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}|[A-Za-z0-9-]{1,63}(\.[A-Za-z0-9-]{1,63})+)$`)
)

// WorkloadIdentity is a [TokenSource] that exchanges the pod's projected service account token for an Entra
// ID access token for Cognitive Services (the client credentials grant with a client assertion), with the
// standard library. It reads the token file on every exchange, because Kubernetes rotates it, and caches the
// access token until shortly before it expires: five minutes, or half its lifetime when that is shorter. Its
// errors are fixed text with a class and never quote the file, the response or the variables' values. It is
// safe for concurrent use: concurrent callers share one exchange, and each waits for it only as long as its
// own context allows.
type WorkloadIdentity struct {
	clientID, tokenFile string
	tokenURL            string
	client              *http.Client
	now                 func() time.Time

	// mu guards the fields below; it is never held across the network.
	mu        sync.Mutex
	token     string
	refreshAt time.Time
	// inflight is the exchange in progress, shared by every caller that needs a token meanwhile.
	inflight *exchangeCall
}

// exchangeCall is one exchange in progress. Its result is set before done is closed; waiters is guarded by
// WorkloadIdentity.mu.
type exchangeCall struct {
	cancel  context.CancelFunc
	waiters int
	done    chan struct{}
	token   string
	err     error
}

// NewWorkloadIdentity reads AZURE_CLIENT_ID, AZURE_TENANT_ID, AZURE_FEDERATED_TOKEN_FILE and the optional
// AZURE_AUTHORITY_HOST. An error names the variable at fault, never its value, and matches
// [providers.ErrInvalidConfig]. It makes no network call.
func NewWorkloadIdentity() (*WorkloadIdentity, error) {
	return newWorkloadIdentity(os.Getenv, nil)
}

func newWorkloadIdentity(getenv func(string) string, rt http.RoundTripper) (*WorkloadIdentity, error) {
	invalid := func(name, what string) error {
		return fmt.Errorf("foundry: %s %s: %w", name, what, providers.ErrInvalidConfig)
	}
	clientID, tenantID, tokenFile := getenv(envClientID), getenv(envTenantID), getenv(envTokenFile)
	switch {
	case !guidPattern.MatchString(clientID):
		return nil, invalid(envClientID, "is not set or not a GUID")
	case !tenantPattern.MatchString(tenantID):
		return nil, invalid(envTenantID, "is not set or not a tenant ID")
	case tokenFile == "":
		return nil, invalid(envTokenFile, "is not set")
	}
	host := getenv(envAuthorityHost)
	if host == "" {
		host = defaultAuthorityHost
	}
	u, err := url.Parse(host)
	if err != nil || u.Scheme != "https" || !authorityHosts[u.Host] || u.User != nil ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return nil, invalid(envAuthorityHost, "is not a known Entra ID authority host")
	}
	u.Path = "/" + tenantID + "/oauth2/v2.0/token"
	return &WorkloadIdentity{
		clientID: clientID, tokenFile: tokenFile, tokenURL: u.String(),
		client: newHTTPClient(rt, tokenRequestTimeout), now: time.Now,
	}, nil
}

// Token returns a cached access token, or exchanges the federated token for a new one. Callers that need a
// token while an exchange is in progress share it; each returns when its own context ends, and the exchange
// goes on (bounded by tokenRequestTimeout) while anyone still waits, so one canceled caller fails no one else.
func (w *WorkloadIdentity) Token(ctx context.Context) (string, error) {
	if ctx.Err() != nil {
		return "", tokenCtxError(ctx)
	}
	w.mu.Lock()
	if w.token != "" && w.now().Before(w.refreshAt) {
		tok := w.token
		w.mu.Unlock()
		return tok, nil
	}
	call := w.inflight
	if call == nil {
		// Detached from this caller's cancellation: the exchange serves every waiter, and is canceled only
		// when all of them have left.
		xctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		call = &exchangeCall{done: make(chan struct{}), cancel: cancel}
		w.inflight = call
		go w.run(xctx, call)
	}
	call.waiters++
	w.mu.Unlock()
	select {
	case <-call.done:
		return call.token, call.err
	case <-ctx.Done():
		w.mu.Lock()
		call.waiters--
		if call.waiters == 0 {
			// Nobody waits any more: stop the exchange, and let the next caller start a fresh one rather than
			// join this canceled one.
			call.cancel()
			if w.inflight == call {
				w.inflight = nil
			}
		}
		w.mu.Unlock()
		return "", tokenCtxError(ctx)
	}
}

// run performs one exchange for every caller waiting on call, and caches its token.
func (w *WorkloadIdentity) run(ctx context.Context, call *exchangeCall) {
	defer call.cancel()
	tok, refreshAt, err := w.exchange(ctx)
	w.mu.Lock()
	if err == nil {
		w.token, w.refreshAt = tok, refreshAt
	}
	if w.inflight == call {
		w.inflight = nil
	}
	w.mu.Unlock()
	call.token, call.err = tok, err
	close(call.done)
}

func tokenCtxError(ctx context.Context) error {
	if errors.Is(ctx.Err(), context.Canceled) {
		return fmt.Errorf("foundry: the token request was canceled: %w", context.Canceled)
	}
	return &providers.RetryableError{Err: fmt.Errorf("foundry: no access token: %w", context.DeadlineExceeded)}
}

// refreshMargin is how long before its expiry a token of the given lifetime is replaced: tokenRefreshMargin,
// or half the lifetime when that is shorter, so a short-lived token is still reused for a while instead of
// being exchanged on every call.
func refreshMargin(lifetime time.Duration) time.Duration {
	return min(tokenRefreshMargin, lifetime/2)
}

// retryableEntraCodes are the AADSTS codes of a 400 that a later exchange may not repeat: the assertion (the
// projected service account token) is not within its valid time range, which the kubelet's rotation or clock
// skew can cause briefly, and Entra ID's own transient error. Any other code is permanent.
var retryableEntraCodes = map[int64]bool{
	700024: true, // client assertion is not within its valid time range
	500133: true, // assertion is not within its valid time range
	50033:  true, // RetryableError: a transient error
}

// retryableEntraRefusal reads the error_codes of an Entra ID error body, within maxTokenResponse, and reports
// whether one of them is retryable. The body is never logged or returned: it may quote the request.
func retryableEntraRefusal(body io.Reader) bool {
	data, err := io.ReadAll(io.LimitReader(body, maxTokenResponse+1))
	if err != nil || len(data) > maxTokenResponse {
		return false
	}
	var e struct {
		ErrorCodes []int64 `json:"error_codes"`
	}
	if json.Unmarshal(data, &e) != nil {
		return false
	}
	for _, c := range e.ErrorCodes {
		if retryableEntraCodes[c] {
			return true
		}
	}
	return false
}

// exchange returns a new access token and the time to replace it. ctx ends only when every caller waiting
// for it has left (see Token); the exchange is bounded by tokenRequestTimeout.
func (w *WorkloadIdentity) exchange(ctx context.Context) (string, time.Time, error) {
	assertion, err := readTokenFile(w.tokenFile)
	if err != nil {
		// Retryable: the kubelet swaps the projected volume while it rotates the token. Retries are bounded by
		// the caller.
		return "", time.Time{}, &providers.RetryableError{Err: fmt.Errorf("foundry: %w", err)}
	}
	form := url.Values{
		"grant_type":            {"client_credentials"},
		"client_id":             {w.clientID},
		"client_assertion_type": {"urn:ietf:params:oauth:client-assertion-type:jwt-bearer"},
		"client_assertion":      {assertion},
		"scope":                 {cognitiveServicesScope},
	}
	ctx, cancel := context.WithTimeout(ctx, tokenRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", time.Time{}, errors.New("foundry: could not build the token request")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	start := w.now()
	resp, err := w.client.Do(req)
	if err != nil {
		return "", time.Time{}, &providers.RetryableError{Err: errors.New("foundry: no answer from Entra ID")}
	}
	defer func() { _ = resp.Body.Close() }()
	switch code := resp.StatusCode; {
	case code == http.StatusRequestTimeout || code == http.StatusTooManyRequests || code >= 500,
		code == http.StatusBadRequest && retryableEntraRefusal(resp.Body):
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrain))
		return "", time.Time{}, &providers.RetryableError{
			Err: fmt.Errorf("foundry: Entra ID returned status %d", code),
		}
	case code != http.StatusOK:
		// Other 400s and 401: the federation, the client ID or the tenant is wrong. The body may quote the
		// request: it is drained, never read into an error.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrain))
		return "", time.Time{}, fmt.Errorf("foundry: Entra ID refused the token request with status %d: %w",
			code, providers.ErrForbidden)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxTokenResponse+1))
	if err != nil || len(data) > maxTokenResponse {
		return "", time.Time{}, &providers.RetryableError{Err: errors.New("foundry: the answer of Entra ID could not be read")}
	}
	var body struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(data, &body); err != nil || body.AccessToken == "" ||
		!strings.EqualFold(body.TokenType, "Bearer") || body.ExpiresIn <= 0 {
		return "", time.Time{}, &providers.RetryableError{Err: errors.New("foundry: the answer of Entra ID could not be used")}
	}
	if err := checkToken(body.AccessToken); err != nil {
		// Not cached: a token for another audience would fail every call until it expired.
		return "", time.Time{}, fmt.Errorf("foundry: Entra ID returned a token that is not for Cognitive Services: %w",
			providers.ErrForbidden)
	}
	lifetime := time.Duration(min(body.ExpiresIn, 24*3600)) * time.Second
	return body.AccessToken, start.Add(lifetime - refreshMargin(lifetime)), nil
}

// readTokenFile reads a regular file of at most maxTokenFile bytes. It opens the path without blocking, so a
// pipe cannot hold the read, and checks the opened file. Errors are fixed and never quote the path or the
// content.
func readTokenFile(path string) (string, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0) // #nosec G304 -- AZURE_FEDERATED_TOKEN_FILE from the pod's environment, never cluster or model data
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", errors.New("the federated token file does not exist")
	case err != nil:
		return "", errors.New("the federated token file could not be read")
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return "", errors.New("the federated token file is not a readable regular file")
	}
	b, err := io.ReadAll(io.LimitReader(f, maxTokenFile+1))
	switch {
	case err != nil:
		return "", errors.New("the federated token file could not be read")
	case len(b) > maxTokenFile:
		return "", errors.New("the federated token file is too large")
	}
	tok := strings.TrimSpace(string(b))
	if tok == "" {
		return "", errors.New("the federated token file is empty")
	}
	return tok, nil
}
