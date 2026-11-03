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

package ratelimit

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

func resp(status int, body string, kv ...string) *http.Response {
	h := http.Header{}
	for i := 0; i < len(kv); i += 2 {
		h.Set(kv[i], kv[i+1])
	}
	return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(body))}
}

func TestLimited(t *testing.T) {
	reset := func(d time.Duration) string { return strconv.FormatInt(now.Add(d).Unix(), 10) }
	cases := []struct {
		name    string
		r       *http.Response
		limited bool
		want    time.Duration
	}{
		{"retry-after", resp(403, "", "Retry-After", "12"), true, 12 * time.Second},
		{"retry-after capped", resp(403, "", "Retry-After", "999999"), true, time.Hour},
		{"retry-after date", resp(429, "", "Retry-After", now.Add(20*time.Second).Format(http.TimeFormat)), true, 20 * time.Second},
		{"quota reset", resp(403, "", "X-RateLimit-Remaining", "0", "X-RateLimit-Reset", reset(45*time.Second)), true, 45 * time.Second},
		{"quota reset in the past", resp(403, "", "X-RateLimit-Remaining", "0", "X-RateLimit-Reset", reset(-time.Minute)), true, time.Minute},
		{"headerless 429", resp(429, ""), true, time.Minute},
		{"secondary 403", resp(403, "You have exceeded a secondary rate limit", "X-RateLimit-Remaining", "10"), true, time.Minute},
		{"abuse 403", resp(403, "Abuse Detection mechanism"), true, time.Minute},
		{"plain 403", resp(403, "Resource not accessible"), false, 0},
	}
	for _, c := range cases {
		d, ok := Limited(c.r, now)
		if ok != c.limited || d != c.want {
			t.Errorf("%s: Limited() = %v, %v; want %v, %v", c.name, d, ok, c.want, c.limited)
		}
	}
}

// failingReader returns some bytes, then an error, like a connection reset or a body cut short.
type failingReader struct {
	data []byte
	err  error
}

func (f *failingReader) Read(p []byte) (int, error) {
	if len(f.data) == 0 {
		return 0, f.err
	}
	n := copy(p, f.data)
	f.data = f.data[n:]
	return n, nil
}

// A 403 whose body cannot be read to the end of the probe may have hidden the rate limit message: it is
// retryable, never a refusal for good.
func TestUnreadableBodyIsALimit(t *testing.T) {
	for _, err := range []error{io.ErrUnexpectedEOF, errors.New("connection reset by peer")} {
		resp := &http.Response{StatusCode: http.StatusForbidden, Header: http.Header{},
			Body: io.NopCloser(&failingReader{data: []byte(`{"message":"You have exc`), err: err})}
		d, limited := Limited(resp, time.Now())
		if !limited || d != SecondaryWait {
			t.Fatalf("%v: got (%v, %v), want a rate limit with the secondary wait", err, d, limited)
		}
	}
}
