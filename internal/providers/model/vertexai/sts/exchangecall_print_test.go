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

package sts

import (
	"strings"
	"testing"

	"github.com/shadewright-labs/shadewright/internal/testutil/printguard"
)

// A finished exchange, alone or held by another value, shows no part of its token in any printed, logged or
// encoded form, and still hands the token to its waiters.
func TestFinishedExchangeNeverPrintsTheToken(t *testing.T) {
	canary := strings.Join([]string{"ya29", "EXCHANGECANARY", "a4b5"}, ".")
	call := &exchangeCall{done: make(chan struct{}), cancel: func() {}}
	call.finish(canary, nil)
	printguard.Check(t, call, canary)
	if got, err := call.result(); got != canary || err != nil {
		t.Errorf("result = %q, %v; want the token", got, err)
	}
}
