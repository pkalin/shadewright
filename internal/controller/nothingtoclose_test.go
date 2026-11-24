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
	"fmt"
	"io"
	"net/url"
	"testing"

	"github.com/shadewright-labs/shadewright/internal/policy/freshness"
	"github.com/shadewright-labs/shadewright/internal/providers"
)

// TestNothingToCloseNamesItsCases pins which errors of the close after a stale stop mean "nothing to close". An
// error outside the list, like a transport failure of a cluster read that is no cluster error by type, must not be
// taken for one: closeAfterStaleStop returns it, so the retry is the work queue's and not a silent requeue.
func TestNothingToCloseNamesItsCases(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"merged while closing", fmt.Errorf("close: %w", providers.ErrConflict), true},
		{"not this request's", errChangeRequestNotOurs, true},
		{"handoff in flight", errHandoffInFlight, true},
		{"repository gone", fmt.Errorf("entry: %w", errRepositoryGone), true},
		{"no settings", errNoSettings, true},
		{"target branch changed", fmt.Errorf("entry: %w", errTargetBranchChanged), true},
		{"entry unusable", fmt.Errorf("entry: %w", freshness.ErrRepositoryUnusable), true},
		{"transport failure", &url.Error{Op: "Get", URL: "https://cluster.invalid", Err: io.EOF}, false},
		{"unknown", fmt.Errorf("something else"), false},
	} {
		if got := nothingToClose(tc.err); got != tc.want {
			t.Errorf("%s: nothingToClose = %t, want %t", tc.name, got, tc.want)
		}
	}
}
