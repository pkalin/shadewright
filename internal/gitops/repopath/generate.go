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

package repopath

import (
	"cmp"
	"context"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/pkalin/shadewright/internal/providers"
)

// MaxListDepth is how far below a Flux Kustomization's path [Generate] lists directories: the path itself is
// depth 0, so a directory at depth MaxListDepth is still listed and one below it stops the walk.
const MaxListDepth = 5

// fluxIgnored are the names Flux's source-controller leaves out of every GitRepository artifact by default
// (its VCS, CI and extra exclusions), at any depth. A repository read in full holds them while the artifact
// Flux builds from does not, so the walk stops at one instead of reading what Flux would not apply.
var fluxIgnored = map[string]bool{
	".git": true, ".gitignore": true, ".gitmodules": true, ".gitattributes": true,
	".github": true, ".circleci": true, ".travis.yml": true, ".gitlab-ci.yml": true, "appveyor.yml": true,
	".drone.yml": true, "cloudbuild.yaml": true, "codeship-services.yml": true, "codeship-steps.yml": true,
	".goreleaser.yml": true, ".sops.yaml": true, ".flux.yaml": true,
}

// Resource is one entry of the kustomization Flux generates for a directory without one: a manifest file, or
// a directory that holds its own kustomization file and is built as a kustomize base.
type Resource struct {
	// Path is the repository path, built from validated segments.
	Path string
	// Dir is true for a directory with a kustomization file.
	Dir bool
}

// Lister lists one directory, as [providers.GitProvider.ListDir] does. It is where the caller counts the
// listing against its budget; an error it returns ends [Generate] and is returned as it is.
type Lister func(ctx context.Context, dir string) ([]providers.DirEntry, error)

// StopKind says why [Generate] stopped on what it listed.
type StopKind int

// Reasons for a [*Stop].
const (
	// StopTooDeep means a directory is nested more than MaxListDepth below the root.
	StopTooDeep StopKind = iota + 1
	// StopInvalidName means a listed name is not one plain path segment (see [Join]), or an entry is neither
	// a regular file nor a directory.
	StopInvalidName
	// StopSourceIgnore means a listed directory holds a .sourceignore file.
	StopSourceIgnore
	// StopFluxIgnored means a listed name is one Flux leaves out of its artifact by default.
	StopFluxIgnored
)

// Stop is the error [Generate] returns when what it listed cannot be built as Flux would build it. Dir is the
// validated directory the entry was listed in; the entry's own name is not kept, since it is not validated.
type Stop struct {
	Kind StopKind
	Dir  string
}

// Error describes the stop without quoting any listed name.
func (s *Stop) Error() string {
	switch s.Kind {
	case StopTooDeep:
		return fmt.Sprintf("directories below %s are nested deeper than %d", s.Dir, MaxListDepth)
	case StopSourceIgnore:
		return fmt.Sprintf("%s exists, so Flux may leave files out of what it applies", path.Join(s.Dir, SourceIgnoreFile))
	case StopFluxIgnored:
		return fmt.Sprintf("directory %s holds a file or directory that Flux leaves out of what it applies by default", s.Dir)
	default:
		return fmt.Sprintf("directory %s holds an entry whose name is not a plain path segment, or that is not a regular file or a directory", s.Dir)
	}
}

// Generate returns what Flux's kustomize-controller puts in the kustomization it generates for root, a
// directory without a kustomization file: every .yaml and .yml file below root and every directory below it
// that holds a kustomization file (which is then not descended into), depth first and sorted by name within
// a directory, as Flux walks the tree. Other files are not returned. list is called once for root and once
// for each directory below it.
//
// It refuses rather than guesses: a directory deeper than MaxListDepth below root, a name that is not one
// plain segment (hidden names included), an entry of another type, a .sourceignore file or a name Flux
// excludes by default each stop the walk with a [*Stop]. Symbolic links and submodules are what the lister
// skips, so they are never followed.
func Generate(ctx context.Context, root string, list Lister) ([]Resource, error) {
	var out []Resource
	var walk func(dir string, depth int) error
	walk = func(dir string, depth int) error {
		entries, err := list(ctx, dir)
		if err != nil {
			return err
		}
		entries = slices.SortedFunc(slices.Values(entries), func(a, b providers.DirEntry) int {
			return cmp.Compare(a.Name, b.Name)
		})
		own := false
		for _, e := range entries {
			switch {
			case e.Name == SourceIgnoreFile:
				return &Stop{Kind: StopSourceIgnore, Dir: dir}
			case fluxIgnored[e.Name]:
				return &Stop{Kind: StopFluxIgnored, Dir: dir}
			case !isSegment(e.Name) || (e.Type != providers.DirEntryFile && e.Type != providers.DirEntryDir):
				return &Stop{Kind: StopInvalidName, Dir: dir}
			}
			if e.Type == providers.DirEntryFile && slices.Contains(KustomizationFileNames, e.Name) {
				own = true
			}
		}
		if own && depth > 0 {
			out = append(out, Resource{Path: dir, Dir: true})
			return nil
		}
		for _, e := range entries {
			p := path.Join(dir, e.Name)
			if len(p) > MaxLen {
				return &Stop{Kind: StopInvalidName, Dir: dir}
			}
			switch {
			case e.Type == providers.DirEntryFile && IsManifest(e.Name):
				out = append(out, Resource{Path: p})
			case e.Type == providers.DirEntryDir:
				if depth+1 > MaxListDepth {
					return &Stop{Kind: StopTooDeep, Dir: root}
				}
				if err := walk(p, depth+1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(root, 0); err != nil {
		return nil, err
	}
	return out, nil
}

// isSegment reports whether name is one plain path segment as [Join] accepts it: no slash, not "." or "..",
// and starting with a letter, digit or underscore.
func isSegment(name string) bool {
	return !strings.Contains(name, "/") && segmentRE.MatchString(name) && !strings.Contains(name, "..")
}
