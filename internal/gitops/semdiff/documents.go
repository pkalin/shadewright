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
	"strings"

	yaml "go.yaml.in/yaml/v3"

	"github.com/pkalin/shadewright/internal/gitops/gityaml"
)

// documents returns an upper bound of the objects kustomize processes from one manifest or patch file, or from
// an inline patch, at most MaxBuildDocuments+1 (the count stops there). name is the file, or what holds the
// inline patch, for the refusal's detail. It refuses with InvalidYAML what [gityaml.Objects] refuses: input
// that is not UTF-8, YAML anchors, aliases and merge keys, a separator line with more than a comment after
// "---", and a part that does not parse.
// Once the count is over MaxBuildDocuments the rest of the input is not read, nor checked: the caller must
// refuse it.
func documents(name string, data []byte) (int, error) {
	n, err := gityaml.Objects(data, MaxBuildDocuments)
	if err != nil {
		return 0, refuse(ReasonInvalidYAML, name+" "+err.Error())
	}
	return n, nil
}

// errCopy is the error of [refuseCopy]; its text goes into a refusal's detail.
var errCopy = errors.New("JSON patch copy operations are not supported")

// refuseCopy refuses, with UnsupportedKustomization, a patch file or inline patch that holds a JSON patch
// (RFC 6902) copy operation: a mapping, anywhere in the patch, whose "op" key is "copy" however quoted or
// escaped. Each copy can double a value, so a patch of a few hundred bytes renders gigabytes, past every
// bound the build checks before the render (#488). It looks at every mapping rather than only at the items of
// a top-level list, so how kustomize tells a JSON patch from a strategic merge patch does not matter; a
// strategic merge patch that sets a field "op" to "copy" is refused too. It decodes data as [documents] does,
// and is called after it, which has refused input that is not UTF-8, does not parse or holds an alias.
func refuseCopy(name string, data []byte) error {
	for _, p := range gityaml.Parts(data) {
		dec := yaml.NewDecoder(strings.NewReader(p.Text))
		for {
			var doc yaml.Node
			err := dec.Decode(&doc)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return refuse(ReasonInvalidYAML, fmt.Sprintf("%s line %d: %v", name, p.Line, gityaml.ErrNotParse))
			}
			if c := copyOp(&doc); c != nil {
				return refuse(ReasonUnsupportedKustomization, fmt.Sprintf("%s line %d: %v", name, p.Line+c.Line-1, errCopy))
			}
		}
	}
	return nil
}

// copyOp returns the first "op" key below n whose value is "copy", or nil. Aliases are not followed.
func copyOp(n *yaml.Node) *yaml.Node {
	for i, c := range n.Content {
		if n.Kind == yaml.MappingNode && i%2 == 0 && i+1 < len(n.Content) && c.Kind == yaml.ScalarNode &&
			c.Value == "op" && n.Content[i+1].Kind == yaml.ScalarNode && n.Content[i+1].Value == "copy" {
			return c
		}
		if o := copyOp(c); o != nil {
			return o
		}
	}
	return nil
}
