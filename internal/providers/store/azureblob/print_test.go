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
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"

	"github.com/shadewright-labs/shadewright/internal/redact"
	"github.com/shadewright-labs/shadewright/internal/testutil/printguard"
)

// fakeEntra answers the OpenID configuration and the token request of a workload identity credential with
// the access token it holds.
type fakeEntra struct{ token string }

func (e fakeEntra) Do(req *http.Request) (*http.Response, error) {
	base := "https://login.microsoftonline.com/" + req.URL.Path[1:strings.Index(req.URL.Path[1:], "/")+1]
	var v any = map[string]string{
		"authorization_endpoint": base + "/oauth2/v2.0/authorize",
		"token_endpoint":         base + "/oauth2/v2.0/token",
		"issuer":                 base + "/v2.0",
	}
	if strings.HasSuffix(req.URL.Path, "/oauth2/v2.0/token") {
		v = map[string]any{"token_type": "Bearer", "expires_in": 3600, "access_token": e.token}
	}
	b, _ := json.Marshal(v)
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(strings.NewReader(string(b))), Request: req}, nil
}

// A Store whose credential is azidentity's workload identity, after a token request, shows neither the
// projected service account token it read nor the access token it got, in any printed, logged or encoded
// form.
func TestStoreNeverPrintsTheCredential(t *testing.T) {
	assertion := strings.Join([]string{"eyJhbGciOiJSUzI1NiJ9", "ASSERTIONCANARY", "c0d1"}, ".")
	access := strings.Join([]string{"eyJ0eXAiOiJKV1QifQ", "ACCESSCANARY", "e2f3"}, ".")
	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte(assertion), 0o600); err != nil {
		t.Fatal(err)
	}
	cred, err := azidentity.NewWorkloadIdentityCredential(&azidentity.WorkloadIdentityCredentialOptions{
		ClientID: "00000000-0000-0000-0000-000000000001", TenantID: "00000000-0000-0000-0000-000000000002",
		TokenFilePath: file, DisableInstanceDiscovery: true,
		ClientOptions: azcore.ClientOptions{Transport: fakeEntra{token: access}},
	})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := cred.GetToken(t.Context(), policy.TokenRequestOptions{Scopes: []string{storageScope}})
	if err != nil || tok.Token != access {
		t.Fatalf("GetToken = %v, want the fake access token", err)
	}
	red, err := redact.New()
	if err != nil {
		t.Fatal(err)
	}
	st := newStore(&url.URL{Scheme: "https", Host: testAccount + blobHostSuffix, Path: "/" + testContainer}, cred, red, nil)
	printguard.Check(t, st, assertion)
	printguard.Check(t, st, access)
}
