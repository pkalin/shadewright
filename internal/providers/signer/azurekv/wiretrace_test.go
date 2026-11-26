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
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"

	"github.com/shadewright-labs/shadewright/internal/testutil/tracecanary"
)

// No trace hook in the caller's context sees a request the signer sends, so none sees the bearer token: not
// the challenge, not across a retry, not on a later Sign. A control request built with the caller's context
// shows the token to the recorder, so the test can fail. The caller's cancellation and deadline still end a
// Sign in flight.
func TestCallerTraceHooksNeverSeeTheBearer(t *testing.T) {
	var inner policy.Transporter
	s, v := setupWith(t, "https://"+testHost+"/keys/github-app", Options{}, &fakeCredential{},
		func(tr policy.Transporter) policy.Transporter {
			inner = tr
			return tr
		}, clientConfig{})
	var hold atomic.Bool
	arrivals := tracecanary.NewArrivals()
	sign := v.signOK("https://" + testHost + "/keys/github-app/" + testVersion)
	v.respond = func(n int, w http.ResponseWriter, r *http.Request, body []byte) {
		switch {
		case hold.Load():
			arrivals.Arrived()
			<-r.Context().Done()
		case n == 1:
			writeError(w, http.StatusServiceUnavailable, "ServiceUnavailable")
		default:
			sign(n, w, r, body)
		}
	}

	var rec tracecanary.Recorder
	ctx := rec.Context(context.Background())
	for range 2 {
		if _, err := s.Sign(ctx, digestOf("header.payload")); err != nil {
			t.Fatalf("Sign: %v", err)
		}
	}
	if got := len(v.authorized()); got != 3 {
		t.Errorf("%d authorized requests, want 3 (one retried)", got)
	}
	if calls := rec.Calls(); len(calls) != 0 {
		t.Errorf("the caller's trace saw the signer's requests: %v", calls)
	}

	// Control: a request built the old way, with the caller's context and the bearer.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+testHost+"/keys/github-app", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := inner.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if len(rec.Seen(testToken)) == 0 {
		t.Fatalf("control: the recorder did not see the bearer; calls: %v", rec.Calls())
	}

	hold.Store(true)
	tracecanary.Ends(t, arrivals, func(ctx context.Context) error {
		_, err := s.Sign(ctx, digestOf("header.payload"))
		return err
	})
}
