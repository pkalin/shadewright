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

package tools

import (
	"encoding/json"
	"testing"
)

// FuzzArguments feeds arbitrary arguments to every tool's parser. It never panics, and arguments it accepts
// are within the bounds: at most MaxArgumentBytes, one JSON object, and a namespace, when they name one, in
// the watched set.
func FuzzArguments(f *testing.F) {
	for _, name := range Names() {
		f.Add(name, with(name, nil))
		f.Add(name, with(name, map[string]any{argNamespace: otherNS}))
	}
	f.Add(NameGetResource, `{"kind":"Pod","namespace":"shop","name":"a","Kind":"Secret"}`)
	f.Add(NameGetResource, `{"kind":"Pod","namespace":"shop","name":"a","namespace":"kube-system"}`)
	f.Add(NameReadGitFile, `{"path":"a/../../b"}`)
	set := newEnv(f, nil).set
	f.Fuzz(func(t *testing.T, name, args string) {
		tl, ok := lookup(name)
		if !ok {
			return
		}
		if _, err := tl.parse(set, json.RawMessage(args)); err != nil {
			return
		}
		if len(args) > MaxArgumentBytes {
			t.Fatalf("accepted %d bytes", len(args))
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(args), &m); err != nil && len(args) > 0 {
			t.Fatalf("accepted arguments that are not one object: %q", args)
		}
		if ns, ok := m[argNamespace]; ok && ns != testNS {
			t.Fatalf("accepted namespace %v", ns)
		}
		if _, ok := m[argNamespace]; !ok && tl.name != NameReadGitFile && tl.name != NameGetRecentChanges {
			t.Fatalf("accepted %s without a namespace: %q", tl.name, args)
		}
	})
}
