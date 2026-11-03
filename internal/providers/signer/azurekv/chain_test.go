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
	"net"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/pkalin/shadewright/internal/providers"
)

// failingTransport fails every request with err, as a broken connection or an SDK failure would.
type failingTransport struct{ err error }

func (f failingTransport) Do(*http.Request) (*http.Response, error) { return nil, f.err }

// sdkText stands for text an SDK or the network stack puts in an error: response data, a request URL with
// a query, a header and a token. None of it may stay anywhere in the chain Sign or New returns.
const sdkText = "GET https://" + testHost + "/keys/github-app?sig=" + testToken + " Bearer " + bodySecret

// chain returns err and every error it wraps, following both Unwrap() error and Unwrap() []error.
func chain(err error) []error {
	var out []error
	queue := []error{err}
	for len(queue) > 0 {
		e := queue[0]
		queue = queue[1:]
		if e == nil {
			continue
		}
		out = append(out, e)
		switch u := e.(type) {
		case interface{ Unwrap() error }:
			queue = append(queue, u.Unwrap())
		case interface{ Unwrap() []error }:
			queue = append(queue, u.Unwrap()...)
		}
	}
	return out
}

// assertCleanChain fails if any error in the chain of err, printed with %v or %+v, carries SDK, URL,
// header or token text, or is a type from the Azure SDK or the network stack that a caller could unwrap
// and print.
func assertCleanChain(t *testing.T, name string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: no error", name)
	}
	for i, e := range chain(err) {
		text := e.Error() + "\n" + fmt.Sprintf("%v", e) + "\n" + fmt.Sprintf("%+v", e)
		for _, bad := range []string{testToken, bodySecret, "Bearer", "sig="} {
			if strings.Contains(text, bad) {
				t.Errorf("%s: chain[%d] (%T) leaks %q: %v", name, i, e, bad, e)
				break
			}
		}
		typ := reflect.TypeOf(e)
		if typ.Kind() == reflect.Pointer {
			typ = typ.Elem()
		}
		if pkg := typ.PkgPath(); strings.HasPrefix(pkg, "github.com/Azure/") || pkg == "net" || pkg == "net/url" {
			t.Errorf("%s: chain[%d] is a %T, which a caller could unwrap and print", name, i, e)
		}
	}
}

// TestSignErrorChainHasNoSDKText walks the whole chain of every Sign failure class and checks that no
// SDK, URL, header or token text survives anywhere in it, while the provider kinds still match.
func TestSignErrorChainHasNoSDKText(t *testing.T) {
	key, err := ParseKeyID("https://" + testHost + "/keys/github-app")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name      string
		err       error
		retryable bool
		class     error
	}{
		{"network", &net.OpError{Op: "read", Net: "tcp", Err: errors.New(sdkText)}, true, errRequestNetwork},
		{"sdk", errors.New(sdkText), false, errRequestSDK},
	} {
		s, err := newSigner(key, &fakeCredential{}, Options{MaxRetries: -1}, clientConfig{
			transport:  failingTransport{err: tc.err},
			retryDelay: time.Millisecond,
		})
		if err != nil {
			t.Fatal(err)
		}
		_, err = s.Sign(context.Background(), digestOf("x"))
		assertCleanChain(t, tc.name, err)
		if providers.IsRetryable(err) != tc.retryable {
			t.Errorf("%s: retryable = %v, want %v: %v", tc.name, providers.IsRetryable(err), tc.retryable, err)
		}
		if !errors.Is(err, tc.class) {
			t.Errorf("%s: err = %v, want it to match its class %v", tc.name, err, tc.class)
		}
	}

	// Response errors and token request failures go through the SDK's own error types.
	s, v, _ := setup(t, "https://"+testHost+"/keys/github-app", Options{MaxRetries: -1})
	v.respond = func(_ int, w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.Header().Set("WWW-Authenticate", "Bearer "+bodySecret)
		writeError(w, http.StatusServiceUnavailable, "ServiceUnavailable")
	}
	_, err = s.Sign(context.Background(), digestOf("x"))
	assertCleanChain(t, "response", err)
	if !providers.IsRetryable(err) {
		t.Errorf("response: err = %v, want retryable", err)
	}

	s, v, cred := setup(t, "https://"+testHost+"/keys/github-app", Options{MaxRetries: -1})
	v.respond = v.signOK("https://" + testHost + "/keys/github-app/" + testVersion)
	cred.err = authFailure(http.StatusServiceUnavailable, "")
	_, err = s.Sign(context.Background(), digestOf("x"))
	assertCleanChain(t, "credential", err)
	if !providers.IsRetryable(err) {
		t.Errorf("credential: err = %v, want retryable", err)
	}
}

// TestNewErrorChainHasNoSDKText checks that a missing workload identity setup fails with fixed text and no
// azidentity error in the chain.
func TestNewErrorChainHasNoSDKText(t *testing.T) {
	for _, name := range []string{"AZURE_CLIENT_ID", "AZURE_TENANT_ID", "AZURE_FEDERATED_TOKEN_FILE"} {
		t.Setenv(name, "")
	}
	_, err := New("https://"+testHost+"/keys/github-app", Options{})
	assertCleanChain(t, "new", err)
	if !errors.Is(err, errNoWorkloadIdentity) {
		t.Errorf("err = %v, want the workload identity error", err)
	}
}
