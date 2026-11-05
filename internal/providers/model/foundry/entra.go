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
	// tokenRefreshMargin is how long before its expiry a cached access token is replaced.
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
// access token until five minutes before it expires. Its errors are fixed text with a class and never
// quote the file, the response or the variables' values. It is safe for concurrent use.
type WorkloadIdentity struct {
	clientID, tokenFile string
	tokenURL            string
	client              *http.Client
	now                 func() time.Time

	// lock is held by the one Token call that may exchange; a channel, so a waiting caller can give up when
	// its context ends.
	lock    chan struct{}
	token   string
	expires time.Time
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
		client: newHTTPClient(rt), now: time.Now, lock: make(chan struct{}, 1),
	}, nil
}

// Token returns a cached access token, or exchanges the federated token for a new one.
func (w *WorkloadIdentity) Token(ctx context.Context) (string, error) {
	select {
	case w.lock <- struct{}{}:
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.Canceled) {
			return "", fmt.Errorf("foundry: the token request was canceled: %w", context.Canceled)
		}
		return "", &providers.RetryableError{Err: fmt.Errorf("foundry: no access token: %w", context.DeadlineExceeded)}
	}
	defer func() { <-w.lock }()
	if w.token != "" && w.now().Before(w.expires.Add(-tokenRefreshMargin)) {
		return w.token, nil
	}
	tok, expires, err := w.exchange(ctx)
	if err != nil {
		return "", err
	}
	w.token, w.expires = tok, expires
	return tok, nil
}

func (w *WorkloadIdentity) exchange(ctx context.Context) (string, time.Time, error) {
	fail := func(msg string, class error) (string, time.Time, error) {
		return "", time.Time{}, fmt.Errorf("foundry: %s: %w", msg, class)
	}
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
		if errors.Is(ctx.Err(), context.Canceled) {
			return fail("the token request was canceled", context.Canceled)
		}
		return "", time.Time{}, &providers.RetryableError{Err: errors.New("foundry: no answer from Entra ID")}
	}
	defer func() { _ = resp.Body.Close() }()
	switch code := resp.StatusCode; {
	case code == http.StatusTooManyRequests || code >= 500:
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrain))
		return "", time.Time{}, &providers.RetryableError{
			Err: fmt.Errorf("foundry: Entra ID returned status %d", code),
		}
	case code != http.StatusOK:
		// 400 and 401: the federation, the client ID or the tenant is wrong. The body may quote the request.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrain))
		return fail(fmt.Sprintf("Entra ID refused the token request with status %d", code), providers.ErrForbidden)
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
	return body.AccessToken, start.Add(time.Duration(min(body.ExpiresIn, 24*3600)) * time.Second), nil
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
