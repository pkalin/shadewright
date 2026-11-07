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
package sourcemap

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"maps"
	"path"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/runtime/schema"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"

	"github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/gitops/repopath"
	"github.com/pkalin/shadewright/internal/providers"
)

// Limits on what the mapping reads from Git, so a hostile repository cannot make it read without bound.
const (
	// MaxDepth is how many nested kustomization directories are followed.
	MaxDepth = 8
	// MaxFiles is how many manifest files are read.
	MaxFiles = 256
	// MaxReads is how many ReadFile calls one mapping makes in all, kustomization file lookups included, so
	// a build cannot fan out into many directories.
	MaxReads = 512
	// MaxFileBytes is the largest file read. The provider refuses larger files before downloading them
	// (providers.ErrTooLarge); the mapping checks the size again in case an adapter does not.
	MaxFileBytes = providers.MaxFileBytes
	// MaxTotalBytes is how many bytes one mapping reads in all, kustomization and .sourceignore lookups and
	// cached reads included, so that many files near MaxFileBytes cannot add up without bound. Each read asks
	// the provider for at most what is left of it, so the budget bounds what is downloaded, cached and parsed,
	// and a build of exactly MaxTotalBytes resolves. The semantic diff takes its render budget from it
	// (semdiff.MaxBuildBytes).
	MaxTotalBytes = 16 << 20
	// MaxDocuments is how many YAML documents one mapping decodes in all manifest files, and [ReadField] in
	// its one file, ones holding only comments or blank lines included, so that a file of many small
	// documents cannot keep the decoder busy. A separator with nothing before it is skipped by the reader
	// and never decoded, so it is not counted.
	MaxDocuments = 4096
)

// kustomizationKeys are the only fields a kustomization file may use: anything else (patches, generators,
// components, transformers, namespace, name prefix, images, replicas, labels) changes what is built.
var kustomizationKeys = map[string]bool{"apiVersion": true, "kind": true, "metadata": true, "resources": true}

// walker collects the manifest files a kustomization directory builds, following its resources.
type walker struct {
	git    providers.GitProvider
	commit string
	// cache, when set, answers reads at commit already made through the settings entry whose digest is repo.
	cache *ReadCache
	repo  [sha256.Size]byte
	dirs  map[string]bool
	files []string
	seen  map[string]bool
	reads int
	// bytes and docs count what the mapping has read and decoded, against MaxTotalBytes and MaxDocuments.
	bytes int
	docs  int
	// ignoreChecked holds the directories already checked for a .sourceignore file.
	ignoreChecked map[string]bool
}

func newWalker(git providers.GitProvider, commit string, cache *ReadCache, repo [sha256.Size]byte) *walker {
	return &walker{git: git, commit: commit, cache: cache, repo: repo, dirs: map[string]bool{}, seen: map[string]bool{}, ignoreChecked: map[string]bool{}}
}

// walk adds the files built from dir. A directory reached twice is read once.
func (w *walker) walk(ctx context.Context, dir string, depth int) (*Result, error) {
	if depth > MaxDepth {
		r := needsIntervention(ReasonTooManyFiles, "kustomization directories are nested deeper than %d", MaxDepth)
		return &r, nil
	}
	if w.dirs[dir] {
		return nil, nil
	}
	w.dirs[dir] = true

	if stop, err := w.checkIgnore(ctx, dir); stop != nil || err != nil {
		return stop, err
	}
	file, stop, err := w.kustomizationFile(ctx, dir)
	if stop != nil || err != nil {
		return stop, err
	}
	var k map[string]any
	if err := yaml.UnmarshalStrict(file.Data, &k); err != nil {
		r := needsIntervention(ReasonFileNotReadable, "%s is not a valid kustomization file", file.Path)
		return &r, nil
	}
	for _, key := range slices.Sorted(maps.Keys(k)) {
		if kustomizationKeys[key] {
			continue
		}
		what := "an unsupported field"
		if repopath.SafeKey(key) {
			what = fmt.Sprintf("%q", key)
		}
		r := needsIntervention(ReasonUnsupportedKustomization,
			"%s uses %s; only plain resources are supported", file.Path, what)
		return &r, nil
	}
	if kind, ok := k["kind"]; ok && kind != kindKustomization {
		r := needsIntervention(ReasonUnsupportedKustomization, "%s is not a Kustomization", file.Path)
		return &r, nil
	}
	resources, ok := k["resources"].([]any)
	if _, present := k["resources"]; present && !ok {
		r := needsIntervention(ReasonFileNotReadable, "resources in %s is not a list", file.Path)
		return &r, nil
	}
	for _, res := range resources {
		ref, isString := res.(string)
		p, valid := repopath.Join(dir, ref)
		if !isString || !valid {
			r := needsIntervention(ReasonUnsupportedKustomization,
				"%s lists a resource that is not a relative path inside the repository", file.Path)
			return &r, nil
		}
		if !repopath.IsManifest(p) {
			if stop, err := w.walk(ctx, p, depth+1); stop != nil || err != nil {
				return stop, err
			}
			continue
		}
		if w.seen[p] {
			continue
		}
		if stop, err := w.checkIgnore(ctx, path.Dir(p)); stop != nil || err != nil {
			return stop, err
		}
		if len(w.files) >= MaxFiles {
			r := needsIntervention(ReasonTooManyFiles, "the build lists more than %d files", MaxFiles)
			return &r, nil
		}
		w.seen[p] = true
		w.files = append(w.files, p)
	}
	return nil, nil
}

// sourceIgnoreFile is the file whose rules Flux applies to a GitRepository artifact.
const sourceIgnoreFile = repopath.SourceIgnoreFile

// checkIgnore stops when dir or any directory above it, up to the repository root, holds a .sourceignore
// file. Flux would drop the files it matches from the artifact while this walker reads the full tree, so the
// rules are not interpreted: any such file fails closed. Each directory costs one read from the budget.
func (w *walker) checkIgnore(ctx context.Context, dir string) (*Result, error) {
	for d := dir; !w.ignoreChecked[d]; d = path.Dir(d) {
		w.ignoreChecked[d] = true
		p := path.Join(d, sourceIgnoreFile)
		_, stop, err := w.read(ctx, p)
		if err != nil {
			return nil, err
		}
		switch {
		case stop == nil:
			return new(needsIntervention(ReasonSourceOptions,
				"%s exists, so Flux may leave files out of what it applies", p)), nil
		case stop.Reason != reasonFileNotFound:
			return stop, nil
		}
		if d == "." {
			break
		}
	}
	return nil, nil
}

// kustomizationFile reads the one kustomization file in dir.
func (w *walker) kustomizationFile(ctx context.Context, dir string) (providers.File, *Result, error) {
	var found []providers.File
	for _, name := range repopath.KustomizationFileNames {
		f, stop, err := w.read(ctx, path.Join(dir, name))
		if stop != nil {
			if stop.Reason == reasonFileNotFound {
				continue
			}
			return providers.File{}, stop, nil
		}
		if err != nil {
			return providers.File{}, nil, err
		}
		found = append(found, f)
	}
	switch len(found) {
	case 0:
		r := needsIntervention(ReasonNoKustomizationFile,
			"directory %s has no kustomization file, so the files Flux builds from it cannot be listed", dir)
		return providers.File{}, &r, nil
	case 1:
		return found[0], nil, nil
	default:
		r := needsIntervention(ReasonUnsupportedKustomization, "directory %s has more than one kustomization file", dir)
		return providers.File{}, &r, nil
	}
}

// reasonFileNotFound is internal to the walker: a file that may legitimately be absent. It is never returned.
const reasonFileNotFound Reason = "fileNotFound"

// read returns a file at the walker's commit, or a stop result when it is missing or too large.
func (w *walker) read(ctx context.Context, p string) (providers.File, *Result, error) {
	if w.reads >= MaxReads {
		return providers.File{}, new(needsIntervention(ReasonTooManyFiles,
			"the build needs more than %d file reads", MaxReads)), nil
	}
	// A read from the cache counts too, so the cache never changes where the budget runs out.
	w.reads++
	// The provider, or the cache, refuses a file larger than what is left of the budget before downloading it,
	// so the mapping downloads, caches and parses at most MaxTotalBytes.
	limit := min(MaxFileBytes, MaxTotalBytes-w.bytes)
	f, err := w.cache.readFile(ctx, w.git, w.repo, w.commit, p, limit)
	if providers.IsNotFound(err) {
		// A missing file costs nothing, so it is never refused for the budget.
		r := needsIntervention(reasonFileNotFound, "%s does not exist", p)
		return providers.File{}, &r, nil
	}
	if err != nil && !providers.IsTooLarge(err) {
		return providers.File{}, nil, transient("read "+p, err)
	}
	if err != nil || len(f.Data) > limit {
		if limit < MaxFileBytes {
			// Over what is left of the budget. Whether it is also over MaxFileBytes is not known, nor asked:
			// a cold and a warm cache give this one reason alike.
			return providers.File{}, new(needsIntervention(ReasonInputTooLarge,
				"the build reads more than %d bytes in all; %s was not downloaded", MaxTotalBytes, p)), nil
		}
		r := needsIntervention(ReasonFileNotReadable, "%s is larger than %d bytes", p, MaxFileBytes)
		return providers.File{}, &r, nil
	}
	w.bytes += len(f.Data)
	return f, nil, nil
}

// declaration is the one document that declares the target.
type declaration struct {
	path string
	doc  map[string]any
}

// identity is what the mapping matches documents on.
type identity struct {
	gvk       schema.GroupVersionKind
	namespace string
	name      string
}

// String returns the identity as kind namespace/name, for messages.
func (id identity) String() string {
	return fmt.Sprintf("%s %s/%s", id.gvk.Kind, id.namespace, id.name)
}

// findDeclaration reads the files in order and returns the only document that declares id. A document with
// the same group, kind and name but another version, or with no namespace, could also be the target, so it
// makes the match ambiguous. It stops at the first sign of ambiguity: a second exact match or a near one. Only
// the exact match is kept; every other document is dropped once it has been matched, and the documents decoded
// count against MaxDocuments.
func (w *walker) findDeclaration(ctx context.Context, id identity) (declaration, *Result, error) {
	var found *declaration
	what := id.String()
	for _, p := range w.files {
		f, stop, err := w.read(ctx, p)
		if stop != nil {
			if stop.Reason == reasonFileNotFound {
				r := needsIntervention(ReasonFileNotReadable, "%s is listed in the build but does not exist", p)
				return declaration{}, &r, nil
			}
			return declaration{}, stop, nil
		}
		if err != nil {
			return declaration{}, nil, err
		}
		docs := newDocuments(f.Data, &w.docs)
		for {
			doc, err := docs.next()
			if errors.Is(err, io.EOF) {
				break
			}
			if errors.Is(err, errTooManyDocuments) {
				return declaration{}, new(needsIntervention(ReasonInputTooLarge,
					"the build holds more than %d YAML documents", MaxDocuments)), nil
			}
			if err != nil {
				r := needsIntervention(ReasonFileNotReadable, "%s is not valid YAML", p)
				return declaration{}, &r, nil
			}
			switch match(doc, id) {
			case matchExact:
				if found != nil {
					return declaration{}, new(needsIntervention(ReasonAmbiguousDeclaration,
						"%s is declared more than once (%s and %s)", what, found.path, p)), nil
				}
				found = &declaration{path: p, doc: doc}
			case matchNear:
				return declaration{}, new(needsIntervention(ReasonAmbiguousDeclaration,
					"%s may declare %s without its namespace or with another version", p, what)), nil
			case matchList:
				r := needsIntervention(ReasonDeclarationNotFound, "%s holds a List object, which is not supported", p)
				return declaration{}, &r, nil
			}
		}
	}
	if found == nil {
		return declaration{}, new(needsIntervention(ReasonDeclarationNotFound,
			"no plain manifest declares %s; it may come from a patch or generator", what)), nil
	}
	return *found, nil, nil
}

type matchKind int

const (
	matchNone matchKind = iota
	matchExact
	matchNear
	matchList
)

func match(doc map[string]any, id identity) matchKind {
	apiVersion, _ := doc["apiVersion"].(string)
	kind, _ := doc["kind"].(string)
	if _, hasItems := doc["items"]; hasItems && strings.HasSuffix(kind, "List") {
		return matchList
	}
	meta, _ := doc["metadata"].(map[string]any)
	name, _ := meta["name"].(string)
	namespace, _ := meta["namespace"].(string)
	gv, err := schema.ParseGroupVersion(apiVersion)
	if err != nil || gv.Group != id.gvk.Group || kind != id.gvk.Kind || name != id.name {
		return matchNone
	}
	switch {
	case namespace == id.namespace && gv.Version == id.gvk.Version:
		return matchExact
	case namespace == id.namespace || namespace == "":
		return matchNear
	default:
		return matchNone
	}
}

// errTooManyDocuments is returned by documents.next when the document budget is spent.
var errTooManyDocuments = errors.New("too many YAML documents")

// documents decodes a multi-document YAML file one document at a time, counting each against a budget of
// MaxDocuments that may be shared by several files.
type documents struct {
	r       *utilyaml.YAMLReader
	decoded *int
}

func newDocuments(data []byte, decoded *int) documents {
	return documents{r: utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(data))), decoded: decoded}
}

// next returns the next document, nil for one that is empty or holds only comments, io.EOF after the last,
// errTooManyDocuments when the budget is spent, or another error when the file is not valid YAML. A
// document is counted before it is decoded, so the one over the budget is never decoded. Duplicate keys are
// an error, so a document cannot hide a second value for the same field.
func (d documents) next() (map[string]any, error) {
	raw, err := d.r.Read()
	if err != nil {
		return nil, err
	}
	if *d.decoded++; *d.decoded > MaxDocuments {
		return nil, errTooManyDocuments
	}
	var doc map[string]any
	if err := yaml.UnmarshalStrict(raw, &doc); err != nil {
		return nil, err
	}
	return doc, nil
}

// ReadField returns the value of field in the one document of data that declares target. It is an error when
// no document declares the target, when more than one may (another version, a missing namespace, a second
// copy) or when a List holds it, because the value could then belong to another object. found is false when
// the document does not set the field. A file of more than MaxDocuments documents is an error too.
func ReadField(data []byte, target v1alpha1.ObjectRef, field Field) (value any, found bool, err error) {
	gv, err := schema.ParseGroupVersion(target.APIVersion)
	if err != nil {
		return nil, false, fmt.Errorf("invalid apiVersion")
	}
	id := identity{gvk: gv.WithKind(target.Kind), namespace: target.Namespace, name: target.Name}
	// One document is decoded at a time, at most MaxDocuments in this call, and only the match is kept.
	decoded := 0
	docs := newDocuments(data, &decoded)
	var exact map[string]any
	for {
		doc, err := docs.next()
		if errors.Is(err, io.EOF) {
			break
		}
		if errors.Is(err, errTooManyDocuments) {
			return nil, false, fmt.Errorf("the file holds more than %d YAML documents", MaxDocuments)
		}
		if err != nil {
			return nil, false, fmt.Errorf("the file is not valid YAML")
		}
		switch match(doc, id) {
		case matchExact:
			if exact != nil {
				return nil, false, fmt.Errorf("%s is declared more than once", id)
			}
			exact = doc
		case matchNear, matchList:
			return nil, false, fmt.Errorf("%s may be declared more than once", id)
		}
	}
	if exact == nil {
		return nil, false, fmt.Errorf("%s is not declared in the file", id)
	}
	value, found = field.Get(exact)
	return value, found, nil
}
