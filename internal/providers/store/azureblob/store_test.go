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
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"

	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/redact"
)

const testToken = "test-access-token-value"

// Settings of the store in the tests.
const (
	testAccount   = "stswanabc"
	testContainer = "analyses"
)

// staticCredential returns a fixed token for the Storage scope only.
type staticCredential struct {
	mu     sync.Mutex
	scopes [][]string
	err    error
}

func (c *staticCredential) GetToken(_ context.Context, o policy.TokenRequestOptions) (azcore.AccessToken, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.scopes = append(c.scopes, o.Scopes)
	if c.err != nil {
		return azcore.AccessToken{}, c.err
	}
	return azcore.AccessToken{Token: testToken, ExpiresOn: time.Now().Add(time.Hour)}, nil
}

// request is what the test server saw of one request.
type request struct {
	method, path string
	header       http.Header
	body         string
}

// server answers each request with the next handler of answers; the last one repeats.
type server struct {
	*httptest.Server
	mu   sync.Mutex
	seen []request
}

func newServer(t *testing.T, answers ...http.HandlerFunc) *server {
	t.Helper()
	s := &server{}
	s.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		n := len(s.seen)
		s.seen = append(s.seen, request{method: r.Method, path: r.URL.Path, header: r.Header.Clone(), body: string(b)})
		s.mu.Unlock()
		answers[min(n, len(answers)-1)](w, r)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *server) requests() []request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]request(nil), s.seen...)
}

func status(code int, header ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		for i := 0; i+1 < len(header); i += 2 {
			w.Header().Set(header[i], header[i+1])
		}
		w.WriteHeader(code)
		_, _ = io.WriteString(w, "<Error><Message>body text "+testToken+"</Message></Error>")
	}
}

// testStore returns a store that writes to container "analyses" on s, and records its waits instead of sleeping.
func testStore(t *testing.T, s *server) (*Store, *[]time.Duration) {
	t.Helper()
	u, err := url.Parse(s.URL)
	if err != nil {
		t.Fatal(err)
	}
	red, err := redact.New()
	if err != nil {
		t.Fatal(err)
	}
	st := newStore(u.JoinPath(testContainer), &staticCredential{}, red, s.Client().Transport)
	var waits []time.Duration
	st.sleep = func(ctx context.Context, d time.Duration) error {
		waits = append(waits, d)
		return ctx.Err()
	}
	return st, &waits
}

const recordData = `{"schemaVersion":1,"reason":"NoProposal","summary":"the container ran out of memory"}`

func testRecord() providers.AnalysisRecord {
	return providers.AnalysisRecord{
		ID:          "6f1e0b4a-3c2d-4e5f-8a9b-0c1d2e3f4a5b-20261106t100000z",
		Fingerprint: "sha256:" + strings.Repeat("ab", 32),
		CreatedAt:   time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC),
		Data:        []byte(recordData),
	}
}

func TestPutCreates(t *testing.T) {
	s := newServer(t, status(http.StatusCreated))
	st, waits := testStore(t, s)
	ref, err := st.Put(t.Context(), testRecord())
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	reqs := s.requests()
	if len(reqs) != 1 || len(*waits) != 0 {
		t.Fatalf("%d requests and %d waits, want one request", len(reqs), len(*waits))
	}
	r := reqs[0]
	wantPath := "/analyses/" + testRecord().ID + ".json"
	if r.method != http.MethodPut || r.path != wantPath {
		t.Errorf("request %s %s, want PUT %s", r.method, r.path, wantPath)
	}
	if !strings.HasSuffix(ref, wantPath) {
		t.Errorf("reference %q, want the blob URL", ref)
	}
	for k, v := range map[string]string{
		"If-None-Match": "*", "X-Ms-Blob-Type": "BlockBlob", "X-Ms-Version": apiVersion,
		"Content-Type": "application/json", "Authorization": "Bearer " + testToken,
	} {
		if got := r.header.Get(k); got != v {
			t.Errorf("header %s = %q, want %q", k, got, v)
		}
	}
	if r.body != recordData {
		t.Errorf("body %q, want the record's data", r.body)
	}
	cred := st.cred.(*staticCredential)
	if len(cred.scopes) != 1 || len(cred.scopes[0]) != 1 || cred.scopes[0][0] != storageScope {
		t.Errorf("token scopes %v, want only %s", cred.scopes, storageScope)
	}
}

// An existing blob is never overwritten nor retried, whether Blob Storage answers 409 or 412, and the error says
// so without quoting the response.
func TestPutExistingIsFinal(t *testing.T) {
	for name, h := range map[string]http.HandlerFunc{
		"409": status(http.StatusConflict, "x-ms-error-code", "BlobAlreadyExists"),
		"412": status(http.StatusPreconditionFailed, "x-ms-error-code", "ConditionNotMet"),
	} {
		t.Run(name, func(t *testing.T) {
			s := newServer(t, h)
			st, waits := testStore(t, s)
			_, err := st.Put(t.Context(), testRecord())
			if !providers.IsConflict(err) || providers.IsRetryable(err) {
				t.Fatalf("Put = %v, want a final conflict", err)
			}
			if !strings.Contains(err.Error(), "already exists") || !strings.Contains(err.Error(), name) {
				t.Errorf("error %q: want it to say the record exists, with the status", err)
			}
			if strings.Contains(err.Error(), testToken) || strings.Contains(err.Error(), "body text") {
				t.Errorf("error %q quotes the response or the token", err)
			}
			if n := len(s.requests()); n != 1 || len(*waits) != 0 {
				t.Errorf("%d requests and %d waits, want one request and no retry", n, len(*waits))
			}
		})
	}
}

// A transient server error is retried, honouring Retry-After, and the next try creates the record.
func TestPutRetriesTransientThenCreates(t *testing.T) {
	s := newServer(t, status(http.StatusServiceUnavailable, "Retry-After", "2", "x-ms-error-code", "ServerBusy"),
		status(http.StatusInternalServerError), status(http.StatusCreated))
	st, waits := testStore(t, s)
	if _, err := st.Put(t.Context(), testRecord()); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if n := len(s.requests()); n != 3 {
		t.Fatalf("%d requests, want 3", n)
	}
	if len(*waits) != 2 || (*waits)[0] != 2*time.Second {
		t.Errorf("waits %v, want the Retry-After of 2s first, then a backoff", *waits)
	}
	for _, r := range s.requests() {
		if r.header.Get("If-None-Match") != "*" {
			t.Errorf("a retry was sent without If-None-Match: *")
		}
	}
}

// Transient errors that do not end are retried at most maxTries times, and the error stays retryable.
func TestPutGivesUpAfterMaxTries(t *testing.T) {
	s := newServer(t, status(http.StatusBadGateway))
	st, _ := testStore(t, s)
	_, err := st.Put(t.Context(), testRecord())
	if !providers.IsRetryable(err) || !strings.Contains(err.Error(), "502") {
		t.Fatalf("Put = %v, want a retryable error with the status", err)
	}
	if n := len(s.requests()); n != maxTries {
		t.Errorf("%d requests, want %d", n, maxTries)
	}
}

// Answers that cannot change on a retry are final after one request.
func TestPutFinalAnswers(t *testing.T) {
	for name, tc := range map[string]struct {
		code int
		is   func(error) bool
	}{
		"forbidden":    {http.StatusForbidden, providers.IsForbidden},
		"unauthorized": {http.StatusUnauthorized, providers.IsForbidden},
		"no container": {http.StatusNotFound, providers.IsNotFound},
		"bad request":  {http.StatusBadRequest, providers.IsRejected},
		"redirect":     {http.StatusTemporaryRedirect, providers.IsRejected},
		"other 2xx":    {http.StatusOK, providers.IsRejected},
	} {
		t.Run(name, func(t *testing.T) {
			s := newServer(t, status(tc.code, "Location", "https://elsewhere.example/"))
			st, _ := testStore(t, s)
			_, err := st.Put(t.Context(), testRecord())
			if err == nil || !tc.is(err) || providers.IsRetryable(err) {
				t.Fatalf("Put = %v", err)
			}
			if n := len(s.requests()); n != 1 {
				t.Errorf("%d requests, want 1", n)
			}
		})
	}
}

// A server that does not answer makes each try time out; the write ends within the caller's deadline with a
// deadline error, and no try is started after it.
func TestPutTimeout(t *testing.T) {
	release := make(chan struct{})
	s := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	})
	defer close(release)
	st, _ := testStore(t, s)
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := st.Put(ctx, testRecord())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Put = %v, want a deadline error", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("Put took %s, want it to end at the caller's deadline", d)
	}
	if n := len(s.requests()); n != 1 {
		t.Errorf("%d requests, want 1", n)
	}
}

// A try that times out on its own is retried while the write has time left.
func TestPutRetriesATryThatTimedOut(t *testing.T) {
	var n int
	var mu sync.Mutex
	s := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n++
		first := n == 1
		mu.Unlock()
		if first {
			<-r.Context().Done()
			return
		}
		w.WriteHeader(http.StatusCreated)
	})
	st, _ := testStore(t, s)
	// One try's timeout is 10 seconds: a shorter one keeps the test quick.
	st.tryTimeout = 200 * time.Millisecond
	if _, err := st.Put(t.Context(), testRecord()); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if got := len(s.requests()); got != 2 {
		t.Errorf("%d requests, want 2", got)
	}
}

// A canceled context ends the write at once, with no request and no retry.
func TestPutCanceled(t *testing.T) {
	s := newServer(t, status(http.StatusCreated))
	st, _ := testStore(t, s)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := st.Put(ctx, testRecord()); !errors.Is(err, context.Canceled) {
		t.Fatalf("Put = %v, want canceled", err)
	}
	if n := len(s.requests()); n != 0 {
		t.Errorf("%d requests, want none", n)
	}
}

// A cancel during the wait between tries ends the write without another request.
func TestPutCanceledWhileWaiting(t *testing.T) {
	s := newServer(t, status(http.StatusServiceUnavailable))
	st, _ := testStore(t, s)
	ctx, cancel := context.WithCancel(t.Context())
	st.sleep = func(ctx context.Context, _ time.Duration) error {
		cancel()
		<-ctx.Done()
		return ctx.Err()
	}
	if _, err := st.Put(ctx, testRecord()); !errors.Is(err, context.Canceled) {
		t.Fatalf("Put = %v, want canceled", err)
	}
	if n := len(s.requests()); n != 1 {
		t.Errorf("%d requests, want 1", n)
	}
}

// A record that still holds a secret pattern, or that is not a valid record, is refused before any network
// call: no token is requested and no request is sent.
func TestPutRefusesBeforeAnyCall(t *testing.T) {
	for name, tc := range map[string]struct {
		edit func(*providers.AnalysisRecord)
		is   func(error) bool
	}{
		"token": {func(r *providers.AnalysisRecord) {
			r.Data = []byte(`{"summary":"saw ghp_abcdefghijklmnopqrstuvwxyz0123456789 in the log"}`)
		}, providers.IsRejected},
		"password": {func(r *providers.AnalysisRecord) {
			r.Data = []byte(`{"summary":"password: hunter2xyz9"}`)
		}, providers.IsRejected},
		"jwt": {func(r *providers.AnalysisRecord) {
			r.Data = []byte(`{"summary":"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U"}`)
		}, providers.IsRejected},
		"hash not verified": {func(r *providers.AnalysisRecord) {
			r.Data = []byte(`{"request":"remediation-0a1b2c3d4e5f6a7b8c9d0e1f"}`)
		}, providers.IsRejected},
		"id with a slash": {func(r *providers.AnalysisRecord) { r.ID = "../other" }, providers.IsRejected},
		"empty id":        {func(r *providers.AnalysisRecord) { r.ID = "" }, providers.IsRejected},
		"upper-case id":   {func(r *providers.AnalysisRecord) { r.ID = "Record" }, providers.IsRejected},
		"no data":         {func(r *providers.AnalysisRecord) { r.Data = nil }, providers.IsRejected},
		"not json":        {func(r *providers.AnalysisRecord) { r.Data = []byte("plain text") }, providers.IsRejected},
		"too large": {func(r *providers.AnalysisRecord) {
			r.Data = []byte(`"` + strings.Repeat("a b ", MaxRecordBytes/4) + `"`)
		}, providers.IsTooLarge},
	} {
		t.Run(name, func(t *testing.T) {
			s := newServer(t, status(http.StatusCreated))
			st, _ := testStore(t, s)
			rec := testRecord()
			tc.edit(&rec)
			_, err := st.Put(t.Context(), rec)
			if err == nil || !tc.is(err) || providers.IsRetryable(err) {
				t.Fatalf("Put = %v, want a final refusal", err)
			}
			if strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), "ghp_") {
				t.Errorf("error %q quotes the record", err)
			}
			if n := len(s.requests()); n != 0 {
				t.Errorf("%d requests, want none", n)
			}
			if c := st.cred.(*staticCredential); len(c.scopes) != 0 {
				t.Errorf("a token was requested")
			}
		})
	}
}

// Values the caller verified, the ID and the fingerprint are kept by the check, so a record that names a
// request by its hashed name is written.
func TestPutKeepsVerifiedValues(t *testing.T) {
	s := newServer(t, status(http.StatusCreated))
	st, _ := testStore(t, s)
	rec := testRecord()
	name := "remediation-0a1b2c3d4e5f6a7b8c9d0e1f"
	rec.Data = []byte(`{"request":"` + name + `","fingerprint":"` + rec.Fingerprint + `"}`)
	rec.Verified = []string{name}
	if _, err := st.Put(t.Context(), rec); err != nil {
		t.Fatalf("Put: %v", err)
	}
}

// A token request that fails is classed: Entra ID throttling is retried, a refusal is final, and the SDK's text
// is never in the error.
func TestPutTokenErrors(t *testing.T) {
	s := newServer(t, status(http.StatusCreated))
	st, _ := testStore(t, s)
	st.cred = &staticCredential{err: errors.New("AZURE_CLIENT_ID=secret-looking-value: no answer")}
	_, err := st.Put(t.Context(), testRecord())
	if !providers.IsRetryable(err) || strings.Contains(err.Error(), "secret-looking-value") {
		t.Fatalf("Put = %v, want a retryable error without the SDK text", err)
	}
	if n := len(s.requests()); n != 0 {
		t.Errorf("%d requests to Blob Storage without a token, want none", n)
	}
}

// A certificate the client does not trust is final: the client of a plain http.Transport does not trust the test
// server's certificate.
func TestPutRefusedCertificateIsFinal(t *testing.T) {
	s := newServer(t, status(http.StatusCreated))
	st, _ := testStore(t, s)
	st.client.Transport = &http.Transport{}
	_, err := st.Put(t.Context(), testRecord())
	if !providers.IsForbidden(err) || providers.IsRetryable(err) {
		t.Fatalf("Put = %v, want a final refusal of the certificate", err)
	}
	if strings.Contains(err.Error(), s.URL) {
		t.Errorf("error %q quotes the URL", err)
	}
}

func TestNewValidatesConfig(t *testing.T) {
	cred := &staticCredential{}
	for name, cfg := range map[string]Config{
		"empty account":        {Account: "", Container: testContainer},
		"short account":        {Account: "ab", Container: testContainer},
		"upper-case account":   {Account: "StAnalyses", Container: testContainer},
		"account with a dot":   {Account: "evil.example", Container: testContainer},
		"long account":         {Account: strings.Repeat("a", 25), Container: testContainer},
		"empty container":      {Account: testAccount, Container: ""},
		"container with slash": {Account: testAccount, Container: "analyses/x"},
		"double hyphen":        {Account: testAccount, Container: "ana--lyses"},
		"trailing hyphen":      {Account: testAccount, Container: "analyses-"},
		"long container":       {Account: testAccount, Container: strings.Repeat("a", 64)},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := New(cfg, Options{Credential: cred}); !providers.IsInvalidConfig(err) {
				t.Errorf("New = %v, want invalid configuration", err)
			}
		})
	}
	st, err := New(Config{Account: testAccount, Container: testContainer}, Options{Credential: cred})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := st.container.String(); got != "https://stswanabc.blob.core.windows.net/analyses" {
		t.Errorf("container URL %q", got)
	}
}

func TestNewWithoutWorkloadIdentity(t *testing.T) {
	t.Setenv("AZURE_CLIENT_ID", "")
	t.Setenv("AZURE_TENANT_ID", "")
	t.Setenv("AZURE_FEDERATED_TOKEN_FILE", "")
	if _, err := New(Config{Account: testAccount, Container: testContainer}, Options{}); !providers.IsInvalidConfig(err) {
		t.Fatalf("New = %v, want invalid configuration", err)
	}
}
