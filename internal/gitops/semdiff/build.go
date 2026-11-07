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
	"errors"
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/pkalin/shadewright/internal/gitops/repopath"
	"github.com/pkalin/shadewright/internal/gitops/sourcemap"
	"github.com/pkalin/shadewright/internal/providers"
)

// Limits on a build, so a hostile repository cannot make the check read or render without bound.
const (
	// MaxFileBytes is the largest file accepted, as for files read from Git.
	MaxFileBytes = sourcemap.MaxFileBytes
	// MaxFiles is how many files a build may hold, kustomization files included.
	MaxFiles = sourcemap.MaxFiles
	// MaxReads is how many reads [Load] makes in all, kustomization and .sourceignore lookups included.
	MaxReads = sourcemap.MaxReads
	// MaxDepth is how many nested kustomization directories are followed.
	MaxDepth = sourcemap.MaxDepth
	// MaxVisits is how many times directories are visited in all, with repetition: a directory counts once
	// for every reference that reaches it, so a diamond of overlays over one base counts every path through
	// it, as kustomize builds it.
	MaxVisits = 256
	// MaxBuildBytes bounds the input kustomize processes, with repetition: every visit of a directory adds
	// the size of its kustomization file and of the resource and patch files it lists. A large base reached
	// through many overlays is rendered once per path, so this bounds the work and memory of a render. It is
	// the mapping's read budget, so one literal sets both.
	MaxBuildBytes = sourcemap.MaxTotalBytes
	// MaxObjects is the most objects one render may return.
	MaxObjects = 4096
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

// cleanPath checks that p is a relative path inside the repository and returns it cleaned, with "." for the
// root (an empty spec.path means the root, as in Flux).
func cleanPath(p string) (string, bool) {
	if p == "" {
		return ".", true
	}
	return repopath.Join(".", p)
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
	// parsed caches the references of each directory already read, per kind it was reached as, so a
	// directory reached as a Kustomization and as a Component is checked for both.
	parsed map[dirKind]*refs
	// ignoreChecked holds the directories already checked for a .sourceignore file.
	ignoreChecked map[string]bool
	visits        int
	buildBytes    int
	// generate, when set, lists directories so that root, the path rendered, may have no kustomization file:
	// the walk then generates the one Flux would (Load only; Check finds it in the files it is given).
	generate repopath.Lister
	root     string
}

type dirKind struct{ dir, kind string }

// refs are the references of one kustomization file, and the bytes one visit of it adds to the build.
type refs struct {
	dirs, components, files []string
	bytes                   int
}

func newWalker(read reader) *walker {
	return &walker{
		read: read, files: map[string][]byte{}, resources: map[string]bool{},
		parsed: map[dirKind]*refs{}, ignoreChecked: map[string]bool{},
	}
}

// walk visits dir, which must hold a kustomization file of kind want, and everything it refers to. Every
// visit counts against MaxVisits and MaxBuildBytes, also when the directory was read before.
func (w *walker) walk(ctx context.Context, dir, want string, depth int) error {
	if depth > MaxDepth {
		return refuse(ReasonTooLarge, fmt.Sprintf("kustomization directories are nested deeper than %d", MaxDepth))
	}
	if w.visits++; w.visits > MaxVisits {
		return refuse(ReasonTooLarge, fmt.Sprintf("the build visits directories more than %d times", MaxVisits))
	}
	key := dirKind{dir, want}
	r, ok := w.parsed[key]
	if !ok {
		var err error
		if r, err = w.parse(ctx, dir, want); err != nil {
			return err
		}
		for _, f := range r.files {
			if err := w.file(ctx, f); err != nil {
				return err
			}
			r.bytes += len(w.files[f])
		}
		w.parsed[key] = r
	}
	if w.buildBytes += r.bytes; w.buildBytes > MaxBuildBytes {
		return refuse(ReasonTooLarge, fmt.Sprintf("the build processes more than %d bytes", MaxBuildBytes))
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

// file records a file the build reads. It must exist, and no .sourceignore may apply to it.
func (w *walker) file(ctx context.Context, p string) error {
	if _, ok := w.files[p]; ok {
		return nil
	}
	if err := w.checkIgnore(ctx, path.Dir(p)); err != nil {
		return err
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

// checkIgnore refuses when dir or any directory above it holds a .sourceignore file, as the source mapping
// does: Flux would drop the files it matches from what it applies, so a render of the full tree could
// differ from what Flux builds. The rules are not interpreted.
func (w *walker) checkIgnore(ctx context.Context, dir string) error {
	for d := dir; !w.ignoreChecked[d]; d = path.Dir(d) {
		w.ignoreChecked[d] = true
		p := path.Join(d, repopath.SourceIgnoreFile)
		_, found, err := w.get(ctx, p)
		if err != nil {
			return err
		}
		if found {
			return refuse(ReasonSourceIgnored, p+" exists, so Flux may leave files out of what it builds")
		}
		if d == "." {
			break
		}
	}
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
	if err := w.checkIgnore(ctx, dir); err != nil {
		return nil, err
	}
	var name string
	var data []byte
	for _, n := range repopath.KustomizationFileNames {
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
	if name == "" && w.generate != nil && dir == w.root && want == kindKustomization {
		var err error
		if name, data, err = w.generated(ctx, dir); err != nil {
			return nil, err
		}
	}
	if name == "" {
		return nil, w.noKustomization(ctx, dir)
	}
	w.files[name] = data
	var k map[string]any
	if err := yaml.UnmarshalStrict(data, &k); err != nil {
		return nil, refuse(ReasonInvalidYAML, name+" is not a valid kustomization file")
	}
	if err := checkHeader(name, k, want); err != nil {
		return nil, err
	}
	r, err := collectRefs(name, dir, k, w.resources)
	if err != nil {
		return nil, err
	}
	r.bytes = len(data)
	return r, nil
}

// generatedFile is the name Flux gives the kustomization it generates for a path without one.
const generatedFile = "kustomization.yaml"

// generated returns the kustomization file Flux's kustomize-controller generates for dir, which has none: plain
// resources only, listing every manifest below dir and every directory below it with its own kustomization
// file ([repopath.Generate]). It is added to the build's files under generatedFile, so [Check] renders what Flux
// renders without listing anything itself.
func (w *walker) generated(ctx context.Context, dir string) (string, []byte, error) {
	var listed []string
	resources, err := repopath.Generate(ctx, dir, func(ctx context.Context, d string) ([]providers.DirEntry, error) {
		listed = append(listed, d)
		return w.generate(ctx, d)
	})
	if s, ok := errors.AsType[*repopath.Stop](err); ok {
		reason := ReasonInvalidReference
		switch s.Kind {
		case repopath.StopTooDeep:
			reason = ReasonTooLarge
		case repopath.StopSourceIgnore, repopath.StopFluxIgnored:
			reason = ReasonSourceIgnored
		}
		return "", nil, refuse(reason, s.Error())
	}
	if err != nil {
		return "", nil, err
	}
	// Generate stops at a .sourceignore file in any directory it listed, so none needs to be read again.
	for _, d := range listed {
		w.ignoreChecked[d] = true
	}
	refs := make([]string, 0, len(resources))
	for _, r := range resources {
		rel := r.Path
		if dir != "." {
			rel = strings.TrimPrefix(r.Path, dir+"/")
		}
		refs = append(refs, rel)
	}
	// Marshalled, not written as text, so a name such as "true" or "1" stays a string.
	data, err := yaml.Marshal(map[string]any{"apiVersion": apiKustomization, "kind": kindKustomization, keyResources: refs})
	if err != nil {
		return "", nil, refuse(ReasonInvalidInput, "the kustomization Flux generates for "+dir+" cannot be written")
	}
	name := path.Join(dir, generatedFile)
	if len(w.files) >= MaxFiles {
		return "", nil, refuse(ReasonTooLarge, fmt.Sprintf("the build reads more than %d files", MaxFiles))
	}
	return name, data, nil
}

// noKustomization explains why dir has no kustomization file. A resource that does not end in .yaml or .yml
// is followed as a directory; when it is in fact a file (deploy.json, say), the reason says so.
func (w *walker) noKustomization(ctx context.Context, dir string) error {
	if _, found, err := w.read(ctx, dir); err != nil {
		return err
	} else if found {
		return refuse(ReasonUnsupportedKustomization,
			dir+" is listed as a resource but is a file without a .yaml or .yml extension; only YAML files are supported")
	}
	return refuse(ReasonFileMissing, "directory "+dir+" has no kustomization file")
}

// checkHeader refuses fields outside the allow-list and a kind or apiVersion other than want's.
func checkHeader(name string, k map[string]any, want string) error {
	for _, key := range slices.Sorted(maps.Keys(k)) {
		if allowedKeys[key] {
			continue
		}
		what := "an unsupported field"
		if repopath.SafeKey(key) {
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
			p, valid := repopath.Join(dir, s)
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
		if repopath.IsManifest(p) {
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
			p, valid := repopath.Join(dir, s)
			if !isString || !valid {
				return nil, bad
			}
			files = append(files, p)
		}
	}
	return files, nil
}
