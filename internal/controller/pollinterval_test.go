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

package controller

import (
	"testing"
	"time"
)

// TestPullRequestPollIntervalBounds checks that a poll interval out of its bounds is refused, never changed, and
// that zero means the default (#434).
func TestPullRequestPollIntervalBounds(t *testing.T) {
	for _, tc := range []struct {
		in    time.Duration
		valid bool
		used  time.Duration
	}{
		{in: 0, valid: true, used: DefaultPullRequestPollInterval},
		{in: -time.Second, valid: false, used: -time.Second},
		{in: time.Second, valid: false, used: time.Second},
		{in: MinPullRequestPollInterval - time.Nanosecond, valid: false, used: MinPullRequestPollInterval - time.Nanosecond},
		{in: MinPullRequestPollInterval, valid: true, used: MinPullRequestPollInterval},
		{in: DefaultPullRequestPollInterval, valid: true, used: DefaultPullRequestPollInterval},
		{in: MaxPullRequestPollInterval, valid: true, used: MaxPullRequestPollInterval},
		{in: MaxPullRequestPollInterval + time.Nanosecond, valid: false, used: MaxPullRequestPollInterval + time.Nanosecond},
		{in: time.Hour, valid: false, used: time.Hour},
	} {
		r := &RemediationRequestReconciler{PullRequestPollInterval: tc.in}
		if err := r.validatePullRequestPollInterval(); (err == nil) != tc.valid {
			t.Errorf("validatePullRequestPollInterval() with %s = %v, want valid %v", tc.in, err, tc.valid)
		}
		if got := r.pullRequestPollInterval(); got != tc.used {
			t.Errorf("pullRequestPollInterval() with %s = %s, want %s", tc.in, got, tc.used)
		}
	}
	if DefaultPullRequestPollInterval >= FullRecheckInterval {
		t.Errorf("the default poll interval %s does not poll between the full rechecks (%s)",
			DefaultPullRequestPollInterval, FullRecheckInterval)
	}
}

// TestShortDuration checks the format of the bounds in the flag's help and in the refusal.
func TestShortDuration(t *testing.T) {
	for in, want := range map[time.Duration]string{
		5 * time.Minute: "5m", time.Minute: "1m", 15 * time.Second: "15s", 10 * time.Second: "10s",
		90 * time.Second: "1m30s", 0: "0s",
	} {
		if got := ShortDuration(in); got != want {
			t.Errorf("ShortDuration(%s) = %q, want %q", in, got, want)
		}
	}
}
