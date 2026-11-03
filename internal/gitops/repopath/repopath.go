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
// Package repopath holds the path rules shared by the source mapping and the semantic diff check: which
// references from a kustomization file or a Flux Kustomization are plain relative paths inside the
// repository, and which file names kustomize reads. Paths come from untrusted Git content, so every rule
// refuses rather than guesses.
package repopath

import (
	"path"
	"regexp"
	"slices"
	"strings"
)

// MaxLen is the longest path accepted, the MaxLength of GitFilePath.
const MaxLen = 512

// KustomizationFileNames are the names kustomize looks for in a directory.
var KustomizationFileNames = []string{"kustomization.yaml", "kustomization.yml", "Kustomization"}

// SourceIgnoreFile is the file whose rules Flux applies to a GitRepository artifact.
const SourceIgnoreFile = ".sourceignore"

// segmentRE is one path segment that is neither "." nor "..": it starts with a letter, digit or underscore
// and holds no ':', '@', '?' or '/', so it can never form a URL scheme, an SCP-style Git address or a query.
var segmentRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)

// keyRE is a kustomization field name that is safe to quote in a message.
var keyRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]{0,63}$`)

// Join resolves ref, a relative path from a kustomization file or a Kustomization, against dir, a clean
// path inside the repository ("." for its root). It refuses absolute paths, segments that are not plain
// names, segments kustomize calls ambiguous ("a..b"), a first segment github.com (which kustomize reads as
// a remote base even without a scheme), and results outside the repository. Because no segment may hold
// ':', '@' or '?', no URL, SCP-style address or query can pass.
func Join(dir, ref string) (string, bool) {
	if ref == "" || len(ref) > MaxLen || strings.HasPrefix(ref, "/") {
		return "", false
	}
	first := true
	for seg := range strings.SplitSeq(ref, "/") {
		switch {
		case seg == "" || seg == "." || seg == "..":
			continue
		case !segmentRE.MatchString(seg) || strings.Contains(seg, ".."):
			return "", false
		case first && strings.EqualFold(seg, "github.com"):
			return "", false
		}
		first = false
	}
	p := path.Join(dir, ref)
	if p == ".." || strings.HasPrefix(p, "../") || len(p) > MaxLen {
		return "", false
	}
	return p, true
}

// IsManifest reports whether p names a YAML file by its extension.
func IsManifest(p string) bool {
	return strings.HasSuffix(p, ".yaml") || strings.HasSuffix(p, ".yml")
}

// IsKustomizationFile reports whether the base name of p is one kustomize reads as a kustomization.
func IsKustomizationFile(p string) bool {
	return slices.Contains(KustomizationFileNames, path.Base(p))
}

// SafeKey reports whether a kustomization field name is safe to quote in a message.
func SafeKey(key string) bool { return keyRE.MatchString(key) }
