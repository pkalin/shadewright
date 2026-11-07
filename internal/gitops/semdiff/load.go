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
package semdiff

import (
	"context"
	"fmt"

	"github.com/pkalin/shadewright/internal/gitops/sourcemap"
	"github.com/pkalin/shadewright/internal/providers"
)

// Load reads from git, at commit, every file the kustomization in dir builds from: the kustomization files
// and the resource and patch files they list, following nested directories and components. The result is
// the Files of an [Input]. dir is the Kustomization's spec.path, relative to the repository root.
//
// It refuses with an [*Error] what [Check] would refuse in the tree itself (a field outside the allow-list,
// a remote or escaping reference, a missing file, a .sourceignore file, a limit), and returns a [*sourcemap.TransientError]
// (matching [sourcemap.ErrTransient]) when Git cannot be read; that error carries no provider text.
func Load(ctx context.Context, git providers.GitProvider, commit, dir string) (map[string][]byte, error) {
	root, ok := cleanPath(dir)
	if !ok {
		return nil, refuse(ReasonInvalidInput, "the path is not a relative path inside the repository")
	}
	reads := 0
	w := newWalker(func(ctx context.Context, p string) ([]byte, bool, error) {
		if reads++; reads > MaxReads {
			return nil, false, refuse(ReasonTooLarge, fmt.Sprintf("the build needs more than %d file reads", MaxReads))
		}
		f, err := git.ReadFile(ctx, commit, p, providers.MaxFileBytes)
		switch {
		case providers.IsNotFound(err):
			return nil, false, nil
		case providers.IsTooLarge(err):
			return nil, false, refuse(ReasonTooLarge, fmt.Sprintf("%s is larger than %d bytes", p, MaxFileBytes))
		case err != nil:
			d, _ := providers.RetryAfter(err)
			return nil, false, &sourcemap.TransientError{Op: "read " + p, RetryAfter: d}
		}
		return f.Data, true, nil
	})
	if err := w.walk(ctx, root, kindKustomization, 0); err != nil {
		return nil, err
	}
	return w.files, nil
}
