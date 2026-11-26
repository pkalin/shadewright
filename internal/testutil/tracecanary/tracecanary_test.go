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

package tracecanary

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// holdingServer answers no request: it reports each one to the returned Arrivals and holds it until the
// client goes away.
func holdingServer(t *testing.T) (*httptest.Server, *Arrivals) {
	t.Helper()
	a := NewArrivals()
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		a.Arrived()
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	return srv, a
}

func get(srv *httptest.Server) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
		if err != nil {
			return err
		}
		resp, err := srv.Client().Do(req)
		if err == nil {
			_ = resp.Body.Close()
		}
		return err
	}
}

// A request held by the server ends with its context's error, for both ways a context ends.
func TestEndsPassesARequestThatEndsWithItsContext(t *testing.T) {
	srv, a := holdingServer(t)
	Ends(t, a, get(srv))
}

// A call that fails before the server has its request, or that fails with an error other than the end of its
// context, is refused, so a client that never sends cannot pass.
func TestEndsRefusesAnUnrelatedFailure(t *testing.T) {
	srv, a := holdingServer(t)
	unrelated := errors.New("fixture: validation failed before send")
	for _, tc := range []struct {
		name string
		call func(ctx context.Context) error
	}{
		{"before the request arrives", func(context.Context) error { return unrelated }},
		{"another error after it arrived", func(ctx context.Context) error {
			_ = get(srv)(ctx)
			return unrelated
		}},
	} {
		for _, m := range []mode{cancelMode, deadlineMode} {
			t.Run(tc.name+"/"+string(m), func(t *testing.T) {
				if err := ends(a, m, tc.call); err == nil {
					t.Error("ends accepted the call")
				}
			})
		}
	}
}
