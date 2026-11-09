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

// renderedBytes renders files and returns the bytes of the objects as JSON.
func renderedBytes(t *testing.T, files map[string][]byte) int {
	t.Helper()
	objs, err := render(context.Background(), files, ".")
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	n := 0
	for _, o := range objs {
		b, err := json.Marshal(o)
		if err != nil {
			t.Fatal(err)
		}
		n += len(b)
	}
	return n
}

// TestGrowthBoundsTheRender renders fanoutObjects with each transformer and a value of 1 KiB, and checks that
// what the render grows by is at most the bound the walk computes: a place the bound does not count would show.
func TestGrowthBoundsTheRender(t *testing.T) {
	v := strings.Repeat("x", 1<<10)
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
	}
	base := renderedBytes(t, map[string][]byte{rootKust: []byte("resources: [o.yaml]\n"), "o.yaml": []byte(fanoutObjects)})
	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			files := map[string][]byte{rootKust: []byte(extra + "resources: [o.yaml]\n"), "o.yaml": []byte(fanoutObjects)}
			w, err := tree(context.Background(), files, ".")
			if err != nil {
				t.Fatalf("tree: %v", err)
			}
			bound := growth(&w.coef, &w.shape, w.patchBytes, &w.patchShape, w.buildDocs)
			grew := renderedBytes(t, files) - base
			if grew > bound {
				t.Fatalf("the render grew by %d bytes, over the bound of %d", grew, bound)
			}
			t.Logf("%s: grew %d, bound %d", name, grew, bound)
		})
	}
}

// TestFieldSpecsAsCopied pins the copied field specs to the counts of kustomize v0.21.2 (22 commonLabels specs
// with 9 metadata labels and 1 volume claim template, 9 commonAnnotations) and the places per object of each.
func TestFieldSpecsAsCopied(t *testing.T) {
	for name, c := range map[string]struct {
		specs     string
		n, places int
	}{
		"labels":      {labelSpecs, 32, 8},
		"annotations": {annotationSpecs, 9, 3},
		"namespace":   {namespaceSpecs, 4, 2},
		"replicas":    {replicaSpecs, 4, 1},
	} {
		if got := len(parseSpecs(c.specs)); got != c.n {
			t.Errorf("%s: %d specs, want %d", name, got, c.n)
		}
		if got := boundOf(parseSpecs(c.specs)).perObject; got != c.places {
			t.Errorf("%s: %d places per object, want %d", name, got, c.places)
		}
	}
}
