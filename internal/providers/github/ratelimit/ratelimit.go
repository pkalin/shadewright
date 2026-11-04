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

// Package ratelimit holds GitHub's rate limit rules shared by the installation token source and the REST
// adapter: how long to wait, and whether a 403 or 429 is a rate limit at all. Nothing here returns or keeps
// response text.
package ratelimit

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/pkalin/shadewright/internal/providers/retryafter"
)

const (
	// SecondaryWait is GitHub's advice for a rate limit that comes without a usable delay: at least a minute.
	SecondaryWait = time.Minute
	// MaxDelay caps any delay GitHub asks for.
	MaxDelay = time.Hour
	// maxProbeBody bounds the part of a 403 body searched for a secondary rate limit.
	maxProbeBody = 16 << 10
)

// RetryAfter reads Retry-After in seconds or as an HTTP date; without it, an exhausted quota with
// X-RateLimit-Reset (Unix seconds) gives the delay until the reset. Capped at [MaxDelay]; past or
// unparsable values give zero.
func RetryAfter(h http.Header, now time.Time) time.Duration {
	var d time.Duration
	if v := h.Get("Retry-After"); v != "" {
		d = retryafter.Parse(v, now, MaxDelay)
	} else if h.Get("X-RateLimit-Remaining") == "0" {
		if reset, err := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			d = time.Unix(reset, 0).Sub(now)
		}
	}
	return min(max(d, 0), MaxDelay)
}

// Delay is [RetryAfter] with [SecondaryWait] as the fallback when the headers give no delay.
func Delay(h http.Header, now time.Time) time.Duration {
	if d := RetryAfter(h, now); d > 0 {
		return d
	}
	return SecondaryWait
}

// Limited reports whether a 403 or 429 response is a rate limit, and the delay to wait. GitHub's advice:
// obey Retry-After, else wait for X-RateLimit-Reset when the quota is exhausted, else wait at least a
// minute. A 429 without those headers is a rate limit; a 403 without them only when its body says so, or
// when the body cannot be read. The body is searched through a bounded reader and never kept or returned.
func Limited(resp *http.Response, now time.Time) (time.Duration, bool) {
	h := resp.Header
	if h.Get("Retry-After") != "" || h.Get("X-RateLimit-Remaining") == "0" {
		return Delay(h, now), true
	}
	if resp.StatusCode == http.StatusTooManyRequests || mentionsRateLimit(resp.Body) {
		return SecondaryWait, true
	}
	return 0, false
}

// mentionsRateLimit reports whether the start of a 403 body names a rate limit. A body that cannot be read to
// the end of the probe (a reset, a stall, a truncated response) may have hidden the message, so it counts as
// a rate limit: retrying is the safe side, a refusal for good is never assumed from a failed read.
func mentionsRateLimit(r io.Reader) bool {
	if r == nil {
		return false
	}
	data, err := io.ReadAll(io.LimitReader(r, maxProbeBody))
	if err != nil {
		return true
	}
	s := strings.ToLower(string(data))
	return strings.Contains(s, "rate limit") || strings.Contains(s, "abuse detection")
}
