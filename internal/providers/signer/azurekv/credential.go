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
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
)

const (
	// maxTokenFile bounds the federated token file. A projected service account token is a JWT of a few
	// KiB.
	maxTokenFile = 64 << 10
	// tokenFileReuse is how long a token read from the file is reused, as azidentity's
	// WorkloadIdentityCredential does: Kubernetes rotates the token at 80% of a lifetime of at least one
	// hour, so a token read now is valid for at least 12 more minutes.
	tokenFileReuse = 10 * time.Minute
	// tokenFileRetryDelay is the wait before the one retry of a token file that does not exist, which can
	// happen for a moment while the kubelet swaps the projected volume's ..data link.
	tokenFileRetryDelay = 50 * time.Millisecond
)

const (
	// errTokenFileMissing means the federated token file does not exist, also after one retry.
	errTokenFileMissing fixedError = "the federated token file does not exist"
	// errTokenFileUnreadable means the federated token file could not be opened, checked or read.
	errTokenFileUnreadable fixedError = "the federated token file could not be read"
	// errTokenFileNotRegular means the federated token file is a directory, a pipe or a device.
	errTokenFileNotRegular fixedError = "the federated token file is not a regular file"
	// errTokenFileTooLarge means the federated token file is larger than maxTokenFile.
	errTokenFileTooLarge fixedError = "the federated token file is larger than 64 KiB"
	// errTokenFileEmpty means the federated token file is empty.
	errTokenFileEmpty fixedError = "the federated token file is empty"

	// errEntraNetwork is a token request that got no answer from Entra ID (DNS, dial, reset, timeout, a
	// TLS alert, a response body that broke off). It is transient.
	errEntraNetwork fixedError = "no answer from Entra ID"
	// errEntraTLS is a certificate of Entra ID that the client refused. It is final.
	errEntraTLS fixedError = "the certificate of Entra ID was refused"
	// errEntraClient is a known local failure of the token request in the HTTP client (an https request
	// answered in plain HTTP, no response at all). It is final.
	errEntraClient fixedError = "the token request failed in the HTTP client"
	// errEntraAnswer is a successful answer from Entra ID that could not be used (for example a body that
	// does not parse). It is transient.
	errEntraAnswer fixedError = "the answer of Entra ID could not be used"
)

// newWorkloadCredential builds the workload identity credential: a client assertion credential whose
// assertion is the projected service account token in wi.tokenFile. It is what azidentity's
// WorkloadIdentityCredential does (same tenant, client ID, authority host from AZURE_AUTHORITY_HOST,
// instance discovery and token reuse), except that this package reads the file itself and sends the
// token requests through guardTransport. So a read failure is known exactly, and a network or certificate
// failure toward Entra ID is cleaned and classed as one toward Key Vault is. inner replaces the HTTP client
// in tests; nil means defaultTransport.
func newWorkloadCredential(wi workloadIdentity, co azcore.ClientOptions, inner policy.Transporter) (azcore.TokenCredential, error) {
	if inner == nil {
		inner = defaultTransport()
	}
	co.Transport = guardTransport{inner: inner, entra: true}
	tf := &tokenFile{path: wi.tokenFile}
	cred, err := azidentity.NewClientAssertionCredential(wi.tenantID, wi.clientID, tf.assertion,
		&azidentity.ClientAssertionCredentialOptions{ClientOptions: co})
	if err != nil {
		// Not wrapped: the azidentity error is SDK text that can quote the configured values. Every other
		// setting it checks was checked by workloadIdentityFromEnv, so what is left is the tenant ID's
		// format, which is left to azidentity instead of copying its rule.
		return nil, fmt.Errorf("%w: azidentity refused %s", errNoWorkloadIdentity, envTenantID)
	}
	return cred, nil
}

// tokenRequestKey is the context key under which cleanCredential passes a *tokenRequest to the assertion
// callback and to guardTransport for the same token request.
type tokenRequestKey struct{}

// tokenRequest records the last thing this package saw during one token request: a read of the token
// file, or an exchange with Entra ID through guardTransport. azidentity passes on only the text of those
// errors, so this is how cleanCredential learns their class. Each record replaces the one before, so an
// earlier failure never outlives a later success. Plain fields are enough: MSAL calls the assertion
// callback, and azcore calls the transport, synchronously in the goroutine of GetToken.
type tokenRequest struct {
	// seen is set once anything was recorded.
	seen bool
	// readErr is the result of the last read of the token file, nil after a good read.
	readErr error
	// read is set when the last record is a read of the token file.
	read bool
	// transportErr is the error guardTransport returned for the last exchange; nil when it passed on a
	// response.
	transportErr error
	// status is the status of the response guardTransport passed on in the last exchange.
	status int
}

func (r *tokenRequest) recordRead(err error) {
	*r = tokenRequest{seen: true, read: true, readErr: err}
}

func (r *tokenRequest) recordExchange(status int, err error) {
	*r = tokenRequest{seen: true, status: status, transportErr: err}
}

// tokenRequestFrom returns the token request recorder in ctx, or nil.
func tokenRequestFrom(ctx context.Context) *tokenRequest {
	r, _ := ctx.Value(tokenRequestKey{}).(*tokenRequest)
	return r
}

// tokenFile reads the federated token file and reuses its content for tokenFileReuse.
type tokenFile struct {
	path      string
	mu        sync.Mutex
	content   string
	expiresAt time.Time
}

// assertion returns the token in the file. The result of a read is recorded for the token request in ctx,
// because azidentity passes on only an error's text. A file that does not exist is read once more after
// tokenFileRetryDelay; when ctx ends first, its error is returned and recorded.
func (f *tokenFile) assertion(ctx context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if time.Now().Before(f.expiresAt) {
		return f.content, nil
	}
	content, err := readTokenFile(f.path)
	if errors.Is(err, errTokenFileMissing) {
		select {
		case <-time.After(tokenFileRetryDelay):
			content, err = readTokenFile(f.path)
		case <-ctx.Done():
			// Recorded as the end of the request, so the class is the context's and not that of an
			// earlier exchange.
			if r := tokenRequestFrom(ctx); r != nil {
				r.recordExchange(0, ctx.Err())
			}
			return "", ctx.Err()
		}
	}
	if r := tokenRequestFrom(ctx); r != nil {
		r.recordRead(err)
	}
	if err != nil {
		return "", err
	}
	f.content, f.expiresAt = content, time.Now().Add(tokenFileReuse)
	return content, nil
}

// readTokenFile reads a regular file of at most maxTokenFile bytes. It opens the path without blocking, so
// a pipe cannot hold the read, and checks the opened file, so nothing can be swapped in between. Errors
// are fixed and never quote the path or the content.
func readTokenFile(path string) (string, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0) // #nosec G304 -- AZURE_FEDERATED_TOKEN_FILE from the pod's environment, never cluster or model data
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", errTokenFileMissing
	case err != nil:
		return "", errTokenFileUnreadable
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	switch {
	case err != nil:
		return "", errTokenFileUnreadable
	case !st.Mode().IsRegular():
		return "", errTokenFileNotRegular
	}
	b, err := io.ReadAll(io.LimitReader(f, maxTokenFile+1))
	switch {
	case err != nil:
		return "", errTokenFileUnreadable
	case len(b) > maxTokenFile:
		return "", errTokenFileTooLarge
	case len(b) == 0:
		return "", errTokenFileEmpty
	}
	return string(b), nil
}

// entraCause turns what guardTransport returned for the last exchange with Entra ID into the cause of a
// credential error, and reports whether the request may succeed later.
func entraCause(err error) (error, bool) {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return context.DeadlineExceeded, true
	case errors.Is(err, context.Canceled):
		return context.Canceled, false
	case errors.Is(err, errRequestTLS):
		return errEntraTLS, false
	}
	if fe, ok := errors.AsType[fixedError](err); ok && fe.retryable() {
		return errEntraNetwork, true
	}
	return errEntraClient, false
}
