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

// ReadLimit only ever narrows a read: out-of-range limits clamp to 0 and MaxFileBytes (#298).
func TestReadLimit(t *testing.T) {
	for in, want := range map[int]int{
		-1: 0, 0: 0, 1: 1, providers.MaxFileBytes: providers.MaxFileBytes,
		providers.MaxFileBytes + 1: providers.MaxFileBytes, 1 << 40: providers.MaxFileBytes,
	} {
		if got := providers.ReadLimit(in); got != want {
			t.Errorf("ReadLimit(%d) = %d, want %d", in, got, want)
		}
	}
}

// NotSent adds ErrNotSent and keeps the text and the chain of the error it marks: the kinds and the delay the
// remote system asked for still match.
func TestNotSentKeepsTheChain(t *testing.T) {
	if providers.NotSent(nil) != nil {
		t.Error("NotSent(nil) is not nil")
	}
	inner := &providers.RetryableError{RetryAfter: 7 * time.Second,
		Err: fmt.Errorf("the key file: %w", providers.ErrCredentialUnavailable)}
	err := providers.NotSent(inner)
	if !providers.IsNotSent(err) || providers.IsNotSent(inner) {
		t.Errorf("IsNotSent: marked %v, unmarked %v", providers.IsNotSent(err), providers.IsNotSent(inner))
	}
	if !providers.IsCredentialUnavailable(err) || !providers.IsRetryable(err) {
		t.Errorf("the chain is lost: credential %v, retryable %v", providers.IsCredentialUnavailable(err),
			providers.IsRetryable(err))
	}
	if d, ok := providers.RetryAfter(err); !ok || d != 7*time.Second {
		t.Errorf("RetryAfter = %v, %v, want 7s", d, ok)
	}
	if err.Error() != inner.Error() {
		t.Errorf("text %q, want %q", err.Error(), inner.Error())
	}
	var re *providers.RetryableError
	if !errors.As(err, &re) || re != inner {
		t.Error("errors.As does not find the wrapped RetryableError")
	}
}

// NotProcessed adds ErrNotProcessed and keeps the text, the retryable class and the delay of the error it marks,
// without marking it as not sent.
func TestNotProcessedKeepsTheChain(t *testing.T) {
	if providers.NotProcessed(nil) != nil {
		t.Error("NotProcessed(nil) is not nil")
	}
	inner := &providers.RetryableError{RetryAfter: 7 * time.Second, Err: errors.New("status 429")}
	err := providers.NotProcessed(inner)
	if !providers.IsNotProcessed(err) || providers.IsNotProcessed(inner) || providers.IsNotSent(err) {
		t.Errorf("marked %v, unmarked %v, not sent %v", providers.IsNotProcessed(err),
			providers.IsNotProcessed(inner), providers.IsNotSent(err))
	}
	if d, ok := providers.RetryAfter(err); !providers.IsRetryable(err) || !ok || d != 7*time.Second {
		t.Errorf("the chain is lost: retryable %v, RetryAfter %v, %v", providers.IsRetryable(err), d, ok)
	}
	if err.Error() != inner.Error() {
		t.Errorf("text %q, want %q", err.Error(), inner.Error())
	}
}

// A refused call is free only when its error is marked and no attempt was billed without a usage.
func TestRefusedUnbilled(t *testing.T) {
	marked := providers.NotProcessed(errors.New("status 429"))
	for _, tc := range []struct {
		c    providers.Completion
		err  error
		want bool
	}{
		{err: marked, want: true},
		{c: providers.Completion{UsageUnknown: true}, err: marked},
		{err: errors.New("status 429")},
		{err: providers.NotSent(errors.New("no token"))},
		{},
	} {
		if got := providers.RefusedUnbilled(tc.c, tc.err); got != tc.want {
			t.Errorf("RefusedUnbilled(%+v, %v) = %v, want %v", tc.c, tc.err, got, tc.want)
		}
	}
}
