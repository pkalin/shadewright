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

package fake_test

import (
	"context"
	"strings"
	"testing"

	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/providers/fake"
)

// UpdateChangeRequest refuses a title or body the GitHub adapter refuses with ErrRejected, after the open state
// like there (#604).
func TestGitUpdateChangeRequestRejectsInvalidInput(t *testing.T) {
	ctx := context.Background()
	g := fake.NewGit(base, map[string][]byte{path: []byte(oldVal)})
	head, err := g.BranchHead(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.CreateBranch(ctx, branch, head); err != nil {
		t.Fatal(err)
	}
	cr, err := g.OpenChangeRequest(ctx, providers.NewChangeRequest{HeadBranch: branch, BaseBranch: base, Title: "t", Body: "b"})
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string][2]string{
		"blank title":   {" ", "b"},
		"long title":    {strings.Repeat("t", 257), "b"},
		"invalid title": {"\xff", "b"},
		"invalid body":  {"t", "\xff"},
		"long body":     {"t", strings.Repeat("b", 65537)},
	} {
		if err := g.UpdateChangeRequest(ctx, cr.Number, c[0], c[1]); !providers.IsRejected(err) {
			t.Errorf("%s: got %v, want a rejection", name, err)
		}
	}
	if err := g.UpdateChangeRequest(ctx, cr.Number, "t2", "b2"); err != nil {
		t.Fatal(err)
	}
}
