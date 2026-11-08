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
package github

import (
	"context"
	"errors"
	"net/http"
	"net/url"

	"github.com/pkalin/shadewright/internal/providers"
)

const (
	opIsAncestor = "GitHub check ancestry"
	perPage      = "per_page"
)

var _ providers.AncestryChecker = (*Provider)(nil)

// IsAncestor reports whether ancestor is commit or one of its ancestors, with the compare API in the
// direction commit...ancestor: an ancestor is "behind" commit, so the answer lists no commits and no files
// and stays small. Any other status ("ahead", "diverged") is false. A body over the call's limit can only be
// one with commits or files to list, so it is false too, as for the base of a new branch.
func (p *Provider) IsAncestor(ctx context.Context, ancestor, commit string) (bool, error) {
	if !validSHA(ancestor) || !validSHA(commit) {
		return false, errors.New(opIsAncestor + ": commit is not a full SHA")
	}
	if ancestor == commit {
		return true, nil
	}
	ctx, cancel := p.withTimeout(ctx)
	defer cancel()
	r, err := p.do(ctx, call{
		op: opIsAncestor, method: http.MethodGet, path: "/compare/" + commit + "..." + ancestor,
		query: url.Values{perPage: {"1"}},
	})
	if errors.Is(err, errBodyTooLarge) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !r.ok() {
		return false, statusError(opIsAncestor, r.status)
	}
	var c compareJSON
	if err := decode(opIsAncestor, r, &c); err != nil {
		return false, err
	}
	switch c.Status {
	case "behind", "identical":
		return true, nil
	case "ahead", "diverged":
		return false, nil
	}
	return false, errors.New(opIsAncestor + ": GitHub returned an unexpected compare status")
}
