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
	"errors"
	"fmt"
	"io"
	"maps"
	"path"
	"regexp"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/runtime/schema"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"

	"github.com/pkalin/shadewright/internal/providers"
)

// Limits on what the mapping reads from Git, so a hostile repository cannot make it read without bound.
const (
	// MaxDepth is how many nested kustomization directories are followed.
	MaxDepth = 8
	// MaxFiles is how many manifest files are read.
	MaxFiles = 256
	// MaxFileBytes is the largest file read.
	MaxFileBytes = 1 << 20
	// maxRefLen is the longest path accepted, the MaxLength of GitFilePath.
	maxRefLen = 512
)

// kustomizationFileNames are the names kustomize looks for in a directory.
var kustomizationFileNames = []string{"kustomization.yaml", "kustomization.yml", kindKustomization}

// kustomizationKeys are the only fields a kustomization file may use: anything else (patches, generators,
// components, transformers, namespace, name prefix, images, replicas, labels) changes what is built.
var kustomizationKeys = map[string]bool{"apiVersion": true, "kind": true, "metadata": true, "resources": true}

// segmentRE is one path segment that is neither "." nor "..": it starts with a letter, digit or underscore
// and holds no characters that could make it a URL, an option or a hidden file.
var segmentRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)

// keyRE is a kustomization field name that is safe to quote in a message.
var keyRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]{0,63}$`)

// joinRel resolves ref, a relative path from a kustomization file or a Kustomization, against dir, a clean
// path inside the repository ("." for its root). It refuses absolute paths, URLs, segments that are not
// plain names, and results outside the repository.
func joinRel(dir, ref string) (string, bool) {
	if ref == "" || len(ref) > maxRefLen || strings.HasPrefix(ref, "/") {
		return "", false
	}
	for seg := range strings.SplitSeq(ref, "/") {
		if seg != "" && seg != "." && seg != ".." && !segmentRE.MatchString(seg) {
			return "", false
		}
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

// walker collects the manifest files a kustomization directory builds, following its resources.
type walker struct {
	git    providers.GitProvider
	commit string
	dirs   map[string]bool
	files  []string
	seen   map[string]bool
}

func newWalker(git providers.GitProvider, commit string) *walker {
	return &walker{git: git, commit: commit, dirs: map[string]bool{}, seen: map[string]bool{}}
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
		if keyRE.MatchString(key) {
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
		p, valid := joinRel(dir, ref)
		if !isString || !valid {
			r := needsIntervention(ReasonUnsupportedKustomization,
				"%s lists a resource that is not a relative path inside the repository", file.Path)
			return &r, nil
		}
		if !isManifestFile(p) {
			if stop, err := w.walk(ctx, p, depth+1); stop != nil || err != nil {
				return stop, err
			}
			continue
		}
		if w.seen[p] {
			continue
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

// kustomizationFile reads the one kustomization file in dir.
func (w *walker) kustomizationFile(ctx context.Context, dir string) (providers.File, *Result, error) {
	var found []providers.File
	for _, name := range kustomizationFileNames {
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
	f, err := w.git.ReadFile(ctx, w.commit, p)
	if providers.IsNotFound(err) {
		r := needsIntervention(reasonFileNotFound, "%s does not exist", p)
		return providers.File{}, &r, nil
	}
	if err != nil {
		return providers.File{}, nil, fmt.Errorf("read %s: %w", p, err)
	}
	if len(f.Data) > MaxFileBytes {
		r := needsIntervention(ReasonFileNotReadable, "%s is larger than %d bytes", p, MaxFileBytes)
		return providers.File{}, &r, nil
	}
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

// findDeclaration reads every file and returns the only document that declares id. A document with the same
// group, kind and name but another version, or with no namespace, could also be the target, so it makes the
// match ambiguous.
func (w *walker) findDeclaration(ctx context.Context, id identity) (declaration, *Result, error) {
	var exact []declaration
	var near []string
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
		docs, err := decodeDocuments(f.Data)
		if err != nil {
			r := needsIntervention(ReasonFileNotReadable, "%s is not valid YAML", p)
			return declaration{}, &r, nil
		}
		for _, doc := range docs {
			switch match(doc, id) {
			case matchExact:
				exact = append(exact, declaration{path: p, doc: doc})
			case matchNear:
				near = append(near, p)
			case matchList:
				r := needsIntervention(ReasonDeclarationNotFound, "%s holds a List object, which is not supported", p)
				return declaration{}, &r, nil
			}
		}
	}
	what := id.String()
	switch {
	case len(near) > 0:
		return declaration{}, new(needsIntervention(ReasonAmbiguousDeclaration,
			"%s may declare %s without its namespace or with another version", near[0], what)), nil
	case len(exact) == 0:
		return declaration{}, new(needsIntervention(ReasonDeclarationNotFound,
			"no plain manifest declares %s; it may come from a patch or generator", what)), nil
	case len(exact) > 1:
		return declaration{}, new(needsIntervention(ReasonAmbiguousDeclaration,
			"%s is declared more than once (%s and %s)", what, exact[0].path, exact[1].path)), nil
	}
	return exact[0], nil, nil
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

// decodeDocuments splits a multi-document YAML file and decodes each document. Empty documents are skipped.
// Duplicate keys are an error, so a document cannot hide a second value for the same field.
func decodeDocuments(data []byte) ([]map[string]any, error) {
	r := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(data)))
	var docs []map[string]any
	for {
		raw, err := r.Read()
		if errors.Is(err, io.EOF) {
			return docs, nil
		}
		if err != nil {
			return nil, err
		}
		var doc map[string]any
		if err := yaml.UnmarshalStrict(raw, &doc); err != nil {
			return nil, err
		}
		if doc != nil {
			docs = append(docs, doc)
		}
	}
}
