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

package prometheus

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pkalin/shadewright/internal/providers"
)

// lateAnswer is a transport whose answer arrives only once the request's context has ended, as net/http can
// return one that raced the end of the context (#767). It ends the caller's context itself, so the test needs
// no clock. With inBody set, the context ends instead while the body is read, which then reads whole.
type lateAnswer struct {
	code   int
	end    context.CancelFunc
	inBody bool
}

func (a lateAnswer) RoundTrip(r *http.Request) (*http.Response, error) {
	var body io.Reader = strings.NewReader(`{"status":"success","data":{"resultType":"vector","result":[]}}`)
	if a.inBody {
		body = io.MultiReader(endingReader{a.end}, body)
	} else {
		a.end()
		<-r.Context().Done()
	}
	return &http.Response{StatusCode: a.code, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(body), Request: r}, nil
}

// endingReader ends a context on its first read and then reports EOF, so a MultiReader goes on to the body.
type endingReader struct{ end context.CancelFunc }

func (e endingReader) Read([]byte) (int, error) {
	e.end()
	return 0, io.EOF
}

// A query whose context ended reports that end, whatever answer arrived after it (#779): no result, no
// refusal and no Forbidden taken from that answer.
func TestAnswerAfterContextEnd(t *testing.T) {
	for _, c := range []struct {
		code   int
		inBody bool
	}{
		{http.StatusOK, false}, {http.StatusOK, true}, {http.StatusBadRequest, false}, {http.StatusForbidden, false},
	} {
		t.Run(strconv.Itoa(c.code)+"/"+strconv.FormatBool(c.inBody), func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			s, err := New(Config{
				URL: "https://prometheus.example/prom", Namespaces: []string{testNS},
				Client: &http.Client{Transport: lateAnswer{code: c.code, end: cancel, inBody: c.inBody}},
			})
			if err != nil {
				t.Fatal(err)
			}
			s.now = func() time.Time { return now }
			_, err = s.Query(ctx, instant("container_restarts_total"))
			if !errors.Is(err, context.Canceled) || providers.IsRejected(err) || providers.IsForbidden(err) {
				t.Fatalf("Query = %v, want the caller's cancellation", err)
			}
		})
	}
}
