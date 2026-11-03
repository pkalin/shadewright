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
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
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
)

const (
	// errTokenFileUnreadable means the federated token file could not be opened or read.
	errTokenFileUnreadable fixedError = "the federated token file could not be read"
	// errTokenFileNotRegular means the federated token file is a directory, a pipe or a device.
	errTokenFileNotRegular fixedError = "the federated token file is not a regular file"
	// errTokenFileTooLarge means the federated token file is larger than maxTokenFile.
	errTokenFileTooLarge fixedError = "the federated token file is larger than 64 KiB"
	// errTokenFileEmpty means the federated token file is empty.
	errTokenFileEmpty fixedError = "the federated token file is empty"
)

// newWorkloadCredential builds the workload identity credential: a client assertion credential whose
// assertion is the projected service account token in wi.tokenFile. It is what azidentity's
// WorkloadIdentityCredential does (same tenant, client ID, authority host from AZURE_AUTHORITY_HOST,
// instance discovery and token reuse), except that this package reads the file itself, so a read failure
// is known exactly instead of guessed from an error without a response. co is for tests.
func newWorkloadCredential(wi workloadIdentity, co azcore.ClientOptions) (azcore.TokenCredential, error) {
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

// tokenFileReadKey is the context key under which cleanCredential passes a *tokenFileRead to the assertion
// callback of the same token request.
type tokenFileReadKey struct{}

// tokenFileRead records a failed read of the token file during one token request.
type tokenFileRead struct {
	mu      sync.Mutex
	readErr error
}

func (r *tokenFileRead) set(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.readErr = err
}

func (r *tokenFileRead) err() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.readErr
}

// tokenFile reads the federated token file and reuses its content for tokenFileReuse.
type tokenFile struct {
	path      string
	mu        sync.Mutex
	content   string
	expiresAt time.Time
}

// assertion returns the token in the file. A read failure is a fixed error, and it is also recorded for
// the token request in ctx, because azidentity passes on only its text.
func (f *tokenFile) assertion(ctx context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now()
	if now.Before(f.expiresAt) {
		return f.content, nil
	}
	content, err := readTokenFile(f.path)
	if err != nil {
		if r, ok := ctx.Value(tokenFileReadKey{}).(*tokenFileRead); ok {
			r.set(err)
		}
		return "", err
	}
	f.content, f.expiresAt = content, now.Add(tokenFileReuse)
	return content, nil
}

// readTokenFile reads a regular file of at most maxTokenFile bytes. The path is checked before it is opened,
// so a pipe never blocks the read, and the opened file is checked again. Errors are fixed and never quote
// the path or the content.
func readTokenFile(path string) (string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", errTokenFileUnreadable
	}
	if !st.Mode().IsRegular() {
		return "", errTokenFileNotRegular
	}
	f, err := os.Open(path)
	if err != nil {
		return "", errTokenFileUnreadable
	}
	defer func() { _ = f.Close() }()
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
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
