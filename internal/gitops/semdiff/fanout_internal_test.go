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
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// fanoutObjects reaches every place the bound models: workload templates and selectors, affinity and topology
// terms, volume claim templates, a CronJob, network policy peers, role binding subjects, role resource names,
// containers and init containers anywhere (a custom resource too, for the legacy image filter), volumes,
// image volumes and name references. Every list holds several items, so a place counted per object instead of
// per item would show.
const fanoutObjects = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
  namespace: n
spec:
  selector:
    matchLabels: {app: web}
  template:
    metadata:
      labels: {app: web}
    spec:
      serviceAccountName: sa
      affinity:
        podAffinity:
          requiredDuringSchedulingIgnoredDuringExecution:
            - labelSelector: {matchLabels: {a: b}}
              topologyKey: k
            - labelSelector: {matchLabels: {a: c}}
              topologyKey: k
      topologySpreadConstraints:
        - labelSelector: {matchLabels: {a: b}}
        - labelSelector: {matchLabels: {a: c}}
      initContainers:
        - name: i1
          image: app
      containers:
        - name: c1
          image: app:1
          envFrom:
            - configMapRef: {name: cm}
            - secretRef: {name: s}
        - name: c2
          image: app
          env:
            - name: E
              valueFrom: {configMapKeyRef: {name: cm, key: k}}
      volumes:
        - name: v1
          configMap: {name: cm}
        - name: v2
          image: {reference: app}
---
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: db
  namespace: n
spec:
  serviceName: svc
  selector:
    matchLabels: {app: db}
  template:
    metadata:
      labels: {app: db}
    spec:
      containers:
        - name: db
          image: app
  volumeClaimTemplates:
    - metadata: {name: d1}
    - metadata: {name: d2}
---
apiVersion: batch/v1
kind: CronJob
metadata:
  name: job
  namespace: n
spec:
  schedule: "* * * * *"
  jobTemplate:
    spec:
      template:
        spec:
          restartPolicy: Never
          containers:
            - name: j
              image: app
---
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: np
  namespace: n
spec:
  podSelector: {matchLabels: {a: b}}
  ingress:
    - from:
        - podSelector: {matchLabels: {a: b}}
        - podSelector: {matchLabels: {a: c}}
---
apiVersion: v1
kind: Service
metadata:
  name: svc
  namespace: n
spec:
  selector: {app: db}
  ports:
    - port: 80
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: cm
  namespace: n
---
apiVersion: v1
kind: Secret
metadata:
  name: s
  namespace: n
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: sa
  namespace: n
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: r
  namespace: n
rules:
  - apiGroups: [""]
    resources: [configmaps]
    resourceNames: [cm, cm]
    verbs: [get]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: rb
  namespace: n
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: Role, name: r}
subjects:
  - {kind: ServiceAccount, name: sa, namespace: n}
  - {kind: ServiceAccount, name: default}
---
apiVersion: example.com/v1
kind: Thing
metadata:
  name: t
  namespace: n
spec:
  deep:
    containers:
      - image: app
      - image: app
`

// objectsFile is the resource file the fixtures list.
const objectsFile = "o.yaml"

// renderedBytes renders files and returns the bytes of the objects as JSON.
func renderedBytes(t *testing.T, files map[string][]byte) int {
	t.Helper()
	b, _ := renderedSize(t, files)
	return b
}

// renderedSize renders files and returns the bytes of the objects as JSON and their YAML nodes: one per
// object, key, value and item.
func renderedSize(t *testing.T, files map[string][]byte) (bytes, nodes int) {
	t.Helper()
	objs, err := render(context.Background(), files, ".")
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	for _, o := range objs {
		b, err := json.Marshal(o)
		if err != nil {
			t.Fatal(err)
		}
		bytes += len(b)
		nodes += 1 + nodesOf(o)
	}
	return bytes, nodes
}

func nodesOf(v any) int {
	n := 1
	switch t := v.(type) {
	case map[string]any:
		for _, e := range t {
			n += 1 + nodesOf(e)
		}
	case []any:
		for _, e := range t {
			n += nodesOf(e)
		}
	}
	return n
}

// jsonPerNode is the most JSON punctuation one node adds besides its text: the quotes of a key and of its
// value, the colon, the comma, and braces or brackets.
const jsonPerNode = 6

// TestGrowthBoundsTheRender renders fanoutObjects with each transformer and a value of 1 KiB, or of many small
// entries, and checks that what the render grows by, in bytes and in nodes, is at most the bound the walk
// computes: a place the bound does not count would show.
func TestGrowthBoundsTheRender(t *testing.T) {
	v := strings.Repeat("x", 1<<10)
	var dense strings.Builder
	for i := range 100 {
		fmt.Fprintf(&dense, "a%d: x, ", i)
	}
	d := dense.String() + "z: x"
	jsonPatch := "patches:\n  - target: {kind: Deployment}\n    patch: |\n      - op: add\n        path: /spec/template/spec/containers/-\n" +
		"        value: {name: added, image: app}\n      - op: add\n        path: /spec/template/spec/containers/-\n" +
		"        value: {name: added2, image: app}\n"
	targeted := "patches:\n  - target: {kind: Deployment}\n    patch: |\n      apiVersion: apps/v1\n      kind: Deployment\n" +
		"      metadata: {name: any}\n      spec: {template: {metadata: {annotations: {k: " + v + "}}}}\n"
	cases := map[string]string{
		"commonLabels":       "commonLabels: {a: " + v + "}\n",
		"labels":             "labels:\n  - pairs: {a: " + v + "}\n    includeSelectors: true\n    includeTemplates: true\n",
		"labels with fields": "labels:\n  - pairs: {a: " + v + "}\n    fields:\n      - path: spec/template/spec/containers[]/x\n        create: true\n",
		"commonAnnotations":  "commonAnnotations: {a: " + v + "}\n",
		// Each \x01 and < is 6 bytes in the JSON the render marshals the objects to.
		"commonAnnotations, escaped": "commonAnnotations: {a: \"" + strings.Repeat(`\x01<`, 512) + "\"}\n",
		"namespace":                  "namespace: n" + v + "\n",
		"namePrefix":                 "namePrefix: p" + v + "-\n",
		"nameSuffix":                 "nameSuffix: -s" + v + "\n",
		"images":                     "images:\n  - name: app\n    newName: b" + v + "\n",
		"images, empty name":         "images:\n  - name: \"\"\n    newName: b" + v + "\n",
		"replicas":                   "replicas:\n  - name: web\n    count: 1" + strings.Repeat("0", 8) + "\n",
		"targeted inline patch":      targeted,
		"commonLabels, dense":        "commonLabels: {" + d + "}\n",
		"commonAnnotations, dense":   "commonAnnotations: {" + d + "}\n",
		"labels, dense": "labels:\n  - pairs: {" + d + "}\n    includeSelectors: true\n    includeTemplates: true\n" +
			"    fields:\n      - path: spec/template/spec/containers[]/x/y\n        create: true\n",
		"namespace on subjects": "namespace: n" + v + "\n",
		"targeted dense patch": "patches:\n  - target: {kind: Deployment}\n    patch: |\n      apiVersion: apps/v1\n" +
			"      kind: Deployment\n      metadata: {name: any, annotations: {" + d + "}}\n",
		"JSON patch, then images": jsonPatch + "images:\n  - name: app\n    newName: b" + v + "\n",
		"JSON patch, then labels": jsonPatch + "labels:\n  - pairs: {" + d + "}\n    fields:\n      - path: spec/template/spec/containers[]/x\n        create: true\n",
		"rename by a JSON patch": "patches:\n  - target: {kind: ConfigMap}\n    patch: |\n      - op: replace\n        path: /metadata/name\n" +
			"        value: c" + v + "\n",
		"rename by a patch, then a prefix": "namePrefix: p" + v + "-\npatches:\n  - target: {kind: Secret}\n    patch: |\n" +
			"      - op: replace\n        path: /metadata/name\n        value: s" + v + "\n",
		// The ConfigMap becomes a Deployment, where commonLabels writes three places, not one.
		"kind change, then commonLabels": "commonLabels: {a: " + v + "}\npatches:\n  - target: {kind: ConfigMap}\n" +
			"    patch: |\n      - op: replace\n        path: /kind\n        value: Deployment\n" +
			"      - op: replace\n        path: /apiVersion\n        value: apps/v1\n",
	}
	baseBytes, baseNodes := renderedSize(t, map[string][]byte{rootKust: []byte("resources: [" + objectsFile + "]\n"), objectsFile: []byte(fanoutObjects)})
	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			files := map[string][]byte{rootKust: []byte(extra + "resources: [" + objectsFile + "]\n"), objectsFile: []byte(fanoutObjects)}
			w, err := walkTree(context.Background(), files, ".")
			if err != nil {
				t.Fatalf("walkTree: %v", err)
			}
			b, n := renderedSize(t, files)
			b, n = b-baseBytes, n-baseNodes
			g := w.growth
			if n > g.nodes {
				t.Errorf("the render grew by %d nodes, over the bound of %d", n, g.nodes)
			}
			if b > satAdd(g.bytes, satMul(jsonPerNode, g.nodes)) {
				t.Errorf("the render grew by %d bytes, over the bound of %d bytes and %d nodes", b, g.bytes, g.nodes)
			}
			t.Logf("%s: grew %d bytes and %d nodes, bound %d and %d", name, b, n, g.bytes, g.nodes)
		})
	}
}

// keyBytes is the most JSON bytes a node the bound counts may take besides the bytes it counts: the key of a
// mapping a transformer creates, such as "matchLabels", with its quotes, colon and comma.
const keyBytes = 16

// wantBytesWithin fails when grew, what a render grew by, is over the bytes of g with keyBytes for each node.
func wantBytesWithin(t *testing.T, grew int, g cost) {
	t.Helper()
	if bound := satAdd(g.bytes, satMul(keyBytes, g.nodes)); grew > bound {
		t.Fatalf("the render grew by %d bytes, over the bound of %d (%d bytes and %d nodes)", grew, bound, g.bytes,
			g.nodes)
	}
}

// TestGrowthBoundsAKindChange renders ConfigMaps that a patch turns into Deployments, by a JSON patch or by
// a strategic merge patch that may change the kind: commonLabels then writes three places in each, not the
// one of a ConfigMap, so the bound counts every object as the costliest kind.
func TestGrowthBoundsAKindChange(t *testing.T) {
	var cms strings.Builder
	for i := range 20 {
		fmt.Fprintf(&cms, "---\napiVersion: v1\nkind: ConfigMap\nmetadata: {name: c%d}\n", i)
	}
	labels := "commonLabels: {a: " + strings.Repeat("x", 1<<10) + "}\n"
	for name, patch := range map[string]string{
		"JSON patch": "  - target: {kind: ConfigMap}\n    patch: |\n      - op: replace\n        path: /kind\n" +
			"        value: Deployment\n      - op: replace\n        path: /apiVersion\n        value: apps/v1\n",
		"strategic merge patch": "  - target: {kind: ConfigMap}\n    options: {allowKindChange: true}\n    patch: |\n" +
			"      apiVersion: apps/v1\n      kind: Deployment\n      metadata: {name: any}\n",
	} {
		t.Run(name, func(t *testing.T) {
			base := map[string][]byte{rootKust: []byte("resources: [" + objectsFile + "]\n"), objectsFile: []byte(cms.String())}
			files := map[string][]byte{
				rootKust: []byte(labels + "resources: [" + objectsFile + "]\npatches:\n" + patch), objectsFile: base[objectsFile],
			}
			b0, _ := renderedSize(t, base)
			b, _ := renderedSize(t, files)
			w, err := walkTree(context.Background(), files, ".")
			if err != nil {
				t.Fatalf("walkTree: %v", err)
			}
			wantBytesWithin(t, b-b0, w.growth)
		})
	}
}

// TestGrowthBoundsAList renders a List of Deployments: each item is an object, of a kind the walk does not
// read, so each counts as the costliest kind.
func TestGrowthBoundsAList(t *testing.T) {
	var list strings.Builder
	list.WriteString("apiVersion: v1\nkind: List\nitems:\n")
	for i := range 20 {
		fmt.Fprintf(&list, "  - {apiVersion: apps/v1, kind: Deployment, metadata: {name: d%d}}\n", i)
	}
	base := map[string][]byte{rootKust: []byte("resources: [" + objectsFile + "]\n"), objectsFile: []byte(list.String())}
	files := map[string][]byte{
		rootKust:    []byte("commonLabels: {a: " + strings.Repeat("x", 1<<10) + "}\nresources: [" + objectsFile + "]\n"),
		objectsFile: base[objectsFile],
	}
	b0, _ := renderedSize(t, base)
	b, _ := renderedSize(t, files)
	w, err := walkTree(context.Background(), files, ".")
	if err != nil {
		t.Fatalf("walkTree: %v", err)
	}
	wantBytesWithin(t, b-b0, w.growth)
}

// TestGrowthBoundsSpecsWithoutCreate renders a place a field spec without create reaches only where its path
// exists, in a mapping a labels field spec of a base created: the selector of a Job, for commonLabels in an
// overlay.
func TestGrowthBoundsSpecsWithoutCreate(t *testing.T) {
	v := strings.Repeat("x", 1<<10)
	var jobs strings.Builder
	for i := range 20 {
		fmt.Fprintf(&jobs, "---\napiVersion: batch/v1\nkind: Job\nmetadata: {name: j%d}\nspec: {}\n", i)
	}
	for name, c := range map[string]struct{ base, overlay, objects string }{
		"a mapping a base created": {"resources: [" + objectsFile + "]\nlabels:\n  - pairs: {b: c}\n    fields:\n" +
			"      - {path: spec/selector/matchLabels, kind: Job, create: true}\n", "commonLabels: {a: " + v + "}\n",
			jobs.String()},
	} {
		t.Run(name, func(t *testing.T) {
			base := map[string][]byte{"base/" + rootKust: []byte(c.base), "base/o.yaml": []byte(c.objects),
				rootKust: []byte("resources: [base]\n")}
			files := map[string][]byte{"base/" + rootKust: []byte(c.base), "base/o.yaml": []byte(c.objects),
				rootKust: []byte(c.overlay + "resources: [base]\n")}
			b0, _ := renderedSize(t, base)
			b, _ := renderedSize(t, files)
			w0, err := walkTree(context.Background(), base, ".")
			if err != nil {
				t.Fatalf("walkTree: %v", err)
			}
			w, err := walkTree(context.Background(), files, ".")
			if err != nil {
				t.Fatalf("walkTree: %v", err)
			}
			wantBytesWithin(t, b-b0, cost{w.growth.bytes - w0.growth.bytes, w.growth.nodes - w0.growth.nodes})
		})
	}
}

// TestFieldSpecsAsCopied pins the copied field specs to the counts of kustomize v0.21.2 (22 commonLabels specs
// with 9 metadata labels and 1 volume claim template, 9 commonAnnotations), those with create (6 commonLabels,
// every metadata label and the volume claim template), and the most places per object of one kind with create.
func TestFieldSpecsAsCopied(t *testing.T) {
	for name, c := range map[string]struct {
		specs             string
		n, create, places int
	}{
		"labels":      {labelSpecs, 32, 16, 3},
		"annotations": {annotationSpecs, 9, 9, 3},
		"namespace":   {namespaceSpecs, 4, 3, 2},
		"replicas":    {replicaSpecs, 4, 4, 1},
	} {
		specs := parseSpecs(c.specs)
		if got := len(specs); got != c.n {
			t.Errorf("%s: %d specs, want %d", name, got, c.n)
		}
		create := 0
		for _, p := range specs {
			if p.create {
				create++
			}
		}
		if create != c.create {
			t.Errorf("%s: %d specs with create, want %d", name, create, c.create)
		}
		places := 0
		for _, kb := range boundOf(specs).kinds {
			places = max(places, kb.places)
		}
		if places != c.places {
			t.Errorf("%s: %d places per object, want %d", name, places, c.places)
		}
	}
}
