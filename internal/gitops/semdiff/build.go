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
	"maps"
	"path"
	"regexp"
	"slices"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/pkalin/shadewright/internal/gitops/sourcemap"
)

// Limits on a build, so a hostile repository cannot make the check read or render without bound.
const (
	// MaxFileBytes is the largest file accepted, as for files read from Git.
	MaxFileBytes = sourcemap.MaxFileBytes
	// MaxFiles is how many files a build may hold, kustomization files included.
	MaxFiles = sourcemap.MaxFiles
	// MaxReads is how many reads [Load] makes in all, kustomization file lookups included.
	MaxReads = sourcemap.MaxReads
	// MaxDepth is how many nested kustomization directories are followed.
	MaxDepth = sourcemap.MaxDepth
	// MaxVisits is how many times directories are visited in all, counting a directory once for every
	// kustomization that lists it, so bases reused through many overlays cannot multiply the output.
	MaxVisits = 256
	// maxRefLen is the longest path accepted, the MaxLength of GitFilePath.
	maxRefLen = 512
)

const (
	kindKustomization = "Kustomization"
	kindComponent     = "Component"
	apiKustomization  = "kustomize.config.k8s.io/v1beta1"
	apiComponent      = "kustomize.config.k8s.io/v1alpha1"
	keyResources      = "resources"
	keyComponents     = "components"
	keyPatches        = "patches"
)

// kustomizationFileNames are the names kustomize looks for in a directory.
var kustomizationFileNames = []string{"kustomization.yaml", "kustomization.yml", kindKustomization}

// allowedKeys are the kustomization fields the renderer accepts. Each is a built-in transformer that reads
// nothing or only local files the walk validates. Everything else is refused: generators (configMapGenerator,
// secretGenerator), helmCharts and helmGlobals, generators, transformers and validators (plugins and KRM
// functions), openapi, crds, configurations, replacements, the deprecated bases, patchesStrategicMerge and
// patchesJson6902, buildMetadata and sortOptions.
var allowedKeys = map[string]bool{
	"apiVersion": true, "kind": true, "metadata": true,
	keyResources: true, keyComponents: true, keyPatches: true,
	"namespace": true, "namePrefix": true, "nameSuffix": true,
	"labels": true, "commonLabels": true, "commonAnnotations": true,
	"images": true, "replicas": true,
}

// patchKeys are the fields a patches entry may use. path is a local file; patch is inline.
var patchKeys = map[string]bool{"path": true, "patch": true, "target": true, "options": true}

// segmentRE is one path segment that is neither "." nor "..": it starts with a letter, digit or underscore
// and holds no ':', '@', '?' or '/', so it can never form a URL scheme, an SCP-style Git address or a query.
var segmentRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)

// keyRE is a kustomization field name that is safe to quote in a message.
var keyRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]{0,63}$`)

// cleanPath checks that p is a clean relative path inside the repository, made of plain segments, and
// returns it with "." for the root.
func cleanPath(p string) (string, bool) {
	if p == "" || p == "." || p == "./" {
		return ".", true
	}
	if len(p) > maxRefLen || strings.HasPrefix(p, "/") {
		return "", false
	}
	c := path.Clean(p)
	for seg := range strings.SplitSeq(c, "/") {
		if !segmentRE.MatchString(seg) || strings.Contains(seg, "..") {
			return "", false
		}
	}
	return c, true
}

// joinRef resolves ref, a reference from a kustomization file in dir, to a clean repository path. It refuses
// what kustomize could read as remote (a URL, an SCP-style address, a github.com path, anything with ':',
// '@' or '?'), absolute paths, segments kustomize calls ambiguous ("a..b"), and paths that leave the
// repository.
func joinRef(dir, ref string) (string, bool) {
	if ref == "" || len(ref) > maxRefLen || strings.HasPrefix(ref, "/") {
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
			// kustomize reads github.com/<org>/<repo> as a remote base even without a scheme.
			return "", false
		}
		first = false
	}
	p := path.Join(dir, ref)
	if p == ".." || strings.HasPrefix(p, "../") || len(p) > maxRefLen {
		return "", false
	}
	return p, true
}

func isManifestFile(p string) bool {
	return strings.HasSuffix(p, ".yaml") || strings.HasSuffix(p, ".yml")
}

func isKustomizationFile(p string) bool {
	return slices.Contains(kustomizationFileNames, path.Base(p))
}

// reader returns the content of a repository file; found is false when it does not exist.
type reader func(ctx context.Context, p string) (data []byte, found bool, err error)

// walker follows a kustomization tree from its root, validates every kustomization file against the
// allow-list and records the files the build reads.
type walker struct {
	read reader
	// files holds every file the build reads, by repository path.
	files map[string][]byte
	// resources holds the manifest files listed under resources.
	resources map[string]bool
	// parsed caches the references of each directory already read.
	parsed map[string]*refs
	visits int
}

// refs are the references of one kustomization file.
type refs struct {
	dirs, components, files []string
}

func newWalker(read reader) *walker {
	return &walker{read: read, files: map[string][]byte{}, resources: map[string]bool{}, parsed: map[string]*refs{}}
}

// walk visits dir, which must hold a kustomization file of kind want, and everything it refers to.
func (w *walker) walk(ctx context.Context, dir, want string, depth int) error {
	if depth > MaxDepth {
		return refuse(ReasonTooLarge, fmt.Sprintf("kustomization directories are nested deeper than %d", MaxDepth))
	}
	if w.visits++; w.visits > MaxVisits {
		return refuse(ReasonTooLarge, fmt.Sprintf("the build visits directories more than %d times", MaxVisits))
	}
	r, ok := w.parsed[dir]
	if !ok {
		var err error
		if r, err = w.parse(ctx, dir, want); err != nil {
			return err
		}
		w.parsed[dir] = r
	}
	for _, f := range r.files {
		if err := w.file(ctx, f); err != nil {
			return err
		}
	}
	for _, d := range r.dirs {
		if err := w.walk(ctx, d, kindKustomization, depth+1); err != nil {
			return err
		}
	}
	for _, d := range r.components {
		if err := w.walk(ctx, d, kindComponent, depth+1); err != nil {
			return err
		}
	}
	return nil
}

// file records a file the build reads. It must exist.
func (w *walker) file(ctx context.Context, p string) error {
	if _, ok := w.files[p]; ok {
		return nil
	}
	data, found, err := w.get(ctx, p)
	if err != nil {
		return err
	}
	if !found {
		return refuse(ReasonFileMissing, p+" is listed in the build but does not exist")
	}
	w.files[p] = data
	return nil
}

func (w *walker) get(ctx context.Context, p string) ([]byte, bool, error) {
	data, found, err := w.read(ctx, p)
	if err != nil || !found {
		return nil, found, err
	}
	if len(data) > MaxFileBytes {
		return nil, false, refuse(ReasonTooLarge, fmt.Sprintf("%s is larger than %d bytes", p, MaxFileBytes))
	}
	if len(w.files) >= MaxFiles {
		return nil, false, refuse(ReasonTooLarge, fmt.Sprintf("the build reads more than %d files", MaxFiles))
	}
	return data, true, nil
}

// parse reads the one kustomization file in dir and returns its validated references.
func (w *walker) parse(ctx context.Context, dir, want string) (*refs, error) {
	var name string
	var data []byte
	for _, n := range kustomizationFileNames {
		p := path.Join(dir, n)
		d, found, err := w.get(ctx, p)
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		if name != "" {
			return nil, refuse(ReasonFileMissing, "directory "+dir+" has more than one kustomization file")
		}
		name, data = p, d
	}
	if name == "" {
		return nil, refuse(ReasonFileMissing, "directory "+dir+" has no kustomization file")
	}
	w.files[name] = data
	var k map[string]any
	if err := yaml.UnmarshalStrict(data, &k); err != nil {
		return nil, refuse(ReasonInvalidYAML, name+" is not a valid kustomization file")
	}
	if err := checkHeader(name, k, want); err != nil {
		return nil, err
	}
	return collectRefs(name, dir, k, w.resources)
}

// checkHeader refuses fields outside the allow-list and a kind or apiVersion other than want's.
func checkHeader(name string, k map[string]any, want string) error {
	for _, key := range slices.Sorted(maps.Keys(k)) {
		if allowedKeys[key] {
			continue
		}
		what := "an unsupported field"
		if keyRE.MatchString(key) {
			what = fmt.Sprintf("%q", key)
		}
		return refuse(ReasonUnsupportedKustomization, name+" uses "+what)
	}
	kind, hasKind := k["kind"]
	api, hasAPI := k["apiVersion"]
	wantAPI := apiKustomization
	if want == kindComponent {
		wantAPI = apiComponent
	}
	// A Kustomization may leave out kind and apiVersion; a Component must name both.
	if (hasKind || want == kindComponent) && kind != want {
		return refuse(ReasonUnsupportedKustomization, name+" is not a "+want)
	}
	if (hasAPI || want == kindComponent) && api != wantAPI {
		return refuse(ReasonUnsupportedKustomization, name+" has an unsupported apiVersion")
	}
	return nil
}

// collectRefs validates the references of a kustomization file in dir.
func collectRefs(name, dir string, k map[string]any, resources map[string]bool) (*refs, error) {
	bad := refuse(ReasonInvalidReference, name+" refers to something that is not a relative path inside the repository")
	r := &refs{}
	list := func(key string) ([]string, error) {
		raw, present := k[key]
		items, ok := raw.([]any)
		if present && raw != nil && !ok {
			return nil, refuse(ReasonUnsupportedKustomization, key+" in "+name+" is not a list")
		}
		out := make([]string, 0, len(items))
		for _, it := range items {
			s, isString := it.(string)
			p, valid := joinRef(dir, s)
			if !isString || !valid {
				return nil, bad
			}
			out = append(out, p)
		}
		return out, nil
	}
	res, err := list(keyResources)
	if err != nil {
		return nil, err
	}
	for _, p := range res {
		if isManifestFile(p) {
			r.files = append(r.files, p)
			resources[p] = true
		} else {
			r.dirs = append(r.dirs, p)
		}
	}
	if r.components, err = list(keyComponents); err != nil {
		return nil, err
	}
	patches, err := patchFiles(name, dir, k[keyPatches], bad)
	if err != nil {
		return nil, err
	}
	r.files = append(r.files, patches...)
	return r, nil
}

// patchFiles validates the patches list and returns the local files it reads.
func patchFiles(name, dir string, raw any, bad error) ([]string, error) {
	if raw == nil {
		return nil, nil
	}
	items, ok := raw.([]any)
	if !ok {
		return nil, refuse(ReasonUnsupportedKustomization, "patches in "+name+" is not a list")
	}
	var files []string
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			return nil, refuse(ReasonUnsupportedKustomization, "a patch in "+name+" is not a map")
		}
		for key := range m {
			if !patchKeys[key] {
				return nil, refuse(ReasonUnsupportedKustomization, "a patch in "+name+" uses an unsupported field")
			}
		}
		if raw, ok := m["path"]; ok {
			s, isString := raw.(string)
			p, valid := joinRef(dir, s)
			if !isString || !valid {
				return nil, bad
			}
			files = append(files, p)
		}
	}
	return files, nil
}
