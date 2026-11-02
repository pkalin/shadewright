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
	"crypto/tls"
	"io"
	"net/http"
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
//   - It refuses a 401 challenge that the Key Vault SDK would parse unsafely: keyvault/internal v1.2.0
//     takes the tenant as strings.Split(authorization, "/")[3] without a length check, so a challenge
//     like authorization="not-a-url" would panic inside Sign.
//
// Both refusals are fixed errors that carry no response text and are not retried.
type guardTransport struct {
	inner policy.Transporter
}

// Do sends the request and checks the response.
func (g guardTransport) Do(req *http.Request) (*http.Response, error) {
	resp, err := g.inner.Do(req)
	if err != nil || resp == nil {
		return resp, err
	}
	if resp.StatusCode == http.StatusUnauthorized && !safeChallenge(resp.Header.Get("WWW-Authenticate")) {
		closeBody(resp)
		return nil, errMalformedChallenge
	}
	if resp.Body == nil || resp.Body == http.NoBody {
		return resp, nil
	}
	if resp.ContentLength > maxResponseBody {
		closeBody(resp)
		return nil, errResponseTooLarge
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody+1))
	closeBody(resp)
	if err != nil {
		return nil, err
	}
	if len(body) > maxResponseBody {
		return nil, errResponseTooLarge
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

// safeChallenge reports whether the Key Vault SDK can parse a WWW-Authenticate value without panicking.
// It splits the value exactly as keyvault/internal v1.2.0 does and then requires that a present
// authorization parameter is an https URL with a tenant segment. An empty header or a missing resource is
// left to the SDK, which returns an error for both.
func safeChallenge(header string) bool {
	vals := map[string]string{}
	for part := range strings.SplitSeq(strings.ReplaceAll(header, "Bearer ", ""), " ") {
		sub := strings.Split(part, "=")
		if len(sub) == 2 {
			vals[sub[0]] = strings.TrimSuffix(strings.ReplaceAll(sub[1], `"`, ""), ",")
		}
	}
	auth, ok := vals["authorization"]
	if !ok || auth == "" {
		return true
	}
	parts := strings.Split(auth, "/")
	return len(parts) >= 4 && parts[0] == "https:" && parts[1] == "" && parts[2] != "" && parts[3] != ""
}

// guardError is a fixed refusal that azcore's retry policy does not retry.
type guardError string

func (e guardError) Error() string { return string(e) }

// NonRetriable marks the error as final for azcore (errorinfo.NonRetriable).
func (guardError) NonRetriable() {}

const (
	errMalformedChallenge guardError = "key vault sent a malformed authentication challenge"
	errResponseTooLarge   guardError = "key vault response is larger than 64 KiB"
)

// defaultTransport is the HTTP client used when tests do not replace it: the standard transport with
// TLS 1.2 or later and bounded connection setup.
func defaultTransport() policy.Transporter {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	t.TLSHandshakeTimeout = 10 * time.Second
	t.ResponseHeaderTimeout = tryTimeout
	return &http.Client{Transport: t}
}
