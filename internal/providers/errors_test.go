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
	fb := fmt.Errorf("create branch: %w", providers.ErrForbidden)
	rj := fmt.Errorf("open: %w", providers.ErrRejected)
	if !providers.IsForbidden(fb) || providers.IsRejected(fb) || providers.IsRetryable(fb) || providers.IsConflict(fb) {
		t.Error("forbidden misclassified")
	}
	if !providers.IsRejected(rj) || providers.IsForbidden(rj) || providers.IsRetryable(rj) || providers.IsNotFound(rj) {
		t.Error("rejected misclassified")
	}
	if providers.IsForbidden(rt) || providers.IsRejected(rt) {
		t.Error("a rate limit is neither forbidden nor rejected")
	}
	mo := fmt.Errorf("complete: %w", providers.ErrMalformedOutput)
	if !providers.IsMalformedOutput(mo) || providers.IsRetryable(mo) || providers.IsRejected(mo) {
		t.Error("malformed output misclassified")
	}
	if providers.IsMalformedOutput(rj) || providers.IsMalformedOutput(rt) {
		t.Error("rejected or retryable counted as malformed output")
	}
	if !providers.IsRetryable(fmt.Errorf("x: %w", providers.ErrRetryable)) {
		t.Error("plain ErrRetryable not retryable")
	}
}

// A path not in the tree is a not found too, so existing callers keep working; a plain not found is not one.
func TestPathNotInTreeIsANotFound(t *testing.T) {
	pt := fmt.Errorf("read file: %w", providers.ErrPathNotInTree)
	if !providers.IsPathNotInTree(pt) || !providers.IsNotFound(pt) || providers.IsRetryable(pt) {
		t.Error("path not in tree misclassified")
	}
	if providers.IsPathNotInTree(fmt.Errorf("read file: %w", providers.ErrNotFound)) {
		t.Error("a plain not found counts as a path not in the tree")
	}
}

func TestIsObjectID(t *testing.T) {
	sha1 := "0123456789abcdef0123456789abcdef01234567"
	sha256 := sha1 + "89abcdef0123456789abcdef"
	for s, want := range map[string]bool{
		sha1:               true,
		sha256:             true,
		"":                 false,
		sha1[:39]:          false,
		sha1 + "0":         false,
		sha256 + "0":       false,
		sha1[:39] + "A":    false,
		sha1[:39] + "g":    false,
		sha1[:39] + "/":    false,
		sha1[:39] + ":":    false,
		"main":             false,
		sha1[:39] + "\x00": false,
	} {
		if got := providers.IsObjectID(s); got != want {
			t.Errorf("IsObjectID(%q) = %v, want %v", s, got, want)
		}
	}
}
