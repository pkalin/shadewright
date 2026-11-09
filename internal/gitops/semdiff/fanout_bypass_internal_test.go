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
	"strings"
	"testing"
)

// The bypasses a review of #495 found in the first bound, each accepted with a render far over it: a JSON patch
// adds containers that an image override then grows, a patch renames an object that a thousand references
// name, and a name prefix rewrites annotation references whose keys carry a domain prefix. Each must now be
// refused, or rendered within the bound.

// buildBound walks files and returns the bound of what their patches and transformers add to the render, as a
// total of bytes and nodes ([cost.total]), or refused when the walk refuses the build as too large.
func buildBound(t *testing.T, files map[string][]byte) (bound int, refused bool) {
	t.Helper()
	w, err := tree(context.Background(), files, ".")
	if se, ok := errors.AsType[*Error](err); ok && se.Reason == ReasonTooLarge {
		return 0, true
	}
	if err != nil {
		t.Fatal(err)
	}
	return w.growth.total(), false
}

func TestBypassJSONPatchImageFanout(t *testing.T) {
	var p strings.Builder
	p.WriteString("- op: add\n  path: /spec/containers\n  value:\n")
	for i := range 1000 {
		fmt.Fprintf(&p, "    - name: c%d\n      image: app\n", i)
	}
	files := map[string][]byte{
		rootKust:   []byte("resources: [pod.yaml]\nimages:\n- name: app\n  newName: " + strings.Repeat("x", 16<<10) + "\npatches:\n- path: p.yaml\n  target: {kind: Pod}\n"),
		"pod.yaml": []byte("apiVersion: v1\nkind: Pod\nmetadata: {name: p}\nspec: {}\n"),
		"p.yaml":   []byte(p.String()),
	}
	bound, refused := buildBound(t, files)
	if refused {
		return
	}
	rendered := renderedBytes(t, files)
	t.Logf("accepted: bound=%d, rendered=%d", bound, rendered)
	if rendered > MaxTransformBytes+MaxBuildBytes {
		t.Errorf("accepted render of %d bytes with bound %d", rendered, bound)
	}
}

func TestBypassPatchRenameFanout(t *testing.T) {
	var refs strings.Builder
	for range 1000 {
		refs.WriteString("    - configMapRef: {name: cm}\n")
	}
	files := map[string][]byte{
		rootKust:   []byte("resources: [cm.yaml, pod.yaml]\npatches:\n- path: p.yaml\n  target: {kind: ConfigMap}\n"),
		"cm.yaml":  []byte("apiVersion: v1\nkind: ConfigMap\nmetadata: {name: cm}\n"),
		"pod.yaml": []byte("apiVersion: v1\nkind: Pod\nmetadata: {name: p}\nspec:\n  containers:\n  - name: app\n    image: app\n    envFrom:\n" + refs.String()),
		"p.yaml":   []byte("- op: replace\n  path: /metadata/name\n  value: " + strings.Repeat("x", 16<<10) + "\n"),
	}
	bound, refused := buildBound(t, files)
	if refused {
		return
	}
	rendered := renderedBytes(t, files)
	t.Logf("accepted: bound=%d, rendered=%d", bound, rendered)
	if rendered > MaxTransformBytes+MaxBuildBytes {
		t.Errorf("accepted render of %d bytes with bound %d", rendered, bound)
	}
}

func TestBypassAnnotationNameReferenceFanout(t *testing.T) {
	var objects strings.Builder
	objects.WriteString("apiVersion: v1\nkind: Secret\nmetadata: {name: s}\n")
	for i := range 1000 {
		fmt.Fprintf(&objects, "---\napiVersion: networking.k8s.io/v1\nkind: Ingress\nmetadata:\n  name: i%d\n  annotations:\n    ingress.kubernetes.io/auth-secret: s\n    nginx.ingress.kubernetes.io/auth-secret: s\n    nginx.ingress.kubernetes.io/auth-tls-secret: s\n", i)
	}
	files := map[string][]byte{rootKust: []byte("resources: [" + objectsFile + "]\nnamePrefix: " + strings.Repeat("x", 2000) + "\n"), objectsFile: []byte(objects.String())}
	base := renderedBytes(t, map[string][]byte{rootKust: []byte("resources: [" + objectsFile + "]\n"), objectsFile: files[objectsFile]})
	bound, refused := buildBound(t, files)
	if refused {
		return
	}
	rendered := renderedBytes(t, files) - base
	t.Logf("accepted: bound=%d, growth=%d", bound, rendered)
	if rendered > MaxTransformBytes {
		t.Errorf("accepted growth of %d bytes with bound %d", rendered, bound)
	}
}
