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
	"sigs.k8s.io/yaml"

	"github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/gitops/gityaml"
	"github.com/pkalin/shadewright/internal/gitops/repopath"
	"github.com/pkalin/shadewright/internal/providers"
)

// Limits on what the mapping reads from Git, so a hostile repository cannot make it read without bound.
const (
	// MaxDepth is how many nested kustomization directories are followed.
	MaxDepth = 8
	// MaxFiles is how many manifest files are read.
	MaxFiles = 256
	// MaxReads is how many ReadFile and ListDir calls one mapping makes in all, kustomization file lookups
	// included, so a build cannot fan out into many directories.
	MaxReads = 512
	// MaxListDepth is how far below the Kustomization's path the mapping lists directories when the path has
	// no kustomization file and Flux would generate one.
	MaxListDepth = repopath.MaxListDepth
	// ListEntryBytes is what one entry of a directory listing costs against MaxTotalBytes
	// (repopath.ListEntryBytes), so the byte budget bounds listings as it bounds files, from the cache or not.
	ListEntryBytes = repopath.ListEntryBytes
	// MaxFileBytes is the largest file read. The provider refuses larger files before downloading them
	// (providers.ErrTooLarge); the mapping checks the size again in case an adapter does not.
	MaxFileBytes = providers.MaxFileBytes
	// MaxTotalBytes is how many bytes one mapping reads in all, kustomization and .sourceignore lookups and
	// cached reads included, so that many files near MaxFileBytes cannot add up without bound. Each read asks
	// the provider for at most what is left of it, so the budget bounds what is downloaded, cached and parsed,
	// and a build of exactly MaxTotalBytes resolves. The semantic diff has its own, smaller render budget
	// (semdiff.MaxBuildBytes), since a render holds far more memory per byte than a read.
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
	if stop != nil && stop.Reason == reasonNoKustomization {
		if depth == 0 {
			// Flux generates a kustomization for its own path only; kustomize needs one in every directory
			// a kustomization file lists.
			return w.generated(ctx, dir)
		}
		r := needsIntervention(ReasonNoKustomizationFile,
			"directory %s is listed as a resource but has no kustomization file", dir)
		return &r, nil
	}
	if stop != nil || err != nil {
		return stop, err
	}
	// sigs.k8s.io/yaml replaces aliases by copies, so the file is checked first (#594). The check stops counting
	// past MaxDocuments without reading the rest, so a file over it is refused too.
	if n, err := gityaml.Objects(file.Data, MaxDocuments); err != nil {
		r := needsIntervention(ReasonFileNotReadable, "%s %v", file.Path, err)
		return &r, nil
	} else if n > MaxDocuments {
		r := needsIntervention(ReasonInputTooLarge, "%s holds more than %d YAML documents", file.Path, MaxDocuments)
		return &r, nil
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
			if stop, err := w.walk(ctx, p, depth+repopath.NestDepth(dir, p)); stop != nil || err != nil {
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
		return providers.File{}, new(needsIntervention(reasonNoKustomization, "directory %s has no kustomization file", dir)), nil
	case 1:
		return found[0], nil, nil
	default:
		r := needsIntervention(ReasonUnsupportedKustomization, "directory %s has more than one kustomization file", dir)
		return providers.File{}, &r, nil
	}
}

// Reasons internal to the walker, never returned: a file that may legitimately be absent, and a directory
// without a kustomization file, which the caller turns into a generated build or a stop.
const (
	reasonFileNotFound    Reason = "fileNotFound"
	reasonNoKustomization Reason = "noKustomization"
)

// errListStopped ends [repopath.Generate] when a listing stopped the mapping; the stop is kept by the caller.
var errListStopped = errors.New("listing stopped")

// generated adds the files of the kustomization Flux generates for dir, which has none (ADR-0012, #122): every
// .yaml and .yml file below it, MaxListDepth deep at most, and every directory below it with a kustomization
// file, which is then walked as such, at its depth below dir so MaxDepth bounds the whole nesting. Entries
// Flux's default exclusions match are skipped. Each listing counts against MaxReads and the byte budget like a
// read. A .sourceignore file in a listed directory, or a manifest or base whose name is not a plain path
// segment, stops the mapping.
func (w *walker) generated(ctx context.Context, dir string) (*Result, error) {
	var stop *Result
	var listed []string
	resources, err := repopath.Generate(ctx, dir, func(ctx context.Context, d string) ([]providers.DirEntry, error) {
		entries, s, err := w.list(ctx, d)
		if s != nil {
			stop = s
			return nil, errListStopped
		}
		listed = append(listed, d)
		return entries, err
	})
	if stop != nil {
		return stop, nil
	}
	if s, ok := errors.AsType[*repopath.Stop](err); ok {
		reason := ReasonInvalidPath
		switch s.Kind {
		case repopath.StopTooDeep:
			reason = ReasonTooManyFiles
		case repopath.StopSourceIgnore:
			reason = ReasonSourceOptions
		}
		return new(needsIntervention(reason, "%s", s.Error())), nil
	}
	if err != nil {
		return nil, err
	}
	// Every listed directory was checked for a .sourceignore file by its listing.
	for _, d := range listed {
		w.ignoreChecked[d] = true
	}
	for _, res := range resources {
		if res.Dir {
			if stop, err := w.walk(ctx, res.Path, res.Depth); stop != nil || err != nil {
				return stop, err
			}
			continue
		}
		if w.seen[res.Path] {
			continue
		}
		if len(w.files) >= MaxFiles {
			return new(needsIntervention(ReasonTooManyFiles, "the build lists more than %d files", MaxFiles)), nil
		}
		w.seen[res.Path] = true
		w.files = append(w.files, res.Path)
	}
	return nil, nil
}

// list returns the entries of dir at the walker's commit, or a stop result when the read budget is spent, the
// host read the tree and dir is not a directory in it, the listing holds an entry no tree can hold, or it is
// over providers.MaxDirEntries. Any other failure, a plain not found included, is transient. A listing counts as one read against
// MaxReads and ListEntryBytes per entry against MaxTotalBytes, from the cache or not.
func (w *walker) list(ctx context.Context, dir string) ([]providers.DirEntry, *Result, error) {
	if w.reads >= MaxReads {
		return nil, new(needsIntervention(ReasonTooManyFiles, "the build needs more than %d Git reads", MaxReads)), nil
	}
	w.reads++
	entries, err := w.cache.listDir(ctx, w.git, w.repo, w.commit, dir)
	shown := shownDir(dir)
	switch repopath.CheckListing(entries, err) {
	case repopath.ListNotADirectory:
		return nil, new(needsIntervention(ReasonNoKustomizationFile,
			"%s has no kustomization file and is not a directory in the tree", shown)), nil
	case repopath.ListTooLarge:
		return nil, new(needsIntervention(ReasonTooManyFiles,
			"%s holds more than %d entries", shown, providers.MaxDirEntries)), nil
	case repopath.ListInvalidEntry:
		return nil, new(needsIntervention(ReasonInvalidPath,
			"%s holds an entry that no Git tree can hold", shown)), nil
	case repopath.ListFailed:
		return nil, nil, transient("list "+shown, err)
	}
	cost := len(entries) * ListEntryBytes
	if cost > MaxTotalBytes-w.bytes {
		return nil, new(needsIntervention(ReasonInputTooLarge,
			"the build reads more than %d bytes in all; the listing of %s is over it", MaxTotalBytes, shown)), nil
	}
	w.bytes += cost
	return entries, nil, nil
}

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
			if refused, ok := errors.AsType[*gityaml.Error](err); ok {
				return declaration{}, new(needsIntervention(ReasonFileNotReadable, "%s %v", p, refused)), nil
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

// errSeparator is returned by documents.read for a separator line with more than a comment after "---".
var errSeparator = errors.New("invalid YAML document separator")

// documents decodes a multi-document YAML file one document at a time, counting each against a budget of
// MaxDocuments that may be shared by several files.
type documents struct {
	r       *bufio.Reader
	decoded *int
	// line is the number of lines read from the file so far.
	line int
}

func newDocuments(data []byte, decoded *int) *documents {
	return &documents{r: bufio.NewReader(bytes.NewReader(data)), decoded: decoded}
}

// readLine returns the next line with "\n" after it, without its own "\n" or "\r\n", as
// k8s.io/apimachinery's yaml.LineReader does, and counts it.
func (d *documents) readLine() ([]byte, error) {
	var b bytes.Buffer
	isPrefix, err := true, error(nil)
	for isPrefix && err == nil {
		var l []byte
		l, isPrefix, err = d.r.ReadLine()
		b.Write(l)
	}
	b.WriteByte('\n')
	d.line++
	return b.Bytes(), err
}

// read returns the next document and the line of the file it starts at, split as k8s.io/apimachinery's
// yaml.YAMLReader splits a file, so that a refusal can name the line of the file: at each line that starts
// with "---" and holds at most a comment after it, which is dropped, except one before any other line of its
// document, which is kept.
func (d *documents) read() ([]byte, int, error) {
	var buf bytes.Buffer
	start := 0
	for {
		line, err := d.readLine()
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, 0, err
		}
		if rest, ok := bytes.CutPrefix(line, []byte("---")); ok {
			if s := strings.TrimSpace(string(rest)); s != "" && s[0] != '#' {
				return nil, 0, errSeparator
			}
			if buf.Len() != 0 {
				return buf.Bytes(), start, nil
			}
			if err != nil {
				return nil, 0, err
			}
		}
		if err != nil {
			if buf.Len() != 0 {
				return buf.Bytes(), start, nil
			}
			return nil, 0, err
		}
		if buf.Len() == 0 {
			start = d.line
		}
		buf.Write(line)
	}
}

// next returns the next document, nil for one that is empty or holds only comments, io.EOF after the last,
// errTooManyDocuments when the budget is spent, a [*gityaml.Error] whose line is the file's for one
// [gityaml.Objects] refuses, or another error when the file is not valid YAML. A document is counted before it
// is checked or decoded, so the one over the budget is never decoded. It is checked before sigs.k8s.io/yaml
// decodes it, since that parser replaces aliases by copies (#594); a document of more than MaxDocuments objects
// (a List) is over the budget too, since the check does not read past it. Duplicate keys are an error, so a
// document cannot hide a second value for the same field.
//
// The check builds a tree of the document that is dropped before sigs.k8s.io/yaml decodes it, so the two do
// not add up: for a document of 1 MiB the peak heap stays that of the decode alone
// (TestReadFieldPeakHeap).
func (d *documents) next() (map[string]any, error) {
	raw, start, err := d.read()
	if err != nil {
		return nil, err
	}
	if *d.decoded++; *d.decoded > MaxDocuments {
		return nil, errTooManyDocuments
	}
	if n, err := gityaml.Objects(raw, MaxDocuments); err != nil {
		e, ok := errors.AsType[*gityaml.Error](err)
		if !ok {
			return nil, err
		}
		if e.Line == 0 {
			return nil, e
		}
		return nil, &gityaml.Error{Line: start + e.Line - 1, Err: e.Err}
	} else if n > MaxDocuments {
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
		if refused, ok := errors.AsType[*gityaml.Error](err); ok {
			return nil, false, refused
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

// shownDir names a listed directory for a message: its path when every segment is valid, else a fixed text,
// since a directory Flux descends into may have a hidden or unusual name.
func shownDir(dir string) string {
	if p, ok := repopath.Join(".", dir); ok {
		return "directory " + p
	}
	return "a listed directory"
}
