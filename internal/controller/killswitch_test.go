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
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/pkalin/shadewright/internal/policy/freshness"
	"github.com/pkalin/shadewright/internal/providers"
	fakegit "github.com/pkalin/shadewright/internal/providers/fake"
)

// gitReads are the GitProvider methods that write nothing to the host. Every other method is a write, and a
// write method added to the interface is gated by this test's rule until it is listed here on purpose.
var gitReads = map[string]bool{"Capabilities": true, "ReadFile": true, "BranchHead": true, opFind: true}

// TestGatedGitGatesEveryWrite calls every GitProvider method of a gated provider whose gate refuses, and checks
// that each write asked the gate and never reached the host, and that no read asked it.
func TestGatedGitGatesEveryWrite(t *testing.T) {
	typ := reflect.TypeFor[providers.GitProvider]()
	writes := 0
	for m := range typ.Methods() {
		t.Run(m.Name, func(t *testing.T) {
			inner := fakegit.NewGit("main", map[string][]byte{"a.yaml": []byte("a: 1\n")})
			var reached []string
			inner.SetFail(func(op string) error {
				reached = append(reached, op)
				return nil
			})
			asked := 0
			gated := &gatedGit{git: inner, allow: func(context.Context) error {
				asked++
				return freshness.ErrPaused
			}}
			args := []reflect.Value{}
			for in := range m.Type.Ins() {
				if in == reflect.TypeFor[context.Context]() {
					args = append(args, reflect.ValueOf(context.Background()))
					continue
				}
				args = append(args, reflect.Zero(in))
			}
			out := reflect.ValueOf(gated).MethodByName(m.Name).Call(args)
			if gitReads[m.Name] {
				if asked != 0 {
					t.Fatalf("the read %s asked the gate", m.Name)
				}
				return
			}
			writes++
			if asked != 1 {
				t.Fatalf("the write %s asked the gate %d times, want once", m.Name, asked)
			}
			if len(reached) != 0 {
				t.Fatalf("the write %s reached the host while the gate refused: %v", m.Name, reached)
			}
			err, _ := reflect.TypeAssert[error](out[len(out)-1])
			if !errors.Is(err, freshness.ErrPaused) {
				t.Fatalf("the write %s returned %v, want the gate's error", m.Name, err)
			}
		})
	}
	if writes != typ.NumMethod()-len(gitReads) {
		t.Fatalf("checked %d writes, want %d", writes, typ.NumMethod()-len(gitReads))
	}
}

// TestGatedGitWritesWhenAllowed checks that an allowed write reaches the host.
func TestGatedGitWritesWhenAllowed(t *testing.T) {
	inner := fakegit.NewGit("main", map[string][]byte{"a.yaml": []byte("a: 1\n")})
	gated := &gatedGit{git: inner, allow: func(context.Context) error { return nil }}
	ctx := context.Background()
	head, err := inner.BranchHead(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := gated.CreateBranch(ctx, "shadewright/x", head); err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}
	if got, err := inner.BranchHead(ctx, "shadewright/x"); err != nil || got != head {
		t.Fatalf("the branch was not created: %q, %v", got, err)
	}
}

// TestProviderErrorKeepsTheKillSwitch checks that wrapping a gate refusal as a provider error keeps it, so the
// caller still tells a pause from a failure of the host.
func TestProviderErrorKeepsTheKillSwitch(t *testing.T) {
	for _, want := range []error{freshness.ErrPaused, freshness.ErrWriteBlocked} {
		if err := freshness.WrapProvider("create the branch", want); !errors.Is(err, want) {
			t.Fatalf("WrapProvider lost %v: %v", want, err)
		}
	}
}
