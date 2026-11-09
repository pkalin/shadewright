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
	"strings"

	yaml "go.yaml.in/yaml/v3"

	"github.com/pkalin/shadewright/internal/gitops/gityaml"
)

// MaxTransformBytes bounds what the patches and transformers of a build can add to its render (#495). A patch
// with a target, commonLabels, labels, commonAnnotations, namespace, namePrefix, nameSuffix, images and replicas
// copy one value into every place they reach, so the render grows with (bytes of the value) x (places), which
// the input budgets ([MaxBuildBytes], [MaxBuildDocuments]) do not bound: 160 KB of input rendered about 100 MB
// over 1,000 objects. The walk sums that product over every visit, with an upper bound of the places ([growth]),
// and refuses a build over this budget with TooLarge before kustomize runs.
//
// How it was chosen (measured 2026-11-09 with hack/load/render in a container limited to 128 MiB, Go 1.27.3,
// arm64): a build at the input budgets (4 MiB, 1,021 objects) peaks at 69 to 75 MiB resident. Images are the
// costliest place per byte (in the test process, one image name of 16 KiB in 1,000 containers peaked at 83 MiB of
// Go heap, an annotation of the same size on 1,000 ConfigMaps at 41 MiB). The same build with an images entry
// at a bound of 4 MiB peaks at 87 to 89 MiB, at 6 MiB at 104 MiB, at 8 MiB at 106 MiB; with an annotation at
// 4 MiB at 79 to 81 MiB. 4 MiB keeps
// about 40 MiB for the rest of the executor, as the input budgets do (design 8.2.2), below the GOMEMLIMIT of 90
// percent of the limit. The supported repository (1,000 objects, design 8.2) with a few labels and annotations,
// a namespace, a name prefix and a few image overrides is bounded at about 1 MiB.
const MaxTransformBytes = 4 << 20

// sat is the most a count of [growth] reaches: a saturated sum is over every budget, never wrapped. Two counts
// of at most sat add up to less than the largest int.
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

// shape is what a file adds to the places a transformer can reach: the items of the sequences under each
// key of [shapeKeys] (an item of a sequence nested directly in a sequence counts for the key above both), the
// items of every sequence, and the name reference places ([nameRefKeys]: each mapping value under such a key,
// plus each item of a sequence there).
type shape struct {
	items map[string]int
	all   int
	refs  int
}

func (s *shape) add(o *shape, times int) {
	if s.items == nil {
		s.items = map[string]int{}
	}
	for k, n := range o.items {
		s.items[k] = satAdd(s.items[k], satMul(n, times))
	}
	s.all = satAdd(s.all, satMul(o.all, times))
	s.refs = satAdd(s.refs, satMul(o.refs, times))
}

// coef holds, for one build, the value bytes a transformer copies per place, by kind of place: per object, per
// item of a sequence under a key, per item of any sequence and per name reference.
type coef struct {
	objects int
	items   map[string]int
	all     int
	refs    int
}

func (c *coef) add(o *coef) {
	if c.items == nil {
		c.items = map[string]int{}
	}
	c.objects = satAdd(c.objects, o.objects)
	for k, n := range o.items {
		c.items[k] = satAdd(c.items[k], n)
	}
	c.all = satAdd(c.all, o.all)
	c.refs = satAdd(c.refs, o.refs)
}

// fieldPath is a field spec of a kustomize built-in transformer: the path it writes, and the kind it is limited
// to ("" for every kind).
type fieldPath struct{ path, kind string }

// The field specs of the built-in transformers in kustomize v0.21.2 (api/internal/konfig/builtinpluginconsts),
// which the renderer runs with no other configuration (configurations is refused): one per line, the path and
// the kind it is limited to, if any. They are copied, since the package is internal; a kustomize upgrade must
// check them again. AFFINITY stands for the four pod (anti-)affinity label selector paths ([affinityPaths]).
const (
	// labelSpecs: commonLabels (commonlabels.go with metadatalabels.go) and the volume claim template labels,
	// which labels with includeSelectors or includeTemplates also use.
	labelSpecs = `metadata/labels
spec/selector Service
spec/selector ReplicationController
spec/template/metadata/labels ReplicationController
spec/selector/matchLabels Deployment
spec/template/metadata/labels Deployment
AFFINITY Deployment
spec/template/spec/topologySpreadConstraints/labelSelector/matchLabels Deployment
spec/selector/matchLabels ReplicaSet
spec/template/metadata/labels ReplicaSet
spec/selector/matchLabels DaemonSet
spec/template/metadata/labels DaemonSet
spec/selector/matchLabels StatefulSet
spec/template/metadata/labels StatefulSet
AFFINITY StatefulSet
spec/template/spec/topologySpreadConstraints/labelSelector/matchLabels StatefulSet
spec/volumeClaimTemplates[]/metadata/labels StatefulSet
spec/selector/matchLabels Job
spec/template/metadata/labels Job
spec/jobTemplate/spec/selector/matchLabels CronJob
spec/jobTemplate/metadata/labels CronJob
spec/jobTemplate/spec/template/metadata/labels CronJob
spec/selector/matchLabels PodDisruptionBudget
spec/podSelector/matchLabels NetworkPolicy
spec/ingress/from/podSelector/matchLabels NetworkPolicy
spec/egress/to/podSelector/matchLabels NetworkPolicy`
	annotationSpecs = `metadata/annotations
spec/template/metadata/annotations ReplicationController
spec/template/metadata/annotations Deployment
spec/template/metadata/annotations ReplicaSet
spec/template/metadata/annotations DaemonSet
spec/template/metadata/annotations StatefulSet
spec/template/metadata/annotations Job
spec/jobTemplate/metadata/annotations CronJob
spec/jobTemplate/spec/template/metadata/annotations CronJob`
	// namespaceSpecs: metadata/namespace, which the namespace transformer sets on every object, and its field
	// specs; the role binding subjects it also sets are counted by item ([transformCoef]).
	namespaceSpecs = `metadata/namespace
metadata/name Namespace
spec/service/namespace APIService
spec/conversion/webhook/clientConfig/service/namespace CustomResourceDefinition`
	replicaSpecs = `spec/replicas Deployment
spec/replicas ReplicationController
spec/replicas ReplicaSet
spec/replicas StatefulSet`
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
		path, kind, _ := strings.Cut(line, " ")
		if path == "AFFINITY" {
			for _, p := range affinityPaths() {
				out = append(out, fieldPath{p, kind})
			}
			continue
		}
		out = append(out, fieldPath{path, kind})
	}
	return out
}

var (
	// nameRefKeys are the last keys of the name reference field specs (namereference.go, none of which
	// creates a field): a name prefix or suffix reaches every reference to a renamed object, each a value
	// under one of these keys or an item of a sequence there (resourceNames, subjects).
	nameRefKeys = keySet("adminSecretName", "auth-secret", "auth-tls-secret", "claimName", "configMap",
		"fastcgi-params-configmap", "ingressClassName", "name", "policyName", "priorityClassName", "resourceNames",
		"secretName", "secretRef", "service", "serviceAccountName", "serviceName", "storageClassName", "subjects",
		"userSecretName", "volumeName")
)

func keySet(keys ...string) map[string]bool {
	m := make(map[string]bool, len(keys))
	for _, k := range keys {
		m[k] = true
	}
	return m
}

// pathBound is the upper bound of the places a set of field specs writes in one build, per byte of the value:
// perObject places in every object, plus one place per item of a sequence under each key in items. A field
// spec reaches one place per object unless a sequence lies on its path: kustomize then visits every item
// (fieldspec.Filter, also for a segment without "[]"), so each place lies in a distinct item of the innermost
// sequence on the path, a sequence under one of the path's keys. A "[]" segment is looked up, never created,
// so a path with one has no place in an object without that sequence.
type pathBound struct {
	perObject int
	items     map[string]int
}

func boundOf(paths []fieldPath) pathBound {
	b := pathBound{items: map[string]int{}}
	perKind := map[string]int{}
	kindless := 0
	for _, p := range paths {
		for seg := range strings.SplitSeq(p.path, "/") {
			b.items[strings.TrimSuffix(seg, "[]")]++
		}
		if strings.Contains(p.path, "[]") {
			continue
		}
		if p.kind == "" {
			kindless++
		} else {
			perKind[p.kind]++
		}
	}
	// An object has one kind, so it matches the specs for every kind and those of its own kind at most.
	b.perObject = kindless
	for _, n := range perKind {
		b.perObject = max(b.perObject, kindless+n)
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
	// shapeKeys are the keys whose sequence items a file's shape counts.
	shapeKeys = func() map[string]bool {
		m := map[string]bool{"subjects": true}
		for _, b := range []pathBound{labelBound, annotationBound, namespaceBound, replicaBound} {
			for k := range b.items {
				m[k] = true
			}
		}
		for k := range imageItems {
			m[k] = true
		}
		return m
	}()
)

func (c *coef) addPaths(v int, b pathBound) {
	if c.items == nil {
		c.items = map[string]int{}
	}
	c.objects = satAdd(c.objects, satMul(v, b.perObject))
	for k, n := range b.items {
		c.items[k] = satAdd(c.items[k], satMul(v, n))
	}
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

// valueBytes is the size of a decoded kustomization value: [jsonBytes] of every string and key, and 8 for any
// other scalar. The kustomization file is at most MaxFileBytes, so the walk is bounded.
func valueBytes(v any) int {
	switch t := v.(type) {
	case string:
		return jsonBytes(t)
	case map[string]any:
		n := 0
		for k, e := range t {
			n = satAdd(n, satAdd(jsonBytes(k), valueBytes(e)))
		}
		return n
	case []any:
		n := 0
		for _, e := range t {
			n = satAdd(n, valueBytes(e))
		}
		return n
	default:
		return 8
	}
}

// transformCoef returns what the transformers of one kustomization add per place, from its decoded fields.
// Patches are counted by the walk ([walker.countShapes]). Fields kustomize would refuse are counted as they are,
// so a malformed one is bounded too; the render then fails.
func transformCoef(k map[string]any) *coef {
	c := &coef{items: map[string]int{}}
	if v, ok := k["commonLabels"]; ok {
		c.addPaths(valueBytes(v), labelBound)
	}
	if v, ok := k["commonAnnotations"]; ok {
		c.addPaths(valueBytes(v), annotationBound)
	}
	if v, ok := k["labels"]; ok {
		n := valueBytes(v)
		c.addPaths(n, labelBound)
		// Field specs of its own reach any key: one place per object each, or per item of any sequence.
		fields := 0
		if entries, ok := v.([]any); ok {
			for _, e := range entries {
				if m, ok := e.(map[string]any); ok {
					if f, ok := m["fields"].([]any); ok {
						fields += len(f)
					} else if m["fields"] != nil {
						fields++
					}
				}
			}
		}
		c.objects = satAdd(c.objects, satMul(n, fields))
		c.all = satAdd(c.all, satMul(n, fields))
	}
	if v, ok := k["namespace"]; ok {
		n := valueBytes(v)
		c.addPaths(n, namespaceBound)
		c.items["subjects"] = satAdd(c.items["subjects"], n)
	}
	for _, key := range []string{"namePrefix", "nameSuffix"} {
		if v, ok := k[key]; ok {
			n := valueBytes(v)
			c.objects = satAdd(c.objects, n) // metadata/name
			c.refs = satAdd(c.refs, n)
		}
	}
	if v, ok := k["images"]; ok {
		n := valueBytes(v)
		for key, times := range imageItems {
			c.items[key] = satAdd(c.items[key], satMul(n, times))
		}
	}
	if v, ok := k["replicas"]; ok {
		c.addPaths(valueBytes(v), replicaBound)
	}
	return c
}

// shapeOf returns the shape of a file or inline patch that [documents] has accepted (UTF-8, parses, no
// anchors, aliases or merge keys), decoding it part by part as [refuseCopy] does.
func shapeOf(name string, data []byte) (*shape, error) {
	s := &shape{items: map[string]int{}}
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
			s.walk(&doc, "")
		}
	}
	return s, nil
}

// walk counts n, the value under key ("" for none).
func (s *shape) walk(n *yaml.Node, key string) {
	switch n.Kind {
	case yaml.DocumentNode:
		for _, c := range n.Content {
			s.walk(c, "")
		}
	case yaml.SequenceNode:
		s.all += len(n.Content)
		if shapeKeys[key] {
			s.items[key] += len(n.Content)
		}
		if nameRefKeys[key] {
			s.refs += len(n.Content)
		}
		for _, c := range n.Content {
			if c.Kind == yaml.SequenceNode {
				s.walk(c, key)
			} else {
				s.walk(c, "")
			}
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			k := n.Content[i].Value
			if nameRefKeys[k] {
				s.refs++
			}
			s.walk(n.Content[i+1], k)
		}
	}
}

// growth is the upper bound of the bytes the transformers add to the render, given the coefficients summed
// over every visit, the shape of the input with repetition, the targeted patches (bytes and shape, with
// repetition) and objects, the documents of the build, an upper bound of its objects. A targeted patch writes
// its content into every object it may select, so its items and references count once per object too.
func growth(c *coef, input *shape, patchBytes int, patches *shape, objects int) int {
	in := &shape{items: maps.Clone(input.items)}
	in.add(&shape{all: input.all, refs: input.refs}, 1)
	in.add(patches, objects)
	g := satAdd(satMul(patchBytes, objects), satMul(c.objects, objects))
	for k, n := range c.items {
		g = satAdd(g, satMul(n, in.items[k]))
	}
	g = satAdd(g, satMul(c.all, in.all))
	return satAdd(g, satMul(c.refs, in.refs))
}
