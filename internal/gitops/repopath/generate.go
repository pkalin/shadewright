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
	"context"
	"fmt"
	"path"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/pkalin/shadewright/internal/providers"
)

// MaxListDepth is how far below a Flux Kustomization's path [Generate] lists directories: the path itself is
// depth 0, so a directory at depth MaxListDepth is still listed and one below it stops the walk.
const MaxListDepth = 5

// ListEntryBytes is what one entry of a directory listing costs against a byte budget: a fixed cost, about
// the size of one entry in a GitHub tree answer, since GitProvider.ListDir does not report what it downloaded.
// A listing of providers.MaxDirEntries entries so costs 125 KiB.
const ListEntryBytes = 128

// The default exclusions of Flux's source-controller, copied from fluxcd/pkg sourceignore/sourceignore.go
// (tag sourceignore/v0.9.0), constants ExcludeVCS, ExcludeExt, ExcludeCI and ExcludeExtra. Flux leaves what
// they match out of every GitRepository artifact that sets no spec.ignore (the source mapping refuses one
// that does). Every pattern is a gitignore pattern without an inner slash, so it matches a name at any depth;
// a trailing slash matches directories only. Keep them equal to upstream when Flux changes them.
const (
	fluxExcludeVCS   = ".git/,.gitignore,.gitmodules,.gitattributes"
	fluxExcludeExt   = "*.jpg,*.jpeg,*.gif,*.png,*.wmv,*.flv,*.tar.gz,*.zip"
	fluxExcludeCI    = ".github/,.circleci/,.travis.yml,.gitlab-ci.yml,appveyor.yml,.drone.yml,cloudbuild.yaml,codeship-services.yml,codeship-steps.yml"
	fluxExcludeExtra = "**/.goreleaser.yml,**/.sops.yaml,**/.flux.yaml"
)

// fluxExcludes is the parsed form of the default exclusions.
var fluxExcludes = parseExcludes(strings.Join([]string{fluxExcludeVCS, fluxExcludeExt, fluxExcludeCI, fluxExcludeExtra}, ","))

type excludes struct {
	names, dirs map[string]bool
	suffixes    []string
}

// parseExcludes reads the default patterns, which take only the forms "name", "name/", "**/name" and "*.ext".
func parseExcludes(list string) excludes {
	x := excludes{names: map[string]bool{}, dirs: map[string]bool{}}
	for p := range strings.SplitSeq(list, ",") {
		p = strings.TrimPrefix(p, "**/")
		switch {
		case strings.HasSuffix(p, "/"):
			x.dirs[strings.TrimSuffix(p, "/")] = true
		case strings.HasPrefix(p, "*"):
			x.suffixes = append(x.suffixes, strings.TrimPrefix(p, "*"))
		default:
			x.names[p] = true
		}
	}
	return x
}

// FluxExcludes reports whether Flux's default exclusions leave an entry with this name out of the artifact.
func FluxExcludes(name string, dir bool) bool {
	x := fluxExcludes
	return x.names[name] || (dir && x.dirs[name]) ||
		slices.ContainsFunc(x.suffixes, func(s string) bool { return strings.HasSuffix(name, s) })
}

// Resource is one entry of the kustomization Flux generates for a directory without one: a manifest file, or
// a directory that holds its own kustomization file and is built as a kustomize base.
type Resource struct {
	// Path is the repository path, checked with [Join].
	Path string
	// Dir is true for a directory with a kustomization file.
	Dir bool
	// Depth is how many directories below the root a Dir resource is, at least 1, so the caller bounds the
	// nesting of what it builds from there by the directories already passed.
	Depth int
}

// Lister lists one directory, as [providers.GitProvider.ListDir] does, sorted by name. It is where the caller
// counts the listing against its budget; an error it returns ends [Generate] and is returned as it is.
type Lister func(ctx context.Context, dir string) ([]providers.DirEntry, error)

// StopKind says why [Generate] stopped on what it listed.
type StopKind int

// Reasons for a [*Stop].
const (
	// StopTooDeep means a directory is nested more than MaxListDepth below the root.
	StopTooDeep StopKind = iota + 1
	// StopInvalidName means a name that would be in the build is not a plain path segment (see [Join]), a
	// directory Flux would descend into has a name that cannot be listed, or an entry is neither a regular file
	// nor a directory.
	StopInvalidName
	// StopSourceIgnore means a listed directory holds a .sourceignore file.
	StopSourceIgnore
)

// Stop is the error [Generate] returns when what it listed cannot be built as Flux would build it. Dir is the
// deepest directory on the way whose path is valid; no listed name that is not valid is kept.
type Stop struct {
	Kind StopKind
	Dir  string
}

// Error describes the stop without quoting any name that is not valid.
func (s *Stop) Error() string {
	switch s.Kind {
	case StopTooDeep:
		return fmt.Sprintf("directories below %s are nested deeper than %d", s.Dir, MaxListDepth)
	case StopSourceIgnore:
		return fmt.Sprintf("a %s file below %s exists, so Flux may leave files out of what it applies", SourceIgnoreFile, s.Dir)
	default:
		return fmt.Sprintf("directory %s holds a manifest or directory whose name is not a plain path segment, or an entry that is neither a regular file nor a directory", s.Dir)
	}
}

// Generate returns what Flux's kustomize-controller puts in the kustomization it generates for root, a
// directory without a kustomization file: every .yaml and .yml file below root and every directory below it
// that holds a kustomization file (which is then not descended into, nor are its entries checked), depth first
// and in name order within a directory, as Flux walks the tree. Entries Flux's default exclusions match are
// skipped, as they are not in the artifact, and so are files that are not manifests, hidden or not. Symbolic
// links and submodules are what the lister skips, so they are never followed. list is called once for root
// and once for each directory below it that is not excluded.
//
// It refuses rather than guesses: a directory deeper than MaxListDepth below root, a manifest or base whose
// path [Join] refuses (a hidden manifest included), a directory whose name cannot be listed, an entry of
// another type, or a .sourceignore file in any listed directory each stop the walk with a [*Stop].
func Generate(ctx context.Context, root string, list Lister) ([]Resource, error) {
	var out []Resource
	var walk func(dir, shown string, depth int) error
	walk = func(dir, shown string, depth int) error {
		entries, err := list(ctx, dir)
		if err != nil {
			return err
		}
		own := false
		for _, e := range entries {
			if e.Name == SourceIgnoreFile {
				return &Stop{Kind: StopSourceIgnore, Dir: shown}
			}
			if e.Type == providers.DirEntryFile && slices.Contains(KustomizationFileNames, e.Name) {
				own = true
			}
		}
		if own && depth > 0 {
			if _, ok := Join(".", dir); !ok {
				return &Stop{Kind: StopInvalidName, Dir: shown}
			}
			out = append(out, Resource{Path: dir, Dir: true, Depth: depth})
			return nil
		}
		for _, e := range entries {
			isDir := e.Type == providers.DirEntryDir
			if FluxExcludes(e.Name, isDir) {
				continue
			}
			p := path.Join(dir, e.Name)
			switch {
			case e.Type == providers.DirEntryFile && !IsManifest(e.Name):
				continue
			case e.Type == providers.DirEntryFile:
				valid, ok := Join(".", p)
				if !ok {
					return &Stop{Kind: StopInvalidName, Dir: shown}
				}
				out = append(out, Resource{Path: valid})
			case isDir:
				if !listable(e.Name) || len(p) > MaxLen {
					return &Stop{Kind: StopInvalidName, Dir: shown}
				}
				if depth+1 > MaxListDepth {
					return &Stop{Kind: StopTooDeep, Dir: root}
				}
				next := shown
				if valid, ok := Join(".", p); ok {
					next = valid
				}
				if err := walk(p, next, depth+1); err != nil {
					return err
				}
			default:
				return &Stop{Kind: StopInvalidName, Dir: shown}
			}
		}
		return nil
	}
	if err := walk(root, root, 0); err != nil {
		return nil, err
	}
	return out, nil
}

// listable reports whether a directory name can be listed even though [Join] may refuse it (a hidden
// directory, which Flux descends into): one segment, not "." or "..", not .git, valid UTF-8 without control
// characters or a backslash, as a GitProvider accepts in a path.
func listable(name string) bool {
	if name == "" || name == "." || name == ".." || strings.EqualFold(name, ".git") || !utf8.ValidString(name) {
		return false
	}
	return !strings.ContainsFunc(name, func(r rune) bool { return r == '/' || r == '\\' || unicode.IsControl(r) })
}

// NestDepth is how many levels of nesting a kustomization in dir that refers to the directory p adds: the
// number of directories from dir down to p when p is below dir, else 1. Counting the directories passed keeps
// a base found deep below a path without a kustomization file within the same depth bound as a chain of
// kustomizations.
func NestDepth(dir, p string) int {
	rel := p
	if dir != "." {
		var ok bool
		if rel, ok = strings.CutPrefix(p, dir+"/"); !ok {
			return 1
		}
	}
	if rel == "" || rel == ".." || strings.HasPrefix(rel, "../") {
		return 1
	}
	return strings.Count(rel, "/") + 1
}

// ListFault is how a walk treats the outcome of one directory listing.
type ListFault int

// Listing outcomes, the same for the source mapping and the semantic diff.
const (
	// ListOK means the listing can be used.
	ListOK ListFault = iota
	// ListNotADirectory means the host read the tree and the path is not a directory in it (permanent).
	ListNotADirectory
	// ListTooLarge means the directory holds more than providers.MaxDirEntries entries (permanent).
	ListTooLarge
	// ListInvalidEntry means the listing holds an entry no tree can hold (permanent).
	ListInvalidEntry
	// ListFailed means the listing failed in a way that may not hold on a retry: a plain not found (a commit
	// the host does not have yet, a failing host), a rate limit, a refused token, a cancelled context.
	ListFailed
)

// CheckListing classifies the outcome of a ListDir call. A plain not found is ListFailed, not
// ListNotADirectory: only providers.ErrPathNotInTree says the host read the tree.
func CheckListing(entries []providers.DirEntry, err error) ListFault {
	switch {
	case providers.IsPathNotInTree(err):
		return ListNotADirectory
	case providers.IsTooLarge(err), err == nil && len(entries) > providers.MaxDirEntries:
		return ListTooLarge
	case providers.IsInvalidEntry(err):
		return ListInvalidEntry
	case err != nil:
		return ListFailed
	}
	return ListOK
}
