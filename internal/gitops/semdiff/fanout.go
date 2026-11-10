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
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	yaml "go.yaml.in/yaml/v3"

	"github.com/pkalin/shadewright/internal/gitops/gityaml"
)

// MaxTransformBytes bounds what the patches and transformers of a build can add to its render (#495), counted
// as bytes plus [NodeBytes] for every YAML node. A patch with a target, commonLabels, labels, commonAnnotations,
// namespace, namePrefix, nameSuffix, images and replicas copy one value into every place they reach, so the
// render grows with (the value) x (places), which the input budgets ([MaxBuildBytes], [MaxBuildDocuments]) do
// not bound: 160 KB of input rendered about 100 MB over 1,000 objects. The walk sums an upper bound of that
// product per visit over the objects the visit builds ([visitGrowth]), and refuses a build over this budget with
// TooLarge before kustomize runs. The bound only over-counts: where the walk cannot tell what a patch or a
// transformer reaches, it counts every place it may reach. A patch without a target counts as one with a target
// that selects the one object it names (#626); what patches merge into one mapping is bounded by [MaxMergedKeys].
//
// How it was chosen (measured 2026-11-09 with hack/load/render in a container limited to 128 MiB, Go 1.27.3,
// arm64): memory follows nodes more than bytes. A node a patch or transformer adds held about 650 to 800 bytes
// of Go heap in the render, a byte of a long scalar about 4 (an annotation) to 6 (an image name), hence
// [NodeBytes]. The build at the input budgets (4 MiB, 1,021 objects) peaks at 45 MiB of Go heap (69 to 71 MiB
// resident) alone; with one value at this budget, it peaks at 57 to 62 MiB (84 to 91 resident) for the byte-dense
// cases (an annotation or an image name copied into every place) and at 60 to 68 MiB (85 to 97 resident) for
// the node-dense ones (a targeted patch of one-character entries or of a list of numbers, a common annotation
// of one-character keys), well under the GOMEMLIMIT of 115 MiB (90 percent of the limit). At 6 MiB a node-dense
// case reached 81 MiB of heap and 105 MiB resident, too close. A build of about 1,000 objects with common
// labels, a namespace, a name prefix and an image override is at about 87 percent of the budget; with a patch
// on every Deployment too, about 800 objects fit.
const MaxTransformBytes = 4 << 20

// MaxMergedKeys bounds the keys and items the strategic merge patches of a build can merge into one mapping or
// sequence of an object (#626), counted per object and per path from its root and summed over every patch that
// may reach that object in every visit ([mergedKeys]). [MaxTransformBytes] counts what patches add, which is
// linear in their size, but kustomize merges each patch into the whole object it patches, so its time grows
// faster than the merged mapping's width: one ConfigMap and 60 patches of 255 data keys each, within every other bound, rendered one
// mapping of 15,300 keys in 16 s and allocated 4 GB (measured 2026-11-10, Go 1.27.3, arm64). With 16 such
// patches (4,080 keys) the render took 0.5 s. A patch without a target counts toward the object it names (kind
// and name), one with a target toward every object its target may select; where objects may be renamed before
// a patch runs, every patch counts toward every object. The keys that name the object it patches (apiVersion,
// kind, metadata, and its name and namespace) are in that object already and count for none. A resource's own
// mapping adds at most gityaml.MaxMappingKeys more.
//
// The bound is an estimate with known gaps, not a bound of the render: it does not count keys merged under
// sibling paths of one mapping together, keys that labels, commonLabels or commonAnnotations add, patches
// wrapped in a List, JSON 6902 moves, or a list a JSON patch creates that a strategic merge patch then merges
// into. #614 tracks bounding the render itself.
const MaxMergedKeys = 4096

// NodeBytes is what one YAML node a patch or transformer adds counts for against [MaxTransformBytes]: the heap a
// node holds in the render over that of a byte, rounded.
const NodeBytes = 128

// sat is the most a count reaches: a saturated sum is over every budget, never wrapped. Two counts of at most
// sat add up to less than the largest int.
const sat = 1 << 61

func satAdd(a, b int) int { return min(a+b, sat) }

func satMul(a, b int) int {
	if a == 0 || b == 0 {
		return 0
	}
	if a > sat/b {
		return sat
	}
	return a * b
}

// cost is what a value adds to the render: its bytes (as JSON, [jsonBytes]) and its YAML nodes.
type cost struct{ bytes, nodes int }

func (c cost) plus(o cost) cost { return cost{satAdd(c.bytes, o.bytes), satAdd(c.nodes, o.nodes)} }

func (c cost) times(k int) cost { return cost{satMul(c.bytes, k), satMul(c.nodes, k)} }

// total is the cost counted against MaxTransformBytes.
func (c cost) total() int { return satAdd(c.bytes, satMul(c.nodes, NodeBytes)) }

// shape is what a set of objects offers the transformers: places to write and references to rewrite.
type shape struct {
	// docs is an upper bound of the objects: the documents of the resource files; kinds counts them by kind
	// ("" for none, and anyKind for those a patch may have changed).
	docs  int
	kinds map[string]int
	// keys are the values under each key of [leafKeys]. items are the items of the sequences under each key
	// of [shapeKeys] (an item of a sequence nested directly in a sequence counts for the key above both), and
	// all those of every sequence.
	keys, items map[string]int
	all         int
	// loose are the nodes that may sit under any key once patched: those a JSON patch writes to a path it
	// does not spell plainly, and with a move operation every node of the objects. Each may be an item of a
	// sequence under every key.
	loose int
	// scalars are the scalar values (not mapping keys), each of which may be a name reference; nodes are all
	// the nodes; maxScalar is the bytes of the largest scalar.
	scalars, nodes, maxScalar int
}

func (s *shape) add(o *shape, times int) {
	if times == 0 {
		return
	}
	for _, m := range []*map[string]int{&s.kinds, &s.keys, &s.items} {
		if *m == nil {
			*m = map[string]int{}
		}
	}
	s.docs = satAdd(s.docs, satMul(o.docs, times))
	for _, m := range []struct{ to, from map[string]int }{{s.kinds, o.kinds}, {s.keys, o.keys}, {s.items, o.items}} {
		for k, n := range m.from {
			m.to[k] = satAdd(m.to[k], satMul(n, times))
		}
	}
	s.all = satAdd(s.all, satMul(o.all, times))
	s.loose = satAdd(s.loose, satMul(o.loose, times))
	s.scalars = satAdd(s.scalars, satMul(o.scalars, times))
	s.nodes = satAdd(s.nodes, satMul(o.nodes, times))
	s.maxScalar = max(s.maxScalar, o.maxScalar)
}

// coef holds what the transformers of a visit copy per place, by kind of place: per object, per item of a
// sequence under a key, per value under a key, per loose node ([shape]), per object, item of any sequence or
// loose node (all: the field specs of labels), and the bytes a name change adds to each scalar that may be a
// reference to the renamed object.
type coef struct {
	// objects is per object, by kind ([specKinds], "" for every other kind, and anyKind).
	objects map[string]cost
	// items is per item of a sequence under the key, leaves per value or item under the key.
	items, leaves map[string]cost
	loose, all    cost
	refs          int
}

func newCoef() *coef {
	return &coef{objects: map[string]cost{}, items: map[string]cost{}, leaves: map[string]cost{}}
}

func (c *coef) add(o *coef) {
	for k, n := range o.objects {
		c.objects[k] = c.objects[k].plus(n)
	}
	for k, n := range o.items {
		c.items[k] = c.items[k].plus(n)
	}
	for k, n := range o.leaves {
		c.leaves[k] = c.leaves[k].plus(n)
	}
	c.loose = c.loose.plus(o.loose)
	c.all = c.all.plus(o.all)
	c.refs = satAdd(c.refs, o.refs)
}

// fieldPath is a field spec of a kustomize built-in transformer: the path it writes, the kind it is limited
// to ("" for every kind), and whether it creates the path when missing.
type fieldPath struct {
	path, kind string
	create     bool
}

// The field specs of the built-in transformers in kustomize v0.21.2 (api/internal/konfig/builtinpluginconsts),
// which the renderer runs with no other configuration (configurations is refused): one per line, the path and
// the kind it is limited to, if any; a leading + marks create: true. They are copied, since the package is
// internal; a kustomize upgrade must check them again. AFFINITY stands for the four pod (anti-)affinity label
// selector paths ([affinityPaths]).
const (
	// labelSpecs: commonLabels (commonlabels.go with metadatalabels.go) and the volume claim template labels,
	// which labels with includeSelectors or includeTemplates also use.
	labelSpecs = `+metadata/labels
+spec/selector Service
+spec/selector ReplicationController
+spec/template/metadata/labels ReplicationController
+spec/selector/matchLabels Deployment
+spec/template/metadata/labels Deployment
AFFINITY Deployment
spec/template/spec/topologySpreadConstraints/labelSelector/matchLabels Deployment
+spec/selector/matchLabels ReplicaSet
+spec/template/metadata/labels ReplicaSet
+spec/selector/matchLabels DaemonSet
+spec/template/metadata/labels DaemonSet
+spec/selector/matchLabels StatefulSet
+spec/template/metadata/labels StatefulSet
AFFINITY StatefulSet
spec/template/spec/topologySpreadConstraints/labelSelector/matchLabels StatefulSet
+spec/volumeClaimTemplates[]/metadata/labels StatefulSet
spec/selector/matchLabels Job
+spec/template/metadata/labels Job
spec/jobTemplate/spec/selector/matchLabels CronJob
+spec/jobTemplate/metadata/labels CronJob
+spec/jobTemplate/spec/template/metadata/labels CronJob
spec/selector/matchLabels PodDisruptionBudget
spec/podSelector/matchLabels NetworkPolicy
spec/ingress/from/podSelector/matchLabels NetworkPolicy
spec/egress/to/podSelector/matchLabels NetworkPolicy`
	annotationSpecs = `+metadata/annotations
+spec/template/metadata/annotations ReplicationController
+spec/template/metadata/annotations Deployment
+spec/template/metadata/annotations ReplicaSet
+spec/template/metadata/annotations DaemonSet
+spec/template/metadata/annotations StatefulSet
+spec/template/metadata/annotations Job
+spec/jobTemplate/metadata/annotations CronJob
+spec/jobTemplate/spec/template/metadata/annotations CronJob`
	// namespaceSpecs: metadata/namespace, which the namespace transformer sets on every object, and its field
	// specs; the role binding subjects it also sets are counted by item ([transformCoef]).
	namespaceSpecs = `+metadata/namespace
+metadata/name Namespace
+spec/service/namespace APIService
spec/conversion/webhook/clientConfig/service/namespace CustomResourceDefinition`
	replicaSpecs = `+spec/replicas Deployment
+spec/replicas ReplicationController
+spec/replicas ReplicaSet
+spec/replicas StatefulSet`
)

// affinityPaths are the pod affinity and anti-affinity label selectors that commonLabels writes in Deployments
// and StatefulSets.
func affinityPaths() []string {
	out := make([]string, 0, 4)
	for _, a := range []string{"podAffinity", "podAntiAffinity"} {
		for _, term := range []string{
			"preferredDuringSchedulingIgnoredDuringExecution/podAffinityTerm",
			"requiredDuringSchedulingIgnoredDuringExecution",
		} {
			out = append(out, "spec/template/spec/affinity/"+a+"/"+term+"/labelSelector/matchLabels")
		}
	}
	return out
}

// parseSpecs reads field specs written as above.
func parseSpecs(text string) []fieldPath {
	var out []fieldPath
	for line := range strings.SplitSeq(text, "\n") {
		line, create := strings.CutPrefix(line, "+")
		path, kind, _ := strings.Cut(line, " ")
		if path == "AFFINITY" {
			for _, p := range affinityPaths() {
				out = append(out, fieldPath{p, kind, create})
			}
			continue
		}
		out = append(out, fieldPath{path, kind, create})
	}
	return out
}

// pathBound is the upper bound of the places a set of field specs writes in one build, by the kind of spec.
//
// A field spec with create and no sequence on its path reaches one place per object of the kinds it applies
// to (places, per kind; "" for the kinds no spec names), and may create every mapping on its path (createNodes:
// a key and a value for each, specs sharing their prefixes). A sequence on its path makes kustomize visit every
// item (fieldspec.Filter, also for a segment without "[]"), so each place then lies in a distinct item of the
// innermost sequence, a sequence under one of the path's keys: one place per item of a sequence under each
// key in items, with up to itemCreate nodes created in it. A "[]" segment is looked up, never created, so a
// path with one has no place in an object without that sequence.
//
// A field spec without create writes only where its whole path exists, so each place is a mapping under its
// last key, or an item of a sequence there: leaves holds, per last key, the most such specs one node can match
// (specs of one kind with the same path).
//
// A loose node ([shape]) has one key above it, so it hosts at most loose places: the most specs of one kind
// whose paths hold one key, counted per segment.
type pathBound struct {
	kinds             map[string]kindBound
	items, leaves     map[string]int
	itemCreate, loose int
}

type kindBound struct{ places, createNodes int }

// specKinds are the kinds the field specs name; an object of another kind is counted as "".
var specKinds = func() map[string]bool {
	m := map[string]bool{}
	for _, text := range []string{labelSpecs, annotationSpecs, namespaceSpecs, replicaSpecs} {
		for _, p := range parseSpecs(text) {
			if p.kind != "" {
				m[p.kind] = true
			}
		}
	}
	return m
}()

// keyNamespace is the namespace field of a kustomization file and of an object.
const keyNamespace = "namespace"

// anyKind counts the objects whose kind a patch may change: they are counted as the costliest kind.
const anyKind = "*"

func boundOf(paths []fieldPath) pathBound {
	b := pathBound{kinds: map[string]kindBound{}, items: map[string]int{}, leaves: map[string]int{}}
	byKind := map[string][]fieldPath{"": nil}
	for k := range specKinds {
		byKind[k] = nil
	}
	for _, p := range paths {
		byKind[p.kind] = append(byKind[p.kind], p)
		if !p.create {
			continue
		}
		segs := strings.Split(p.path, "/")
		for _, seg := range segs {
			b.items[strings.TrimSuffix(seg, "[]")]++
		}
		b.itemCreate = max(b.itemCreate, 2*len(segs))
	}
	// An object has one kind, so it matches the specs for every kind and those of its own kind at most.
	for kind, own := range byKind {
		specs := own
		if kind != "" {
			specs = append(append([]fieldPath{}, byKind[""]...), own...)
		}
		var kb kindBound
		prefixes, keys, same := map[string]bool{}, map[string]int{}, map[string]int{}
		for _, p := range specs {
			for seg := range strings.SplitSeq(p.path, "/") {
				keys[strings.TrimSuffix(seg, "[]")]++
			}
			if !p.create {
				same[p.path]++
				leaf := strings.TrimSuffix(p.path[strings.LastIndex(p.path, "/")+1:], "[]")
				b.leaves[leaf] = max(b.leaves[leaf], same[p.path])
				continue
			}
			if strings.Contains(p.path, "[]") {
				continue
			}
			kb.places++
			for i := range len(p.path) + 1 {
				if i == len(p.path) || p.path[i] == '/' {
					prefixes[p.path[:i]] = true
				}
			}
		}
		kb.createNodes = 2 * len(prefixes)
		b.kinds[kind] = kb
		for _, n := range keys {
			b.loose = max(b.loose, n)
		}
	}
	return b
}

var (
	labelBound      = boundOf(parseSpecs(labelSpecs))
	annotationBound = boundOf(parseSpecs(annotationSpecs))
	namespaceBound  = boundOf(parseSpecs(namespaceSpecs))
	replicaBound    = boundOf(parseSpecs(replicaSpecs))
	// imageItems: the image transformer changes an image field only when its value names the entry's image
	// (an empty name matches the empty field its "[]" specs create in an item), in containers, initContainers
	// and volumes items by its field specs and again in every containers and initContainers item, anywhere, by
	// its legacy filter; an image or reference field that is itself a sequence is visited item by item.
	imageItems = map[string]int{"containers": 2, "initContainers": 2, "volumes": 1, "image": 2, "reference": 1}
	// imageLoose is the most places of imageItems one loose node hosts, and imageCreate the nodes the image
	// transformer may create in an item (image/reference: two keys and two values).
	imageLoose, imageCreate = 2, 4
	// shapeKeys are the keys whose sequence items a file's shape counts, and leafKeys those whose mappings it
	// counts too.
	shapeKeys = func() map[string]bool {
		m := map[string]bool{"subjects": true}
		for _, b := range []pathBound{labelBound, annotationBound, namespaceBound, replicaBound} {
			for k := range b.items {
				m[k] = true
			}
			for k := range b.leaves {
				m[k] = true
			}
		}
		for k := range imageItems {
			m[k] = true
		}
		return m
	}()
	leafKeys = func() map[string]bool {
		m := map[string]bool{}
		for _, b := range []pathBound{labelBound, annotationBound, namespaceBound, replicaBound} {
			for k := range b.leaves {
				m[k] = true
			}
		}
		return m
	}()
)

// perObject adds f(kind), the cost per object of each kind, to the coefficients, and to anyKind the most of
// them.
func (c *coef) perObject(f func(kind string) cost) {
	var most cost
	for _, k := range append(slices.Collect(maps.Keys(specKinds)), "") {
		v := f(k)
		c.objects[k] = c.objects[k].plus(v)
		most = cost{max(most.bytes, v.bytes), max(most.nodes, v.nodes)}
	}
	c.objects[anyKind] = c.objects[anyKind].plus(most)
}

func (c *coef) addPaths(v cost, b pathBound) {
	c.perObject(func(kind string) cost {
		kb, ok := b.kinds[kind]
		if !ok {
			kb = b.kinds[""]
		}
		return v.times(kb.places).plus(cost{nodes: kb.createNodes})
	})
	item := v.plus(cost{nodes: b.itemCreate})
	for k, n := range b.items {
		c.items[k] = c.items[k].plus(item.times(n))
	}
	// A spec without create adds the value to a mapping or item that exists: no node of the path.
	for k, n := range b.leaves {
		c.leaves[k] = c.leaves[k].plus(v.times(n))
	}
	c.loose = c.loose.plus(item.times(b.loose))
}

// jsonBytes is the most bytes s takes in the JSON the render marshals each object to: 6 for a byte that JSON
// may escape as \u00XX (a control character, a quote, a backslash, and <, > and &, which encoding/json escapes
// for HTML), 1 for any other. So a value written with YAML escapes or full of such characters is not counted
// below what the render holds.
func jsonBytes(s string) int {
	n := 0
	for i := range len(s) {
		switch c := s[i]; {
		case c < 0x20, c == '"', c == '\\', c == '<', c == '>', c == '&', c >= 0x80:
			n += 6
		default:
			n++
		}
	}
	return n
}

// valueCost is the cost of a decoded kustomization value: [jsonBytes] of every string and key, 8 bytes for any
// other scalar, and a node for every key, value and item. The kustomization file is at most MaxFileBytes, so
// the walk is bounded.
func valueCost(v any) cost {
	switch t := v.(type) {
	case string:
		return cost{jsonBytes(t), 1}
	case map[string]any:
		c := cost{nodes: 1}
		for k, e := range t {
			c = c.plus(cost{jsonBytes(k), 1}).plus(valueCost(e))
		}
		return c
	case []any:
		c := cost{nodes: 1}
		for _, e := range t {
			c = c.plus(valueCost(e))
		}
		return c
	default:
		return cost{8, 1}
	}
}

// transformCoef returns what the transformers of one kustomization add per place, from its decoded fields.
// Patches are counted by the walk ([walker.countShapes]). Fields kustomize would refuse are counted as they are,
// so a malformed one is bounded too; the render then fails.
func transformCoef(k map[string]any) *coef {
	c := newCoef()
	if v, ok := k["commonLabels"]; ok {
		c.addPaths(valueCost(v), labelBound)
	}
	if v, ok := k["commonAnnotations"]; ok {
		c.addPaths(valueCost(v), annotationBound)
	}
	if v, ok := k["labels"]; ok {
		// Each entry copies its pairs, with the commonLabels specs at most; a malformed entry or list counts
		// as a whole.
		entries, ok := v.([]any)
		if !ok {
			entries = []any{v}
		}
		for _, e := range entries {
			m, ok := e.(map[string]any)
			n := valueCost(e)
			if ok {
				n = valueCost(m["pairs"])
			}
			c.addPaths(n, labelBound)
			// Field specs of its own reach any key: one place per object each, or per item of any sequence,
			// and may create every mapping on their path.
			fields, ok := m["fields"].([]any)
			if !ok && m["fields"] != nil {
				fields = []any{m["fields"]}
			}
			for _, f := range fields {
				place := n.plus(cost{nodes: 2 * fieldSegments(f)})
				c.all = c.all.plus(place)
			}
		}
	}
	if v, ok := k[keyNamespace]; ok {
		n := valueCost(v)
		c.addPaths(n, namespaceBound)
		// The namespace field of each role binding subject: a key and a value, in an item under subjects, or
		// in a loose node.
		subject := n.plus(cost{nodes: 2})
		c.items["subjects"] = c.items["subjects"].plus(subject)
		c.loose = c.loose.plus(subject)
	}
	for _, key := range []string{"namePrefix", "nameSuffix"} {
		if v, ok := k[key]; ok {
			n := valueCost(v).bytes
			c.perObject(func(string) cost { return cost{bytes: n} }) // metadata/name
			c.refs = satAdd(c.refs, n)
		}
	}
	if v, ok := k["images"]; ok {
		place := cost{bytes: valueCost(v).bytes, nodes: imageCreate}
		for key, times := range imageItems {
			c.items[key] = c.items[key].plus(place.times(times))
		}
		c.loose = c.loose.plus(place.times(imageLoose))
	}
	if v, ok := k["replicas"]; ok {
		c.addPaths(cost{bytes: valueCost(v).bytes}, replicaBound)
	}
	return c
}

// fieldSegments is the number of segments of the path of a labels field spec, or, for a malformed one, of
// the longest path a kustomization file could hold.
func fieldSegments(f any) int {
	if m, ok := f.(map[string]any); ok {
		if p, ok := m["path"].(string); ok {
			return strings.Count(p, "/") + 1
		}
	}
	return MaxFileBytes / 2
}

// scan is what the walk counts in a file or an inline patch: its shape as a resource or a patch (what it puts
// into the objects), its bytes ([jsonBytes]) and nodes, the bytes of the largest scalar a patch may write as a
// name or a namespace (rename), and whether a JSON patch moves a value or may change a kind.
type scan struct {
	shape shape
	// bytes and nodes are those of the text, which a patch with a target adds to every object it selects.
	bytes, nodes       int
	rename             int
	moves, kindChanges bool
	// merges is what the file merges into the objects as a patch ([patchMergesOf]), counted on its first use as
	// one: nil until then.
	merges *patchMerges
}

func newScan() *scan {
	return &scan{shape: shape{kinds: map[string]int{}, keys: map[string]int{}, items: map[string]int{}}}
}

// scanOf returns the scan of a file or inline patch that [documents] has accepted (UTF-8, parses, no
// anchors, aliases or merge keys), decoding it part by part as [refuseCopy] does. objects is what documents
// counted in it: each item of a List is an object of its own, of a kind the scan does not tell, so the objects
// past the documents count as anyKind.
func scanOf(name string, data []byte, objects int) (*scan, error) {
	s := newScan()
	s.bytes = jsonBytes(string(data))
	for _, p := range gityaml.Parts(data) {
		dec := yaml.NewDecoder(strings.NewReader(p.Text))
		for {
			var doc yaml.Node
			err := dec.Decode(&doc)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return nil, refuse(ReasonInvalidYAML, fmt.Sprintf("%s line %d: %v", name, p.Line, gityaml.ErrNotParse))
			}
			s.shape.docs++
			s.shape.kinds[kindOf(&doc)]++
			s.nodes = satAdd(s.nodes, nodesIn(&doc))
			// A document that is a sequence is a JSON patch (kustomize reads it as one).
			if len(doc.Content) == 1 && doc.Content[0].Kind == yaml.SequenceNode {
				s.jsonPatch(doc.Content[0])
			} else {
				s.walk(&doc, "", false)
			}
		}
	}
	if extra := objects - s.shape.docs; extra > 0 {
		s.shape.docs += extra
		s.shape.kinds[anyKind] += extra
	}
	return s, nil
}

// kindOf is the kind of an object document, "" for none.
func kindOf(doc *yaml.Node) string {
	if len(doc.Content) == 1 && doc.Content[0].Kind == yaml.MappingNode {
		m := doc.Content[0].Content
		for i := 0; i+1 < len(m); i += 2 {
			if m[i].Value == keyKind && m[i+1].Kind == yaml.ScalarNode {
				return m[i+1].Value
			}
		}
	}
	return ""
}

// walk counts n, the value under key ("" for none); named is set below a JSON patch value written to a name or
// a namespace, whose scalars may all become one.
func (s *scan) walk(n *yaml.Node, key string, named bool) {
	sh := &s.shape
	sh.nodes++
	if leafKeys[key] {
		sh.keys[key]++
	}
	switch n.Kind {
	case yaml.DocumentNode:
		for _, c := range n.Content {
			s.walk(c, "", named)
		}
	case yaml.ScalarNode:
		sh.scalars++
		b := jsonBytes(n.Value)
		sh.maxScalar = max(sh.maxScalar, b)
		if named || key == keyName || key == keyNamespace {
			s.rename = max(s.rename, b)
		}
	case yaml.SequenceNode:
		sh.all += len(n.Content)
		if shapeKeys[key] {
			sh.items[key] += len(n.Content)
		}
		for _, c := range n.Content {
			if c.Kind == yaml.SequenceNode {
				s.walk(c, key, named)
			} else {
				s.walk(c, "", named)
			}
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			sh.nodes++ // the key
			s.walk(n.Content[i+1], n.Content[i].Value, named)
		}
	}
}

// nodesIn counts the nodes of n, mapping keys included.
func nodesIn(n *yaml.Node) int {
	c := 1
	for i, e := range n.Content {
		if n.Kind == yaml.MappingNode && i%2 == 0 {
			c++
			continue
		}
		c = satAdd(c, nodesIn(e))
	}
	return c
}

// jsonPatch counts what the operations of a JSON patch put into an object: the value of each, under the key
// its path ends in, or as an item of the sequence under the last key before a trailing run of indexes or "-"
// (a mapping key that looks like an index counts as an item too, which can only over-count). A path that is
// not a plain string or holds an escape puts its value anywhere: its nodes are loose ([shape]). A value
// written to a path that ends in name or namespace may become a name, and one written to kind changes it; an
// operation other than add, replace, remove or test (a move: copy is refused) may put any node of an object
// anywhere.
func (s *scan) jsonPatch(ops *yaml.Node) {
	for _, op := range ops.Content {
		if op.Kind != yaml.MappingNode {
			continue
		}
		var path, value *yaml.Node
		for i := 0; i+1 < len(op.Content); i += 2 {
			switch v := op.Content[i+1]; op.Content[i].Value {
			case "op":
				if v.Kind != yaml.ScalarNode || v.Value != "add" && v.Value != "replace" && v.Value != "remove" &&
					v.Value != "test" {
					s.moves = true
				}
			case "path":
				path = v
			case "value":
				value = v
			}
		}
		if path == nil || path.Kind != yaml.ScalarNode || strings.Contains(path.Value, "~") {
			s.kindChanges = true
			if value != nil {
				before := s.shape.nodes
				s.walk(value, "", true)
				s.shape.loose = satAdd(s.shape.loose, s.shape.nodes-before)
			}
			continue
		}
		segs := strings.Split(path.Value, "/")
		item := false
		for len(segs) > 1 && isIndex(segs[len(segs)-1]) {
			segs, item = segs[:len(segs)-1], true
		}
		last := segs[len(segs)-1]
		if last == "kind" && !item {
			s.kindChanges = true
		}
		if value == nil {
			continue
		}
		named := !item && (last == "name" || last == keyNamespace)
		sh := &s.shape
		switch {
		case !item:
			s.walk(value, last, named)
		case value.Kind == yaml.SequenceNode:
			// An item that is itself a sequence: its items count for the key above both.
			sh.all++
			if shapeKeys[last] {
				sh.items[last]++
			}
			s.walk(value, last, named)
		default:
			sh.all++
			if shapeKeys[last] {
				sh.items[last]++
			}
			s.walk(value, "", named)
		}
	}
}

// isIndex says whether a JSON pointer segment can name a sequence item: digits, or "-" for the end.
func isIndex(seg string) bool {
	if seg == "-" {
		return true
	}
	return seg != "" && strings.Trim(seg, "0123456789") == ""
}

// ops are the patches and transformers one visit applies to the objects it builds: those of its kustomization
// file and of the components it lists.
type ops struct {
	coef coef
	// untargeted is the shape of the patches without a target, which kustomize applies to the one object each
	// names, and untargetedCost what they add to it: as much as each would with a target that selects that one
	// object (#626). targeted are the patches with a target, applied to every object it may select.
	untargeted     shape
	untargetedCost cost
	targeted       []targetedPatch
	// merges are the strategic merge patches, which [mergedKeys] counts per object. nameChanges is set when
	// one of the patches may change the name of an object (a JSON patch, or allowNameChange), and renamesFirst
	// when a component listed renames objects before the patches of the kustomization run ([ops.renames]).
	merges                    []mergePatch
	nameChanges, renamesFirst bool
	// rename sums, per patch, the largest scalar it may write as a name or a namespace; moves is set when one
	// of them is a JSON patch with an operation other than add, replace, remove or test, and kindChanges when
	// one may change the kind of an object.
	rename             int
	moves, kindChanges bool
}

func newOps() *ops { return &ops{coef: *newCoef()} }

// renames says whether o may give an object another name or kind: a name prefix or suffix, a patch that may
// change a name or a kind, or a JSON patch that moves values. A patch run later may then name the object by
// either name.
func (o *ops) renames() bool {
	return o.coef.refs > 0 || o.nameChanges || o.renamesFirst || o.moves || o.kindChanges
}

// mergePatch is a patch file or inline patch as a visit applies it: what it merges, and, with a target, the
// kind the target names (anyKind when it may select any kind).
type mergePatch struct {
	merges   *patchMerges
	targeted bool
	kind     string
}

// targetedPatch is a patch with a target: its shape, what it adds to each object it selects, and the kind its
// target names when that is a plain name ("" when the target may select any kind: its fields are regular
// expressions).
type targetedPatch struct {
	shape shape
	cost  cost
	kind  string
}

func (o *ops) add(p *ops) {
	o.coef.add(&p.coef)
	o.untargeted.add(&p.untargeted, 1)
	o.untargetedCost = o.untargetedCost.plus(p.untargetedCost)
	o.targeted = append(o.targeted, p.targeted...)
	o.merges = append(o.merges, p.merges...)
	o.nameChanges = o.nameChanges || p.nameChanges
	o.renamesFirst = o.renamesFirst || p.renamesFirst
	o.rename = satAdd(o.rename, p.rename)
	o.moves = o.moves || p.moves
	o.kindChanges = o.kindChanges || p.kindChanges
}

// visitGrowth applies o to s, the shape of the objects a visit builds (its resources, its directories already
// built, and its components' resources), and returns what it adds to the render; s becomes the shape of the
// result. kustomize runs the patches of a kustomization before its transformers, and every component's after
// the resources; the bound counts every patch before every transformer, so a node any patch adds is a place for
// every transformer, and a scalar any of them adds may be a reference a name change rewrites.
func visitGrowth(s *shape, o *ops) cost {
	objects := s.docs
	anyKinds := o.moves || o.kindChanges
	// A target that names a kind selects the objects of that kind, unless a patch may change kinds.
	selected := make([]int, len(o.targeted))
	for i, t := range o.targeted {
		selected[i] = objects
		if t.kind != "" && !anyKinds {
			selected[i] = min(objects, satAdd(s.kinds[t.kind], s.kinds[anyKind]))
		}
	}
	g := o.untargetedCost
	s.add(&o.untargeted, 1)
	for i, t := range o.targeted {
		g = g.plus(t.cost.times(selected[i]))
		s.add(&t.shape, selected[i])
	}
	if o.moves {
		s.loose = satAdd(s.loose, s.nodes)
	}
	if anyKinds {
		s.kinds = map[string]int{anyKind: objects}
	}
	c := &o.coef
	for k, n := range s.kinds {
		if k != anyKind && !specKinds[k] {
			k = ""
		}
		g = g.plus(c.objects[k].times(n))
	}
	for k, n := range c.items {
		g = g.plus(n.times(s.items[k]))
	}
	for k, n := range c.leaves {
		g = g.plus(n.times(satAdd(s.keys[k], s.items[k])))
	}
	g = g.plus(c.loose.times(s.loose))
	// A labels field spec of its own reaches one place per object, or per item of any sequence or loose node,
	// and the mappings it creates may sit under any key, where a later spec without create finds them.
	fields := c.all.times(satAdd(objects, satAdd(s.all, s.loose)))
	g = g.plus(fields)
	s.loose = satAdd(s.loose, fields.nodes)
	// A name changes by every prefix and suffix, and to any scalar a patch writes as a name; each reference
	// to it, any scalar, grows by as much.
	rename := satAdd(c.refs, o.rename)
	if o.moves {
		rename = satAdd(rename, s.maxScalar)
	}
	g.bytes = satAdd(g.bytes, satMul(rename, satAdd(s.scalars, g.nodes)))
	s.scalars = satAdd(s.scalars, g.nodes)
	s.nodes = satAdd(s.nodes, g.nodes)
	return g
}

// pathStep is a key below a path of [mergeWidths]: the id of the path, and the key ([itemKey] for the items of
// a sequence there).
type pathStep struct {
	parent int
	key    string
}

// itemKey stands for the items of a sequence in a path: the mappings of all of them share one path. A mapping key
// spelled the same shares it too, which only over-counts.
const itemKey = "[]"

// mergeWidths counts, per path from an object's root, the keys and items strategic merge patches merge into the
// mapping or sequence at that path ([MaxMergedKeys]). Paths are a trie of ids (0 is the root, and a key's id is
// above its parent's), so a deeply nested document costs one entry per node, not per byte of its paths.
type mergeWidths struct {
	ids   map[pathStep]int
	steps []pathStep
	n     []int
}

func newMergeWidths() *mergeWidths {
	return &mergeWidths{ids: map[pathStep]int{}, steps: []pathStep{{}}, n: []int{0}}
}

// child returns the id of key below the path parent, adding it when new.
func (m *mergeWidths) child(parent int, key string) int {
	s := pathStep{parent, key}
	if id, ok := m.ids[s]; ok {
		return id
	}
	id := len(m.steps)
	m.ids[s] = id
	m.steps = append(m.steps, s)
	m.n = append(m.n, 0)
	return id
}

// names says whether key, below the path at, names the object a patch applies to: apiVersion, kind and metadata
// at the root, name and namespace under metadata. Every object holds them, so a patch merges none of them in.
func (m *mergeWidths) names(at int, key string) bool {
	switch {
	case at == 0:
		return key == keyAPIVersion || key == keyKind || key == keyMetadata
	case m.steps[at] == pathStep{0, keyMetadata}:
		return key == keyName || key == keyNamespace
	}
	return false
}

// count adds the keys of each mapping and the items of each sequence of n, a patch value at the path at.
func (m *mergeWidths) count(n *yaml.Node, at int) {
	switch n.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			k := n.Content[i].Value
			if !m.names(at, k) {
				m.n[at] = satAdd(m.n[at], 1)
			}
			m.count(n.Content[i+1], m.child(at, k))
		}
	case yaml.SequenceNode:
		m.n[at] = satAdd(m.n[at], len(n.Content))
		item := m.child(at, itemKey)
		for _, c := range n.Content {
			m.count(c, item)
		}
	}
}

// objectKey names the object a strategic merge patch without a target applies to: its kind and name. The
// namespace is left out, so patches of objects that differ only by it count together, which only over-counts.
type objectKey struct{ kind, name string }

// mergeDoc is one strategic merge patch document: the object it names and what it merges into each path.
type mergeDoc struct {
	key    objectKey
	widths *mergeWidths
}

// patchMerges is what a patch file or inline patch merges: its strategic merge patch documents, and whether it
// holds a JSON patch too, which may change the name of an object.
type patchMerges struct {
	docs []mergeDoc
	json bool
}

// patchMergesOf returns what the strategic merge patches in a patch file or inline patch, which [scanOf] has
// accepted, merge into the objects they patch. A document that is a sequence is a JSON patch: it merges nothing,
// and [MaxTransformBytes] counts each of its values.
func patchMergesOf(name string, data []byte) (*patchMerges, error) {
	pm := &patchMerges{}
	for _, p := range gityaml.Parts(data) {
		dec := yaml.NewDecoder(strings.NewReader(p.Text))
		for {
			var doc yaml.Node
			err := dec.Decode(&doc)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return nil, refuse(ReasonInvalidYAML, fmt.Sprintf("%s line %d: %v", name, p.Line, gityaml.ErrNotParse))
			}
			if len(doc.Content) != 1 {
				continue
			}
			switch root := doc.Content[0]; root.Kind {
			case yaml.MappingNode:
				m := newMergeWidths()
				m.count(root, 0)
				pm.docs = append(pm.docs, mergeDoc{objectKey{kindOf(&doc), nameOf(root)}, m})
			case yaml.SequenceNode:
				pm.json = true
			}
		}
	}
	return pm, nil
}

// nameOf is metadata.name of a document's root mapping, or "" when it has none.
func nameOf(root *yaml.Node) string {
	md := valueAt(root, keyMetadata)
	if md == nil || md.Kind != yaml.MappingNode {
		return ""
	}
	if n := valueAt(md, keyName); n != nil && n.Kind == yaml.ScalarNode {
		return n.Value
	}
	return ""
}

// valueAt is the value of key in the mapping m, or nil.
func valueAt(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// mergedKeys sums, per object and per path from its root, the keys and items the strategic merge patches of a
// build may merge into the mapping or sequence at that path ([MaxMergedKeys]). A patch without a target counts
// toward the object it names (objects); one with a target toward every object of the kind it names, or of any
// kind (kinds, anyKind), as it may reach each. Paths share one trie (paths; its own counts stay unused) and the
// counts are sparse, so many objects with a small patch each cost what their patches hold.
type mergedKeys struct {
	paths   *mergeWidths
	objects map[objectKey]map[int]int
	kinds   map[string]map[int]int
}

func newMergedKeys() *mergedKeys {
	return &mergedKeys{paths: newMergeWidths(), objects: map[objectKey]map[int]int{}, kinds: map[string]map[int]int{}}
}

// apply adds the patches of a visit. When objects may have another name or kind than a patch names by the time
// it runs (renamed: the walk below the visit, or a component it lists, renames objects, or one of its own
// patches may), every patch counts toward every object.
func (m *mergedKeys) apply(o *ops, renamed bool) {
	for _, p := range o.merges {
		for _, d := range p.merges.docs {
			var dst map[int]int
			switch {
			case renamed:
				dst = bucket(m.kinds, anyKind)
			case p.targeted:
				dst = bucket(m.kinds, p.kind)
			default:
				dst = bucket(m.objects, d.key)
			}
			m.add(dst, d.widths)
		}
	}
}

// bucket returns the counts of k, adding them when new.
func bucket[K comparable](buckets map[K]map[int]int, k K) map[int]int {
	b, ok := buckets[k]
	if !ok {
		b = map[int]int{}
		buckets[k] = b
	}
	return b
}

// add adds the counts of w to dst, path by path.
func (m *mergedKeys) add(dst map[int]int, w *mergeWidths) {
	ids := make([]int, len(w.steps))
	for i := 1; i < len(w.steps); i++ {
		ids[i] = m.paths.child(ids[w.steps[i].parent], w.steps[i].key)
	}
	for i, n := range w.n {
		if n > 0 {
			dst[ids[i]] = satAdd(dst[ids[i]], n)
		}
	}
}

// widest is the most keys and items counted at one path of one object: what the patches naming it, those whose
// target names its kind and those whose target may select any kind merge there together. An object no patch
// names gets those of its kind and of any kind.
func (m *mergedKeys) widest() int {
	anyK := m.kinds[anyKind]
	best := 0
	for _, n := range anyK {
		best = max(best, n)
	}
	for k, counts := range m.kinds {
		if k == anyKind {
			continue
		}
		for p, n := range counts {
			best = max(best, satAdd(n, anyK[p]))
		}
	}
	for key, counts := range m.objects {
		kc := m.kinds[key.kind]
		for p, n := range counts {
			best = max(best, satAdd(satAdd(n, kc[p]), anyK[p]))
		}
	}
	return best
}
