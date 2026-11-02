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

package providers_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/pkalin/shadewright/internal/providers"
)

func TestErrorKinds(t *testing.T) {
	nf := fmt.Errorf("read file: %w", providers.ErrNotFound)
	cf := fmt.Errorf("commit: %w", providers.ErrConflict)
	rt := fmt.Errorf("open: %w", &providers.RetryableError{RetryAfter: 30 * time.Second, Err: errors.New("429")})

	if !providers.IsNotFound(nf) || providers.IsConflict(nf) || providers.IsRetryable(nf) {
		t.Error("not found misclassified")
	}
	if !providers.IsConflict(cf) || providers.IsNotFound(cf) || providers.IsRetryable(cf) {
		t.Error("conflict misclassified")
	}
	if !providers.IsRetryable(rt) || providers.IsConflict(rt) {
		t.Error("retryable misclassified")
	}
	if d, ok := providers.RetryAfter(rt); !ok || d != 30*time.Second {
		t.Errorf("RetryAfter = %v, %v", d, ok)
	}
	if _, ok := providers.RetryAfter(nf); ok {
		t.Error("RetryAfter on a non-retryable error")
	}
	if !providers.IsRetryable(fmt.Errorf("x: %w", providers.ErrRetryable)) {
		t.Error("plain ErrRetryable not retryable")
	}
}
