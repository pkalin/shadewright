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
	"fmt"
	"path"

	utiljson "k8s.io/apimachinery/pkg/util/json"
	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/api/types"
	"sigs.k8s.io/kustomize/kyaml/filesys"
)

// renderSlot serialises builds: kustomize keeps global state (the OpenAPI schema it sets on every run), so
// concurrent builds in one process can race. It is a one-slot channel rather than a mutex so that a caller
// waiting for its turn can give up when its context ends.
var renderSlot = make(chan struct{}, 1)

// options returns the locked-down kustomize options: built-in transformers only (no exec or container
// plugins, no KRM functions), Helm off, no managed-by label. Loading is not restricted to the kustomization
// root, because the file system holds nothing but the files of this build, each at a validated path inside
// the repository; a resource in a sibling directory therefore builds as it would from the repository.
func options() *krusty.Options {
	o := krusty.MakeDefaultOptions()
	o.LoadRestrictions = types.LoadRestrictionsNone
	o.PluginConfig = types.DisabledPluginConfig()
	o.PluginConfig.HelmConfig.Enabled = false
	o.AddManagedbyLabel = false
	return o
}

// render builds dir from files on an in-memory file system and returns the objects, decoded as Kubernetes
// decodes JSON (whole numbers as int64). Remote bases and remote files cannot be reached: the walk has
// refused every reference that kustomize could read as a URL or Git address, and the file system holds
// only the given files. The kustomize error text is dropped, since it may quote file contents.
//
// The wait for the render slot honours ctx; the render itself cannot be cancelled once it has started.
func render(ctx context.Context, files map[string][]byte, dir string) (objs []map[string]any, err error) {
	failed := refuse(ReasonRenderFailed, "kustomize could not build "+dir)
	defer func() {
		if recover() != nil {
			objs, err = nil, failed
		}
	}()
	fs := filesys.MakeFsInMemory()
	for p, data := range files {
		if fs.WriteFile(path.Join("/", p), data) != nil {
			return nil, failed
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case renderSlot <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-renderSlot }()
	m, err := krusty.MakeKustomizer(options()).Run(fs, path.Join("/", dir))
	if err != nil {
		return nil, failed
	}
	if m.Size() > MaxObjects {
		return nil, refuse(ReasonTooLarge, fmt.Sprintf("the build renders more than %d objects", MaxObjects))
	}
	for _, r := range m.Resources() {
		b, err := r.MarshalJSON()
		if err != nil {
			return nil, failed
		}
		var obj map[string]any
		if err := utiljson.Unmarshal(b, &obj); err != nil || obj == nil {
			return nil, failed
		}
		objs = append(objs, obj)
	}
	return objs, nil
}
