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

// Package retryafter parses the Retry-After header of an HTTP response (RFC 9110, section 10.2.3) for the
// adapters under internal/providers. It imports the standard library only.
package retryafter

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Parse reads a Retry-After value in seconds or as an HTTP date and returns the delay it asks for, capped at
// maxDelay. A date is measured from now. A past date, a negative number or an unparsable value gives zero.
func Parse(v string, now time.Time, maxDelay time.Duration) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" || maxDelay <= 0 {
		return 0
	}
	var d time.Duration
	if secs, err := strconv.ParseInt(v, 10, 64); err == nil {
		// Capped in seconds first: a huge value must not overflow the Duration.
		d = time.Duration(min(max(secs, 0), int64(maxDelay/time.Second)+1)) * time.Second
	} else if t, err := http.ParseTime(v); err == nil {
		d = t.Sub(now)
	}
	return min(max(d, 0), maxDelay)
}
