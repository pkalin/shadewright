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
	// its own budget, smaller than the mapping's read budget (sourcemap.MaxTotalBytes): a render holds about
	// 13 MiB per MiB of input, so 4 MiB is what fits in the executor's 128 MiB memory limit with room for the
	// rest of the process (design 8.2.2, #482). A larger build is refused, not killed.
	MaxBuildBytes = 4 << 20
	// MaxObjects is the most objects one render may return, the 1,000 objects of the supported repository
	// (design 8.2) rounded up.
	MaxObjects = 1024
	// MaxBuildDocuments bounds the objects kustomize reads before it renders, with repetition like
	// MaxBuildBytes: every visit of a directory adds the YAML documents of the resource and patch files it lists
	// and of its inline patches, a List counted as 1 plus its items ([documents]). The byte budget alone lets 4 MiB of
	// tiny objects through, about 54,000, which kustomize cannot build in 128 MiB; MaxObjects is checked only on
	// the render's output. The count is an upper bound of the objects the render returns (generators are
	// refused, so only overlays reaching a base more than once multiply objects, and each visit counts), and
	// patches and empty documents count too, so it is set above MaxObjects: the render's own check still
	// refuses a build of 1,025 to 4,096 objects, after kustomize built them (design 8.2.2, #488).
	MaxBuildDocuments = 4096
)

const (
	kindKustomization = "Kustomization"
	kindComponent     = "Component"
	apiKustomization  = "kustomize.config.k8s.io/v1beta1"
	apiComponent      = "kustomize.config.k8s.io/v1alpha1"
	keyKind           = "kind"
	keyResources      = "resources"
	keyComponents     = "components"
	keyPatches        = "patches"
)

// allowedKeys are the kustomization fields the renderer accepts. Each is a built-in transformer that reads
// nothing or only local files the walk validates. Everything else is refused: generators (configMapGenerator,
// secretGenerator), helmCharts and helmGlobals, generators, transformers and validators (plugins and KRM
// functions), openapi, crds, configurations, replacements, the deprecated bases, patchesStrategicMerge and
// patchesJson6902, buildMetadata and sortOptions. A patch may not use JSON patch copy operations ([refuseCopy]).
var allowedKeys = map[string]bool{
	"apiVersion": true, keyKind: true, "metadata": true,
	keyResources: true, keyComponents: true, keyPatches: true,
	keyNamespace: true, "namePrefix": true, "nameSuffix": true,
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
	buildDocs     int
	// docs caches the documents of each file read ([documents]).
	docs map[string]int
	// scans caches the scan of each file read ([scanOf]); growth sums what each visit's patches and
	// transformers can add to the render ([visitGrowth]), which [MaxTransformBytes] bounds.
	scans  map[string]*scan
	growth cost
	// generate, when set, lists directories so that root, the path rendered, may have no kustomization file:
	// the walk then generates the one Flux would (Load only; Check finds it in the files it is given).
	generate repopath.Lister
	root     string
	// isDir says whether a resource path is a directory; see collectRefs.
	isDir func(string) bool
	// dirResources are the directories the generated kustomization lists, whatever their names end in.
	dirResources map[string]bool
}

type dirKind struct{ dir, kind string }

// refs are the references of one kustomization file, and the bytes one visit of it adds to the build.
type refs struct {
	dirs, components, files []string
	// patches are the patch files among files, checked by [refuseCopy].
	patches []string
	bytes   int
	// docs is what one visit adds to the documents of the build: those of its files and inline patches.
	docs int
	// coef is what its transformers copy per place; untargeted are its patches without a target, which
	// kustomize applies to the one object each names, and targeted those with one, which it applies to every
	// object the target selects.
	coef                 *coef
	untargeted, targeted []patchSource
	// ops is what one visit applies, once the patches are scanned ([walker.countShapes]).
	ops *ops
}

// patchSource is a patch: a file (path) or inline text; kind is the kind its target names, when that is a
// plain name, and kindChange is set when its options allow it to change the kind of an object.
type patchSource struct {
	path, inline, kind string
	kindChange         bool
}

func newWalker(read reader) *walker {
	w := &walker{
		read: read, files: map[string][]byte{}, resources: map[string]bool{},
		parsed: map[dirKind]*refs{}, ignoreChecked: map[string]bool{}, dirResources: map[string]bool{},
		docs: map[string]int{}, scans: map[string]*scan{},
	}
	w.isDir = func(p string) bool { return w.dirResources[p] }
	return w
}

// walk visits dir, which must hold a kustomization file of kind want, and everything it refers to. Every
// visit counts against MaxVisits and MaxBuildBytes, also when the directory was read before.
func (w *walker) walk(ctx context.Context, dir, want string, depth int) error {
	_, _, err := w.visit(ctx, dir, want, depth)
	return err
}

// visit is walk for one directory. It returns the shape of the objects the visit builds and, for a Component,
// the patches and transformers it applies to those of the kustomization that lists it; a Kustomization applies
// its own and its components' at once, and adds what they can add to the render to w.growth ([visitGrowth]).
func (w *walker) visit(ctx context.Context, dir, want string, depth int) (*shape, *ops, error) {
	if depth > MaxDepth {
		return nil, nil, refuse(ReasonTooLarge, fmt.Sprintf("kustomization directories are nested deeper than %d", MaxDepth))
	}
	if w.visits++; w.visits > MaxVisits {
		return nil, nil, refuse(ReasonTooLarge, fmt.Sprintf("the build visits directories more than %d times", MaxVisits))
	}
	key := dirKind{dir, want}
	r, ok := w.parsed[key]
	if !ok {
		var err error
		if r, err = w.parse(ctx, dir, want); err != nil {
			return nil, nil, err
		}
		for _, f := range r.files {
			if err := w.file(ctx, f); err != nil {
				return nil, nil, err
			}
			r.bytes += len(w.files[f])
			d, ok := w.docs[f]
			if !ok {
				var err error
				if d, err = documents(f, w.files[f]); err != nil {
					return nil, nil, err
				}
				w.docs[f] = d
			}
			r.docs = min(r.docs+d, MaxBuildDocuments+1)
		}
		for _, f := range r.patches {
			if err := refuseCopy(f, w.files[f]); err != nil {
				return nil, nil, err
			}
		}
		w.parsed[key] = r
	}
	if w.buildBytes += r.bytes; w.buildBytes > MaxBuildBytes {
		return nil, nil, refuse(ReasonTooLarge, fmt.Sprintf("the build processes more than %d bytes", MaxBuildBytes))
	}
	if w.buildDocs += r.docs; w.buildDocs > MaxBuildDocuments {
		return nil, nil, refuse(ReasonTooLarge, fmt.Sprintf("the build reads more than %d YAML documents", MaxBuildDocuments))
	}
	// Only now: documents stops reading a file at its budget, so a file over it was not checked and is never
	// decoded again.
	if r.ops == nil {
		if err := w.countShapes(r); err != nil {
			return nil, nil, err
		}
	}
	s := &shape{}
	for _, f := range r.files[:len(r.files)-len(r.patches)] {
		s.add(&w.scans[f].shape, 1)
	}
	o := newOps()
	o.add(r.ops)
	// A directory below dir adds one level per directory passed, as in the source mapping, so a base found
	// deep below a path without a kustomization file is bounded like a chain of kustomizations.
	for _, d := range r.dirs {
		sub, _, err := w.visit(ctx, d, kindKustomization, depth+repopath.NestDepth(dir, d))
		if err != nil {
			return nil, nil, err
		}
		s.add(sub, 1)
	}
	for _, d := range r.components {
		sub, comp, err := w.visit(ctx, d, kindComponent, depth+repopath.NestDepth(dir, d))
		if err != nil {
			return nil, nil, err
		}
		s.add(sub, 1)
		o.add(comp)
	}
	if want == kindComponent {
		return s, o, nil
	}
	w.growth = w.growth.plus(visitGrowth(s, o))
	return s, nil, nil
}

// countShapes scans, on the first visit of r, its files and inline patches, and sums what its patches and
// transformers apply into r.ops. Every file and inline patch was checked by [documents] in full before (the
// build is within MaxBuildDocuments), so it is UTF-8 YAML without anchors, aliases or merge keys.
func (w *walker) countShapes(r *refs) error {
	for _, f := range r.files {
		if _, ok := w.scans[f]; ok {
			continue
		}
		s, err := scanOf(f, w.files[f], w.docs[f])
		if err != nil {
			return err
		}
		w.scans[f] = s
	}
	o := newOps()
	o.coef.add(r.coef)
	for i, list := range [][]patchSource{r.untargeted, r.targeted} {
		for _, p := range list {
			s := w.scans[p.path]
			if p.path == "" {
				var err error
				if s, err = scanOf("an inline patch", []byte(p.inline), 0); err != nil {
					return err
				}
			}
			// A patch adds no object.
			sh := s.shape
			sh.docs, sh.kinds = 0, nil
			if i == 0 {
				o.untargeted.add(&sh, 1)
			} else {
				o.targeted = append(o.targeted, targetedPatch{shape: sh, cost: cost{s.bytes, s.nodes}, kind: p.kind})
			}
			o.rename = satAdd(o.rename, s.rename)
			o.moves = o.moves || s.moves
			o.kindChanges = o.kindChanges || s.kindChanges || p.kindChange
		}
	}
	r.ops = o
	return nil
}

// checkGrowth refuses a build whose patches and transformers can add more than [MaxTransformBytes] to its
// render ([visitGrowth]). The walk calls it once the whole tree is counted, so every visit is in the sum.
func (w *walker) checkGrowth() error {
	if w.growth.total() > MaxTransformBytes {
		return refuse(ReasonTooLarge, fmt.Sprintf("the patches and transformers of the build can add more than %d "+
			"bytes to the render, counting %d for each YAML node", MaxTransformBytes, NodeBytes))
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
	// The YAML parser below replaces aliases by copies, so what documents refuses (anchors, aliases, merge keys,
	// input that is not UTF-8) is refused here too, before it runs. documents stops reading at its budget, so a
	// file over it is refused as well: the rest of it was not checked.
	if d, err := documents(name, data); err != nil {
		return nil, err
	} else if d > MaxBuildDocuments {
		return nil, refuse(ReasonTooLarge, fmt.Sprintf("%s holds more than %d YAML documents", name, MaxBuildDocuments))
	}
	var k map[string]any
	if err := yaml.UnmarshalStrict(data, &k); err != nil {
		return nil, refuse(ReasonInvalidYAML, name+" is not a valid kustomization file")
	}
	if err := checkHeader(name, k, want); err != nil {
		return nil, err
	}
	r, err := collectRefs(name, dir, k, w.resources, w.isDir)
	if err != nil {
		return nil, err
	}
	r.bytes = len(data)
	r.coef = transformCoef(k)
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
		case repopath.StopSourceIgnore:
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
		if r.Dir {
			w.dirResources[r.Path] = true
		}
		rel := r.Path
		if dir != "." {
			rel = strings.TrimPrefix(r.Path, dir+"/")
		}
		refs = append(refs, rel)
	}
	// Marshalled, not written as text, so a name such as "true" or "1" stays a string.
	data, err := yaml.Marshal(map[string]any{"apiVersion": apiKustomization, keyKind: kindKustomization, keyResources: refs})
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
	kind, hasKind := k[keyKind]
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
// isDir, when it holds for a resource, makes it a directory even though its name ends in .yaml or .yml, as
// kustomize decides by what is on disk.
func collectRefs(name, dir string, k map[string]any, resources map[string]bool, isDir func(string) bool) (*refs, error) {
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
		if repopath.IsManifest(p) && !isDir(p) {
			r.files = append(r.files, p)
			resources[p] = true
		} else {
			r.dirs = append(r.dirs, p)
		}
	}
	if r.components, err = list(keyComponents); err != nil {
		return nil, err
	}
	p, err := patchFiles(name, dir, k[keyPatches], bad)
	if err != nil {
		return nil, err
	}
	r.files = append(r.files, p.files...)
	r.patches = p.files
	r.docs = p.docs
	r.untargeted = p.untargeted
	r.targeted = p.targeted
	return r, nil
}

// targetKind returns the kind a patch target names when it is a plain name (letters and digits), which
// kustomize matches as an anchored regular expression, so only objects of that kind; "" otherwise.
func targetKind(target any) string {
	m, _ := target.(map[string]any)
	k, _ := m["kind"].(string)
	if k == "" || strings.Trim(k, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789") != "" {
		return ""
	}
	return k
}

// patchList is what the patches list of a kustomization file reads: its local files, the documents of its
// inline patches, and the patches without and with a target.
type patchList struct {
	files                []string
	docs                 int
	untargeted, targeted []patchSource
}

// patchFiles validates the patches list and returns the local files it reads, the documents of its inline
// patches ([documents]), which kustomize reads like a patch file, the patches without a target, and those
// with a target, which kustomize applies to every object the target selects. An inline patch with a JSON patch copy
// operation is refused ([refuseCopy]); the walk checks patch files the same way.
func patchFiles(name, dir string, raw any, bad error) (patchList, error) {
	var l patchList
	if raw == nil {
		return l, nil
	}
	items, ok := raw.([]any)
	if !ok {
		return l, refuse(ReasonUnsupportedKustomization, "patches in "+name+" is not a list")
	}
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			return l, refuse(ReasonUnsupportedKustomization, "a patch in "+name+" is not a map")
		}
		for key := range m {
			if !patchKeys[key] {
				return l, refuse(ReasonUnsupportedKustomization, "a patch in "+name+" uses an unsupported field")
			}
		}
		// Any target, however narrow it looks, counts as selecting every object: its fields are regular
		// expressions and label or annotation selectors, which the walk does not evaluate.
		target, targeted := m["target"]
		kind := targetKind(target)
		// Any value of allowKindChange counts: kustomize refuses a malformed one.
		opts, _ := m["options"].(map[string]any)
		_, kindChange := opts["allowKindChange"]
		if raw, ok := m["path"]; ok {
			s, isString := raw.(string)
			p, valid := repopath.Join(dir, s)
			if !isString || !valid {
				return l, bad
			}
			l.files = append(l.files, p)
			if targeted {
				l.targeted = append(l.targeted, patchSource{path: p, kind: kind, kindChange: kindChange})
			} else {
				l.untargeted = append(l.untargeted, patchSource{path: p, kindChange: kindChange})
			}
		}
		if s, ok := m["patch"].(string); ok {
			what := "an inline patch in " + name
			d, err := documents(what, []byte(s))
			if err != nil {
				return l, err
			}
			if err := refuseCopy(what, []byte(s)); err != nil {
				return l, err
			}
			l.docs = min(l.docs+d, MaxBuildDocuments+1)
			if targeted {
				l.targeted = append(l.targeted, patchSource{inline: s, kind: kind, kindChange: kindChange})
			} else {
				l.untargeted = append(l.untargeted, patchSource{inline: s, kindChange: kindChange})
			}
		}
	}
	return l, nil
}
