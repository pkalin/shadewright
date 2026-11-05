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
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/pkalin/shadewright/internal/providers"
)

var errClass = errors.New("paused")

func TestCheckWriteWithoutCheckpoint(t *testing.T) {
	if err := providers.CheckWrite(context.Background()); err != nil {
		t.Fatalf("CheckWrite without a checkpoint = %v", err)
	}
}

// TestCheckWriteReturnsOnlyTheClass: the refusal is reduced to its class, or ErrWriteStopped, so none of the
// checkpoint's text is returned.
func TestCheckWriteReturnsOnlyTheClass(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want error
	}{
		{nil, nil},
		{fmt.Errorf("%w: https://10.0.0.1/apis", errClass), errClass},
		{errors.New("https://10.0.0.1/apis: refused"), providers.ErrWriteStopped},
	} {
		c := providers.WithWriteCheckpoint(context.Background(), func(context.Context) error { return tc.err }, errClass)
		if got := providers.CheckWrite(c); got != tc.want { //nolint:errorlint // the exact sentinel, nothing wrapped
			t.Errorf("CheckWrite(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

// TestNilCheckpointKeepsTheInheritedOne: a nil check leaves ctx unchanged, so it cannot remove a checkpoint.
func TestNilCheckpointKeepsTheInheritedOne(t *testing.T) {
	c := providers.WithWriteCheckpoint(context.Background(), func(context.Context) error { return errClass }, errClass)
	c = providers.WithWriteCheckpoint(c, nil)
	if err := providers.CheckWrite(c); !errors.Is(err, errClass) {
		t.Fatalf("CheckWrite after a nil checkpoint = %v, want the inherited refusal", err)
	}
}
