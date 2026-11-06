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

// TestPullRequestPollIntervalBounds checks that the flag refuses a poll interval out of its bounds and that the
// reconciler never polls faster or slower than them (#434).
func TestPullRequestPollIntervalBounds(t *testing.T) {
	for _, tc := range []struct {
		in      time.Duration
		valid   bool
		applied time.Duration
	}{
		{in: 0, valid: false, applied: DefaultPullRequestPollInterval},
		{in: -time.Second, valid: false, applied: DefaultPullRequestPollInterval},
		{in: time.Second, valid: false, applied: MinPullRequestPollInterval},
		{in: MinPullRequestPollInterval - time.Nanosecond, valid: false, applied: MinPullRequestPollInterval},
		{in: MinPullRequestPollInterval, valid: true, applied: MinPullRequestPollInterval},
		{in: DefaultPullRequestPollInterval, valid: true, applied: DefaultPullRequestPollInterval},
		{in: MaxPullRequestPollInterval, valid: true, applied: MaxPullRequestPollInterval},
		{in: MaxPullRequestPollInterval + time.Nanosecond, valid: false, applied: MaxPullRequestPollInterval},
		{in: time.Hour, valid: false, applied: MaxPullRequestPollInterval},
	} {
		if err := ValidatePullRequestPollInterval(tc.in); (err == nil) != tc.valid {
			t.Errorf("ValidatePullRequestPollInterval(%s) = %v, want valid %v", tc.in, err, tc.valid)
		}
		r := &RemediationRequestReconciler{PullRequestPollInterval: tc.in}
		if got := r.pullRequestPollInterval(); got != tc.applied {
			t.Errorf("pullRequestPollInterval() with %s = %s, want %s", tc.in, got, tc.applied)
		}
	}
	if DefaultPullRequestPollInterval >= freshRecheckInterval {
		t.Errorf("the default poll interval %s does not poll between the full rechecks (%s)",
			DefaultPullRequestPollInterval, freshRecheckInterval)
	}
}
