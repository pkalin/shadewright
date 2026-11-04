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

package retryafter

import (
	"net/http"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	const maxDelay = 10 * time.Minute
	cases := map[string]time.Duration{
		"":                    0,
		"7":                   7 * time.Second,
		" 7 ":                 7 * time.Second,
		"0":                   0,
		"-5":                  0,
		"1.5":                 0,
		"600":                 maxDelay,
		"601":                 maxDelay,
		"99999999999999":      maxDelay,
		"9223372036854775807": maxDelay,
		"soon":                0,
		now.Add(20 * time.Second).Format(http.TimeFormat): 20 * time.Second,
		now.Add(time.Hour).Format(http.TimeFormat):        maxDelay,
		now.Add(-time.Minute).Format(http.TimeFormat):     0,
		now.Add(20 * time.Second).Format(time.RFC850):     20 * time.Second,
	}
	for v, want := range cases {
		if got := Parse(v, now, maxDelay); got != want {
			t.Errorf("Parse(%q) = %s, want %s", v, got, want)
		}
	}
	if got := Parse("7", now, 0); got != 0 {
		t.Errorf("Parse with no cap = %s, want 0", got)
	}
}
